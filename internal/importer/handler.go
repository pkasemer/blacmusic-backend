package importer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/audit"
	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/httpx"
	"github.com/blacmusic/backend/internal/storage"
	"github.com/blacmusic/backend/internal/sysmetrics"
)

// Blobs is the object storage the import routes need.
type Blobs interface {
	PresignPut(ctx context.Context, key string, ttl time.Duration) (string, error)
	Stat(ctx context.Context, key string) (int64, error)
	Remove(ctx context.Context, key string) error
	StartMultipart(ctx context.Context, key string) (string, error)
	PresignPart(ctx context.Context, key, uploadID string, number int, ttl time.Duration) (string, error)
	ListParts(ctx context.Context, key, uploadID string) ([]storage.Part, error)
	CompleteMultipart(ctx context.Context, key, uploadID string, parts []storage.Part) error
	AbortMultipart(ctx context.Context, key, uploadID string) error
}

// Options configures the importer.
type Options struct {
	MaxAudioBytes int64
	PartSize      int64         // files over twice this size are sent in parts
	URLTTL        time.Duration // how long an upload link works
	MaxTotalBytes int64         // all files of one import together
	MinFreeBytes  int64         // an import is refused when the disk would drop below this (0: no check)
	AdminSiteURL  string        // base URL of the admin site, for links in answers ("" leaves them out)
	// FreeBytes reports the free space on the disk that holds the storage; ok is false when unknown.
	FreeBytes func() (bytes int64, ok bool)
}

// Handler serves /v1/admin/imports and the key routes.
type Handler struct {
	pool   *pgxpool.Pool
	blobs  Blobs
	tokens *auth.TokenService
	opt    Options
}

// NewHandler creates the handler.
func NewHandler(pool *pgxpool.Pool, blobs Blobs, tokens *auth.TokenService, opt Options) *Handler {
	if opt.FreeBytes == nil {
		opt.FreeBytes = diskFree
	}
	return &Handler{pool: pool, blobs: blobs, tokens: tokens, opt: opt}
}

// Routes registers the import routes (mounted at /v1/admin/imports, behind Authenticate):
//
//	POST   /                create a job from a manifest (Idempotency-Key header) -> upload links
//	GET    /                the latest jobs
//	GET    /{id}            status and per-track results
//	POST   /{id}/links      fresh upload links for the files that have not arrived
//	POST   /{id}/start      every file is there: do the import
//	DELETE /{id}            cancel a job that has not started
func (h *Handler) Routes(r chi.Router) {
	r.Use(h.Authenticate)
	r.Post("/", h.create)
	r.Get("/", h.list)
	r.Get("/{id}", h.get)
	r.Post("/{id}/links", h.links)
	r.Post("/{id}/start", h.start)
	r.Delete("/{id}", h.cancel)
}

type partURL struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
}

type target struct {
	Type          string    `json:"type"` // single | multipart
	Method        string    `json:"method"`
	URL           string    `json:"url,omitempty"`
	PartSize      int64     `json:"partSize,omitempty"`
	PartCount     int       `json:"partCount,omitempty"`
	Parts         []partURL `json:"parts,omitempty"`         // the parts still to send
	UploadedParts []int     `json:"uploadedParts,omitempty"` // already in storage
	ExpiresAt     time.Time `json:"expiresAt"`
}

type fileView struct {
	Name      string  `json:"name"`
	Kind      string  `json:"kind"`
	SizeBytes int64   `json:"sizeBytes"`
	Uploaded  bool    `json:"uploaded"`
	Upload    *target `json:"upload,omitempty"`
}

type fileRow struct {
	name, kind, key, multipart string
	size, partSize             int64
}

func partCount(size, part int64) int { return int((size + part - 1) / part) }

