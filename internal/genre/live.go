package genre

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The team manages the genres, so the list in use is read from the database and refreshed; All is what a process
// starts with. A genre switched off is no longer offered or accepted, but songs that have it keep it. A renamed
// genre's earlier names are still understood.

// entry is a genre as it is managed.
type entry struct {
	Genre
	active bool
	former []string
}

type snapshot struct {
	list  []Genre // the ones offered, in order
	byKey map[string]entry
}

var current atomic.Pointer[snapshot]

func init() {
	entries := make([]entry, len(All))
	for i, g := range All {
		entries[i] = entry{Genre: g, active: true}
	}
	current.Store(build(entries))
}

func build(entries []entry) *snapshot {
	s := &snapshot{list: []Genre{}, byKey: map[string]entry{}}
	// earlier names first, then other spellings, then ids, then names: a name in use wins
	for _, e := range entries {
		for _, f := range e.former {
			s.byKey[strings.ToLower(f)] = e
		}
	}
	for _, e := range entries {
		if e.ID == "hip-hop-rap" {
			for _, alias := range []string{"hip hop", "hip-hop", "hiphop"} {
				s.byKey[alias] = e
			}
		}
		s.byKey[e.ID] = e
	}
	for _, e := range entries {
		s.byKey[strings.ToLower(e.Name)] = e
		if e.active {
			s.list = append(s.list, e.Genre)
		}
	}
	return s
}

// NormalizeChange is Normalize for an edit: sending back the genre something already has is always allowed, even
// one that is switched off or from before the list.
func NormalizeChange(next, has string) (string, error) {
	if strings.EqualFold(strings.TrimSpace(next), has) {
		return has, nil
	}
	return Normalize(next)
}

// Reload reads the genres from the database.
func Reload(ctx context.Context, pool *pgxpool.Pool) error {
	rows, err := pool.Query(ctx, `
		SELECT g.slug, g.label, g.active, ARRAY(SELECT a.label FROM tax_genre_aliases a WHERE a.genre_id = g.id)
		FROM tax_genres g ORDER BY g.position, g.label`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var entries []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.ID, &e.Name, &e.active, &e.former); err != nil {
			return err
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil || len(entries) == 0 {
		return err
	}
	current.Store(build(entries))
	return nil
}

// Watch keeps the list fresh until ctx ends: another process may have changed it.
func Watch(ctx context.Context, pool *pgxpool.Pool, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := Reload(ctx, pool); err != nil && ctx.Err() == nil {
			slog.Warn("genres not reloaded", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
