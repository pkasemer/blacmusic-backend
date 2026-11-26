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
	"github.com/blacmusic/backend/internal/httpx"
	"github.com/blacmusic/backend/internal/image"
	"github.com/blacmusic/backend/internal/playlist"
)

// Editorial playlists are the playlists owned by one dedicated account (EDITORIAL_OWNER_EMAIL), which must
// exist and be an administrator. Only that account's playlists are reachable here.
func (h *Handler) editorialRoutes(r chi.Router) {
	r.Get("/playlists", h.listEditorial)
	r.Post("/playlists", h.createEditorial)
	r.Get("/playlists/{id}", h.getEditorial)
	r.Patch("/playlists/{id}", h.patchEditorial)
	r.Put("/playlists/{id}/items", h.putEditorialItems)
	r.Post("/playlists/{id}/publish", h.publishEditorial)
	r.Post("/playlists/{id}/unpublish", h.unpublishEditorial)
	r.Delete("/playlists/{id}", h.deleteEditorial)
}

var (
	errEditorialUnset   = fail4(http.StatusConflict, "editorial_not_configured", "the editorial account is not configured (EDITORIAL_OWNER_EMAIL)")
	errEditorialMissing = fail4(http.StatusConflict, "editorial_account_missing", "the editorial account must exist and be an administrator")
)

// editorialOwner is the user id of the editorial account.
func (h *Handler) editorialOwner(ctx context.Context) (string, error) {
	if strings.TrimSpace(h.EditorialEmail) == "" {
		return "", errEditorialUnset
	}
	var id string
	err := h.pool.QueryRow(ctx, `SELECT id::text FROM users WHERE lower(email) = lower($1) AND role = 'admin' AND banned_at IS NULL`, strings.TrimSpace(h.EditorialEmail)).Scan(&id)
	if err == pgx.ErrNoRows {
		return "", errEditorialMissing
	}
	return id, err
}

type groupRef struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type editorialRow struct {
	ID                 string     `json:"id"`
	Name               string     `json:"name"`
	Description        string     `json:"description"`
	IsPublic           bool       `json:"isPublic"`
	IsFeatured         bool       `json:"isFeatured"`
	Published          bool       `json:"published"` // public and featured: shown on Home
	TrackCount         int        `json:"trackCount"`
	PlayableTrackCount int        `json:"playableTrackCount"`
	CoverURL           string     `json:"coverUrl,omitempty"`
	UpdatedAt          time.Time  `json:"updatedAt"`
	Groups             []groupRef `json:"groups"`
}

const editorialSelect = `
	SELECT p.id, p.name, p.description, p.is_public, p.is_featured, p.is_public AND p.is_featured,
	       (SELECT count(*) FROM playlist_items i WHERE i.playlist_id = p.id),
	       (SELECT count(*) FROM playlist_items i JOIN tracks t ON t.id = i.track_id WHERE i.playlist_id = p.id AND t.visibility = 'public'),
	       p.cover_image_id, p.updated_at
	FROM playlists p`

func (h *Handler) scanEditorial(rows pgx.Rows) ([]editorialRow, error) {
	defer rows.Close()
	out := []editorialRow{}
	for rows.Next() {
		var x editorialRow
		var cover *string
		if err := rows.Scan(&x.ID, &x.Name, &x.Description, &x.IsPublic, &x.IsFeatured, &x.Published, &x.TrackCount, &x.PlayableTrackCount, &cover, &x.UpdatedAt); err != nil {
			return nil, err
		}
		if cover != nil && *cover != "" {
			x.CoverURL = image.URL(h.PublicURL, *cover, image.SizeMedium)
		}
		x.Groups = []groupRef{}
		out = append(out, x)
	}
	return out, rows.Err()
}

// withGroups fills in the Home groups each playlist belongs to.
func (h *Handler) withGroups(ctx context.Context, list []editorialRow) error {
	if len(list) == 0 {
		return nil
	}
	ids := make([]string, len(list))
	idx := map[string]int{}
	for i, p := range list {
		ids[i], idx[p.ID] = p.ID, i
	}
	rows, err := h.pool.Query(ctx, `SELECT gi.playlist_id, g.id, g.title FROM playlist_group_items gi JOIN playlist_groups g ON g.id = gi.group_id
		WHERE gi.playlist_id = ANY($1) ORDER BY g.position, g.id`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var pid string
		var g groupRef
		if err := rows.Scan(&pid, &g.ID, &g.Title); err != nil {
			return err
		}
		list[idx[pid]].Groups = append(list[idx[pid]].Groups, g)
	}
	return rows.Err()
}

