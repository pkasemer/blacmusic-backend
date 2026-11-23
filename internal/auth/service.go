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
	ID          string    `json:"id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"displayName"`
	Role        string    `json:"role"` // user | creator | admin (creators and admins may upload)
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
}

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
	u := &User{Email: email, DisplayName: displayName}
	err = s.pool.QueryRow(ctx,
		`INSERT INTO users(email, password_hash, display_name) VALUES ($1,$2,$3) RETURNING id, role, created_at`,
		email, hash, displayName).Scan(&u.ID, &u.Role, &u.CreatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return nil, nil, ErrEmailTaken
	}
	if err != nil {
		return nil, nil, err
	}
	pair, err := s.issue(ctx, u.ID)
	if err != nil {
		return nil, nil, err
	}
	return u, pair, nil
}

func (s *Service) Login(ctx context.Context, email, password string) (*User, *TokenPair, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	u := &User{}
	var hash string
	err := s.pool.QueryRow(ctx,
		`SELECT id, email, display_name, role, created_at, password_hash FROM users WHERE email=$1`, email).
		Scan(&u.ID, &u.Email, &u.DisplayName, &u.Role, &u.CreatedAt, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		CheckPassword(dummy(), password)
		return nil, nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, nil, err
	}
	if !CheckPassword(hash, password) {
		return nil, nil, ErrInvalidCredentials
	}
	pair, err := s.issue(ctx, u.ID)
	if err != nil {
		return nil, nil, err
	}
	return u, pair, nil
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
	err := s.pool.QueryRow(ctx,
		`SELECT id, email, display_name, role, created_at FROM users WHERE id=$1`, userID).
		Scan(&u.ID, &u.Email, &u.DisplayName, &u.Role, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInvalidToken
	}
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
