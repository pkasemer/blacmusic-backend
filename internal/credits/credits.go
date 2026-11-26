// Package credits keeps who worked on a release or one of its tracks: producers, writers, engineers,
// labels and so on, written by the artist.
package credits

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Limits and roles.
const (
	MaxCredits   = 100
	MaxNameRunes = 100
	MaxLabelRune = 40
)

// Roles is every allowed role.
var Roles = map[string]bool{
	"producer": true, "executive_producer": true, "songwriter": true, "composer": true, "lyricist": true,
	"writer": true, "recording_engineer": true, "mixing_engineer": true, "mastering_engineer": true,
	"featured_artist": true, "label": true, "publisher": true, "other": true,
}

// ErrNotFound means there is no such release or track for this user.
var ErrNotFound = errors.New("credits: not found")

// ErrNoArtist means the user has no artist profile.
var ErrNoArtist = errors.New("credits: this account has no artist profile")

// ValidationError is a bad request.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// Credit is the API shape.
type Credit struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Role       string `json:"role"`
	RoleLabel  string `json:"roleLabel,omitempty"` // only when role is "other"
	TrackID    string `json:"trackId,omitempty"`   // empty: the whole release
	TrackTitle string `json:"trackTitle,omitempty"`
	ArtistID   string `json:"artistId,omitempty"` // a Blac Music artist, so the app can open their page
	// Scope is "track" or "release"; only the track endpoint sends it.
	Scope string `json:"scope,omitempty"`
}

// Input is what the artist sends for one credit.
type Input struct {
	Name      string `json:"name"`
	Role      string `json:"role"`
	RoleLabel string `json:"roleLabel"`
	TrackID   string `json:"trackId"`
	ArtistID  string `json:"artistId"`
}

// Store reads and writes credits.
type Store struct{ pool *pgxpool.Pool }

// NewStore creates the store.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const creditCols = `c.id, c.name, c.role, c.role_label, COALESCE(c.track_id, ''), COALESCE(t.title, ''), COALESCE(c.artist_id, '')`

