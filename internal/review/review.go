// Package review holds what artists have sent for the team to check: new releases, and changes to releases that are
// online. It records the submissions and tells the artist about them; package release applies the decisions.
package review

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrPending: a new release is locked while the team checks it.
var ErrPending = errors.New("this release is waiting for review: withdraw it to change it")

// Message answers a 409 review_pending.
const Message = "this release is waiting for our team to check it: withdraw it to change it"

// DB is a pool or a transaction.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// TrackChange is a waiting edit of one song; a nil field is unchanged. TrackNumber 0 clears the number.
type TrackChange struct {
	Title       *string `json:"title,omitempty"`
	Explicit    *bool   `json:"explicit,omitempty"`
	Genre       *string `json:"genre,omitempty"`
	TrackNumber *int    `json:"trackNumber,omitempty"`
}

// Changes are what waits to be applied to a release that is online.
type Changes struct {
	// Release holds title, description, genre, releaseDate and coverImageId ("" for no cover).
	Release map[string]string      `json:"release,omitempty"`
	Tracks  map[string]TrackChange `json:"tracks,omitempty"`
	// Credits is the whole new list, as the artist sent it.
	Credits json.RawMessage `json:"credits,omitempty"`
	// Lyrics are the new lyrics by track, ready to store.
	Lyrics    map[string]json.RawMessage `json:"lyrics,omitempty"`
	NewTracks []string                   `json:"newTracks,omitempty"`
}

// Empty says whether nothing is waiting.
func (c *Changes) Empty() bool {
	return len(c.Release) == 0 && len(c.Tracks) == 0 && len(c.Credits) == 0 && len(c.Lyrics) == 0 && len(c.NewTracks) == 0
}

// Drop forgets everything about a song that is gone.
func (c *Changes) Drop(trackID string) {
	delete(c.Tracks, trackID)
	delete(c.Lyrics, trackID)
	c.NewTracks = slices.DeleteFunc(c.NewTracks, func(id string) bool { return id == trackID })
}

// Row is one submission.
type Row struct {
	ID, ReleaseID, Kind, Status, Reason string
	Revision                            int
	SubmittedAt                         time.Time
	DecidedAt, PublishAt                *time.Time
	Changes                             Changes
}

const columns = `id, release_id, kind, status, reason, revision, submitted_at, decided_at, publish_at, changes`

