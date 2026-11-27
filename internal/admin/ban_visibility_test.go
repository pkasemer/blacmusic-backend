package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/blacmusic/backend/internal/artist"
	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/catalog"
	"github.com/blacmusic/backend/internal/credits"
	"github.com/blacmusic/backend/internal/library"
	"github.com/blacmusic/backend/internal/lyrics"
	"github.com/blacmusic/backend/internal/playback"
	"github.com/blacmusic/backend/internal/playlist"
	"github.com/blacmusic/backend/internal/release"
	"github.com/blacmusic/backend/internal/source"
	"github.com/blacmusic/backend/internal/source/native"
	"github.com/blacmusic/backend/internal/testutil"
)

type fakeSigner struct{}

func (fakeSigner) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	return "http://signed.invalid/" + key, nil
}

// listenerRoutes mounts what a listener reads, the way the server does.
func listenerRoutes(f *full) http.Handler {
	pool := f.h.pool
	reg := source.NewRegistry()
	reg.Register(native.New(pool, fakeSigner{}, time.Hour, "http://api.test"))
	cat := catalog.NewHandler(pool, reg, "http://api.test")
	playlists := playlist.NewStore(pool, "http://api.test")
	pl := playlist.NewHandler(playlists)
	lib := library.NewHandler(pool, playlists)
	r := chi.NewRouter()
	r.Group(func(g chi.Router) {
		g.Use(auth.Require(auth.NewTokenService(testutil.Secret, time.Minute)))
		g.Get("/search", cat.Search)
		g.Get("/browse/tracks", cat.Browse)
		g.Get("/browse/artists", cat.BrowseArtists)
		g.Get("/search/artists", cat.SearchArtists)
		g.Get("/artists/{id}/tracks", cat.ArtistTracks)
		g.Get("/artists/{id}/related", cat.RelatedArtists)
		g.Get("/tracks/{id}", cat.GetTrack)
		g.Get("/playback/{id}", playback.NewHandler(pool, reg).Resolve)
		g.Get("/browse/playlists", pl.Browse)
		g.Get("/search/playlists", pl.Search)
		g.Get("/me/playlists", pl.ListMine)
		g.Get("/playlists/{id}", pl.Get)
		g.Route("/me/library", lib.Routes)
		lyrics.NewHandler(lyrics.NewStore(pool)).Routes(g)
		credits.NewHandler(credits.NewStore(pool)).Routes(g)
		release.NewHandler(release.NewStore(pool, f.blobs, "http://api.test")).Routes(g)
		artist.NewHandler(artist.NewStore(pool, testutil.GiB, "http://api.test")).Routes(g)
	})
	return r
}