// GET /v1/admin/playlists?q=&featured=true|false&published=true|false&limit=&offset=
func (h *Handler) listEditorial(w http.ResponseWriter, r *http.Request) {
	owner, err := h.editorialOwner(r.Context())
	if err != nil {
		reply(w, err)
		return
	}
	q := r.URL.Query()
	for _, k := range []string{"featured", "published"} {
		if v := q.Get(k); v != "" && v != "true" && v != "false" {
			reply(w, fail4(http.StatusBadRequest, "bad_request", k+" must be true or false"))
			return
		}
	}
	limit, offset := page(r)
	search := strings.TrimSpace(q.Get("q"))
	where := ` WHERE p.owner_id::text = $1 AND ($2 = '' OR p.name ILIKE $3) AND ($4 = '' OR ($4 = 'true') = p.is_featured)
		AND ($5 = '' OR ($5 = 'true') = (p.is_public AND p.is_featured))`
	args := []any{owner, search, like(search), q.Get("featured"), q.Get("published")}
	var total int
	if err := h.pool.QueryRow(r.Context(), `SELECT count(*) FROM playlists p`+where, args...).Scan(&total); err != nil {
		reply(w, err)
		return
	}
	rows, err := h.pool.Query(r.Context(), editorialSelect+where+` ORDER BY p.updated_at DESC, p.id LIMIT $6 OFFSET $7`, append(args, limit, offset)...)
	if err != nil {
		reply(w, err)
		return
	}
	list, err := h.scanEditorial(rows)
	if err == nil {
		err = h.withGroups(r.Context(), list)
	}
	if err != nil {
		reply(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"playlists": list, "total": total})
}

type editorialItem struct {
	ItemID     string `json:"itemId"`
	TrackID    string `json:"trackId"`
	Title      string `json:"title"`
	ArtistName string `json:"artistName"`
	DurationMs int    `json:"durationMs"`
	Position   int    `json:"position"`
	Public     bool   `json:"public"` // false: hidden from listeners (draft, taken down...), flagged for the editor
}