// state looks at storage: whether the file has fully arrived, and for a multipart file which parts are good.
func (h *Handler) state(ctx context.Context, f fileRow) (complete bool, good []storage.Part, err error) {
	if n, serr := h.blobs.Stat(ctx, f.key); serr == nil {
		return n >= 1 && n <= f.size, nil, nil
	} else if !errors.Is(serr, storage.ErrNotFound) {
		return false, nil, serr
	}
	if f.multipart == "" {
		return false, nil, nil
	}
	have, err := h.blobs.ListParts(ctx, f.key, f.multipart)
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	n := partCount(f.size, f.partSize)
	for _, p := range have {
		want := f.partSize
		if p.Number == n {
			want = f.size - f.partSize*int64(n-1)
		}
		if p.Number >= 1 && p.Number <= n && p.Size == want {
			good = append(good, p)
		}
	}
	return false, good, nil
}

// link says where to send a file that has not arrived.
func (h *Handler) link(ctx context.Context, f fileRow, good []storage.Part) (*target, error) {
	expires := time.Now().Add(h.opt.URLTTL).UTC()
	if f.multipart == "" {
		url, err := h.blobs.PresignPut(ctx, f.key, h.opt.URLTTL)
		if err != nil {
			return nil, err
		}
		return &target{Type: "single", Method: http.MethodPut, URL: url, ExpiresAt: expires}, nil
	}
	have := map[int]bool{}
	t := &target{Type: "multipart", Method: http.MethodPut, PartSize: f.partSize, PartCount: partCount(f.size, f.partSize), ExpiresAt: expires}
	for _, p := range good {
		have[p.Number] = true
		t.UploadedParts = append(t.UploadedParts, p.Number)
	}
	for n := 1; n <= t.PartCount; n++ {
		if have[n] {
			continue
		}
		url, err := h.blobs.PresignPart(ctx, f.key, f.multipart, n, h.opt.URLTTL)
		if err != nil {
			return nil, err
		}
		t.Parts = append(t.Parts, partURL{Number: n, URL: url})
	}
	return t, nil
}

