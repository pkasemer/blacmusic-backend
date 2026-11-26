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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/source"
	"github.com/blacmusic/backend/internal/testutil"
)

type home struct {
	t      *testing.T
	pool   *pgxpool.Pool
	router http.Handler
	token  string
}

func setupHome(t *testing.T) *home {
	t.Helper()
	pool := testutil.Pool(t)
	h := NewHandler(pool, source.NewRegistry(), "http://api.test")
	r := chi.NewRouter()
	r.Use(auth.Require(auth.NewTokenService(testutil.Secret, time.Minute)))
	r.Get("/browse/artists", h.BrowseArtists)
	r.Get("/artists/{id}/tracks", h.ArtistTracks)
	r.Get("/tracks/{id}", h.GetTrack)
	return &home{t: t, pool: pool, router: r, token: testutil.Token(t, testutil.User(t, pool, "user", true))}
}

func (h *home) get(path string) (int, map[string]any) {
	h.t.Helper()
	req := httptest.NewRequest("GET", path, bytes.NewReader(nil))
	req.Header.Set("Authorization", "Bearer "+h.token)
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// publicArtist creates an artist with one public release holding one track.
func (h *home) publicArtist(name string, verified bool, followers int, publishedAgo string) (artistID, releaseID, trackID string) {
	h.t.Helper()
	ctx := context.Background()
	uid := testutil.User(h.t, h.pool, "user", true)
	artistID = testutil.Artist(h.t, h.pool, uid, name, testutil.GiB)
	if verified {
		h.exec(`UPDATE artists SET verification = 'verified' WHERE id = $1`, artistID)
	}
	releaseID = testutil.Release(h.t, h.pool, artistID, name+" Album", "album", "public")
	h.exec(`UPDATE albums SET published_at = now() - $2::interval WHERE id = $1`, releaseID, publishedAgo)
	trackID, _ = testutil.ReadyTrack(h.t, h.pool, uid, artistID, releaseID, name+" Song", 10, "public")
	for i := 0; i < followers; i++ {
		fan := testutil.User(h.t, h.pool, "user", true)
		if _, err := h.pool.Exec(ctx, `INSERT INTO library_artists (user_id, artist_id) VALUES ($1, $2)`, fan, artistID); err != nil {
			h.t.Fatal(err)
		}
	}
	return
}

func (h *home) exec(sql string, args ...any) {
	h.t.Helper()
	if _, err := h.pool.Exec(context.Background(), sql, args...); err != nil {
		h.t.Fatal(err)
	}
}

func idsOf(list any, key string, only map[string]bool) []string {
	out := []string{}
	for _, it := range list.([]any) {
		id := it.(map[string]any)["id"].(string)
		if only[id] {
			out = append(out, id)
		}
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestBrowseArtists(t *testing.T) {
	h := setupHome(t)
	ctx := context.Background()
	u := testutil.Unique("ba")

	// verified first, then by followers, then newest
	verifiedQuiet, _, _ := h.publicArtist(u+" verified quiet", true, 0, "5 days")
	popular, _, _ := h.publicArtist(u+" popular", false, 3, "4 days")
	smaller, _, _ := h.publicArtist(u+" smaller", false, 1, "1 day")
	// excluded: only a draft release; no releases at all; a public release with no tracks
	draftOnly := func() string {
		uid := testutil.User(t, h.pool, "user", true)
		id := testutil.Artist(t, h.pool, uid, u+" draft only", testutil.GiB)
		rel := testutil.Release(t, h.pool, id, "Hidden", "album", "draft")
		testutil.ReadyTrack(t, h.pool, uid, id, rel, "x", 1, "draft")
		return id
	}()
	nothing := testutil.Artist(t, h.pool, testutil.User(t, h.pool, "user", true), u+" nothing", testutil.GiB)
	emptyRelease := func() string {
		uid := testutil.User(t, h.pool, "user", true)
		id := testutil.Artist(t, h.pool, uid, u+" empty", testutil.GiB)
		testutil.Release(t, h.pool, id, "Empty", "album", "public")
		return id
	}()
	mine := map[string]bool{verifiedQuiet: true, popular: true, smaller: true, draftOnly: true, nothing: true, emptyRelease: true}

	code, body := h.get("/browse/artists?sort=featured&limit=50")
	if code != 200 {
		t.Fatalf("featured = %d %v", code, body)
	}
	if got, want := idsOf(body["artists"], "id", mine), []string{verifiedQuiet, popular, smaller}; !equal(got, want) {
		t.Fatalf("featured order = %v, want verified first then most followed: %v", got, want)
	}
	// the shape the home screen needs
	for _, a := range body["artists"].([]any) {
		m := a.(map[string]any)
		if m["id"] == popular {
			if m["name"] != u+" popular" || m["verified"] != false || m["followerCount"].(float64) != 3 || m["releaseCount"].(float64) != 1 {
				t.Fatalf("artist shape: %v", m)
			}
		}
		if m["id"] == verifiedQuiet && m["verified"] != true {
			t.Fatalf("verified flag missing: %v", m)
		}
	}

	// new: the artist who released most recently first
	_, body = h.get("/browse/artists?sort=new&limit=50")
	if got, want := idsOf(body["artists"], "id", mine), []string{smaller, popular, verifiedQuiet}; !equal(got, want) {
		t.Fatalf("new order = %v, want %v", got, want)
	}

	// a picture comes from the owner's avatar
	var avatar string
	owner := ""
	if err := h.pool.QueryRow(ctx, `SELECT owner_user_id::text FROM artists WHERE id = $1`, popular).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if err := h.pool.QueryRow(ctx, `INSERT INTO images (owner_id, purpose, status, original_key, size_bytes)
		VALUES ($1,'avatar','ready','k',1) RETURNING id`, owner).Scan(&avatar); err != nil {
		t.Fatal(err)
	}
	h.exec(`UPDATE users SET avatar_image_id = $2 WHERE id = $1`, owner, avatar)
	_, body = h.get("/browse/artists?limit=50")
	found := false
	for _, a := range body["artists"].([]any) {
		if m := a.(map[string]any); m["id"] == popular {
			found = true
			if m["imageUrl"] != "http://api.test/v1/media/"+avatar+"/medium.jpg" {
				t.Fatalf("imageUrl = %v", m["imageUrl"])
			}
		}
	}
	if !found {
		t.Fatal("popular artist missing")
	}

	if _, page := h.get("/browse/artists?sort=featured&limit=1&offset=0"); len(page["artists"].([]any)) != 1 {
		t.Fatalf("limit=1 returned %d", len(page["artists"].([]any)))
	}
	if code, _ := h.get("/browse/artists?sort=best"); code != 400 {
		t.Fatalf("bad sort = %d, want 400", code)
	}
}

func TestArtistTracks(t *testing.T) {
	h := setupHome(t)
	ctx := context.Background()
	uid := testutil.User(t, h.pool, "user", true)
	artistID := testutil.Artist(t, h.pool, uid, testutil.Unique("Many Songs"), testutil.GiB)
	rel := testutil.Release(t, h.pool, artistID, "Hits", "album", "public")
	h.exec(`UPDATE albums SET published_at = now() - interval '2 days', genre = 'Gospel' WHERE id = $1`, rel)
	older, _ := testutil.ReadyTrack(t, h.pool, uid, artistID, rel, "Older", 1, "public")
	newer, _ := testutil.ReadyTrack(t, h.pool, uid, artistID, rel, "Newer", 1, "public")
	h.exec(`UPDATE tracks SET created_at = now() - interval '1 hour' WHERE id = $1`, older)
	draftRel := testutil.Release(t, h.pool, artistID, "Next", "single", "draft")
	hidden, _ := testutil.ReadyTrack(t, h.pool, uid, artistID, draftRel, "Hidden", 1, "draft")
	other := testutil.Artist(t, h.pool, testutil.User(t, h.pool, "user", true), testutil.Unique("Someone Else"), testutil.GiB)
	otherRel := testutil.Release(t, h.pool, other, "Theirs", "single", "public")
	theirs, _ := testutil.ReadyTrack(t, h.pool, uid, other, otherRel, "Not mine", 1, "public")

	// two listeners saved "Older": it is the popular one
	for i := 0; i < 2; i++ {
		fan := testutil.User(t, h.pool, "user", true)
		if _, err := h.pool.Exec(ctx, `INSERT INTO library_tracks (user_id, track_id) VALUES ($1,$2)`, fan, older); err != nil {
			t.Fatal(err)
		}
	}

	code, body := h.get("/artists/" + artistID + "/tracks")
	if code != 200 {
		t.Fatalf("default sort = %d %v", code, body)
	}
	tracks := body["tracks"].([]any)
	if len(tracks) != 2 || tracks[0].(map[string]any)["id"] != older || tracks[1].(map[string]any)["id"] != newer {
		t.Fatalf("popular order = %v, want [%s %s] (no drafts, no other artists)", idsOf(body["tracks"], "id", map[string]bool{older: true, newer: true, hidden: true, theirs: true}), older, newer)
	}
	first := tracks[0].(map[string]any)
	if first["artistId"] != artistID || first["albumTitle"] != "Hits" || first["genre"] != "Gospel" || first["source"] != "native" {
		t.Fatalf("track shape: %v", first)
	}

	// new: by when the release went live, then id; both share a release, so the order is stable and complete
	code, body = h.get("/artists/" + artistID + "/tracks?sort=new&limit=1")
	if code != 200 || len(body["tracks"].([]any)) != 1 {
		t.Fatalf("sort=new&limit=1 = %d %v", code, body)
	}
	if code, _ = h.get("/artists/" + artistID + "/tracks?sort=loudest"); code != 400 {
		t.Fatalf("bad sort = %d", code)
	}
	if code, _ = h.get("/artists/art_nope/tracks"); code != 404 {
		t.Fatalf("unknown artist = %d, want 404", code)
	}
}

func TestTrackDetailHasEverythingTheTrackPageShows(t *testing.T) {
	h := setupHome(t)
	uid := testutil.User(t, h.pool, "user", true)
	artistID := testutil.Artist(t, h.pool, uid, testutil.Unique("Detail Artist"), testutil.GiB)
	rel := testutil.Release(t, h.pool, artistID, "Detail Album", "album", "public")
	h.exec(`UPDATE albums SET release_date = '2024-05-01', published_at = '2024-05-03 10:00:00+00', genre = 'Afrobeats' WHERE id = $1`, rel)
	track, _ := testutil.ReadyTrack(t, h.pool, uid, artistID, rel, "Detail Song", 1, "public")
	h.exec(`UPDATE tracks SET explicit = true, genre = 'Amapiano' WHERE id = $1`, track)

	code, tr := h.get("/tracks/" + track)
	if code != 200 {
		t.Fatalf("GET track = %d %v", code, tr)
	}
	want := map[string]any{
		"id": track, "title": "Detail Song", "artistId": artistID, "albumId": rel, "albumTitle": "Detail Album",
		"durationMs": float64(180000), "genre": "Amapiano", "explicit": true, "releaseDate": "2024-05-01",
	}
	for k, v := range want {
		if tr[k] != v {
			t.Errorf("%s = %v, want %v", k, tr[k], v)
		}
	}
	if tr["artistName"] == "" {
		t.Error("artistName missing")
	}

	// A track without its own genre takes its release's; without a release date, the day it went live.
	h.exec(`UPDATE tracks SET genre = '' WHERE id = $1`, track)
	h.exec(`UPDATE albums SET release_date = NULL WHERE id = $1`, rel)
	_, tr = h.get("/tracks/" + track)
	if tr["genre"] != "Afrobeats" || tr["releaseDate"] != "2024-05-03" {
		t.Fatalf("fallbacks: genre=%v releaseDate=%v", tr["genre"], tr["releaseDate"])
	}
}
