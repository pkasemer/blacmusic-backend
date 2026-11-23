package podcast

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/playback"
	"github.com/blacmusic/backend/internal/source"
	podcastsrc "github.com/blacmusic/backend/internal/source/podcast"
)

type api struct {
	t      *testing.T
	env    *env
	router http.Handler
	tokens *auth.TokenService
}

func setupAPI(t *testing.T) *api {
	t.Helper()
	e := setup(t)
	tokens := auth.NewTokenService("test-secret-test-secret-test-secret!", time.Minute)
	reg := source.NewRegistry()
	reg.Register(podcastsrc.New(e.pool))
	h := NewHandler(e.store)
	pb := playback.NewHandler(e.pool, reg)

	r := chi.NewRouter()
	r.Group(func(g chi.Router) {
		g.Use(auth.Require(tokens))
		h.Routes(g, func(next http.Handler) http.Handler { return next })
		g.Get("/playback/{id}", pb.Resolve)
	})
	return &api{t: t, env: e, router: r, tokens: tokens}
}

func (a *api) token() string {
	a.t.Helper()
	tok, _, err := a.tokens.NewAccess(a.env.user())
	if err != nil {
		a.t.Fatal(err)
	}
	return tok
}

func (a *api) do(method, path, token string, body any) (int, map[string]any) {
	a.t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	a.router.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func code(m map[string]any) string {
	if e, ok := m["error"].(map[string]any); ok {
		s, _ := e["code"].(string)
		return s
	}
	return ""
}

func TestPodcastAPIEndToEnd(t *testing.T) {
	a := setupAPI(t)
	tok := a.token()
	a.env.feed.set(rss("API Show", 1, 2, 3), "", 0)

	// Add by feed URL: 201 and subscribed; the same URL again: 200.
	status, show := a.do("POST", "/shows", tok, map[string]string{"feedUrl": a.env.feed.url()})
	if status != 201 || show["title"] != "API Show" || show["subscribed"] != true || show["episodeCount"].(float64) != 3 {
		t.Fatalf("add = %d %v", status, show)
	}
	showID := show["id"].(string)
	t.Cleanup(func() { a.env.pool.Exec(context.Background(), `DELETE FROM shows WHERE id=$1`, showID) }) //nolint:errcheck
	if status, _ = a.do("POST", "/shows", tok, map[string]string{"feedUrl": a.env.feed.url()}); status != 200 {
		t.Fatalf("re-add = %d, want 200", status)
	}

	// Invalid input.
	if status, body := a.do("POST", "/shows", tok, map[string]string{"feedUrl": "not a url"}); status != 422 || code(body) != "invalid_feed" {
		t.Fatalf("bad url = %d %v", status, body)
	}
	if status, _ = a.do("POST", "/shows", tok, map[string]any{"feedUrl": "x", "extra": 1}); status != 400 {
		t.Fatalf("unknown field = %d, want 400", status)
	}

	// Show, search, episodes.
	if status, body := a.do("GET", "/shows/"+showID, tok, nil); status != 200 || body["author"] != "Host" {
		t.Fatalf("get show = %d %v", status, body)
	}
	if status, _ = a.do("GET", "/shows/shw_nope", tok, nil); status != 404 {
		t.Fatalf("unknown show = %d", status)
	}
	if status, body := a.do("GET", "/shows/search?q=api+show", tok, nil); status != 200 || len(body["shows"].([]any)) < 1 {
		t.Fatalf("search = %d %v", status, body)
	}
	if status, _ = a.do("GET", "/shows/search", tok, nil); status != 400 {
		t.Fatalf("search without q = %d, want 400", status)
	}
	status, body := a.do("GET", "/shows/"+showID+"/episodes?limit=2", tok, nil)
	eps := body["episodes"].([]any)
	if status != 200 || len(eps) != 2 || eps[0].(map[string]any)["title"] != "Episode 3" {
		t.Fatalf("episodes = %d %v", status, body)
	}
	if status, _ = a.do("GET", "/shows/"+showID+"/episodes?order=sideways", tok, nil); status != 400 {
		t.Fatalf("bad order = %d, want 400", status)
	}
	epID := eps[0].(map[string]any)["id"].(string)
	if eps[0].(map[string]any)["source"] != "podcast" {
		t.Fatalf("episode source: %v", eps[0])
	}

	// Playback: the enclosure URL, no signing involved, and an unknown episode is 404.
	status, pb := a.do("GET", "/playback/"+epID, tok, nil)
	play := pb["playback"].(map[string]any)
	if status != 200 || pb["episodeId"] != epID || pb["source"] != "podcast" || pb["trackId"] != nil ||
		play["mode"] != "url" || play["url"] != "https://cdn.example.com/3.mp3" ||
		play["format"].(map[string]any)["codec"] != "mp3" {
		t.Fatalf("playback = %d %v", status, pb)
	}
	if status, body = a.do("GET", "/playback/epi_nope", tok, nil); status != 404 || code(body) != "not_found" {
		t.Fatalf("playback unknown episode = %d %v", status, body)
	}
	if status, _ = a.do("GET", "/playback/"+epID+"?quality=ultra", tok, nil); status != 400 {
		t.Fatalf("bad quality = %d, want 400", status)
	}

	// Progress.
	if status, _ = a.do("PUT", "/episodes/"+epID+"/progress", tok, map[string]int{"positionMs": 120000}); status != 204 {
		t.Fatalf("progress = %d", status)
	}
	status, ep := a.do("GET", "/episodes/"+epID, tok, nil)
	prog, _ := ep["progress"].(map[string]any)
	if status != 200 || prog["positionMs"].(float64) != 120000 || prog["completed"] != false || ep["showTitle"] != "API Show" {
		t.Fatalf("episode = %d %v", status, ep)
	}
	for name, b := range map[string]any{
		"missing position": map[string]any{},
		"negative":         map[string]int{"positionMs": -1},
		"absurd":           map[string]int{"positionMs": 999_999_999},
	} {
		if status, _ = a.do("PUT", "/episodes/"+epID+"/progress", tok, b); status != 400 && status != 422 {
			t.Errorf("%s = %d, want 400/422", name, status)
		}
	}
	if status, _ = a.do("PUT", "/episodes/epi_nope/progress", tok, map[string]int{"positionMs": 1}); status != 404 {
		t.Fatalf("progress on unknown = %d, want 404", status)
	}
	if status, body = a.do("GET", "/me/episodes/in-progress", tok, nil); status != 200 || len(body["episodes"].([]any)) != 1 {
		t.Fatalf("in-progress = %d %v", status, body)
	}

	// Subscriptions are per user.
	tok2 := a.token()
	if status, body = a.do("GET", "/shows/"+showID, tok2, nil); status != 200 || body["subscribed"] != false {
		t.Fatalf("other user sees subscribed=%v", body["subscribed"])
	}
	if status, _ = a.do("PUT", "/me/shows/"+showID, tok2, nil); status != 204 {
		t.Fatalf("subscribe = %d", status)
	}
	if status, _ = a.do("PUT", "/me/shows/shw_nope", tok2, nil); status != 404 {
		t.Fatalf("subscribe unknown = %d, want 404", status)
	}
	status, body = a.do("GET", "/me/shows", tok2, nil)
	if status != 200 || len(body["shows"].([]any)) != 1 || body["shows"].([]any)[0].(map[string]any)["unplayedCount"].(float64) != 3 {
		t.Fatalf("my shows = %d %v", status, body)
	}
	if status, body = a.do("GET", "/me/episodes/in-progress", tok2, nil); len(body["episodes"].([]any)) != 0 {
		t.Fatalf("progress leaked between users: %v", body)
	}
	if status, _ = a.do("DELETE", "/me/shows/"+showID, tok2, nil); status != 204 {
		t.Fatalf("unsubscribe = %d", status)
	}
	if status, _ = a.do("GET", "/me/shows", "garbage", nil); status != 401 {
		t.Fatalf("bad token = %d, want 401", status)
	}
}

func TestPlaybackStillServesTracks(t *testing.T) {
	// Episodes got a branch in the playback handler; an unknown track must still 404 as before.
	a := setupAPI(t)
	tok := a.token()
	if status, body := a.do("GET", "/playback/trk_nope", tok, nil); status != 404 {
		t.Fatalf("unknown track = %d %v", status, body)
	}
}
