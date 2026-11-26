package push

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	batchSize        = 100
	receiptDelay     = 15 * time.Minute
	receiptGiveUp    = 24 * time.Hour
	maxSendAttempts  = 5
	leaseFor         = 2 * time.Minute
	retentionPeriod  = 14 * 24 * time.Hour
	notRegisteredErr = "DeviceNotRegistered"
)

var retryDelays = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, time.Hour, 6 * time.Hour}

// Gateway is the part of Expo the sender uses.
type Gateway interface {
	Send(ctx context.Context, msgs []ExpoMessage) ([]Ticket, error)
	Receipts(ctx context.Context, ids []string) (map[string]Receipt, error)
}

// Sender empties the outbox: it sends pending messages in batches of up to 100,
// reads receipts about 15 minutes later and forgets tokens whose device is gone.
type Sender struct {
	pool *pgxpool.Pool
	gw   Gateway
	// Fake (development) sends nothing: messages are marked delivered and stay
	// readable in the outbox.
	Fake     bool
	Interval time.Duration
	// ReceiptDelay can be shortened in tests.
	ReceiptDelay time.Duration

	lastPurge time.Time
}

// NewSender creates the sender. gw may be nil when fake is true.
func NewSender(pool *pgxpool.Pool, gw Gateway, fake bool) *Sender {
	return &Sender{pool: pool, gw: gw, Fake: fake, Interval: 5 * time.Second, ReceiptDelay: receiptDelay}
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

// Tick does one round of sending, receipts and housekeeping. Exposed for tests.
func (s *Sender) Tick(ctx context.Context) {
	for {
		n, err := s.sendBatch(ctx)
		if err != nil {
			slog.Error("push send", "err", err)
			break
		}
		if n < batchSize {
			break
		}
	}
	if !s.Fake {
		if err := s.readReceipts(ctx); err != nil {
			slog.Error("push receipts", "err", err)
		}
	}
	if time.Since(s.lastPurge) > time.Hour {
		s.lastPurge = time.Now()
		if _, err := s.pool.Exec(ctx, `DELETE FROM push_messages WHERE created_at < now() - make_interval(secs => $1)`, retentionPeriod.Seconds()); err != nil {
			slog.Error("push purge", "err", err)
		}
	}
}

type pending struct {
	id    int64
	token string
	title string
	body  string
	data  map[string]string
	tries int
}

// claim takes up to batchSize due messages (and any whose sender died mid-send).
func (s *Sender) claim(ctx context.Context) ([]pending, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE push_messages SET status = 'sending', lease_until = now() + make_interval(secs => $1), attempts = attempts + 1
		WHERE id IN (
			SELECT id FROM push_messages
			WHERE (status = 'pending' AND next_attempt_at <= now()) OR (status = 'sending' AND lease_until < now())
			ORDER BY id FOR UPDATE SKIP LOCKED LIMIT $2)
		RETURNING id, token, title, body, data, attempts`, leaseFor.Seconds(), batchSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pending
	for rows.Next() {
		var p pending
		var blob []byte
		if err := rows.Scan(&p.id, &p.token, &p.title, &p.body, &blob, &p.tries); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(blob, &p.data)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Sender) sendBatch(ctx context.Context) (int, error) {
	batch, err := s.claim(ctx)
	if err != nil || len(batch) == 0 {
		return 0, err
	}
	if s.Fake {
		for _, p := range batch {
			if _, err := s.pool.Exec(ctx, `UPDATE push_messages SET status = 'delivered', sent_at = now(), lease_until = NULL WHERE id = $1`, p.id); err != nil {
				return len(batch), err
			}
		}
		return len(batch), nil
	}
	msgs := make([]ExpoMessage, len(batch))
	for i, p := range batch {
		msgs[i] = ExpoMessage{To: p.token, Title: p.title, Body: p.body, Sound: "default", ChannelID: "default", Priority: "high", Data: p.data}
	}
	tickets, err := s.gw.Send(ctx, msgs)
	if err != nil {
		var he *HTTPError
		retry := !errors.As(err, &he) || he.Retryable
		for _, p := range batch {
			if rerr := s.retryOrFail(ctx, p, err.Error(), retry); rerr != nil {
				return len(batch), rerr
			}
		}
		slog.Warn("push batch not sent", "size", len(batch), "err", err)
		return len(batch), nil
	}
	for i, p := range batch {
		t := tickets[i]
		switch {
		case t.OK:
			_, err = s.pool.Exec(ctx, `UPDATE push_messages SET status = 'sent', ticket_id = $2, sent_at = now(), lease_until = NULL,
				receipt_check_at = now() + make_interval(secs => $3) WHERE id = $1`, p.id, t.ID, s.ReceiptDelay.Seconds())
		case t.Error == notRegisteredErr:
			err = s.dropToken(ctx, p.id, p.token, "device is not registered")
		case t.Error == "MessageRateExceeded":
			err = s.retryOrFail(ctx, p, t.Error, true)
		default:
			_, err = s.pool.Exec(ctx, `UPDATE push_messages SET status = 'failed', error = $2, lease_until = NULL WHERE id = $1`, p.id, t.Error+": "+t.Message)
		}
		if err != nil {
			return len(batch), err
		}
	}
	return len(batch), nil
}

func (s *Sender) retryOrFail(ctx context.Context, p pending, cause string, retry bool) error {
	if !retry || p.tries >= maxSendAttempts {
		_, err := s.pool.Exec(ctx, `UPDATE push_messages SET status = 'failed', error = $2, lease_until = NULL WHERE id = $1`, p.id, cause)
		return err
	}
	d := retryDelays[min(p.tries-1, len(retryDelays)-1)]
	_, err := s.pool.Exec(ctx, `UPDATE push_messages SET status = 'pending', error = $2, lease_until = NULL,
		next_attempt_at = now() + make_interval(secs => $3) WHERE id = $1`, p.id, cause, d.Seconds())
	return err
}

// dropToken marks the message and deletes the token everywhere (the phone no longer has the app).
func (s *Sender) dropToken(ctx context.Context, id int64, token, why string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM push_tokens WHERE token = $1`, token); err != nil {
		return err
	}
	// its other queued messages have nowhere to go either
	_, err := s.pool.Exec(ctx, `UPDATE push_messages SET status = 'invalid_token', error = $3, lease_until = NULL
		WHERE id = $1 OR (token = $2 AND status IN ('pending', 'sending'))`, id, token, why)
	return err
}

