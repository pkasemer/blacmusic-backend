package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/blacmusic/backend/internal/identity"
)

var (
	// ErrEmailConflict: the provider's email matches an existing account but the provider
	// does not vouch for it, so the identity cannot be linked.
	ErrEmailConflict = errors.New("an account with this email exists, and the provider has not verified the address")
	// ErrNoEmail: the provider sent no email address, which an account needs.
	ErrNoEmail = errors.New("the provider did not share an email address")
)

// noPassword is stored for accounts that only sign in through a provider. It is
// not a valid bcrypt hash, so no password ever matches it.
const noPassword = "!"

const (
	googleStateTTL = 10 * time.Minute
	loginCodeTTL   = 60 * time.Second
)

// SocialConfig is what sign-in with Apple/Google needs. Either provider may be nil.
type SocialConfig struct {
	Apple       *identity.Apple
	Google      identity.GoogleProvider
	AppRedirect string // where the browser goes after Google: blacmusic://auth/google
}

// SetSocial enables sign-in with Apple and/or Google.
func (s *Service) SetSocial(c SocialConfig) { s.social = c }

// SocialResult is the outcome of a provider sign-in.
type SocialResult struct {
	User  *User
	Pair  *TokenPair
	IsNew bool // an account was created
	// Linked: the provider identity was attached to an account that already existed.
	Linked bool
}

// SignInApple verifies an Apple identity token and signs the user in.
func (s *Service) SignInApple(ctx context.Context, identityToken, nonce, givenName, familyName string) (*SocialResult, error) {
	if s.social.Apple == nil {
		return nil, identity.ErrUnavailable
	}
	claims, err := s.social.Apple.Verify(ctx, identityToken, nonce)
	if err != nil {
		return nil, err
	}
	return s.SocialSignIn(ctx, claims, strings.TrimSpace(givenName+" "+familyName))
}

// SocialSignIn finds, links or creates the account for a provider identity.
//
//   - A known identity signs in to its account.
//   - Otherwise, if the provider VERIFIED the email and an account has it, the identity is
//     linked to that account. If that account's own email was never verified by us, whoever
//     registered it may not own the address, so its password is removed and its sessions
//     are ended: only the provider's proof counts from here on.
//   - A provider email that is NOT verified is never linked (ErrEmailConflict when an account exists).
//   - Otherwise a new account is created, verified when the provider verified the email.
func (s *Service) SocialSignIn(ctx context.Context, c *identity.Claims, nameHint string) (*SocialResult, error) {
	var res *SocialResult
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		res, err = s.socialOnce(ctx, c, nameHint)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // lost a race to create the same email/identity: look again
			continue
		}
		break
	}
	if err != nil {
		return nil, err
	}
	var banned bool
	if err := s.pool.QueryRow(ctx, `SELECT banned_at IS NOT NULL FROM users WHERE id = $1`, res.User.ID).Scan(&banned); err != nil {
		return nil, err
	}
	if banned {
		return nil, ErrAccountBanned
	}
	if res.User, err = s.Me(ctx, res.User.ID); err != nil {
		return nil, err
	}
	if res.Pair, err = s.issue(ctx, res.User.ID); err != nil {
		return nil, err
	}
	return res, nil
}

