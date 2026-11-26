// Package importopt keeps the list of ways to import songs (upload your own files, YouTube, ...) and
// whether each is switched on. The app shows the list; the transfer API enforces it.
package importopt

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Well-known option ids.
const (
	Manual  = "manual"  // the artist names songs and uploads a file for each
	YouTube = "youtube" // songs picked from the artist's connected YouTube channel
)

// Option is one way to import.
type Option struct {
	ID          string `json:"id"`
	Enabled     bool   `json:"enabled"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Note        string `json:"note,omitempty"` // short text for a disabled option, e.g. "Coming soon"
}

// ValidationError is a bad request.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

var idRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,29}$`)

// Store reads and changes the options.
type Store struct{ pool *pgxpool.Pool }

// NewStore creates the store.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// List returns all options in display order.
func (s *Store) List(ctx context.Context) ([]Option, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, enabled, title, description, note FROM import_options ORDER BY position, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Option{}
	for rows.Next() {
		var o Option
		if err := rows.Scan(&o.ID, &o.Enabled, &o.Title, &o.Description, &o.Note); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// Enabled says whether the option is on. An option that does not exist is off.
func (s *Store) Enabled(ctx context.Context, id string) (bool, error) {
	var on bool
	err := s.pool.QueryRow(ctx, `SELECT enabled FROM import_options WHERE id = $1`, id).Scan(&on)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return on, err
}

// Patch holds what an administrator may change; nil means unchanged. Position only matters for new options.
type Patch struct {
	Enabled     *bool   `json:"enabled"`
	Title       *string `json:"title"`
	Description *string `json:"description"`
	Note        *string `json:"note"`
	Position    *int    `json:"position"`
}

// Upsert changes an option, creating it (disabled) when the id is new.
func (s *Store) Upsert(ctx context.Context, id string, p Patch) (*Option, error) {
	id = strings.TrimSpace(id)
	if !idRe.MatchString(id) {
		return nil, &ValidationError{"id must be lowercase letters, digits and underscores, starting with a letter (30 at most)"}
	}
	if p.Enabled == nil && p.Title == nil && p.Description == nil && p.Note == nil && p.Position == nil {
		return nil, &ValidationError{"send enabled, title, description, note and/or position"}
	}
	for name, v := range map[string]*string{"title": p.Title, "description": p.Description, "note": p.Note} {
		if v == nil {
			continue
		}
		*v = strings.TrimSpace(*v)
		limit := map[string]int{"title": 60, "description": 300, "note": 100}[name]
		if utf8.RuneCountInString(*v) > limit {
			return nil, &ValidationError{name + " is too long"}
		}
	}
	var o Option
	err := s.pool.QueryRow(ctx, `
		INSERT INTO import_options (id, enabled, title, description, note, position)
		VALUES ($1, COALESCE($2, false), COALESCE($3, ''), COALESCE($4, ''), COALESCE($5, ''), COALESCE($6, (SELECT COALESCE(max(position), 0) + 1 FROM import_options)))
		ON CONFLICT (id) DO UPDATE SET
			enabled = COALESCE($2, import_options.enabled), title = COALESCE($3, import_options.title),
			description = COALESCE($4, import_options.description), note = COALESCE($5, import_options.note),
			position = COALESCE($6, import_options.position), updated_at = now()
		RETURNING id, enabled, title, description, note`, id, p.Enabled, p.Title, p.Description, p.Note, p.Position).
		Scan(&o.ID, &o.Enabled, &o.Title, &o.Description, &o.Note)
	if err != nil {
		return nil, err
	}
	return &o, nil
}
