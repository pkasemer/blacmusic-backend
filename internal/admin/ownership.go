package admin

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/blacmusic/backend/internal/audit"
)

// Handing a catalog artist (one without an account) to a user, typically when the artist signs up in the app, and
// taking an artist away from a user.

// POST /v1/admin/artists/{id}/owner {userId, reason} -> the artist detail.
//
// The songs already there do not count against the new owner's storage allowance; the owner gets the welcome
// allowance (once per artist) for their own uploads. Their profile picture becomes the artist's; with none, the
// artist keeps showing its catalog picture.
func (h *Handler) assignOwner(w http.ResponseWriter, r *http.Request) {
	var in struct {
		UserID string `json:"userId"`
		Reason string `json:"reason"`
	}
	if err := decodeBody(w, r, &in, false); err != nil {
		reply(w, err)
		return
	}
	why, err := reason(in.Reason)
	if err != nil {
		reply(w, err)
		return
	}
	ctx := r.Context()
	id := param(r, "id")
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		reply(w, err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var name string
	var banned bool
	var owner *string
	if err := tx.QueryRow(ctx, `SELECT name, banned_at IS NOT NULL, owner_user_id::text FROM artists WHERE id = $1 FOR UPDATE`, id).
		Scan(&name, &banned, &owner); err != nil {
		reply(w, err)
		return
	}
	var email string
	var userBanned, verified, hasArtist bool
	err = tx.QueryRow(ctx, `SELECT email, banned_at IS NOT NULL, email_verified_at IS NOT NULL,
		EXISTS (SELECT 1 FROM artists WHERE owner_user_id = u.id) FROM users u WHERE u.id::text = $1 FOR UPDATE OF u`, in.UserID).
		Scan(&email, &userBanned, &verified, &hasArtist)
	userMissing := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !userMissing {
		reply(w, err)
		return
	}
	var refusal error
	switch {
	case userMissing:
		refusal = fail4(http.StatusNotFound, "user_not_found", "there is no account with that id")
	case owner != nil:
		refusal = fail4(http.StatusConflict, "already_owned", "this artist already belongs to an account: remove it from that account first")
	case hasArtist:
		refusal = fail4(http.StatusConflict, "user_has_artist", "this account already has an artist profile")
	case userBanned:
		refusal = fail4(http.StatusConflict, "user_banned", "this account is banned")
	case banned:
		refusal = fail4(http.StatusConflict, "artist_banned", "this artist is banned")
	case !verified:
		refusal = fail4(http.StatusConflict, "email_unverified", "this account has not verified its email address yet")
	}
	if refusal != nil {
		reply(w, refusal)
		return
	}

	_, err = tx.Exec(ctx, `UPDATE artists SET owner_user_id = $2::uuid, assigned_at = now(), assigned_by = $3::uuid, updated_at = now() WHERE id = $1`,
		id, in.UserID, actorID(r))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" { // the person made their own profile at the same moment
		reply(w, fail4(http.StatusConflict, "user_has_artist", "this account already has an artist profile"))
		return
	}
	if err != nil {
		reply(w, err)
		return
	}
	// what is there already is not charged to the new owner
	if _, err := tx.Exec(ctx, `UPDATE uploads SET quota_exempt = true WHERE artist_id = $1`, id); err != nil {
		reply(w, err)
		return
	}
	if err := h.artists.GrantWelcome(ctx, tx, id); err != nil {
		reply(w, err)
		return
	}
	if err := h.log(ctx, tx, r, audit.Entry{Action: "artist.assign", TargetType: "artist", TargetID: id, TargetLabel: name, Reason: why,
		Details: map[string]any{"userId": in.UserID, "email": email}}); err != nil {
		reply(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		reply(w, err)
		return
	}
	h.getArtist(w, r)
}

// DELETE /v1/admin/artists/{id}/owner {reason} -> the artist detail, back in the catalog.
//
// Releases stay as they are (public ones stay public). The account stays and loses the artist tools. The artist keeps
// the picture it shows now. A verification request is withdrawn; a verified badge stays (revoke it separately).
// Files the person never finished sending are dropped and their open transfers cancelled; a file already waiting
// for the audio processor still becomes a song. 409 busy while one of their files is being processed.
func (h *Handler) removeOwner(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Reason string `json:"reason"`
	}
	if err := decodeBody(w, r, &in, false); err != nil {
		reply(w, err)
		return
	}
	why, err := reason(in.Reason)
	if err != nil {
		reply(w, err)
		return
	}
	ctx := r.Context()
	id := param(r, "id")
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		reply(w, err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var name, owner, email string
	if err := tx.QueryRow(ctx, `SELECT a.name, COALESCE(a.owner_user_id::text, ''), COALESCE(u.email, '')
		FROM artists a LEFT JOIN users u ON u.id = a.owner_user_id WHERE a.id = $1 FOR UPDATE OF a`, id).Scan(&name, &owner, &email); err != nil {
		reply(w, err)
		return
	}
	if owner == "" {
		reply(w, fail4(http.StatusConflict, "not_owned", "this artist has no account"))
		return
	}
	var processing bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM uploads WHERE artist_id = $1 AND user_id::text = $2 AND status = 'processing')`, id, owner).
		Scan(&processing); err != nil {
		reply(w, err)
		return
	}
	if processing {
		reply(w, errBusy)
		return
	}

	rows, err := tx.Query(ctx, `DELETE FROM uploads WHERE artist_id = $1 AND user_id::text = $2 AND NOT by_team AND status IN ('pending', 'failed')
		RETURNING original_key, multipart_id`, id, owner)
	if err != nil {
		reply(w, err)
		return
	}
	type object struct{ key, multipart string }
	dropped, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (o object, err error) { return o, row.Scan(&o.key, &o.multipart) })
	if err != nil {
		reply(w, err)
		return
	}
	// the picture shown now (the owner's, else the catalog one) stays with the artist
	if _, err := tx.Exec(ctx, `UPDATE artists a SET
		avatar_image_id = COALESCE((SELECT u.avatar_image_id FROM users u WHERE u.id = a.owner_user_id), a.avatar_image_id),
		owner_user_id = NULL, assigned_at = NULL, assigned_by = NULL, updated_at = now(),
		verification = CASE WHEN verification = 'pending' THEN 'unverified' ELSE verification END,
		verification_message = '', verification_requested_at = NULL
		WHERE a.id = $1`, id); err != nil {
		reply(w, err)
		return
	}
	if err := h.log(ctx, tx, r, audit.Entry{Action: "artist.unassign", TargetType: "artist", TargetID: id, TargetLabel: name, Reason: why,
		Details: map[string]any{"userId": owner, "email": email}}); err != nil {
		reply(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		reply(w, err)
		return
	}

	// the rest is tidying: it never undoes the removal
	after := context.WithoutCancel(ctx)
	if h.Blobs != nil {
		for _, o := range dropped {
			if o.multipart != "" {
				_ = h.Blobs.AbortMultipart(after, o.key, o.multipart)
			}
			_ = h.Blobs.Remove(after, o.key)
		}
	}
	if h.Transfers != nil {
		if err := h.Transfers.CancelAllOpen(after, owner); err != nil {
			slog.Warn("artist removed from user: transfers not cancelled", "artist", id, "err", err)
		}
	}
	h.getArtist(w, r)
}
