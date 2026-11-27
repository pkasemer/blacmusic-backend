package admin

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/blacmusic/backend/internal/account"
	"github.com/blacmusic/backend/internal/audit"
	"github.com/blacmusic/backend/internal/httpx"
	"github.com/blacmusic/backend/internal/image"
)

func (h *Handler) userRoutes(r chi.Router) {
	r.Get("/users", h.listUsers)
	r.Get("/users/{id}", h.getUser)
	r.Get("/users/{id}/preference-graph", h.preferenceGraph)
	r.Post("/users/{id}/ban", h.banUser)
	r.Post("/users/{id}/unban", h.unbanUser)
	r.Get("/users/{id}/deletion-preview", h.userDeletionPreview)
	r.Delete("/users/{id}", h.deleteUser)
	r.Put("/users/{id}/signals", h.setUserSignals)
	r.Get("/users/{id}/recommendations", h.explainRecommendations)
	r.Get("/recommendations/outcomes", h.recommendationOutcomes)
	r.Get("/recommendations/readiness", h.recommendationReadiness)
	r.Get("/recommendations/blocks", h.listBlocks)
	r.Post("/recommendations/blocks", h.addBlock)
	r.Delete("/recommendations/blocks/{id}", h.removeBlock)
}

type userRow struct {
	ID            string    `json:"id"`
	Email         string    `json:"email"`
	DisplayName   string    `json:"displayName"`
	Role          string    `json:"role"`
	EmailVerified bool      `json:"emailVerified"`
	Banned        bool      `json:"banned"`
	HasArtist     bool      `json:"hasArtist"`
	CreatedAt     time.Time `json:"createdAt"`
	// SignalsExcluded: what this account does is left out of what other listeners are offered
	SignalsExcluded bool `json:"signalsExcluded"`
}

// GET /v1/admin/users?q=&role=&status=active|banned|unverified&sort=createdAt|email|displayName&order=asc|desc&limit=&offset=
func (h *Handler) listUsers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	status := q.Get("status")
	switch status {
	case "", "active", "banned", "unverified":
	default:
		reply(w, fail4(http.StatusBadRequest, "bad_request", "status must be active, banned or unverified"))
		return
	}
	cols := map[string]string{"": "u.created_at", "createdAt": "u.created_at", "email": "lower(u.email)", "displayName": "lower(u.display_name)"}
	col, ok := cols[q.Get("sort")]
	if !ok {
		reply(w, fail4(http.StatusBadRequest, "bad_request", "sort must be createdAt, email or displayName"))
		return
	}
	dir := "DESC"
	if col != "u.created_at" {
		dir = "ASC"
	}
	switch q.Get("order") {
	case "asc":
		dir = "ASC"
	case "desc":
		dir = "DESC"
	case "":
	default:
		reply(w, fail4(http.StatusBadRequest, "bad_request", "order must be asc or desc"))
		return
	}
	limit, offset := page(r)
	search := strings.TrimSpace(q.Get("q"))
	where := ` WHERE ($1 = '' OR u.email ILIKE $2 OR u.display_name ILIKE $2 OR u.id::text = $1)
		AND ($3 = '' OR u.role = $3)
		AND ($4 = '' OR ($4 = 'active' AND u.banned_at IS NULL) OR ($4 = 'banned' AND u.banned_at IS NOT NULL)
		     OR ($4 = 'unverified' AND u.email_verified_at IS NULL))`
	args := []any{search, like(search), q.Get("role"), status}
	var total int
	if err := h.pool.QueryRow(r.Context(), `SELECT count(*) FROM users u`+where, args...).Scan(&total); err != nil {
		reply(w, err)
		return
	}
	rows, err := h.pool.Query(r.Context(), `
		SELECT u.id::text, u.email, u.display_name, u.role, u.email_verified_at IS NOT NULL, u.banned_at IS NOT NULL,
		       EXISTS (SELECT 1 FROM artists a WHERE a.owner_user_id = u.id), u.created_at,
		       u.role = 'admin' OR u.signals_excluded_at IS NOT NULL
		FROM users u`+where+` ORDER BY `+col+` `+dir+`, u.id LIMIT $5 OFFSET $6`, append(args, limit, offset)...)
	if err != nil {
		reply(w, err)
		return
	}
	defer rows.Close()
	out := []userRow{}
	for rows.Next() {
		var u userRow
		if err := rows.Scan(&u.ID, &u.Email, &u.DisplayName, &u.Role, &u.EmailVerified, &u.Banned, &u.HasArtist, &u.CreatedAt, &u.SignalsExcluded); err != nil {
			reply(w, err)
			return
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		reply(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"users": out, "total": total})
}

