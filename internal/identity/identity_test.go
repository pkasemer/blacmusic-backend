package identity

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type appleFixture struct {
	key *rsa.PrivateKey
	srv *httptest.Server
	hit int
	// down makes the JWKS endpoint fail
	down bool
}

func newAppleFixture(t *testing.T) *appleFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &appleFixture{key: key}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hit++
		if f.down {
			w.WriteHeader(500)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *appleFixture) verifier() *Apple {
	a := NewApple([]string{"com.blac.music", "com.blac.music.dev"})
	a.JWKSURL = f.srv.URL
	return a
}

func (f *appleFixture) token(t *testing.T, mutate func(claims jwt.MapClaims), method jwt.SigningMethod, key any, kid string) string {
	t.Helper()
	c := jwt.MapClaims{
		"iss": "https://appleid.apple.com", "aud": "com.blac.music", "sub": "001234.abcd", "exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(), "email": "Kato@Example.com", "email_verified": "true",
	}
	if mutate != nil {
		mutate(c)
	}
	tok := jwt.NewWithClaims(method, c)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (f *appleFixture) good(t *testing.T, mutate func(jwt.MapClaims)) string {
	return f.token(t, mutate, jwt.SigningMethodRS256, f.key, "k1")
}

func TestAppleVerify(t *testing.T) {
	f := newAppleFixture(t)
	a := f.verifier()
	ctx := context.Background()

	c, err := a.Verify(ctx, f.good(t, nil), "")
	if err != nil || c.Provider != ProviderApple || c.Subject != "001234.abcd" || c.Email != "kato@example.com" || !c.EmailVerified {
		t.Fatalf("valid token = %+v %v", c, err)
	}
	// both of our app ids are accepted
	if _, err := a.Verify(ctx, f.good(t, func(c jwt.MapClaims) { c["aud"] = "com.blac.music.dev" }), ""); err != nil {
		t.Fatalf("dev bundle id: %v", err)
	}
	// a boolean email_verified, and an unverified one
	if c, _ = a.Verify(ctx, f.good(t, func(c jwt.MapClaims) { c["email_verified"] = true }), ""); !c.EmailVerified {
		t.Fatal("boolean email_verified not read")
	}
	if c, _ = a.Verify(ctx, f.good(t, func(c jwt.MapClaims) { c["email_verified"] = "false" }), ""); c.EmailVerified {
		t.Fatal("email_verified=false read as true")
	}
	// the key set is fetched once and cached
	hits := f.hit
	a.Verify(ctx, f.good(t, nil), "") //nolint:errcheck
	if f.hit != hits {
		t.Fatal("the key set was fetched again for every token")
	}

	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	for name, tok := range map[string]string{
		"wrong audience":   f.good(t, func(c jwt.MapClaims) { c["aud"] = "com.someone.else" }),
		"wrong issuer":     f.good(t, func(c jwt.MapClaims) { c["iss"] = "https://evil.example" }),
		"expired":          f.good(t, func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Hour).Unix() }),
		"no expiry":        f.good(t, func(c jwt.MapClaims) { delete(c, "exp") }),
		"no subject":       f.good(t, func(c jwt.MapClaims) { c["sub"] = "" }),
		"forged signature": f.token(t, nil, jwt.SigningMethodRS256, other, "k1"),
		"unknown key id":   f.token(t, nil, jwt.SigningMethodRS256, f.key, "nope"),
		"HS256 confusion":  f.token(t, nil, jwt.SigningMethodHS256, []byte("secret"), "k1"),
		"garbage":          "not.a.jwt",
		"empty":            "",
	} {
		if _, err := a.Verify(ctx, tok, ""); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s: err = %v, want ErrInvalidToken", name, err)
		}
	}
	// the fake prefix is only honoured when the fake is switched on
	if _, err := a.Verify(ctx, "fake:abc:x@y.com", ""); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("fake token accepted by a real verifier: %v", err)
	}
}

func TestAppleNonce(t *testing.T) {
	f := newAppleFixture(t)
	a := f.verifier()
	sum := sha256.Sum256([]byte("raw-nonce"))
	hashed := f.good(t, func(c jwt.MapClaims) { c["nonce"] = hex.EncodeToString(sum[:]) })
	raw := f.good(t, func(c jwt.MapClaims) { c["nonce"] = "raw-nonce" })
	ctx := context.Background()
	for _, tok := range []string{hashed, raw} {
		if _, err := a.Verify(ctx, tok, "raw-nonce"); err != nil {
			t.Fatalf("matching nonce: %v", err)
		}
	}
	if _, err := a.Verify(ctx, hashed, "someone-elses"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("mismatching nonce = %v", err)
	}
	if _, err := a.Verify(ctx, hashed, ""); err != nil {
		t.Fatalf("no nonce supplied: %v", err)
	}
}

func TestAppleKeysUnreachable(t *testing.T) {
	f := newAppleFixture(t)
	f.down = true
	a := f.verifier()
	if _, err := a.Verify(context.Background(), f.good(t, nil), ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Apple down, nothing cached = %v, want ErrUnavailable", err)
	}
	f.down = false
	if _, err := a.Verify(context.Background(), f.good(t, nil), ""); err != nil {
		t.Fatalf("after Apple came back: %v", err)
	}
	f.down = true
	a.fetchedAt = time.Now().Add(-2 * time.Hour) // stale cache and Apple down: keep serving the known key
	if _, err := a.Verify(context.Background(), f.good(t, nil), ""); err != nil {
		t.Fatalf("stale cache while Apple is down: %v", err)
	}
	if _, err := NewApple(nil).Verify(context.Background(), "x.y.z", ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no audiences configured = %v", err)
	}
}

