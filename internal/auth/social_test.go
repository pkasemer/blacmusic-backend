package auth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/identity"
	"github.com/blacmusic/backend/internal/testutil"
)

type socialEnv struct {
	t    *testing.T
	pool *pgxpool.Pool
	svc  *auth.Service
	r    http.Handler
}

func setupSocial(t *testing.T) *socialEnv {
	t.Helper()
	pool := testutil.Pool(t)
	svc := auth.NewService(pool, auth.NewTokenService(testutil.Secret, time.Minute), time.Hour)
	apple := identity.NewApple([]string{"com.blac.music"})
	apple.Fake = true
	svc.SetSocial(auth.SocialConfig{
		Apple: apple, Google: &identity.FakeGoogle{CallbackURL: "http://api.test/v1/auth/google/callback"}, AppRedirect: "blacmusic://auth/google",
	})
	h := auth.NewHandler(svc)
	r := chi.NewRouter()
	r.Post("/auth/login", h.Login)
	r.Post("/auth/register", h.Register)
	r.Post("/auth/apple", h.Apple)
	r.Post("/auth/google/start", h.GoogleStart)
	r.Get("/auth/google/callback", h.GoogleCallback)
	r.Post("/auth/google/exchange", h.GoogleExchange)
	r.Post("/auth/refresh", h.Refresh)
	return &socialEnv{t: t, pool: pool, svc: svc, r: r}
}

func (e *socialEnv) email(prefix string) string {
	e.t.Helper()
	email := strings.ToLower(testutil.Unique(prefix)) + "@example.com"
	e.t.Cleanup(func() { e.pool.Exec(context.Background(), `DELETE FROM users WHERE email = $1`, email) }) //nolint:errcheck
	return email
}

func (e *socialEnv) call(method, path string, body any) (int, map[string]any, http.Header) {
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	rec := httptest.NewRecorder()
	e.r.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec.Header()
}

func code(b map[string]any) string {
	m, _ := b["error"].(map[string]any)
	s, _ := m["code"].(string)
	return s
}

func user(b map[string]any) map[string]any { u, _ := b["user"].(map[string]any); return u }

func TestAppleSignInCreatesAndRecognisesAccounts(t *testing.T) {
	e := setupSocial(t)
	email := e.email("apple")
	tok := "fake:apple-sub-" + email + ":" + email

	st, b, _ := e.call("POST", "/auth/apple", map[string]any{"identityToken": tok, "fullName": map[string]string{"givenName": "Nakato", "familyName": "Kato"}})
	u := user(b)
	if st != 200 || b["isNewUser"] != true || b["linked"] != false || u["email"] != email || u["displayName"] != "Nakato Kato" ||
		u["emailVerified"] != true || u["hasPassword"] != false || b["accessToken"] == "" || b["refreshToken"] == "" {
		t.Fatalf("first sign-in = %d %v", st, b)
	}
	id := u["id"]
	// the same Apple user comes back: no new account; the name is not asked for again
	st, b, _ = e.call("POST", "/auth/apple", map[string]any{"identityToken": tok})
	if st != 200 || b["isNewUser"] != false || user(b)["id"] != id || user(b)["displayName"] != "Nakato Kato" {
		t.Fatalf("second sign-in = %d %v", st, b)
	}
	// the tokens work
	if st, _, _ = e.call("POST", "/auth/refresh", map[string]string{"refreshToken": b["refreshToken"].(string)}); st != 200 {
		t.Fatalf("refresh = %d", st)
	}
	// no name given: falls back to the email's local part
	other := e.email("noname")
	_, b, _ = e.call("POST", "/auth/apple", map[string]any{"identityToken": "fake:s2-" + other + ":" + other})
	if user(b)["displayName"] != strings.Split(other, "@")[0] {
		t.Fatalf("fallback name = %v", user(b)["displayName"])
	}
	// bad requests
	if st, b, _ = e.call("POST", "/auth/apple", map[string]any{}); st != 400 {
		t.Fatalf("no token = %d", st)
	}
	if st, b, _ = e.call("POST", "/auth/apple", map[string]any{"identityToken": "garbage"}); st != 401 || code(b) != "invalid_token" {
		t.Fatalf("garbage token = %d %v", st, b)
	}
	// a token with no email cannot make an account
	if st, b, _ = e.call("POST", "/auth/apple", map[string]any{"identityToken": "fake:no-email-sub:"}); st != 422 || code(b) != "no_email" {
		t.Fatalf("no email = %d %v", st, b)
	}
}

