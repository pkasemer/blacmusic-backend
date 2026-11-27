package listening

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/httpx"
	"github.com/blacmusic/backend/internal/recommend"
)

const (
	soundName = "Your sound"
	// soundDescription is also how the playlist an app made by itself is recognised.
	soundDescription = "Made for you from what you love. Blac adds new songs as you listen."

	soundSize     = 40  // what the server fills it to
	soundMax      = 100 // never more, with the listener's own songs
	soundDaily    = 8   // new songs a day once it is filled
	refreshPeriod = `interval '23 hours'`
)

// YourSound is GET /v1/me/your-sound -> {playlistId}. The first time it takes over the playlist the app made, or
// makes one, and fills it; after that the server keeps it up to date about daily.
func (h *Handler) YourSound(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	id, err := Sound(r.Context(), h.pool, uid)
	if err != nil {
		internal(w, "your sound", err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"playlistId": id})
}

// Sound finds, adopts or makes the listener's playlist, refreshing it when it is due, and returns its id.
func Sound(ctx context.Context, pool *pgxpool.Pool, userID string) (string, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// one at a time for each listener, so there is never a second playlist
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('your_sound:' || $1))`, userID); err != nil {
		return "", err
	}
	var id string
	var due bool
	err = tx.QueryRow(ctx, `SELECT playlist_id, refreshed_at IS NULL OR refreshed_at < now() - `+refreshPeriod+`
		FROM your_sound WHERE user_id = $1`, userID).Scan(&id, &due)
	if errors.Is(err, pgx.ErrNoRows) {
		due = true
		err = tx.QueryRow(ctx, `SELECT id FROM playlists WHERE owner_id = $1 AND name = $2 AND description = $3
			ORDER BY updated_at DESC, id LIMIT 1`, userID, soundName, soundDescription).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			err = tx.QueryRow(ctx, `INSERT INTO playlists (owner_id, name, description, is_public) VALUES ($1, $2, $3, false) RETURNING id`,
				userID, soundName, soundDescription).Scan(&id)
		}
		if err != nil {
			return "", err
		}
		_, err = tx.Exec(ctx, `INSERT INTO your_sound (user_id, playlist_id) VALUES ($1, $2)`, userID, id)
	}
	if err != nil {
		return "", err
	}
	if due {
		if err := refresh(ctx, tx, userID, id); err != nil {
			return "", err
		}
	}
	return id, tx.Commit(ctx)
}

// RefreshDue updates up to limit playlists that have not been updated for about a day. The worker calls it.
func RefreshDue(ctx context.Context, pool *pgxpool.Pool, limit int) (int, error) {
	rows, err := pool.Query(ctx, `SELECT user_id::text FROM your_sound
		WHERE refreshed_at IS NULL OR refreshed_at < now() - `+refreshPeriod+` ORDER BY refreshed_at NULLS FIRST LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	users, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	done := 0
	for _, u := range users {
		if _, err := Sound(ctx, pool, u); err != nil {
			if ctx.Err() != nil {
				return done, ctx.Err()
			}
			slog.Warn("your sound not refreshed", "err", err)
			continue
		}
		done++
	}
	return done, nil
}

// refresh drops what the server added that no longer belongs, adds new songs and trims the oldest of its own.
// Songs the listener added are never touched.
func refresh(ctx context.Context, tx pgx.Tx, userID, playlistID string) error {
	var one int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM playlists WHERE id = $1 FOR UPDATE`, playlistID).Scan(&one); err != nil {
		return err
	}
	gone, err := tx.Exec(ctx, `
		DELETE FROM playlist_items i WHERE i.playlist_id = $2 AND i.by_server AND (
			i.track_id IN (`+recommend.Avoided+`) OR i.track_id IN (SELECT track_id FROM rec_blocked_tracks) OR NOT EXISTS (SELECT 1 FROM tracks t WHERE t.id = i.track_id AND t.visibility = 'public'))`,
		userID, playlistID)
	if err != nil {
		return err
	}
	var total, offered int
	if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM playlist_items WHERE playlist_id = $2),
		(SELECT count(*) FROM your_sound_offers WHERE user_id = $1)`, userID, playlistID).Scan(&total, &offered); err != nil {
		return err
	}
	want := soundDaily
	if offered == 0 || total < soundSize/2 { // new, or nearly empty: fill it
		want = max(soundSize-total, soundDaily)
	}
	var picks []string
	var suggested *recommend.Result
	if want = min(want, soundMax); want > 0 {
		var err error
		if suggested, err = recommend.ForSound(ctx, tx, userID, playlistID, want); err != nil {
			return err
		}
		for _, p := range suggested.Picks {
			picks = append(picks, p.TrackID)
		}
	}
	// make room by letting the oldest of the server's songs go
	trimmed, err := tx.Exec(ctx, `
		DELETE FROM playlist_items WHERE id IN (
			SELECT id FROM playlist_items WHERE playlist_id = $1 AND by_server ORDER BY added_at, position
			LIMIT GREATEST((SELECT count(*) FROM playlist_items WHERE playlist_id = $1) + $2 - $3, 0))`,
		playlistID, len(picks), soundSize)
	if err != nil {
		return err
	}
	// close the gaps, then put the new songs at the end, as far as the limit allows
	if _, err := tx.Exec(ctx, `UPDATE playlist_items i SET position = n.position FROM (
			SELECT id, row_number() OVER (ORDER BY position, added_at, id) - 1 AS position FROM playlist_items WHERE playlist_id = $1) n
		WHERE i.id = n.id AND i.position <> n.position`, playlistID); err != nil {
		return err
	}
	added, err := tx.Exec(ctx, `
		WITH have AS (SELECT count(*)::int AS n FROM playlist_items WHERE playlist_id = $1),
		put AS (
			INSERT INTO playlist_items (playlist_id, track_id, position, added_by, by_server)
			SELECT $1, p.id, have.n + p.i::int - 1, $2, true FROM unnest($3::text[]) WITH ORDINALITY AS p(id, i), have
			WHERE have.n + p.i <= $4 RETURNING track_id)
		INSERT INTO your_sound_offers (user_id, track_id) SELECT $2, track_id FROM put
		ON CONFLICT (user_id, track_id) DO UPDATE SET offered_at = now()`, playlistID, userID, picks, soundMax)
	if err != nil {
		return err
	}
	if suggested != nil && added.RowsAffected() > 0 {
		// only what went in
		kept := suggested.Picks[:min(int(added.RowsAffected()), len(suggested.Picks))]
		if err := recommend.Record(ctx, tx, userID, "your_sound", suggested.Mode, "", kept); err != nil {
			return err
		}
	}
	if gone.RowsAffected()+trimmed.RowsAffected()+added.RowsAffected() > 0 {
		if _, err := tx.Exec(ctx, `UPDATE playlists SET updated_at = now() WHERE id = $1`, playlistID); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE your_sound SET refreshed_at = now() WHERE user_id = $1`, userID)
	return err
}
