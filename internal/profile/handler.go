// Package profile lets a signed-in user edit their own profile: name and the
// profile picture, which is also their artist image.
package profile

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/httpx"
	"github.com/blacmusic/backend/internal/image"
	"github.com/blacmusic/backend/internal/review"
)

// Handler serves PATCH /v1/me.
type Handler struct {
	pool *pgxpool.Pool
	auth *auth.Service
	// Review: an artist's new picture is shown to listeners only once the team approves it.
	Review bool
}

// NewHandler creates the handler.
func NewHandler(pool *pgxpool.Pool, svc *auth.Service) *Handler {
	return &Handler{pool: pool, auth: svc}
}

// Update changes the caller's display name and/or profile picture and returns
// the updated user. avatarImageId must be one of the caller's own finished
// images with purpose "avatar" (see POST /v1/images); "" removes the picture.
// No verified email is needed: it is a profile, not an artist feature.
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	var req struct {
		DisplayName   *string `json:"displayName"`
		AvatarImageID *string `json:"avatarImageId"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.DisplayName == nil && req.AvatarImageID == nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "send displayName and/or avatarImageId")
		return
	}

	var name *string
	if req.DisplayName != nil {
		n := strings.TrimSpace(*req.DisplayName)
		if c := utf8.RuneCountInString(n); c < 1 || c > 50 {
			httpx.Error(w, http.StatusUnprocessableEntity, "validation_error", "displayName must be 1-50 characters")
			return
		}
		name = &n
	}
	setAvatar := req.AvatarImageID != nil
	var avatar *string
	if setAvatar && *req.AvatarImageID != "" {
		if err := image.CheckOwned(r.Context(), h.pool, *req.AvatarImageID, uid, "avatar"); err != nil {
			httpx.Error(w, http.StatusUnprocessableEntity, "validation_error", "avatarImageId must be one of your finished avatar images")
			return
		}
		avatar = req.AvatarImageID
	}

	err := func() error {
		tx, err := h.pool.Begin(r.Context())
		if err != nil {
			return err
		}
		defer tx.Rollback(r.Context()) //nolint:errcheck
		if _, err := tx.Exec(r.Context(), `UPDATE users SET display_name = COALESCE($2, display_name) WHERE id = $1`, uid, name); err != nil {
			return err
		}
		if setAvatar {
			if err := review.SetPicture(r.Context(), tx, uid, avatar, h.Review); err != nil {
				return err
			}
		}
		return tx.Commit(r.Context())
	}()
	if err != nil {
		slog.Error("update profile", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal", "something went wrong")
		return
	}
	u, err := h.auth.Me(r.Context(), uid)
	if errors.Is(err, auth.ErrInvalidToken) {
		httpx.Error(w, http.StatusUnauthorized, "unauthorized", "account no longer exists")
		return
	}
	if err != nil {
		slog.Error("reload profile", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal", "something went wrong")
		return
	}
	httpx.JSON(w, http.StatusOK, u)
}