func TestNothingOfABannedArtistReachesListeners(t *testing.T) {
	f := setupFull(t)
	pool, ctx := f.h.pool, context.Background()
	routes := listenerRoutes(f)
	get := func(token, path string) (int, string) {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		routes.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	send := func(token, method, path string) int {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		routes.ServeHTTP(w, req)
		return w.Code
	}
	listener := testutil.User(t, pool, "user", true)
	listenerT := testutil.Token(t, listener)
	playlists := playlist.NewStore(pool, "http://api.test")

	for _, kind := range []string{"catalog", "owned"} {
		t.Run(kind, func(t *testing.T) {
			tag := strings.ReplaceAll(testutil.Unique("Banme"+kind), "-", "")
			var art string
			if kind == "owned" {
				art = testutil.Artist(t, pool, testutil.User(t, pool, "user", true), tag+" Artist", testutil.GiB)
			} else if err := pool.QueryRow(ctx, `INSERT INTO artists (name) VALUES ($1) RETURNING id`, tag+" Artist").Scan(&art); err != nil {
				t.Fatal(err)
			}
			rel := testutil.Release(t, pool, art, tag+" Release", "single", "public")
			pool.Exec(ctx, `UPDATE albums SET published_at = now() WHERE id = $1`, rel) //nolint:errcheck
			track, _ := testutil.ReadyTrack(t, pool, f.admin, art, rel, tag+" Song", 10, "public")
			t.Cleanup(func() {
				c := context.Background()
				for _, q := range []string{`DELETE FROM uploads WHERE artist_id = $1`, `DELETE FROM tracks WHERE artist_id = $1`, `DELETE FROM albums WHERE artist_id = $1`, `DELETE FROM artists WHERE id = $1`} {
					pool.Exec(c, q, art) //nolint:errcheck
				}
			})

			// a listener has saved everything; the listener and the editorial account both have it in a public playlist
			lists := map[string]string{} // playlist -> its owner's token
			for _, owner := range []string{listener, f.admin} {
				p, err := playlists.Create(ctx, owner, "Mix "+testutil.Unique("m"), "", true)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := playlists.AddItem(ctx, p.ID, owner, track, nil); err != nil {
					t.Fatal(err)
				}
				lists[p.ID] = testutil.Token(t, owner)
			}
			for _, path := range []string{"/me/library/tracks/" + track, "/me/library/albums/" + rel, "/me/library/artists/" + art} {
				if code := send(listenerT, "PUT", path); code != 204 {
					t.Fatalf("save %s = %d", path, code)
				}
			}

			paths := []string{
				"/search?q=" + tag, "/search/artists?q=" + tag, "/search/albums?q=" + tag, "/search/playlists?q=Mix",
				"/browse/tracks?limit=50", "/browse/artists?limit=50", "/browse/artists?sort=new&limit=50", "/browse/albums?type=all&limit=50", "/browse/playlists?sort=new&limit=50",
				"/artists/" + art, "/artists/" + art + "/tracks", "/artists/" + art + "/releases", "/artists/" + art + "/related",
				"/releases/" + rel, "/albums/" + rel, "/releases/" + rel + "/related", "/releases/" + rel + "/credits", "/albums/" + rel + "/credits",
				"/tracks/" + track, "/tracks/" + track + "/lyrics", "/tracks/" + track + "/credits", "/playback/" + track,
				"/me/library/tracks", "/me/library/albums", "/me/library/artists", "/me/library/playlists",
			}
			shows := func(token, path string) (int, bool) {
				code, body := get(token, path)
				var parsed any
				_ = json.Unmarshal([]byte(body), &parsed)
				found := false
				for _, s := range []string{tag, art, rel, track} {
					found = found || strings.Contains(body, s)
				}
				return code, found
			}
			visibleBefore := 0
			for _, p := range paths {
				if _, found := shows(listenerT, p); found {
					visibleBefore++
				}
			}
			if visibleBefore < 15 {
				t.Fatalf("only %d routes showed the artist before the ban: the sweep proves too little", visibleBefore)
			}

			if code, b := call(f.r, "POST", "/admin/artists/"+art+"/ban", f.adminT, map[string]any{"reason": "testing the ban sweep"}); code != 204 {
				t.Fatalf("ban = %d %v", code, b)
			}
			for _, p := range paths {
				code, found := shows(listenerT, p)
				if found {
					t.Errorf("after the ban %s still shows the artist (%d)", p, code)
				}
				if code >= 500 {
					t.Errorf("after the ban %s = %d", p, code)
				}
			}
			for id, ownerT := range lists {
				for _, p := range []string{"/playlists/" + id, "/me/playlists"} {
					if code, found := shows(ownerT, p); found || code != 200 {
						t.Errorf("for the playlist's owner %s = %d, shows the artist: %v", p, code, found)
					}
				}
				_, body := get(ownerT, "/playlists/"+id)
				if !strings.Contains(body, `"trackCount":0`) || !strings.Contains(body, `"totalDurationSec":0`) {
					t.Errorf("the owner's playlist still counts the hidden song: %s", body)
				}
			}
			for _, w := range [][2]string{{"PUT", "/me/library/artists/" + art}, {"POST", "/artists/" + art + "/follow"}, {"PUT", "/me/library/tracks/" + track}, {"PUT", "/me/library/albums/" + rel}} {
				if code := send(testutil.Token(t, testutil.User(t, pool, "user", true)), w[0], w[1]); code != 404 {
					t.Errorf("%s %s on a banned artist = %d, want 404", w[0], w[1], code)
				}
			}

			// lifting the ban brings the song back where it was
			if code, b := call(f.r, "POST", "/admin/artists/"+art+"/unban", f.adminT, map[string]any{}); code != 204 {
				t.Fatalf("unban = %d %v", code, b)
			}
			for id, ownerT := range lists {
				if _, found := shows(ownerT, "/playlists/"+id); !found {
					t.Error("the song did not come back to the playlist after the unban")
				}
			}

			// a release an administrator takes down is hidden from playlist owners the same way
			if code, b := call(f.r, "POST", "/admin/releases/"+rel+"/takedown", f.adminT, map[string]any{"reason": "testing the takedown"}); code != 204 {
				t.Fatalf("takedown = %d %v", code, b)
			}
			for id, ownerT := range lists {
				if _, found := shows(ownerT, "/playlists/"+id); found {
					t.Error("a taken-down song still shows to the playlist's owner")
				}
			}
		})
	}
}