type banInfo struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
	By     *struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	} `json:"by"`
}

// GET /v1/admin/users/{id}
func (h *Handler) getUser(w http.ResponseWriter, r *http.Request) {
	id := param(r, "id")
	var u struct {
		userRow
		AvatarURL   string         `json:"avatarUrl,omitempty"`
		Ban         *banInfo       `json:"ban"`
		SignIn      map[string]any `json:"signIn"`
		Artist      map[string]any `json:"artist"`
		Counts      map[string]int `json:"counts"`
		LastSeenAt  *time.Time     `json:"lastSeenAt"`
		PushEnabled bool           `json:"pushEnabled"`
	}
	var avatarID *string
	var bannedAt *time.Time
	var banReason, bannedBy, bannedByEmail, hash string
	var artistID, artistName string
	err := h.pool.QueryRow(r.Context(), `
		SELECT u.id::text, u.email, u.display_name, u.role, u.email_verified_at IS NOT NULL, u.banned_at, u.created_at, u.avatar_image_id,
		       u.ban_reason, COALESCE(u.banned_by::text, ''), COALESCE((SELECT email FROM users b WHERE b.id = u.banned_by), ''),
		       u.password_hash, COALESCE(a.id, ''), COALESCE(a.name, ''),
		       EXISTS (SELECT 1 FROM push_tokens p WHERE p.user_id = u.id),
		       (SELECT max(created_at) FROM refresh_tokens t WHERE t.user_id = u.id),
		       u.role = 'admin' OR u.signals_excluded_at IS NOT NULL
		FROM users u LEFT JOIN artists a ON a.owner_user_id = u.id WHERE u.id::text = $1`, id).
		Scan(&u.ID, &u.Email, &u.DisplayName, &u.Role, &u.EmailVerified, &bannedAt, &u.CreatedAt, &avatarID,
			&banReason, &bannedBy, &bannedByEmail, &hash, &artistID, &artistName, &u.PushEnabled, &u.LastSeenAt, &u.SignalsExcluded)
	if err != nil {
		reply(w, err)
		return
	}
	u.Banned = bannedAt != nil
	u.HasArtist = artistID != ""
	u.AvatarURL = image.PersonURL(h.PublicURL, avatarID, "")
	if bannedAt != nil {
		u.Ban = &banInfo{At: *bannedAt, Reason: banReason}
		if bannedBy != "" {
			u.Ban.By = &struct {
				ID    string `json:"id"`
				Email string `json:"email"`
			}{bannedBy, bannedByEmail}
		}
	}
	u.SignIn = map[string]any{"password": hash != "!", "apple": false, "google": false}
	prov, err := h.pool.Query(r.Context(), `SELECT provider FROM auth_identities WHERE user_id::text = $1`, id)
	if err != nil {
		reply(w, err)
		return
	}
	for prov.Next() {
		var p string
		if err := prov.Scan(&p); err != nil {
			prov.Close()
			reply(w, err)
			return
		}
		u.SignIn[p] = true
	}
	prov.Close()
	if artistID != "" {
		u.Artist = map[string]any{"id": artistID, "name": artistName}
	}
	u.Counts = map[string]int{}
	var playlists, followed, saved int
	if err := h.pool.QueryRow(r.Context(), `
		SELECT (SELECT count(*) FROM playlists WHERE owner_id::text = $1),
		       (SELECT count(*) FROM library_artists WHERE user_id::text = $1),
		       (SELECT count(*) FROM library_tracks WHERE user_id::text = $1) + (SELECT count(*) FROM library_albums WHERE user_id::text = $1)
		       + (SELECT count(*) FROM library_playlists WHERE user_id::text = $1)`, id).Scan(&playlists, &followed, &saved); err != nil {
		reply(w, err)
		return
	}
	u.Counts["playlists"], u.Counts["followedArtists"], u.Counts["savedItems"] = playlists, followed, saved
	httpx.JSON(w, http.StatusOK, u)
}

