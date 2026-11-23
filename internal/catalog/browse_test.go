package catalog

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
	"github.com/blacmusic/backend/internal/source"
	"github.com/blacmusic/backend/internal/testutil"
)

func browse(t *testing.T, h http.Handler, token, query string) (int, []map[string]any) {
	t.Helper()
	req := httptest.NewRequest("GET", "/browse/tracks"+query, bytes.NewReader(nil))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out struct {
		Tracks []map[string]any `json:"tracks"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out.Tracks
}

func TestBrowseTracks(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()
	r := chi.NewRouter()
	r.Use(auth.Require(auth.NewTokenService(testutil.Secret, time.Minute)))
	r.Get("/browse/tracks", NewHandler(pool, source.NewRegistry()).Browse)

	// One artist with a public album (2 tracks, with a cover), a newer public single, and a draft.
	genre := testutil.Unique("genre")
	uid := testutil.User(t, pool, "user", true)
	artistID := testutil.Artist(t, pool, uid, testutil.Unique("Browse Band"), testutil.GiB)
	album := testutil.Release(t, pool, artistID, "Older Album", "album", "public")
	single := testutil.Release(t, pool, artistID, "Newer Single", "single", "public")
	draft := testutil.Release(t, pool, artistID, "Hidden", "album", "draft")
	for id, ago := range map[string]string{album: "2 days", single: "1 day"} {
		if _, err := pool.Exec(ctx, `UPDATE albums SET published_at = now() - $2::interval, genre = $3,
			artwork_url = 'http://api.test/v1/media/img_x/medium.jpg' WHERE id = $1`, id, ago, genre); err != nil {
			t.Fatal(err)
		}
	}
	a1, _ := testutil.ReadyTrack(t, pool, uid, artistID, album, "Album One", 10, "public")
	a2, _ := testutil.ReadyTrack(t, pool, uid, artistID, album, "Album Two", 10, "public")
	s1, _ := testutil.ReadyTrack(t, pool, uid, artistID, single, "Newer Single", 10, "public")
	hidden, _ := testutil.ReadyTrack(t, pool, uid, artistID, draft, "Hidden Track", 10, "draft")
	for _, id := range []string{a1, a2, s1} { // (the genre filter below scopes everything to this test's data)
		if _, err := pool.Exec(ctx, `UPDATE tracks SET genre = $2 WHERE id = $1`, id, genre); err != nil {
			t.Fatal(err)
		}
	}

	listener := testutil.Token(t, testutil.User(t, pool, "user", true))
	ids := func(tracks []map[string]any) []string {
		out := []string{}
		for _, tr := range tracks {
			out = append(out, tr["id"].(string))
		}
		return out
	}

	// Newest first; drafts never appear; covers come with the tracks.
	code, tracks := browse(t, r, listener, "?sort=new&genre="+genre)
	if code != 200 || len(tracks) != 3 || tracks[0]["id"] != s1 {
		t.Fatalf("sort=new = %d %v, want the newer single first", code, ids(tracks))
	}
	for _, tr := range tracks {
		if tr["id"] == hidden {
			t.Fatal("a draft track leaked into browse")
		}
		if tr["artworkUrl"] != "http://api.test/v1/media/img_x/medium.jpg" || tr["source"] != "native" || tr["artistName"] == "" || tr["albumId"] == "" {
			t.Fatalf("track shape: %v", tr)
		}
	}
	if _, def := browse(t, r, listener, "?genre="+genre); len(def) != 3 || def[0]["id"] != s1 {
		t.Fatalf("default sort must be new: %v", ids(def))
	}

	// Popular: more library saves first.
	for i := 0; i < 2; i++ {
		fan := testutil.User(t, pool, "user", true)
		if _, err := pool.Exec(ctx, `INSERT INTO library_tracks (user_id, track_id) VALUES ($1, $2)`, fan, a2); err != nil {
			t.Fatal(err)
		}
	}
	fan := testutil.User(t, pool, "user", true)
	if _, err := pool.Exec(ctx, `INSERT INTO library_tracks (user_id, track_id) VALUES ($1, $2)`, fan, a1); err != nil {
		t.Fatal(err)
	}
	_, pop := browse(t, r, listener, "?sort=popular&genre="+genre)
	if got := ids(pop); len(got) != 3 || got[0] != a2 || got[1] != a1 || got[2] != s1 {
		t.Fatalf("sort=popular = %v, want [%s %s %s]", got, a2, a1, s1)
	}

	// Paging and the genre filter.
	_, page := browse(t, r, listener, "?sort=new&genre="+genre+"&limit=1&offset=1")
	if len(page) != 1 || page[0]["id"] == s1 {
		t.Fatalf("page 2 = %v", ids(page))
	}
	if _, none := browse(t, r, listener, "?genre=no-such-genre-"+genre); len(none) != 0 {
		t.Fatalf("unknown genre returned %v", ids(none))
	}

	if code, _ := browse(t, r, listener, "?sort=trending"); code != 400 {
		t.Fatalf("bad sort = %d, want 400", code)
	}
	if code, _ := browse(t, r, "garbage", ""); code != 401 {
		t.Fatalf("no token = %d, want 401", code)
	}
}
