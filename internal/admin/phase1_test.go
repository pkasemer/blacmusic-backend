package admin

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/redis/go-redis/v9"

	"github.com/blacmusic/backend/internal/account"
	"github.com/blacmusic/backend/internal/artist"
	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/blobtest"
	"github.com/blacmusic/backend/internal/playlist"
	"github.com/blacmusic/backend/internal/release"
	"github.com/blacmusic/backend/internal/testutil"
	"github.com/blacmusic/backend/internal/upload"
)

type full struct {
	t      *testing.T
	r      http.Handler
	h      *Handler
	svc    *auth.Service
	rel    *release.Store
	admin  string // an administrator's user id
	adminT string // and the token
	blobs  *blobtest.Mem
}

func setupFull(t *testing.T) *full {
	t.Helper()
	pool := testutil.Pool(t)
	blobs := blobtest.New()
	h := NewHandler(pool, artist.NewStore(pool, 10*testutil.GiB, "http://api.test"), playlist.NewStore(pool, "http://api.test"))
	tokens := auth.NewTokenService(testutil.Secret, time.Minute)
	f := &full{t: t, h: h, svc: auth.NewService(pool, tokens, time.Hour), rel: release.NewStore(pool, blobs, "http://api.test"), blobs: blobs}
	h.Uploads = upload.NewHandler(upload.NewStore(pool), blobs, upload.Options{MaxBytes: 10 << 20, URLTTL: time.Hour, PartSize: 5000})
	h.Uploads.OnAdminComplete = h.OnUploadComplete
	bans := auth.NewBanCache(pool, time.Minute)
	h.Auth, h.Bans, h.PublicURL = f.svc, bans, "http://api.test"
	h.Accounts, h.Releases = account.NewService(pool, f.rel, blobs), f.rel
	if addr := os.Getenv("TEST_REDIS_ADDR"); addr != "" {
		h.Redis = redis.NewClient(&redis.Options{Addr: addr})
		t.Cleanup(func() { h.Redis.Close() })
	}
	r := chi.NewRouter()
	r.Post("/admin/auth/login", h.Login)
	r.Group(func(g chi.Router) {
		g.Use(auth.RequireWith(tokens, bans))
		g.Route("/admin", h.Routes)
		g.Get("/ping", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	})
	f.r = r
	f.admin = testutil.User(t, pool, "admin", true)
	f.adminT = testutil.Token(t, f.admin)
	return f
}

func errCode(b map[string]any) string {
	m, _ := b["error"].(map[string]any)
	s, _ := m["code"].(string)
	return s
}

func (f *full) audit(action, target string) int {
	f.t.Helper()
	var n int
	if err := f.h.pool.QueryRow(context.Background(), `SELECT count(*) FROM admin_audit_log WHERE action = $1 AND target_id = $2`, action, target).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func TestBanningAUserHidesTheirWorkAndUnbanRestoresIt(t *testing.T) {
	f := setupFull(t)
	pool, ctx := f.h.pool, context.Background()
	user := testutil.User(t, pool, "user", true)
	userT := testutil.Token(t, user)
	art := testutil.Artist(t, pool, user, testutil.Unique("Banned"), testutil.GiB)
	pub := testutil.Release(t, pool, art, "Live", "single", "public")
	testutil.ReadyTrack(t, pool, user, art, pub, "Live song", 1, "public")
	sched := testutil.Release(t, pool, art, "Soon", "single", "scheduled")
	testutil.ReadyTrack(t, pool, user, art, sched, "Soon song", 1, "draft")
	var pl string
	pool.QueryRow(ctx, `INSERT INTO playlists (owner_id, name, is_public) VALUES ($1, 'mine', true) RETURNING id`, user).Scan(&pl)       //nolint:errcheck
	pool.Exec(ctx, `INSERT INTO refresh_tokens (user_id, token_hash, expires_at) VALUES ($1, 'h-'||$1, now() + interval '1 day')`, user) //nolint:errcheck

	if code, _ := call(f.r, "GET", "/ping", userT, nil); code != 204 {
		t.Fatalf("before the ban = %d", code)
	}
	for name, c := range map[string]struct {
		body map[string]any
		want int
	}{"no reason": {map[string]any{}, 422}, "short reason": {map[string]any{"reason": "no"}, 422}} {
		if code, _ := call(f.r, "POST", "/admin/users/"+user+"/ban", f.adminT, c.body); code != c.want {
			t.Errorf("%s = %d, want %d", name, code, c.want)
		}
	}
	if code, b := call(f.r, "POST", "/admin/users/"+user+"/ban", f.adminT, map[string]any{"reason": "spam uploads"}); code != 204 {
		t.Fatalf("ban = %d %v", code, b)
	}
	if code, b := call(f.r, "POST", "/admin/users/"+user+"/ban", f.adminT, map[string]any{"reason": "again please"}); code != 409 || errCode(b) != "already_banned" {
		t.Fatalf("second ban = %d %v", code, b)
	}
	// the user is out everywhere
	if code, b := call(f.r, "GET", "/ping", userT, nil); code != 403 || errCode(b) != "account_banned" {
		t.Fatalf("a banned user's request = %d %v, want 403 account_banned", code, b)
	}
	if _, err := f.svc.Authenticate(ctx, "x@y.z", "nope"); err != auth.ErrInvalidCredentials {
		t.Fatalf("unknown login = %v", err)
	}
	var live, sch, tracks, pubPl, revoked int
	pool.QueryRow(ctx, `SELECT count(*) FROM albums WHERE id = $1 AND visibility = 'draft' AND taken_down_at IS NOT NULL`, pub).Scan(&live)                 //nolint:errcheck
	pool.QueryRow(ctx, `SELECT count(*) FROM albums WHERE id = $1 AND visibility = 'draft' AND taken_down_prev_visibility = 'scheduled'`, sched).Scan(&sch) //nolint:errcheck
	pool.QueryRow(ctx, `SELECT count(*) FROM tracks WHERE artist_id = $1 AND visibility = 'public'`, art).Scan(&tracks)                                     //nolint:errcheck
	pool.QueryRow(ctx, `SELECT count(*) FROM playlists WHERE id = $1 AND is_public`, pl).Scan(&pubPl)                                                       //nolint:errcheck
	pool.QueryRow(ctx, `SELECT count(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, user).Scan(&revoked)                                //nolint:errcheck
	if live != 1 || sch != 1 || tracks != 0 || pubPl != 0 || revoked != 0 {
		t.Fatalf("after the ban: taken down %d/%d, public tracks %d, public playlists %d, live sessions %d", live, sch, tracks, pubPl, revoked)
	}
	if _, err := f.rel.Publish(ctx, pub, user, nil); err != release.ErrTakenDown {
		t.Fatalf("publishing a taken-down release = %v, want ErrTakenDown", err)
	}
	if f.audit("user.ban", user) != 1 {
		t.Fatal("the ban was not written to the audit log")
	}
	// the user's detail shows the ban
	if code, b := call(f.r, "GET", "/admin/users/"+user, f.adminT, nil); code != 200 || b["banned"] != true || b["ban"].(map[string]any)["reason"] != "spam uploads" {
		t.Fatalf("detail = %d %v", code, b)
	}

	// unban puts everything back exactly
	if code, b := call(f.r, "POST", "/admin/users/"+user+"/unban", f.adminT, nil); code != 204 {
		t.Fatalf("unban = %d %v", code, b)
	}
	if code, _ := call(f.r, "POST", "/admin/users/"+user+"/unban", f.adminT, nil); code != 409 {
		t.Fatalf("second unban = %d, want 409", code)
	}
	var vis, schedVis string
	var prevNull bool
	pool.QueryRow(ctx, `SELECT visibility, taken_down_at IS NULL FROM albums WHERE id = $1`, pub).Scan(&vis, &prevNull) //nolint:errcheck
	pool.QueryRow(ctx, `SELECT visibility FROM albums WHERE id = $1`, sched).Scan(&schedVis)                            //nolint:errcheck
	pool.QueryRow(ctx, `SELECT count(*) FROM tracks WHERE artist_id = $1 AND visibility = 'public'`, art).Scan(&tracks) //nolint:errcheck
	pool.QueryRow(ctx, `SELECT count(*) FROM playlists WHERE id = $1 AND is_public`, pl).Scan(&pubPl)                   //nolint:errcheck
	if vis != "public" || !prevNull || schedVis != "scheduled" || tracks != 1 || pubPl != 1 {
		t.Fatalf("after unban: %s/%v, scheduled %s, public tracks %d, public playlists %d", vis, prevNull, schedVis, tracks, pubPl)
	}
	if code, _ := call(f.r, "GET", "/ping", userT, nil); code != 204 {
		t.Fatalf("after the unban = %d", code)
	}
	if f.audit("user.unban", user) != 1 {
		t.Fatal("the unban was not written to the audit log")
	}
	// administrators are never banned here
	if code, b := call(f.r, "POST", "/admin/users/"+f.admin+"/ban", f.adminT, map[string]any{"reason": "oops, myself"}); code != 409 || errCode(b) != "is_admin" {
		t.Fatalf("banning an admin = %d %v", code, b)
	}
}

func TestBanningAnArtistAlone(t *testing.T) {
	f := setupFull(t)
	pool, ctx := f.h.pool, context.Background()
	user := testutil.User(t, pool, "user", true)
	art := testutil.Artist(t, pool, user, testutil.Unique("Solo"), testutil.GiB)
	rel := testutil.Release(t, pool, art, "Single", "single", "public")
	testutil.ReadyTrack(t, pool, user, art, rel, "Song", 1, "public")

	if code, b := call(f.r, "POST", "/admin/artists/"+art+"/ban", f.adminT, map[string]any{"reason": "impersonation"}); code != 204 {
		t.Fatalf("ban = %d %v", code, b)
	}
	// the account still works, the profile is gone for everybody else, and nothing can be published
	if code, _ := call(f.r, "GET", "/ping", testutil.Token(t, user), nil); code != 204 {
		t.Fatalf("the account of a banned artist = %d, want it to work", code)
	}
	if _, err := artist.NewStore(pool, testutil.GiB, "http://api.test").Get(ctx, art, ""); err != artist.ErrNotFound {
		t.Fatalf("a banned profile for a stranger = %v, want ErrNotFound", err)
	}
	if a, err := artist.NewStore(pool, testutil.GiB, "http://api.test").Get(ctx, art, user); err != nil || !a.Banned {
		t.Fatalf("the owner sees it banned: %+v %v", a, err)
	}
	if _, err := f.rel.Publish(ctx, rel, user, nil); err == nil {
		t.Fatal("a banned artist published")
	}
	if code, b := call(f.r, "GET", "/admin/artists/"+art, f.adminT, nil); code != 200 || b["ban"] == nil {
		t.Fatalf("detail = %d %v", code, b)
	}
	if code, _ := call(f.r, "POST", "/admin/artists/"+art+"/unban", f.adminT, nil); code != 204 {
		t.Fatalf("unban = %d", code)
	}
	var vis string
	pool.QueryRow(ctx, `SELECT visibility FROM albums WHERE id = $1`, rel).Scan(&vis) //nolint:errcheck
	if vis != "public" {
		t.Fatalf("release after unban = %s", vis)
	}
	if f.audit("artist.ban", art) != 1 || f.audit("artist.unban", art) != 1 {
		t.Fatal("artist ban and unban must be audited")
	}
}

func TestUsersAndArtistsLists(t *testing.T) {
	f := setupFull(t)
	pool := f.h.pool
	tag := strings.ToLower(testutil.Unique("lst"))
	banned := testutil.User(t, pool, "user", true)
	pool.Exec(context.Background(), `UPDATE users SET email = $2, banned_at = now() WHERE id = $1`, banned, "banned-"+tag+"@example.com") //nolint:errcheck
	plain := testutil.User(t, pool, "user", false)
	pool.Exec(context.Background(), `UPDATE users SET email = $2 WHERE id = $1`, plain, "plain-"+tag+"@example.com") //nolint:errcheck

	code, b := call(f.r, "GET", "/admin/users?q="+tag, f.adminT, nil)
	if users, _ := b["users"].([]any); code != 200 || len(users) != 2 || b["total"] != float64(2) {
		t.Fatalf("users by q = %d %v", code, b)
	}
	if _, b = call(f.r, "GET", "/admin/users?q="+tag+"&status=banned", f.adminT, nil); b["total"] != float64(1) {
		t.Fatalf("banned = %v", b)
	}
	if _, b = call(f.r, "GET", "/admin/users?q="+tag+"&status=unverified", f.adminT, nil); b["total"] != float64(1) {
		t.Fatalf("unverified = %v", b)
	}
	if _, b = call(f.r, "GET", "/admin/users?q="+tag+"&limit=1", f.adminT, nil); len(b["users"].([]any)) != 1 || b["total"] != float64(2) {
		t.Fatalf("paging must keep the total = %v", b)
	}
	if code, _ = call(f.r, "GET", "/admin/users?status=bogus", f.adminT, nil); code != 400 {
		t.Fatalf("bad status = %d", code)
	}
	row := b["users"].([]any)[0].(map[string]any)
	for _, k := range []string{"id", "email", "displayName", "role", "emailVerified", "banned", "hasArtist", "createdAt"} {
		if _, ok := row[k]; !ok {
			t.Fatalf("user row lacks %q: %v", k, row)
		}
	}

	owner := testutil.User(t, pool, "user", true)
	art := testutil.Artist(t, pool, owner, "Listed "+tag, testutil.GiB)
	pool.Exec(context.Background(), `UPDATE artists SET verification = 'pending', verification_message = 'please', verification_requested_at = now() WHERE id = $1`, art) //nolint:errcheck
	code, b = call(f.r, "GET", "/admin/artists?q="+tag, f.adminT, nil)
	arts, _ := b["artists"].([]any)
	if code != 200 || len(arts) != 1 || b["total"] != float64(1) {
		t.Fatalf("artists by q = %d %v", code, b)
	}
	ar := arts[0].(map[string]any)
	if ar["verificationMessage"] != "please" || ar["requestedAt"] == nil || ar["ownerUserId"] != owner || ar["banned"] != false {
		t.Fatalf("artist row = %v", ar)
	}
	if _, b = call(f.r, "GET", "/admin/artists?q="+tag+"&verification=verified", f.adminT, nil); b["total"] != float64(0) {
		t.Fatalf("verified filter = %v", b)
	}
	if _, b = call(f.r, "GET", "/admin/artists?status=pending&q="+tag, f.adminT, nil); b["total"] != float64(1) {
		t.Fatalf("the old review-queue form = %v", b)
	}
	// verification decisions, including revoke, are audited
	if code, _ = call(f.r, "POST", "/admin/artists/"+art+"/verification", f.adminT, map[string]any{"decision": "approve"}); code != 204 {
		t.Fatalf("approve = %d", code)
	}
	if code, _ = call(f.r, "POST", "/admin/artists/"+art+"/verification", f.adminT, map[string]any{"decision": "revoke"}); code != 422 {
		t.Fatalf("revoke without a note = %d, want 422", code)
	}
	if code, _ = call(f.r, "POST", "/admin/artists/"+art+"/verification", f.adminT, map[string]any{"decision": "revoke", "note": "fake account"}); code != 204 {
		t.Fatalf("revoke = %d", code)
	}
	if code, b = call(f.r, "POST", "/admin/artists/"+art+"/verification", f.adminT, map[string]any{"decision": "revoke", "note": "again"}); code != 409 || errCode(b) != "not_verified" {
		t.Fatalf("revoking an unverified artist = %d %v", code, b)
	}
	if f.audit("artist.verify", art) != 1 || f.audit("artist.revoke", art) != 1 {
		t.Fatal("verification decisions must be audited")
	}
	// patch
	code, b = call(f.r, "PATCH", "/admin/artists/"+art, f.adminT, map[string]any{"name": "Renamed " + tag, "bio": "new bio"})
	if code != 200 || b["name"] != "Renamed "+tag || b["bio"] != "new bio" {
		t.Fatalf("patch = %d %v", code, b)
	}
	if code, _ = call(f.r, "PATCH", "/admin/artists/"+art, f.adminT, map[string]any{"name": " "}); code != 422 {
		t.Fatalf("empty name = %d", code)
	}
	if f.audit("artist.update", art) != 1 {
		t.Fatal("the edit must be audited")
	}
}

func TestStatsAndAuditLog(t *testing.T) {
	f := setupFull(t)
	code, b := call(f.r, "GET", "/admin/stats/overview", f.adminT, nil)
	if code != 200 {
		t.Fatalf("overview = %d %v", code, b)
	}
	for _, k := range []string{"users", "artists", "releases", "tracks", "shows", "plays", "storageBytes"} {
		if _, ok := b[k]; !ok {
			t.Fatalf("overview lacks %q: %v", k, b)
		}
	}
	for _, k := range []string{"total", "verifiedEmail", "banned", "new7d", "new30d"} {
		if _, ok := b["users"].(map[string]any)[k]; !ok {
			t.Fatalf("overview.users lacks %q", k)
		}
	}
	code, b = call(f.r, "GET", "/admin/stats/timeseries?metric=signups&from=2026-11-01&to=2026-11-10", f.adminT, nil)
	pts, _ := b["points"].([]any)
	if code != 200 || len(pts) != 10 || pts[0].(map[string]any)["date"] != "2026-11-01" {
		t.Fatalf("timeseries = %d %v (want 10 zero-filled days)", code, b)
	}
	if code, _ = call(f.r, "GET", "/admin/stats/timeseries?metric=signups&from=2024-01-01&to=2026-11-10", f.adminT, nil); code != 400 {
		t.Fatalf("a range over 366 days = %d, want 400", code)
	}
	if code, _ = call(f.r, "GET", "/admin/stats/timeseries?metric=nope", f.adminT, nil); code != 400 {
		t.Fatalf("bad metric = %d", code)
	}
	if code, b = call(f.r, "GET", "/admin/stats/timeseries?metric=plays&bucket=week&from=2026-11-02&to=2026-11-22", f.adminT, nil); code != 200 || len(b["points"].([]any)) != 3 {
		t.Fatalf("weekly = %d %v", code, b)
	}
	if code, b = call(f.r, "GET", "/admin/stats/top?kind=artists&range=7d&limit=5", f.adminT, nil); code != 200 || b["items"] == nil {
		t.Fatalf("top = %d %v", code, b)
	}
	if code, _ = call(f.r, "GET", "/admin/stats/top?kind=nope", f.adminT, nil); code != 400 {
		t.Fatalf("bad kind = %d", code)
	}

	// the log: read it, filter it, and it cannot be changed
	user := testutil.User(t, f.h.pool, "user", true)
	call(f.r, "POST", "/admin/users/"+user+"/ban", f.adminT, map[string]any{"reason": "for the log"})
	code, b = call(f.r, "GET", "/admin/audit-log?targetType=user&targetId="+user, f.adminT, nil)
	entries, _ := b["entries"].([]any)
	if code != 200 || len(entries) != 1 || b["total"] != float64(1) {
		t.Fatalf("audit log = %d %v", code, b)
	}
	e := entries[0].(map[string]any)
	if e["action"] != "user.ban" || e["reason"] != "for the log" || e["actor"].(map[string]any)["id"] != f.admin || e["targetType"] != "user" {
		t.Fatalf("entry = %v", e)
	}
	if _, b = call(f.r, "GET", "/admin/audit-log?action=user&targetId="+user, f.adminT, nil); b["total"] != float64(1) {
		t.Fatalf("an action group matches its members: %v", b)
	}
	if _, err := f.h.pool.Exec(context.Background(), `DELETE FROM admin_audit_log WHERE target_id = $1`, user); err == nil {
		t.Fatal("the audit log accepted a DELETE")
	}
	if _, err := f.h.pool.Exec(context.Background(), `UPDATE admin_audit_log SET reason = 'x' WHERE target_id = $1`, user); err == nil {
		t.Fatal("the audit log accepted an UPDATE")
	}
}

func TestAdminLoginAndLockout(t *testing.T) {
	f := setupFull(t)
	pool, ctx := f.h.pool, context.Background()
	if f.h.Redis != nil { // the per-address counter outlives a test run: start and end clean (httptest requests come from 192.0.2.1)
		f.h.Redis.Del(ctx, "adminlogin:ip:192.0.2.1")
		t.Cleanup(func() { f.h.Redis.Del(context.Background(), "adminlogin:ip:192.0.2.1") })
	}
	hash, err := auth.HashPassword("a-good-passphrase-1")
	if err != nil {
		t.Fatal(err)
	}
	mk := func(role string) string {
		id := testutil.User(t, pool, role, true)
		email := strings.ToLower(testutil.Unique(role)) + "@example.com"
		pool.Exec(ctx, `UPDATE users SET email = $2, password_hash = $3 WHERE id = $1`, id, email, hash) //nolint:errcheck
		return email
	}
	adminEmail, userEmail := mk("admin"), mk("user")
	sessions := func(email string) int {
		var n int
		pool.QueryRow(ctx, `SELECT count(*) FROM refresh_tokens t JOIN users u ON u.id = t.user_id WHERE u.email = $1`, email).Scan(&n) //nolint:errcheck
		return n
	}

	code, b := call(f.r, "POST", "/admin/auth/login", "", map[string]any{"email": adminEmail, "password": "a-good-passphrase-1"})
	if code != 200 || b["accessToken"] == nil || b["refreshToken"] == nil || b["user"].(map[string]any)["role"] != "admin" {
		t.Fatalf("admin login = %d %v", code, b)
	}
	// a normal user with the right password gets the same answer as a wrong password, and no session
	for name, c := range map[string]map[string]any{
		"a user":         {"email": userEmail, "password": "a-good-passphrase-1"},
		"wrong password": {"email": adminEmail, "password": "wrong-wrong-wrong"},
		"unknown email":  {"email": "nobody-" + userEmail, "password": "whatever-1234"},
	} {
		code, b = call(f.r, "POST", "/admin/auth/login", "", c)
		if code != 401 || errCode(b) != "invalid_credentials" || b["accessToken"] != nil {
			t.Errorf("%s = %d %v, want 401 invalid_credentials and no tokens", name, code, b)
		}
	}
	if sessions(userEmail) != 0 {
		t.Fatal("a non-admin got a session from the admin login")
	}
	var logins, failures int
	pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE action = 'login'), count(*) FILTER (WHERE action = 'login_failed') FROM admin_audit_log WHERE target_label IN ($1, $2)`, adminEmail, userEmail).Scan(&logins, &failures) //nolint:errcheck
	if logins != 1 || failures != 2 {
		t.Fatalf("audit: %d logins and %d failures, want 1 and 2", logins, failures)
	}

	if f.h.Redis == nil {
		t.Skip("TEST_REDIS_ADDR not set: skipping the lockout check")
	}
	locked := mk("admin")
	for i := 0; i < emailLimit; i++ {
		if code, _ = call(f.r, "POST", "/admin/auth/login", "", map[string]any{"email": locked, "password": "wrong-wrong-wrong"}); code != 401 {
			t.Fatalf("failure %d = %d", i+1, code)
		}
	}
	code, b = call(f.r, "POST", "/admin/auth/login", "", map[string]any{"email": locked, "password": "a-good-passphrase-1"})
	if code != 429 || errCode(b) != "too_many_attempts" {
		t.Fatalf("after %d failures even the right password = %d %v, want 429", emailLimit, code, b)
	}
	f.h.Redis.Del(ctx, "adminlogin:email:"+hashKey(locked))
}

func TestAdminVerifiesAnyArtist(t *testing.T) {
	f := setupFull(t)
	pool, ctx := f.h.pool, context.Background()
	owner := testutil.User(t, pool, "user", true)
	art := testutil.Artist(t, pool, owner, testutil.Unique("Direct"), testutil.GiB)
	path := "/admin/artists/" + art + "/verification"
	decide := func(body map[string]any) (int, map[string]any) { return call(f.r, "POST", path, f.adminT, body) }
	state := func() (s string) {
		pool.QueryRow(ctx, `SELECT verification || '/' || verification_note || '/' || flagged FROM artists WHERE id = $1`, art).Scan(&s) //nolint:errcheck
		return s
	}
	reasonOf := func(action string) (s string) {
		pool.QueryRow(ctx, `SELECT reason || '|' || (details->>'from') || '>' || (details->>'to') FROM admin_audit_log WHERE action = $1 AND target_id = $2 ORDER BY at DESC, id DESC LIMIT 1`, action, art).Scan(&s) //nolint:errcheck
		return s
	}

	if code, b := decide(map[string]any{"decision": "verify", "note": strings.Repeat("n", 501)}); code != 422 {
		t.Fatalf("long note = %d %v", code, b)
	}
	// every state without the badge can be verified, with or without a note
	for from, note := range map[string]string{"unverified": "  met in person ", "rejected": "", "pending": "documents checked"} {
		pool.Exec(ctx, `UPDATE artists SET verification = $2, verification_note = 'old', flagged = true WHERE id = $1`, art, from) //nolint:errcheck
		if code, b := decide(map[string]any{"decision": "verify", "note": note}); code != 204 {
			t.Fatalf("verify from %s = %d %v", from, code, b)
		}
		if got := state(); got != "verified//false" {
			t.Fatalf("after verify from %s: %s", from, got)
		}
		if got, want := reasonOf("artist.verify"), strings.TrimSpace(note)+"|"+from+">verified"; got != want {
			t.Fatalf("audit = %q, want %q", got, want)
		}
	}
	if code, b := decide(map[string]any{"decision": "verify"}); code != 409 || errCode(b) != "already_verified" {
		t.Fatalf("twice = %d %v", code, b)
	}
	if code, _ := decide(map[string]any{"decision": "revoke", "note": "mistake"}); code != 204 || state() != "unverified/mistake/false" {
		t.Fatalf("revoke = %d, %s", code, state())
	}
	// approve still needs a request, and keeps its note in the audit log only
	if code, b := decide(map[string]any{"decision": "approve"}); code != 409 || errCode(b) != "not_pending" {
		t.Fatalf("approve without a request = %d %v", code, b)
	}
	pool.Exec(ctx, `UPDATE artists SET verification = 'pending' WHERE id = $1`, art) //nolint:errcheck
	if code, _ := decide(map[string]any{"decision": "approve", "note": "ID seen"}); code != 204 || state() != "verified//false" || reasonOf("artist.verify") != "ID seen|pending>verified" {
		t.Fatalf("approve = %d, %s, %s", code, state(), reasonOf("artist.verify"))
	}

	// a catalog artist (no account) too; a banned artist or banned owner never
	var catalog string
	if err := pool.QueryRow(ctx, `INSERT INTO artists (name) VALUES ($1) RETURNING id`, testutil.Unique("Catalog")).Scan(&catalog); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM artists WHERE id = $1`, catalog) }) //nolint:errcheck
	if code, b := call(f.r, "POST", "/admin/artists/"+catalog+"/verification", f.adminT, map[string]any{"decision": "verify"}); code != 204 {
		t.Fatalf("catalog artist = %d %v", code, b)
	}
	pool.Exec(ctx, `UPDATE artists SET verification = 'unverified', banned_at = now() WHERE id = $1`, catalog) //nolint:errcheck
	if code, b := call(f.r, "POST", "/admin/artists/"+catalog+"/verification", f.adminT, map[string]any{"decision": "verify"}); code != 409 || errCode(b) != "artist_banned" {
		t.Fatalf("banned artist = %d %v", code, b)
	}
	pool.Exec(ctx, `UPDATE artists SET verification = 'unverified' WHERE id = $1`, art) //nolint:errcheck
	pool.Exec(ctx, `UPDATE users SET banned_at = now() WHERE id = $1`, owner)           //nolint:errcheck
	if code, b := decide(map[string]any{"decision": "verify"}); code != 409 || errCode(b) != "artist_banned" {
		t.Fatalf("banned owner = %d %v", code, b)
	}
	if code, _ := call(f.r, "POST", "/admin/artists/art_nope/verification", f.adminT, map[string]any{"decision": "verify"}); code != 404 {
		t.Fatalf("unknown = %d", code)
	}
}
