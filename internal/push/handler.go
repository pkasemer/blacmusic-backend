package push

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/httpx"
)

// Handler serves the push endpoints (all authenticated).
type Handler struct{ store *Store }

// NewHandler creates the handler.
func NewHandler(store *Store) *Handler { return &Handler{store: store} }

// Routes registers, on a router already behind authentication:
//
//	POST   /me/push-tokens           {token, platform, deviceName?}  -> 204
//	DELETE /me/push-tokens           {token}                         -> 204
//	GET    /me/notification-settings                                 -> {transfers, releases, releaseEmails}
//	PUT    /me/notification-settings {transfers?, releases?, releaseEmails?} -> {transfers, releases, releaseEmails}
func (h *Handler) Routes(r chi.Router) {
	r.Post("/me/push-tokens", h.register)
	r.Delete("/me/push-tokens", h.remove)
	r.Get("/me/notification-settings", h.getSettings)
	r.Put("/me/notification-settings", h.putSettings)
}

// DevRoutes registers the development outbox (only when PUSH_FAKE is on):
//
//	GET    /dev/push-outbox?limit=  the caller's latest messages, newest first
//	DELETE /dev/push-outbox         clears them
func (h *Handler) DevRoutes(r chi.Router) {
	r.Get("/dev/push-outbox", h.outbox)
	r.Delete("/dev/push-outbox", h.clearOutbox)
}

func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	var req struct {
		Token      string `json:"token"`
		Platform   string `json:"platform"`
		DeviceName string `json:"deviceName"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if err := h.store.Register(r.Context(), uid, req.Token, req.Platform, req.DeviceName); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) remove(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	var req struct {
		Token string `json:"token"`
	}
	if err := httpx.Decode(w, r, &req); err != nil || req.Token == "" {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "token is required")
		return
	}
	if err := h.store.Remove(r.Context(), uid, req.Token); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) getSettings(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	st, err := h.store.Settings(r.Context(), uid)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, st)
}

func (h *Handler) putSettings(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	var req struct {
		Transfers     *bool `json:"transfers"`
		Releases      *bool `json:"releases"`
		ReleaseEmails *bool `json:"releaseEmails"`
	}
	if err := httpx.Decode(w, r, &req); err != nil || (req.Transfers == nil && req.Releases == nil && req.ReleaseEmails == nil) {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "send transfers, releases and/or releaseEmails as true or false")
		return
	}
	st, err := h.store.SetSettings(r.Context(), uid, SettingsPatch{Transfers: req.Transfers, Releases: req.Releases, ReleaseEmails: req.ReleaseEmails})
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, st)
}

func (h *Handler) outbox(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	limit := 50
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = min(n, 200)
	}
	msgs, err := h.store.Outbox(r.Context(), uid, limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"messages": msgs})
}

func (h *Handler) clearOutbox(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	if err := h.store.ClearOutbox(r.Context(), uid); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	var ve *ValidationError
	if errors.As(err, &ve) {
		httpx.Error(w, http.StatusUnprocessableEntity, "validation_error", ve.Msg)
		return
	}
	slog.Error("push handler", "err", err)
	httpx.Error(w, http.StatusInternalServerError, "internal", "something went wrong")
}