func TestLinkingRules(t *testing.T) {
	e := setupSocial(t)
	ctx := context.Background()

	// 1. a verified account with a password: Google links to it and the password keeps working
	email := e.email("link")
	_, reg, _ := e.call("POST", "/auth/register", map[string]string{"email": email, "password": "supersecret1", "displayName": "Kato"})
	uid := user(reg)["id"].(string)
	e.pool.Exec(ctx, `UPDATE users SET email_verified_at = now() WHERE id = $1`, uid) //nolint:errcheck
	res, err := e.svc.SocialSignIn(ctx, &identity.Claims{Provider: "google", Subject: "g-1", Email: email, EmailVerified: true, Name: "Google Name"}, "")
	if err != nil || res.User.ID != uid || !res.Linked || res.IsNew || !res.User.HasPassword || res.User.DisplayName != "Kato" {
		t.Fatalf("link = %+v %v", res, err)
	}
	if st, _, _ := e.call("POST", "/auth/login", map[string]string{"email": email, "password": "supersecret1"}); st != 200 {
		t.Fatalf("password login after linking = %d", st)
	}
	// a second provider on the same account
	res, err = e.svc.SocialSignIn(ctx, &identity.Claims{Provider: "apple", Subject: "a-1", Email: email, EmailVerified: true}, "")
	if err != nil || res.User.ID != uid || !res.Linked {
		t.Fatalf("second provider = %+v %v", res, err)
	}
	// each provider now signs in to it directly, even if the email it reports changes
	for _, c := range []*identity.Claims{{Provider: "google", Subject: "g-1", Email: "changed@example.com", EmailVerified: true}, {Provider: "apple", Subject: "a-1"}} {
		if res, err = e.svc.SocialSignIn(ctx, c, ""); err != nil || res.User.ID != uid || res.Linked || res.IsNew {
			t.Fatalf("known identity %s = %+v %v", c.Provider, res, err)
		}
	}

	// 2. a provider email that is NOT verified is never linked
	email2 := e.email("unverified")
	e.call("POST", "/auth/register", map[string]string{"email": email2, "password": "supersecret1", "displayName": "X"})
	if _, err := e.svc.SocialSignIn(ctx, &identity.Claims{Provider: "google", Subject: "g-2", Email: email2, EmailVerified: false}, ""); !errors.Is(err, auth.ErrEmailConflict) {
		t.Fatalf("unverified provider email = %v, want ErrEmailConflict", err)
	}
	var n int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM auth_identities WHERE subject = 'g-2'`).Scan(&n) //nolint:errcheck
	if n != 0 {
		t.Fatal("an identity was linked on an unverified email")
	}
	if st, b, _ := e.call("POST", "/auth/google/start", nil); st != 200 || b["authUrl"] == nil {
		t.Fatalf("start = %d", st)
	}

	// 3. the account was registered with a password but its email was never verified: the person who
	//    registered it may not own the address. A verified provider takes it over: password gone, sessions ended.
	email3 := e.email("pre-hijack")
	_, reg, _ = e.call("POST", "/auth/register", map[string]string{"email": email3, "password": "attackers-pass1", "displayName": "Mallory"})
	attackerRefresh := reg["refreshToken"].(string)
	res, err = e.svc.SocialSignIn(ctx, &identity.Claims{Provider: "google", Subject: "g-3", Email: email3, EmailVerified: true}, "")
	if err != nil || !res.Linked || res.User.HasPassword || !res.User.EmailVerified {
		t.Fatalf("takeover = %+v %v", res, err)
	}
	if st, _, _ := e.call("POST", "/auth/login", map[string]string{"email": email3, "password": "attackers-pass1"}); st != 401 {
		t.Fatalf("the earlier password still works: %d", st)
	}
	if st, _, _ := e.call("POST", "/auth/refresh", map[string]string{"refreshToken": attackerRefresh}); st != 401 {
		t.Fatalf("the earlier session still works: %d", st)
	}

	// 4. an unverified provider email with no account creates an unverified account
	email4 := e.email("fresh")
	res, err = e.svc.SocialSignIn(ctx, &identity.Claims{Provider: "apple", Subject: "a-4", Email: email4, EmailVerified: false}, "Given Family")
	if err != nil || !res.IsNew || res.User.EmailVerified || res.User.DisplayName != "Given Family" {
		t.Fatalf("unverified new account = %+v %v", res, err)
	}
}

func TestProviderOnlyAccountsHaveNoPassword(t *testing.T) {
	e := setupSocial(t)
	email := e.email("only")
	e.call("POST", "/auth/apple", map[string]any{"identityToken": "fake:s-" + email + ":" + email})
	for _, pw := range []string{"", "!", "supersecret1", "x"} {
		st, b, _ := e.call("POST", "/auth/login", map[string]string{"email": email, "password": pw})
		if st != 401 || code(b) != "invalid_credentials" {
			t.Fatalf("password %q = %d %v", pw, st, b)
		}
	}
}

func TestConcurrentFirstSignIns(t *testing.T) {
	e := setupSocial(t)
	email := e.email("race")
	var wg sync.WaitGroup
	ids := make(chan string, 6)
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := e.svc.SocialSignIn(context.Background(), &identity.Claims{Provider: "google", Subject: "race-sub", Email: email, EmailVerified: true}, "")
			if err != nil {
				errs <- err
				return
			}
			ids <- res.User.ID
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatalf("a concurrent sign-in failed: %v", err)
	}
	seen := map[string]bool{}
	for id := range ids {
		seen[id] = true
	}
	var n int
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM users WHERE email = $1`, email).Scan(&n) //nolint:errcheck
	if len(seen) != 1 || n != 1 {
		t.Fatalf("%d distinct users returned, %d rows: the same identity must be one account", len(seen), n)
	}
}

