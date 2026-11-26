package push

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/testutil"
)

func tok(n string) string { return "ExponentPushToken[" + n + "abcdefgh]" }

type fakeExpo struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	sendBodies [][]ExpoMessage
	auth       string
	// per-message behaviour, by token
	ticketFor func(m ExpoMessage, n int) map[string]any
	sendCode  int
	receipts  map[string]map[string]any // ticket id -> receipt
	asked     [][]string
}

func newFakeExpo(t *testing.T) *fakeExpo {
	f := &fakeExpo{t: t, sendCode: 200, receipts: map[string]map[string]any{}}
	n := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/send", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.auth = r.Header.Get("Authorization")
		var msgs []ExpoMessage
		_ = json.NewDecoder(r.Body).Decode(&msgs)
		f.sendBodies = append(f.sendBodies, msgs)
		if f.sendCode != 200 {
			w.WriteHeader(f.sendCode)
			_, _ = w.Write([]byte(`{"errors":[{"code":"X","message":"nope"}]}`))
			return
		}
		out := make([]map[string]any, len(msgs))
		for i, m := range msgs {
			n++
			if f.ticketFor != nil {
				out[i] = f.ticketFor(m, n)
			} else {
				out[i] = map[string]any{"status": "ok", "id": fmt.Sprintf("ticket-%d", n)}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": out})
	})
	mux.HandleFunc("/receipts", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var req struct {
			IDs []string `json:"ids"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.asked = append(f.asked, req.IDs)
		data := map[string]any{}
		for _, id := range req.IDs {
			if rc, ok := f.receipts[id]; ok {
				data[id] = rc
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeExpo) client(token string) *Expo {
	e := NewExpo(token)
	e.SendURL, e.ReceiptsURL = f.srv.URL+"/send", f.srv.URL+"/receipts"
	return e
}

type env struct {
	t    *testing.T
	pool *pgxpool.Pool
	st   *Store
}

func setup(t *testing.T) *env {
	t.Helper()
	pool := testutil.Pool(t)
	testutil.SerializeOutbox(t, pool)
	// the sender works the global outbox: start empty so other runs' leftovers do not interfere
	pool.Exec(context.Background(), `DELETE FROM push_messages`) //nolint:errcheck
	return &env{t: t, pool: pool, st: NewStore(pool)}
}

func (e *env) status(token string) []string {
	rows, err := e.pool.Query(context.Background(), `SELECT status FROM push_messages WHERE token = $1 ORDER BY id`, token)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		rows.Scan(&s) //nolint:errcheck
		out = append(out, s)
	}
	return out
}

func (e *env) hasToken(token string) bool {
	var n int
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM push_tokens WHERE token = $1`, token).Scan(&n) //nolint:errcheck
	return n == 1
}

func TestTokens(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	u1, u2 := testutil.User(t, e.pool, "user", true), testutil.User(t, e.pool, "user", true)
	a := tok("aaaa" + u1[:6])

	for name, args := range map[string][3]string{
		"not an expo token": {"fcm:abcdef", "ios", ""},
		"empty":             {"", "ios", ""},
		"bad platform":      {a, "windows", ""},
	} {
		if err := e.st.Register(ctx, u1, args[0], args[1], args[2]); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := e.st.Register(ctx, u1, a, "ios", "Kato's iPhone"); err != nil {
		t.Fatal(err)
	}
	if err := e.st.Register(ctx, u1, a, "ios", "Kato's iPhone"); err != nil { // idempotent
		t.Fatal(err)
	}
	var n int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM push_tokens WHERE token = $1`, a).Scan(&n) //nolint:errcheck
	if n != 1 {
		t.Fatalf("%d rows for one token", n)
	}
	// the phone changes accounts: the token moves
	if err := e.st.Register(ctx, u2, a, "ios", "same phone"); err != nil {
		t.Fatal(err)
	}
	var owner string
	e.pool.QueryRow(ctx, `SELECT user_id::text FROM push_tokens WHERE token = $1`, a).Scan(&owner) //nolint:errcheck
	if owner != u2 {
		t.Fatal("the token did not move to the new account")
	}
	// removing someone else's token does nothing; removing your own works; unknown is fine
	e.st.Remove(ctx, u1, a) //nolint:errcheck
	if !e.hasToken(a) {
		t.Fatal("removed another user's token")
	}
	if err := e.st.Remove(ctx, u2, a); err != nil || e.hasToken(a) {
		t.Fatalf("remove own token: %v", err)
	}
	if err := e.st.Remove(ctx, u2, a); err != nil {
		t.Fatalf("removing twice: %v", err)
	}
	// at most 10 per user: the least recently used goes
	var first string
	for i := 0; i < MaxTokensPerUser+2; i++ {
		tk := tok(fmt.Sprintf("dev%02d%s", i, u1[:6]))
		if i == 0 {
			first = tk
		}
		if err := e.st.Register(ctx, u1, tk, "android", ""); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	e.pool.QueryRow(ctx, `SELECT count(*) FROM push_tokens WHERE user_id = $1`, u1).Scan(&n) //nolint:errcheck
	if n != MaxTokensPerUser || e.hasToken(first) {
		t.Fatalf("%d tokens, oldest kept: %v", n, e.hasToken(first))
	}
}

func TestSettingsAndEnqueue(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	u := testutil.User(t, e.pool, "user", true)
	st, err := e.st.Settings(ctx, u)
	if err != nil || !st.Transfers || !st.Releases {
		t.Fatalf("defaults = %+v %v", st, err)
	}
	off := false
	if st, err = e.st.SetSettings(ctx, u, SettingsPatch{Transfers: &off}); err != nil || st.Transfers || !st.Releases {
		t.Fatalf("partial update = %+v %v", st, err)
	}
	if st, _ = e.st.Settings(ctx, u); st.Transfers || !st.Releases {
		t.Fatalf("not persisted: %+v", st)
	}

	m := Message{Title: "T", Body: "B", Type: TypeTransfer, ID: "tr_1"}
	if n, err := Enqueue(ctx, e.pool, u, CategoryReleases, m, ""); err != nil || n != 0 {
		t.Fatalf("no device = %d %v", n, err)
	}
	t1, t2 := tok("one"+u[:6]), tok("two"+u[:6])
	e.st.Register(ctx, u, t1, "ios", "")     //nolint:errcheck
	e.st.Register(ctx, u, t2, "android", "") //nolint:errcheck
	if n, err := Enqueue(ctx, e.pool, u, CategoryTransfers, m, "k1"); err != nil || n != 0 {
		t.Fatalf("switched off = %d %v", n, err)
	}
	if n, err := Enqueue(ctx, e.pool, u, CategoryReleases, m, "k1"); err != nil || n != 2 {
		t.Fatalf("one message per device = %d %v", n, err)
	}
	if n, _ := Enqueue(ctx, e.pool, u, CategoryReleases, m, "k1"); n != 0 {
		t.Fatalf("the same event queued again: %d", n)
	}
	if n, _ := Enqueue(ctx, e.pool, u, CategoryReleases, m, ""); n != 2 {
		t.Fatalf("without a key it queues: %d", n)
	}
	if n, err := Enqueue(ctx, e.pool, "00000000-0000-0000-0000-000000000000", CategoryReleases, m, ""); err != nil || n != 0 {
		t.Fatalf("unknown user = %d %v", n, err)
	}
	var data string
	e.pool.QueryRow(ctx, `SELECT data::text FROM push_messages WHERE token = $1 ORDER BY id LIMIT 1`, t1).Scan(&data) //nolint:errcheck
	if !strings.Contains(data, `"type": "transfer"`) || !strings.Contains(data, `"id": "tr_1"`) || strings.Contains(data, "url") {
		t.Fatalf("data = %s", data)
	}
}

func TestSendingInBatches(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	f := newFakeExpo(t)
	u := testutil.User(t, e.pool, "user", true)
	for i := 0; i < 250; i++ {
		_, err := e.pool.Exec(ctx, `INSERT INTO push_messages (user_id, token, title, body, data) VALUES ($1, $2, 'Hi', 'There', '{"type":"release","id":"alb_1"}')`,
			u, tok(fmt.Sprintf("%03d", i)))
		if err != nil {
			t.Fatal(err)
		}
	}
	s := NewSender(e.pool, f.client("secret-token"), false)
	s.Tick(ctx)
	if len(f.sendBodies) != 3 || len(f.sendBodies[0]) != 100 || len(f.sendBodies[1]) != 100 || len(f.sendBodies[2]) != 50 {
		sizes := []int{}
		for _, b := range f.sendBodies {
			sizes = append(sizes, len(b))
		}
		t.Fatalf("batches = %v, want [100 100 50]", sizes)
	}
	m := f.sendBodies[0][0]
	if m.Sound != "default" || m.ChannelID != "default" || m.Title != "Hi" || m.Body != "There" || m.Data["type"] != "release" || m.Data["id"] != "alb_1" || !strings.HasPrefix(m.To, "ExponentPushToken[") {
		t.Fatalf("message = %+v", m)
	}
	if f.auth != "Bearer secret-token" {
		t.Fatalf("access token header = %q", f.auth)
	}
	var sent, waiting int
	e.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status = 'sent'), count(*) FILTER (WHERE receipt_check_at > now() + interval '14 minutes' AND ticket_id IS NOT NULL) FROM push_messages WHERE user_id = $1`, u).Scan(&sent, &waiting) //nolint:errcheck
	if sent != 250 || waiting != 250 {
		t.Fatalf("sent %d, awaiting a receipt in ~15 min: %d", sent, waiting)
	}
	// nothing is sent twice
	s.Tick(ctx)
	if len(f.sendBodies) != 3 {
		t.Fatalf("sent again: %d requests", len(f.sendBodies))
	}
	// no token configured: no Authorization header
	f2 := newFakeExpo(t)
	e.pool.Exec(ctx, `INSERT INTO push_messages (user_id, token, title, body) VALUES ($1, $2, 'a', 'b')`, u, tok("noauth")) //nolint:errcheck
	NewSender(e.pool, f2.client(""), false).Tick(ctx)
	if f2.auth != "" || len(f2.sendBodies) != 1 {
		t.Fatalf("auth header %q, requests %d", f2.auth, len(f2.sendBodies))
	}
}

func TestTicketErrorsAndRetries(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	f := newFakeExpo(t)
	u := testutil.User(t, e.pool, "user", true)
	gone, busy, bad, ok := tok("gone"+u[:6]), tok("busy"+u[:6]), tok("bad"+u[:6]), tok("ok"+u[:6])
	for _, tk := range []string{gone, busy, bad, ok} {
		e.st.Register(ctx, u, tk, "ios", "")                                                                         //nolint:errcheck
		e.pool.Exec(ctx, `INSERT INTO push_messages (user_id, token, title, body) VALUES ($1, $2, 'a', 'b')`, u, tk) //nolint:errcheck
	}
	// a second message waiting for the same dead device
	e.pool.Exec(ctx, `INSERT INTO push_messages (user_id, token, title, body, next_attempt_at) VALUES ($1, $2, 'later', 'b', now() + interval '1 hour')`, u, gone) //nolint:errcheck
	f.ticketFor = func(m ExpoMessage, n int) map[string]any {
		switch m.To {
		case gone:
			return map[string]any{"status": "error", "message": "gone", "details": map[string]any{"error": "DeviceNotRegistered"}}
		case busy:
			return map[string]any{"status": "error", "message": "slow down", "details": map[string]any{"error": "MessageRateExceeded"}}
		case bad:
			return map[string]any{"status": "error", "message": "too big", "details": map[string]any{"error": "MessageTooBig"}}
		}
		return map[string]any{"status": "ok", "id": "t-ok"}
	}
	NewSender(e.pool, f.client(""), false).Tick(ctx)

	if e.hasToken(gone) || !e.hasToken(ok) {
		t.Fatal("DeviceNotRegistered must delete that token only")
	}
	if got := e.status(gone); got[0] != "invalid_token" || got[1] != "invalid_token" {
		t.Fatalf("messages for the dead device = %v", got)
	}
	if got := e.status(busy); got[0] != "pending" {
		t.Fatalf("rate limited = %v, want pending (retry later)", got)
	}
	var due time.Duration
	var attempts int
	e.pool.QueryRow(ctx, `SELECT attempts, extract(epoch FROM next_attempt_at - now()) * interval '1 second' FROM push_messages WHERE token = $1`, busy).Scan(&attempts, &due) //nolint:errcheck
	if attempts != 1 || due < 20*time.Second || due > 40*time.Second {
		t.Fatalf("retry in %v after %d attempts", due, attempts)
	}
	if got := e.status(bad); got[0] != "failed" {
		t.Fatalf("other error = %v", got)
	}
	if got := e.status(ok); got[0] != "sent" {
		t.Fatalf("ok = %v", got)
	}
}

func TestRequestFailures(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	f := newFakeExpo(t)
	u := testutil.User(t, e.pool, "user", true)
	tk := tok("req" + u[:6])
	e.pool.Exec(ctx, `INSERT INTO push_messages (user_id, token, title, body) VALUES ($1, $2, 'a', 'b')`, u, tk) //nolint:errcheck
	s := NewSender(e.pool, f.client(""), false)

	// Expo down: retried with growing delays, then given up after 5 attempts
	f.sendCode = 503
	for attempt := 1; attempt <= maxSendAttempts; attempt++ {
		s.Tick(ctx)
		st := e.status(tk)[0]
		if attempt < maxSendAttempts && st != "pending" {
			t.Fatalf("attempt %d: %s, want pending", attempt, st)
		}
		e.pool.Exec(ctx, `UPDATE push_messages SET next_attempt_at = now() WHERE token = $1`, tk) //nolint:errcheck
	}
	if got := e.status(tk)[0]; got != "failed" {
		t.Fatalf("after %d attempts: %s", maxSendAttempts, got)
	}
	// a request Expo refuses outright (bad request) is not retried
	tk2 := tok("req2" + u[:6])
	e.pool.Exec(ctx, `INSERT INTO push_messages (user_id, token, title, body) VALUES ($1, $2, 'a', 'b')`, u, tk2) //nolint:errcheck
	f.sendCode = 400
	s.Tick(ctx)
	if got := e.status(tk2)[0]; got != "failed" {
		t.Fatalf("400 = %s", got)
	}
	// a sender that died mid-send: its messages are picked up again
	tk3 := tok("req3" + u[:6])
	e.pool.Exec(ctx, `INSERT INTO push_messages (user_id, token, title, body, status, lease_until) VALUES ($1, $2, 'a', 'b', 'sending', now() - interval '1 minute')`, u, tk3) //nolint:errcheck
	f.sendCode = 200
	s.Tick(ctx)
	if got := e.status(tk3)[0]; got != "sent" {
		t.Fatalf("abandoned message = %s", got)
	}
	// a message with a live lease is left alone
	tk4 := tok("req4" + u[:6])
	e.pool.Exec(ctx, `INSERT INTO push_messages (user_id, token, title, body, status, lease_until) VALUES ($1, $2, 'a', 'b', 'sending', now() + interval '1 minute')`, u, tk4) //nolint:errcheck
	s.Tick(ctx)
	if got := e.status(tk4)[0]; got != "sending" {
		t.Fatalf("leased message = %s", got)
	}
}

func TestReceipts(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	f := newFakeExpo(t)
	u := testutil.User(t, e.pool, "user", true)
	mk := func(name, ticket string, sentAgo time.Duration) string {
		tk := tok(name + u[:6])
		e.st.Register(ctx, u, tk, "ios", "") //nolint:errcheck
		e.pool.Exec(ctx, `INSERT INTO push_messages (user_id, token, title, body, status, ticket_id, sent_at, receipt_check_at)
			VALUES ($1, $2, 'a', 'b', 'sent', $3, now() - make_interval(secs => $4), now() - interval '1 second')`, u, tk, ticket, sentAgo.Seconds()) //nolint:errcheck
		return tk
	}
	delivered := mk("deliv", "r-ok", 20*time.Minute)
	dead := mk("dead", "r-dead", 20*time.Minute)
	failed := mk("fail", "r-fail", 20*time.Minute)
	missing := mk("miss", "r-missing", 20*time.Minute)
	stale := mk("stale", "r-stale", 25*time.Hour)
	notDue := tok("notdue" + u[:6])
	e.pool.Exec(ctx, `INSERT INTO push_messages (user_id, token, title, body, status, ticket_id, sent_at, receipt_check_at)
		VALUES ($1, $2, 'a', 'b', 'sent', 'r-later', now(), now() + interval '10 minutes')`, u, notDue) //nolint:errcheck
	f.receipts["r-ok"] = map[string]any{"status": "ok"}
	f.receipts["r-dead"] = map[string]any{"status": "error", "message": "gone", "details": map[string]any{"error": "DeviceNotRegistered"}}
	f.receipts["r-fail"] = map[string]any{"status": "error", "message": "APNs refused", "details": map[string]any{"error": "InvalidCredentials"}}

	NewSender(e.pool, f.client(""), false).Tick(ctx)

	for tk, want := range map[string]string{delivered: "delivered", dead: "invalid_token", failed: "failed", missing: "sent", stale: "expired", notDue: "sent"} {
		if got := e.status(tk)[0]; got != want {
			t.Errorf("%s: %s, want %s", tk[:20], got, want)
		}
	}
	if e.hasToken(dead) || !e.hasToken(delivered) {
		t.Fatal("only the device that is gone loses its token")
	}
	asked := strings.Join(f.asked[0], ",")
	if strings.Contains(asked, "r-later") {
		t.Fatal("a receipt that is not due yet was asked for")
	}
	var again time.Duration
	e.pool.QueryRow(ctx, `SELECT extract(epoch FROM receipt_check_at - now()) * interval '1 second' FROM push_messages WHERE token = $1`, missing).Scan(&again) //nolint:errcheck
	if again < 10*time.Minute {
		t.Fatalf("a receipt Expo has not produced yet should be asked for again in ~15 min, got %v", again)
	}
}

func TestFakeModeSendsNothing(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	u := testutil.User(t, e.pool, "user", true)
	tk := tok("fk" + u[:6])
	e.pool.Exec(ctx, `INSERT INTO push_messages (user_id, token, title, body) VALUES ($1, $2, 'a', 'b')`, u, tk) //nolint:errcheck
	NewSender(e.pool, nil, true).Tick(ctx)                                                                       // no gateway at all
	if got := e.status(tk)[0]; got != "delivered" {
		t.Fatalf("fake mode = %s", got)
	}
}

func TestHandlers(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	h := NewHandler(e.st)
	r := chi.NewRouter()
	r.Group(func(g chi.Router) {
		g.Use(auth.Require(auth.NewTokenService(testutil.Secret, time.Minute)))
		h.Routes(g)
		h.DevRoutes(g)
	})
	u := testutil.User(t, e.pool, "user", true)
	call := func(method, path string, body any) (int, map[string]any) {
		var raw []byte
		if body != nil {
			raw, _ = json.Marshal(body)
		}
		req := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
		req.Header.Set("Authorization", "Bearer "+testutil.Token(t, u))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	tk := tok("h" + u[:6])
	if code, _ := call("POST", "/me/push-tokens", map[string]string{"token": "nope", "platform": "ios"}); code != 422 {
		t.Fatalf("bad token = %d", code)
	}
	if code, _ := call("POST", "/me/push-tokens", map[string]string{"token": tk, "platform": "ios", "deviceName": "iPhone"}); code != 204 {
		t.Fatalf("register = %d", code)
	}
	if code, _ := call("POST", "/me/push-tokens", map[string]string{"token": tk, "platform": "ios", "deviceName": "iPhone"}); code != 204 {
		t.Fatalf("register twice = %d", code)
	}
	// pushEnabled on the user object follows the tokens
	svc := auth.NewService(e.pool, auth.NewTokenService(testutil.Secret, time.Minute), time.Hour)
	if me, err := svc.Me(ctx, u); err != nil || !me.PushEnabled {
		t.Fatalf("pushEnabled = %+v %v", me, err)
	}
	if code, b := call("GET", "/me/notification-settings", nil); code != 200 || b["transfers"] != true || b["releases"] != true {
		t.Fatalf("settings = %d %v", code, b)
	}
	if code, b := call("PUT", "/me/notification-settings", map[string]bool{"releases": false}); code != 200 || b["releases"] != false || b["transfers"] != true {
		t.Fatalf("put settings = %d %v", code, b)
	}
	if code, b := call("GET", "/me/notification-settings", nil); code != 200 || b["releaseEmails"] != true || b["releases"] != false {
		t.Fatalf("releaseEmails default = %d %v", code, b)
	}
	if code, b := call("PUT", "/me/notification-settings", map[string]bool{"releaseEmails": false}); code != 200 || b["releaseEmails"] != false || b["releases"] != false || b["transfers"] != true {
		t.Fatalf("put releaseEmails = %d %v (the other switches must keep their values)", code, b)
	}
	if code, _ := call("PUT", "/me/notification-settings", map[string]any{}); code != 400 {
		t.Fatalf("empty put = %d", code)
	}
	// the dev outbox shows what was queued for this user
	Enqueue(ctx, e.pool, u, CategoryTransfers, Message{Title: "Import finished", Body: "done", Type: TypeTransfer, ID: "tr_9"}, "") //nolint:errcheck
	code, b := call("GET", "/dev/push-outbox", nil)
	msgs, _ := b["messages"].([]any)
	if code != 200 || len(msgs) != 1 || msgs[0].(map[string]any)["title"] != "Import finished" || msgs[0].(map[string]any)["data"].(map[string]any)["id"] != "tr_9" {
		t.Fatalf("outbox = %d %v", code, b)
	}
	if code, _ := call("DELETE", "/dev/push-outbox", nil); code != 204 {
		t.Fatalf("clear = %d", code)
	}
	if _, b = call("GET", "/dev/push-outbox", nil); len(b["messages"].([]any)) != 0 {
		t.Fatal("outbox not cleared")
	}
	// sign-out removes the token
	if code, _ := call("DELETE", "/me/push-tokens", map[string]string{"token": tk}); code != 204 || e.hasToken(tk) {
		t.Fatal("token not removed")
	}
	if me, _ := svc.Me(ctx, u); me.PushEnabled {
		t.Fatal("pushEnabled stayed true")
	}
	if code, _ := call("DELETE", "/me/push-tokens", map[string]string{}); code != 400 {
		t.Fatalf("delete without token = %d", code)
	}
	// deleting the account removes the tokens and queued messages
	e.st.Register(ctx, u, tk, "ios", "")                   //nolint:errcheck
	e.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, u) //nolint:errcheck
	if e.hasToken(tk) {
		t.Fatal("token outlived the account")
	}
}
