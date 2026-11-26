package admin

import (
	"context"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/blacmusic/backend/internal/audit"
	"github.com/blacmusic/backend/internal/genre"
	"github.com/blacmusic/backend/internal/httpx"
	"github.com/blacmusic/backend/internal/image"
	"github.com/blacmusic/backend/internal/release"
)

func (h *Handler) releaseRoutes(r chi.Router) {
	r.Get("/releases", h.listReleases)
	r.Get("/releases/{id}", h.getRelease)
	r.Patch("/releases/{id}", h.patchRelease)
	r.Post("/releases/{id}/takedown", h.takedownRelease)
	r.Post("/releases/{id}/restore", h.restoreRelease)
	r.Get("/releases/{id}/deletion-preview", h.releaseDeletionPreview)
	r.Delete("/releases/{id}", h.deleteRelease)
	r.Patch("/tracks/{id}", h.patchTrack)
	r.Delete("/tracks/{id}", h.deleteTrack)
}

type releaseRow struct {
	ID                 string     `json:"id"`
	Title              string     `json:"title"`
	Type               string     `json:"type"`
	Kind               string     `json:"kind"`
	ArtistID           string     `json:"artistId,omitempty"`
	ArtistName         string     `json:"artistName,omitempty"`
	Visibility         string     `json:"visibility"`
	TakenDown          bool       `json:"takenDown"`
	PreviousVisibility string     `json:"previousVisibility,omitempty"`
	TrackCount         int        `json:"trackCount"`
	CoverURL           string     `json:"coverUrl,omitempty"`
	PublishedAt        *time.Time `json:"publishedAt"`
	CreatedAt          time.Time  `json:"createdAt"`
}

// GET /v1/admin/releases?q=&visibility=draft|scheduled|public&takenDown=true|false&artistId=&type=single|album&limit=&offset=
// Includes drafts and taken-down releases. visibility is the stored state, which is "draft" for a taken-down
// release (that is how it is hidden): previousVisibility says what it was before the takedown.
func (h *Handler) listReleases(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	switch q.Get("visibility") {
	case "", "draft", "scheduled", "public":
	default:
		reply(w, fail4(http.StatusBadRequest, "bad_request", "visibility must be draft, scheduled or public"))
		return
	}
	switch q.Get("type") {
	case "", "single", "album":
	default:
		reply(w, fail4(http.StatusBadRequest, "bad_request", "type must be single or album"))
		return
	}
	if v := q.Get("takenDown"); v != "" && v != "true" && v != "false" {
		reply(w, fail4(http.StatusBadRequest, "bad_request", "takenDown must be true or false"))
		return
	}
	limit, offset := page(r)
	search := strings.TrimSpace(q.Get("q"))
	where := ` WHERE ($1 = '' OR al.title ILIKE $2 OR a.name ILIKE $2 OR al.id = $1) AND ($3 = '' OR al.visibility = $3)
		AND ($4 = '' OR ($4 = 'true') = (al.taken_down_at IS NOT NULL)) AND ($5 = '' OR al.artist_id = $5) AND ($6 = '' OR al.release_type = $6)`
	args := []any{search, like(search), q.Get("visibility"), q.Get("takenDown"), q.Get("artistId"), q.Get("type")}
	var total int
	if err := h.pool.QueryRow(r.Context(), `SELECT count(*) FROM albums al LEFT JOIN artists a ON a.id = al.artist_id`+where, args...).Scan(&total); err != nil {
		reply(w, err)
		return
	}
	rows, err := h.pool.Query(r.Context(), `
		SELECT al.id, al.title, al.release_type, COALESCE(a.id, ''), COALESCE(a.name, ''), al.visibility, al.taken_down_at IS NOT NULL,
		       COALESCE(al.taken_down_prev_visibility, ''), (SELECT count(*) FROM tracks t WHERE t.album_id = al.id),
		       COALESCE(al.artwork_url, ''), al.published_at, al.created_at
		FROM albums al LEFT JOIN artists a ON a.id = al.artist_id`+where+` ORDER BY al.created_at DESC, al.id LIMIT $7 OFFSET $8`, append(args, limit, offset)...)
	if err != nil {
		reply(w, err)
		return
	}
	defer rows.Close()
	out := []releaseRow{}
	for rows.Next() {
		var x releaseRow
		if err := rows.Scan(&x.ID, &x.Title, &x.Type, &x.ArtistID, &x.ArtistName, &x.Visibility, &x.TakenDown, &x.PreviousVisibility,
			&x.TrackCount, &x.CoverURL, &x.PublishedAt, &x.CreatedAt); err != nil {
			reply(w, err)
			return
		}
		x.Kind = release.KindOf(x.Type, x.TrackCount)
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		reply(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"releases": out, "total": total})
}

