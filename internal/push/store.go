// Package push sends notifications to the app through Expo's push service. Events
// (a transfer finished, a scheduled release went live) put messages in an outbox
// in the database; the worker sends them in batches and reads the receipts later.
package push

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MaxTokensPerUser bounds the devices one account can register; the oldest is dropped beyond it.
const MaxTokensPerUser = 10

// Categories users can switch off.
const (
	CategoryTransfers = "transfers"
	CategoryReleases  = "releases"
)

// Notification types the app routes taps by (data.type).
const (
	TypeTransfer = "transfer"
	TypeRelease  = "release"
	TypeAlbum    = "album"
	TypeArtist   = "artist"
	TypePlaylist = "playlist"
	TypeTrack    = "track"
)

var tokenRe = regexp.MustCompile(`^(ExponentPushToken|ExpoPushToken)\[[A-Za-z0-9_\-]{8,200}\]$`)

// ValidationError is a bad request.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// DB is what Enqueue needs; both a pool and a transaction satisfy it, so an event
// can queue its message in the same transaction that caused it.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Message is what the user sees, and where a tap goes.
type Message struct {
	Title string
	Body  string
	Type  string // data.type
	ID    string // data.id
	URL   string // data.url, optional
}

// Settings are the user's switches.
type Settings struct {
	Transfers     bool `json:"transfers"`     // push about imports
	Releases      bool `json:"releases"`      // push about releases (went live, audio processed)
	ReleaseEmails bool `json:"releaseEmails"` // emails about releases (published, audio processed)
}

// SettingsPatch changes the switches that are not nil.
type SettingsPatch struct {
	Transfers, Releases, ReleaseEmails *bool
}

// Store holds tokens, settings and the outbox.
type Store struct{ pool *pgxpool.Pool }

// NewStore creates the store.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Register stores a device's token for the user. A token that belonged to another
// account moves to this one (a phone can change accounts). Beyond MaxTokensPerUser
// devices the least recently used one is dropped.
func (s *Store) Register(ctx context.Context, userID, token, platform, deviceName string) error {
	token = strings.TrimSpace(token)
	if !tokenRe.MatchString(token) {
		return &ValidationError{"token must look like ExponentPushToken[...]"}
	}
	if platform != "ios" && platform != "android" {
		return &ValidationError{"platform must be ios or android"}
	}
	deviceName = strings.TrimSpace(deviceName)
	if utf8.RuneCountInString(deviceName) > 100 {
		deviceName = string([]rune(deviceName)[:100])
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO push_tokens (token, user_id, platform, device_name) VALUES ($1, $2, $3, $4)
		ON CONFLICT (token) DO UPDATE SET user_id = $2, platform = $3, device_name = $4, updated_at = now()`,
		token, userID, platform, deviceName); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM push_tokens WHERE user_id = $1 AND token IN (
			SELECT token FROM push_tokens WHERE user_id = $1 ORDER BY updated_at DESC, token OFFSET $2)`,
		userID, MaxTokensPerUser); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Remove forgets one of the user's tokens (at sign-out). Unknown tokens are fine.
func (s *Store) Remove(ctx context.Context, userID, token string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM push_tokens WHERE user_id = $1 AND token = $2`, userID, strings.TrimSpace(token))
	return err
}

// Settings returns the user's switches (all on by default).
func (s *Store) Settings(ctx context.Context, userID string) (Settings, error) {
	st := Settings{Transfers: true, Releases: true, ReleaseEmails: true}
	err := s.pool.QueryRow(ctx, `SELECT transfers, releases, release_emails FROM notification_settings WHERE user_id = $1`, userID).
		Scan(&st.Transfers, &st.Releases, &st.ReleaseEmails)
	if errors.Is(err, pgx.ErrNoRows) {
		return st, nil
	}
	return st, err
}

// SetSettings changes the switches that are not nil.
func (s *Store) SetSettings(ctx context.Context, userID string, p SettingsPatch) (Settings, error) {
	cur, err := s.Settings(ctx, userID)
	if err != nil {
		return cur, err
	}
	if p.Transfers != nil {
		cur.Transfers = *p.Transfers
	}
	if p.Releases != nil {
		cur.Releases = *p.Releases
	}
	if p.ReleaseEmails != nil {
		cur.ReleaseEmails = *p.ReleaseEmails
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO notification_settings (user_id, transfers, releases, release_emails) VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id) DO UPDATE SET transfers = $2, releases = $3, release_emails = $4`,
		userID, cur.Transfers, cur.Releases, cur.ReleaseEmails)
	return cur, err
}

// Enqueue queues a message for every device of the user, unless the user switched the
// category off or has no device. dedupeKey (optional) makes repeating the same event harmless.
// It returns how many device messages were queued.
func Enqueue(ctx context.Context, db DB, userID, category string, m Message, dedupeKey string) (int, error) {
	var on bool
	err := db.QueryRow(ctx, `
		SELECT CASE $2 WHEN 'transfers' THEN COALESCE(ns.transfers, true) ELSE COALESCE(ns.releases, true) END
		FROM users u LEFT JOIN notification_settings ns ON ns.user_id = u.id WHERE u.id = $1`, userID, category).Scan(&on)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !on) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	data := map[string]string{"type": m.Type, "id": m.ID}
	if m.URL != "" {
		data["url"] = m.URL
	}
	blob, _ := json.Marshal(data)
	var key *string
	if dedupeKey != "" {
		key = &dedupeKey
	}
	tag, err := db.Exec(ctx, `
		INSERT INTO push_messages (user_id, token, title, body, data, dedupe_key)
		SELECT $1, t.token, $2, $3, $4, $5 FROM push_tokens t WHERE t.user_id = $1
		ON CONFLICT DO NOTHING`, userID, m.Title, m.Body, blob, key)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// Queued is a message as the dev outbox shows it.
type Queued struct {
	ID        int64             `json:"id"`
	Token     string            `json:"token"`
	Title     string            `json:"title"`
	Body      string            `json:"body"`
	Data      map[string]string `json:"data"`
	Status    string            `json:"status"`
	Error     string            `json:"error,omitempty"`
	CreatedAt string            `json:"createdAt"`
}

// Outbox lists the user's latest messages (development tool).
func (s *Store) Outbox(ctx context.Context, userID string, limit int) ([]Queued, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, token, title, body, data, status, error, to_char(created_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM push_messages WHERE user_id = $1 ORDER BY id DESC LIMIT $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Queued{}
	for rows.Next() {
		var q Queued
		var blob []byte
		if err := rows.Scan(&q.ID, &q.Token, &q.Title, &q.Body, &blob, &q.Status, &q.Error, &q.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(blob, &q.Data)
		out = append(out, q)
	}
	return out, rows.Err()
}

// ClearOutbox deletes the user's messages (development tool).
func (s *Store) ClearOutbox(ctx context.Context, userID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM push_messages WHERE user_id = $1`, userID)
	return err
}
