package mailer

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DB is what Enqueue needs; a pool and a transaction both satisfy it, so an event can queue its
// email in the same transaction that caused it.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// EnqueueRelease queues an email about one of the user's releases, unless they switched release
// emails off or have no verified address. dedupeKey (optional) makes repeating the same event
// harmless. It reports whether a message was queued.
func EnqueueRelease(ctx context.Context, db DB, userID, subject, body, dedupeKey string) (bool, error) {
	var on bool
	err := db.QueryRow(ctx, `
		SELECT COALESCE(ns.release_emails, true) AND u.email_verified_at IS NOT NULL
		FROM users u LEFT JOIN notification_settings ns ON ns.user_id = u.id WHERE u.id = $1`, userID).Scan(&on)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !on) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var key *string
	if dedupeKey != "" {
		key = &dedupeKey
	}
	tag, err := db.Exec(ctx, `INSERT INTO email_messages (user_id, subject, body, dedupe_key) VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`,
		userID, subject, body, key)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

const (
	emailBatch     = 20
	emailLease     = 2 * time.Minute
	emailAttempts  = 5
	emailRetention = 30 * 24 * time.Hour
)

var emailRetry = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 3 * time.Hour}

// Sender empties the email outbox through a Mailer: failures are retried with growing pauses.
type Sender struct {
	pool     *pgxpool.Pool
	mail     Mailer
	Interval time.Duration

	lastPurge time.Time
}

// NewSender creates the sender.
func NewSender(pool *pgxpool.Pool, m Mailer) *Sender {
	return &Sender{pool: pool, mail: m, Interval: 5 * time.Second}
}

// Run loops until ctx ends.
func (s *Sender) Run(ctx context.Context) {
	t := time.NewTicker(s.Interval)
	defer t.Stop()
	for {
		s.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

type queued struct {
	id      int64
	to      string
	subject string
	body    string
	tries   int
}

// Tick sends what is due. Exposed for tests.
func (s *Sender) Tick(ctx context.Context) {
	for {
		batch, err := s.claim(ctx)
		if err != nil {
			slog.Error("email claim", "err", err)
			return
		}
		for _, q := range batch {
			s.deliver(ctx, q)
		}
		if len(batch) < emailBatch {
			break
		}
	}
	if time.Since(s.lastPurge) > time.Hour {
		s.lastPurge = time.Now()
		if _, err := s.pool.Exec(ctx, `DELETE FROM email_messages WHERE created_at < now() - make_interval(secs => $1)`, emailRetention.Seconds()); err != nil {
			slog.Error("email purge", "err", err)
		}
	}
}

// claim takes due messages (and any whose sender died mid-send). A message whose user is gone or has
// no verified address is dropped instead of sent.
func (s *Sender) claim(ctx context.Context) ([]queued, error) {
	rows, err := s.pool.Query(ctx, `
		WITH due AS (
			SELECT m.id FROM email_messages m
			WHERE (m.status = 'pending' AND m.next_attempt_at <= now()) OR (m.status = 'sending' AND m.lease_until < now())
			ORDER BY m.id FOR UPDATE SKIP LOCKED LIMIT $2)
		UPDATE email_messages m SET status = 'sending', lease_until = now() + make_interval(secs => $1), attempts = m.attempts + 1
		FROM due, users u
		WHERE m.id = due.id AND u.id = m.user_id
		RETURNING m.id, COALESCE(CASE WHEN u.email_verified_at IS NOT NULL THEN u.email END, ''), m.subject, m.body, m.attempts`,
		emailLease.Seconds(), emailBatch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []queued
	for rows.Next() {
		var q queued
		if err := rows.Scan(&q.id, &q.to, &q.subject, &q.body, &q.tries); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

func (s *Sender) deliver(ctx context.Context, q queued) {
	if q.to == "" {
		s.mark(ctx, q.id, `UPDATE email_messages SET status = 'failed', error = 'no verified address', lease_until = NULL WHERE id = $1`)
		return
	}
	if err := s.mail.Send(ctx, q.to, q.subject, q.body); err != nil {
		slog.Warn("email not sent", "id", q.id, "attempt", q.tries, "err", err)
		if q.tries >= emailAttempts {
			s.mark(ctx, q.id, `UPDATE email_messages SET status = 'failed', error = $2, lease_until = NULL WHERE id = $1`, err.Error())
			return
		}
		d := emailRetry[min(q.tries-1, len(emailRetry)-1)]
		s.mark(ctx, q.id, `UPDATE email_messages SET status = 'pending', error = $2, lease_until = NULL,
			next_attempt_at = now() + make_interval(secs => $3) WHERE id = $1`, err.Error(), d.Seconds())
		return
	}
	s.mark(ctx, q.id, `UPDATE email_messages SET status = 'sent', sent_at = now(), error = '', lease_until = NULL WHERE id = $1`)
}

func (s *Sender) mark(ctx context.Context, id int64, sql string, args ...any) {
	if _, err := s.pool.Exec(ctx, sql, append([]any{id}, args...)...); err != nil {
		slog.Error("email status", "id", id, "err", err)
	}
}