func jobFiles(ctx context.Context, pool *pgxpool.Pool, jobID string) ([]fileRow, error) {
	rows, err := pool.Query(ctx, `SELECT name, kind, storage_key, multipart_id, size_bytes, part_size FROM import_files WHERE job_id = $1 ORDER BY storage_key`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []fileRow
	for rows.Next() {
		var f fileRow
		if err := rows.Scan(&f.name, &f.kind, &f.key, &f.multipart, &f.size, &f.partSize); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// fileViews describes the files of a job that still waits for them, with a fresh link for each missing one.
func (h *Handler) fileViews(ctx context.Context, jobID string) ([]fileView, error) {
	files, err := jobFiles(ctx, h.pool, jobID)
	if err != nil {
		return nil, err
	}
	out := []fileView{}
	for _, f := range files {
		done, good, err := h.state(ctx, f)
		if err != nil {
			return nil, err
		}
		v := fileView{Name: f.name, Kind: f.kind, SizeBytes: f.size, Uploaded: done}
		if !done {
			if v.Upload, err = h.link(ctx, f, good); err != nil {
				return nil, err
			}
		}
		out = append(out, v)
	}
	return out, nil
}

type existingArtist struct {
	id, name string
}

// findArtist decides which artist an import goes to. id "" with a nil error means: create one.
func findArtist(ctx context.Context, db *pgxpool.Pool, spec ArtistSpec) (existingArtist, int, string, error) {
	var a existingArtist
	if spec.ArtistID != "" {
		var banned bool
		err := db.QueryRow(ctx, `SELECT id, name, banned_at IS NOT NULL FROM artists WHERE id = $1`, spec.ArtistID).Scan(&a.id, &a.name, &banned)
		if errors.Is(err, pgx.ErrNoRows) {
			return a, http.StatusNotFound, "artist_not_found", errors.New("there is no artist with that artistId")
		}
		if err != nil {
			return a, 0, "", err
		}
		if banned {
			return a, http.StatusConflict, "artist_banned", errors.New("this artist is banned")
		}
		return a, 0, "", nil
	}
	// By name only an artist without an account is reused: songs are never added to somebody's own profile by accident.
	rows, err := db.Query(ctx, `SELECT id, name, owner_user_id IS NOT NULL, banned_at IS NOT NULL FROM artists
		WHERE lower(btrim(name)) = lower($1) ORDER BY created_at, id`, spec.Name)
	if err != nil {
		return a, 0, "", err
	}
	defer rows.Close()
	var ownedIDs []string
	banned := false
	for rows.Next() {
		var id, name string
		var owned, isBanned bool
		if err := rows.Scan(&id, &name, &owned, &isBanned); err != nil {
			return a, 0, "", err
		}
		switch {
		case isBanned:
			banned = true
		case owned:
			ownedIDs = append(ownedIDs, id)
		case a.id == "":
			a = existingArtist{id, name}
		}
	}
	if err := rows.Err(); err != nil {
		return a, 0, "", err
	}
	switch {
	case a.id != "":
		return a, 0, "", nil
	case banned:
		return a, http.StatusConflict, "artist_banned", errors.New("an artist with this name is banned")
	case len(ownedIDs) > 0:
		return a, http.StatusConflict, "artist_owned", errors.New("an artist with this name has their own account (" + strings.Join(ownedIDs, ", ") + "): send artist.artistId to import into it on purpose")
	}
	return a, 0, "", nil
}

// existingReleases maps the normalised titles of an artist's releases to their ids.
func existingReleases(ctx context.Context, db *pgxpool.Pool, artistID string) (map[string]string, error) {
	out := map[string]string{}
	if artistID == "" {
		return out, nil
	}
	rows, err := db.Query(ctx, `SELECT id, title FROM albums WHERE artist_id = $1 ORDER BY created_at DESC`, artistID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, title string
		if err := rows.Scan(&id, &title); err != nil {
			return nil, err
		}
		out[normalizedTitle(title)] = id // the oldest wins
	}
	return out, rows.Err()
}

type plannedItem struct {
	TrackSpec
	status, releaseID, note string
}

// POST /v1/admin/imports with an Idempotency-Key header.
func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := who(ctx)
	idem := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if n := len(idem); n < 8 || n > 100 {
		httpx.Error(w, http.StatusBadRequest, "idempotency_key_required", "send an Idempotency-Key header (8-100 characters, for example a UUID)")
		return
	}
	// the same key gives the same job back, whatever the body says
	var existing string
	err := h.pool.QueryRow(ctx, `SELECT id FROM import_jobs WHERE created_by::text = $1 AND idempotency_key = $2`, p.UserID, idem).Scan(&existing)
	if err == nil {
		h.respond(w, r, existing, http.StatusOK, true)
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		serverError(w, err)
		return
	}

	var req Request
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_manifest", "invalid manifest JSON: "+err.Error())
		return
	}
	files, problems := check(&req, h.opt.MaxAudioBytes)
	if len(problems) > 0 {
		httpx.ErrorDetails(w, http.StatusUnprocessableEntity, "invalid_manifest", problems[0].Message, map[string]any{"problems": problems})
		return
	}

	artist, status, code, err := findArtist(ctx, h.pool, req.Artist)
	if err != nil {
		if status == 0 {
			serverError(w, err)
			return
		}
		httpx.Error(w, status, code, err.Error())
		return
	}
	artistName := req.Artist.Name
	if artist.id != "" {
		artistName = artist.name
	}
	dupes, err := existingReleases(ctx, h.pool, artist.id)
	if err != nil {
		serverError(w, err)
		return
	}

	// what will be skipped, and therefore which files are really needed
	album := req.Defaults.Type == "album"
	items := make([]plannedItem, len(req.Tracks))
	seen := map[string]bool{}
	needed := map[string]string{} // file name -> kind
	albumDup := ""
	if album {
		albumDup = dupes[normalizedTitle(req.Album.Title)]
	}
	active := 0
	for i, t := range req.Tracks {
		it := plannedItem{TrackSpec: t, status: "pending", note: t.Note}
		key := normalizedTitle(t.Title)
		switch {
		case album && albumDup != "":
			it.status, it.releaseID = "skippedDuplicate", albumDup
		case !album && dupes[key] != "":
			it.status, it.releaseID = "skippedDuplicate", dupes[key]
		case !album && seen[key]:
			it.status = "skippedDuplicate"
			it.note = "listed twice in this import"
		}
		seen[key] = true
		if it.status == "pending" {
			active++
			needed[t.AudioFile] = "audio"
			if !album && t.CoverFile != "" {
				needed[t.CoverFile] = "image"
			}
		}
		items[i] = it
	}
	if album && active > 0 && req.Album.CoverFile != "" {
		needed[req.Album.CoverFile] = "image"
	}
	picture := ""
	if artist.id == "" && active > 0 && req.Artist.PictureFile != "" { // an existing artist's picture is never replaced
		picture = req.Artist.PictureFile
		needed[picture] = "image"
	}

	var total, audioBytes int64
	for name, kind := range needed {
		total += files[name].SizeBytes
		if kind == "audio" {
			audioBytes += files[name].SizeBytes
		}
	}
	if h.opt.MaxTotalBytes > 0 && total > h.opt.MaxTotalBytes {
		httpx.ErrorDetails(w, http.StatusUnprocessableEntity, "import_too_large",
			"one import is at most "+strconv.FormatInt(h.opt.MaxTotalBytes>>20, 10)+" MB: split the catalogue", map[string]int64{"totalBytes": total, "limitBytes": h.opt.MaxTotalBytes})
		return
	}
	if h.opt.MinFreeBytes > 0 && active > 0 {
		if free, ok := h.opt.FreeBytes(); ok {
			need := audioBytes*7/2 + (total - audioBytes) // the originals, the playback copies and room to work
			if free-need < h.opt.MinFreeBytes {
				httpx.ErrorDetails(w, http.StatusInsufficientStorage, "insufficient_storage", "the server does not have enough free disk space for this import",
					map[string]int64{"freeBytes": free, "needBytes": need, "keepFreeBytes": h.opt.MinFreeBytes})
				return
			}
		}
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var jobID string
	var keyID *string
	if p.KeyID != "" {
		keyID = &p.KeyID
	}
	var artistRef *string
	if artist.id != "" {
		artistRef = &artist.id
	}
	al := AlbumSpec{}
	if album {
		al = *req.Album
	}
	albumGenre := ""
	if al.Genre != nil {
		albumGenre = *al.Genre
	}
	status0 := "awaiting_files"
	if active == 0 { // nothing new: the job is already done
		status0 = "completed"
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO import_jobs (created_by, api_key_id, idempotency_key, artist_id, artist_name, artist_bio, picture_file, release_type, publish,
		                         album_title, album_genre, album_cover_file, album_date, rights_basis, status, finished_at)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, CASE WHEN $15 = 'completed' THEN now() END) RETURNING id`,
		p.UserID, keyID, idem, artistRef, artistName, req.Artist.Bio, picture, req.Defaults.Type, req.Defaults.Visibility,
		al.Title, albumGenre, al.CoverFile, req.Defaults.ReleaseDate, req.RightsBasis, status0).Scan(&jobID)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" { // the same key at the same moment
		_ = tx.Rollback(ctx)
		if err := h.pool.QueryRow(ctx, `SELECT id FROM import_jobs WHERE created_by::text = $1 AND idempotency_key = $2`, p.UserID, idem).Scan(&existing); err != nil {
			serverError(w, err)
			return
		}
		h.respond(w, r, existing, http.StatusOK, true)
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	for i, it := range items {
		gen := ""
		if it.Genre != nil {
			gen = *it.Genre
		}
		cover := it.CoverFile
		if album {
			cover = ""
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO import_items (job_id, position, title, genre, explicit, release_date, audio_file, cover_file, note, status, release_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			jobID, i+1, it.Title, gen, it.Explicit, it.ReleaseDate, it.AudioFile, cover, it.note, it.status, it.releaseID); err != nil {
			serverError(w, err)
			return
		}
	}

	// storage places for the files that are needed; the object name never depends on the caller's file name
	var multiparts [][2]string
	committed := false
	defer func() {
		if !committed {
			for _, m := range multiparts {
				_ = h.blobs.AbortMultipart(context.WithoutCancel(ctx), m[0], m[1])
			}
		}
	}()
	n := 0
	for _, f := range req.Files {
		kind, ok := needed[f.Name]
		if !ok {
			continue
		}
		n++
		key := "imports/" + jobID + "/" + strconv.Itoa(n)
		var multipart string
		var partSize int64
		if kind == "audio" && h.opt.PartSize > 0 && f.SizeBytes > 2*h.opt.PartSize {
			partSize = h.opt.PartSize
			if multipart, err = h.blobs.StartMultipart(ctx, key); err != nil {
				serverError(w, err)
				return
			}
			multiparts = append(multiparts, [2]string{key, multipart})
		}
		if _, err := tx.Exec(ctx, `INSERT INTO import_files (job_id, name, kind, size_bytes, storage_key, multipart_id, part_size) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			jobID, f.Name, kind, f.SizeBytes, key, multipart, partSize); err != nil {
			serverError(w, err)
			return
		}
	}
	if err := audit.Write(ctx, tx, audit.Entry{ActorID: p.UserID, Action: "import.create", TargetType: "import", TargetID: jobID, TargetLabel: artistName,
		Details: h.auditDetails(p, map[string]any{"tracks": len(items), "toCreate": active, "bytes": total, "visibility": req.Defaults.Visibility,
			"rightsConfirmed": true, "rightsBasis": req.RightsBasis}), IP: audit.IP(r)}); err != nil {
		serverError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		serverError(w, err)
		return
	}
	committed = true
	h.respond(w, r, jobID, http.StatusCreated, true)
}