// readReceipts asks Expo how the sent messages fared.
func (s *Sender) readReceipts(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `
		SELECT id, token, ticket_id, sent_at FROM push_messages
		WHERE status = 'sent' AND receipt_check_at <= now() ORDER BY receipt_check_at LIMIT 1000`)
	if err != nil {
		return err
	}
	type due struct {
		id     int64
		token  string
		ticket string
		sent   time.Time
	}
	var all []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.id, &d.token, &d.ticket, &d.sent); err != nil {
			rows.Close()
			return err
		}
		all = append(all, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for start := 0; start < len(all); start += 300 {
		chunk := all[start:min(start+300, len(all))]
		ids := make([]string, len(chunk))
		for i, d := range chunk {
			ids[i] = d.ticket
		}
		receipts, err := s.gw.Receipts(ctx, ids)
		if err != nil {
			return err
		}
		for _, d := range chunk {
			r, ok := receipts[d.ticket]
			switch {
			case !ok && time.Since(d.sent) > receiptGiveUp:
				_, err = s.pool.Exec(ctx, `UPDATE push_messages SET status = 'expired', error = 'no receipt within a day' WHERE id = $1`, d.id)
			case !ok: // not ready yet: look again later
				_, err = s.pool.Exec(ctx, `UPDATE push_messages SET receipt_check_at = now() + make_interval(secs => $2) WHERE id = $1`, d.id, s.ReceiptDelay.Seconds())
			case r.OK:
				_, err = s.pool.Exec(ctx, `UPDATE push_messages SET status = 'delivered' WHERE id = $1`, d.id)
			case r.Error == notRegisteredErr:
				err = s.dropToken(ctx, d.id, d.token, "device is not registered")
			default:
				slog.Warn("push receipt error", "error", r.Error, "message", r.Message)
				_, err = s.pool.Exec(ctx, `UPDATE push_messages SET status = 'failed', error = $2 WHERE id = $1`, d.id, r.Error+": "+r.Message)
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// Plural helps the notification texts: "1 song", "9 songs".
func Plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}
