package upload

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/blacmusic/backend/internal/artist"
	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/genre"
	"github.com/blacmusic/backend/internal/httpx"
	"github.com/blacmusic/backend/internal/review"
	"github.com/blacmusic/backend/internal/storage"
)

// Blobs is the slice of object storage the upload API needs.
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

// Options configures the handler.
type Options struct {
	MaxBytes int64         // largest accepted file
	URLTTL   time.Duration // lifetime of presigned upload URLs
	PartSize int64         // part size of resumable uploads; files over twice this size use them
}

// Tracks removes a finished track; the release store provides it.
type Tracks interface {
	DeleteTrack(ctx context.Context, id, userID string) error
}

// Handler serves /v1/uploads. Uploading needs an artist profile.
type Handler struct {
	// OnAdminComplete, when set, is told when an administrator completed an upload (the admin API writes its audit row).
	OnAdminComplete func(r *http.Request, u *Upload)
	store           *Store
	blobs           Blobs
	opt             Options
	Tracks          Tracks // set to enable replacing the file of a draft track
}

// NewHandler creates the handler.
func NewHandler(store *Store, blobs Blobs, opt Options) *Handler {
	return &Handler{store: store, blobs: blobs, opt: opt}
}

// Routes registers the routes on r (already authenticated).
func (h *Handler) Routes(r chi.Router) {
	r.Post("/", h.create)
	r.Get("/", h.list)
	r.Get("/{id}", h.get)
	r.Get("/{id}/parts", h.parts)
	r.Post("/{id}/complete", h.complete)
	r.Delete("/{id}", h.delete)
}

// TrackRoutes registers the track routes that start uploads.
func (h *Handler) TrackRoutes(r chi.Router) {
	r.Post("/tracks/{id}/replace-audio", h.ReplaceAudio)
}

type createRequest struct {
	admin          bool   // set by AdminCreate: an administrator uploads for the release artist
	ReleaseID      string `json:"releaseId"`
	Title          string `json:"title"`
	Genre          string `json:"genre"`
	TrackNumber    *int   `json:"trackNumber"`
	Explicit       bool   `json:"explicit"`
	Filename       string `json:"filename"`
	SizeBytes      int64  `json:"sizeBytes"`
	RightsAccepted bool   `json:"rightsAccepted"`
	TermsVersion   string `json:"termsVersion"`
}

// partTarget is a presigned URL for one part.
type partTarget struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
}

// uploadTarget tells the client where and how to send the file. For
// type "single" PUT the whole file to url; for "multipart" PUT each part to its
// own URL, then complete.
type uploadTarget struct {
	Type      string       `json:"type"` // single | multipart
	Method    string       `json:"method"`
	URL       string       `json:"url,omitempty"`
	PartSize  int64        `json:"partSize,omitempty"`
	PartCount int          `json:"partCount,omitempty"`
	Parts     []partTarget `json:"parts,omitempty"`
	ExpiresAt time.Time    `json:"expiresAt"`
}

type createResponse struct {
	view
	Upload   uploadTarget `json:"upload"`
	MaxBytes int64        `json:"maxBytes"`
}

func partCount(size, partSize int64) int { return int((size + partSize - 1) / partSize) }

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	var req createRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if resp, ok := h.start(w, r, uid, req); ok {
		httpx.JSON(w, http.StatusCreated, resp)
	}
}

