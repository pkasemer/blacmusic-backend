package admin

import (
	"context"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/blacmusic/backend/internal/artist"
	"github.com/blacmusic/backend/internal/audit"
	"github.com/blacmusic/backend/internal/httpx"
	"github.com/blacmusic/backend/internal/image"
)

func (h *Handler) artistRoutes(r chi.Router) {
	r.Get("/artists", h.listArtists)
	r.Get("/artists/{id}", h.getArtist)
	r.Patch("/artists/{id}", h.patchArtist)
	r.Post("/artists/{id}/ban", h.banArtist)
	r.Post("/artists/{id}/unban", h.unbanArtist)
	r.Get("/artists/{id}/deletion-preview", h.artistDeletionPreview)
	r.Delete("/artists/{id}", h.deleteArtist)
	r.Post("/artists/{id}/owner", h.assignOwner)
	r.Delete("/artists/{id}/owner", h.removeOwner)
}

type artistRow struct {
	ID                  string     `json:"id"`
	Name                string     `json:"name"`
	ImageURL            string     `json:"imageUrl,omitempty"`
	Verification        string     `json:"verification"`
	Flagged             bool       `json:"flagged"`
	Banned              bool       `json:"banned"`
	OwnerUserID         string     `json:"ownerUserId,omitempty"`
	OwnerEmail          string     `json:"ownerEmail,omitempty"`
	ReleaseCount        int        `json:"releaseCount"`
	FollowerCount       int        `json:"followerCount"`
	VerificationMessage string     `json:"verificationMessage,omitempty"`
	RequestedAt         *time.Time `json:"requestedAt,omitempty"`
	CreatedAt           time.Time  `json:"createdAt"`
}

// GET /v1/admin/artists?q=&verification=unverified|pending|verified|rejected&status=active|banned&flagged=true|false&limit=&offset=
//
// The older review-queue form keeps working: status=pending lists pending requests (oldest first) and
// status=flagged lists flagged artists.
func (h *Handler) listArtists(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	status, verification, flagged := q.Get("status"), q.Get("verification"), q.Get("flagged")
	order := `a.created_at DESC`
	switch status {
	case "pending":
		verification, status, order = "pending", "", `a.verification_requested_at, a.id`
	case "flagged":
		flagged, status, order = "true", "", `a.created_at, a.id`
	case "", "active", "banned":
	default:
		reply(w, fail4(http.StatusBadRequest, "bad_request", "status must be active, banned, pending or flagged"))
		return
	}
	switch verification {
	case "", "unverified", "pending", "verified", "rejected":
	default:
		reply(w, fail4(http.StatusBadRequest, "bad_request", "verification must be unverified, pending, verified or rejected"))
		return
	}
	if flagged != "" && flagged != "true" && flagged != "false" {
		reply(w, fail4(http.StatusBadRequest, "bad_request", "flagged must be true or false"))
		return
	}
	limit, offset := page(r)
	search := strings.TrimSpace(q.Get("q"))
	where := ` WHERE ($1 = '' OR a.name ILIKE $2 OR u.email ILIKE $2 OR a.id = $1)
		AND ($3 = '' OR a.verification = $3) AND ($4 = '' OR ($4 = 'true') = a.flagged)
		AND ($5 = '' OR ($5 = 'banned') = (a.banned_at IS NOT NULL))`
	args := []any{search, like(search), verification, flagged, status}
	var total int
	if err := h.pool.QueryRow(r.Context(), `SELECT count(*) FROM artists a LEFT JOIN users u ON u.id = a.owner_user_id`+where, args...).Scan(&total); err != nil {
		reply(w, err)
		return
	}
	rows, err := h.pool.Query(r.Context(), `
		SELECT a.id, a.name, COALESCE(u.avatar_image_id, a.avatar_image_id), COALESCE(a.image_url, ''), a.verification, a.flagged, a.banned_at IS NOT NULL,
		       COALESCE(a.owner_user_id::text, ''), COALESCE(u.email, ''),
		       (SELECT count(*) FROM albums al WHERE al.artist_id = a.id),
		       (SELECT count(*) FROM library_artists l WHERE l.artist_id = a.id),
		       a.verification_message, a.verification_requested_at, a.created_at
		FROM artists a LEFT JOIN users u ON u.id = a.owner_user_id`+where+` ORDER BY `+order+` LIMIT $6 OFFSET $7`, append(args, limit, offset)...)
	if err != nil {
		reply(w, err)
		return
	}
	defer rows.Close()
	out := []artistRow{}
	for rows.Next() {
		var a artistRow
		var avatarID *string
		var legacy string
		if err := rows.Scan(&a.ID, &a.Name, &avatarID, &legacy, &a.Verification, &a.Flagged, &a.Banned, &a.OwnerUserID, &a.OwnerEmail,
			&a.ReleaseCount, &a.FollowerCount, &a.VerificationMessage, &a.RequestedAt, &a.CreatedAt); err != nil {
			reply(w, err)
			return
		}
		a.ImageURL = image.PersonURL(h.PublicURL, avatarID, legacy)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		reply(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"artists": out, "total": total})
}

