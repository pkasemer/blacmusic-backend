package admin

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/httpx"
	"github.com/blacmusic/backend/internal/showcase"
)

// The signed-out showcase (the artists and albums on the app's Get started screens):
//
//	GET /v1/admin/showcase                       what is stored, with consent notes and whether each is shown
//	PUT /v1/admin/showcase {items:[{kind, id, consentNote?}]}   replaces the list and its order
func (h *Handler) showcaseRoutes(r chi.Router) {
	if h.Showcase == nil {
		return
	}
	r.Get("/showcase", h.getShowcase)
	r.Put("/showcase", h.putShowcase)
}

func (h *Handler) getShowcase(w http.ResponseWriter, r *http.Request) {
	items, err := h.Showcase.Admin(r.Context())
	if err != nil {
		slog.Error("admin showcase", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal", "something went wrong")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items, "max": showcase.MaxItems})
}

func (h *Handler) putShowcase(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Items []showcase.Input `json:"items"`
	}
	if err := httpx.Decode(w, r, &req); err != nil || req.Items == nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "items is required (use [] to clear the showcase)")
		return
	}
	uid, _ := auth.UserID(r.Context())
	err := h.Showcase.Replace(r.Context(), uid, req.Items)
	var ve *showcase.ValidationError
	switch {
	case errors.As(err, &ve):
		httpx.Error(w, http.StatusUnprocessableEntity, "validation_error", ve.Msg)
		return
	case err != nil:
		slog.Error("admin showcase", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal", "something went wrong")
		return
	}
	h.getShowcase(w, r)
}
