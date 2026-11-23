package artist

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/httpx"
)

// Handler serves the artist profile endpoints (all require authentication).
type Handler struct{ store *Store }

// NewHandler creates the handler.
func NewHandler(store *Store) *Handler { return &Handler{store: store} }

// Routes registers the routes on r (already authenticated).
func (h *Handler) Routes(r chi.Router) {
	r.Post("/artists", h.create)
	r.Get("/me/artist", h.mine)
	r.Get("/artists/{id}", h.get)
	r.Patch("/artists/{id}", h.update)
	r.Post("/artists/{id}/verification-request", h.requestVerification)
	r.Get("/artists/{id}/quota", h.quota)
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	var req struct {
		Name string `json:"name"`
		Bio  string `json:"bio"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	a, err := h.store.Create(r.Context(), uid, req.Name, req.Bio)
	if err != nil {
		Fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, a)
}

func (h *Handler) mine(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	a, err := h.store.GetByOwner(r.Context(), uid)
	if errors.Is(err, ErrNotFound) {
		httpx.Error(w, http.StatusNotFound, "no_artist", "this account has no artist profile yet")
		return
	}
	if err != nil {
		Fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, a)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	a, err := h.store.Get(r.Context(), chi.URLParam(r, "id"), uid)
	if err != nil {
		Fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, a)
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	var req struct {
		Name          *string `json:"name"`
		Bio           *string `json:"bio"`
		AvatarImageID *string `json:"avatarImageId"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	a, err := h.store.Update(r.Context(), chi.URLParam(r, "id"), uid, Patch{Name: req.Name, Bio: req.Bio, AvatarImageID: req.AvatarImageID})
	if err != nil {
		Fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, a)
}

func (h *Handler) requestVerification(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	var req struct {
		Message string `json:"message"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	id := chi.URLParam(r, "id")
	if err := h.store.RequestVerification(r.Context(), id, uid, req.Message); err != nil {
		Fail(w, err)
		return
	}
	a, err := h.store.Get(r.Context(), id, uid)
	if err != nil {
		Fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusAccepted, a)
}

func (h *Handler) quota(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	q, err := h.store.Quota(r.Context(), chi.URLParam(r, "id"), uid)
	if err != nil {
		Fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, q)
}

// Fail writes the HTTP error for a store error.
func Fail(w http.ResponseWriter, err error) {
	var ve *ValidationError
	switch {
	case errors.As(err, &ve):
		httpx.Error(w, http.StatusUnprocessableEntity, "validation_error", ve.Msg)
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, http.StatusNotFound, "not_found", "artist not found")
	case errors.Is(err, ErrEmailNotVerified):
		httpx.Error(w, http.StatusForbidden, "email_not_verified", "verify your email address before becoming an artist")
	case errors.Is(err, ErrExists):
		httpx.Error(w, http.StatusConflict, "artist_exists", "this account already has an artist profile")
	case errors.Is(err, ErrAlreadyVerified):
		httpx.Error(w, http.StatusConflict, "already_verified", "this artist is already verified")
	case errors.Is(err, ErrNotPending):
		httpx.Error(w, http.StatusConflict, "not_pending", "there is no pending verification request")
	default:
		slog.Error("artist handler", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal", "something went wrong")
	}
}