// GET /v1/admin/artists/{id}
func (h *Handler) getArtist(w http.ResponseWriter, r *http.Request) {
	id := param(r, "id")
	ctx := r.Context()
	var a struct {
		ID           string         `json:"id"`
		Name         string         `json:"name"`
		Bio          string         `json:"bio"`
		ImageURL     string         `json:"imageUrl,omitempty"`
		Verification map[string]any `json:"verification"`
		Flagged      bool           `json:"flagged"`
		Ban          *banInfo       `json:"ban"`
		Owner        map[string]any `json:"owner"`
		Storage      map[string]any `json:"storage"`
		Counts       map[string]int `json:"counts"`
		RecentGrants []grantRow     `json:"recentGrants"`
		CreatedAt    time.Time      `json:"createdAt"`
	}
	var avatarID *string
	var legacy, state, message, note, owner, ownerEmail, ownerName, banReason, bannedBy, bannedByEmail string
	var requested, bannedAt *time.Time
	err := h.pool.QueryRow(ctx, `
		SELECT a.id, a.name, a.bio, COALESCE(u.avatar_image_id, a.avatar_image_id), COALESCE(a.image_url, ''), a.verification, a.verification_message, a.verification_note,
		       a.verification_requested_at, a.flagged, a.banned_at, a.ban_reason, COALESCE(a.banned_by::text, ''),
		       COALESCE((SELECT email FROM users b WHERE b.id = a.banned_by), ''),
		       COALESCE(a.owner_user_id::text, ''), COALESCE(u.email, ''), COALESCE(u.display_name, ''), a.created_at
		FROM artists a LEFT JOIN users u ON u.id = a.owner_user_id WHERE a.id = $1`, id).
		Scan(&a.ID, &a.Name, &a.Bio, &avatarID, &legacy, &state, &message, &note, &requested, &a.Flagged, &bannedAt, &banReason, &bannedBy,
			&bannedByEmail, &owner, &ownerEmail, &ownerName, &a.CreatedAt)
	if err != nil {
		reply(w, err)
		return
	}
	a.ImageURL = image.PersonURL(h.PublicURL, avatarID, legacy)
	a.Verification = map[string]any{"state": state, "message": message, "note": note, "requestedAt": requested}
	if bannedAt != nil {
		a.Ban = &banInfo{At: *bannedAt, Reason: banReason}
		if bannedBy != "" {
			a.Ban.By = &struct {
				ID    string `json:"id"`
				Email string `json:"email"`
			}{bannedBy, bannedByEmail}
		}
	}
	if owner != "" {
		a.Owner = map[string]any{"id": owner, "email": ownerEmail, "displayName": ownerName}
	}
	storage, err := h.artistStorage(ctx, id)
	if err != nil {
		reply(w, err)
		return
	}
	a.Storage = storage
	a.Counts = map[string]int{}
	var releases, public, tracks, followers, plays int
	if err := h.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM albums WHERE artist_id = $1), (SELECT count(*) FROM albums WHERE artist_id = $1 AND visibility = 'public'),
		       (SELECT count(*) FROM tracks WHERE artist_id = $1), (SELECT count(*) FROM library_artists WHERE artist_id = $1),
		       (SELECT count(*) FROM plays WHERE artist_id = $1)`, id).Scan(&releases, &public, &tracks, &followers, &plays); err != nil {
		reply(w, err)
		return
	}
	a.Counts["releases"], a.Counts["publicReleases"], a.Counts["tracks"], a.Counts["followers"], a.Counts["plays"] = releases, public, tracks, followers, plays
	a.RecentGrants = []grantRow{}
	rows, err := h.pool.Query(ctx, `SELECT id, kind, bytes, note, COALESCE(reference, ''), expires_at, created_at FROM storage_grants
		WHERE artist_id = $1 ORDER BY created_at DESC, id DESC LIMIT 10`, id)
	if err != nil {
		reply(w, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var g grantRow
		if err := rows.Scan(&g.ID, &g.Kind, &g.Bytes, &g.Note, &g.Reference, &g.ExpiresAt, &g.CreatedAt); err != nil {
			reply(w, err)
			return
		}
		a.RecentGrants = append(a.RecentGrants, g)
	}
	if err := rows.Err(); err != nil {
		reply(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, a)
}

type filesAndBytes struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
}

// storageBreakdown says everything an artist keeps on the server. The first three are the audio originals: together
// they are usedBytes (charged to the allowance) plus teamUploadBytes (not charged). Playback copies and pictures take
// disk space but are never charged.
type storageBreakdown struct {
	PublishedAudio    filesAndBytes `json:"publishedAudio"`    // originals whose release is public now
	UnpublishedAudio  filesAndBytes `json:"unpublishedAudio"`  // originals whose release is a draft, scheduled or taken down
	UnfinishedUploads filesAndBytes `json:"unfinishedUploads"` // not processed yet, at the reserved size
	PlaybackAudio     filesAndBytes `json:"playbackAudio"`     // the copies the app plays
	Pictures          filesAndBytes `json:"pictures"`          // pictures in use: the artist's and every release cover
}

// artistStorage reads the allowance and the breakdown from one snapshot, so the parts match the total even while
// uploads are being processed.
func (h *Handler) artistStorage(ctx context.Context, id string) (map[string]any, error) {
	tx, err := h.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	q, err := artist.QuotaFor(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	var b storageBreakdown
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE u.status = 'ready' AND al.visibility = 'public'),
		       COALESCE(sum(u.size_bytes) FILTER (WHERE u.status = 'ready' AND al.visibility = 'public'), 0),
		       count(*) FILTER (WHERE u.status = 'ready' AND al.visibility IS DISTINCT FROM 'public'),
		       COALESCE(sum(u.size_bytes) FILTER (WHERE u.status = 'ready' AND al.visibility IS DISTINCT FROM 'public'), 0),
		       count(*) FILTER (WHERE u.status <> 'ready'),
		       COALESCE(sum(u.size_bytes) FILTER (WHERE u.status <> 'ready'), 0)
		FROM uploads u LEFT JOIN albums al ON al.id = u.release_id
		WHERE u.artist_id = $1 AND u.status IN ('pending', 'queued', 'processing', 'ready')`, id).
		Scan(&b.PublishedAudio.Files, &b.PublishedAudio.Bytes, &b.UnpublishedAudio.Files, &b.UnpublishedAudio.Bytes,
			&b.UnfinishedUploads.Files, &b.UnfinishedUploads.Bytes); err != nil {
		return nil, err
	}
	if err := tx.QueryRow(ctx, `SELECT count(*), COALESCE(sum(tr.size_bytes), 0) FROM track_renditions tr JOIN tracks t ON t.id = tr.track_id
		WHERE t.artist_id = $1`, id).Scan(&b.PlaybackAudio.Files, &b.PlaybackAudio.Bytes); err != nil {
		return nil, err
	}
	if err := tx.QueryRow(ctx, `
		SELECT count(*), COALESCE(sum(stored_bytes), 0) FROM images WHERE id IN (
			SELECT COALESCE(u.avatar_image_id, a.avatar_image_id) FROM artists a LEFT JOIN users u ON u.id = a.owner_user_id WHERE a.id = $1
			UNION SELECT cover_image_id FROM albums WHERE artist_id = $1)`, id).Scan(&b.Pictures.Files, &b.Pictures.Bytes); err != nil {
		return nil, err
	}
	var team int64 // originals that are not charged to the allowance
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(size_bytes), 0) FROM uploads
		WHERE artist_id = $1 AND quota_exempt AND status IN ('pending', 'queued', 'processing', 'ready')`, id).Scan(&team); err != nil {
		return nil, err
	}
	return map[string]any{"usedBytes": q.UsedStorageBytes, "quotaBytes": q.TotalStorageBytes, "teamUploadBytes": team, "breakdown": b}, nil
}

type grantRow struct {
	ID        int64      `json:"id"`
	Kind      string     `json:"kind"`
	Bytes     int64      `json:"bytes"`
	Note      string     `json:"note"`
	Reference string     `json:"reference,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
}

