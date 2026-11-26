package importopt

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/blacmusic/backend/internal/httpx"
)

// Handler serves GET /v1/import-options (any signed-in user).
type Handler struct{ store *Store }

// NewHandler creates the handler.
func NewHandler(s *Store) *Handler { return &Handler{store: s} }

// List answers {"options":[{id, enabled, title?, description?, note?}]} in display order.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	opts, err := h.store.List(r.Context())
	if err != nil {
		slog.Error("import options", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal", "something went wrong")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"options": opts})
}

// AdminRoutes registers, on the admin router:
//
//	GET /import-options                       the same list
//	PUT /import-options/{id} {enabled?, title?, description?, note?, position?}   change or add one
func (h *Handler) AdminRoutes(r chi.Router) {
	r.Get("/import-options", h.List)
	r.Put("/import-options/{id}", h.put)
}

func (h *Handler) put(w http.ResponseWriter, r *http.Request) {
	var p Patch
	if err := httpx.Decode(w, r, &p); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	o, err := h.store.Upsert(r.Context(), chi.URLParam(r, "id"), p)
	var ve *ValidationError
	switch {
	case errors.As(err, &ve):
		httpx.Error(w, http.StatusUnprocessableEntity, "validation_error", ve.Msg)
	case err != nil:
		slog.Error("import option update", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal", "something went wrong")
	default:
		httpx.JSON(w, http.StatusOK, o)
	}
}
