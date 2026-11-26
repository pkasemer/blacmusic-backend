package stats

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/httpx"
)

// Handler serves the plays and statistics endpoints (authenticated).
type Handler struct{ store *Store }

// NewHandler creates the handler.
func NewHandler(s *Store) *Handler { return &Handler{store: s} }

// Routes registers, on a router already behind authentication (limit wraps the play endpoint):
//
//	POST /me/plays {trackId, listenedSec, source?}        -> 204
//	GET  /me/artist/stats?range=7d|28d|90d|all            -> ArtistStats (artists only)
//	GET  /releases/{id}/stats?range=...                   -> ReleaseStats (the owner only)
func (h *Handler) Routes(r chi.Router, limit func(http.Handler) http.Handler) {
	r.With(limit).Post("/me/plays", h.play)
	r.Get("/me/artist/stats", h.artist)
	r.Get("/releases/{id}/stats", h.release)
	r.Get("/albums/{id}/stats", h.release)
}

func (h *Handler) play(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	var in PlayInput
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if _, err := h.store.Record(r.Context(), uid, in); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) artist(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	s, err := h.store.Artist(r.Context(), uid, r.URL.Query().Get("range"), time.Now())
	if err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, s)
}

func (h *Handler) release(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	s, err := h.store.Release(r.Context(), uid, chi.URLParam(r, "id"), r.URL.Query().Get("range"), time.Now())
	if err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, s)
}

func fail(w http.ResponseWriter, err error) {
	var ve *ValidationError
	switch {
	case errors.Is(err, ErrTrackNotFound):
		httpx.Error(w, http.StatusNotFound, "not_found", "track not found")
	case errors.Is(err, ErrNoArtist):
		httpx.Error(w, http.StatusForbidden, "artist_required", "create your artist profile first")
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, http.StatusNotFound, "not_found", "not found")
	case errors.As(err, &ve):
		httpx.Error(w, http.StatusUnprocessableEntity, "validation_error", ve.Msg)
	default:
		slog.Error("stats", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal", "something went wrong")
	}
}
