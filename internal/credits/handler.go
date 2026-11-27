package credits

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/httpx"
	"github.com/blacmusic/backend/internal/review"
)

// Handler serves the credits endpoints (authenticated).
type Handler struct{ store *Store }

// NewHandler creates the handler.
func NewHandler(s *Store) *Handler { return &Handler{store: s} }

// Routes registers, on a router already behind authentication:
//
//	GET /releases/{id}/credits                 -> {"credits":[...]}   (a release the viewer may see)
//	PUT /releases/{id}/credits {"credits":[]}  -> the same            (owner only; replaces the whole list)
//	GET /tracks/{id}/credits                   -> {"credits":[...]}   (the track's own, then the release's; each with scope)
//	GET /credits/suggest?q=&limit=             -> {"names":[...]}     (names this artist credited before)
func (h *Handler) Routes(r chi.Router) {
	r.Get("/releases/{id}/credits", h.release)
	r.Get("/albums/{id}/credits", h.release)
	r.Put("/releases/{id}/credits", h.put)
	r.Put("/albums/{id}/credits", h.put)
	r.Get("/tracks/{id}/credits", h.track)
	r.Get("/credits/suggest", h.suggest)
}

func (h *Handler) release(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	list, info, err := h.store.OwnerList(r.Context(), chi.URLParam(r, "id"), uid)
	if err != nil {
		fail(w, err)
		return
	}
	reply(w, list, info)
}

func reply(w http.ResponseWriter, list []Credit, info *review.Info) {
	out := map[string]any{"credits": list}
	if info != nil {
		out["review"] = info
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) track(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	list, err := h.store.ForTrack(r.Context(), chi.URLParam(r, "id"), uid)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"credits": list})
}

func (h *Handler) put(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	var req struct {
		Credits []Input `json:"credits"`
	}
	if err := httpx.Decode(w, r, &req); err != nil || req.Credits == nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "send {\"credits\": [...]} (an empty list removes every credit)")
		return
	}
	if _, err := h.store.Replace(r.Context(), chi.URLParam(r, "id"), uid, req.Credits); err != nil {
		fail(w, err)
		return
	}
	list, info, err := h.store.OwnerList(r.Context(), chi.URLParam(r, "id"), uid)
	if err != nil {
		fail(w, err)
		return
	}
	reply(w, list, info)
}

func (h *Handler) suggest(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	limit := 10
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = min(n, 30)
	}
	names, err := h.store.Suggest(r.Context(), uid, r.URL.Query().Get("q"), limit)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"names": names})
}

func fail(w http.ResponseWriter, err error) {
	var ve *ValidationError
	switch {
	case errors.Is(err, review.ErrPending):
		httpx.Error(w, http.StatusConflict, "review_pending", review.Message)
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, ErrNoArtist):
		httpx.Error(w, http.StatusForbidden, "artist_required", "create your artist profile first")
	case errors.As(err, &ve):
		httpx.Error(w, http.StatusUnprocessableEntity, "validation_error", ve.Msg)
	default:
		slog.Error("credits", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal", "something went wrong")
	}
}
