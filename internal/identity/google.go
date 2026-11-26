package identity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// GoogleProvider is the backend-hosted Google sign-in: the user's browser goes to
// AuthURL, Google redirects back with a code, and Exchange turns it into Claims.
type GoogleProvider interface {
	// AuthURL is where the browser is sent. loginHint is optional (an email to pre-fill).
	AuthURL(state, codeChallenge, loginHint string) string
	// Exchange trades the authorisation code for the user's Claims.
	Exchange(ctx context.Context, code, codeVerifier string) (*Claims, error)
}

// ReadScopes are the only scopes requested: none of them is sensitive, so Google
// needs no verification review for them.
const ReadScopes = "openid email profile"

// GoogleOAuth is the real Google provider.
type GoogleOAuth struct {
	ClientID, ClientSecret, RedirectURL string
	AuthBase, TokenURL                  string
	HTTP                                *http.Client
}

// NewGoogleOAuth returns the provider for an OAuth client of type "web application".
func NewGoogleOAuth(clientID, clientSecret, redirectURL string) *GoogleOAuth {
	return &GoogleOAuth{
		ClientID: clientID, ClientSecret: clientSecret, RedirectURL: redirectURL,
		AuthBase: "https://accounts.google.com/o/oauth2/v2/auth", TokenURL: "https://oauth2.googleapis.com/token",
		HTTP: &http.Client{Timeout: 15 * time.Second},
	}
}

func (g *GoogleOAuth) AuthURL(state, challenge, loginHint string) string {
	q := url.Values{
		"client_id": {g.ClientID}, "redirect_uri": {g.RedirectURL}, "response_type": {"code"}, "scope": {ReadScopes},
		"state": {state}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}, "prompt": {"select_account"},
	}
	if loginHint != "" {
		q.Set("login_hint", loginHint)
	}
	return g.AuthBase + "?" + q.Encode()
}

// Exchange redeems the code. The ID token comes straight from Google's token
// endpoint over TLS, so its payload is trusted after checking issuer, audience
// and expiry (OpenID Connect Core 3.1.3.7).
func (g *GoogleOAuth) Exchange(ctx context.Context, code, verifier string) (*Claims, error) {
	form := url.Values{
		"code": {code}, "client_id": {g.ClientID}, "client_secret": {g.ClientSecret}, "redirect_uri": {g.RedirectURL},
		"grant_type": {"authorization_code"}, "code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var tr struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
	}
	_ = json.Unmarshal(body, &tr)
	switch {
	case resp.StatusCode >= 500:
		return nil, ErrUnavailable
	case resp.StatusCode != http.StatusOK || tr.IDToken == "":
		return nil, ErrInvalidToken
	}
	var c struct {
		jwt.RegisteredClaims
		Email         string `json:"email"`
		EmailVerified any    `json:"email_verified"`
		Name          string `json:"name"`
	}
	if _, _, err := jwt.NewParser().ParseUnverified(tr.IDToken, &c); err != nil {
		return nil, ErrInvalidToken
	}
	if c.Issuer != "https://accounts.google.com" && c.Issuer != "accounts.google.com" {
		return nil, ErrInvalidToken
	}
	aud, _ := c.GetAudience()
	okAud := false
	for _, a := range aud {
		okAud = okAud || a == g.ClientID
	}
	if exp, _ := c.GetExpirationTime(); !okAud || exp == nil || time.Now().After(exp.Time) || c.Subject == "" {
		return nil, ErrInvalidToken
	}
	return &Claims{Provider: ProviderGoogle, Subject: c.Subject, Email: Normalise(c.Email), EmailVerified: truthy(c.EmailVerified), Name: strings.TrimSpace(c.Name)}, nil
}

// FakeGoogle serves development: the "consent page" is the callback itself.
// start's loginHint picks the identity (default google-demo@example.com), so a
// test can sign in as an email that already has an account.
type FakeGoogle struct {
	CallbackURL string
}

func (f *FakeGoogle) AuthURL(state, _, loginHint string) string {
	email := Normalise(loginHint)
	if email == "" {
		email = "google-demo@example.com"
	}
	return f.CallbackURL + "?" + url.Values{"state": {state}, "code": {"fake:" + email}}.Encode()
}

func (f *FakeGoogle) Exchange(_ context.Context, code, _ string) (*Claims, error) {
	email, ok := strings.CutPrefix(code, "fake:")
	if !ok || email == "" {
		return nil, ErrInvalidToken
	}
	name := email
	if i := strings.IndexByte(email, '@'); i > 0 {
		name = email[:i]
	}
	return &Claims{Provider: ProviderGoogle, Subject: "fake-" + email, Email: email, EmailVerified: true, Name: "Google " + name}, nil
}
