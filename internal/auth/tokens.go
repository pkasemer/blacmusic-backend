// Package auth implements registration, login, JWT access tokens and rotating
// refresh tokens.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// TokenService signs and verifies short-lived access tokens.
type TokenService struct {
	secret []byte
	ttl    time.Duration
}

// NewTokenService creates a TokenService (HS256).
func NewTokenService(secret string, accessTTL time.Duration) *TokenService {
	return &TokenService{secret: []byte(secret), ttl: accessTTL}
}

// NewAccess issues an access token for userID and returns its expiry.
func (t *TokenService) NewAccess(userID string) (string, time.Time, error) {
	now := time.Now()
	exp := now.Add(t.ttl)
	claims := jwt.RegisteredClaims{
		Subject:   userID,
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(exp),
		Issuer:    "blac-music",
	}
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.secret)
	return s, exp, err
}

// ParseAccess verifies an access token and returns the user id.
func (t *TokenService) ParseAccess(token string) (string, error) {
	var claims jwt.RegisteredClaims
	parsed, err := jwt.ParseWithClaims(token, &claims, func(*jwt.Token) (any, error) {
		return t.secret, nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer("blac-music"), jwt.WithExpirationRequired())
	if err != nil || !parsed.Valid || claims.Subject == "" {
		return "", errors.New("invalid access token")
	}
	return claims.Subject, nil
}

// NewRefresh returns a random opaque refresh token and its storable hash.
func NewRefresh() (raw, hash string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	raw = base64.RawURLEncoding.EncodeToString(b)
	return raw, HashRefresh(raw), nil
}

// HashRefresh is the value stored in refresh_tokens.token_hash.
func HashRefresh(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
