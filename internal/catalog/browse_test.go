package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	r.Get("/browse/tracks", NewHandler(pool, source.NewRegistry(), "http://api.test").Browse)

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

	// The artist's picture (the owner's avatar) comes with every track.
	var avatar string
	if err := pool.QueryRow(ctx, `INSERT INTO images (owner_id, purpose, status, original_key, size_bytes)
		VALUES ($1,'avatar','ready','k',1) RETURNING id`, uid).Scan(&avatar); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET avatar_image_id = $2 WHERE id = $1`, uid, avatar); err != nil {
		t.Fatal(err)
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
		if tr["artistImageUrl"] != "http://api.test/v1/media/"+avatar+"/medium.jpg" {
			t.Fatalf("artistImageUrl = %v, want the owner's avatar", tr["artistImageUrl"])
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
	// A genre from the list can be asked for by id or in any capitalisation.
	if _, err := pool.Exec(ctx, `UPDATE tracks SET genre = 'Hip-Hop/Rap' WHERE id = $1`, a1); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"hip-hop-rap", "HIP-HOP/RAP", "Hip-Hop/Rap"} {
		_, rap := browse(t, r, listener, "?genre="+url.QueryEscape(q)+"&limit=50")
		found := false
		for _, tr := range rap {
			found = found || tr["id"] == a1
		}
		if !found {
			t.Fatalf("?genre=%s did not find the Hip-Hop/Rap track", q)
		}
	}
	if _, none := browse(t, r, listener, "?genre=no-such-genre-"+genre); len(none) != 0 {
		t.Fatalf("unknown genre returned %v", ids(none))
	}

	if code, _ := browse(t, r, listener, "?sort=loudest"); code != 400 {
		t.Fatalf("bad sort = %d, want 400", code)
	}
	if code, _ := browse(t, r, "garbage", ""); code != 401 {
		t.Fatalf("no token = %d, want 401", code)
	}
}

// Every track says whether its artist is verified: always present, true or false.
func TestTracksCarryTheVerifiedFlag(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()
	r := chi.NewRouter()
	r.Use(auth.Require(auth.NewTokenService(testutil.Secret, time.Minute)))
	h := NewHandler(pool, source.NewRegistry(), "http://api.test")
	r.Get("/browse/tracks", h.Browse)
	r.Get("/artists/{id}/tracks", h.ArtistTracks)

	genre := testutil.Unique("verified")
	uid := testutil.User(t, pool, "user", true)
	artistID := testutil.Artist(t, pool, uid, testutil.Unique("Flag Band"), testutil.GiB)
	rel := testutil.Release(t, pool, artistID, "Flag Album", "single", "public")
	track, _ := testutil.ReadyTrack(t, pool, uid, artistID, rel, "Flag Song", 10, "public")
	pool.Exec(ctx, `UPDATE tracks SET genre = $2 WHERE id = $1`, track, genre) //nolint:errcheck
	listener := testutil.Token(t, testutil.User(t, pool, "user", true))

	flag := func() any {
		t.Helper()
		code, tracks := browse(t, r, listener, "?genre="+genre)
		if code != 200 || len(tracks) != 1 {
			t.Fatalf("browse = %d %v", code, tracks)
		}
		v, present := tracks[0]["artistVerified"]
		if !present {
			t.Fatalf("artistVerified missing: %v", tracks[0])
		}
		return v
	}
	if flag() != false {
		t.Fatal("an unverified artist must give artistVerified:false")
	}
	pool.Exec(ctx, `UPDATE artists SET verification = 'verified' WHERE id = $1`, artistID) //nolint:errcheck
	if flag() != true {
		t.Fatal("a verified artist must give artistVerified:true")
	}
	req := httptest.NewRequest("GET", "/artists/"+artistID+"/tracks", bytes.NewReader(nil))
	req.Header.Set("Authorization", "Bearer "+listener)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var out struct{ Tracks []map[string]any }
	if json.Unmarshal(rec.Body.Bytes(), &out) != nil || len(out.Tracks) != 1 || out.Tracks[0]["artistVerified"] != true {
		t.Fatalf("artist tracks = %d %s", rec.Code, rec.Body.String())
	}
}

func TestSearchArtists(t *testing.T) {
	pool := testutil.Pool(t)
	r := chi.NewRouter()
	r.Use(auth.Require(auth.NewTokenService(testutil.Secret, time.Minute)))
	r.Get("/search/artists", NewHandler(pool, source.NewRegistry(), "http://api.test").SearchArtists)

	tok := "zq" + testutil.Unique("s")
	mk := func(name, visibility string) string {
		uid := testutil.User(t, pool, "user", true)
		id := testutil.Artist(t, pool, uid, name, testutil.GiB)
		rel := testutil.Release(t, pool, id, "R "+name, "single", visibility)
		testutil.ReadyTrack(t, pool, uid, id, rel, "T", 1, map[string]string{"public": "public", "draft": "draft"}[visibility])
		return id
	}
	contains := mk("x"+tok+"y", "public")
	word := mk("The "+tok+" Crew", "public")
	prefix := mk(tok+" Band", "public")
	exact := mk(tok, "public")
	mk(tok+" Hidden", "draft") // no public release: never listed
	listener := testutil.Token(t, testutil.User(t, pool, "user", true))

	get := func(path string) (int, []any) {
		req := httptest.NewRequest("GET", path, bytes.NewReader(nil))
		req.Header.Set("Authorization", "Bearer "+listener)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var out struct{ Artists []any }
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out.Artists
	}
	code, list := get("/search/artists?q=" + url.QueryEscape(tok))
	if code != 200 || len(list) != 4 {
		t.Fatalf("search = %d %v", code, list)
	}
	var got []string
	for _, a := range list {
		got = append(got, a.(map[string]any)["id"].(string))
	}
	want := []string{exact, prefix, word, contains}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want exact, prefix, word, contains = %v", got, want)
		}
	}
	first := list[0].(map[string]any)
	for _, k := range []string{"id", "name", "verified", "followerCount", "releaseCount"} {
		if _, ok := first[k]; !ok {
			t.Fatalf("artist lacks %q: %v", k, first)
		}
	}
	if code, list = get("/search/artists?q=" + url.QueryEscape(tok) + "&limit=2&offset=1"); code != 200 || len(list) != 2 || list[0].(map[string]any)["id"] != prefix {
		t.Fatalf("paging = %d %v", code, list)
	}
	// wildcards are text, not patterns
	if code, list = get("/search/artists?q=" + url.QueryEscape("%")); code != 200 || len(list) != 0 {
		t.Fatalf("percent sign = %d %v, want nothing", code, list)
	}
	if code, _ = get("/search/artists?q="); code != 400 {
		t.Fatalf("empty q = %d", code)
	}
}

func TestRelatedArtists(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()
	r := chi.NewRouter()
	r.Use(auth.Require(auth.NewTokenService(testutil.Secret, time.Minute)))
	r.Get("/artists/{id}/related", NewHandler(pool, source.NewRegistry(), "http://api.test").RelatedArtists)

	g1, g2 := testutil.Unique("gA"), testutil.Unique("gB")
	mk := func(visibility string, genres ...string) string {
		uid := testutil.User(t, pool, "user", true)
		id := testutil.Artist(t, pool, uid, testutil.Unique("Rel"), testutil.GiB)
		for i, g := range genres {
			rel := testutil.Release(t, pool, id, "R", "single", visibility)
			pool.Exec(ctx, `UPDATE albums SET genre = $2 WHERE id = $1`, rel, g) //nolint:errcheck
			testutil.ReadyTrack(t, pool, uid, id, rel, "T"+g, 1, visibility)
			_ = i
		}
		if len(genres) == 0 {
			rel := testutil.Release(t, pool, id, "R", "single", visibility)
			testutil.ReadyTrack(t, pool, uid, id, rel, "T", 1, visibility)
		}
		return id
	}
	self := mk("public", g1, g2)
	both := mk("public", g1, g2)
	one := mk("public", g1)
	none := mk("public")
	mk("draft", g1, g2) // no public release: never suggested
	listener := testutil.Token(t, testutil.User(t, pool, "user", true))

	get := func(path string) (int, []any) {
		req := httptest.NewRequest("GET", path, bytes.NewReader(nil))
		req.Header.Set("Authorization", "Bearer "+listener)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var out struct{ Artists []any }
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out.Artists
	}
	code, list := get("/artists/" + self + "/related?limit=50")
	if code != 200 {
		t.Fatalf("related = %d", code)
	}
	pos := map[string]int{}
	for i, a := range list {
		m := a.(map[string]any)
		if m["id"] == self {
			t.Fatal("the artist is related to itself")
		}
		pos[m["id"].(string)] = i
	}
	if _, ok := pos[both]; !ok || pos[both] > pos[one] || pos[one] > pos[none] {
		t.Fatalf("order: two shared genres (%d), one (%d), none (%d): want that order", pos[both], pos[one], pos[none])
	}
	first := list[0].(map[string]any)
	for _, k := range []string{"id", "name", "verified", "followerCount", "releaseCount"} {
		if _, ok := first[k]; !ok {
			t.Fatalf("artist lacks %q: %v", k, first)
		}
	}
	if code, list = get("/artists/" + self + "/related?limit=2"); code != 200 || len(list) != 2 || list[0].(map[string]any)["id"] != both {
		t.Fatalf("limit = %d %v", code, list)
	}
	if code, _ = get("/artists/art_nope/related"); code != 404 {
		t.Fatalf("unknown artist = %d, want 404", code)
	}
}