// start validates and reserves an upload and prepares where the file goes. On any problem it writes
// the HTTP error itself and reports false.
func (h *Handler) start(w http.ResponseWriter, r *http.Request, uid string, req createRequest) (*createResponse, bool) {
	req.Title, req.Genre = strings.TrimSpace(req.Title), strings.TrimSpace(req.Genre)
	req.Filename, req.TermsVersion = strings.TrimSpace(req.Filename), strings.TrimSpace(req.TermsVersion)

	if msg := h.validate(req); msg != "" {
		httpx.Error(w, http.StatusUnprocessableEntity, "validation_error", msg)
		return nil, false
	}
	var gerr error
	if req.Genre, gerr = genre.Normalize(req.Genre); gerr != nil {
		httpx.Error(w, http.StatusUnprocessableEntity, "invalid_genre", gerr.Error())
		return nil, false
	}
	if !req.RightsAccepted {
		httpx.Error(w, http.StatusUnprocessableEntity, "rights_not_accepted", "you must confirm that you own the rights to this content")
		return nil, false
	}

	var partSize int64
	if h.opt.PartSize > 0 && req.SizeBytes > 2*h.opt.PartSize {
		partSize = h.opt.PartSize
	}
	u, err := h.store.Create(r.Context(), NewUpload{
		AsAdmin: req.admin,
		UserID:  uid, ReleaseID: req.ReleaseID, Title: req.Title, Genre: req.Genre, TrackNumber: req.TrackNumber,
		Explicit: req.Explicit, Filename: req.Filename, SizeBytes: req.SizeBytes, TermsVersion: req.TermsVersion,
		PartSize: partSize,
	})
	var quota *QuotaError
	switch {
	case errors.As(err, &quota):
		httpx.ErrorDetails(w, http.StatusPaymentRequired, "quota_exceeded",
			"this file does not fit in your remaining storage", map[string]int64{
				"requiredBytes": quota.Required, "remainingBytes": quota.Remaining,
				"limitBytes": quota.Limit, "usedBytes": quota.Used,
			})
		return nil, false
	case errors.Is(err, review.ErrPending):
		httpx.Error(w, http.StatusConflict, "review_pending", review.Message)
		return nil, false
	case errors.Is(err, ErrArtistBanned):
		httpx.Error(w, http.StatusForbidden, artist.CodeSuspended, "this artist profile has been suspended")
		return nil, false
	case errors.Is(err, ErrNoArtist):
		httpx.Error(w, http.StatusForbidden, "artist_required", "create your artist profile before uploading")
		return nil, false
	case errors.Is(err, ErrReleaseNotFound):
		httpx.Error(w, http.StatusNotFound, "not_found", "release not found")
		return nil, false
	case errors.Is(err, ErrLimit):
		httpx.Error(w, http.StatusConflict, "limit_reached", "too many unfinished uploads; wait for them to finish")
		return nil, false
	case errors.Is(err, ErrDailyLimit):
		w.Header().Set("Retry-After", "3600")
		httpx.Error(w, http.StatusTooManyRequests, "daily_limit", "daily upload limit reached; request verification to lift it")
		return nil, false
	case err != nil:
		serverError(w, err)
		return nil, false
	}

	target, err := h.newTarget(r.Context(), u)
	if err != nil {
		// Nothing was handed to the client yet: take the reservation back.
		if st, derr := h.store.Delete(r.Context(), u.ID, uid); derr == nil {
			h.cleanup(r.Context(), st)
		}
		serverError(w, err)
		return nil, false
	}
	return &createResponse{view: toView(u), Upload: *target, MaxBytes: h.opt.MaxBytes}, true
}

// newTarget prepares the object store side of a fresh upload.
func (h *Handler) newTarget(ctx context.Context, u *Upload) (*uploadTarget, error) {
	expires := time.Now().Add(h.opt.URLTTL).UTC()
	if u.PartSize == 0 {
		url, err := h.blobs.PresignPut(ctx, u.OriginalKey, h.opt.URLTTL)
		if err != nil {
			return nil, err
		}
		return &uploadTarget{Type: "single", Method: http.MethodPut, URL: url, ExpiresAt: expires}, nil
	}
	id, err := h.blobs.StartMultipart(ctx, u.OriginalKey)
	if err != nil {
		return nil, err
	}
	u.MultipartID = id
	if err := h.store.SetMultipart(ctx, u.ID, id); err != nil {
		_ = h.blobs.AbortMultipart(ctx, u.OriginalKey, id)
		return nil, err
	}
	n := partCount(u.SizeBytes, u.PartSize)
	all := make([]int, n)
	for i := range all {
		all[i] = i + 1
	}
	return h.multipartTarget(ctx, u, all)
}

// multipartTarget presigns the given part numbers.
func (h *Handler) multipartTarget(ctx context.Context, u *Upload, numbers []int) (*uploadTarget, error) {
	t := &uploadTarget{
		Type: "multipart", Method: http.MethodPut,
		PartSize: u.PartSize, PartCount: partCount(u.SizeBytes, u.PartSize),
		ExpiresAt: time.Now().Add(h.opt.URLTTL).UTC(),
		Parts:     make([]partTarget, 0, len(numbers)),
	}
	for _, n := range numbers {
		url, err := h.blobs.PresignPart(ctx, u.OriginalKey, u.MultipartID, n, h.opt.URLTTL)
		if err != nil {
			return nil, err
		}
		t.Parts = append(t.Parts, partTarget{Number: n, URL: url})
	}
	return t, nil
}

