package image

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/httpx"
	"github.com/blacmusic/backend/internal/storage"
)

// MaxImagesPerUser bounds pending and finished images per account.
const MaxImagesPerUser = 50

const uploadTTL = 15 * time.Minute

// Blobs is the slice of object storage the image service needs.
type Blobs interface {
	PresignPut(ctx context.Context, key string, ttl time.Duration) (string, error)
	Stat(ctx context.Context, key string) (int64, error)
	Download(ctx context.Context, key, dst string) error
	UploadFile(ctx context.Context, key, src, contentType string) error
	Remove(ctx context.Context, key string) error
	Open(ctx context.Context, key string) (io.ReadCloser, int64, error)
}

// Handler creates, processes and serves images.
type Handler struct {
	pool      *pgxpool.Pool
	blobs     Blobs
	publicURL string
	sem       chan struct{} // decoding a big image takes ~100 MB; allow two at a time
}

// NewHandler creates the handler.
func NewHandler(pool *pgxpool.Pool, blobs Blobs, publicURL string) *Handler {
	return &Handler{pool: pool, blobs: blobs, publicURL: publicURL, sem: make(chan struct{}, 2)}
}

// Routes registers the authenticated routes (mounted at /v1/images).
func (h *Handler) Routes(r chi.Router) {
	r.Post("/", h.create)
	r.Post("/{id}/complete", h.complete)
}

// ServeRoute registers the public read route (mounted at /v1/media). Image ids
// are unguessable and hold only avatars and covers.
func (h *Handler) ServeRoute(r chi.Router) {
	r.Get("/{id}/{file}", h.serve)
}

func originalKey(id string) string      { return "images/originals/" + id }
func variantKey(id, size string) string { return "images/" + id + "/" + size + ".jpg" }

type uploadTarget struct {
	Method    string    `json:"method"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type view struct {
	ID      string            `json:"id"`
	Purpose string            `json:"purpose"`
	Status  string            `json:"status"`
	Width   int               `json:"width,omitempty"`
	Height  int               `json:"height,omitempty"`
	Error   string            `json:"error,omitempty"`
	URLs    map[string]string `json:"urls,omitempty"` // when ready: small, medium, large
	Upload  *uploadTarget     `json:"upload,omitempty"`
}

func (h *Handler) urls(id string) map[string]string {
	m := map[string]string{}
	for _, s := range Sizes {
		m[s] = URL(h.publicURL, id, s)
	}
	return m
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	var req struct {
		Purpose   string `json:"purpose"`
		SizeBytes int64  `json:"sizeBytes"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.Purpose != "avatar" && req.Purpose != "cover" {
		httpx.Error(w, http.StatusUnprocessableEntity, "validation_error", "purpose must be avatar or cover")
		return
	}
	if req.SizeBytes < 1 || req.SizeBytes > MaxUploadBytes {
		httpx.Error(w, http.StatusUnprocessableEntity, "validation_error", "sizeBytes must be between 1 byte and 10 MB")
		return
	}

	var id string
	err := h.pool.QueryRow(r.Context(), `
		WITH n AS (SELECT 'img_' || replace(gen_random_uuid()::text, '-', '') AS id)
		INSERT INTO images (id, owner_id, purpose, original_key, size_bytes)
		SELECT n.id, $1, $2, 'images/originals/' || n.id, $3 FROM n
		WHERE (SELECT count(*) FROM images WHERE owner_id = $1 AND status IN ('pending','ready')) < $4
		RETURNING id`, uid, req.Purpose, req.SizeBytes, MaxImagesPerUser).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, http.StatusConflict, "limit_reached", "too many images; delete unused ones first")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	url, err := h.blobs.PresignPut(r.Context(), originalKey(id), uploadTTL)
	if err != nil {
		serverError(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, view{
		ID: id, Purpose: req.Purpose, Status: "pending",
		Upload: &uploadTarget{Method: http.MethodPut, URL: url, ExpiresAt: time.Now().Add(uploadTTL).UTC()},
	})
}

