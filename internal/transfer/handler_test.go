package transfer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/testutil"
)

func (e *env) router() http.Handler {
	r := chi.NewRouter()
	r.Group(func(g chi.Router) {
		g.Use(auth.Require(auth.NewTokenService(testutil.Secret, time.Minute)))
		g.Route("/transfers", NewHandler(e.m).Routes)
	})
	return r
}

func (e *env) call(r http.Handler, method, path, key string, body any) (int, map[string]any) {
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Authorization", "Bearer "+testutil.Token(e.t, e.user))
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func errCode(b map[string]any) string {
	m, _ := b["error"].(map[string]any)
	s, _ := m["code"].(string)
	return s
}

func TestHTTPFlow(t *testing.T) {
	e := setup(t)
	r := e.router()

	code, b := e.call(r, "POST", "/transfers", "k1", map[string]any{"items": []map[string]any{{"videoId": "demoMukeneAud"}, {"videoId": "demoBlessedLyr", "title": "Blessed (single)"}}})
	if code != 201 || b["status"] != "collecting" || b["queue"] != nil {
		t.Fatalf("create = %d %v", code, b)
	}
	id := b["id"].(string)
	for _, k := range []string{"id", "status", "queue", "progress", "attempt", "maxAttempts", "nextAttemptAt", "error", "reason", "createdAt", "startedAt", "finishedAt", "items"} {
		if _, ok := b[k]; !ok {
			t.Fatalf("view lacks %q: %v", k, b)
		}
	}
	items := b["items"].([]any)
	first := items[0].(map[string]any)
	hint, _ := first["upload"].(map[string]any)
	if first["status"] != "awaiting_audio" || hint["releaseId"] != first["releaseId"] || hint["title"] != "Mukene" {
		t.Fatalf("item = %v", first)
	}
	if items[1].(map[string]any)["title"] != "Blessed (single)" {
		t.Fatalf("the artist's title was not used: %v", items[1])
	}
	if code, again := e.call(r, "POST", "/transfers", "k1", map[string]any{"items": []map[string]any{{"videoId": "demoMukeneAud"}}}); code != 201 || again["id"] != id {
		t.Fatalf("same key = %d %v", code, again)
	}
	if code, b = e.call(r, "POST", "/transfers", "k2", map[string]any{"items": []map[string]any{{"videoId": "demoBetterVis"}}}); code != 409 || errCode(b) != "transfer_active" {
		t.Fatalf("second open transfer = %d %v", code, b)
	}
	if code, b = e.call(r, "POST", "/transfers", "", map[string]any{"items": []map[string]any{}}); code != 422 {
		t.Fatalf("empty = %d %v", code, b)
	}
	if code, b = e.call(r, "GET", "/transfers/"+id, "", nil); code != 200 || b["id"] != id {
		t.Fatalf("get = %d", code)
	}
	if code, b = e.call(r, "GET", "/transfers", "", nil); code != 200 || len(b["transfers"].([]any)) != 1 {
		t.Fatalf("list = %d %v", code, b)
	}
	if code, b = e.call(r, "GET", "/transfers/tr_nope", "", nil); code != 404 {
		t.Fatalf("unknown = %d", code)
	}

	// submitting early names the songs that still need audio
	code, b = e.call(r, "POST", "/transfers/"+id+"/submit", "", nil)
	d, _ := b["error"].(map[string]any)["details"].(map[string]any)
	if code != 409 || errCode(b) != "audio_missing" || len(d["itemIds"].([]any)) != 2 {
		t.Fatalf("submit early = %d %v", code, b)
	}
	// attach, submit, watch the queue position
	tr := e.get(id)
	for _, it := range tr.Items {
		upID := e.upload(it, "queued")
		if code, b = e.call(r, "POST", "/transfers/"+id+"/items/"+it.ID+"/audio", "", map[string]string{"uploadId": upID}); code != 200 {
			t.Fatalf("attach = %d %v", code, b)
		}
	}
	if code, b = e.call(r, "POST", "/transfers/"+id+"/items/"+tr.Items[0].ID+"/audio", "", map[string]string{}); code != 400 {
		t.Fatalf("attach without uploadId = %d", code)
	}
	code, b = e.call(r, "POST", "/transfers/"+id+"/submit", "", nil)
	q, _ := b["queue"].(map[string]any)
	if code != 200 || b["status"] != "queued" || q["position"] != float64(1) || q["length"] != float64(1) {
		t.Fatalf("submit = %d %v", code, b)
	}
	if code, b = e.call(r, "POST", "/transfers/"+id+"/submit", "", nil); code != 409 || errCode(b) != "wrong_state" {
		t.Fatalf("submit twice = %d %v", code, b)
	}
	if code, b = e.call(r, "POST", "/transfers/"+id+"/cancel", "", nil); code != 200 || b["status"] != "cancelled" {
		t.Fatalf("cancel = %d %v", code, b)
	}
	if code, b = e.call(r, "POST", "/transfers/"+id+"/retry", "", nil); code != 409 {
		t.Fatalf("retry of a cancelled transfer = %d", code)
	}

	// anonymous and other users
	req := httptest.NewRequest("GET", "/transfers/"+id, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("anonymous = %d", rec.Code)
	}
	other := testutil.User(t, e.pool, "user", true)
	req = httptest.NewRequest("GET", "/transfers/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+testutil.Token(t, other))
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("someone else's transfer = %d", rec.Code)
	}
}

func TestServerSentEvents(t *testing.T) {
	e := setup(t)
	srv := httptest.NewServer(e.router())
	defer srv.Close()
	tr := e.create(ItemRequest{VideoID: "demoMukeneAud"})

	req, _ := http.NewRequest("GET", srv.URL+"/transfers/"+tr.ID+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+testutil.Token(t, e.user))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") || resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("headers = %d %v", resp.StatusCode, resp.Header)
	}
	sc := bufio.NewScanner(resp.Body)
	next := func() map[string]any {
		var event string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: ") && event == "transfer":
				var v map[string]any
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &v); err != nil {
					t.Fatal(err)
				}
				return v
			}
		}
		return nil
	}
	if v := next(); v == nil || v["status"] != "collecting" {
		t.Fatalf("first event = %v", v)
	}
	// a change is pushed without the client asking
	if _, err := e.m.Cancel(context.Background(), e.user, tr.ID); err != nil {
		t.Fatal(err)
	}
	if v := next(); v == nil || v["status"] != "cancelled" {
		t.Fatalf("second event = %v", v)
	}
	// the stream ends once the transfer is finished
	if next() != nil {
		t.Fatal("stream continued after the final state")
	}
}