// auditDetails adds which key was used.
func (h *Handler) auditDetails(p principal, d map[string]any) map[string]any {
	if p.KeyID != "" {
		d["apiKeyId"], d["apiKeyName"] = p.KeyID, p.KeyName
	}
	return d
}

// visible returns the job's status if the caller may see it: a signed-in administrator sees every job, a key only
// the jobs of the administrator who made it.
func (h *Handler) visible(ctx context.Context, id string) (status string, err error) {
	p := who(ctx)
	var creator string
	err = h.pool.QueryRow(ctx, `SELECT status, created_by::text FROM import_jobs WHERE id = $1`, id).Scan(&status, &creator)
	if err == nil && p.KeyID != "" && creator != p.UserID {
		err = pgx.ErrNoRows
	}
	return status, err
}

func (h *Handler) respond(w http.ResponseWriter, r *http.Request, id string, code int, withFiles bool) {
	v, err := h.view(r.Context(), id)
	if err != nil {
		serverError(w, err)
		return
	}
	if withFiles && v.Status == "pending" {
		files, err := h.fileViews(r.Context(), id)
		if err != nil {
			serverError(w, err)
			return
		}
		v.Files = files
	}
	httpx.JSON(w, code, v)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := h.visible(r.Context(), id); err != nil {
		h.missing(w, err)
		return
	}
	h.respond(w, r, id, http.StatusOK, false)
}

