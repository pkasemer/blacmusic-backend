package admin

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/blacmusic/backend/internal/audit"
	"github.com/blacmusic/backend/internal/httpx"
)

// Keeping a song, or all of an artist's songs, out of what is suggested: mixes, autoplay and "Your sound". Nothing
// else changes: they play, can be searched for, and stay in everyone's own playlists.

type recBlock struct {
	ID         int64     `json:"id"`
	Kind       string    `json:"kind"` // track or artist
	TargetID   string    `json:"targetId"`
	Name       string    `json:"name"`       // the song's title or the artist's name
	ArtistName string    `json:"artistName"` // of a song
	Songs      int       `json:"songs"`      // how many songs it keeps out now
	Reason     string    `json:"reason"`
	BlockedBy  *person   `json:"blockedBy"`
	BlockedAt  time.Time `json:"blockedAt"`
}

// GET /v1/admin/recommendations/blocks, newest first.
func (h *Handler) listBlocks(w http.ResponseWriter, r *http.Request) {
	rows, err := h.pool.Query(r.Context(), `
		SELECT b.id, CASE WHEN b.track_id IS NULL THEN 'artist' ELSE 'track' END, COALESCE(b.track_id, b.artist_id),
		       COALESCE(t.title, a.name, ''), COALESCE(ta.name, ''),
		       CASE WHEN b.track_id IS NULL THEN (SELECT count(*) FROM tracks x WHERE x.artist_id = b.artist_id AND x.visibility = 'public') ELSE 1 END,
		       b.reason, u.id::text, u.email, b.blocked_at
		FROM rec_blocks b
		LEFT JOIN tracks t ON t.id = b.track_id
		LEFT JOIN artists ta ON ta.id = t.artist_id
		LEFT JOIN artists a ON a.id = b.artist_id
		LEFT JOIN users u ON u.id = b.blocked_by
		ORDER BY b.blocked_at DESC, b.id DESC`)
	if err != nil {
		reply(w, err)
		return
	}
	defer rows.Close()
	out := []recBlock{}
	for rows.Next() {
		var b recBlock
		var by, email *string
		if err := rows.Scan(&b.ID, &b.Kind, &b.TargetID, &b.Name, &b.ArtistName, &b.Songs, &b.Reason, &by, &email, &b.BlockedAt); err != nil {
			reply(w, err)
			return
		}
		if by != nil && email != nil {
			b.BlockedBy = &person{ID: *by, Email: *email}
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		reply(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"blocks": out})
}

// POST /v1/admin/recommendations/blocks {trackId | artistId, reason} -> 201 {id, removedFromPlaylists}
func (h *Handler) addBlock(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TrackID  string `json:"trackId"`
		ArtistID string `json:"artistId"`
		Reason   string `json:"reason"`
	}
	if err := decodeBody(w, r, &in, false); err != nil {
		reply(w, err)
		return
	}
	if (in.TrackID == "") == (in.ArtistID == "") {
		reply(w, invalid("send trackId or artistId, one of them"))
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
	kind, target, find := "track", in.TrackID, `SELECT title FROM tracks WHERE id = $1 FOR SHARE`
	if in.ArtistID != "" {
		kind, target, find = "artist", in.ArtistID, `SELECT name FROM artists WHERE id = $1 FOR SHARE`
	}
	var label string
	if err := tx.QueryRow(ctx, find, target).Scan(&label); errors.Is(err, pgx.ErrNoRows) {
		reply(w, errNotFound)
		return
	} else if err != nil {
		reply(w, err)
		return
	}
	var id int64
	err = tx.QueryRow(ctx, `INSERT INTO rec_blocks (track_id, artist_id, reason, blocked_by)
		VALUES (NULLIF($1, ''), NULLIF($2, ''), $3, $4::uuid) RETURNING id`, in.TrackID, in.ArtistID, why, actorID(r)).Scan(&id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		reply(w, fail4(http.StatusConflict, "already_blocked", "it is already blocked"))
		return
	}
	if err != nil {
		reply(w, err)
		return
	}
	removed, err := takeOutOfYourSound(ctx, tx, kind, target)
	if err != nil {
		reply(w, err)
		return
	}
	if err := h.log(ctx, tx, r, audit.Entry{Action: "recommendation.block", TargetType: kind, TargetID: target, TargetLabel: label, Reason: why,
		Details: map[string]any{"removedFromPlaylists": removed}}); err != nil {
		reply(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		reply(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"id": id, "removedFromPlaylists": removed})
}

// takeOutOfYourSound removes what the server itself had put in listeners' "Your sound" of what has just been blocked.
// Songs listeners added themselves are left alone. What is removed is forgotten as offered, so that after an unblock
// it can be suggested again.
func takeOutOfYourSound(ctx context.Context, tx pgx.Tx, kind, target string) (int, error) {
	match := `i.track_id = $1`
	if kind == "artist" {
		match = `i.track_id IN (SELECT id FROM tracks WHERE artist_id = $1)`
	}
	// the playlists are locked in one order, the way a refresh or the listener's own edit locks them
	rows, err := tx.Query(ctx, `SELECT p.id FROM playlists p
		WHERE p.id IN (SELECT y.playlist_id FROM your_sound y JOIN playlist_items i ON i.playlist_id = y.playlist_id
		               WHERE i.by_server AND `+match+`)
		ORDER BY p.id FOR UPDATE`, target)
	if err != nil {
		return 0, err
	}
	playlists, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(playlists) == 0 {
		return 0, err
	}
	var removed int
	if err := tx.QueryRow(ctx, `
		WITH gone AS (
			DELETE FROM playlist_items i WHERE i.playlist_id = ANY($2) AND i.by_server AND `+match+`
			RETURNING i.playlist_id, i.track_id),
		forgotten AS (
			DELETE FROM your_sound_offers o USING gone g, your_sound y
			WHERE y.playlist_id = g.playlist_id AND o.user_id = y.user_id AND o.track_id = g.track_id)
		SELECT count(*) FROM gone`, target, playlists).Scan(&removed); err != nil {
		return 0, err
	}
	// positions run 0..n-1 without gaps
	if _, err := tx.Exec(ctx, `UPDATE playlist_items i SET position = n.position FROM (
			SELECT id, row_number() OVER (PARTITION BY playlist_id ORDER BY position, added_at, id) - 1 AS position
			FROM playlist_items WHERE playlist_id = ANY($1)) n
		WHERE i.id = n.id AND i.position <> n.position`, playlists); err != nil {
		return 0, err
	}
	_, err = tx.Exec(ctx, `UPDATE playlists SET updated_at = now() WHERE id = ANY($1)`, playlists)
	return removed, err
}

// DELETE /v1/admin/recommendations/blocks/{id} -> 204. It can be suggested again from the next list.
func (h *Handler) removeBlock(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(param(r, "id"), 10, 64)
	if err != nil {
		reply(w, errNotFound)
		return
	}
	ctx := r.Context()
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		reply(w, err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var kind, target, label, why string
	err = tx.QueryRow(ctx, `
		WITH gone AS (DELETE FROM rec_blocks WHERE id = $1 RETURNING track_id, artist_id, reason)
		SELECT CASE WHEN g.track_id IS NULL THEN 'artist' ELSE 'track' END, COALESCE(g.track_id, g.artist_id), COALESCE(t.title, a.name, ''), g.reason
		FROM gone g LEFT JOIN tracks t ON t.id = g.track_id LEFT JOIN artists a ON a.id = g.artist_id`, id).Scan(&kind, &target, &label, &why)
	if errors.Is(err, pgx.ErrNoRows) {
		reply(w, errNotFound)
		return
	}
	if err != nil {
		reply(w, err)
		return
	}
	if err := h.log(ctx, tx, r, audit.Entry{Action: "recommendation.unblock", TargetType: kind, TargetID: target, TargetLabel: label,
		Details: map[string]any{"blockedFor": why}}); err != nil {
		reply(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		reply(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
