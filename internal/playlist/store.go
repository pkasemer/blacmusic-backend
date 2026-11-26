// Package playlist implements user playlists with ordered, re-orderable items,
// and the discovery of public playlists (browse, featuring, saving).
package playlist

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/image"
	"github.com/blacmusic/backend/internal/source"
	"github.com/blacmusic/backend/internal/trackview"
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
	ErrForbidden     = errors.New("not allowed")
)

// ValidationError is returned for bad input.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// Owner is who made a playlist.
type Owner struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	AvatarURL   string `json:"avatarUrl,omitempty"`
}

// Playlist is the summary shape.
type Playlist struct {
	ID      string `json:"id"`
	OwnerID string `json:"ownerId"`
	Owner   Owner  `json:"owner"`
	Name    string `json:"name"`
	// Description is empty for most user playlists.
	Description string `json:"description"`
	IsPublic    bool   `json:"isPublic"`
	// IsFeatured marks playlists the Blac team curates (only public ones).
	IsFeatured bool `json:"isFeatured"`
	// ArtworkURL is the custom cover; absent when none was set.
	ArtworkURL string `json:"artworkUrl,omitempty"`
	// ArtworkLargeURL is the custom cover at about 1280 px; absent when none was set.
	ArtworkLargeURL string `json:"artworkLargeUrl,omitempty"`
	// TotalDurationSec is the length of the tracks the viewer can play, together, in seconds.
	TotalDurationSec int `json:"totalDurationSec"`
	// ArtworkURLs are up to four covers taken from the first tracks that have
	// artwork, for a mosaic when there is no custom cover.
	ArtworkURLs []string `json:"artworkUrls"`
	// TrackCount counts the tracks the viewer can play: others only see the
	// public ones, the owner sees all of them.
	TrackCount int `json:"trackCount"`
	// SaveCount is how many users saved the playlist to their library; Saved
	// says whether the viewer did.
	SaveCount int  `json:"saveCount"`
	Saved     bool `json:"saved"`
	// ContainsTrack is only present when the list was asked for with ?trackId=:
	// whether that track is already in the playlist.
	ContainsTrack *bool     `json:"containsTrack,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
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
	// IsFeatured may only be set by an administrator.
	IsFeatured *bool
	// CoverImageID is one of the caller's finished "cover" images; "" removes the cover.
	CoverImageID *string
}

// Store holds playlist persistence and rules.
type Store struct {
	pool      *pgxpool.Pool
	publicURL string
}

// NewStore creates a Store. publicURL is the API's public base URL, used to
// build picture links.
func NewStore(pool *pgxpool.Pool, publicURL string) *Store {
	return &Store{pool: pool, publicURL: publicURL}
}

// playlistSelect reads a playlist as seen by a viewer, who is always $1: the
// owner sees every track, anyone else only the public ones.
const playlistSelect = `
	SELECT p.id, p.owner_id::text, u.display_name, u.avatar_image_id,
	       p.name, p.description, p.is_public, p.is_featured, p.cover_image_id,
	       ARRAY(SELECT s.art FROM (
	           SELECT COALESCE(NULLIF(t.artwork_url, ''), NULLIF(al.artwork_url, '')) AS art, min(i.position) AS pos
	           FROM playlist_items i
	           JOIN tracks t        ON t.id  = i.track_id
	           LEFT JOIN albums al  ON al.id = t.album_id
	           WHERE i.playlist_id = p.id AND (t.visibility = 'public' OR p.owner_id = $1)
	             AND COALESCE(NULLIF(t.artwork_url, ''), NULLIF(al.artwork_url, '')) IS NOT NULL
	           GROUP BY 1 ORDER BY 2 LIMIT 4) s ORDER BY s.pos),
	       (SELECT count(*) FROM playlist_items i JOIN tracks t ON t.id = i.track_id
	         WHERE i.playlist_id = p.id AND (t.visibility = 'public' OR p.owner_id = $1)),
	       (SELECT count(*) FROM library_playlists l WHERE l.playlist_id = p.id),
	       EXISTS (SELECT 1 FROM library_playlists l WHERE l.playlist_id = p.id AND l.user_id = $1),
	       p.created_at, p.updated_at,
	       (SELECT COALESCE(sum(t.duration_ms), 0) / 1000 FROM playlist_items i JOIN tracks t ON t.id = i.track_id
	         WHERE i.playlist_id = p.id AND (t.visibility = 'public' OR p.owner_id = $1))
	FROM playlists p
	JOIN users u ON u.id = p.owner_id`

func (s *Store) scanPlaylist(row pgx.Row) (*Playlist, error) {
	var p Playlist
	var avatarID, coverID *string
	err := row.Scan(&p.ID, &p.OwnerID, &p.Owner.DisplayName, &avatarID,
		&p.Name, &p.Description, &p.IsPublic, &p.IsFeatured, &coverID,
		&p.ArtworkURLs, &p.TrackCount, &p.SaveCount, &p.Saved, &p.CreatedAt, &p.UpdatedAt, &p.TotalDurationSec)
	if err != nil {
		return nil, err
	}
	p.Owner.ID = p.OwnerID
	p.Owner.AvatarURL = image.PersonURL(s.publicURL, avatarID, "")
	if coverID != nil && *coverID != "" {
		p.ArtworkURL = image.URL(s.publicURL, *coverID, image.SizeMedium)
		p.ArtworkLargeURL = image.URL(s.publicURL, *coverID, image.SizeLarge)
	}
	if p.ArtworkURLs == nil {
		p.ArtworkURLs = []string{}
	}
	return &p, nil
}

func (s *Store) scanPlaylists(rows pgx.Rows) ([]Playlist, error) {
	defer rows.Close()
	out := []Playlist{}
	for rows.Next() {
		p, err := s.scanPlaylist(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
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
	return s.scanPlaylist(s.pool.QueryRow(ctx, playlistSelect+` WHERE p.id = $2`, owner, id))
}

// ListByOwner returns the owner's playlists, most recently changed first.
func (s *Store) ListByOwner(ctx context.Context, owner string, limit, offset int) ([]Playlist, error) {
	rows, err := s.pool.Query(ctx,
		playlistSelect+` WHERE p.owner_id = $1 ORDER BY p.updated_at DESC, p.id LIMIT $2 OFFSET $3`, owner, limit, offset)
	if err != nil {
		return nil, err
	}
	return s.scanPlaylists(rows)
}

// MarkContaining sets ContainsTrack on every playlist in ps.
func (s *Store) MarkContaining(ctx context.Context, ps []Playlist, trackID string) error {
	ids := make([]string, len(ps))
	for i, p := range ps {
		ids[i] = p.ID
	}
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT playlist_id FROM playlist_items WHERE track_id = $1 AND playlist_id = ANY($2)`, trackID, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	has := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		has[id] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range ps {
		v := has[ps[i].ID]
		ps[i].ContainsTrack = &v
	}
	return nil
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// Search finds public playlists with at least one public track whose name,
// description or owner name contains q (case-insensitive). Private playlists
// are never returned, not even the viewer's own. The team's featured playlists
// come first, then the better name matches (exact, then starting with q, then
// containing q, then matches only in the description or owner), then the most
// saved, then the newest.
func (s *Store) Search(ctx context.Context, viewer, q string, limit, offset int) ([]Playlist, error) {
	contains := "%" + escapeLike(q) + "%"
	prefix := escapeLike(q) + "%"
	rows, err := s.pool.Query(ctx, playlistSelect+`
		WHERE p.is_public
		  AND (p.name ILIKE $2 OR p.description ILIKE $2 OR u.display_name ILIKE $2)
		  AND EXISTS (SELECT 1 FROM playlist_items i JOIN tracks t ON t.id = i.track_id
		              WHERE i.playlist_id = p.id AND t.visibility = 'public')
		ORDER BY p.is_featured DESC,
		         CASE WHEN lower(p.name) = lower($4) THEN 0
		              WHEN p.name ILIKE $3 THEN 1
		              WHEN p.name ILIKE $2 THEN 2 ELSE 3 END,
		         `+saveCountExpr+` DESC, p.updated_at DESC, p.id
		LIMIT $5 OFFSET $6`, viewer, contains, prefix, q, limit, offset)
	if err != nil {
		return nil, err
	}
	return s.scanPlaylists(rows)
}

// Browse sort orders.
const (
	SortFeatured = "featured"
	SortNew      = "new"
	SortPopular  = "popular"
)

const saveCountExpr = `(SELECT count(*) FROM library_playlists l WHERE l.playlist_id = p.id)`

// Browse lists public playlists that have at least one public track.
//
//	featured: the team's picks first, then most saved, then newest
//	new:      most recently created or changed
//	popular:  most saved, then newest
func (s *Store) Browse(ctx context.Context, viewer, sort string, limit, offset int) ([]Playlist, error) {
	var order string
	switch sort {
	case "", SortFeatured:
		order = `p.is_featured DESC, ` + saveCountExpr + ` DESC, p.updated_at DESC, p.id`
	case SortNew:
		order = `p.updated_at DESC, p.id`
	case SortPopular:
		order = saveCountExpr + ` DESC, p.updated_at DESC, p.id`
	default:
		return nil, &ValidationError{"sort must be featured, new or popular"}
	}
	rows, err := s.pool.Query(ctx, playlistSelect+`
		WHERE p.is_public
		  AND EXISTS (SELECT 1 FROM playlist_items i JOIN tracks t ON t.id = i.track_id
		              WHERE i.playlist_id = p.id AND t.visibility = 'public')
		ORDER BY `+order+` LIMIT $2 OFFSET $3`, viewer, limit, offset)
	if err != nil {
		return nil, err
	}
	return s.scanPlaylists(rows)
}

// ListSaved returns the public playlists the viewer saved, newest save first.
func (s *Store) ListSaved(ctx context.Context, viewer string, limit, offset int) ([]Playlist, error) {
	rows, err := s.pool.Query(ctx, playlistSelect+`
		JOIN library_playlists mine ON mine.playlist_id = p.id AND mine.user_id = $1
		WHERE p.is_public
		ORDER BY mine.added_at DESC, p.id LIMIT $2 OFFSET $3`, viewer, limit, offset)
	if err != nil {
		return nil, err
	}
	return s.scanPlaylists(rows)
}

// Get returns a playlist and a page of items. Private playlists are reported
// as not found to anyone but the owner, who also is the only one to see tracks
// that are not public yet.
func (s *Store) Get(ctx context.Context, id, viewer string, limit, offset int) (*Detail, error) {
	p, err := s.scanPlaylist(s.pool.QueryRow(ctx, playlistSelect+` WHERE p.id = $2`, viewer, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !p.IsPublic && p.OwnerID != viewer {
		return nil, ErrNotFound
	}

	q := itemSelect + ` WHERE i.playlist_id = $1`
	if p.OwnerID != viewer {
		q += ` AND t.visibility = 'public'`
	}
	rows, err := s.pool.Query(ctx, q+` ORDER BY i.position LIMIT $2 OFFSET $3`, id, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Item{}
	for rows.Next() {
		it, err := s.scanItem(rows)
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

// Update changes a playlist. The owner may change everything but featuring.
// An administrator may feature a public playlist and edit the name,
// description and cover of any public playlist, so editorial playlists can be
// maintained; nobody else's visibility is theirs to change.
func (s *Store) Update(ctx context.Context, id, actor string, patch Patch) (*Playlist, error) {
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

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var owner string
	var isPublic, isFeatured bool
	err = tx.QueryRow(ctx, `SELECT owner_id::text, is_public, is_featured FROM playlists WHERE id = $1 FOR UPDATE`, id).
		Scan(&owner, &isPublic, &isFeatured)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	isOwner := owner == actor
	isAdmin := false
	if !isOwner || patch.IsFeatured != nil {
		if err := tx.QueryRow(ctx, `SELECT role = 'admin' FROM users WHERE id = $1`, actor).Scan(&isAdmin); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}
	if !isOwner && (!isAdmin || !isPublic) {
		return nil, ErrNotFound // the same answer as for a playlist that does not exist
	}
	if patch.IsFeatured != nil && !isAdmin {
		return nil, ErrForbidden
	}
	if !isOwner && patch.IsPublic != nil {
		return nil, ErrForbidden
	}

	newPublic, newFeatured := isPublic, isFeatured
	if patch.IsPublic != nil {
		newPublic = *patch.IsPublic
	}
	if patch.IsFeatured != nil {
		newFeatured = *patch.IsFeatured
	}
	if newFeatured && !newPublic {
		if patch.IsFeatured != nil {
			return nil, &ValidationError{"only public playlists can be featured"}
		}
		newFeatured = false // made private: no longer featured
	}

	setCover := patch.CoverImageID != nil
	var cover *string
	if setCover && *patch.CoverImageID != "" {
		if err := image.CheckOwned(ctx, tx, *patch.CoverImageID, actor, "cover"); err != nil {
			return nil, &ValidationError{"coverImageId must be one of your finished cover images"}
		}
		cover = patch.CoverImageID
	}

	if _, err := tx.Exec(ctx, `
		UPDATE playlists SET
			name          = COALESCE($2, name),
			description   = COALESCE($3, description),
			is_public     = $4,
			is_featured   = $5,
			featured_at   = CASE WHEN NOT $5 THEN NULL WHEN NOT is_featured THEN now() ELSE featured_at END,
			cover_image_id = CASE WHEN $6 THEN $7 ELSE cover_image_id END,
			updated_at    = now()
		WHERE id = $1`, id, patch.Name, patch.Description, newPublic, newFeatured, setCover, cover); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.scanPlaylist(s.pool.QueryRow(ctx, playlistSelect+` WHERE p.id = $2`, actor, id))
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

// itemSelect reads playlist items with the same track shape as search and
// browse. $1 is the playlist for the WHERE the caller appends.
var itemSelect = trackview.Select(`i.id::text, i.position, i.added_at`) + `
	JOIN playlist_items i ON i.track_id = t.id`

func (s *Store) scanItem(row pgx.Row) (*Item, error) {
	var it Item
	t, err := trackview.Scan(row, s.publicURL, &it.ID, &it.Position, &it.AddedAt)
	if err != nil {
		return nil, err
	}
	it.Track = t
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
	it, err := s.scanItem(tx.QueryRow(ctx, itemSelect+` WHERE i.id = $1`, itemID))
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
