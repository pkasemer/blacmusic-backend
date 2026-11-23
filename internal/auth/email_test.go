package auth_test

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/testutil"
)

// fakeMailer records what would have been emailed.
type fakeMailer struct {
	mu   sync.Mutex
	sent []string
	err  error
}

func (m *fakeMailer) Send(_ context.Context, to, subject, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.sent = append(m.sent, body)
	return nil
}

var codeRe = regexp.MustCompile(`\b(\d{6})\b`)

func (m *fakeMailer) lastCode(t *testing.T) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sent) == 0 {
		t.Fatal("no email was sent")
	}
	c := codeRe.FindString(m.sent[len(m.sent)-1])
	if c == "" {
		t.Fatalf("no code in %q", m.sent[len(m.sent)-1])
	}
	return c
}

func (m *fakeMailer) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent)
}

func setupEmail(t *testing.T) (*auth.Service, *fakeMailer, string, func(string, string)) {
	t.Helper()
	pool := testutil.Pool(t)
	m := &fakeMailer{}
	svc := auth.NewService(pool, auth.NewTokenService(testutil.Secret, time.Minute), time.Hour)
	svc.SetMailer(m)
	uid := testutil.User(t, pool, "user", false)
	exec := func(sql, arg string) {
		if _, err := pool.Exec(context.Background(), sql, arg); err != nil {
			t.Fatal(err)
		}
	}
	return svc, m, uid, exec
}

func TestEmailVerificationHappyPath(t *testing.T) {
	svc, m, uid, _ := setupEmail(t)
	ctx := context.Background()

	already, err := svc.SendEmailCode(ctx, uid)
	if err != nil || already {
		t.Fatalf("send: already=%v err=%v", already, err)
	}
	if u, _ := svc.Me(ctx, uid); u.EmailVerified {
		t.Fatal("must not be verified before the code is entered")
	}
	if err := svc.VerifyEmailCode(ctx, uid, m.lastCode(t)); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if u, _ := svc.Me(ctx, uid); !u.EmailVerified {
		t.Fatal("Me must report emailVerified after the code is accepted")
	}
	// A used code is gone, and asking again for a verified account sends nothing.
	if already, err = svc.SendEmailCode(ctx, uid); err != nil || !already || m.count() != 1 {
		t.Fatalf("after verifying: already=%v err=%v mails=%d", already, err, m.count())
	}
	if err := svc.VerifyEmailCode(ctx, uid, "123456"); err != nil {
		t.Fatalf("a verified account treats any later verify as success, got %v", err)
	}
}

func TestWrongCodesLockTheCode(t *testing.T) {
	svc, m, uid, _ := setupEmail(t)
	ctx := context.Background()
	if _, err := svc.SendEmailCode(ctx, uid); err != nil {
		t.Fatal(err)
	}
	right := m.lastCode(t)
	wrong := "000000"
	if right == wrong {
		wrong = "111111"
	}
	for i := 0; i < 5; i++ {
		if err := svc.VerifyEmailCode(ctx, uid, wrong); !errors.Is(err, auth.ErrInvalidCode) {
			t.Fatalf("wrong guess %d = %v, want ErrInvalidCode", i, err)
		}
	}
	// Five wrong guesses burn the code: even the right one no longer works.
	if err := svc.VerifyEmailCode(ctx, uid, right); !errors.Is(err, auth.ErrInvalidCode) {
		t.Fatalf("right code after lockout = %v, want ErrInvalidCode", err)
	}
	if u, _ := svc.Me(ctx, uid); u.EmailVerified {
		t.Fatal("must stay unverified")
	}
}

func TestResendCooldownAndNewCodeReplacesOld(t *testing.T) {
	svc, m, uid, _ := setupEmail(t)
	ctx := context.Background()
	pool := testutil.Pool(t)
	if _, err := svc.SendEmailCode(ctx, uid); err != nil {
		t.Fatal(err)
	}
	first := m.lastCode(t)

	var soon *auth.TooSoonError
	if _, err := svc.SendEmailCode(ctx, uid); !errors.As(err, &soon) || soon.RetryAfter <= 0 {
		t.Fatalf("immediate resend = %v, want TooSoonError", err)
	}
	if m.count() != 1 {
		t.Fatal("a refused resend must not send mail")
	}

	// After the cooldown a new code replaces the old one.
	if _, err := pool.Exec(ctx, `UPDATE email_codes SET sent_at = now() - interval '2 minutes' WHERE user_id::text = $1`, uid); err != nil {
		t.Fatal(err)
	}
	for tries := 0; tries < 20; tries++ { // a fresh code that differs from the first
		if _, err := svc.SendEmailCode(ctx, uid); err != nil {
			t.Fatal(err)
		}
		if m.lastCode(t) != first {
			break
		}
		pool.Exec(ctx, `UPDATE email_codes SET sent_at = now() - interval '2 minutes' WHERE user_id::text = $1`, uid) //nolint:errcheck
	}
	second := m.lastCode(t)
	if second == first {
		t.Skip("random codes collided; extremely unlikely")
	}
	if err := svc.VerifyEmailCode(ctx, uid, first); !errors.Is(err, auth.ErrInvalidCode) {
		t.Fatalf("old code = %v, want ErrInvalidCode", err)
	}
	if err := svc.VerifyEmailCode(ctx, uid, second); err != nil {
		t.Fatalf("new code: %v", err)
	}
}

func TestExpiredAndMalformedCodes(t *testing.T) {
	svc, m, uid, _ := setupEmail(t)
	ctx := context.Background()
	pool := testutil.Pool(t)
	if _, err := svc.SendEmailCode(ctx, uid); err != nil {
		t.Fatal(err)
	}
	code := m.lastCode(t)
	for _, bad := range []string{"", "12345", "1234567", "12345a", " 12345", "١٢٣٤٥٦"} {
		if err := svc.VerifyEmailCode(ctx, uid, bad); !errors.Is(err, auth.ErrInvalidCode) {
			t.Errorf("%q = %v, want ErrInvalidCode", bad, err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE email_codes SET expires_at = now() - interval '1 second' WHERE user_id::text = $1`, uid); err != nil {
		t.Fatal(err)
	}
	if err := svc.VerifyEmailCode(ctx, uid, code); !errors.Is(err, auth.ErrInvalidCode) {
		t.Fatalf("expired = %v, want ErrInvalidCode", err)
	}
}

func TestMailFailureLetsYouRetryImmediately(t *testing.T) {
	svc, m, uid, _ := setupEmail(t)
	ctx := context.Background()
	m.err = fmt.Errorf("smtp down")
	if _, err := svc.SendEmailCode(ctx, uid); !errors.Is(err, auth.ErrMailFailed) {
		t.Fatalf("err = %v, want ErrMailFailed", err)
	}
	m.err = nil
	if _, err := svc.SendEmailCode(ctx, uid); err != nil { // no cooldown after a failed send
		t.Fatalf("retry after mail failure: %v", err)
	}
}