// complete validates the uploaded file and produces the stored sizes. It runs
// inside the request: a small image takes a fraction of a second.
func (h *Handler) complete(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	id := chi.URLParam(r, "id")
	if !idRe.MatchString(id) {
		httpx.Error(w, http.StatusNotFound, "not_found", "image not found")
		return
	}

	var purpose, status, owner string
	var width, height *int
	err := h.pool.QueryRow(r.Context(), `SELECT purpose, status, owner_id::text, width, height FROM images WHERE id = $1`, id).
		Scan(&purpose, &status, &owner, &width, &height)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && owner != uid) {
		httpx.Error(w, http.StatusNotFound, "not_found", "image not found")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	switch status {
	case "ready":
		v := view{ID: id, Purpose: purpose, Status: "ready", URLs: h.urls(id)}
		if width != nil && height != nil {
			v.Width, v.Height = *width, *height
		}
		httpx.JSON(w, http.StatusOK, v)
		return
	case "failed":
		httpx.Error(w, http.StatusUnprocessableEntity, "invalid_image", "this upload was rejected; create a new one")
		return
	}

	size, err := h.blobs.Stat(r.Context(), originalKey(id))
	if errors.Is(err, storage.ErrNotFound) {
		httpx.Error(w, http.StatusConflict, "not_uploaded", "the file has not been uploaded yet")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	if size < 1 || size > MaxUploadBytes {
		h.reject(r.Context(), id, "the image must be between 1 byte and 10 MB")
		httpx.Error(w, http.StatusUnprocessableEntity, "invalid_image", "the image must be between 1 byte and 10 MB")
		return
	}

	select {
	case h.sem <- struct{}{}:
		defer func() { <-h.sem }()
	case <-r.Context().Done():
		return
	}
	res, err := h.fetchAndProcess(r.Context(), id)
	var bad *InvalidImageError
	if errors.As(err, &bad) {
		h.reject(r.Context(), id, bad.Msg)
		httpx.Error(w, http.StatusUnprocessableEntity, "invalid_image", bad.Msg)
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	for _, s := range Sizes {
		if err := h.putVariant(r.Context(), id, s, res.Variants[s]); err != nil {
			serverError(w, err)
			return
		}
	}
	if _, err := h.pool.Exec(r.Context(), `UPDATE images SET status = 'ready', width = $2, height = $3 WHERE id = $1`,
		id, res.Width, res.Height); err != nil {
		serverError(w, err)
		return
	}
	_ = h.blobs.Remove(r.Context(), originalKey(id)) // the raw upload is no longer needed
	httpx.JSON(w, http.StatusOK, view{ID: id, Purpose: purpose, Status: "ready", Width: res.Width, Height: res.Height, URLs: h.urls(id)})
}

func (h *Handler) fetchAndProcess(ctx context.Context, id string) (*Processed, error) {
	dir, err := os.MkdirTemp("", "blac-img-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "raw")
	if err := h.blobs.Download(ctx, originalKey(id), path); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Process(raw)
}

func (h *Handler) putVariant(ctx context.Context, id, size string, data []byte) error {
	dir, err := os.MkdirTemp("", "blac-img-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, size)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return h.blobs.UploadFile(ctx, variantKey(id, size), path, "image/jpeg")
}

// reject marks an upload failed and deletes the raw file.
func (h *Handler) reject(ctx context.Context, id, msg string) {
	if _, err := h.pool.Exec(ctx, `UPDATE images SET status = 'failed', error = $2 WHERE id = $1`, id, msg); err != nil {
		slog.Error("mark image failed", "err", err)
	}
	_ = h.blobs.Remove(ctx, originalKey(id))
}

// serve streams a stored size. Ids never change content, so it is cacheable forever.
func (h *Handler) serve(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	size, ok := strings.CutSuffix(chi.URLParam(r, "file"), ".jpg")
	if !idRe.MatchString(id) || !ok || sizePixels[size] == 0 {
		http.NotFound(w, r)
		return
	}
	body, n, err := h.blobs.Open(r.Context(), variantKey(id, size))
	if errors.Is(err, storage.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		slog.Error("open image", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer body.Close()
	etag := `"` + id + "-" + size + `"`
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", "image/jpeg")
	hd.Set("Content-Length", strconv.FormatInt(n, 10))
	hd.Set("Cache-Control", "public, max-age=31536000, immutable")
	hd.Set("ETag", etag)
	hd.Set("X-Content-Type-Options", "nosniff")
	_, _ = io.Copy(w, body)
}

// ExpirePending deletes uploads that were never completed.
func ExpirePending(ctx context.Context, pool *pgxpool.Pool, blobs interface {
	Remove(ctx context.Context, key string) error
}, olderThan time.Duration) (int, error) {
	rows, err := pool.Query(ctx, `
		DELETE FROM images WHERE status = 'pending' AND created_at < now() - make_interval(secs => $1)
		RETURNING original_key`, olderThan.Seconds())
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return 0, err
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, k := range keys {
		_ = blobs.Remove(ctx, k)
	}
	return len(keys), nil
}

func serverError(w http.ResponseWriter, err error) {
	slog.Error("image handler", "err", err)
	httpx.Error(w, http.StatusInternalServerError, "internal", "something went wrong")
}

// ExpireUnused deletes finished or rejected images that nothing refers to any
// more (a replaced avatar, a replaced cover) once they are older than
// olderThan, so replacing a picture does not use up the per-account image
// allowance or leave files behind. The age gives a freshly uploaded image time
// to be attached.
func ExpireUnused(ctx context.Context, pool *pgxpool.Pool, blobs interface {
	Remove(ctx context.Context, key string) error
}, olderThan time.Duration) (int, error) {
	rows, err := pool.Query(ctx, `
		DELETE FROM images i
		WHERE i.status IN ('ready', 'failed')
		  AND i.created_at < now() - make_interval(secs => $1)
		  AND NOT EXISTS (SELECT 1 FROM users u  WHERE u.avatar_image_id = i.id)
		  AND NOT EXISTS (SELECT 1 FROM artists a WHERE a.avatar_image_id = i.id)
		  AND NOT EXISTS (SELECT 1 FROM albums al WHERE al.cover_image_id = i.id)
		  AND NOT EXISTS (SELECT 1 FROM playlists pl WHERE pl.cover_image_id = i.id)
		RETURNING i.id`, olderThan.Seconds())
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, id := range ids {
		for _, size := range Sizes {
			_ = blobs.Remove(ctx, variantKey(id, size))
		}
		_ = blobs.Remove(ctx, originalKey(id))
	}
	return len(ids), nil
}

// Ingest turns raw image bytes into a finished image owned by ownerID, exactly
// as the upload flow does (validation, orientation, EXIF removal, three sizes),
// for server-side sources such as a channel thumbnail. It returns the image id.
func Ingest(ctx context.Context, pool *pgxpool.Pool, blobs Blobs, ownerID, purpose string, raw []byte) (string, error) {
	res, err := Process(raw)
	if err != nil {
		return "", err
	}
	var id string
	err = pool.QueryRow(ctx, `
		WITH n AS (SELECT 'img_' || replace(gen_random_uuid()::text, '-', '') AS id)
		INSERT INTO images (id, owner_id, purpose, original_key, size_bytes, status, width, height)
		SELECT n.id, $1, $2, 'images/originals/' || n.id, $3, 'ready', $4, $5 FROM n
		RETURNING id`, ownerID, purpose, len(raw), res.Width, res.Height).Scan(&id)
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "blac-img-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	for _, size := range Sizes {
		path := filepath.Join(dir, size)
		if err := os.WriteFile(path, res.Variants[size], 0o600); err != nil {
			return "", err
		}
		if err := blobs.UploadFile(ctx, variantKey(id, size), path, "image/jpeg"); err != nil {
			_, _ = pool.Exec(ctx, `DELETE FROM images WHERE id = $1`, id)
			return "", err
		}
	}
	return id, nil
}

// Purge deletes every image a user owns: the stored files first, then the rows.
// It is part of deleting an account and safe to run again.
func Purge(ctx context.Context, pool *pgxpool.Pool, blobs interface {
	Remove(ctx context.Context, key string) error
}, ownerID string) error {
	rows, err := pool.Query(ctx, `SELECT id FROM images WHERE owner_id = $1`, ownerID)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		for _, size := range Sizes {
			_ = blobs.Remove(ctx, variantKey(id, size))
		}
		_ = blobs.Remove(ctx, originalKey(id))
	}
	_, err = pool.Exec(ctx, `DELETE FROM images WHERE owner_id = $1`, ownerID)
	return err
}
