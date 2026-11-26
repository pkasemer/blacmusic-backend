package youtube

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/testutil"
)

type env struct {
	t     *testing.T
	h     *Handler
	fake  *Fake
	r     http.Handler
	store *Store
	hooks []string
}

func setup(t *testing.T) *env {
	t.Helper()
	pool := testutil.Pool(t)
	e := &env{t: t, fake: NewFake("http://api.test/v1/integrations/youtube/callback"), store: NewStore(pool)}
	vault, _ := NewVault(make([]byte, 32))
	e.h = NewHandler(e.store, e.fake, vault, "blacmusic://youtube/connected")
	e.h.OnDisconnect = func(_ context.Context, uid string) error { e.hooks = append(e.hooks, uid); return nil }
	r := chi.NewRouter()
	r.Get("/callback", e.h.Callback)
	r.Group(func(g chi.Router) {
		g.Use(auth.Require(auth.NewTokenService(testutil.Secret, time.Minute)))
		g.Route("/yt", e.h.Routes)
	})
	e.r = r
	return e
}

func (e *env) call(method, path, user string) (int, map[string]any, http.Header) {
	req := httptest.NewRequest(method, path, bytes.NewReader(nil))
	if user != "" {
		req.Header.Set("Authorization", "Bearer "+testutil.Token(e.t, user))
	}
	rec := httptest.NewRecorder()
	e.r.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec.Header()
}

// connect runs the whole consent flow and returns the app redirect.
func (e *env) connect(user string) *url.URL {
	e.t.Helper()
	code, body, _ := e.call("POST", "/yt/connect", user)
	if code != 200 {
		e.t.Fatalf("connect = %d %v", code, body)
	}
	auth, _ := url.Parse(body["authUrl"].(string))
	code, _, hd := e.call("GET", "/callback?"+auth.RawQuery, "")
	if code != http.StatusFound {
		e.t.Fatalf("callback = %d", code)
	}
	loc, _ := url.Parse(hd.Get("Location"))
	return loc
}

func TestConnectFlow(t *testing.T) {
	e := setup(t)
	pool := testutil.Pool(t)
	user := testutil.User(t, pool, "user", true)

	e.h.AudioImport = true
	if code, b, _ := e.call("GET", "/yt/", user); code != 200 || b["connected"] != false {
		t.Fatalf("before connecting = %d %v", code, b)
	}
	loc := e.connect(user)
	if loc.Scheme != "blacmusic" || loc.Query().Get("status") != "ok" {
		t.Fatalf("redirect = %s", loc)
	}
	_, b, _ := e.call("GET", "/yt/", user)
	ch, _ := b["channel"].(map[string]any)
	if b["audioImport"] != true {
		t.Fatalf("audioImport missing: %v", b)
	}
	if b["connected"] != true || ch["title"] != "Akeine (demo channel)" || b["needsReconnect"] != false {
		t.Fatalf("status = %v", b)
	}
	// the refresh token is encrypted at rest and never returned
	var enc []byte
	if err := pool.QueryRow(context.Background(), `SELECT refresh_token_enc FROM youtube_connections WHERE user_id = $1`, user).Scan(&enc); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(enc), "fake-refresh") || len(enc) < 20 {
		t.Fatal("refresh token not encrypted")
	}
	if raw, _ := json.Marshal(b); strings.Contains(string(raw), "fake-refresh") {
		t.Fatal("token leaked in the status response")
	}

	// a state works once
	_, body, _ := e.call("POST", "/yt/connect", user)
	au, _ := url.Parse(body["authUrl"].(string))
	e.call("GET", "/callback?"+au.RawQuery, "")
	_, _, hd := e.call("GET", "/callback?"+au.RawQuery, "")
	if l, _ := url.Parse(hd.Get("Location")); l.Query().Get("reason") != "state_expired" {
		t.Fatalf("replayed state = %s", hd.Get("Location"))
	}
	// unknown state, user denied access
	_, _, hd = e.call("GET", "/callback?state=nope&code=fake-code", "")
	if l, _ := url.Parse(hd.Get("Location")); l.Query().Get("status") != "error" {
		t.Fatalf("unknown state = %s", hd.Get("Location"))
	}
	_, body, _ = e.call("POST", "/yt/connect", user)
	au, _ = url.Parse(body["authUrl"].(string))
	_, _, hd = e.call("GET", "/callback?error=access_denied&state="+au.Query().Get("state"), "")
	if l, _ := url.Parse(hd.Get("Location")); l.Query().Get("reason") != "access_denied" {
		t.Fatalf("denied = %s", hd.Get("Location"))
	}
	// the denied state is gone too
	_, _, hd = e.call("GET", "/callback?state="+au.Query().Get("state")+"&code=fake-code", "")
	if l, _ := url.Parse(hd.Get("Location")); l.Query().Get("status") != "error" {
		t.Fatal("a denied state must not be reusable")
	}
	// an expired state
	_, body, _ = e.call("POST", "/yt/connect", user)
	au, _ = url.Parse(body["authUrl"].(string))
	pool.Exec(context.Background(), `UPDATE youtube_oauth_states SET expires_at = now() - interval '1 second' WHERE state = $1`, au.Query().Get("state")) //nolint:errcheck
	_, _, hd = e.call("GET", "/callback?"+au.RawQuery, "")
	if l, _ := url.Parse(hd.Get("Location")); l.Query().Get("reason") != "state_expired" {
		t.Fatalf("expired state = %s", hd.Get("Location"))
	}

	// requires a login
	if code, _, _ := e.call("POST", "/yt/connect", ""); code != 401 {
		t.Fatalf("anonymous connect = %d", code)
	}
}