func TestGoogleFlow(t *testing.T) {
	e := setupSocial(t)
	email := e.email("g")

	st, b, _ := e.call("POST", "/auth/google/start", map[string]string{"loginHint": email})
	if st != 200 {
		t.Fatalf("start = %d %v", st, b)
	}
	authURL, _ := url.Parse(b["authUrl"].(string))
	// the fake consent page is the callback itself: follow it
	st, _, hd := e.call("GET", "/auth/google/callback?"+authURL.RawQuery, nil)
	loc, _ := url.Parse(hd.Get("Location"))
	if st != http.StatusFound || loc.Scheme != "blacmusic" || loc.Host != "auth" || loc.Path != "/google" || loc.Query().Get("status") != "ok" || loc.Query().Get("code") == "" {
		t.Fatalf("callback = %d %s", st, hd.Get("Location"))
	}
	oneTime := loc.Query().Get("code")

	// nothing exists until the app redeems the code
	var n int
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM users WHERE email = $1`, email).Scan(&n) //nolint:errcheck
	if n != 0 {
		t.Fatal("the callback created an account")
	}
	st, b, _ = e.call("POST", "/auth/google/exchange", map[string]string{"code": oneTime})
	u := user(b)
	if st != 200 || b["isNewUser"] != true || u["email"] != email || u["emailVerified"] != true || u["hasPassword"] != false || !strings.HasPrefix(u["displayName"].(string), "Google ") {
		t.Fatalf("exchange = %d %v", st, b)
	}
	// single use
	if st, b, _ = e.call("POST", "/auth/google/exchange", map[string]string{"code": oneTime}); st != 401 || code(b) != "invalid_token" {
		t.Fatalf("replayed code = %d %v", st, b)
	}
	// the state is single use too
	_, _, hd = e.call("GET", "/auth/google/callback?"+authURL.RawQuery, nil)
	if l, _ := url.Parse(hd.Get("Location")); l.Query().Get("reason") != "state_expired" {
		t.Fatalf("replayed state = %s", hd.Get("Location"))
	}
	// signing in again returns the same account
	_, b, _ = e.call("POST", "/auth/google/start", map[string]string{"loginHint": email})
	au2, _ := url.Parse(b["authUrl"].(string))
	_, _, hd = e.call("GET", "/auth/google/callback?"+au2.RawQuery, nil)
	l2, _ := url.Parse(hd.Get("Location"))
	_, b, _ = e.call("POST", "/auth/google/exchange", map[string]string{"code": l2.Query().Get("code")})
	if b["isNewUser"] != false || user(b)["id"] != u["id"] {
		t.Fatalf("second sign-in = %v", b)
	}
	// an empty start body is fine and uses the default fake identity
	if st, _, _ = e.call("POST", "/auth/google/start", nil); st != 200 {
		t.Fatalf("start without body = %d", st)
	}
}

func TestGoogleFlowFailures(t *testing.T) {
	e := setupSocial(t)
	ctx := context.Background()
	start := func() *url.URL {
		_, b, _ := e.call("POST", "/auth/google/start", map[string]string{"loginHint": e.email("f")})
		u, _ := url.Parse(b["authUrl"].(string))
		return u
	}
	reason := func(path string) string {
		_, _, hd := e.call("GET", path, nil)
		l, _ := url.Parse(hd.Get("Location"))
		if l.Query().Get("status") != "error" {
			t.Fatalf("expected an error redirect, got %s", hd.Get("Location"))
		}
		return l.Query().Get("reason")
	}
	// the user pressed Cancel at Google
	au := start()
	if r := reason("/auth/google/callback?error=access_denied&state=" + au.Query().Get("state")); r != "access_denied" {
		t.Fatalf("denied = %s", r)
	}
	if r := reason("/auth/google/callback?" + au.RawQuery); r != "state_expired" {
		t.Fatalf("a denied state must be gone: %s", r)
	}
	if r := reason("/auth/google/callback?state=never-issued&code=fake:x@y.com"); r != "state_expired" {
		t.Fatalf("unknown state = %s", r)
	}
	// an expired state
	au = start()
	e.pool.Exec(ctx, `UPDATE auth_google_states SET expires_at = now() - interval '1 second'`) //nolint:errcheck
	if r := reason("/auth/google/callback?" + au.RawQuery); r != "state_expired" {
		t.Fatalf("expired = %s", r)
	}
	// Google refuses the code
	au = start()
	if r := reason("/auth/google/callback?state=" + au.Query().Get("state") + "&code=real-looking-code"); r != "exchange_failed" {
		t.Fatalf("bad code = %s", r)
	}
	// no code at all
	au = start()
	if r := reason("/auth/google/callback?state=" + au.Query().Get("state")); r != "exchange_failed" {
		t.Fatalf("missing code = %s", r)
	}
	// a one-time code that has expired, and nonsense
	au = start()
	_, _, hd := e.call("GET", "/auth/google/callback?"+au.RawQuery, nil)
	l, _ := url.Parse(hd.Get("Location"))
	e.pool.Exec(ctx, `UPDATE auth_login_codes SET expires_at = now() - interval '1 second'`) //nolint:errcheck
	if st, b, _ := e.call("POST", "/auth/google/exchange", map[string]string{"code": l.Query().Get("code")}); st != 401 || code(b) != "invalid_token" {
		t.Fatalf("expired one-time code = %d %v", st, b)
	}
	if st, b, _ := e.call("POST", "/auth/google/exchange", map[string]string{"code": "nonsense"}); st != 401 {
		t.Fatalf("nonsense code = %d %v", st, b)
	}
	if st, _, _ := e.call("POST", "/auth/google/exchange", map[string]string{}); st != 400 {
		t.Fatalf("no code = %d", st)
	}
	// the one-time code is stored hashed
	au = start()
	_, _, hd = e.call("GET", "/auth/google/callback?"+au.RawQuery, nil)
	l, _ = url.Parse(hd.Get("Location"))
	var stored int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM auth_login_codes WHERE code_hash = $1`, l.Query().Get("code")).Scan(&stored) //nolint:errcheck
	if stored != 0 {
		t.Fatal("the one-time code is stored in clear text")
	}
}