func TestAppleFake(t *testing.T) {
	a := NewApple(nil)
	a.Fake = true
	c, err := a.Verify(context.Background(), "fake:user1:Kato@Example.com", "")
	if err != nil || c.Subject != "fake-user1" || c.Email != "kato@example.com" || !c.EmailVerified {
		t.Fatalf("fake = %+v %v", c, err)
	}
	if _, err := a.Verify(context.Background(), "fake::x", ""); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("empty fake subject = %v", err)
	}
}

// ---------------------------------------------------------------------- Google

func googleServer(t *testing.T, idToken func() (string, int)) *GoogleOAuth {
	t.Helper()
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.Form
		if form.Get("code") != "good-code" || form.Get("code_verifier") != "ver" || form.Get("client_secret") != "sec" || form.Get("grant_type") != "authorization_code" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		tok, status := idToken()
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"id_token": tok})
	}))
	t.Cleanup(srv.Close)
	g := NewGoogleOAuth("client-1", "sec", "https://api.example/v1/auth/google/callback")
	g.TokenURL = srv.URL
	return g
}

func idToken(t *testing.T, mutate func(jwt.MapClaims)) string {
	t.Helper()
	c := jwt.MapClaims{"iss": "https://accounts.google.com", "aud": "client-1", "sub": "1099", "exp": time.Now().Add(time.Hour).Unix(),
		"email": "Nakato@Gmail.com", "email_verified": true, "name": "Nakato K"}
	if mutate != nil {
		mutate(c)
	}
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte("irrelevant"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestGoogleAuthURL(t *testing.T) {
	g := NewGoogleOAuth("client-1", "sec", "https://api.example/cb")
	u, _ := url.Parse(g.AuthURL("st", "chal", "me@gmail.com"))
	q := u.Query()
	if q.Get("scope") != "openid email profile" || q.Get("code_challenge") != "chal" || q.Get("code_challenge_method") != "S256" ||
		q.Get("state") != "st" || q.Get("login_hint") != "me@gmail.com" || q.Get("response_type") != "code" || q.Get("client_id") != "client-1" {
		t.Fatalf("auth url = %v", q)
	}
	if strings.Contains(g.AuthURL("s", "c", ""), "login_hint") {
		t.Fatal("empty hint sent")
	}
}

func TestGoogleExchange(t *testing.T) {
	ctx := context.Background()
	g := googleServer(t, func() (string, int) { return idToken(t, nil), 200 })
	c, err := g.Exchange(ctx, "good-code", "ver")
	if err != nil || c.Provider != ProviderGoogle || c.Subject != "1099" || c.Email != "nakato@gmail.com" || !c.EmailVerified || c.Name != "Nakato K" {
		t.Fatalf("exchange = %+v %v", c, err)
	}
	if _, err := g.Exchange(ctx, "bad-code", "ver"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("bad code = %v", err)
	}
	if _, err := g.Exchange(ctx, "good-code", "wrong-verifier"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("bad verifier = %v", err)
	}
	for name, mut := range map[string]func(jwt.MapClaims){
		"wrong audience": func(c jwt.MapClaims) { c["aud"] = "other-client" },
		"wrong issuer":   func(c jwt.MapClaims) { c["iss"] = "https://evil.example" },
		"expired":        func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Minute).Unix() },
		"no subject":     func(c jwt.MapClaims) { delete(c, "sub") },
	} {
		g := googleServer(t, func() (string, int) { return idToken(t, mut), 200 })
		if _, err := g.Exchange(ctx, "good-code", "ver"); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// "accounts.google.com" without the scheme is also a valid issuer; an unverified email is reported as such
	g = googleServer(t, func() (string, int) {
		return idToken(t, func(c jwt.MapClaims) { c["iss"] = "accounts.google.com"; c["email_verified"] = false }), 200
	})
	if c, err = g.Exchange(ctx, "good-code", "ver"); err != nil || c.EmailVerified {
		t.Fatalf("unverified email = %+v %v", c, err)
	}
	g = googleServer(t, func() (string, int) { return "", 503 })
	if _, err := g.Exchange(ctx, "good-code", "ver"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Google 503 = %v", err)
	}
	g.TokenURL = "http://127.0.0.1:1"
	if _, err := g.Exchange(ctx, "good-code", "ver"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unreachable = %v", err)
	}
}

func TestFakeGoogle(t *testing.T) {
	f := &FakeGoogle{CallbackURL: "http://api/cb"}
	u, _ := url.Parse(f.AuthURL("st", "", "Someone@Example.com"))
	if u.Query().Get("state") != "st" || u.Query().Get("code") != "fake:someone@example.com" {
		t.Fatalf("fake url = %s", u)
	}
	if !strings.Contains(f.AuthURL("st", "", ""), "google-demo%40example.com") {
		t.Fatal("default identity missing")
	}
	c, err := f.Exchange(context.Background(), "fake:someone@example.com", "")
	if err != nil || c.Email != "someone@example.com" || !c.EmailVerified || c.Subject != "fake-someone@example.com" || c.Name != "Google someone" {
		t.Fatalf("fake exchange = %+v %v", c, err)
	}
	if _, err := f.Exchange(context.Background(), "real-code", ""); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("non-fake code = %v", err)
	}
}
