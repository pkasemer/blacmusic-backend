package onboarding

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/testutil"
)

// The app keeps the ids, so the catalogue must stay well formed.
func TestCatalogue(t *testing.T) {
	rec := httptest.NewRecorder()
	NewHandler(nil, "").Config(rec, httptest.NewRequest("GET", "/config", nil))
	type item struct{ ID, Label string }
	var c struct {
		LanguageGroups []struct {
			item
			Languages []item
		}
		Regions []item
		Genres  []item
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil || len(c.LanguageGroups) == 0 || len(c.Regions) == 0 || len(c.Genres) == 0 {
		t.Fatalf("catalogue = %v %s", err, rec.Body)
	}
	lists := map[string][]item{"region": c.Regions, "genre": c.Genres}
	for _, g := range c.LanguageGroups {
		lists["group"] = append(lists["group"], g.item)
		lists["language"] = append(lists["language"], g.Languages...)
	}
	for kind, list := range lists {
		seen := map[string]bool{}
		for _, x := range list {
			if x.ID == "" || x.Label == "" || seen[x.ID] {
				t.Fatalf("%s %+v is incomplete or repeated", kind, x)
			}
			seen[x.ID] = true
		}
	}
}

func TestOnboarding(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()
	h := NewHandler(pool, "http://api.test")
	r := chi.NewRouter()
	r.Get("/artists", h.Artists)
	r.Group(func(g chi.Router) {
		g.Use(auth.Require(auth.NewTokenService(testutil.Secret, time.Minute)))
		g.Get("/preferences", h.Get)
		g.Post("/preferences", h.Save)
		g.Post("/complete", h.Complete)
	})
	call := func(method, path, token, body string) (int, string) {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code, strings.TrimSpace(rec.Body.String())
	}

	name := testutil.Unique("Onboard")
	owner := testutil.User(t, pool, "user", true)
	shown := testutil.Artist(t, pool, owner, name+" shown", testutil.GiB)
	rel := testutil.Release(t, pool, shown, "R", "single", "public")
	testutil.ReadyTrack(t, pool, owner, shown, rel, "T", 1, "public")
	silent := testutil.Artist(t, pool, testutil.User(t, pool, "user", true), name+" silent", testutil.GiB) // nothing online

	// no account needed to look at artists
	code, body := call("GET", "/artists?q="+strings.ReplaceAll(name, " ", "%20"), "", "")
	if code != 200 || !strings.Contains(body, shown) || strings.Contains(body, silent) {
		t.Fatalf("artists = %d %s", code, body)
	}
	if code, _ := call("GET", "/preferences", "", ""); code != 401 {
		t.Fatalf("signed out = %d", code)
	}

	user := testutil.User(t, pool, "user", true)
	token := testutil.Token(t, user)
	empty := `{"languages":{"preferred":[],"understood":[]},"regions":[],"genres":[],"artistIds":[],"completed":false}`
	if code, body := call("GET", "/preferences", token, ""); code != 200 || body != empty {
		t.Fatalf("before = %d %s", code, body)
	}
	save := `{"languages":{"preferred":["lg","lg"],"understood":["en","sw"]},"regions":["central"],"discoveryMode":"explore_more","genres":["afrobeats"],"artistIds":["art_nothing","` + shown + `"]}`
	for range 2 {
		if code, body := call("POST", "/preferences", token, save); code != 204 {
			t.Fatalf("save = %d %s", code, body)
		}
	}
	want := `{"languages":{"preferred":["lg"],"understood":["en","sw"]},"regions":["central"],"discoveryMode":"explore_more","genres":["afrobeats"],"artistIds":["` + shown + `"],"completed":false}`
	if _, body := call("GET", "/preferences", token, ""); body != want {
		t.Fatalf("after = %s", body)
	}
	if code, _ := call("POST", "/preferences", token, `{"discoveryMode":"loud"}`); code != 422 {
		t.Fatalf("bad mode = %d", code)
	}
	for range 2 {
		if code, body := call("POST", "/complete", token, `{}`); code != 204 {
			t.Fatalf("complete = %d %s", code, body)
		}
	}
	if _, body := call("GET", "/preferences", token, ""); !strings.Contains(body, `"completed":true`) {
		t.Fatalf("completed = %s", body)
	}
	count := func() (n int) {
		var seed []byte
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM recommendation_profiles WHERE user_id = $1`, user).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 1 {
			var m map[string]any
			if err := pool.QueryRow(ctx, `SELECT seed FROM recommendation_profiles WHERE user_id = $1`, user).Scan(&seed); err != nil || json.Unmarshal(seed, &m) != nil || m["discoveryMode"] != "explore_more" {
				t.Fatalf("seed = %s %v", seed, err)
			}
		}
		return n
	}
	if count() != 1 {
		t.Fatal("the recommendation profile was not started")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, user); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM listener_preferences WHERE user_id = $1) + (SELECT count(*) FROM recommendation_profiles WHERE user_id = $1)`, user).Scan(&left); err != nil || left != 0 {
		t.Fatalf("left after deletion = %d %v", left, err)
	}
}