func TestDiscovery(t *testing.T) {
	e := setup(t)
	pool := testutil.Pool(t)
	user := testutil.User(t, pool, "user", true)
	if code, b, _ := e.call("GET", "/yt/videos", user); code != 409 || b["error"].(map[string]any)["code"] != "youtube_not_connected" {
		t.Fatalf("not connected = %d %v", code, b)
	}
	e.connect(user)

	// the first listing syncs by itself and returns one entry per song
	code, b, _ := e.call("GET", "/yt/videos", user)
	vs, _ := b["videos"].([]any)
	if code != 200 || len(vs) != 4 || b["total"] != float64(4) {
		t.Fatalf("videos = %d %v", code, b)
	}
	first := vs[0].(map[string]any)
	for _, k := range []string{"videoId", "title", "publishedAt", "durationSec", "kind", "groupId", "primary", "variants", "suggested", "alreadyImported"} {
		if _, ok := first[k]; !ok {
			t.Fatalf("video lacks %q: %v", k, first)
		}
	}
	if e.fake.Calls["videos"] != 1 {
		t.Fatalf("videos fetched %d times", e.fake.Calls["videos"])
	}
	e.call("GET", "/yt/videos?limit=1&offset=1", user)
	if e.fake.Calls["videos"] != 1 {
		t.Fatal("the cache should serve paging without calling YouTube again")
	}
	if _, b, _ = e.call("GET", "/yt/videos?limit=1&offset=1", user); len(b["videos"].([]any)) != 1 || b["total"] != float64(4) {
		t.Fatalf("paging = %v", b)
	}
	// music=0 shows shorts and promos as well
	if _, b, _ = e.call("GET", "/yt/videos?music=0&limit=200", user); b["total"].(float64) <= 4 {
		t.Fatalf("music=0 total = %v", b["total"])
	}
	// an explicit refresh re-reads the channel
	e.fake.List = e.fake.List[:2]
	if code, b, _ = e.call("POST", "/yt/sync", user); code != 200 || b["videoCount"] != float64(2) {
		t.Fatalf("sync = %d %v", code, b)
	}
	if e.fake.Calls["videos"] != 2 {
		t.Fatal("sync must call YouTube")
	}

	// a song already in the catalog is flagged
	artistID := testutil.Artist(t, pool, user, testutil.Unique("a"), 1<<30)
	rel := testutil.Release(t, pool, artistID, "r", "single", "public")
	testutil.ReadyTrack(t, pool, user, artistID, rel, "Mukene (feat. Someone)", 1, "public")
	_, b, _ = e.call("GET", "/yt/videos", user)
	m := b["videos"].([]any)[0].(map[string]any)
	if m["alreadyImported"] != true || m["duplicateOf"] != "Mukene (feat. Someone)" {
		t.Fatalf("already imported = %v", m)
	}
}

