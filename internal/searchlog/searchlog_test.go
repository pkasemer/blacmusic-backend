package searchlog

import (
	"context"
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

func TestNormalize(t *testing.T) {
	long := strings.Repeat("ab ", 60)
	for in, want := range map[string]string{
		"  Hello   WORLD ":    "hello world",
		"Jose Chameleone":     "jose chameleone",
		"MUKENE":              "mukene",
		"a":                   "",
		"  ":                  "",
		"Ñandú  Müller":       "ñandú müller",
		strings.ToUpper(long): strings.TrimSpace(string([]rune(strings.Join(strings.Fields(long), " "))[:MaxQueryRunes])),
	} {
		got, ok := Normalize(in)
		if want == "" {
			if ok {
				t.Errorf("Normalize(%q) = %q, want it skipped", in, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("Normalize(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
}

func TestMiddlewareRecordsFirstPagesOnly(t *testing.T) {
	pool := testutil.Pool(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := New(ctx, pool)

	tag := strings.ToLower(testutil.Unique("sq"))
	var answers = map[string]string{
		"/v1/search":           `{"tracks":[{"id":"a"},{"id":"b"}]}`,
		"/v1/search/artists":   `{"artists":[]}`,
		"/v1/search/playlists": `{"playlists":[{"id":"p"}]}`,
		"/v1/search/albums":    `{"releases":[{"id":"r"},{"id":"s"},{"id":"t"}]}`,
	}
	r := chi.NewRouter()
	user := testutil.User(t, pool, "user", true)
	admin := testutil.User(t, pool, "admin", true)
	r.Use(auth.RequireWith(auth.NewTokenService(testutil.Secret, time.Minute), nil))
	r.Use(l.Middleware)
	for p, body := range answers {
		p, body := p, body
		r.Get(p, func(w http.ResponseWriter, req *http.Request) {
			if req.URL.Query().Get("fail") != "" {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		})
	}
	get := func(who, path, query string) {
		t.Helper()
		req := httptest.NewRequest("GET", path+"?"+query, nil)
		req.Header.Set("Authorization", "Bearer "+testutil.Token(t, who))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Body.Len() == 0 && !strings.Contains(query, "fail") {
			t.Fatalf("%s answered nothing", path)
		}
	}
	q := func(s string) string { return "q=" + url.QueryEscape(s) }

	get(user, "/v1/search", q("  "+strings.ToUpper(tag)+"   Song "))
	get(user, "/v1/search/artists", q(tag+" artist"))
	get(user, "/v1/search/playlists", q(tag+" mix")+"&offset=0")
	get(user, "/v1/search/albums", q(tag+" album"))
	get(user, "/v1/search", q(tag+" second page")+"&offset=20") // not the first page
	get(user, "/v1/search", q("x"))                             // too short
	get(user, "/v1/search", q(tag+" failing")+"&fail=1")        // a failed search
	get(admin, "/v1/search", q(tag+" by an admin"))             // admins are left out

	type row struct {
		kind    string
		results int
	}
	want := map[string]row{
		tag + " song": {"tracks", 2}, tag + " artist": {"artists", 0}, tag + " mix": {"playlists", 1}, tag + " album": {"albums", 3},
	}
	var got map[string]row
	for i := 0; i < 50; i++ { // the logger works in the background
		got = map[string]row{}
		rows, err := pool.Query(ctx, `SELECT query, kind, results FROM search_queries WHERE query LIKE $1`, tag+"%")
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var q string
			var r row
			if err := rows.Scan(&q, &r.kind, &r.results); err != nil {
				t.Fatal(err)
			}
			got[q] = r
		}
		rows.Close()
		if len(got) >= len(want) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // leave room for rows that must NOT appear
	rows, _ := pool.Query(ctx, `SELECT query, kind, results FROM search_queries WHERE query LIKE $1`, tag+"%")
	got = map[string]row{}
	for rows.Next() {
		var q string
		var r row
		rows.Scan(&q, &r.kind, &r.results) //nolint:errcheck
		got[q] = r
	}
	rows.Close()
	if len(got) != len(want) {
		t.Fatalf("recorded %v, want exactly %v", got, want)
	}
	for q, w := range want {
		if got[q] != w {
			t.Errorf("%q recorded as %+v, want %+v", q, got[q], w)
		}
	}
}
