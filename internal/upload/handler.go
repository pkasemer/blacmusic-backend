package upload

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/httpx"
	"github.com/blacmusic/backend/internal/storage"
)

// Blobs is the slice of object storage the upload API needs.
type Blobs interface {
	PresignPut(ctx context.Context, key string, ttl time.Duration) (string, error)
	Stat(ctx context.Context, key string) (int64, error)
	Remove(ctx context.Context, key string) error
}

// Handler serves /v1/uploads. Only creators and admins may use it.
type Handler struct {
	store    *Store
	pool     *pgxpool.Pool
	blobs    Blobs
	maxBytes int64
	urlTTL   time.Duration
}

// NewHandler creates the handler.
func NewHandler(pool *pgxpool.Pool, blobs Blobs, maxBytes int64, urlTTL time.Duration) *Handler {
	return &Handler{store: NewStore(pool), pool: pool, blobs: blobs, maxBytes: maxBytes, urlTTL: urlTTL}
}

// Routes registers the routes on r (already authenticated).
func (h *Handler) Routes(r chi.Router) {
	r.Use(h.requireCreator)
	r.Post("/", h.create)
	r.Get("/", h.list)
	r.Get("/{id}", h.get)
	r.Post("/{id}/complete", h.complete)
}

func (h *Handler) requireCreator(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid, _ := auth.UserID(r.Context())
		var role string
		err := h.pool.QueryRow(r.Context(), `SELECT role FROM users WHERE id = $1`, uid).Scan(&role)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && role != "creator" && role != "admin") {
			httpx.Error(w, http.StatusForbidden, "forbidden", "your account is not allowed to upload")
			return
		}
		if err != nil {
			serverError(w, err)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type createRequest struct {
	Title      string `json:"title"`
	ArtistName string `json:"artistName"`
	AlbumTitle string `json:"albumTitle"`
	Explicit   bool   `json:"explicit"`
	Filename   string `json:"filename"`
	SizeBytes  int64  `json:"sizeBytes"`
}

type uploadTarget struct {
	Method    string    `json:"method"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type createResponse struct {
	view
	Upload   uploadTarget `json:"upload"`
	MaxBytes int64        `json:"maxBytes"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	var req createRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	req.ArtistName = strings.TrimSpace(req.ArtistName)
	req.AlbumTitle = strings.TrimSpace(req.AlbumTitle)
	req.Filename = strings.TrimSpace(req.Filename)

	if msg := h.validate(req); msg != "" {
		httpx.Error(w, http.StatusUnprocessableEntity, "validation_error", msg)
		return
	}

	u, err := h.store.Create(r.Context(), NewUpload{
		UserID: uid, Title: req.Title, ArtistName: req.ArtistName, AlbumTitle: req.AlbumTitle,
		Explicit: req.Explicit, Filename: req.Filename, SizeBytes: req.SizeBytes,
	})
	if errors.Is(err, ErrLimit) {
		httpx.Error(w, http.StatusConflict, "limit_reached", "too many unfinished uploads; wait for them to finish")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}

	url, err := h.blobs.PresignPut(r.Context(), u.OriginalKey, h.urlTTL)
	if err != nil {
		serverError(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, createResponse{
		view:     toView(u),
		Upload:   uploadTarget{Method: http.MethodPut, URL: url, ExpiresAt: time.Now().Add(h.urlTTL).UTC()},
		MaxBytes: h.maxBytes,
	})
}

func (h *Handler) validate(req createRequest) string {
	switch {
	case utf8.RuneCountInString(req.Title) < 1 || utf8.RuneCountInString(req.Title) > 200:
		return "title must be 1-200 characters"
	case utf8.RuneCountInString(req.ArtistName) < 1 || utf8.RuneCountInString(req.ArtistName) > 100:
		return "artistName must be 1-100 characters"
	case utf8.RuneCountInString(req.AlbumTitle) > 200:
		return "albumTitle must be at most 200 characters"
	case utf8.RuneCountInString(req.Filename) < 1 || utf8.RuneCountInString(req.Filename) > 255:
		return "filename must be 1-255 characters"
	case req.SizeBytes < 1:
		return "sizeBytes is required"
	case req.SizeBytes > h.maxBytes:
		return "file is too large (limit " + strconv.FormatInt(h.maxBytes>>20, 10) + " MB)"
	}
	return ""
}

// complete is called by the client after its PUT finished. It checks the file
// really arrived and hands it to the worker. Calling it again is harmless.
func (h *Handler) complete(w http.ResponseWriter, r *http.Request) {
	u, ok := h.owned(w, r)
	if !ok {
		return
	}
	if u.Status != StatusPending {
		httpx.JSON(w, http.StatusOK, toView(u)) // already queued, processing or finished
		return
	}

	size, err := h.blobs.Stat(r.Context(), u.OriginalKey)
	if errors.Is(err, storage.ErrNotFound) {
		httpx.Error(w, http.StatusConflict, "not_uploaded", "the file has not been uploaded yet")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	if size < 1 || size > h.maxBytes {
		msg := "the uploaded file is empty"
		if size > h.maxBytes {
			msg = "the uploaded file is too large"
		}
		_ = h.blobs.Remove(r.Context(), u.OriginalKey)
		if err := h.store.Fail(r.Context(), u.ID, msg); err != nil {
			serverError(w, err)
			return
		}
		httpx.Error(w, http.StatusUnprocessableEntity, "validation_error", msg)
		return
	}

	u, err = h.store.Queue(r.Context(), u.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	httpx.JSON(w, http.StatusAccepted, toView(u))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	if u, ok := h.owned(w, r); ok {
		httpx.JSON(w, http.StatusOK, toView(u))
	}
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	limit, offset := 50, 0
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = min(n, 200)
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && n > 0 {
		offset = min(n, 100000)
	}
	items, err := h.store.ListByUser(r.Context(), uid, limit, offset)
	if err != nil {
		serverError(w, err)
		return
	}
	out := make([]view, len(items))
	for i, u := range items {
		out[i] = toView(u)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"uploads": out})
}

// owned loads the upload in the URL; other people's uploads look like missing ones.
func (h *Handler) owned(w http.ResponseWriter, r *http.Request) (*Upload, bool) {
	uid, _ := auth.UserID(r.Context())
	u, err := h.store.Get(r.Context(), chi.URLParam(r, "id"))
	if errors.Is(err, ErrNotFound) || (err == nil && u.UserID != uid) {
		httpx.Error(w, http.StatusNotFound, "not_found", "upload not found")
		return nil, false
	}
	if err != nil {
		serverError(w, err)
		return nil, false
	}
	return u, true
}

// view is the public shape of an upload.
type view struct {
	ID         string    `json:"id"`
	Status     Status    `json:"status"`
	Title      string    `json:"title"`
	ArtistName string    `json:"artistName"`
	AlbumTitle string    `json:"albumTitle,omitempty"`
	Explicit   bool      `json:"explicit"`
	Filename   string    `json:"filename"`
	SizeBytes  int64     `json:"sizeBytes"`
	TrackID    string    `json:"trackId,omitempty"` // set once ready
	Error      string    `json:"error,omitempty"`   // set when failed
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

func toView(u *Upload) view {
	v := view{
		ID: u.ID, Status: u.Status, Title: u.Title, ArtistName: u.ArtistName, AlbumTitle: u.AlbumTitle,
		Explicit: u.Explicit, Filename: u.Filename, SizeBytes: u.SizeBytes, Error: u.Error,
		CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt,
	}
	if u.Status == StatusReady {
		v.TrackID = u.TrackID
	}
	return v
}

func serverError(w http.ResponseWriter, err error) {
	slog.Error("upload handler", "err", err)
	httpx.Error(w, http.StatusInternalServerError, "internal", "something went wrong")
}