func TestRevokedAccessAndDisconnect(t *testing.T) {
	e := setup(t)
	pool := testutil.Pool(t)
	user := testutil.User(t, pool, "user", true)
	e.connect(user)
	e.call("GET", "/yt/videos", user)

	// Google says the refresh token is no longer valid
	e.fake.RefreshErr = ErrInvalidGrant
	code, b, _ := e.call("POST", "/yt/sync", user)
	if code != 409 || b["error"].(map[string]any)["code"] != "reconnect_required" {
		t.Fatalf("revoked = %d %v", code, b)
	}
	if _, st, _ := e.call("GET", "/yt/", user); st["needsReconnect"] != true {
		t.Fatalf("status = %v", st)
	}
	e.fake.RefreshErr = nil
	if code, _, _ = e.call("POST", "/yt/sync", user); code != 409 {
		t.Fatal("a connection that needs reconnecting must not call Google until reconnected")
	}
	e.connect(user) // reconnecting clears it
	if _, st, _ := e.call("GET", "/yt/", user); st["needsReconnect"] != false {
		t.Fatal("reconnect did not reset the state")
	}
	if code, _, _ = e.call("POST", "/yt/sync", user); code != 200 {
		t.Fatalf("sync after reconnect = %d", code)
	}

	// quota and Google errors are transient and reported as such
	e.fake.VideosErr = ErrQuota
	if code, b, hd := e.call("POST", "/yt/sync", user); code != 503 || hd.Get("Retry-After") == "" || b["error"].(map[string]any)["code"] != "youtube_quota" {
		t.Fatalf("quota = %d %v", code, b)
	}
	e.fake.VideosErr = &APIError{Status: 500}
	if code, _, _ := e.call("POST", "/yt/sync", user); code != 502 {
		t.Fatalf("google 500 = %d", code)
	}
	e.fake.VideosErr = nil

	// disconnect revokes at Google and removes everything
	if code, _, _ := e.call("DELETE", "/yt/", user); code != 204 {
		t.Fatalf("disconnect = %d", code)
	}
	if len(e.fake.Revoked) != 1 || e.fake.Revoked[0] != "fake-refresh" {
		t.Fatalf("revoked at Google: %v", e.fake.Revoked)
	}
	var n int
	pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM youtube_connections WHERE user_id = $1) + (SELECT count(*) FROM youtube_videos WHERE user_id = $1)`, user).Scan(&n) //nolint:errcheck
	if n != 0 {
		t.Fatalf("%d rows left after disconnect", n)
	}
	if len(e.hooks) != 1 || e.hooks[0] != user {
		t.Fatalf("disconnect hook = %v", e.hooks)
	}
	if code, _, _ := e.call("DELETE", "/yt/", user); code != 204 {
		t.Fatal("disconnecting twice must be fine")
	}
	// a Google outage while revoking does not keep the tokens
	e.connect(user)
	e.fake.RevokeErr = &APIError{Status: 500}
	if code, _, _ := e.call("DELETE", "/yt/", user); code != 204 {
		t.Fatal("revoke failure must not block disconnecting")
	}
	if _, st, _ := e.call("GET", "/yt/", user); st["connected"] != false {
		t.Fatal("still connected")
	}
}

func TestUnavailableWithoutConfiguration(t *testing.T) {
	pool := testutil.Pool(t)
	h := NewHandler(NewStore(pool), nil, nil, "blacmusic://youtube/connected")
	r := chi.NewRouter()
	r.Get("/callback", h.Callback)
	r.Group(func(g chi.Router) {
		g.Use(auth.Require(auth.NewTokenService(testutil.Secret, time.Minute)))
		g.Route("/yt", h.Routes)
	})
	user := testutil.User(t, pool, "user", true)
	req := httptest.NewRequest("POST", "/yt/connect", nil)
	req.Header.Set("Authorization", "Bearer "+testutil.Token(t, user))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "youtube_unavailable") {
		t.Fatalf("unconfigured = %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/callback?state=x&code=y", nil))
	if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), "unavailable") {
		t.Fatalf("callback unconfigured = %d %s", rec.Code, rec.Header().Get("Location"))
	}
}

func TestStaleCacheIsPurgedAndRefetched(t *testing.T) {
	e := setup(t)
	pool := testutil.Pool(t)
	ctx := context.Background()
	user := testutil.User(t, pool, "user", true)
	e.connect(user)
	e.call("GET", "/yt/videos", user) // first listing reads the channel
	if e.fake.Calls["videos"] != 1 {
		t.Fatalf("videos fetched %d times", e.fake.Calls["videos"])
	}
	// younger than the limit: kept
	if n, err := e.store.PurgeStale(ctx, 25*24*time.Hour); err != nil || n != 0 {
		t.Fatalf("a fresh cache was purged: %d %v", n, err)
	}
	pool.Exec(ctx, `UPDATE youtube_videos SET fetched_at = now() - interval '26 days' WHERE user_id = $1`, user) //nolint:errcheck
	other := testutil.User(t, pool, "user", true)
	e.connect(other)
	e.call("GET", "/yt/videos", other) // another user's fresh cache must survive
	if n, err := e.store.PurgeStale(ctx, 25*24*time.Hour); err != nil || n != 1 {
		t.Fatalf("purge = %d %v, want 1 connection", n, err)
	}
	var mine, theirs int
	pool.QueryRow(ctx, `SELECT count(*) FROM youtube_videos WHERE user_id = $1`, user).Scan(&mine)    //nolint:errcheck
	pool.QueryRow(ctx, `SELECT count(*) FROM youtube_videos WHERE user_id = $1`, other).Scan(&theirs) //nolint:errcheck
	if mine != 0 || theirs == 0 {
		t.Fatalf("stale user has %d videos, other user %d", mine, theirs)
	}
	// the next listing reads the channel again instead of showing an empty list
	_, b, _ := e.call("GET", "/yt/videos", user)
	if len(b["videos"].([]any)) == 0 || e.fake.Calls["videos"] < 3 {
		t.Fatalf("after the purge: %v, channel reads %d", b["total"], e.fake.Calls["videos"])
	}
}
