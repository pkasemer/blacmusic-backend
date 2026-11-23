package podcast

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/httpx"
)

// Handler serves the podcast endpoints. Every route requires authentication.
type Handler struct{ store *Store }

// NewHandler creates the handler.
func NewHandler(store *Store) *Handler { return &Handler{store: store} }

// Routes registers the routes on r (already authenticated). addLimit, when
// non-nil, wraps POST /shows (it makes the server fetch a URL, so it is
// rate limited).
func (h *Handler) Routes(r chi.Router, addLimit func(http.Handler) http.Handler) {
	r.With(addLimit).Post("/shows", h.addShow)
	r.Get("/shows/search", h.searchShows)
	r.Get("/shows/{id}", h.getShow)
	r.Get("/shows/{id}/episodes", h.listEpisodes)
	r.Get("/episodes/{id}", h.getEpisode)
	r.Put("/episodes/{id}/progress", h.setProgress)

	r.Get("/me/shows", h.listSubscriptions)
	r.Put("/me/shows/{id}", h.subscribe)
	r.Delete("/me/shows/{id}", h.unsubscribe)
	r.Get("/me/episodes/in-progress", h.inProgress)
}

func (h *Handler) addShow(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	var req struct {
		FeedURL string `json:"feedUrl"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	show, created, err := h.store.AddFeed(r.Context(), uid, req.FeedURL)
	if err != nil {
		fail(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	httpx.JSON(w, status, show)
}

func (h *Handler) searchShows(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "q is required")
		return
	}
	limit, offset := page(r, 20, 50)
	shows, err := h.store.SearchShows(r.Context(), uid, q, limit, offset)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"shows": shows})
}

func (h *Handler) getShow(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	show, err := h.store.GetShow(r.Context(), chi.URLParam(r, "id"), uid)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, show)
}

func (h *Handler) listEpisodes(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	limit, offset := page(r, 50, 200)
	order := r.URL.Query().Get("order")
	if order != "" && order != "newest" && order != "oldest" {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "order must be newest or oldest")
		return
	}
	eps, err := h.store.ListEpisodes(r.Context(), chi.URLParam(r, "id"), uid, order == "oldest", limit, offset)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"episodes": eps})
}

func (h *Handler) getEpisode(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	ep, err := h.store.GetEpisode(r.Context(), chi.URLParam(r, "id"), uid)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, ep)
}

const maxPositionMs = 48 * 60 * 60 * 1000

func (h *Handler) setProgress(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	var req struct {
		PositionMs *int  `json:"positionMs"`
		DurationMs *int  `json:"durationMs"`
		Completed  *bool `json:"completed"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.PositionMs == nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "positionMs is required")
		return
	}
	if *req.PositionMs < 0 || *req.PositionMs > maxPositionMs ||
		(req.DurationMs != nil && (*req.DurationMs < 0 || *req.DurationMs > maxPositionMs)) {
		httpx.Error(w, http.StatusUnprocessableEntity, "validation_error", "positionMs and durationMs must be between 0 and 48 hours")
		return
	}
	err := h.store.SetProgress(r.Context(), uid, chi.URLParam(r, "id"),
		ProgressUpdate{PositionMs: *req.PositionMs, DurationMs: req.DurationMs, Completed: req.Completed})
	if err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) listSubscriptions(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	limit, offset := page(r, 100, 200)
	shows, err := h.store.ListSubscriptions(r.Context(), uid, limit, offset)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"shows": shows})
}

func (h *Handler) subscribe(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	if err := h.store.Subscribe(r.Context(), uid, chi.URLParam(r, "id")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) unsubscribe(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	if err := h.store.Unsubscribe(r.Context(), uid, chi.URLParam(r, "id")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) inProgress(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	limit, _ := page(r, 20, 100)
	eps, err := h.store.InProgress(r.Context(), uid, limit)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"episodes": eps})
}

func page(r *http.Request, def, max int) (limit, offset int) {
	limit = def
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = min(n, max)
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && n > 0 {
		offset = min(n, 100000)
	}
	return
}

func fail(w http.ResponseWriter, err error) {
	var fe *FeedError
	switch {
	case errors.As(err, &fe):
		httpx.Error(w, http.StatusUnprocessableEntity, "invalid_feed", fe.Msg)
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, ErrLimit):
		httpx.Error(w, http.StatusConflict, "limit_reached", "subscription limit reached")
	default:
		slog.Error("podcast handler", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal", "something went wrong")
	}
}
