package mailer_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/mailer"
	"github.com/blacmusic/backend/internal/testutil"
)

// sendAttempts is how many times a message is tried before it is given up on.
const sendAttempts = 5

type recorder struct {
	sent []string // "to|subject"
	fail error
}

func (r *recorder) Send(_ context.Context, to, subject, _ string) error {
	if r.fail != nil {
		return r.fail
	}
	r.sent = append(r.sent, to+"|"+subject)
	return nil
}

func row(t *testing.T, pool *pgxpool.Pool, user, subject string) (status string, attempts int, errText string) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `SELECT status, attempts, error FROM email_messages WHERE user_id = $1 AND subject = $2`, user, subject).
		Scan(&status, &attempts, &errText)
	if err != nil {
		t.Fatal(err)
	}
	return
}

func userEmail(t *testing.T, pool *pgxpool.Pool, user string) string {
	t.Helper()
	var e string
	if err := pool.QueryRow(context.Background(), `SELECT email FROM users WHERE id = $1`, user).Scan(&e); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestReleaseEmailsRespectTheSwitchAndTheVerifiedAddress(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()
	user := testutil.User(t, pool, "user", true)

	if ok, err := mailer.EnqueueRelease(ctx, pool, user, "one", "body", "k1"); err != nil || !ok {
		t.Fatalf("first = %v %v", ok, err)
	}
	if ok, _ := mailer.EnqueueRelease(ctx, pool, user, "one", "body", "k1"); ok {
		t.Fatal("the same key queued twice")
	}
	if ok, _ := mailer.EnqueueRelease(ctx, pool, user, "two", "body", ""); !ok {
		t.Fatal("a message without a key is always queued")
	}
	pool.Exec(ctx, `INSERT INTO notification_settings (user_id, release_emails) VALUES ($1, false)`, user) //nolint:errcheck
	if ok, _ := mailer.EnqueueRelease(ctx, pool, user, "three", "body", ""); ok {
		t.Fatal("queued although release emails are off")
	}
	unverified := testutil.User(t, pool, "user", false)
	if ok, _ := mailer.EnqueueRelease(ctx, pool, unverified, "four", "body", ""); ok {
		t.Fatal("queued for an unverified address")
	}
	if ok, _ := mailer.EnqueueRelease(ctx, pool, "00000000-0000-0000-0000-000000000000", "five", "body", ""); ok {
		t.Fatal("queued for an unknown user")
	}
}

func TestSenderDeliversAndRetries(t *testing.T) {
	pool := testutil.Pool(t)
	testutil.SerializeOutbox(t, pool)
	ctx := context.Background()
	pool.Exec(ctx, `DELETE FROM email_messages`) //nolint:errcheck
	user := testutil.User(t, pool, "user", true)
	to := userEmail(t, pool, user)
	mailer.EnqueueRelease(ctx, pool, user, "hello", "body", "") //nolint:errcheck

	// a failing provider: the message waits and is tried again later
	rec := &recorder{fail: errors.New("provider down")}
	s := mailer.NewSender(pool, rec)
	s.Tick(ctx)
	if st, n, msg := row(t, pool, user, "hello"); st != "pending" || n != 1 || msg != "provider down" {
		t.Fatalf("after a failed send = %s, %d attempts, %q", st, n, msg)
	}
	s.Tick(ctx)
	if _, n, _ := row(t, pool, user, "hello"); n != 1 {
		t.Fatalf("retried at once (%d attempts); it must wait", n)
	}

	// the provider recovers and the retry time has come
	pool.Exec(ctx, `UPDATE email_messages SET next_attempt_at = now() WHERE user_id = $1`, user) //nolint:errcheck
	rec.fail = nil
	s.Tick(ctx)
	if st, n, _ := row(t, pool, user, "hello"); st != "sent" || n != 2 {
		t.Fatalf("after recovering = %s, %d attempts", st, n)
	}
	if len(rec.sent) != 1 || rec.sent[0] != to+"|hello" {
		t.Fatalf("sent = %v", rec.sent)
	}
	s.Tick(ctx)
	if len(rec.sent) != 1 {
		t.Fatal("a sent email was sent again")
	}
}

func TestSenderGivesUpAndSkipsAddressesThatAreNoLongerVerified(t *testing.T) {
	pool := testutil.Pool(t)
	testutil.SerializeOutbox(t, pool)
	ctx := context.Background()
	pool.Exec(ctx, `DELETE FROM email_messages`) //nolint:errcheck
	user := testutil.User(t, pool, "user", true)
	mailer.EnqueueRelease(ctx, pool, user, "doomed", "body", "") //nolint:errcheck
	rec := &recorder{fail: errors.New("rejected")}
	s := mailer.NewSender(pool, rec)
	for i := 0; i < sendAttempts; i++ {
		pool.Exec(ctx, `UPDATE email_messages SET next_attempt_at = now() WHERE user_id = $1`, user) //nolint:errcheck
		s.Tick(ctx)
	}
	if st, n, _ := row(t, pool, user, "doomed"); st != "failed" || n != sendAttempts {
		t.Fatalf("after %d refusals = %s, %d attempts", sendAttempts, st, n)
	}

	other := testutil.User(t, pool, "user", true)
	mailer.EnqueueRelease(ctx, pool, other, "orphan", "body", "")                    //nolint:errcheck
	pool.Exec(ctx, `UPDATE users SET email_verified_at = NULL WHERE id = $1`, other) //nolint:errcheck
	rec.fail = nil
	s.Tick(ctx)
	if st, _, msg := row(t, pool, other, "orphan"); st != "failed" || msg != "no verified address" || len(rec.sent) != 0 {
		t.Fatalf("unverified at send time = %s %q, sent %v", st, msg, rec.sent)
	}
}
