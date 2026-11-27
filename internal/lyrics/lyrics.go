// Package lyrics stores the lyrics an artist writes for a track and serves them to listeners: plain text,
// or timed lines (from an LRC file) that the player highlights as the song plays.
package lyrics

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/review"
)

// ErrNotFound means there is no such track for this user, or it has no lyrics.
var ErrNotFound = errors.New("lyrics: not found")

// ValidationError is a bad request. Lines lists everything wrong in timed lyrics, by line as typed.
type ValidationError struct {
	Msg   string
	Lines []LineError
}

func (e *ValidationError) Error() string { return e.Msg }

// Lyrics is the API shape: Lines is empty unless Synced; Text is always the plain lyrics.
type Lyrics struct {
	Synced bool   `json:"synced"`
	Lines  []Line `json:"lines"`
	Text   string `json:"text"`
	// LRC is timed lyrics as they are edited, translations included; absent for plain lyrics.
	LRC string `json:"lrc,omitempty"`
}

// Untranslated counts the sung lines that have no translation.
func (l *Lyrics) Untranslated() (n int) {
	for _, x := range l.Lines {
		if x.Text != "" && x.Translation == "" {
			n++
		}
	}
	return n
}

// Input is what an artist sends: LRC (timed) or plain text. LRC wins when both are present.
type Input struct {
	Text *string `json:"text"`
	LRC  *string `json:"lrc"`
}

// Store reads and writes lyrics.
type Store struct {
	pool *pgxpool.Pool
	// Review: an artist's new or changed lyrics on a song listeners can hear wait for the team.
	Review bool
}

// NewStore creates the store.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Build validates the input and returns what to store. durationMs is the song's length, 0 when unknown.
func Build(in Input, durationMs int) (*Lyrics, error) {
	if in.LRC != nil && strings.TrimSpace(*in.LRC) != "" {
		lines, err := ParseLRC(*in.LRC, durationMs)
		if err != nil {
			return nil, err
		}
		texts := make([]string, len(lines))
		for i, l := range lines {
			texts[i] = l.Text
		}
		text := strings.Join(texts, "\n")
		if utf8.RuneCountInString(text) > MaxTextRunes {
			return nil, &ValidationError{Msg: "the lyrics are too long"}
		}
		typed := strings.Trim(normalizeNewlines(strings.TrimPrefix(*in.LRC, "\ufeff")), "\n")
		return &Lyrics{Synced: true, Lines: lines, Text: text, LRC: typed}, nil
	}
	if in.Text == nil || strings.TrimSpace(*in.Text) == "" {
		return nil, &ValidationError{Msg: "send text (plain lyrics) or lrc (timed lyrics); use DELETE to remove them"}
	}
	rows := strings.Split(strings.ReplaceAll(strings.ReplaceAll(*in.Text, "\r\n", "\n"), "\r", "\n"), "\n")
	for i := range rows {
		rows[i] = strings.TrimRight(rows[i], " \t")
	}
	text := strings.Trim(strings.Join(rows, "\n"), "\n")
	if utf8.RuneCountInString(text) > MaxTextRunes {
		return nil, &ValidationError{Msg: "the lyrics are too long (20000 characters at most)"}
	}
	return &Lyrics{Synced: false, Lines: []Line{}, Text: text}, nil
}

// Get returns the lyrics of a track the viewer may see (public, or their own draft).
func (s *Store) Get(ctx context.Context, trackID, viewer string) (*Lyrics, error) {
	var l Lyrics
	var raw []byte
	err := s.pool.QueryRow(ctx, `
		SELECT tl.synced, tl.text, tl.lines, tl.lrc
		FROM track_lyrics tl JOIN tracks t ON t.id = tl.track_id LEFT JOIN artists a ON a.id = t.artist_id
		WHERE tl.track_id = $1 AND (t.visibility = 'public' OR a.owner_user_id::text = $2)`, trackID, viewer).
		Scan(&l.Synced, &l.Text, &raw, &l.LRC)
	return read(&l, raw, err)
}

func read(l *Lyrics, raw []byte, err error) (*Lyrics, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	l.Lines = []Line{}
	if err := json.Unmarshal(raw, &l.Lines); err != nil {
		return nil, err
	}
	if l.Synced && l.LRC == "" { // saved before what was typed was kept
		l.LRC = Format(l.Lines)
	}
	return l, nil
}

// DB is a pool or a transaction.
type DB interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Of reads any track's lyrics, for an administrator; ErrNotFound when it has none.
func Of(ctx context.Context, db DB, trackID string) (*Lyrics, error) {
	var l Lyrics
	var raw []byte
	err := db.QueryRow(ctx, `SELECT synced, text, lines, lrc FROM track_lyrics WHERE track_id = $1`, trackID).
		Scan(&l.Synced, &l.Text, &raw, &l.LRC)
	return read(&l, raw, err)
}