// PATCH /v1/admin/artists/{id} {name?, bio?}
func (h *Handler) patchArtist(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name    *string `json:"name"`
		Bio     *string `json:"bio"`
		ImageID *string `json:"imageId"`
	}
	if err := decodeBody(w, r, &in, false); err != nil {
		reply(w, err)
		return
	}
	if in.Name == nil && in.Bio == nil && in.ImageID == nil {
		reply(w, invalid("send name, bio and/or imageId"))
		return
	}
	ctx := r.Context()
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		reply(w, err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var name, bio string
	var owned bool
	if err := tx.QueryRow(ctx, `SELECT name, bio, owner_user_id IS NOT NULL FROM artists WHERE id = $1 FOR UPDATE`, param(r, "id")).Scan(&name, &bio, &owned); err != nil {
		reply(w, err)
		return
	}
	if in.ImageID != nil {
		if owned {
			reply(w, fail4(http.StatusUnprocessableEntity, "owned_artist", "this artist's picture is its owner's profile picture"))
			return
		}
		var avatar *string
		if *in.ImageID != "" {
			if err := image.CheckOwned(ctx, tx, *in.ImageID, actorID(r), "avatar"); err != nil {
				reply(w, invalid("imageId must be a finished avatar image that you uploaded"))
				return
			}
			avatar = in.ImageID
		}
		if _, err := tx.Exec(ctx, `UPDATE artists SET avatar_image_id = $2, updated_at = now() WHERE id = $1`, param(r, "id"), avatar); err != nil {
			reply(w, err)
			return
		}
	}
	before := map[string]any{"name": name, "bio": bio}
	if in.Name != nil {
		n := strings.TrimSpace(*in.Name)
		if c := utf8.RuneCountInString(n); c < 1 || c > 100 {
			reply(w, invalid("name must be 1-100 characters"))
			return
		}
		name = n
	}
	if in.Bio != nil {
		b := strings.TrimSpace(*in.Bio)
		if utf8.RuneCountInString(b) > 1000 {
			reply(w, invalid("bio must be at most 1000 characters"))
			return
		}
		bio = b
	}
	if _, err := tx.Exec(ctx, `UPDATE artists SET name = $2, bio = $3, updated_at = now() WHERE id = $1`, param(r, "id"), name, bio); err != nil {
		reply(w, err)
		return
	}
	if err := h.log(ctx, tx, r, audit.Entry{Action: "artist.update", TargetType: "artist", TargetID: param(r, "id"), TargetLabel: name,
		Details: map[string]any{"before": before, "after": map[string]any{"name": name, "bio": bio}, "imageId": in.ImageID}}); err != nil {
		reply(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		reply(w, err)
		return
	}
	h.getArtist(w, r)
}

// POST /v1/admin/artists/{id}/ban {reason}
func (h *Handler) banArtist(w http.ResponseWriter, r *http.Request) {
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
	id := param(r, "id")
	var name, ownerRole string
	var banned bool
	err = tx.QueryRow(ctx, `
		SELECT a.name, a.banned_at IS NOT NULL, COALESCE(u.role, '') FROM artists a LEFT JOIN users u ON u.id = a.owner_user_id
		WHERE a.id = $1 FOR UPDATE OF a`, id).Scan(&name, &banned, &ownerRole)
	if err != nil {
		reply(w, err)
		return
	}
	switch {
	case ownerRole == "admin":
		reply(w, errIsAdmin)
		return
	case banned:
		reply(w, errAlreadyBanned)
		return
	}
	admin := actorID(r)
	if _, err := tx.Exec(ctx, `UPDATE artists SET banned_at = now(), ban_reason = $2, banned_by = $3::uuid WHERE id = $1`, id, why, admin); err != nil {
		reply(w, err)
		return
	}
	n, err := takeDownArtistReleases(ctx, tx, id, admin, "artist suspended: "+why)
	if err != nil {
		reply(w, err)
		return
	}
	if err := h.log(ctx, tx, r, audit.Entry{Action: "artist.ban", TargetType: "artist", TargetID: id, TargetLabel: name, Reason: why,
		Details: map[string]any{"releasesTakenDown": n}}); err != nil {
		reply(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		reply(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /v1/admin/artists/{id}/unban {note?}
func (h *Handler) unbanArtist(w http.ResponseWriter, r *http.Request) {
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
	id := param(r, "id")
	var name string
	var banned, userBanned bool
	err = tx.QueryRow(ctx, `
		SELECT a.name, a.banned_at IS NOT NULL, COALESCE(u.banned_at IS NOT NULL, false)
		FROM artists a LEFT JOIN users u ON u.id = a.owner_user_id WHERE a.id = $1 FOR UPDATE OF a`, id).Scan(&name, &banned, &userBanned)
	if err != nil {
		reply(w, err)
		return
	}
	switch {
	case !banned:
		reply(w, errNotBanned)
		return
	case userBanned:
		reply(w, fail4(http.StatusConflict, "user_banned", "the account is banned too: unban the user instead"))
		return
	}
	if _, err := tx.Exec(ctx, `UPDATE artists SET banned_at = NULL, ban_reason = '', banned_by = NULL WHERE id = $1`, id); err != nil {
		reply(w, err)
		return
	}
	n, err := restoreArtistReleases(ctx, tx, id)
	if err != nil {
		reply(w, err)
		return
	}
	if err := h.log(ctx, tx, r, audit.Entry{Action: "artist.unban", TargetType: "artist", TargetID: id, TargetLabel: name, Reason: note,
		Details: map[string]any{"releasesRestored": n}}); err != nil {
		reply(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		reply(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) artistPreview(ctx context.Context, id string) (map[string]any, string, error) {
	var name string
	if err := h.pool.QueryRow(ctx, `SELECT name FROM artists WHERE id = $1`, id).Scan(&name); err != nil {
		if err == pgx.ErrNoRows {
			return nil, "", errNotFound
		}
		return nil, "", err
	}
	var releases, tracks, uploads, grants, followers int
	var storage int64
	if err := h.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM albums WHERE artist_id = $1), (SELECT count(*) FROM tracks WHERE artist_id = $1),
		       (SELECT count(*) FROM uploads WHERE artist_id = $1),
		       (SELECT COALESCE(sum(size_bytes), 0) FROM uploads WHERE artist_id = $1 AND status <> 'failed'),
		       (SELECT count(*) FROM storage_grants WHERE artist_id = $1), (SELECT count(*) FROM library_artists WHERE artist_id = $1)`, id).
		Scan(&releases, &tracks, &uploads, &storage, &grants, &followers); err != nil {
		return nil, "", err
	}
	return map[string]any{"releases": releases, "tracks": tracks, "uploads": uploads, "storageBytes": storage,
		"storageGrants": grants, "followers": followers}, name, nil
}

// GET /v1/admin/artists/{id}/deletion-preview
func (h *Handler) artistDeletionPreview(w http.ResponseWriter, r *http.Request) {
	p, _, err := h.artistPreview(r.Context(), param(r, "id"))
	if err != nil {
		reply(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, p)
}

// DELETE /v1/admin/artists/{id} {reason, confirmName}: the profile, its releases, tracks, files and storage grants go; the owner's account stays.
func (h *Handler) deleteArtist(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Reason      string `json:"reason"`
		ConfirmName string `json:"confirmName"`
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
	preview, name, err := h.artistPreview(ctx, id)
	if err != nil {
		reply(w, err)
		return
	}
	var ownerRole string
	if err := h.pool.QueryRow(ctx, `SELECT COALESCE(u.role, '') FROM artists a LEFT JOIN users u ON u.id = a.owner_user_id WHERE a.id = $1`, id).Scan(&ownerRole); err != nil {
		reply(w, err)
		return
	}
	if ownerRole == "admin" {
		reply(w, errIsAdmin)
		return
	}
	if strings.TrimSpace(in.ConfirmName) != name {
		reply(w, fail4(http.StatusUnprocessableEntity, "confirm_mismatch", "confirmName must be the artist's name exactly"))
		return
	}
	// releases (with their tracks, files and uploads), tracks of no release, leftover uploads
	for {
		rows, err := h.pool.Query(ctx, `SELECT id FROM albums WHERE artist_id = $1 LIMIT 50`, id)
		if err != nil {
			reply(w, err)
			return
		}
		var ids []string
		for rows.Next() {
			var rid string
			if err := rows.Scan(&rid); err != nil {
				rows.Close()
				reply(w, err)
				return
			}
			ids = append(ids, rid)
		}
		rows.Close()
		if len(ids) == 0 {
			break
		}
		for _, rid := range ids {
			if err := h.Releases.AdminDelete(ctx, rid, nil); err != nil {
				reply(w, err)
				return
			}
		}
	}
	trows, err := h.pool.Query(ctx, `SELECT id FROM tracks WHERE artist_id = $1`, id)
	if err != nil {
		reply(w, err)
		return
	}
	var tracks []string
	for trows.Next() {
		var t string
		if err := trows.Scan(&t); err != nil {
			trows.Close()
			reply(w, err)
			return
		}
		tracks = append(tracks, t)
	}
	trows.Close()
	for _, t := range tracks {
		if err := h.Releases.AdminDeleteTrack(ctx, t, nil); err != nil {
			reply(w, err)
			return
		}
	}
	if err := h.Releases.PurgeArtistUploads(ctx, id); err != nil {
		reply(w, err)
		return
	}
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		reply(w, err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `DELETE FROM artists WHERE id = $1`, id); err != nil {
		reply(w, err)
		return
	}
	if err := h.log(ctx, tx, r, audit.Entry{Action: "artist.delete", TargetType: "artist", TargetID: id, TargetLabel: name, Reason: why, Details: preview}); err != nil {
		reply(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		reply(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