type trackRow struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	TrackNumber *int   `json:"trackNumber"`
	DurationMs  int    `json:"durationMs"`
	Visibility  string `json:"visibility"`
	Explicit    bool   `json:"explicit"`
	Genre       string `json:"genre"`
}

// GET /v1/admin/releases/{id}: the release as its owner sees it (any visibility), its tracks and takedown information.
func (h *Handler) getRelease(w http.ResponseWriter, r *http.Request) {
	id := param(r, "id")
	ctx := r.Context()
	var d struct {
		releaseRow
		Description string         `json:"description"`
		Genre       string         `json:"genre"`
		ReleaseDate string         `json:"releaseDate,omitempty"`
		PublishAt   *time.Time     `json:"publishAt"`
		UpdatedAt   time.Time      `json:"updatedAt"`
		Artist      map[string]any `json:"artist"`
		Takedown    map[string]any `json:"takedown"`
		Tracks      []trackRow     `json:"tracks"`
		Uploads     []uploadRow    `json:"uploads"`
	}
	var downAt *time.Time
	var reasonText, byID, byEmail, prev string
	var byBan, artistBanned bool
	err := h.pool.QueryRow(ctx, `
		SELECT al.id, al.title, al.release_type, COALESCE(a.id, ''), COALESCE(a.name, ''), al.visibility, al.taken_down_at IS NOT NULL,
		       COALESCE(al.taken_down_prev_visibility, ''), (SELECT count(*) FROM tracks t WHERE t.album_id = al.id), COALESCE(al.artwork_url, ''),
		       al.published_at, al.created_at, al.description, al.genre, COALESCE(to_char(al.release_date, 'YYYY-MM-DD'), ''), al.publish_at, al.updated_at,
		       al.taken_down_at, al.takedown_reason, COALESCE(al.taken_down_by::text, ''), COALESCE((SELECT email FROM users b WHERE b.id = al.taken_down_by), ''),
		       al.taken_down_by_ban, COALESCE(a.banned_at IS NOT NULL, false)
		FROM albums al LEFT JOIN artists a ON a.id = al.artist_id WHERE al.id = $1`, id).
		Scan(&d.ID, &d.Title, &d.Type, &d.ArtistID, &d.ArtistName, &d.Visibility, &d.TakenDown, &prev, &d.TrackCount, &d.CoverURL,
			&d.PublishedAt, &d.CreatedAt, &d.Description, &d.Genre, &d.ReleaseDate, &d.PublishAt, &d.UpdatedAt,
			&downAt, &reasonText, &byID, &byEmail, &byBan, &artistBanned)
	if err != nil {
		reply(w, err)
		return
	}
	d.PreviousVisibility = prev
	d.Kind = release.KindOf(d.Type, d.TrackCount)
	if d.ArtistID != "" {
		d.Artist = map[string]any{"id": d.ArtistID, "name": d.ArtistName, "banned": artistBanned}
	}
	if downAt != nil {
		var by any
		if byID != "" {
			by = map[string]any{"id": byID, "email": byEmail}
		}
		d.Takedown = map[string]any{"at": downAt, "reason": reasonText, "by": by, "previousVisibility": prev, "byBan": byBan}
	}
	d.Tracks = []trackRow{}
	rows, err := h.pool.Query(ctx, `SELECT id, title, track_number, duration_ms, visibility, explicit, genre FROM tracks WHERE album_id = $1
		ORDER BY track_number NULLS LAST, created_at, id`, id)
	if err != nil {
		reply(w, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var t trackRow
		var g *string
		if err := rows.Scan(&t.ID, &t.Title, &t.TrackNumber, &t.DurationMs, &t.Visibility, &t.Explicit, &g); err != nil {
			reply(w, err)
			return
		}
		if g != nil {
			t.Genre = *g
		}
		d.Tracks = append(d.Tracks, t)
	}
	if err := rows.Err(); err != nil {
		reply(w, err)
		return
	}
	d.Uploads = []uploadRow{}
	urows, err := h.pool.Query(ctx, `SELECT id, status, title, filename, size_bytes, COALESCE(error, ''), track_number FROM uploads
		WHERE release_id = $1 AND status <> 'ready' ORDER BY created_at, id`, id)
	if err != nil {
		reply(w, err)
		return
	}
	defer urows.Close()
	for urows.Next() {
		var u uploadRow
		if err := urows.Scan(&u.ID, &u.Status, &u.Title, &u.Filename, &u.SizeBytes, &u.Error, &u.TrackNumber); err != nil {
			reply(w, err)
			return
		}
		d.Uploads = append(d.Uploads, u)
	}
	if err := urows.Err(); err != nil {
		reply(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, d)
}

// uploadRow is a file that is not a track yet (waiting, being processed, or failed).
type uploadRow struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	Title       string `json:"title"`
	Filename    string `json:"filename"`
	SizeBytes   int64  `json:"sizeBytes"`
	Error       string `json:"error,omitempty"`
	TrackNumber *int   `json:"trackNumber,omitempty"`
}

// PATCH /v1/admin/releases/{id} {title?, description?, genre?, releaseType?}: any release, any state.
func (h *Handler) patchRelease(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title        *string `json:"title"`
		Description  *string `json:"description"`
		Genre        *string `json:"genre"`
		ReleaseType  *string `json:"releaseType"`
		ReleaseDate  *string `json:"releaseDate"`
		CoverImageID *string `json:"coverImageId"`
	}
	if err := decodeBody(w, r, &in, false); err != nil {
		reply(w, err)
		return
	}
	if in.Title == nil && in.Description == nil && in.Genre == nil && in.ReleaseType == nil && in.ReleaseDate == nil && in.CoverImageID == nil {
		reply(w, invalid("send title, description, genre, releaseType, releaseDate and/or coverImageId"))
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
	var title, description, g, typ, date, owner string
	var tracks int
	if err := tx.QueryRow(ctx, `
		SELECT al.title, al.description, al.genre, al.release_type, COALESCE(to_char(al.release_date, 'YYYY-MM-DD'), ''),
		       (SELECT count(*) FROM tracks WHERE album_id = $1), COALESCE(a.owner_user_id::text, '')
		FROM albums al LEFT JOIN artists a ON a.id = al.artist_id WHERE al.id = $1 FOR UPDATE OF al`, id).
		Scan(&title, &description, &g, &typ, &date, &tracks, &owner); err != nil {
		reply(w, err)
		return
	}
	before := map[string]any{"title": title, "description": description, "genre": g, "releaseType": typ, "releaseDate": date}
	if in.Title != nil {
		title = strings.TrimSpace(*in.Title)
		if n := utf8.RuneCountInString(title); n < 1 || n > 200 {
			reply(w, invalid("title must be 1-200 characters"))
			return
		}
	}
	if in.Description != nil {
		description = strings.TrimSpace(*in.Description)
		if utf8.RuneCountInString(description) > 2000 {
			reply(w, invalid("description must be at most 2000 characters"))
			return
		}
	}
	if in.Genre != nil {
		ng, err := genre.Normalize(strings.TrimSpace(*in.Genre))
		if err != nil {
			reply(w, invalid(err.Error()))
			return
		}
		g = ng
	}
	if in.ReleaseType != nil {
		if *in.ReleaseType != "single" && *in.ReleaseType != "album" {
			reply(w, invalid("releaseType must be single or album"))
			return
		}
		if *in.ReleaseType == "single" && tracks > 1 {
			reply(w, invalid("a single has one track: this release has "+itoa(tracks)))
			return
		}
		typ = *in.ReleaseType
	}
	if in.ReleaseDate != nil {
		date = strings.TrimSpace(*in.ReleaseDate)
		if date != "" {
			if _, err := time.Parse("2006-01-02", date); err != nil {
				reply(w, invalid("releaseDate must look like 2026-11-30 (empty removes it)"))
				return
			}
		}
	}
	var newDate *string
	if date != "" {
		newDate = &date
	}
	if _, err := tx.Exec(ctx, `UPDATE albums SET title = $2, description = $3, genre = $4, release_type = $5, release_date = $6::date, updated_at = now() WHERE id = $1`,
		id, title, description, g, typ, newDate); err != nil {
		reply(w, err)
		return
	}
	if in.CoverImageID != nil {
		var cover, coverURL *string
		if *in.CoverImageID != "" {
			ok := false
			for _, who := range []string{actorID(r), owner} {
				if who != "" && image.CheckOwned(ctx, tx, *in.CoverImageID, who, "cover") == nil {
					ok = true
					break
				}
			}
			if !ok {
				reply(w, invalid("coverImageId must be a finished cover image that you uploaded"))
				return
			}
			u := image.URL(h.PublicURL, *in.CoverImageID, image.SizeMedium)
			cover, coverURL = in.CoverImageID, &u
		}
		if _, err := tx.Exec(ctx, `UPDATE albums SET cover_image_id = $2, artwork_url = $3 WHERE id = $1`, id, cover, coverURL); err != nil {
			reply(w, err)
			return
		}
	}
	if err := h.log(ctx, tx, r, audit.Entry{Action: "release.update", TargetType: "release", TargetID: id, TargetLabel: title,
		Details: map[string]any{"before": before, "after": map[string]any{"title": title, "description": description, "genre": g, "releaseType": typ, "releaseDate": date}, "coverImageId": in.CoverImageID}}); err != nil {
		reply(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		reply(w, err)
		return
	}
	h.getRelease(w, r)
}

// PATCH /v1/admin/tracks/{id} {title?, explicit?, genre?, trackNumber?}: trackNumber 0 clears it.
func (h *Handler) patchTrack(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title       *string `json:"title"`
		Explicit    *bool   `json:"explicit"`
		Genre       *string `json:"genre"`
		TrackNumber *int    `json:"trackNumber"`
	}
	if err := decodeBody(w, r, &in, false); err != nil {
		reply(w, err)
		return
	}
	if in.Title == nil && in.Explicit == nil && in.Genre == nil && in.TrackNumber == nil {
		reply(w, invalid("send title, explicit, genre and/or trackNumber"))
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
	var t trackRow
	var g *string
	if err := tx.QueryRow(ctx, `SELECT id, title, track_number, duration_ms, visibility, explicit, genre FROM tracks WHERE id = $1 FOR UPDATE`, id).
		Scan(&t.ID, &t.Title, &t.TrackNumber, &t.DurationMs, &t.Visibility, &t.Explicit, &g); err != nil {
		reply(w, err)
		return
	}
	if g != nil {
		t.Genre = *g
	}
	before := map[string]any{"title": t.Title, "explicit": t.Explicit, "genre": t.Genre, "trackNumber": t.TrackNumber}
	if in.Title != nil {
		t.Title = strings.TrimSpace(*in.Title)
		if n := utf8.RuneCountInString(t.Title); n < 1 || n > 200 {
			reply(w, invalid("title must be 1-200 characters"))
			return
		}
	}
	if in.Explicit != nil {
		t.Explicit = *in.Explicit
	}
	if in.Genre != nil {
		ng, err := genre.Normalize(strings.TrimSpace(*in.Genre))
		if err != nil {
			reply(w, invalid(err.Error()))
			return
		}
		t.Genre = ng
	}
	if in.TrackNumber != nil {
		if *in.TrackNumber < 0 || *in.TrackNumber > 999 {
			reply(w, invalid("trackNumber must be between 1 and 999 (0 clears it)"))
			return
		}
		if *in.TrackNumber == 0 {
			t.TrackNumber = nil
		} else {
			t.TrackNumber = in.TrackNumber
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE tracks SET title = $2, explicit = $3, genre = $4, track_number = $5 WHERE id = $1`, id, t.Title, t.Explicit, t.Genre, t.TrackNumber); err != nil {
		reply(w, err)
		return
	}
	if err := h.log(ctx, tx, r, audit.Entry{Action: "track.update", TargetType: "track", TargetID: id, TargetLabel: t.Title,
		Details: map[string]any{"before": before, "after": map[string]any{"title": t.Title, "explicit": t.Explicit, "genre": t.Genre, "trackNumber": t.TrackNumber}}}); err != nil {
		reply(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		reply(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, t)
}

// POST /v1/admin/releases/{id}/takedown {reason}: hides the release like a draft and keeps the owner from publishing it again.
func (h *Handler) takedownRelease(w http.ResponseWriter, r *http.Request) {
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
	var title, visibility string
	var down bool
	if err := tx.QueryRow(ctx, `SELECT title, visibility, taken_down_at IS NOT NULL FROM albums WHERE id = $1 FOR UPDATE`, id).Scan(&title, &visibility, &down); err != nil {
		reply(w, err)
		return
	}
	if down {
		reply(w, fail4(http.StatusConflict, "already_taken_down", "this release is already taken down"))
		return
	}
	if _, err := tx.Exec(ctx, `
		UPDATE albums SET taken_down_at = now(), takedown_reason = $2, taken_down_by = $3::uuid, taken_down_prev_visibility = visibility,
		       taken_down_prev_publish_at = publish_at, taken_down_by_ban = false, visibility = 'draft', publish_at = NULL, updated_at = now() WHERE id = $1`,
		id, why, actorID(r)); err != nil {
		reply(w, err)
		return
	}
	if _, err := tx.Exec(ctx, `UPDATE tracks SET visibility = 'draft' WHERE album_id = $1`, id); err != nil {
		reply(w, err)
		return
	}
	if err := h.log(ctx, tx, r, audit.Entry{Action: "release.takedown", TargetType: "release", TargetID: id, TargetLabel: title, Reason: why,
		Details: map[string]any{"previousVisibility": visibility}}); err != nil {
		reply(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		reply(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /v1/admin/releases/{id}/restore {note?}: puts a taken-down release back as it was.
func (h *Handler) restoreRelease(w http.ResponseWriter, r *http.Request) {
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
	id := param(r, "id")
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		reply(w, err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var title string
	var down, byBan bool
	if err := tx.QueryRow(ctx, `SELECT title, taken_down_at IS NOT NULL, taken_down_by_ban FROM albums WHERE id = $1 FOR UPDATE`, id).Scan(&title, &down, &byBan); err != nil {
		reply(w, err)
		return
	}
	switch {
	case !down:
		reply(w, fail4(http.StatusConflict, "not_taken_down", "this release is not taken down"))
		return
	case byBan:
		reply(w, fail4(http.StatusConflict, "artist_banned", "this release was hidden because its artist or account is banned: unban them instead"))
		return
	}
	var vis string
	if err := tx.QueryRow(ctx, `
		UPDATE albums SET visibility = COALESCE(taken_down_prev_visibility, 'draft'), publish_at = taken_down_prev_publish_at, taken_down_at = NULL,
		       takedown_reason = '', taken_down_by = NULL, taken_down_prev_visibility = NULL, taken_down_prev_publish_at = NULL, updated_at = now()
		WHERE id = $1 RETURNING visibility`, id).Scan(&vis); err != nil {
		reply(w, err)
		return
	}
	if vis == "public" {
		if _, err := tx.Exec(ctx, `UPDATE tracks SET visibility = 'public' WHERE album_id = $1`, id); err != nil {
			reply(w, err)
			return
		}
	}
	if err := h.log(ctx, tx, r, audit.Entry{Action: "release.restore", TargetType: "release", TargetID: id, TargetLabel: title, Reason: note,
		Details: map[string]any{"visibility": vis}}); err != nil {
		reply(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		reply(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) releasePreview(ctx context.Context, id string) (map[string]any, string, error) {
	var title string
	if err := h.pool.QueryRow(ctx, `SELECT title FROM albums WHERE id = $1`, id).Scan(&title); err != nil {
		if err == pgx.ErrNoRows {
			return nil, "", errNotFound
		}
		return nil, "", err
	}
	var tracks, files, libs, pls int
	var bytes int64
	if err := h.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM tracks WHERE album_id = $1), (SELECT count(*) FROM uploads WHERE release_id = $1),
		       (SELECT COALESCE(sum(size_bytes), 0) FROM uploads WHERE release_id = $1 AND status <> 'failed'),
		       (SELECT count(*) FROM library_albums WHERE album_id = $1) + (SELECT count(*) FROM library_tracks l JOIN tracks t ON t.id = l.track_id WHERE t.album_id = $1),
		       (SELECT count(*) FROM playlist_items i JOIN tracks t ON t.id = i.track_id WHERE t.album_id = $1)`, id).
		Scan(&tracks, &files, &bytes, &libs, &pls); err != nil {
		return nil, "", err
	}
	return map[string]any{"tracks": tracks, "files": files, "storageBytes": bytes, "libraryReferences": libs, "playlistReferences": pls}, title, nil
}

// GET /v1/admin/releases/{id}/deletion-preview
func (h *Handler) releaseDeletionPreview(w http.ResponseWriter, r *http.Request) {
	p, _, err := h.releasePreview(r.Context(), param(r, "id"))
	if err != nil {
		reply(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, p)
}

// DELETE /v1/admin/releases/{id} {reason}
func (h *Handler) deleteRelease(w http.ResponseWriter, r *http.Request) {
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
	id := param(r, "id")
	preview, title, err := h.releasePreview(r.Context(), id)
	if err != nil {
		reply(w, err)
		return
	}
	err = h.Releases.AdminDelete(r.Context(), id, func(ctx context.Context, tx pgx.Tx) error {
		return h.log(ctx, tx, r, audit.Entry{Action: "release.delete", TargetType: "release", TargetID: id, TargetLabel: title, Reason: why, Details: preview})
	})
	if err != nil {
		reply(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DELETE /v1/admin/tracks/{id} {reason}: a live release left with no tracks goes back to draft.
func (h *Handler) deleteTrack(w http.ResponseWriter, r *http.Request) {
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
	id := param(r, "id")
	var title, releaseID string
	if err := h.pool.QueryRow(r.Context(), `SELECT title, COALESCE(album_id, '') FROM tracks WHERE id = $1`, id).Scan(&title, &releaseID); err != nil {
		reply(w, err)
		return
	}
	err = h.Releases.AdminDeleteTrack(r.Context(), id, func(ctx context.Context, tx pgx.Tx) error {
		return h.log(ctx, tx, r, audit.Entry{Action: "track.delete", TargetType: "track", TargetID: id, TargetLabel: title, Reason: why,
			Details: map[string]any{"releaseId": releaseID}})
	})
	if err != nil {
		reply(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for ; n > 0; n /= 10 {
		s = string(rune('0'+n%10)) + s
	}
	return s
}