func scanCredits(rows pgx.Rows, scope func(c *Credit)) ([]Credit, error) {
	defer rows.Close()
	out := []Credit{}
	for rows.Next() {
		var c Credit
		if err := rows.Scan(&c.ID, &c.Name, &c.Role, &c.RoleLabel, &c.TrackID, &c.TrackTitle, &c.ArtistID); err != nil {
			return nil, err
		}
		if scope != nil {
			scope(&c)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// visibleRelease checks the viewer may see the release (public, or their own).
func (s *Store) visibleRelease(ctx context.Context, releaseID, viewer string) error {
	var ok bool
	err := s.pool.QueryRow(ctx, `
		SELECT al.visibility = 'public' OR a.owner_user_id::text = $2
		FROM albums al LEFT JOIN artists a ON a.id = al.artist_id WHERE al.id = $1`, releaseID, viewer).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !ok) {
		return ErrNotFound
	}
	return err
}

// ForRelease lists the credits of a release the viewer may see: those for the whole release first, then
// those of each track in track order.
func (s *Store) ForRelease(ctx context.Context, releaseID, viewer string) ([]Credit, error) {
	if err := s.visibleRelease(ctx, releaseID, viewer); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+creditCols+` FROM release_credits c LEFT JOIN tracks t ON t.id = c.track_id
		WHERE c.release_id = $1
		ORDER BY (c.track_id IS NOT NULL), t.track_number NULLS LAST, t.created_at, c.position, c.id`, releaseID)
	if err != nil {
		return nil, err
	}
	return scanCredits(rows, nil)
}

// ForTrack lists the track's own credits and then the release-level ones of its release.
func (s *Store) ForTrack(ctx context.Context, trackID, viewer string) ([]Credit, error) {
	var releaseID string
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(t.album_id, '') FROM tracks t LEFT JOIN artists a ON a.id = t.artist_id
		WHERE t.id = $1 AND (t.visibility = 'public' OR a.owner_user_id::text = $2)`, trackID, viewer).Scan(&releaseID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+creditCols+` FROM release_credits c LEFT JOIN tracks t ON t.id = c.track_id
		WHERE c.track_id = $1 OR (c.track_id IS NULL AND c.release_id = $2)
		ORDER BY (c.track_id IS NULL), c.position, c.id`, trackID, releaseID)
	if err != nil {
		return nil, err
	}
	return scanCredits(rows, func(c *Credit) {
		if c.TrackID == "" {
			c.Scope = "release"
		} else {
			c.Scope = "track"
		}
	})
}

// Replace sets the whole list of credits of one of the user's releases.
func (s *Store) Replace(ctx context.Context, releaseID, userID string, in []Input) ([]Credit, error) {
	if len(in) > MaxCredits {
		return nil, &ValidationError{"at most 100 credits per release"}
	}
	var owned bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM albums al JOIN artists a ON a.id = al.artist_id WHERE al.id = $1 AND a.owner_user_id = $2)`,
		releaseID, userID).Scan(&owned); err != nil {
		return nil, err
	}
	if !owned {
		return nil, ErrNotFound
	}
	// the tracks of the release, to check trackId
	trackIDs := map[string]bool{}
	rows, err := s.pool.Query(ctx, `SELECT id FROM tracks WHERE album_id = $1`, releaseID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		trackIDs[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	clean := make([]Input, 0, len(in))
	seen := map[string]bool{}
	var artistIDs []string
	for i, c := range in {
		c.Name = strings.Join(strings.Fields(c.Name), " ")
		c.RoleLabel = strings.TrimSpace(c.RoleLabel)
		c.TrackID, c.ArtistID = strings.TrimSpace(c.TrackID), strings.TrimSpace(c.ArtistID)
		at := "credit " + strconv.Itoa(i+1) + ": "
		switch {
		case utf8.RuneCountInString(c.Name) < 1 || utf8.RuneCountInString(c.Name) > MaxNameRunes:
			return nil, &ValidationError{at + "name must be 1-100 characters"}
		case !Roles[c.Role]:
			return nil, &ValidationError{at + "unknown role"}
		case utf8.RuneCountInString(c.RoleLabel) > MaxLabelRune:
			return nil, &ValidationError{at + "roleLabel is at most 40 characters"}
		case c.TrackID != "" && !trackIDs[c.TrackID]:
			return nil, &ValidationError{at + "trackId is not a track of this release"}
		}
		if c.Role != "other" {
			c.RoleLabel = ""
		}
		key := strings.ToLower(c.Name) + "|" + c.Role + "|" + strings.ToLower(c.RoleLabel) + "|" + c.TrackID
		if seen[key] {
			continue // the same person in the same role on the same thing, once
		}
		seen[key] = true
		if c.ArtistID != "" {
			artistIDs = append(artistIDs, c.ArtistID)
		}
		clean = append(clean, c)
	}
	if len(artistIDs) > 0 {
		var found int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM artists WHERE id = ANY($1)`, artistIDs).Scan(&found); err != nil {
			return nil, err
		}
		if found != len(uniq(artistIDs)) {
			return nil, &ValidationError{"an artistId does not match a Blac Music artist"}
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `DELETE FROM release_credits WHERE release_id = $1`, releaseID); err != nil {
		return nil, err
	}
	for i, c := range clean {
		var track, artist *string
		if c.TrackID != "" {
			track = &c.TrackID
		}
		if c.ArtistID != "" {
			artist = &c.ArtistID
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO release_credits (release_id, track_id, name, role, role_label, artist_id, position)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`, releaseID, track, c.Name, c.Role, c.RoleLabel, artist, i); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.ForRelease(ctx, releaseID, userID)
}

// Suggest lists names the user's artist has credited before (most used first) that contain q.
func (s *Store) Suggest(ctx context.Context, userID, q string, limit int) ([]string, error) {
	var artistID string
	err := s.pool.QueryRow(ctx, `SELECT id FROM artists WHERE owner_user_id = $1`, userID).Scan(&artistID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoArtist
	}
	if err != nil {
		return nil, err
	}
	esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.TrimSpace(q))
	rows, err := s.pool.Query(ctx, `
		SELECT min(c.name) FROM release_credits c JOIN albums al ON al.id = c.release_id
		WHERE al.artist_id = $1 AND c.name ILIKE $2
		GROUP BY lower(c.name)
		ORDER BY (lower(c.name) LIKE lower($3)) DESC, count(*) DESC, lower(c.name) LIMIT $4`,
		artistID, "%"+esc+"%", esc+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