func (s *Service) socialOnce(ctx context.Context, c *identity.Claims, nameHint string) (*SocialResult, error) {
	email := identity.Normalise(c.Email)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	res := &SocialResult{User: &User{}}
	err = tx.QueryRow(ctx, `SELECT user_id::text FROM auth_identities WHERE provider = $1 AND subject = $2`, c.Provider, c.Subject).Scan(&res.User.ID)
	switch {
	case err == nil:
		if c.EmailVerified && email != "" {
			if _, err := tx.Exec(ctx, `UPDATE users SET email_verified_at = COALESCE(email_verified_at, now()) WHERE id = $1 AND email = $2`, res.User.ID, email); err != nil {
				return nil, err
			}
		}
	case errors.Is(err, pgx.ErrNoRows):
		if email == "" {
			return nil, ErrNoEmail
		}
		var verifiedByUs bool
		err = tx.QueryRow(ctx, `SELECT id::text, email_verified_at IS NOT NULL FROM users WHERE email = $1 FOR UPDATE`, email).Scan(&res.User.ID, &verifiedByUs)
		switch {
		case err == nil:
			if !c.EmailVerified {
				return nil, ErrEmailConflict
			}
			if !verifiedByUs {
				if _, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2, email_verified_at = now() WHERE id = $1`, res.User.ID, noPassword); err != nil {
					return nil, err
				}
				if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, res.User.ID); err != nil {
					return nil, err
				}
			}
			res.Linked = true
		case errors.Is(err, pgx.ErrNoRows):
			err = tx.QueryRow(ctx, `
				INSERT INTO users (email, password_hash, display_name, email_verified_at)
				VALUES ($1, $2, $3, CASE WHEN $4 THEN now() END) RETURNING id::text`,
				email, noPassword, displayNameFor(nameHint, c.Name, email), c.EmailVerified).Scan(&res.User.ID)
			if err != nil {
				return nil, err
			}
			res.IsNew = true
		default:
			return nil, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO auth_identities (provider, subject, user_id, email) VALUES ($1, $2, $3, $4)`,
			c.Provider, c.Subject, res.User.ID, email); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	return res, tx.Commit(ctx)
}

// displayNameFor picks the first non-empty of the app-supplied name, the provider's name and the email's local part.
func displayNameFor(candidates ...string) string {
	email := candidates[len(candidates)-1]
	for _, n := range candidates[:len(candidates)-1] {
		if n = strings.TrimSpace(n); n != "" {
			return cut(n)
		}
	}
	if i := strings.IndexByte(email, '@'); i > 0 {
		return cut(email[:i])
	}
	return cut(email)
}

func cut(s string) string {
	if utf8.RuneCountInString(s) > 50 {
		return string([]rune(s)[:50])
	}
	return s
}

// ---------------------------------------------------------------- Google flow

func randomString() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// GoogleStart records a started sign-in and returns the URL for the in-app browser.
func (s *Service) GoogleStart(ctx context.Context, loginHint string) (string, error) {
	if s.social.Google == nil {
		return "", identity.ErrUnavailable
	}
	state, err1 := randomString()
	verifier, err2 := randomString()
	if err := errors.Join(err1, err2); err != nil {
		return "", err
	}
	_, _ = s.pool.Exec(ctx, `DELETE FROM auth_google_states WHERE expires_at < now()`)
	_, _ = s.pool.Exec(ctx, `DELETE FROM auth_login_codes WHERE expires_at < now()`)
	if _, err := s.pool.Exec(ctx, `INSERT INTO auth_google_states (state, verifier, expires_at) VALUES ($1, $2, now() + make_interval(secs => $3))`,
		state, verifier, googleStateTTL.Seconds()); err != nil {
		return "", err
	}
	return s.social.Google.AuthURL(state, sha256challenge(verifier), strings.TrimSpace(loginHint)), nil
}

func sha256challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// GoogleCallback finishes what Google started and returns the URL the browser
// must be sent to (the app's deep link). It creates no account: it only parks
// the verified identity behind a one-time code for GoogleExchange.
func (s *Service) GoogleCallback(ctx context.Context, state, code, googleError string) string {
	redirect := func(status, reason, oneTime string) string {
		q := url.Values{"status": {status}}
		if reason != "" {
			q.Set("reason", reason)
		}
		if oneTime != "" {
			q.Set("code", oneTime)
		}
		sep := "?"
		if strings.Contains(s.social.AppRedirect, "?") {
			sep = "&"
		}
		return s.social.AppRedirect + sep + q.Encode()
	}
	if s.social.Google == nil {
		return redirect("error", "unavailable", "")
	}
	var verifier string
	var expires time.Time
	err := s.pool.QueryRow(ctx, `DELETE FROM auth_google_states WHERE state = $1 RETURNING verifier, expires_at`, state).Scan(&verifier, &expires)
	if err != nil || time.Now().After(expires) {
		return redirect("error", "state_expired", "")
	}
	if googleError != "" {
		if googleError == "access_denied" {
			return redirect("error", "access_denied", "")
		}
		return redirect("error", "exchange_failed", "")
	}
	claims, err := s.social.Google.Exchange(ctx, code, verifier)
	switch {
	case errors.Is(err, identity.ErrUnavailable):
		return redirect("error", "unavailable", "")
	case err != nil:
		return redirect("error", "exchange_failed", "")
	case identity.Normalise(claims.Email) == "":
		return redirect("error", "no_email", "")
	}
	raw, err := randomString()
	if err == nil {
		var blob []byte
		if blob, err = json.Marshal(claims); err == nil {
			_, err = s.pool.Exec(ctx, `INSERT INTO auth_login_codes (code_hash, claims, expires_at) VALUES ($1, $2, now() + make_interval(secs => $3))`,
				sha256hex(raw), blob, loginCodeTTL.Seconds())
		}
	}
	if err != nil {
		return redirect("error", "exchange_failed", "")
	}
	return redirect("ok", "", raw)
}

// GoogleExchange redeems the one-time code from the deep link for a login.
func (s *Service) GoogleExchange(ctx context.Context, code string) (*SocialResult, error) {
	var blob []byte
	var expires time.Time
	err := s.pool.QueryRow(ctx, `DELETE FROM auth_login_codes WHERE code_hash = $1 RETURNING claims, expires_at`, sha256hex(code)).Scan(&blob, &expires)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && time.Now().After(expires)) {
		return nil, identity.ErrInvalidToken
	}
	if err != nil {
		return nil, err
	}
	var claims identity.Claims
	if err := json.Unmarshal(blob, &claims); err != nil {
		return nil, err
	}
	return s.SocialSignIn(ctx, &claims, "")
}