func (h *Handler) validate(req createRequest) string {
	switch {
	case utf8.RuneCountInString(req.Title) < 1 || utf8.RuneCountInString(req.Title) > 200:
		return "title must be 1-200 characters"
	case utf8.RuneCountInString(req.Genre) > 40:
		return "genre must be at most 40 characters"
	case req.TrackNumber != nil && (*req.TrackNumber < 1 || *req.TrackNumber > 999):
		return "trackNumber must be between 1 and 999"
	case utf8.RuneCountInString(req.Filename) < 1 || utf8.RuneCountInString(req.Filename) > 255:
		return "filename must be 1-255 characters"
	case utf8.RuneCountInString(req.TermsVersion) < 1 || utf8.RuneCountInString(req.TermsVersion) > 32:
		return "termsVersion must be 1-32 characters"
	case req.SizeBytes < 1:
		return "sizeBytes is required"
	case req.SizeBytes > h.opt.MaxBytes:
		return "file is too large (limit " + strconv.FormatInt(h.opt.MaxBytes>>20, 10) + " MB)"
	}
	return ""
}

type uploadedPart struct {
	Number int   `json:"number"`
	Size   int64 `json:"size"`
}

// parts reports how far a multipart upload got and returns fresh URLs for the
// parts still to send, so an app can pick up after a dropped connection, an
// app restart, or expired URLs.
func (h *Handler) parts(w http.ResponseWriter, r *http.Request) {
	u, ok := h.owned(w, r)
	if !ok {
		return
	}
	if u.PartSize == 0 {
		httpx.Error(w, http.StatusBadRequest, "not_multipart", "this upload is a single PUT; there are no parts")
		return
	}
	if u.Status != StatusPending || u.MultipartID == "" {
		httpx.Error(w, http.StatusConflict, "not_pending", "this upload is no longer waiting for parts")
		return
	}
	have, err := h.blobs.ListParts(r.Context(), u.OriginalKey, u.MultipartID)
	if errors.Is(err, storage.ErrNotFound) {
		httpx.Error(w, http.StatusConflict, "expired", "this upload was discarded; start a new one")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	done, missing, wrong := h.checkParts(u, have)
	_ = wrong // wrong-size parts are simply offered again below
	todo := append(append([]int{}, missing...), wrong...)
	slices.Sort(todo)
	target, err := h.multipartTarget(r.Context(), u, todo)
	if err != nil {
		serverError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"partSize": u.PartSize, "partCount": target.PartCount,
		"uploaded": done, "parts": target.Parts, "expiresAt": target.ExpiresAt,
	})
}

// checkParts compares what the object store has with what the upload needs:
// every part must be partSize bytes except the last, which holds the rest.
func (h *Handler) checkParts(u *Upload, have []storage.Part) (done []uploadedPart, missing, wrong []int) {
	n := partCount(u.SizeBytes, u.PartSize)
	byNumber := make(map[int]storage.Part, len(have))
	for _, p := range have {
		byNumber[p.Number] = p
	}
	done = []uploadedPart{}
	for i := 1; i <= n; i++ {
		want := u.PartSize
		if i == n {
			want = u.SizeBytes - u.PartSize*int64(n-1)
		}
		p, ok := byNumber[i]
		switch {
		case !ok:
			missing = append(missing, i)
		case p.Size != want:
			wrong = append(wrong, i)
		default:
			done = append(done, uploadedPart{Number: i, Size: p.Size})
		}
	}
	return done, missing, wrong
}