// Save stores lyrics Build made for a track that exists. The caller has checked who may.
func Save(ctx context.Context, db DB, trackID string, l *Lyrics) error {
	raw, err := json.Marshal(l.Lines)
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, `
		INSERT INTO track_lyrics (track_id, synced, text, lines, lrc) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (track_id) DO UPDATE SET synced = $2, text = $3, lines = $4, lrc = $5, updated_at = now()`,
		trackID, l.Synced, l.Text, raw, l.LRC)
	return err
}

// Put sets the lyrics of one of the user's tracks (draft or live).
func (s *Store) Put(ctx context.Context, trackID, userID string, in Input) (*Lyrics, error) {
	var durationMs int
	err := s.pool.QueryRow(ctx, `SELECT t.duration_ms FROM tracks t JOIN artists a ON a.id = t.artist_id
		WHERE t.id = $1 AND a.owner_user_id = $2`, trackID, userID).Scan(&durationMs)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	l, err := Build(in, durationMs)
	if err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	releaseID, waits, held, err := s.gate(ctx, tx, trackID)
	if err != nil {
		return nil, err
	}
	if waits {
		raw, err := json.Marshal(l)
		if err != nil {
			return nil, err
		}
		approved, err := Of(ctx, tx, trackID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		same := approved != nil && approved.Synced == l.Synced && approved.Text == l.Text && approved.LRC == l.LRC
		err = review.Stage(ctx, tx, releaseID, func(c *review.Changes) {
			if same {
				delete(c.Lyrics, trackID)
				return
			}
			if c.Lyrics == nil {
				c.Lyrics = map[string]json.RawMessage{}
			}
			c.Lyrics[trackID] = raw
		})
		if err != nil {
			return nil, err
		}
	} else {
		if err := Save(ctx, tx, trackID, l); err != nil {
			return nil, err
		}
		if held {
			if err := review.Alter(ctx, tx, releaseID, nil); err != nil {
				return nil, err
			}
		}
	}
	return l, tx.Commit(ctx)
}

// gate locks a song's release and says whether a change to the song waits for review; held is a new song that is
// itself still waiting. A release being checked as new takes no changes (review.ErrPending).
func (s *Store) gate(ctx context.Context, tx pgx.Tx, trackID string) (releaseID string, waits, held bool, err error) {
	if err = tx.QueryRow(ctx, `SELECT COALESCE(album_id, '') FROM tracks WHERE id = $1`, trackID).Scan(&releaseID); err != nil {
		return "", false, false, err
	}
	var visibility string
	if err = tx.QueryRow(ctx, `SELECT visibility FROM albums WHERE id = $1 FOR UPDATE`, releaseID).Scan(&visibility); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, false, nil
		}
		return "", false, false, err
	}
	if err = tx.QueryRow(ctx, `SELECT review_hold FROM tracks WHERE id = $1`, trackID).Scan(&held); err != nil {
		return "", false, false, err
	}
	if err = review.Locked(ctx, tx, releaseID); err != nil {
		return "", false, false, err
	}
	return releaseID, s.Review && !held && visibility != "draft", held, nil
}

// ForOwner is what the song's owner gets: lyrics waiting for review as they sent them, else the approved ones, with
// what is waiting or was refused. info is nil for anyone else and when nothing is.
func (s *Store) ForOwner(ctx context.Context, trackID, viewer string) (*Lyrics, *review.Info, error) {
	var releaseID string
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(t.album_id, '') FROM tracks t JOIN artists a ON a.id = t.artist_id
		WHERE t.id = $1 AND a.owner_user_id::text = $2`, trackID, viewer).Scan(&releaseID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, err
	}
	if err == nil {
		info, row, err := review.ForOwner(ctx, s.pool, releaseID)
		if err != nil {
			return nil, nil, err
		}
		if info != nil && row.Kind == "update" {
			if raw, ok := row.Changes.Lyrics[trackID]; ok {
				if row.Status == "pending" {
					var waiting Lyrics
					if err := json.Unmarshal(raw, &waiting); err != nil {
						return nil, nil, err
					}
					return &waiting, info, nil
				}
				l, err := s.Get(ctx, trackID, viewer)
				return l, info, err
			}
		}
	}
	l, err := s.Get(ctx, trackID, viewer)
	return l, nil, err
}

// Delete removes the lyrics of one of the user's tracks. Removing lyrics that are not there is fine.
func (s *Store) Delete(ctx context.Context, trackID, userID string) error {
	var owned bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM tracks t JOIN artists a ON a.id = t.artist_id WHERE t.id = $1 AND a.owner_user_id = $2)`,
		trackID, userID).Scan(&owned); err != nil {
		return err
	}
	if !owned {
		return ErrNotFound
	}
	// taking lyrics down never waits, but a release being checked as new is not changed
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	releaseID, _, _, err := s.gate(ctx, tx, trackID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM track_lyrics WHERE track_id = $1`, trackID); err != nil {
		return err
	}
	if err := review.Alter(ctx, tx, releaseID, func(c *review.Changes) { delete(c.Lyrics, trackID) }); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