// Scan reads a row selected with Columns.
func scan(row pgx.Row) (*Row, error) {
	var r Row
	var raw []byte
	err := row.Scan(&r.ID, &r.ReleaseID, &r.Kind, &r.Status, &r.Reason, &r.Revision, &r.SubmittedAt, &r.DecidedAt, &r.PublishAt, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, json.Unmarshal(raw, &r.Changes)
}

// Get reads one submission; nil when there is none.
func Get(ctx context.Context, db DB, id string) (*Row, error) {
	return scan(db.QueryRow(ctx, `SELECT `+columns+` FROM release_reviews WHERE id = $1`, id))
}

// Latest is the release's last submission; nil when it never had one.
func Latest(ctx context.Context, db DB, releaseID string) (*Row, error) {
	return scan(db.QueryRow(ctx, `SELECT `+columns+` FROM release_reviews WHERE release_id = $1 ORDER BY seq DESC LIMIT 1`, releaseID))
}

// Pending is the submission waiting for the release; nil when none is.
func Pending(ctx context.Context, db DB, releaseID string) (*Row, error) {
	return scan(db.QueryRow(ctx, `SELECT `+columns+` FROM release_reviews WHERE release_id = $1 AND status = 'pending'`, releaseID))
}

// Locked returns ErrPending while the release is a new one being checked.
func Locked(ctx context.Context, db DB, releaseID string) error {
	var locked bool
	if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM release_reviews WHERE release_id = $1 AND status = 'pending' AND kind = 'new')`,
		releaseID).Scan(&locked); err != nil {
		return err
	}
	if locked {
		return ErrPending
	}
	return nil
}

// Info is what the artist is told about a release that has something waiting or refused.
type Info struct {
	Status      string     `json:"status"` // pending | rejected
	Kind        string     `json:"kind"`   // new | update
	SubmittedAt time.Time  `json:"submittedAt"`
	DecidedAt   *time.Time `json:"decidedAt,omitempty"`
	Reason      string     `json:"reason,omitempty"`
	PublishAt   *time.Time `json:"publishAt,omitempty"`
	Changes     []string   `json:"changes,omitempty"`
}

// ForOwner gives the artist's view of a release's last submission, with the submission itself; both nil when
// nothing is waiting or refused.
func ForOwner(ctx context.Context, db DB, releaseID string) (*Info, *Row, error) {
	r, err := Latest(ctx, db, releaseID)
	if err != nil || r == nil || (r.Status != "pending" && r.Status != "rejected") {
		return nil, nil, err
	}
	info := &Info{Status: r.Status, Kind: r.Kind, SubmittedAt: r.SubmittedAt, DecidedAt: r.DecidedAt, Reason: r.Reason, PublishAt: r.PublishAt}
	if r.Kind == "update" {
		if info.Changes, err = Labels(ctx, db, r.Changes); err != nil {
			return nil, nil, err
		}
	}
	return info, r, nil
}

var releaseLabels = []struct{ key, label string }{
	{"title", "Title"}, {"coverImageId", "Cover"}, {"description", "Description"}, {"genre", "Genre"}, {"releaseDate", "Release date"},
}

// Labels names the waiting changes in words.
func Labels(ctx context.Context, db DB, c Changes) ([]string, error) {
	var ids []string
	for id := range c.Tracks {
		ids = append(ids, id)
	}
	for id := range c.Lyrics {
		ids = append(ids, id)
	}
	ids = append(ids, c.NewTracks...)
	titles := map[string]string{}
	if len(ids) > 0 {
		rows, err := db.Query(ctx, `SELECT id, title FROM tracks WHERE id = ANY($1)`, ids)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var id, title string
			if err := rows.Scan(&id, &title); err != nil {
				return nil, err
			}
			titles[id] = title
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	out := []string{}
	for _, f := range releaseLabels {
		if _, ok := c.Release[f.key]; ok {
			out = append(out, f.label)
		}
	}
	each := func(prefix string, ids []string) {
		var named []string
		for _, id := range ids {
			named = append(named, prefix+": "+titles[id])
		}
		slices.Sort(named)
		out = append(out, named...)
	}
	each("New track", c.NewTracks)
	var edited, sung []string
	for id := range c.Tracks {
		edited = append(edited, id)
	}
	for id := range c.Lyrics {
		sung = append(sung, id)
	}
	each("Track", edited)
	each("Lyrics", sung)
	if len(c.Credits) > 0 {
		out = append(out, "Credits")
	}
	return out, nil
}

// SubmitNew sends a draft release to be checked, or changes when an already waiting one asks to go live. The caller
// holds the release's row lock.
func SubmitNew(ctx context.Context, db DB, releaseID string, publishAt *time.Time) error {
	tag, err := db.Exec(ctx, `UPDATE release_reviews SET publish_at = $2, revision = revision + 1
		WHERE release_id = $1 AND status = 'pending' AND kind = 'new'`, releaseID, publishAt)
	if err != nil || tag.RowsAffected() == 1 {
		return err
	}
	// anything older still waiting is replaced by the whole release
	if _, err := db.Exec(ctx, `UPDATE release_reviews SET status = 'withdrawn', decided_at = now() WHERE release_id = $1 AND status = 'pending'`, releaseID); err != nil {
		return err
	}
	var id string
	if err := db.QueryRow(ctx, `INSERT INTO release_reviews (release_id, kind, publish_at) VALUES ($1, 'new', $2) RETURNING id`,
		releaseID, publishAt).Scan(&id); err != nil {
		return err
	}
	return tellSubmitted(ctx, db, releaseID, id, "new")
}

// Stage adds to (or takes from) the changes waiting for a release that is online. The artist is told when something
// starts waiting, not for each later change. The caller holds the release's row lock.
func Stage(ctx context.Context, db DB, releaseID string, change func(*Changes)) error {
	r, err := Pending(ctx, db, releaseID)
	if err != nil {
		return err
	}
	if r != nil && r.Kind == "new" {
		return ErrPending
	}
	if r == nil {
		var c Changes
		change(&c)
		if c.Empty() {
			return nil
		}
		raw, err := json.Marshal(c)
		if err != nil {
			return err
		}
		var id string
		if err := db.QueryRow(ctx, `INSERT INTO release_reviews (release_id, kind, changes) VALUES ($1, 'update', $2) RETURNING id`,
			releaseID, raw).Scan(&id); err != nil {
			return err
		}
		return tellSubmitted(ctx, db, releaseID, id, "update")
	}
	change(&r.Changes)
	return store(ctx, db, r)
}

// store saves a waiting submission that changed: a new revision, or withdrawn when an update has nothing left.
func store(ctx context.Context, db DB, r *Row) error {
	if r.Kind == "update" && r.Changes.Empty() {
		return Close(ctx, db, r.ID, "withdrawn", "", "")
	}
	raw, err := json.Marshal(r.Changes)
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, `UPDATE release_reviews SET changes = $2, revision = revision + 1 WHERE id = $1`, r.ID, raw)
	return err
}

// Alter changes whatever is waiting for a release, if anything, because the release itself changed: a decision on
// what the team saw before is then refused. forget may be nil. Nothing new starts waiting.
func Alter(ctx context.Context, db DB, releaseID string, forget func(*Changes)) error {
	r, err := Pending(ctx, db, releaseID)
	if err != nil || r == nil {
		return err
	}
	if forget != nil {
		forget(&r.Changes)
	}
	return store(ctx, db, r)
}

// Close ends a submission: approved, rejected (with the reason) or withdrawn. by is the administrator, or "".
func Close(ctx context.Context, db DB, id, status, by, reason string) error {
	var who *string
	if by != "" {
		who = &by
	}
	_, err := db.Exec(ctx, `UPDATE release_reviews SET status = $2, decided_at = now(), decided_by = $3, reason = $4 WHERE id = $1`,
		id, status, who, reason)
	return err
}