func TestProvidersCanBeUnavailable(t *testing.T) {
	pool := testutil.Pool(t)
	svc := auth.NewService(pool, auth.NewTokenService(testutil.Secret, time.Minute), time.Hour)
	svc.SetSocial(auth.SocialConfig{AppRedirect: "blacmusic://auth/google"})
	h := auth.NewHandler(svc)
	r := chi.NewRouter()
	r.Post("/auth/apple", h.Apple)
	r.Post("/auth/google/start", h.GoogleStart)
	r.Get("/auth/google/callback", h.GoogleCallback)
	r.Post("/auth/google/exchange", h.GoogleExchange)
	do := func(method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec, out
	}
	if rec, b := do("POST", "/auth/apple", `{"identityToken":"x"}`); rec.Code != 503 || code(b) != "provider_unavailable" {
		t.Fatalf("apple = %d %v", rec.Code, b)
	}
	if rec, b := do("POST", "/auth/google/start", `{}`); rec.Code != 503 || code(b) != "provider_unavailable" {
		t.Fatalf("google start = %d %v", rec.Code, b)
	}
	if rec, _ := do("GET", "/auth/google/callback?state=x&code=y", ""); rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), "reason=unavailable") {
		t.Fatalf("callback = %d %s", rec.Code, rec.Header().Get("Location"))
	}
	// the code can still be redeemed or refused without Google configured
	if rec, _ := do("POST", "/auth/google/exchange", `{"code":"x"}`); rec.Code != 401 {
		t.Fatalf("exchange = %d", rec.Code)
	}
}