// lockUser reads and locks a user for a state change.
func lockUser(ctx context.Context, tx pgx.Tx, id string) (uuid, email, role string, banned bool, err error) {
	err = tx.QueryRow(ctx, `SELECT id::text, email, role, banned_at IS NOT NULL FROM users WHERE id::text = $1 FOR UPDATE`, id).
		Scan(&uuid, &email, &role, &banned)
	if err == pgx.ErrNoRows {
		err = errNotFound
	}
	return
}

// POST /v1/admin/users/{id}/ban {reason}
func (h *Handler) banUser(w http.ResponseWriter, r *http.Request) {
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
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		reply(w, err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	id, email, role, banned, err := lockUser(ctx, tx, param(r, "id"))
	if err != nil {
		reply(w, err)
		return
	}
	switch {
	case role == "admin":
		reply(w, errIsAdmin)
		return
	case banned:
		reply(w, errAlreadyBanned)
		return
	}
	admin := actorID(r)
	if _, err := tx.Exec(ctx, `UPDATE users SET banned_at = now(), ban_reason = $2, banned_by = $3::uuid WHERE id = $1::uuid`, id, why, admin); err != nil {
		reply(w, err)
		return
	}
	if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now() WHERE user_id = $1::uuid AND revoked_at IS NULL`, id); err != nil {
		reply(w, err)
		return
	}
	details := map[string]any{}
	var artistID string
	var artistBanned bool
	err = tx.QueryRow(ctx, `SELECT id, banned_at IS NOT NULL FROM artists WHERE owner_user_id = $1::uuid FOR UPDATE`, id).Scan(&artistID, &artistBanned)
	if err != nil && err != pgx.ErrNoRows {
		reply(w, err)
		return
	}
	if err == nil {
		details["artistId"] = artistID
		if !artistBanned {
			if _, err := tx.Exec(ctx, `UPDATE artists SET banned_at = now(), ban_reason = $2, banned_by = $3::uuid WHERE id = $1`, artistID, why, admin); err != nil {
				reply(w, err)
				return
			}
		}
		n, err := takeDownArtistReleases(ctx, tx, artistID, admin, "account suspended: "+why)
		if err != nil {
			reply(w, err)
			return
		}
		details["releasesTakenDown"] = n
	}
	tag, err := tx.Exec(ctx, `UPDATE playlists SET is_public = false, hidden_by_ban = true WHERE owner_id = $1::uuid AND is_public`, id)
	if err != nil {
		reply(w, err)
		return
	}
	details["playlistsHidden"] = tag.RowsAffected()
	if err := h.log(ctx, tx, r, audit.Entry{Action: "user.ban", TargetType: "user", TargetID: id, TargetLabel: email, Reason: why, Details: details}); err != nil {
		reply(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		reply(w, err)
		return
	}
	if h.Bans != nil {
		h.Bans.Invalidate(id)
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /v1/admin/users/{id}/unban {note?}
func (h *Handler) unbanUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Note string `json:"note"`
	}
	if err := decodeBody(w, r, &in, true); err != nil {
		reply(w, err)
		return
	}
	note, err := optionalNote(in.Note)
	if err != nil {
		reply(w, err)
		return
	}
	ctx := r.Context()
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		reply(w, err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	id, email, _, banned, err := lockUser(ctx, tx, param(r, "id"))
	if err != nil {
		reply(w, err)
		return
	}
	if !banned {
		reply(w, errNotBanned)
		return
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET banned_at = NULL, ban_reason = '', banned_by = NULL WHERE id = $1::uuid`, id); err != nil {
		reply(w, err)
		return
	}
	details := map[string]any{}
	var artistID string
	err = tx.QueryRow(ctx, `SELECT id FROM artists WHERE owner_user_id = $1::uuid FOR UPDATE`, id).Scan(&artistID)
	if err != nil && err != pgx.ErrNoRows {
		reply(w, err)
		return
	}
	if err == nil {
		if _, err := tx.Exec(ctx, `UPDATE artists SET banned_at = NULL, ban_reason = '', banned_by = NULL WHERE id = $1`, artistID); err != nil {
			reply(w, err)
			return
		}
		n, err := restoreArtistReleases(ctx, tx, artistID)
		if err != nil {
			reply(w, err)
			return
		}
		details["artistId"], details["releasesRestored"] = artistID, n
	}
	tag, err := tx.Exec(ctx, `UPDATE playlists SET is_public = true, hidden_by_ban = false WHERE owner_id = $1::uuid AND hidden_by_ban`, id)
	if err != nil {
		reply(w, err)
		return
	}
	details["playlistsRestored"] = tag.RowsAffected()
	if err := h.log(ctx, tx, r, audit.Entry{Action: "user.unban", TargetType: "user", TargetID: id, TargetLabel: email, Reason: note, Details: details}); err != nil {
		reply(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		reply(w, err)
		return
	}
	if h.Bans != nil {
		h.Bans.Invalidate(id)
	}
	w.WriteHeader(http.StatusNoContent)
}

// userPreview counts what deleting a user removes.
func (h *Handler) userPreview(ctx context.Context, id string) (map[string]any, string, string, error) {
	var email, role string
	if err := h.pool.QueryRow(ctx, `SELECT email, role FROM users WHERE id::text = $1`, id).Scan(&email, &role); err != nil {
		if err == pgx.ErrNoRows {
			return nil, "", "", errNotFound
		}
		return nil, "", "", err
	}
	var artistID, artistName string
	var releases, tracks, playlists, uploads, library, sessions int
	var storage int64
	var returns bool
	err := h.pool.QueryRow(ctx, `
		SELECT COALESCE((SELECT id FROM artists WHERE owner_user_id::text = $1), ''), COALESCE((SELECT name FROM artists WHERE owner_user_id::text = $1), ''),
		       COALESCE((SELECT assigned_at IS NOT NULL FROM artists WHERE owner_user_id::text = $1), false),
		       (SELECT count(*) FROM (`+account.DoomedReleases+`) r),
		       (SELECT count(*) FROM (`+account.DoomedTracks+`) t),
		       (SELECT count(*) FROM playlists WHERE owner_id::text = $1),
		       (SELECT count(*) FROM uploads u WHERE `+account.DoomedUploads+`),
		       (SELECT COALESCE(sum(u.size_bytes), 0) FROM uploads u WHERE u.status <> 'failed' AND `+account.DoomedUploads+`),
		       (SELECT count(*) FROM library_tracks WHERE user_id::text = $1) + (SELECT count(*) FROM library_albums WHERE user_id::text = $1)
		       + (SELECT count(*) FROM library_artists WHERE user_id::text = $1) + (SELECT count(*) FROM library_playlists WHERE user_id::text = $1),
		       (SELECT count(*) FROM refresh_tokens WHERE user_id::text = $1 AND revoked_at IS NULL AND expires_at > now())`, id).
		Scan(&artistID, &artistName, &returns, &releases, &tracks, &playlists, &uploads, &storage, &library, &sessions)
	if err != nil {
		return nil, "", "", err
	}
	var artist any
	if artistID != "" {
		// returnsToCatalog: the artist stays (received from the catalog); the counts are only what is really deleted
		artist = map[string]any{"id": artistID, "name": artistName, "returnsToCatalog": returns}
	}
	return map[string]any{"artist": artist, "releases": releases, "tracks": tracks, "playlists": playlists, "uploads": uploads,
		"storageBytes": storage, "libraryItems": library, "sessions": sessions}, email, role, nil
}

// GET /v1/admin/users/{id}/deletion-preview
func (h *Handler) userDeletionPreview(w http.ResponseWriter, r *http.Request) {
	p, _, _, err := h.userPreview(r.Context(), param(r, "id"))
	if err != nil {
		reply(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, p)
}

// DELETE /v1/admin/users/{id} {reason, confirmEmail}
func (h *Handler) deleteUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Reason       string `json:"reason"`
		ConfirmEmail string `json:"confirmEmail"`
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
	preview, email, role, err := h.userPreview(ctx, id)
	if err != nil {
		reply(w, err)
		return
	}
	if role == "admin" {
		reply(w, errIsAdmin)
		return
	}
	if !strings.EqualFold(strings.TrimSpace(in.ConfirmEmail), email) {
		reply(w, fail4(http.StatusUnprocessableEntity, "confirm_mismatch", "confirmEmail must be the account's email address"))
		return
	}
	err = h.Accounts.AdminDelete(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		return h.log(ctx, tx, r, audit.Entry{Action: "user.delete", TargetType: "user", TargetID: id, TargetLabel: email, Reason: why, Details: preview})
	})
	if err != nil {
		if errors.Is(err, account.ErrBusy) {
			err = errBusy
		}
		reply(w, err)
		return
	}
	if h.Bans != nil {
		h.Bans.Invalidate(id)
	}
	w.WriteHeader(http.StatusNoContent)
}
