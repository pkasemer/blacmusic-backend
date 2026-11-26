package youtube

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ReadOnlyScope is the only permission we ask for.
const ReadOnlyScope = "https://www.googleapis.com/auth/youtube.readonly"

// Errors a Provider reports. Everything else is treated as transient.
var (
	// ErrInvalidGrant: Google no longer accepts the stored refresh token (the
	// user revoked access or it expired). The user must connect again.
	ErrInvalidGrant = errors.New("youtube: access was revoked, reconnect required")
	// ErrQuota: the daily API quota is used up; retry after it resets.
	ErrQuota = errors.New("youtube: API quota exceeded")
	// ErrNoChannel: the Google account has no YouTube channel.
	ErrNoChannel = errors.New("youtube: this Google account has no channel")
)

// APIError is an unexpected answer from Google (treated as transient unless 4xx).
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("youtube: Google answered %d: %s", e.Status, e.Body)
}

// Token is what the OAuth exchange returns.
type Token struct {
	AccessToken  string
	RefreshToken string // empty on refresh responses
	Expiry       time.Time
	Scope        string
}

// Channel is the connected channel.
type Channel struct {
	ID           string
	Title        string
	ThumbnailURL string
}

// Video is one upload of the channel.
type Video struct {
	ID           string
	Title        string
	Description  string
	PublishedAt  time.Time
	DurationSec  int
	ThumbnailURL string
}

// Provider is the part of YouTube/Google we use. The real implementation talks
// to Google; a fake serves development and tests.
type Provider interface {
	// AuthURL is where the user's browser is sent to grant access.
	AuthURL(state, codeChallenge string) string
	Exchange(ctx context.Context, code, codeVerifier string) (*Token, error)
	Refresh(ctx context.Context, refreshToken string) (*Token, error)
	Channel(ctx context.Context, accessToken string) (*Channel, error)
	// Videos lists the channel's uploads, newest first, at most max.
	Videos(ctx context.Context, accessToken string, max int) ([]Video, error)
	// Revoke tells Google to invalidate a token.
	Revoke(ctx context.Context, token string) error
}
