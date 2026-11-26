package playlist

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const maxGroupPlaylists = 100

var (
	ErrGroupNotFound = errors.New("playlist group not found")
	ErrGroupExists   = errors.New("a playlist group with that id already exists")
)

var groupIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// Group is a named shelf of playlists as listeners see it.
type Group struct {
	ID            string     `json:"id"`
	Title         string     `json:"title"`
	Description   string     `json:"description,omitempty"`
	Position      int        `json:"position"`
	PlaylistCount int        `json:"playlistCount"`
	Playlists     []Playlist `json:"playlists"`
}

// AdminGroup is a group as the team manages it: also inactive ones, and the
// member ids in order (including playlists that are currently not listed
// because they are private or empty).
type AdminGroup struct {
	ID            string   `json:"id"`
	Title         string   `json:"title"`
	Description   string   `json:"description"`
	Position      int      `json:"position"`
	IsActive      bool     `json:"isActive"`
	PlaylistCount int      `json:"playlistCount"` // how many are listed right now
	PlaylistIDs   []string `json:"playlistIds"`
}

// GroupInput creates a group. ID is optional: it defaults to a slug of Title.
type GroupInput struct {
	ID          string
	Title       string
	Description string
	Position    *int
	IsActive    *bool
}

// GroupPatch changes a group; nil fields stay as they are.
type GroupPatch struct {
	Title       *string
	Description *string
	Position    *int
	IsActive    *bool
}

// listed is the condition (on playlists p) for a playlist to be shown to listeners.
const listed = `p.is_public
	AND EXISTS (SELECT 1 FROM playlist_items i JOIN tracks t ON t.id = i.track_id
	            WHERE i.playlist_id = p.id AND t.visibility = 'public')`

