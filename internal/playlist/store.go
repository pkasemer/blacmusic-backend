// Package playlist implements user playlists with ordered, re-orderable items.
package playlist

import (
	"context"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/source"
)

const (
	MaxPlaylistsPerUser = 200
	MaxItemsPerPlaylist = 5000
)

var (
	ErrNotFound      = errors.New("playlist not found")
	ErrItemNotFound  = errors.New("playlist item not found")
	ErrTrackNotFound = errors.New("track not found")
	ErrLimit         = errors.New("limit reached")
)

// ValidationError is returned for bad input.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// Playlist is the summary shape.
type Playlist struct {
	ID          string    `json:"id"`
	OwnerID     string    `json:"ownerId"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	IsPublic    bool      `json:"isPublic"`
	TrackCount  int       `json:"trackCount"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// Item is one entry in a playlist.
type Item struct {
	ID       string       `json:"id"`
	Position int          `json:"position"`
	AddedAt  time.Time    `json:"addedAt"`
	Track    source.Track `json:"track"`
}

// Detail is a playlist with a page of its items.
type Detail struct {
	Playlist
	Items []Item `json:"items"`
}

// Patch holds optional fields for Update.
type Patch struct {
	Name        *string
	Description *string
	IsPublic    *bool
}

// Store holds playlist persistence and rules.
type Store struct{ pool *pgxpool.Pool }

// NewStore creates a Store.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const playlistSelect = `
	SELECT p.id, p.owner_id::text, p.name, p.description, p.is_public, p.created_at, p.updated_at,
	       (SELECT count(*) FROM playlist_items i WHERE i.playlist_id = p.id)
	FROM playlists p`

func scanPlaylist(row pgx.Row) (*Playlist, error) {
	var p Playlist
	err := row.Scan(&p.ID, &p.OwnerID, &p.Name, &p.Description, &p.IsPublic, &p.CreatedAt, &p.UpdatedAt, &p.TrackCount)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func validateName(name string) error {
	if n := utf8.RuneCountInString(name); n < 1 || n > 100 {
		return &ValidationError{"name must be 1-100 characters"}
	}
	return nil
}

func validateDescription(d string) error {
	if utf8.RuneCountInString(d) > 500 {
		return &ValidationError{"description must be at most 500 characters"}
	}
	return nil
}

func (s *Store) Create(ctx context.Context, owner, name, description string, isPublic bool) (*Playlist, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	if err := validateDescription(description); err != nil {
		return nil, err
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM playlists WHERE owner_id = $1`, owner).Scan(&count); err != nil {
		return nil, err
	}
	if count >= MaxPlaylistsPerUser {
		return nil, ErrLimit
	}
	var id string
	err := s.pool.QueryRow(ctx,
		`INSERT INTO playlists(owner_id, name, description, is_public) VALUES ($1,$2,$3,$4) RETURNING id`,
		owner, name, description, isPublic).Scan(&id)
	if err != nil {
		return nil, err
	}
	return scanPlaylist(s.pool.QueryRow(ctx, playlistSelect+` WHERE p.id = $1`, id))
}

