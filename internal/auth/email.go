package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/blacmusic/backend/internal/httpx"
)

// Email verification uses a 6-digit code typed into the app: no website or deep
// link is needed. A code lives 30 minutes, allows 5 wrong guesses, and a new
// one can be requested once a minute.
const (
	codeTTL        = 30 * time.Minute
	resendCooldown = time.Minute
	maxCodeTries   = 5
)

var (
	ErrInvalidCode = errors.New("that code is not valid or has expired")
	ErrMailFailed  = errors.New("the verification email could not be sent, please try again")
)

// TooSoonError is returned when a code was sent too recently.
type TooSoonError struct{ RetryAfter time.Duration }

func (e *TooSoonError) Error() string { return "a code was sent recently" }

func hashCode(userID, code string) string {
	sum := sha256.Sum256([]byte(userID + ":" + code))
	return hex.EncodeToString(sum[:])
}

// SendEmailCode emails the user a fresh code. alreadyVerified is true (and
// nothing is sent) when the address is already confirmed.
func (s *Service) SendEmailCode(ctx context.Context, userID string) (alreadyVerified bool, err error) {
	var email string
	var verified bool
	err = s.pool.QueryRow(ctx, `SELECT email, email_verified_at IS NOT NULL FROM users WHERE id = $1`, userID).Scan(&email, &verified)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrInvalidToken
	}
	if err != nil {
		return false, err
	}
	if verified {
		return true, nil
	}

	var sentAt time.Time
	switch err := s.pool.QueryRow(ctx, `SELECT sent_at FROM email_codes WHERE user_id = $1`, userID).Scan(&sentAt); {
	case err == nil:
		if wait := resendCooldown - time.Since(sentAt); wait > 0 {
			return false, &TooSoonError{RetryAfter: wait}
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return false, err
	}

	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return false, err
	}
	code := fmt.Sprintf("%06d", n.Int64())
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO email_codes (user_id, code_hash, expires_at, attempts, sent_at)
		VALUES ($1, $2, now() + make_interval(secs => $3), 0, now())
		ON CONFLICT (user_id) DO UPDATE
		SET code_hash = EXCLUDED.code_hash, expires_at = EXCLUDED.expires_at, attempts = 0, sent_at = now()`,
		userID, hashCode(userID, code), codeTTL.Seconds()); err != nil {
		return false, err
	}

	body := fmt.Sprintf("Your Blac Music verification code is %s\n\nIt expires in %d minutes. "+
		"If you did not ask for it, you can ignore this email.", code, int(codeTTL.Minutes()))
	if s.mailer == nil {
		return false, errors.New("auth: no mailer configured")
	}
	if err := s.mailer.Send(ctx, email, "Your Blac Music verification code", body); err != nil {
		_, _ = s.pool.Exec(ctx, `DELETE FROM email_codes WHERE user_id = $1`, userID) // let them retry at once
		return false, fmt.Errorf("%w: %v", ErrMailFailed, err)
	}
	return false, nil
}

// VerifyEmailCode checks the code and marks the address verified.
func (s *Service) VerifyEmailCode(ctx context.Context, userID, code string) error {
	if len(code) != 6 {
		return ErrInvalidCode
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			return ErrInvalidCode
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var hash string
	var expires time.Time
	var attempts int
	err = tx.QueryRow(ctx, `SELECT code_hash, expires_at, attempts FROM email_codes WHERE user_id = $1 FOR UPDATE`, userID).
		Scan(&hash, &expires, &attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		// Already verified (or never asked): treat a verified account as success.
		var verified bool
		if qerr := tx.QueryRow(ctx, `SELECT email_verified_at IS NOT NULL FROM users WHERE id = $1`, userID).Scan(&verified); qerr == nil && verified {
			return nil
		}
		return ErrInvalidCode
	}
	if err != nil {
		return err
	}
	if attempts >= maxCodeTries || time.Now().After(expires) {
		return ErrInvalidCode
	}
	if subtle.ConstantTimeCompare([]byte(hash), []byte(hashCode(userID, code))) != 1 {
		if _, err := tx.Exec(ctx, `UPDATE email_codes SET attempts = attempts + 1 WHERE user_id = $1`, userID); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil { // keep the failed-attempt count
			return err
		}
		return ErrInvalidCode
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET email_verified_at = now() WHERE id = $1 AND email_verified_at IS NULL`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM email_codes WHERE user_id = $1`, userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SendEmailCode handles POST /v1/auth/email/send-code.
func (h *Handler) SendEmailCode(w http.ResponseWriter, r *http.Request) {
	id, _ := UserID(r.Context())
	already, err := h.svc.SendEmailCode(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return
	}
	if already {
		httpx.JSON(w, http.StatusOK, map[string]any{"emailVerified": true})
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"sent": true, "expiresInSeconds": int(codeTTL.Seconds()), "resendAfterSeconds": int(resendCooldown.Seconds()),
	})
}

// VerifyEmail handles POST /v1/auth/email/verify {code}.
func (h *Handler) VerifyEmail(w http.ResponseWriter, r *http.Request) {
	id, _ := UserID(r.Context())
	var req struct {
		Code string `json:"code"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if err := h.svc.VerifyEmailCode(r.Context(), id, req.Code); err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"emailVerified": true})
}