func (h *Handler) missing(w http.ResponseWriter, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, http.StatusNotFound, "not_found", "no such import")
		return
	}
	serverError(w, err)
}

func (h *Handler) links(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	status, err := h.visible(r.Context(), id)
	if err != nil {
		h.missing(w, err)
		return
	}
	if status != "awaiting_files" {
		httpx.Error(w, http.StatusConflict, "not_pending", "this import is no longer waiting for files")
		return
	}
	h.respond(w, r, id, http.StatusOK, true)
}

// start checks that every file arrived (joining the parts of big ones) and hands the job to the worker.
func (h *Handler) start(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	status, err := h.visible(ctx, id)
	if err != nil {
		h.missing(w, err)
		return
	}
	if status != "awaiting_files" {
		h.respond(w, r, id, http.StatusOK, false) // already started: starting again changes nothing
		return
	}
	files, err := jobFiles(ctx, h.pool, id)
	if err != nil {
		serverError(w, err)
		return
	}
	missing := []string{}
	for _, f := range files {
		done, good, err := h.state(ctx, f)
		if err != nil {
			serverError(w, err)
			return
		}
		if !done && f.multipart != "" && len(good) == partCount(f.size, f.partSize) {
			if err := h.blobs.CompleteMultipart(ctx, f.key, f.multipart, sortedParts(good)); err != nil {
				serverError(w, err)
				return
			}
			done = true
		}
		if !done {
			missing = append(missing, f.name)
			continue
		}
		size, err := h.blobs.Stat(ctx, f.key)
		if err != nil {
			serverError(w, err)
			return
		}
		if _, err := h.pool.Exec(ctx, `UPDATE import_files SET actual_bytes = $3 WHERE job_id = $1 AND name = $2`, id, f.name, size); err != nil {
			serverError(w, err)
			return
		}
	}
	if len(missing) > 0 {
		httpx.ErrorDetails(w, http.StatusConflict, "files_missing", "some files have not arrived (or are larger than declared)", map[string]any{"missing": missing})
		return
	}
	if _, err := h.pool.Exec(ctx, `UPDATE import_jobs SET status = 'processing', started_at = now() WHERE id = $1 AND status = 'awaiting_files'`, id); err != nil {
		serverError(w, err)
		return
	}
	h.respond(w, r, id, http.StatusAccepted, false)
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	if _, err := h.visible(ctx, id); err != nil {
		h.missing(w, err)
		return
	}
	tag, err := h.pool.Exec(ctx, `UPDATE import_jobs SET status = 'cancelled', finished_at = now() WHERE id = $1 AND status = 'awaiting_files'`, id)
	if err != nil {
		serverError(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.Error(w, http.StatusConflict, "not_pending", "only an import that has not started can be cancelled")
		return
	}
	files, err := jobFiles(ctx, h.pool, id)
	if err != nil {
		serverError(w, err)
		return
	}
	removeStaged(ctx, h.blobs, files)
	p := who(ctx)
	_ = audit.Write(ctx, h.pool, audit.Entry{ActorID: p.UserID, Action: "import.cancel", TargetType: "import", TargetID: id, Details: h.auditDetails(p, map[string]any{}), IP: audit.IP(r)})
	w.WriteHeader(http.StatusNoContent)
}

type stagedBlobs interface {
	Remove(ctx context.Context, key string) error
	AbortMultipart(ctx context.Context, key, uploadID string) error
}

func removeStaged(ctx context.Context, blobs stagedBlobs, files []fileRow) {
	for _, f := range files {
		if f.multipart != "" {
			_ = blobs.AbortMultipart(ctx, f.key, f.multipart)
		}
		_ = blobs.Remove(ctx, f.key)
	}
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := who(ctx)
	mine := ""
	if p.KeyID != "" {
		mine = p.UserID
	}
	rows, err := h.pool.Query(ctx, `SELECT id FROM import_jobs WHERE $1 = '' OR created_by::text = $1 ORDER BY created_at DESC LIMIT 20`, mine)
	if err != nil {
		serverError(w, err)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			serverError(w, err)
			return
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := []*jobView{}
	for _, id := range ids {
		v, err := h.view(ctx, id)
		if err != nil {
			serverError(w, err)
			return
		}
		v.Results, v.Errors = nil, nil
		out = append(out, v)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"imports": out})
}

// diskFree is the free space of the disk the server runs on (the storage volume lives on the same disk).
func diskFree() (int64, bool) {
	_, _, free, ok := sysmetrics.Disk()
	return free, ok
}
