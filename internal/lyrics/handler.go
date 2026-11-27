package lyrics

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/httpx"
	"github.com/blacmusic/backend/internal/review"
)

// Handler serves the lyrics endpoints (authenticated).
type Handler struct{ store *Store }

// NewHandler creates the handler.
func NewHandler(s *Store) *Handler { return &Handler{store: s} }

// Routes registers, on a router already behind authentication:
//
//	GET    /tracks/{id}/lyrics                 -> {"lyrics":{synced, lines:[{timeMs,text}], text}}   404 when none
//	PUT    /tracks/{id}/lyrics {text?, lrc?}   -> the same (the artist's own tracks, draft or live)
//	DELETE /tracks/{id}/lyrics                 -> 204
func (h *Handler) Routes(r chi.Router) {
	r.Get("/tracks/{id}/lyrics", h.get)
	r.Put("/tracks/{id}/lyrics", h.put)
	r.Delete("/tracks/{id}/lyrics", h.remove)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	l, info, err := h.store.ForOwner(r.Context(), chi.URLParam(r, "id"), uid)
	if err != nil {
		fail(w, err)
		return
	}
	reply(w, l, info)
}

func reply(w http.ResponseWriter, l *Lyrics, info *review.Info) {
	out := map[string]any{"lyrics": l}
	if info != nil {
		out["review"] = info
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) put(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	var in Input
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	l, err := h.store.Put(r.Context(), chi.URLParam(r, "id"), uid, in)
	if err != nil {
		fail(w, err)
		return
	}
	// the answer is what they sent, saying so when it waits
	_, info, err := h.store.ForOwner(r.Context(), chi.URLParam(r, "id"), uid)
	if err != nil && !errors.Is(err, ErrNotFound) {
		fail(w, err)
		return
	}
	reply(w, l, info)
}

func (h *Handler) remove(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	if err := h.store.Delete(r.Context(), chi.URLParam(r, "id"), uid); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Invalid answers a refused save, with every wrong line when the lyrics are timed.
func Invalid(w http.ResponseWriter, ve *ValidationError) {
	if len(ve.Lines) == 0 {
		httpx.Error(w, http.StatusUnprocessableEntity, "validation_error", ve.Msg)
		return
	}
	httpx.ErrorDetails(w, http.StatusUnprocessableEntity, "validation_error", ve.Msg, map[string]any{"errors": ve.Lines})
}

func fail(w http.ResponseWriter, err error) {
	var ve *ValidationError
	switch {
	case errors.Is(err, review.ErrPending):
		httpx.Error(w, http.StatusConflict, "review_pending", review.Message)
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, http.StatusNotFound, "not_found", "no lyrics for this track")
	case errors.As(err, &ve):
		Invalid(w, ve)
	default:
		slog.Error("lyrics", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal", "something went wrong")
	}
}
