package auth

import (
	"context"
	"errors"
	"net/mail"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/mailer"
)

var (
	ErrEmailTaken         = errors.New("email already registered")
	ErrInvalidCredentials = errors.New("invalid email or password")
	ErrInvalidToken       = errors.New("invalid or expired refresh token")
)

// ValidationError is returned for bad user input.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// User is the public account shape.
type User struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	Role        string `json:"role"` // user | admin (creator is a legacy value)
	// EmailVerified is required before a user can turn on artist mode.
	EmailVerified bool `json:"emailVerified"`
	// AvatarURL is the profile picture (medium size), absent when none is set.
	// It is also the image of the user's artist profile.
	AvatarURL string `json:"avatarUrl,omitempty"`
	// HasPassword is false for accounts that only sign in with Apple or Google.
	HasPassword bool `json:"hasPassword"`
	// PushEnabled is true when at least one phone is registered for push notifications.
	PushEnabled bool      `json:"pushEnabled"`
	CreatedAt   time.Time `json:"createdAt"`
}

// TokenPair is returned by register, login and refresh.
type TokenPair struct {
	AccessToken  string    `json:"accessToken"`
	ExpiresAt    time.Time `json:"expiresAt"`
	RefreshToken string    `json:"refreshToken"`
}

// Service holds the auth business logic.
type Service struct {
	pool       *pgxpool.Pool
	tokens     *TokenService
	refreshTTL time.Duration
	mailer     mailer.Mailer
	avatarURL  func(imageID string) string
	social     SocialConfig
}

// SetAvatarURL sets how an avatar image id becomes a public URL. (The auth
// package cannot import the image package, which depends on it.)
func (s *Service) SetAvatarURL(f func(imageID string) string) { s.avatarURL = f }

func (s *Service) avatar(id *string) string {
	if id == nil || *id == "" || s.avatarURL == nil {
		return ""
	}
	return s.avatarURL(*id)
}

// SetMailer sets how verification emails are delivered.
func (s *Service) SetMailer(m mailer.Mailer) { s.mailer = m }

// NewService creates the auth service.
func NewService(pool *pgxpool.Pool, tokens *TokenService, refreshTTL time.Duration) *Service {
	return &Service{pool: pool, tokens: tokens, refreshTTL: refreshTTL}
}

// Tokens exposes the token service (used by the auth middleware).
func (s *Service) Tokens() *TokenService { return s.tokens }

// A real bcrypt hash compared when the email is unknown, so login timing does
// not reveal which emails exist.
var (
	dummyOnce sync.Once
	dummyHash string
)

func dummy() string {
	dummyOnce.Do(func() { dummyHash, _ = HashPassword("not-a-real-password") })
	return dummyHash
}

func (s *Service) Register(ctx context.Context, email, password, displayName string) (*User, *TokenPair, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	displayName = strings.TrimSpace(displayName)

	if addr, err := mail.ParseAddress(email); err != nil || addr.Address != email {
		return nil, nil, &ValidationError{"invalid email address"}
	}
	if len(password) < 8 {
		return nil, nil, &ValidationError{"password must be at least 8 characters"}
	}
	if len(password) > 72 {
		return nil, nil, &ValidationError{"password must be at most 72 bytes"}
	}
	if n := utf8.RuneCountInString(displayName); n < 1 || n > 50 {
		return nil, nil, &ValidationError{"displayName must be 1-50 characters"}
	}

	hash, err := HashPassword(password)
	if err != nil {
		return nil, nil, err
	}
	u := &User{Email: email, DisplayName: displayName, HasPassword: true}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// An account on this address that was never confirmed (nobody has proved they own the
	// address), has no Apple/Google sign-in and no artist profile is only a leftover of a
	// sign-up that was abandoned or mistyped: the new sign-up replaces it, with its sessions
	// and anything it holds. A confirmed account is never replaced.
	if _, err := tx.Exec(ctx, `
		DELETE FROM users u
		WHERE u.email = $1 AND u.email_verified_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM auth_identities i WHERE i.user_id = u.id)
		  AND NOT EXISTS (SELECT 1 FROM artists a WHERE a.owner_user_id = u.id)`, email); err != nil {
		return nil, nil, err
	}
	err = tx.QueryRow(ctx,
		`INSERT INTO users(email, password_hash, display_name) VALUES ($1,$2,$3) RETURNING id, role, created_at`,
		email, hash, displayName).Scan(&u.ID, &u.Role, &u.CreatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return nil, nil, ErrEmailTaken
	}
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	pair, err := s.issue(ctx, u.ID)
	if err != nil {
		return nil, nil, err
	}
	return u, pair, nil
}