// complete is called by the client after its upload finished. For a multipart
// upload the server looks at the parts itself (the client does not send ETags),
// refuses to assemble an incomplete file, and tells the client which parts to
// send again. Once the file is in place it is handed to the worker. Calling it
// again is harmless.
func (h *Handler) complete(w http.ResponseWriter, r *http.Request) {
	u, ok := h.owned(w, r)
	if !ok {
		return
	}
	if u.Status != StatusPending {
		httpx.JSON(w, http.StatusOK, toView(u)) // already queued, processing or finished
		return
	}

	if u.MultipartID != "" {
		if !h.assemble(w, r, u) {
			return
		}
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
	// The reserved storage is what was declared, so a file may not be bigger.
	if size < 1 || size > u.SizeBytes {
		msg := "the uploaded file is empty"
		if size > u.SizeBytes {
			msg = "the uploaded file is larger than the size that was declared"
		}
		_ = h.blobs.Remove(r.Context(), u.OriginalKey)
		if err := h.store.Fail(r.Context(), u.ID, msg); err != nil {
			serverError(w, err)
			return
		}
		httpx.Error(w, http.StatusUnprocessableEntity, "validation_error", msg)
		return
	}

	u, err = h.store.Queue(r.Context(), u.ID, size)
	if err != nil {
		serverError(w, err)
		return
	}
	if h.OnAdminComplete != nil && isAdmin(r.Context()) {
		h.OnAdminComplete(r, u)
	}
	httpx.JSON(w, http.StatusAccepted, toView(u))
}

// assemble checks and joins the parts of a multipart upload. It writes the
// error response and returns false when the upload cannot be completed yet.
func (h *Handler) assemble(w http.ResponseWriter, r *http.Request, u *Upload) bool {
	have, err := h.blobs.ListParts(r.Context(), u.OriginalKey, u.MultipartID)
	if errors.Is(err, storage.ErrNotFound) {
		// A previous attempt may have assembled the file and then failed to
		// record it. If the object is there, carry on; otherwise it is gone.
		if _, serr := h.blobs.Stat(r.Context(), u.OriginalKey); serr == nil {
			return true
		}
		httpx.Error(w, http.StatusConflict, "expired", "this upload was discarded; start a new one")
		return false
	}
	if err != nil {
		serverError(w, err)
		return false
	}
	_, missing, wrong := h.checkParts(u, have)
	if len(missing) > 0 || len(wrong) > 0 {
		httpx.ErrorDetails(w, http.StatusConflict, "parts_missing",
			"some parts have not been uploaded (or have the wrong size); send them again and complete once more",
			map[string]any{"missing": nonNil(missing), "wrongSize": nonNil(wrong), "partSize": u.PartSize})
		return false
	}
	n := partCount(u.SizeBytes, u.PartSize)
	byNumber := make(map[int]storage.Part, len(have))
	for _, p := range have {
		byNumber[p.Number] = p
	}
	ordered := make([]storage.Part, 0, n)
	for i := 1; i <= n; i++ {
		ordered = append(ordered, byNumber[i])
	}
	if err := h.blobs.CompleteMultipart(r.Context(), u.OriginalKey, u.MultipartID, ordered); err != nil {
		serverError(w, err)
		return false
	}
	return true
}

func nonNil(a []int) []int {
	if a == nil {
		return []int{}
	}
	return a
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	if u, ok := h.owned(w, r); ok {
		httpx.JSON(w, http.StatusOK, toView(u))
	}
}

// delete cancels an upload that has not become a track (pending or failed),
// which also returns its reserved storage. A finished upload is removed by
// deleting its track.
func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	var st *Stale
	var err error
	if isAdmin(r.Context()) {
		st, err = h.store.DeleteAny(r.Context(), chi.URLParam(r, "id"))
	} else {
		st, err = h.store.Delete(r.Context(), chi.URLParam(r, "id"), uid)
	}
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, http.StatusNotFound, "not_found", "upload not found")
	case errors.Is(err, ErrInUse):
		httpx.Error(w, http.StatusConflict, "in_progress", "this upload is already being processed or finished; delete the track instead")
	case err != nil:
		serverError(w, err)
	default:
		h.cleanup(r.Context(), st)
		w.WriteHeader(http.StatusNoContent)
	}
}

