package youtube

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotConnected = errors.New("youtube: not connected")
	ErrStateInvalid = errors.New("youtube: unknown or expired state")
)

// Connection is a user's linked channel.
type Connection struct {
	UserID       string
	ChannelID    string
	ChannelTitle string
	ThumbnailURL string
	Status       string // active | needs_reconnect
	ConnectedAt  time.Time
	LastSyncedAt *time.Time
	RefreshEnc   []byte
}

// Store is the database side of the connection and the video cache.
type Store struct{ pool *pgxpool.Pool }

// NewStore creates the store.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const stateTTL = 10 * time.Minute

// SaveState records a started authorisation (single use, ten minutes).
func (s *Store) SaveState(ctx context.Context, userID, state, verifier string) error {
	_, _ = s.pool.Exec(ctx, `DELETE FROM youtube_oauth_states WHERE expires_at < now()`)
	_, err := s.pool.Exec(ctx,
		`INSERT INTO youtube_oauth_states (state, user_id, verifier, expires_at) VALUES ($1, $2, $3, now() + make_interval(secs => $4))`,
		state, userID, verifier, stateTTL.Seconds())
	return err
}

// TakeState consumes a state: it can be used once, and only before it expires.
func (s *Store) TakeState(ctx context.Context, state string) (userID, verifier string, err error) {
	var expires time.Time
	err = s.pool.QueryRow(ctx,
		`DELETE FROM youtube_oauth_states WHERE state = $1 RETURNING user_id::text, verifier, expires_at`, state).
		Scan(&userID, &verifier, &expires)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && time.Now().After(expires)) {
		return "", "", ErrStateInvalid
	}
	return userID, verifier, err
}

// Upsert stores (or replaces) the user's connection.
func (s *Store) Upsert(ctx context.Context, userID string, ch *Channel, refreshEnc []byte, scopes string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO youtube_connections (user_id, channel_id, channel_title, thumbnail_url, refresh_token_enc, scopes)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (user_id) DO UPDATE SET channel_id = $2, channel_title = $3, thumbnail_url = $4,
			refresh_token_enc = $5, scopes = $6, status = 'active', connected_at = now(), last_synced_at = NULL`,
		userID, ch.ID, ch.Title, ch.ThumbnailURL, refreshEnc, scopes)
	if err != nil {
		return err
	}
	// A different channel means the cached videos are not the new channel's.
	_, err = s.pool.Exec(ctx, `DELETE FROM youtube_videos WHERE user_id = $1`, userID)
	return err
}

// Get returns the user's connection.
func (s *Store) Get(ctx context.Context, userID string) (*Connection, error) {
	var c Connection
	err := s.pool.QueryRow(ctx, `
		SELECT user_id::text, channel_id, channel_title, thumbnail_url, status, connected_at, last_synced_at, refresh_token_enc
		FROM youtube_connections WHERE user_id = $1`, userID).
		Scan(&c.UserID, &c.ChannelID, &c.ChannelTitle, &c.ThumbnailURL, &c.Status, &c.ConnectedAt, &c.LastSyncedAt, &c.RefreshEnc)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotConnected
	}
	return &c, err
}

// Delete removes the connection and every cached video.
func (s *Store) Delete(ctx context.Context, userID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `DELETE FROM youtube_videos WHERE user_id = $1`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM youtube_connections WHERE user_id = $1`, userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SetStatus marks the connection active or needs_reconnect.
func (s *Store) SetStatus(ctx context.Context, userID, status string) error {
	_, err := s.pool.Exec(ctx, `UPDATE youtube_connections SET status = $2 WHERE user_id = $1`, userID, status)
	return err
}

// ReplaceVideos swaps the cached videos for a fresh list.
func (s *Store) ReplaceVideos(ctx context.Context, userID string, videos []Video) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `DELETE FROM youtube_videos WHERE user_id = $1`, userID); err != nil {
		return err
	}
	for _, v := range videos {
		if _, err := tx.Exec(ctx, `
			INSERT INTO youtube_videos (user_id, video_id, title, description, published_at, duration_sec, thumbnail_url)
			VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT DO NOTHING`,
			userID, v.ID, v.Title, v.Description, v.PublishedAt, v.DurationSec, v.ThumbnailURL); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE youtube_connections SET last_synced_at = now() WHERE user_id = $1`, userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// PurgeStale deletes cached videos older than maxAge (Google's API terms say API data must be
// refreshed or deleted within 30 days) and makes the next listing read the channel again.
func (s *Store) PurgeStale(ctx context.Context, maxAge time.Duration) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		WITH gone AS (DELETE FROM youtube_videos WHERE fetched_at < now() - make_interval(secs => $1) RETURNING user_id)
		UPDATE youtube_connections SET last_synced_at = NULL WHERE user_id IN (SELECT user_id FROM gone)`, maxAge.Seconds())
	return int(tag.RowsAffected()), err
}

// Videos returns the cached videos, newest first.
func (s *Store) Videos(ctx context.Context, userID string) ([]Video, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT video_id, title, description, published_at, duration_sec, thumbnail_url
		FROM youtube_videos WHERE user_id = $1 ORDER BY published_at DESC, video_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Video
	for rows.Next() {
		var v Video
		if err := rows.Scan(&v.ID, &v.Title, &v.Description, &v.PublishedAt, &v.DurationSec, &v.ThumbnailURL); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// OwnedTitles maps normalised song titles to the title, for every track of the
// user's artist profile (used to flag songs that are already in the catalog).
func (s *Store) OwnedTitles(ctx context.Context, userID string, norm func(string) string) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.title FROM tracks t JOIN artists a ON a.id = t.artist_id WHERE a.owner_user_id = $1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var title string
		if err := rows.Scan(&title); err != nil {
			return nil, err
		}
		out[norm(title)] = title
	}
	return out, rows.Err()
}
