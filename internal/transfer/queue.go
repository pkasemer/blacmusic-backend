package transfer

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"time"

	"github.com/jackc/pgx/v5"
)

// backoff is the wait before the next attempt, indexed by the attempts used so far.
var backoff = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, time.Hour, 6 * time.Hour}

// nextDelay is the retry delay after `attempt` attempts, with up to 10% jitter.
func nextDelay(attempt int) time.Duration {
	i := attempt - 1
	if i < 0 {
		i = 0
	}
	if i >= len(backoff) {
		i = len(backoff) - 1
	}
	d := backoff[i]
	return d + time.Duration(rand.Int63n(int64(d)/10+1))
}

const claimLock = 0x626c6163 // advisory lock key serialising claims ("blac")

// Reclaim puts transfers whose worker stopped (lease expired) back in the
// queue, and fails those that have used all their attempts.
func (m *Manager) Reclaim(ctx context.Context) error {
	if _, err := m.pool.Exec(ctx, `
		UPDATE transfers SET status = 'retrying', next_attempt_at = now(), lease_until = NULL,
		       error = 'the worker stopped; resuming', updated_at = now()
		WHERE status = 'in_progress' AND lease_until < now()`); err != nil {
		return err
	}
	rows, err := m.pool.Query(ctx, `
		UPDATE transfers SET status = 'failed', reason = 'attempts_exhausted', finished_at = now(), updated_at = now()
		WHERE status = 'retrying' AND attempt >= max_attempts RETURNING id`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		m.ended(ctx, id)
	}
	return rows.Err()
}

// Claim starts the oldest waiting transfer if fewer than limit are running. It
// returns "" when there is nothing to do.
func (m *Manager) Claim(ctx context.Context, worker string, limit int, lease time.Duration) (string, error) {
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, claimLock); err != nil {
		return "", err
	}
	var running int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM transfers WHERE status = 'in_progress' AND lease_until > now()`).Scan(&running); err != nil {
		return "", err
	}
	if running >= limit {
		return "", nil
	}
	var id string
	err = tx.QueryRow(ctx, `
		UPDATE transfers SET status = 'in_progress', attempt = attempt + 1, started_at = COALESCE(started_at, now()),
		       lease_until = now() + make_interval(secs => $1), worker_id = $2, error = '', updated_at = now()
		WHERE id = (SELECT id FROM transfers WHERE status IN ('queued', 'retrying') AND next_attempt_at <= now()
		            ORDER BY submitted_at, id FOR UPDATE SKIP LOCKED LIMIT 1)
		RETURNING id`, lease.Seconds(), worker).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return id, tx.Commit(ctx)
}

// Running lists the transfers this worker is working on.
func (m *Manager) Running(ctx context.Context, worker string) ([]string, error) {
	rows, err := m.pool.Query(ctx, `SELECT id FROM transfers WHERE status = 'in_progress' AND worker_id = $1 ORDER BY submitted_at, id`, worker)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// Extend renews the lease. It reports false when the transfer is no longer ours
// (cancelled, or taken over after a long stall).
func (m *Manager) Extend(ctx context.Context, id, worker string, lease time.Duration) (bool, error) {
	tag, err := m.pool.Exec(ctx, `UPDATE transfers SET lease_until = now() + make_interval(secs => $3), updated_at = now()
		WHERE id = $1 AND worker_id = $2 AND status = 'in_progress'`, id, worker, lease.Seconds())
	return tag.RowsAffected() == 1, err
}

// Fail ends a transfer for good.
func (m *Manager) fail(ctx context.Context, id, reason, msg string) error {
	tag, err := m.pool.Exec(ctx, `UPDATE transfers SET status = 'failed', reason = $2, error = $3, lease_until = NULL, finished_at = now(), updated_at = now()
		WHERE id = $1 AND status = 'in_progress'`, id, reason, msg)
	if err == nil && tag.RowsAffected() == 1 {
		m.ended(ctx, id)
	}
	return err
}

func (m *Manager) complete(ctx context.Context, id string) error {
	tag, err := m.pool.Exec(ctx, `UPDATE transfers SET status = 'completed', lease_until = NULL, error = '', finished_at = now(), updated_at = now()
		WHERE id = $1 AND status = 'in_progress'`, id)
	if err == nil && tag.RowsAffected() == 1 {
		m.ended(ctx, id)
	}
	return err
}

// Ended describes a transfer that just reached completed or failed.
type Ended struct {
	TransferID string
	UserID     string
	Status     string // completed | failed
	Reason     string // items_failed | attempts_exhausted | ""
	Done       int    // songs published
	Failed     int    // songs that failed
}

// ended tells OnEnd (the notification hook). A failing hook never affects the transfer.
func (m *Manager) ended(ctx context.Context, id string) {
	if m.OnEnd == nil {
		return
	}
	t, err := m.loadByID(ctx, id)
	if err != nil {
		slog.Error("transfer end hook: load", "id", id, "err", err)
		return
	}
	e := Ended{TransferID: id, UserID: t.UserID, Status: t.Status, Reason: t.Reason}
	for _, it := range t.Items {
		switch it.Status {
		case ItemDone:
			e.Done++
		case ItemFailed:
			e.Failed++
		}
	}
	if err := m.OnEnd(ctx, e); err != nil {
		slog.Error("transfer end hook", "id", id, "err", err)
	}
}

// Retrying schedules another attempt after a transient failure, or fails the
// transfer when the attempts are used up.
func (m *Manager) retrying(ctx context.Context, id string, attempt, max int, cause error) error {
	if attempt >= max {
		return m.fail(ctx, id, "attempts_exhausted", cause.Error())
	}
	d := nextDelay(attempt)
	_, err := m.pool.Exec(ctx, `UPDATE transfers SET status = 'retrying', next_attempt_at = now() + make_interval(secs => $2),
		lease_until = NULL, error = $3, updated_at = now() WHERE id = $1 AND status = 'in_progress'`, id, d.Seconds(), cause.Error())
	return err
}

// pauseForAudio hands a running transfer back to the artist: songs could not be fetched and need a file.
// The transfer returns to collecting (the artist attaches files and submits again).
func (m *Manager) pauseForAudio(ctx context.Context, id string, songs int) error {
	var userID string
	err := m.pool.QueryRow(ctx, `UPDATE transfers SET status = 'collecting', lease_until = NULL, error = '', updated_at = now()
		WHERE id = $1 AND status = 'in_progress' RETURNING user_id::text`, id).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if m.OnNeedsAudio != nil {
		if herr := m.OnNeedsAudio(ctx, NeedsAudio{TransferID: id, UserID: userID, Songs: songs}); herr != nil {
			slog.Error("transfer needs-audio hook", "id", id, "err", herr)
		}
	}
	return nil
}

func (m *Manager) loadByID(ctx context.Context, id string) (*Transfer, error) {
	t, err := scanTransfer(m.pool.QueryRow(ctx, `SELECT `+transferCols+` FROM transfers t WHERE t.id = $1`, id))
	if err != nil {
		return nil, err
	}
	t.Items, err = m.loadItems(ctx, id)
	return t, err
}