func TestRegisteringAgainReplacesAnUnconfirmedAccount(t *testing.T) {
	e := setupSocial(t)
	ctx := context.Background()
	email := e.email("again")

	// first sign-up, never confirmed
	_, first, _ := e.call("POST", "/auth/register", map[string]string{"email": email, "password": "old-password-1", "displayName": "Old Name"})
	oldID, oldRefresh := user(first)["id"].(string), first["refreshToken"].(string)
	e.pool.Exec(ctx, `INSERT INTO playlists (owner_id, name) VALUES ($1, 'left behind')`, oldID) //nolint:errcheck

	// the same address signs up again: it replaces the first account
	st, second, _ := e.call("POST", "/auth/register", map[string]string{"email": email, "password": "new-password-2", "displayName": "New Name"})
	if st != 201 || user(second)["id"] == oldID || user(second)["displayName"] != "New Name" || user(second)["emailVerified"] != false {
		t.Fatalf("second sign-up = %d %v", st, second)
	}
	if st, _, _ := e.call("POST", "/auth/login", map[string]string{"email": email, "password": "old-password-1"}); st != 401 {
		t.Fatalf("the old password still works: %d", st)
	}
	if st, _, _ := e.call("POST", "/auth/login", map[string]string{"email": email, "password": "new-password-2"}); st != 200 {
		t.Fatalf("the new password does not work: %d", st)
	}
	if st, _, _ := e.call("POST", "/auth/refresh", map[string]string{"refreshToken": oldRefresh}); st != 401 {
		t.Fatalf("the old session survived: %d", st)
	}
	var left int
	e.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM users WHERE email = $1) + (SELECT count(*) FROM playlists WHERE owner_id = $2)`, email, oldID).Scan(&left) //nolint:errcheck
	if left != 1 {
		t.Fatalf("expected one account and none of the old one's data, found %d rows", left)
	}
}

func TestConfirmedAndLinkedAccountsAreNeverReplaced(t *testing.T) {
	e := setupSocial(t)
	ctx := context.Background()
	taken := func(email string) {
		t.Helper()
		st, b, _ := e.call("POST", "/auth/register", map[string]string{"email": email, "password": "another-pass-3", "displayName": "Someone Else"})
		if st != 409 || code(b) != "email_taken" {
			t.Fatalf("register on a protected account = %d %v", st, b)
		}
		if st, _, _ := e.call("POST", "/auth/login", map[string]string{"email": email, "password": "original-pass-1"}); st != 200 && st != 401 {
			t.Fatalf("login = %d", st)
		}
	}
	mk := func(prefix string) (string, string) {
		email := e.email(prefix)
		_, b, _ := e.call("POST", "/auth/register", map[string]string{"email": email, "password": "original-pass-1", "displayName": "Owner"})
		return email, user(b)["id"].(string)
	}

	// confirmed
	email, id := mk("confirmed")
	e.pool.Exec(ctx, `UPDATE users SET email_verified_at = now() WHERE id = $1`, id) //nolint:errcheck
	taken(email)
	if st, _, _ := e.call("POST", "/auth/login", map[string]string{"email": email, "password": "original-pass-1"}); st != 200 {
		t.Fatalf("the confirmed account was changed: %d", st)
	}
	// unconfirmed but signed in with Apple/Google
	email, id = mk("linked")
	e.pool.Exec(ctx, `INSERT INTO auth_identities (provider, subject, user_id) VALUES ('apple', $1, $2)`, "sub-"+id, id) //nolint:errcheck
	taken(email)
	// unconfirmed but already an artist
	email, id = mk("artist")
	testutil.Artist(t, e.pool, id, testutil.Unique("a"), 1<<30)
	taken(email)
	var n int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE id = $1`, id).Scan(&n) //nolint:errcheck
	if n != 1 {
		t.Fatal("an artist's account was replaced")
	}
}