// ListByOwner returns the owner's playlists, most recently changed first.
func (s *Store) ListByOwner(ctx context.Context, owner string, limit, offset int) ([]Playlist, error) {
	rows, err := s.pool.Query(ctx,
		playlistSelect+` WHERE p.owner_id = $1 ORDER BY p.updated_at DESC, p.id LIMIT $2 OFFSET $3`, owner, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Playlist{}
	for rows.Next() {
		p, err := scanPlaylist(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// Get returns a playlist and a page of items. Private playlists are reported
// as not found to anyone but the owner.
func (s *Store) Get(ctx context.Context, id, viewer string, limit, offset int) (*Detail, error) {
	p, err := scanPlaylist(s.pool.QueryRow(ctx, playlistSelect+` WHERE p.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !p.IsPublic && p.OwnerID != viewer {
		return nil, ErrNotFound
	}

	rows, err := s.pool.Query(ctx, itemSelect+`
		WHERE i.playlist_id = $1 ORDER BY i.position LIMIT $2 OFFSET $3`, id, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Item{}
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &Detail{Playlist: *p, Items: items}, nil
}

func (s *Store) Update(ctx context.Context, id, owner string, patch Patch) (*Playlist, error) {
	if patch.Name != nil {
		if err := validateName(*patch.Name); err != nil {
			return nil, err
		}
	}
	if patch.Description != nil {
		if err := validateDescription(*patch.Description); err != nil {
			return nil, err
		}
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE playlists SET
			name        = COALESCE($3, name),
			description = COALESCE($4, description),
			is_public   = COALESCE($5, is_public),
			updated_at  = now()
		WHERE id = $1 AND owner_id = $2`, id, owner, patch.Name, patch.Description, patch.IsPublic)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	return scanPlaylist(s.pool.QueryRow(ctx, playlistSelect+` WHERE p.id = $1`, id))
}

func (s *Store) Delete(ctx context.Context, id, owner string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM playlists WHERE id = $1 AND owner_id = $2`, id, owner)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

const itemSelect = `
	SELECT i.id::text, i.position, i.added_at,
	       t.id, t.title, COALESCE(a.name, ''), COALESCE(al.title, ''),
	       t.duration_ms, COALESCE(t.artwork_url, al.artwork_url, '')
	FROM playlist_items i
	JOIN tracks t        ON t.id  = i.track_id
	LEFT JOIN artists a  ON a.id  = t.artist_id
	LEFT JOIN albums  al ON al.id = t.album_id`

func scanItem(row pgx.Row) (*Item, error) {
	var it Item
	err := row.Scan(&it.ID, &it.Position, &it.AddedAt,
		&it.Track.ID, &it.Track.Title, &it.Track.ArtistName, &it.Track.AlbumTitle, &it.Track.DurationMs, &it.Track.ArtworkURL)
	if err != nil {
		return nil, err
	}
	return &it, nil
}

// lockOwned locks the playlist row for the duration of tx and verifies ownership.
func lockOwned(ctx context.Context, tx pgx.Tx, id, owner string) error {
	var ownerID string
	err := tx.QueryRow(ctx, `SELECT owner_id::text FROM playlists WHERE id = $1 FOR UPDATE`, id).Scan(&ownerID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && ownerID != owner) {
		return ErrNotFound
	}
	return err
}

func touch(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, `UPDATE playlists SET updated_at = now() WHERE id = $1`, id)
	return err
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// AddItem inserts a track at position (nil = append) and returns the new item.
func (s *Store) AddItem(ctx context.Context, id, owner, trackID string, position *int) (*Item, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err := lockOwned(ctx, tx, id, owner); err != nil {
		return nil, err
	}
	// Only public tracks can be added (an artist may add their own drafts).
	var addable bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM tracks t LEFT JOIN artists a ON a.id = t.artist_id
		               WHERE t.id = $1 AND (t.visibility = 'public' OR a.owner_user_id::text = $2))`,
		trackID, owner).Scan(&addable); err != nil {
		return nil, err
	}
	if !addable {
		return nil, ErrTrackNotFound
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM playlist_items WHERE playlist_id = $1`, id).Scan(&count); err != nil {
		return nil, err
	}
	if count >= MaxItemsPerPlaylist {
		return nil, ErrLimit
	}
	pos := count
	if position != nil {
		pos = clamp(*position, 0, count)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE playlist_items SET position = position + 1 WHERE playlist_id = $1 AND position >= $2`, id, pos); err != nil {
		return nil, err
	}
	var itemID string
	err = tx.QueryRow(ctx,
		`INSERT INTO playlist_items(playlist_id, track_id, position, added_by) VALUES ($1,$2,$3,$4) RETURNING id::text`,
		id, trackID, pos, owner).Scan(&itemID)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		return nil, ErrTrackNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := touch(ctx, tx, id); err != nil {
		return nil, err
	}
	it, err := scanItem(tx.QueryRow(ctx, itemSelect+` WHERE i.id = $1`, itemID))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return it, nil
}

// RemoveItem deletes an item and closes the gap.
func (s *Store) RemoveItem(ctx context.Context, id, owner, itemID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err := lockOwned(ctx, tx, id, owner); err != nil {
		return err
	}
	var pos int
	err = tx.QueryRow(ctx,
		`DELETE FROM playlist_items WHERE id = $1 AND playlist_id = $2 RETURNING position`, itemID, id).Scan(&pos)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrItemNotFound
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE playlist_items SET position = position - 1 WHERE playlist_id = $1 AND position > $2`, id, pos); err != nil {
		return err
	}
	if err := touch(ctx, tx, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// MoveItem moves an item to newPos (clamped into range), shifting the items between.
func (s *Store) MoveItem(ctx context.Context, id, owner, itemID string, newPos int) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err := lockOwned(ctx, tx, id, owner); err != nil {
		return err
	}
	var oldPos, count int
	err = tx.QueryRow(ctx,
		`SELECT position FROM playlist_items WHERE id = $1 AND playlist_id = $2`, itemID, id).Scan(&oldPos)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrItemNotFound
	}
	if err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM playlist_items WHERE playlist_id = $1`, id).Scan(&count); err != nil {
		return err
	}
	newPos = clamp(newPos, 0, count-1)
	if newPos == oldPos {
		return tx.Commit(ctx)
	}
	if newPos > oldPos {
		_, err = tx.Exec(ctx,
			`UPDATE playlist_items SET position = position - 1 WHERE playlist_id = $1 AND position > $2 AND position <= $3`,
			id, oldPos, newPos)
	} else {
		_, err = tx.Exec(ctx,
			`UPDATE playlist_items SET position = position + 1 WHERE playlist_id = $1 AND position >= $2 AND position < $3`,
			id, newPos, oldPos)
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE playlist_items SET position = $2 WHERE id = $1`, itemID, newPos); err != nil {
		return err
	}
	if err := touch(ctx, tx, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