// Slug turns a title into a group id: "Today's biggest hits" -> "todays-biggest-hits".
func Slug(title string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(title) {
		switch {
		case r == '\'' || r == '’':
			// dropped, so "today's" becomes "todays"
		case r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			b.WriteRune(r)
			dash = false
		default:
			if b.Len() > 0 && !dash {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	return s
}

func validateGroupTitle(t string) error {
	if n := utf8.RuneCountInString(t); n < 1 || n > 60 {
		return &ValidationError{"title must be 1-60 characters"}
	}
	return nil
}

func validateGroupDescription(d string) error {
	if utf8.RuneCountInString(d) > 200 {
		return &ValidationError{"description must be at most 200 characters"}
	}
	return nil
}

// ---- what listeners see ----

// ListGroups returns the active groups that have at least one listed playlist,
// in position order, each with its first perGroup playlists.
func (s *Store) ListGroups(ctx context.Context, viewer string, limit, perGroup int) ([]Group, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT g.id, g.title, g.description, g.position,
		       (SELECT count(*) FROM playlist_group_items gi JOIN playlists p ON p.id = gi.playlist_id
		         WHERE gi.group_id = g.id AND `+listed+`) AS n
		FROM playlist_groups g
		WHERE g.is_active
		ORDER BY g.position, g.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []Group
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.ID, &g.Title, &g.Description, &g.Position, &g.PlaylistCount); err != nil {
			return nil, err
		}
		if g.PlaylistCount > 0 {
			all = append(all, g)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if len(all) > limit {
		all = all[:limit]
	}
	for i := range all {
		ps, err := s.groupPlaylists(ctx, viewer, all[i].ID, perGroup, 0)
		if err != nil {
			return nil, err
		}
		all[i].Playlists = ps
	}
	if all == nil {
		all = []Group{}
	}
	return all, nil
}

// GetGroup returns one active group with a page of its listed playlists. An
// active group without listed playlists is returned with none; unknown or
// inactive groups are ErrGroupNotFound.
func (s *Store) GetGroup(ctx context.Context, viewer, id string, limit, offset int) (*Group, error) {
	var g Group
	err := s.pool.QueryRow(ctx, `
		SELECT g.id, g.title, g.description, g.position,
		       (SELECT count(*) FROM playlist_group_items gi JOIN playlists p ON p.id = gi.playlist_id
		         WHERE gi.group_id = g.id AND `+listed+`)
		FROM playlist_groups g WHERE g.id = $1 AND g.is_active`, id).
		Scan(&g.ID, &g.Title, &g.Description, &g.Position, &g.PlaylistCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrGroupNotFound
	}
	if err != nil {
		return nil, err
	}
	g.Playlists, err = s.groupPlaylists(ctx, viewer, id, limit, offset)
	if err != nil {
		return nil, err
	}
	if g.Playlists == nil {
		g.Playlists = []Playlist{}
	}
	return &g, nil
}

func (s *Store) groupPlaylists(ctx context.Context, viewer, groupID string, limit, offset int) ([]Playlist, error) {
	rows, err := s.pool.Query(ctx, playlistSelect+`
		JOIN playlist_group_items gi ON gi.playlist_id = p.id AND gi.group_id = $2
		WHERE `+listed+`
		ORDER BY gi.position, p.id LIMIT $3 OFFSET $4`, viewer, groupID, limit, offset)
	if err != nil {
		return nil, err
	}
	return s.scanPlaylists(rows)
}

// ---- what the team manages ----

const adminGroupSelect = `
	SELECT g.id, g.title, g.description, g.position, g.is_active,
	       (SELECT count(*) FROM playlist_group_items gi JOIN playlists p ON p.id = gi.playlist_id
	         WHERE gi.group_id = g.id AND ` + listed + `),
	       ARRAY(SELECT gi.playlist_id FROM playlist_group_items gi WHERE gi.group_id = g.id ORDER BY gi.position)
	FROM playlist_groups g`

func scanAdminGroup(row pgx.Row) (*AdminGroup, error) {
	var g AdminGroup
	if err := row.Scan(&g.ID, &g.Title, &g.Description, &g.Position, &g.IsActive, &g.PlaylistCount, &g.PlaylistIDs); err != nil {
		return nil, err
	}
	if g.PlaylistIDs == nil {
		g.PlaylistIDs = []string{}
	}
	return &g, nil
}

// AdminListGroups returns every group, active or not.
func (s *Store) AdminListGroups(ctx context.Context) ([]AdminGroup, error) {
	rows, err := s.pool.Query(ctx, adminGroupSelect+` ORDER BY g.position, g.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminGroup{}
	for rows.Next() {
		g, err := scanAdminGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *g)
	}
	return out, rows.Err()
}

// AdminGetGroup returns one group.
func (s *Store) AdminGetGroup(ctx context.Context, id string) (*AdminGroup, error) {
	g, err := scanAdminGroup(s.pool.QueryRow(ctx, adminGroupSelect+` WHERE g.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrGroupNotFound
	}
	return g, err
}

// CreateGroup adds a group. A missing position puts it after the others.
func (s *Store) CreateGroup(ctx context.Context, in GroupInput) (*AdminGroup, error) {
	in.Title = strings.TrimSpace(in.Title)
	if err := validateGroupTitle(in.Title); err != nil {
		return nil, err
	}
	if err := validateGroupDescription(in.Description); err != nil {
		return nil, err
	}
	id := in.ID
	if id == "" {
		id = Slug(in.Title)
	}
	if !groupIDRe.MatchString(id) {
		return nil, &ValidationError{"id must be 1-40 lowercase letters, digits or dashes"}
	}
	active := true
	if in.IsActive != nil {
		active = *in.IsActive
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO playlist_groups (id, title, description, position, is_active)
		VALUES ($1, $2, $3, COALESCE($4, (SELECT COALESCE(max(position), 0) + 1 FROM playlist_groups)), $5)`,
		id, in.Title, in.Description, in.Position, active)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return nil, ErrGroupExists
	}
	if err != nil {
		return nil, err
	}
	return s.AdminGetGroup(ctx, id)
}

// UpdateGroup changes a group's title, description, position or visibility.
func (s *Store) UpdateGroup(ctx context.Context, id string, p GroupPatch) (*AdminGroup, error) {
	if p.Title != nil {
		t := strings.TrimSpace(*p.Title)
		if err := validateGroupTitle(t); err != nil {
			return nil, err
		}
		p.Title = &t
	}
	if p.Description != nil {
		if err := validateGroupDescription(*p.Description); err != nil {
			return nil, err
		}
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE playlist_groups SET
			title       = COALESCE($2, title),
			description = COALESCE($3, description),
			position    = COALESCE($4, position),
			is_active   = COALESCE($5, is_active),
			updated_at  = now()
		WHERE id = $1`, id, p.Title, p.Description, p.Position, p.IsActive)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrGroupNotFound
	}
	return s.AdminGetGroup(ctx, id)
}

// DeleteGroup removes a group (not its playlists).
func (s *Store) DeleteGroup(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM playlist_groups WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrGroupNotFound
	}
	return nil
}

// SetGroupPlaylists replaces the group's members and their order. Every id
// must be an existing playlist; a playlist may be in many groups. Private or
// empty playlists may be members but are not listed to listeners.
func (s *Store) SetGroupPlaylists(ctx context.Context, id string, playlistIDs []string) (*AdminGroup, error) {
	if len(playlistIDs) > maxGroupPlaylists {
		return nil, &ValidationError{"a group holds at most 100 playlists"}
	}
	seen := map[string]bool{}
	for _, p := range playlistIDs {
		if seen[p] {
			return nil, &ValidationError{"playlistIds must not repeat a playlist: " + p}
		}
		seen[p] = true
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var locked string
	err = tx.QueryRow(ctx, `SELECT id FROM playlist_groups WHERE id = $1 FOR UPDATE`, id).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrGroupNotFound
	}
	if err != nil {
		return nil, err
	}
	if len(playlistIDs) > 0 {
		rows, err := tx.Query(ctx, `SELECT p FROM unnest($1::text[]) p WHERE NOT EXISTS (SELECT 1 FROM playlists WHERE id = p)`, playlistIDs)
		if err != nil {
			return nil, err
		}
		var missing []string
		for rows.Next() {
			var m string
			if err := rows.Scan(&m); err != nil {
				rows.Close()
				return nil, err
			}
			missing = append(missing, m)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if len(missing) > 0 {
			return nil, &ValidationError{"unknown playlists: " + strings.Join(missing, ", ")}
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM playlist_group_items WHERE group_id = $1`, id); err != nil {
		return nil, err
	}
	if len(playlistIDs) > 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO playlist_group_items (group_id, playlist_id, position)
			SELECT $1, p, ord - 1 FROM unnest($2::text[]) WITH ORDINALITY AS u(p, ord)`, id, playlistIDs); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE playlist_groups SET updated_at = now() WHERE id = $1`, id); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.AdminGetGroup(ctx, id)
}
