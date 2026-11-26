package identity

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const appleIssuer = "https://appleid.apple.com"

// Apple verifies identity tokens from the native "Sign in with Apple" sheet.
type Apple struct {
	// Audiences are our app ids (bundle ids); a token for any of them is accepted.
	Audiences []string
	// JWKSURL is where Apple publishes its signing keys.
	JWKSURL string
	HTTP    *http.Client
	// Fake (development only) accepts "fake:<subject>:<email>" as a verified token.
	Fake bool

	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
}

// NewApple creates a verifier for the given bundle ids.
func NewApple(audiences []string) *Apple {
	return &Apple{Audiences: audiences, JWKSURL: appleIssuer + "/auth/keys", HTTP: &http.Client{Timeout: 10 * time.Second}}
}

// Verify checks the identity token's signature (Apple's published RS256 keys),
// issuer, audience and expiry, and returns what it proves. nonce is optional:
// when given and the token carries a nonce, they must match (the token holds
// either the raw value or its SHA-256 in hex).
func (a *Apple) Verify(ctx context.Context, token, nonce string) (*Claims, error) {
	if a.Fake && strings.HasPrefix(token, "fake:") {
		parts := strings.SplitN(token, ":", 3)
		if len(parts) != 3 || parts[1] == "" {
			return nil, ErrInvalidToken
		}
		return &Claims{Provider: ProviderApple, Subject: "fake-" + parts[1], Email: Normalise(parts[2]), EmailVerified: parts[2] != ""}, nil
	}
	if len(a.Audiences) == 0 {
		return nil, ErrUnavailable
	}
	var claims struct {
		jwt.RegisteredClaims
		Email          string `json:"email"`
		EmailVerified  any    `json:"email_verified"` // true or "true"
		Nonce          string `json:"nonce"`
		IsPrivateEmail any    `json:"is_private_email"`
	}
	parsed, err := jwt.ParseWithClaims(token, &claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return a.key(ctx, kid)
	}, jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(appleIssuer), jwt.WithExpirationRequired(), jwt.WithLeeway(time.Minute))
	if err != nil || !parsed.Valid {
		if isUnavailable(err) {
			return nil, ErrUnavailable
		}
		return nil, ErrInvalidToken
	}
	aud, _ := claims.GetAudience()
	if !slices.ContainsFunc(aud, func(x string) bool { return slices.Contains(a.Audiences, x) }) {
		return nil, ErrInvalidToken
	}
	if claims.Subject == "" {
		return nil, ErrInvalidToken
	}
	if nonce != "" && claims.Nonce != "" {
		sum := sha256.Sum256([]byte(nonce))
		if claims.Nonce != nonce && !strings.EqualFold(claims.Nonce, hex.EncodeToString(sum[:])) {
			return nil, ErrInvalidToken
		}
	}
	return &Claims{
		Provider: ProviderApple, Subject: claims.Subject, Email: Normalise(claims.Email), EmailVerified: truthy(claims.EmailVerified),
	}, nil
}

func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x == "true"
	}
	return false
}

type unavailableError struct{ error }

func isUnavailable(err error) bool {
	var u unavailableError
	return errors.As(err, &u)
}

// key returns Apple's public key for kid, refreshing the cached set when the
// key is unknown (at most once a minute) or the cache is older than an hour.
func (a *Apple) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if k, ok := a.keys[kid]; ok && time.Since(a.fetchedAt) < time.Hour {
		return k, nil
	}
	if time.Since(a.fetchedAt) > time.Minute || a.keys == nil {
		if err := a.fetch(ctx); err != nil {
			if k, ok := a.keys[kid]; ok { // keep serving with the old set if Apple is unreachable
				return k, nil
			}
			return nil, unavailableError{err}
		}
	}
	if k, ok := a.keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("unknown signing key %q", kid)
}

func (a *Apple) fetch(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.JWKSURL, nil)
	if err != nil {
		return err
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("apple keys answered %d", resp.StatusCode)
	}
	var set struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	if len(keys) == 0 {
		return fmt.Errorf("apple published no usable keys")
	}
	a.keys, a.fetchedAt = keys, time.Now()
	return nil
}
