package release

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/catalog"
	"github.com/blacmusic/backend/internal/library"
	"github.com/blacmusic/backend/internal/playback"
	"github.com/blacmusic/backend/internal/playlist"
	"github.com/blacmusic/backend/internal/source"
	"github.com/blacmusic/backend/internal/source/native"
	"github.com/blacmusic/backend/internal/testutil"
)

type fakeSigner struct{}

func (fakeSigner) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	return "http://storage.test/" + key + "?sig=1", nil
}

// TestVisibilityIsEnforcedEverywhere checks that a draft track is invisible to
// other listeners in search, track lookup, playback, library and playlists, and
// becomes visible exactly when its release goes public.
func TestVisibilityIsEnforcedEverywhere(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	reg := source.NewRegistry()
	reg.Register(native.New(e.pool, fakeSigner{}, time.Hour))
	catalogH := catalog.NewHandler(e.pool, reg)
	playbackH := playback.NewHandler(e.pool, reg)
	libraryH := library.NewHandler(e.pool)
	r := chi.NewRouter()
	r.Use(auth.Require(auth.NewTokenService(testutil.Secret, time.Minute)))
	r.Get("/search", catalogH.Search)
	r.Get("/tracks/{id}", catalogH.GetTrack)
	r.Get("/playback/{id}", playbackH.Resolve)
	r.Route("/me/library", libraryH.Routes)

	do := func(method, path, token string) (int, map[string]any) {
		req := httptest.NewRequest(method, path, bytes.NewReader(nil))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	ownerID, artistID := e.artistUser()
	owner := testutil.Token(t, ownerID)
	strangerID := testutil.User(t, e.pool, "user", true)
	stranger := testutil.Token(t, strangerID)

	rel := testutil.Release(t, e.pool, artistID, "Hidden Album", "album", "draft")
	title := testutil.Unique("secretsong")
	track, _ := testutil.ReadyTrack(t, e.pool, ownerID, artistID, rel, title, 1000, "draft")

	search := func(token string) int {
		_, b := do("GET", "/search?q="+url.QueryEscape(title), token)
		tr, _ := b["tracks"].([]any)
		return len(tr)
	}
	ps := playlist.NewStore(e.pool)
	strangerList, _ := ps.Create(ctx, strangerID, "Mine", "", false)

	// ---- draft: only the owning artist can reach it.
	if n := search(stranger); n != 0 {
		t.Fatalf("a draft appears in a stranger's search (%d results)", n)
	}
	if code, _ := do("GET", "/tracks/"+track, stranger); code != 404 {
		t.Fatalf("track lookup of a draft = %d, want 404", code)
	}
	if code, _ := do("GET", "/playback/"+track, stranger); code != 404 {
		t.Fatalf("playback of a draft = %d, want 404", code)
	}
	if code, _ := do("PUT", "/me/library/tracks/"+track, stranger); code != 404 {
		t.Fatalf("liking a draft = %d, want 404", code)
	}
	if code, _ := do("PUT", "/me/library/albums/"+rel, stranger); code != 404 {
		t.Fatalf("saving a draft album = %d, want 404", code)
	}
	if _, err := ps.AddItem(ctx, strangerList.ID, strangerID, track, nil); !errors.Is(err, playlist.ErrTrackNotFound) {
		t.Fatalf("adding a draft to a playlist = %v, want ErrTrackNotFound", err)
	}
	if n := search(owner); n != 0 {
		t.Fatalf("drafts do not show up in search even for the owner (%d results)", n)
	}
	if code, b := do("GET", "/tracks/"+track, owner); code != 200 || b["artistId"] != artistID || b["albumId"] != rel {
		t.Fatalf("the owner can open their draft: %d %v", code, b)
	}
	code, pb := do("GET", "/playback/"+track+"?quality=low", owner)
	if code != 200 || pb["playback"].(map[string]any)["quality"] != "low" {
		t.Fatalf("the owner can preview their draft: %d %v", code, pb)
	}

	// ---- published: everyone.
	if _, err := e.store.Publish(ctx, rel, ownerID, nil); err != nil {
		t.Fatal(err)
	}
	if n := search(stranger); n != 1 {
		t.Fatalf("published track not found by search (%d results)", n)
	}
	if code, _ := do("GET", "/tracks/"+track, stranger); code != 200 {
		t.Fatalf("track lookup after publish = %d", code)
	}
	if code, _ := do("GET", "/playback/"+track, stranger); code != 200 {
		t.Fatalf("playback after publish = %d", code)
	}
	if code, _ := do("PUT", "/me/library/tracks/"+track, stranger); code != 204 {
		t.Fatalf("liking after publish = %d", code)
	}
	if code, _ := do("PUT", "/me/library/albums/"+rel, stranger); code != 204 {
		t.Fatalf("saving the album after publish = %d", code)
	}
	if _, err := ps.AddItem(ctx, strangerList.ID, strangerID, track, nil); err != nil {
		t.Fatalf("adding after publish: %v", err)
	}
	if code, b := do("GET", "/me/library/tracks", stranger); code != 200 || len(b["tracks"].([]any)) != 1 {
		t.Fatalf("library list: %d %v", code, b)
	}

	// ---- taken offline again: gone from every surface, library keeps no ghost entry.
	if _, err := e.store.Unpublish(ctx, rel, ownerID); err != nil {
		t.Fatal(err)
	}
	if n := search(stranger); n != 0 {
		t.Fatalf("unpublished track still searchable (%d)", n)
	}
	if code, _ := do("GET", "/playback/"+track, stranger); code != 404 {
		t.Fatalf("playback after unpublish = %d, want 404", code)
	}
	if _, b := do("GET", "/me/library/tracks", stranger); len(b["tracks"].([]any)) != 0 {
		t.Fatalf("an unpublished track must not be listed in the library: %v", b)
	}
	if _, b := do("GET", "/me/library/albums", stranger); len(b["albums"].([]any)) != 0 {
		t.Fatalf("an unpublished album must not be listed in the library: %v", b)
	}
}
