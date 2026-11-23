package release

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/httpx"
)

// Handler serves the release and track-management endpoints (authenticated).
type Handler struct{ store *Store }

// NewHandler creates the handler.
func NewHandler(store *Store) *Handler { return &Handler{store: store} }

// Routes registers the routes on r (already authenticated).
func (h *Handler) Routes(r chi.Router) {
	r.Post("/releases", h.create)
	r.Get("/releases/{id}", h.get)
	r.Get("/albums/{id}", h.get) // same thing under the name the listening UI uses
	r.Patch("/releases/{id}", h.update)
	r.Delete("/releases/{id}", h.delete)
	r.Post("/releases/{id}/publish", h.publish)
	r.Post("/releases/{id}/unpublish", h.unpublish)
	r.Get("/me/releases", h.mine)
	r.Get("/artists/{id}/releases", h.byArtist)

	r.Patch("/tracks/{id}", h.updateTrack)
	r.Delete("/tracks/{id}", h.deleteTrack)
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	var req struct {
		Type         string `json:"type"`
		Title        string `json:"title"`
		Description  string `json:"description"`
		Genre        string `json:"genre"`
		ReleaseDate  string `json:"releaseDate"`
		CoverImageID string `json:"coverImageId"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	rel, err := h.store.Create(r.Context(), uid, Input{
		Type: req.Type, Title: req.Title, Description: req.Description, Genre: req.Genre,
		ReleaseDate: req.ReleaseDate, CoverImageID: req.CoverImageID,
	})
	if err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, rel)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	rel, err := h.store.Get(r.Context(), chi.URLParam(r, "id"), uid)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, rel)
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	var req struct {
		Type         *string `json:"type"`
		Title        *string `json:"title"`
		Description  *string `json:"description"`
		Genre        *string `json:"genre"`
		ReleaseDate  *string `json:"releaseDate"`
		CoverImageID *string `json:"coverImageId"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	rel, err := h.store.Update(r.Context(), chi.URLParam(r, "id"), uid, Patch{
		Type: req.Type, Title: req.Title, Description: req.Description, Genre: req.Genre,
		ReleaseDate: req.ReleaseDate, CoverImageID: req.CoverImageID,
	})
	if err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, rel)
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	if err := h.store.Delete(r.Context(), chi.URLParam(r, "id"), uid); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) publish(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	var req struct {
		PublishAt *time.Time `json:"publishAt"`
	}
	if r.ContentLength != 0 { // the body is optional
		if err := httpx.Decode(w, r, &req); err != nil {
			httpx.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON body (publishAt must be an RFC 3339 time)")
			return
		}
	}
	rel, err := h.store.Publish(r.Context(), chi.URLParam(r, "id"), uid, req.PublishAt)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, rel)
}

func (h *Handler) unpublish(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	rel, err := h.store.Unpublish(r.Context(), chi.URLParam(r, "id"), uid)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, rel)
}

func (h *Handler) mine(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	limit, offset := page(r)
	list, err := h.store.ListMine(r.Context(), uid, limit, offset)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"releases": list})
}

func (h *Handler) byArtist(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	limit, offset := page(r)
	list, err := h.store.ListByArtist(r.Context(), chi.URLParam(r, "id"), uid, limit, offset)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"releases": list})
}

func (h *Handler) updateTrack(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	var req struct {
		Title       *string `json:"title"`
		Explicit    *bool   `json:"explicit"`
		Genre       *string `json:"genre"`
		TrackNumber *int    `json:"trackNumber"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	t, err := h.store.UpdateTrack(r.Context(), chi.URLParam(r, "id"), uid,
		TrackPatch{Title: req.Title, Explicit: req.Explicit, Genre: req.Genre, TrackNumber: req.TrackNumber})
	if err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, t)
}

func (h *Handler) deleteTrack(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	if err := h.store.DeleteTrack(r.Context(), chi.URLParam(r, "id"), uid); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func page(r *http.Request) (limit, offset int) {
	limit = 50
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = min(n, 200)
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && n > 0 {
		offset = min(n, 100000)
	}
	return
}

func logRemoveFailure(key string, err error) {
	slog.Error("could not remove object after delete", "key", key, "err", err)
}

func fail(w http.ResponseWriter, err error) {
	var ve *ValidationError
	switch {
	case errors.As(err, &ve):
		httpx.Error(w, http.StatusUnprocessableEntity, "validation_error", ve.Msg)
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, ErrNoArtist):
		httpx.Error(w, http.StatusForbidden, "artist_required", "create your artist profile first")
	case errors.Is(err, ErrUploadsPending):
		httpx.Error(w, http.StatusConflict, "uploads_pending", "some files are still uploading or processing; wait until they are ready")
	case errors.Is(err, ErrProcessing):
		httpx.Error(w, http.StatusConflict, "processing", "a file in this release is being processed right now; try again in a moment")
	default:
		slog.Error("release handler", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal", "something went wrong")
	}
}
