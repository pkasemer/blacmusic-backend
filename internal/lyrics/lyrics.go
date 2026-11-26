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
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound means there is no such track for this user, or it has no lyrics.
var ErrNotFound = errors.New("lyrics: not found")

// ValidationError is a bad request.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// Lyrics is the API shape: Lines is empty unless Synced; Text is always the plain lyrics.
type Lyrics struct {
	Synced bool   `json:"synced"`
	Lines  []Line `json:"lines"`
	Text   string `json:"text"`
}

// Input is what an artist sends: LRC (timed) or plain text. LRC wins when both are present.
type Input struct {
	Text *string `json:"text"`
	LRC  *string `json:"lrc"`
}

// Store reads and writes lyrics.
type Store struct{ pool *pgxpool.Pool }

// NewStore creates the store.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Build validates the input and returns what to store.
func Build(in Input) (*Lyrics, error) {
	if in.LRC != nil && strings.TrimSpace(*in.LRC) != "" {
		lines, err := ParseLRC(*in.LRC)
		if err != nil {
			return nil, err
		}
		texts := make([]string, len(lines))
		for i, l := range lines {
			texts[i] = l.Text
		}
		text := strings.Join(texts, "\n")
		if utf8.RuneCountInString(text) > MaxTextRunes {
			return nil, &ValidationError{"the lyrics are too long"}
		}
		return &Lyrics{Synced: true, Lines: lines, Text: text}, nil
	}
	if in.Text == nil || strings.TrimSpace(*in.Text) == "" {
		return nil, &ValidationError{"send text (plain lyrics) or lrc (timed lyrics); use DELETE to remove them"}
	}
	rows := strings.Split(strings.ReplaceAll(strings.ReplaceAll(*in.Text, "\r\n", "\n"), "\r", "\n"), "\n")
	for i := range rows {
		rows[i] = strings.TrimRight(rows[i], " \t")
	}
	text := strings.Trim(strings.Join(rows, "\n"), "\n")
	if utf8.RuneCountInString(text) > MaxTextRunes {
		return nil, &ValidationError{"the lyrics are too long (" + "20000 characters at most)"}
	}
	return &Lyrics{Synced: false, Lines: []Line{}, Text: text}, nil
}

// Get returns the lyrics of a track the viewer may see (public, or their own draft).
func (s *Store) Get(ctx context.Context, trackID, viewer string) (*Lyrics, error) {
	var l Lyrics
	var raw []byte
	err := s.pool.QueryRow(ctx, `
		SELECT tl.synced, tl.text, tl.lines
		FROM track_lyrics tl JOIN tracks t ON t.id = tl.track_id LEFT JOIN artists a ON a.id = t.artist_id
		WHERE tl.track_id = $1 AND (t.visibility = 'public' OR a.owner_user_id::text = $2)`, trackID, viewer).
		Scan(&l.Synced, &l.Text, &raw)
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
	return &l, nil
}

// Put sets the lyrics of one of the user's tracks (draft or live).
func (s *Store) Put(ctx context.Context, trackID, userID string, in Input) (*Lyrics, error) {
	l, err := Build(in)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(l.Lines)
	if err != nil {
		return nil, err
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO track_lyrics (track_id, synced, text, lines)
		SELECT t.id, $3, $4, $5 FROM tracks t JOIN artists a ON a.id = t.artist_id
		WHERE t.id = $1 AND a.owner_user_id = $2
		ON CONFLICT (track_id) DO UPDATE SET synced = $3, text = $4, lines = $5, updated_at = now()`,
		trackID, userID, l.Synced, l.Text, raw)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	return l, nil
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
	_, err := s.pool.Exec(ctx, `DELETE FROM track_lyrics WHERE track_id = $1`, trackID)
	return err
}