// editorialDetail answers with the playlist and its ordered items.
func (h *Handler) editorialDetail(w http.ResponseWriter, r *http.Request, owner string) {
	ctx := r.Context()
	rows, err := h.pool.Query(ctx, editorialSelect+` WHERE p.id = $1 AND p.owner_id::text = $2`, param(r, "id"), owner)
	if err != nil {
		reply(w, err)
		return
	}
	list, err := h.scanEditorial(rows)
	if err != nil {
		reply(w, err)
		return
	}
	if len(list) == 0 {
		reply(w, errNotFound)
		return
	}
	if err := h.withGroups(ctx, list); err != nil {
		reply(w, err)
		return
	}
	irows, err := h.pool.Query(ctx, `
		SELECT i.id::text, t.id, t.title, COALESCE(a.name, ''), t.duration_ms, i.position, t.visibility = 'public'
		FROM playlist_items i JOIN tracks t ON t.id = i.track_id LEFT JOIN artists a ON a.id = t.artist_id
		WHERE i.playlist_id = $1 ORDER BY i.position, i.added_at`, param(r, "id"))
	if err != nil {
		reply(w, err)
		return
	}
	defer irows.Close()
	items := []editorialItem{}
	for irows.Next() {
		var x editorialItem
		if err := irows.Scan(&x.ItemID, &x.TrackID, &x.Title, &x.ArtistName, &x.DurationMs, &x.Position, &x.Public); err != nil {
			reply(w, err)
			return
		}
		items = append(items, x)
	}
	if err := irows.Err(); err != nil {
		reply(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, struct {
		editorialRow
		Items []editorialItem `json:"items"`
	}{list[0], items})
}

// GET /v1/admin/playlists/{id}
func (h *Handler) getEditorial(w http.ResponseWriter, r *http.Request) {
	owner, err := h.editorialOwner(r.Context())
	if err != nil {
		reply(w, err)
		return
	}
	h.editorialDetail(w, r, owner)
}

func playlistText(name string, description *string) error {
	if n := utf8.RuneCountInString(name); n < 1 || n > 100 {
		return invalid("name must be 1-100 characters")
	}
	if description != nil && utf8.RuneCountInString(*description) > 500 {
		return invalid("description must be at most 500 characters")
	}
	return nil
}

// POST /v1/admin/playlists {name, description?} -> 201: a new, unpublished (private, not featured) editorial playlist.
func (h *Handler) createEditorial(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := decodeBody(w, r, &in, false); err != nil {
		reply(w, err)
		return
	}
	in.Name, in.Description = strings.TrimSpace(in.Name), strings.TrimSpace(in.Description)
	if err := playlistText(in.Name, &in.Description); err != nil {
		reply(w, err)
		return
	}
	ctx := r.Context()
	owner, err := h.editorialOwner(ctx)
	if err != nil {
		reply(w, err)
		return
	}
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		reply(w, err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var id string
	if err := tx.QueryRow(ctx, `INSERT INTO playlists (owner_id, name, description, is_public) VALUES ($1::uuid, $2, $3, false) RETURNING id`, owner, in.Name, in.Description).Scan(&id); err != nil {
		reply(w, err)
		return
	}
	if err := h.log(ctx, tx, r, audit.Entry{Action: "playlist.create", TargetType: "playlist", TargetID: id, TargetLabel: in.Name}); err != nil {
		reply(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		reply(w, err)
		return
	}
	chi.RouteContext(r.Context()).URLParams.Add("id", id)
	w.Header().Set("Location", "/v1/admin/playlists/"+id)
	rec := &statusWriter{ResponseWriter: w, status: http.StatusCreated}
	h.editorialDetail(rec, r, owner)
}

// statusWriter makes the first write use a chosen status (201 for a create).
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusWriter) WriteHeader(code int) {
	if !s.wrote {
		s.wrote = true
		if code == http.StatusOK {
			code = s.status
		}
		s.ResponseWriter.WriteHeader(code)
	}
}

func (s *statusWriter) Write(b []byte) (int, error) {
	s.WriteHeader(http.StatusOK)
	return s.ResponseWriter.Write(b)
}

// lockEditorial locks one of the editorial playlists.
func (h *Handler) lockEditorial(ctx context.Context, tx pgx.Tx, id, owner string) (name string, err error) {
	err = tx.QueryRow(ctx, `SELECT name FROM playlists WHERE id = $1 AND owner_id::text = $2 FOR UPDATE`, id, owner).Scan(&name)
	if err == pgx.ErrNoRows {
		err = errNotFound
	}
	return
}

// PATCH /v1/admin/playlists/{id} {name?, description?, coverImageId?} ("" removes the cover; the image must be a finished
// cover image of the editorial account or of the administrator making the change)
func (h *Handler) patchEditorial(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name         *string `json:"name"`
		Description  *string `json:"description"`
		CoverImageID *string `json:"coverImageId"`
	}
	if err := decodeBody(w, r, &in, false); err != nil {
		reply(w, err)
		return
	}
	if in.Name == nil && in.Description == nil && in.CoverImageID == nil {
		reply(w, invalid("send name, description and/or coverImageId"))
		return
	}
	ctx := r.Context()
	owner, err := h.editorialOwner(ctx)
	if err != nil {
		reply(w, err)
		return
	}
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		reply(w, err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	id := param(r, "id")
	name, err := h.lockEditorial(ctx, tx, id, owner)
	if err != nil {
		reply(w, err)
		return
	}
	if in.Name != nil {
		n := strings.TrimSpace(*in.Name)
		if err := playlistText(n, nil); err != nil {
			reply(w, err)
			return
		}
		in.Name = &n
		name = n
	}
	if in.Description != nil {
		d := strings.TrimSpace(*in.Description)
		if err := playlistText("x", &d); err != nil {
			reply(w, err)
			return
		}
		in.Description = &d
	}
	setCover := in.CoverImageID != nil
	var cover *string
	if setCover && *in.CoverImageID != "" {
		ok := false
		for _, who := range []string{owner, actorID(r)} {
			if image.CheckOwned(ctx, tx, *in.CoverImageID, who, "cover") == nil {
				ok = true
				break
			}
		}
		if !ok {
			reply(w, invalid("coverImageId must be a finished cover image of the editorial account or of yours"))
			return
		}
		cover = in.CoverImageID
	}
	if _, err := tx.Exec(ctx, `UPDATE playlists SET name = COALESCE($2, name), description = COALESCE($3, description),
		cover_image_id = CASE WHEN $4 THEN $5 ELSE cover_image_id END, updated_at = now() WHERE id = $1`, id, in.Name, in.Description, setCover, cover); err != nil {
		reply(w, err)
		return
	}
	if err := h.log(ctx, tx, r, audit.Entry{Action: "playlist.update", TargetType: "playlist", TargetID: id, TargetLabel: name,
		Details: map[string]any{"name": in.Name, "description": in.Description, "coverImageId": in.CoverImageID}}); err != nil {
		reply(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		reply(w, err)
		return
	}
	h.editorialDetail(w, r, owner)
}

// PUT /v1/admin/playlists/{id}/items {trackIds:[...]}: replaces the whole ordered list in one transaction. Any existing
// track may be listed (tracks hidden from listeners are allowed and flagged); a track may appear twice.
func (h *Handler) putEditorialItems(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TrackIDs []string `json:"trackIds"`
	}
	if err := decodeBody(w, r, &in, false); err != nil || in.TrackIDs == nil {
		reply(w, fail4(http.StatusBadRequest, "bad_request", "send {\"trackIds\": [...]} (an empty list empties the playlist)"))
		return
	}
	if len(in.TrackIDs) > playlist.MaxItemsPerPlaylist {
		reply(w, invalid("too many tracks"))
		return
	}
	ctx := r.Context()
	owner, err := h.editorialOwner(ctx)
	if err != nil {
		reply(w, err)
		return
	}
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		reply(w, err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	id := param(r, "id")
	name, err := h.lockEditorial(ctx, tx, id, owner)
	if err != nil {
		reply(w, err)
		return
	}
	if len(in.TrackIDs) > 0 {
		var found int
		if err := tx.QueryRow(ctx, `SELECT count(DISTINCT id) FROM tracks WHERE id = ANY($1)`, in.TrackIDs).Scan(&found); err != nil {
			reply(w, err)
			return
		}
		distinct := map[string]bool{}
		for _, t := range in.TrackIDs {
			distinct[t] = true
		}
		if found != len(distinct) {
			reply(w, invalid("a trackId does not exist"))
			return
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM playlist_items WHERE playlist_id = $1`, id); err != nil {
		reply(w, err)
		return
	}
	for i, t := range in.TrackIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO playlist_items (playlist_id, track_id, position, added_by) VALUES ($1, $2, $3, $4::uuid)`, id, t, i, actorID(r)); err != nil {
			reply(w, err)
			return
		}
	}
	// a published playlist must keep something to play
	if _, err := tx.Exec(ctx, `UPDATE playlists SET updated_at = now() WHERE id = $1`, id); err != nil {
		reply(w, err)
		return
	}
	if err := h.log(ctx, tx, r, audit.Entry{Action: "playlist.items", TargetType: "playlist", TargetID: id, TargetLabel: name,
		Details: map[string]any{"trackCount": len(in.TrackIDs)}}); err != nil {
		reply(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		reply(w, err)
		return
	}
	h.editorialDetail(w, r, owner)
}

// POST /v1/admin/playlists/{id}/publish: public and featured (needs at least one track that listeners can play: 422 otherwise).
func (h *Handler) publishEditorial(w http.ResponseWriter, r *http.Request) {
	h.setPublished(w, r, true)
}

// POST /v1/admin/playlists/{id}/unpublish: private again and no longer featured.
func (h *Handler) unpublishEditorial(w http.ResponseWriter, r *http.Request) {
	h.setPublished(w, r, false)
}

func (h *Handler) setPublished(w http.ResponseWriter, r *http.Request, on bool) {
	ctx := r.Context()
	owner, err := h.editorialOwner(ctx)
	if err != nil {
		reply(w, err)
		return
	}
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		reply(w, err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	id := param(r, "id")
	name, err := h.lockEditorial(ctx, tx, id, owner)
	if err != nil {
		reply(w, err)
		return
	}
	action := "playlist.unpublish"
	if on {
		action = "playlist.publish"
		var playable int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM playlist_items i JOIN tracks t ON t.id = i.track_id WHERE i.playlist_id = $1 AND t.visibility = 'public'`, id).Scan(&playable); err != nil {
			reply(w, err)
			return
		}
		if playable == 0 {
			reply(w, fail4(http.StatusUnprocessableEntity, "no_public_track", "add at least one track that listeners can play before publishing"))
			return
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE playlists SET is_public = $2, is_featured = $2, hidden_by_ban = false,
		featured_at = CASE WHEN $2 THEN COALESCE(featured_at, now()) ELSE NULL END, updated_at = now() WHERE id = $1`, id, on); err != nil {
		reply(w, err)
		return
	}
	if err := h.log(ctx, tx, r, audit.Entry{Action: action, TargetType: "playlist", TargetID: id, TargetLabel: name}); err != nil {
		reply(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		reply(w, err)
		return
	}
	h.editorialDetail(w, r, owner)
}

// DELETE /v1/admin/playlists/{id} -> 204
func (h *Handler) deleteEditorial(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner, err := h.editorialOwner(ctx)
	if err != nil {
		reply(w, err)
		return
	}
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		reply(w, err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	id := param(r, "id")
	name, err := h.lockEditorial(ctx, tx, id, owner)
	if err != nil {
		reply(w, err)
		return
	}
	var items, saves int
	if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM playlist_items WHERE playlist_id = $1), (SELECT count(*) FROM library_playlists WHERE playlist_id = $1)`, id).Scan(&items, &saves); err != nil {
		reply(w, err)
		return
	}
	if _, err := tx.Exec(ctx, `DELETE FROM playlists WHERE id = $1`, id); err != nil {
		reply(w, err)
		return
	}
	if err := h.log(ctx, tx, r, audit.Entry{Action: "playlist.delete", TargetType: "playlist", TargetID: id, TargetLabel: name,
		Details: map[string]any{"items": items, "saves": saves}}); err != nil {
		reply(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		reply(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
