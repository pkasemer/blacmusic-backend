package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/blacmusic/backend/internal/httpx"
)

// Handler exposes the auth service over HTTP.
type Handler struct{ svc *Service }

// NewHandler creates the HTTP handler.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

type registerRequest struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"displayName"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type refreshRequest struct {
	RefreshToken string `json:"refreshToken"`
}

type authResponse struct {
	User *User `json:"user"`
	*TokenPair
}

func (h *Handler) Register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	u, pair, err := h.svc.Register(r.Context(), req.Email, req.Password, req.DisplayName)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, authResponse{User: u, TokenPair: pair})
}

func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	u, pair, err := h.svc.Login(r.Context(), req.Email, req.Password)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, authResponse{User: u, TokenPair: pair})
}

func (h *Handler) Refresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := httpx.Decode(w, r, &req); err != nil || req.RefreshToken == "" {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "refreshToken is required")
		return
	}
	pair, err := h.svc.Refresh(r.Context(), req.RefreshToken)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, pair)
}

func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := httpx.Decode(w, r, &req); err != nil || req.RefreshToken == "" {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "refreshToken is required")
		return
	}
	if err := h.svc.Logout(r.Context(), req.RefreshToken); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	id, _ := UserID(r.Context())
	u, err := h.svc.Me(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, u)
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	var ve *ValidationError
	var soon *TooSoonError
	switch {
	case errors.As(err, &soon):
		w.Header().Set("Retry-After", strconv.Itoa(int(soon.RetryAfter.Seconds())+1))
		httpx.Error(w, http.StatusTooManyRequests, "too_soon", "a code was just sent; wait a moment before asking for another")
	case errors.Is(err, ErrInvalidCode):
		httpx.Error(w, http.StatusUnprocessableEntity, "invalid_code", err.Error())
	case errors.Is(err, ErrMailFailed):
		httpx.Error(w, http.StatusServiceUnavailable, "email_unavailable", err.Error())
	case errors.As(err, &ve):
		httpx.Error(w, http.StatusUnprocessableEntity, "validation_error", ve.Msg)
	case errors.Is(err, ErrEmailTaken):
		httpx.Error(w, http.StatusConflict, "email_taken", err.Error())
	case errors.Is(err, ErrInvalidCredentials):
		httpx.Error(w, http.StatusUnauthorized, "invalid_credentials", err.Error())
	case errors.Is(err, ErrInvalidToken):
		httpx.Error(w, http.StatusUnauthorized, "invalid_token", err.Error())
	default:
		slog.Error("auth handler", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal", "something went wrong")
	}
}

type ctxKey struct{}

// UserID returns the authenticated user id stored by Require.
func UserID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(ctxKey{}).(string)
	return id, ok
}

// Require rejects requests without a valid Bearer access token.
func Require(tokens *TokenService) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Get("Authorization")
			raw, ok := strings.CutPrefix(h, "Bearer ")
			if !ok || raw == "" {
				httpx.Error(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
				return
			}
			id, err := tokens.ParseAccess(raw)
			if err != nil {
				httpx.Error(w, http.StatusUnauthorized, "unauthorized", "invalid or expired access token")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, id)))
		})
	}
}
