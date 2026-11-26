// Package identity verifies "Sign in with Apple" and "Sign in with Google"
// and turns what they prove into Claims. It knows nothing about accounts: the
// auth package decides which user a Claims value belongs to.
package identity

import (
	"errors"
	"strings"
)

// Provider names, as stored in auth_identities.
const (
	ProviderApple  = "apple"
	ProviderGoogle = "google"
)

// Claims is what a provider vouches for.
type Claims struct {
	Provider      string
	Subject       string // the provider's stable user id
	Email         string
	EmailVerified bool
	Name          string // empty when the provider gave none (Apple only sends it to the app, once)
}

var (
	// ErrInvalidToken: the token or code is not valid (bad signature, wrong audience, expired, reused).
	ErrInvalidToken = errors.New("identity: invalid token")
	// ErrUnavailable: the provider is not configured or could not be reached.
	ErrUnavailable = errors.New("identity: provider unavailable")
)

// Normalise lower-cases and trims an email.
func Normalise(email string) string { return strings.ToLower(strings.TrimSpace(email)) }