func (s *Service) Login(ctx context.Context, email, password string) (*User, *TokenPair, error) {
	u, err := s.Authenticate(ctx, email, password)
	if err != nil {
		return nil, nil, err
	}
	pair, err := s.issue(ctx, u.ID)
	if err != nil {
		return nil, nil, err
	}
	return u, pair, nil
}

// Issue creates a new access and refresh token for a user whose identity was already checked.
func (s *Service) Issue(ctx context.Context, userID string) (*TokenPair, error) {
	return s.issue(ctx, userID)
}

// Authenticate checks an email and password and returns the user without issuing any tokens.
// ErrInvalidCredentials for a wrong email or password; ErrAccountBanned once the password was right.
func (s *Service) Authenticate(ctx context.Context, email, password string) (*User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	u := &User{}
	var hash string
	var avatarID *string
	var banned bool
	err := s.pool.QueryRow(ctx,
		`SELECT id, email, display_name, role, email_verified_at IS NOT NULL, avatar_image_id, created_at, password_hash,
		        EXISTS (SELECT 1 FROM push_tokens p WHERE p.user_id = users.id), banned_at IS NOT NULL FROM users WHERE email=$1`, email).
		Scan(&u.ID, &u.Email, &u.DisplayName, &u.Role, &u.EmailVerified, &avatarID, &u.CreatedAt, &hash, &u.PushEnabled, &banned)
	if errors.Is(err, pgx.ErrNoRows) {
		CheckPassword(dummy(), password)
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, err
	}
	if hash == noPassword { // signs in with Apple/Google only: no password can match
		CheckPassword(dummy(), password)
		return nil, ErrInvalidCredentials
	}
	if !CheckPassword(hash, password) {
		return nil, ErrInvalidCredentials
	}
	if banned { // said only after the password was right, so it tells nothing to someone guessing
		return nil, ErrAccountBanned
	}
	u.HasPassword = true
	u.AvatarURL = s.avatar(avatarID)
	return u, nil
}

// Refresh rotates a refresh token: the old one is revoked atomically and a new
// pair is issued.
func (s *Service) Refresh(ctx context.Context, raw string) (*TokenPair, error) {
	var userID string
	err := s.pool.QueryRow(ctx, `
		UPDATE refresh_tokens SET revoked_at = now()
		WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > now()
		RETURNING user_id`, HashRefresh(raw)).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInvalidToken
	}
	if err != nil {
		return nil, err
	}
	var banned bool
	if err := s.pool.QueryRow(ctx, `SELECT banned_at IS NOT NULL FROM users WHERE id = $1`, userID).Scan(&banned); err != nil {
		return nil, err
	}
	if banned {
		return nil, ErrAccountBanned
	}
	return s.issue(ctx, userID)
}

// Logout revokes a refresh token. Unknown tokens are ignored.
func (s *Service) Logout(ctx context.Context, raw string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE refresh_tokens SET revoked_at = now() WHERE token_hash = $1 AND revoked_at IS NULL`, HashRefresh(raw))
	return err
}

func (s *Service) Me(ctx context.Context, userID string) (*User, error) {
	u := &User{}
	var avatarID *string
	err := s.pool.QueryRow(ctx,
		`SELECT id, email, display_name, role, email_verified_at IS NOT NULL, avatar_image_id, created_at, password_hash <> $2,
		        EXISTS (SELECT 1 FROM push_tokens p WHERE p.user_id = users.id) FROM users WHERE id=$1`, userID, noPassword).
		Scan(&u.ID, &u.Email, &u.DisplayName, &u.Role, &u.EmailVerified, &avatarID, &u.CreatedAt, &u.HasPassword, &u.PushEnabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInvalidToken
	}
	u.AvatarURL = s.avatar(avatarID)
	return u, err
}

func (s *Service) issue(ctx context.Context, userID string) (*TokenPair, error) {
	access, exp, err := s.tokens.NewAccess(userID)
	if err != nil {
		return nil, err
	}
	raw, hash, err := NewRefresh()
	if err != nil {
		return nil, err
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO refresh_tokens(user_id, token_hash, expires_at) VALUES ($1,$2,$3)`,
		userID, hash, time.Now().Add(s.refreshTTL)); err != nil {
		return nil, err
	}
	return &TokenPair{AccessToken: access, ExpiresAt: exp.UTC(), RefreshToken: raw}, nil
}