// cleanup deletes what an upload left in storage: any unfinished multipart
// upload (its parts cost storage until aborted) and the object itself.
func (h *Handler) cleanup(ctx context.Context, st *Stale) {
	if st.MultipartID != "" {
		if err := h.blobs.AbortMultipart(ctx, st.Key, st.MultipartID); err != nil {
			slog.Error("abort multipart upload", "key", st.Key, "err", err)
		}
	}
	_ = h.blobs.Remove(ctx, st.Key)
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
	if errors.Is(err, ErrNotFound) || (err == nil && u.UserID != uid && !isAdmin(r.Context())) {
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
	ID          string    `json:"id"`
	Status      Status    `json:"status"`
	ReleaseID   string    `json:"releaseId,omitempty"`
	Title       string    `json:"title"`
	Genre       string    `json:"genre,omitempty"`
	TrackNumber *int      `json:"trackNumber,omitempty"`
	Explicit    bool      `json:"explicit"`
	Filename    string    `json:"filename"`
	SizeBytes   int64     `json:"sizeBytes"`
	PartSize    int64     `json:"partSize,omitempty"`  // resumable uploads only
	PartCount   int       `json:"partCount,omitempty"` // resumable uploads only
	TrackID     string    `json:"trackId,omitempty"`   // set once ready
	Error       string    `json:"error,omitempty"`     // set when failed
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

func toView(u *Upload) view {
	v := view{
		ID: u.ID, Status: u.Status, ReleaseID: u.ReleaseID, Title: u.Title, Genre: u.Genre, TrackNumber: u.TrackNumber,
		Explicit: u.Explicit, Filename: u.Filename, SizeBytes: u.SizeBytes, Error: u.Error,
		CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt,
	}
	if u.PartSize > 0 {
		v.PartSize, v.PartCount = u.PartSize, partCount(u.SizeBytes, u.PartSize)
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

type replaceRequest struct {
	Filename       string `json:"filename"`
	SizeBytes      int64  `json:"sizeBytes"`
	RightsAccepted bool   `json:"rightsAccepted"`
	TermsVersion   string `json:"termsVersion"`
}

type replaceResponse struct {
	createResponse
	ReplacedTrackID string `json:"replacedTrackId"`
}

// ReplaceAudio swaps the file of a track that is still in a draft: it starts an upload carrying the
// old track's title, genre, number and explicit flag into the same release, then removes the old
// track and its storage. The new upload is reserved first, so a rejected file (too big, over quota,
// not accepted) leaves the existing track untouched.
func (h *Handler) ReplaceAudio(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	trackID := chi.URLParam(r, "id")
	var body replaceRequest
	if err := httpx.Decode(w, r, &body); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if h.Tracks == nil {
		httpx.Error(w, http.StatusNotImplemented, "unavailable", "replacing a file is not available")
		return
	}
	old, err := h.store.DraftTrack(r.Context(), trackID, uid)
	switch {
	case errors.Is(err, ErrTrackNotFound):
		httpx.Error(w, http.StatusNotFound, "not_found", "track not found")
		return
	case errors.Is(err, ErrTrackNotDraft):
		httpx.Error(w, http.StatusConflict, "not_draft", "the file can only be replaced while the release is a draft")
		return
	case err != nil:
		serverError(w, err)
		return
	}
	if _, gerr := genre.Normalize(old.Genre); gerr != nil {
		old.Genre = "" // an older track may carry a genre that is no longer on the list
	}
	resp, ok := h.start(w, r, uid, createRequest{
		ReleaseID: old.ReleaseID, Title: old.Title, Genre: old.Genre, TrackNumber: old.TrackNumber, Explicit: old.Explicit,
		Filename: body.Filename, SizeBytes: body.SizeBytes, RightsAccepted: body.RightsAccepted, TermsVersion: body.TermsVersion,
	})
	if !ok {
		return
	}
	if err := h.Tracks.DeleteTrack(r.Context(), trackID, uid); err != nil {
		// Keep what the artist had: give the new reservation back.
		if st, derr := h.store.Delete(r.Context(), resp.ID, uid); derr == nil {
			h.cleanup(r.Context(), st)
		}
		serverError(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, replaceResponse{createResponse: *resp, ReplacedTrackID: trackID})
}

type adminKey struct{}

// AsAdmin marks the request as made by an administrator, so the routes below accept any upload. Only put it
// behind the admin check.
func AsAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), adminKey{}, true)))
	})
}

func isAdmin(ctx context.Context) bool {
	v, _ := ctx.Value(adminKey{}).(bool)
	return v
}

// AdminRoutes registers, on the admin router, the routes an administrator uses to follow, resume, finish or
// cancel any upload:
//
//	GET    /{id}            status
//	GET    /{id}/parts      what arrived of a multipart upload, with fresh URLs for the rest
//	POST   /{id}/complete
//	DELETE /{id}
func (h *Handler) AdminRoutes(r chi.Router) {
	r.Use(AsAdmin)
	r.Get("/{id}", h.get)
	r.Get("/{id}/parts", h.parts)
	r.Post("/{id}/complete", h.complete)
	r.Delete("/{id}", h.delete)
}

// AdminCreateInput describes a file an administrator uploads into a release.
type AdminCreateInput struct {
	ReleaseID   string
	Title       string
	Genre       string
	TrackNumber *int
	Explicit    bool
	Filename    string
	SizeBytes   int64
}

// AdminCreate reserves an upload in any release, with the rights confirmed by the administrator. On success it
// returns the upload id and a send function that writes the 201 answer (the same body as POST /v1/uploads), so
// the caller can record its audit row first. On any problem the error answer is already written.
func (h *Handler) AdminCreate(w http.ResponseWriter, r *http.Request, adminID string, in AdminCreateInput) (id string, send func(), ok bool) {
	resp, ok := h.start(w, r, adminID, createRequest{
		admin: true, ReleaseID: in.ReleaseID, Title: in.Title, Genre: in.Genre, TrackNumber: in.TrackNumber, Explicit: in.Explicit,
		Filename: in.Filename, SizeBytes: in.SizeBytes, RightsAccepted: true, TermsVersion: "admin",
	})
	if !ok {
		return "", nil, false
	}
	return resp.ID, func() { httpx.JSON(w, http.StatusCreated, resp) }, true
}
