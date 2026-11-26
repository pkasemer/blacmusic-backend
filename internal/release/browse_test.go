package release

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/testutil"
)

func TestBrowseAlbums(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid, artistID := e.artistUser()

	mk := func(title, typ, publishedAgo string, withTrack bool) string {
		rel := testutil.Release(t, e.pool, artistID, testutil.Unique(title), typ, "public")
		e.pool.Exec(ctx, `UPDATE albums SET published_at = now() - $2::interval WHERE id = $1`, rel, publishedAgo) //nolint:errcheck
		if withTrack {
			testutil.ReadyTrack(t, e.pool, uid, artistID, rel, "t", 1, "public")
		}
		return rel
	}
	oldAlbum := mk("old album", "album", "10 days", true)
	newAlbum := mk("new album", "album", "1 day", true)
	single := mk("single", "single", "2 days", true)
	empty := mk("empty album", "album", "0 days", false) // public but nothing to play
	draft := testutil.Release(t, e.pool, artistID, "draft", "album", "draft")
	testutil.ReadyTrack(t, e.pool, uid, artistID, draft, "d", 1, "draft")
	mine := map[string]bool{oldAlbum: true, newAlbum: true, single: true, empty: true, draft: true}

	ids := func(rs []*Release) []string {
		out := []string{}
		for _, r := range rs {
			if mine[r.ID] {
				out = append(out, r.ID)
			}
		}
		return out
	}
	eq := func(a, b []string) bool {
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

	got, err := e.store.Browse(ctx, "new", "album", 50, 0)
	if err != nil || !eq(ids(got), []string{newAlbum, oldAlbum}) {
		t.Fatalf("new/album = %v %v, want newest album first without singles, drafts or empty releases", ids(got), err)
	}
	if got, _ = e.store.Browse(ctx, "", "", 50, 0); !eq(ids(got), []string{newAlbum, oldAlbum}) {
		t.Fatalf("defaults must be new + album: %v", ids(got))
	}
	if got, _ = e.store.Browse(ctx, "new", "single", 50, 0); !eq(ids(got), []string{single}) {
		t.Fatalf("type=single = %v", ids(got))
	}
	if got, _ = e.store.Browse(ctx, "new", "all", 50, 0); !eq(ids(got), []string{newAlbum, single, oldAlbum}) {
		t.Fatalf("type=all = %v", ids(got))
	}

	// popular: saves of the album plus saves of its tracks
	for i := 0; i < 2; i++ {
		fan := testutil.User(t, e.pool, "user", true)
		if _, err := e.pool.Exec(ctx, `INSERT INTO library_albums (user_id, album_id) VALUES ($1,$2)`, fan, oldAlbum); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ = e.store.Browse(ctx, "popular", "album", 50, 0); !eq(ids(got), []string{oldAlbum, newAlbum}) {
		t.Fatalf("popular = %v, want the saved old album first", ids(got))
	}

	// the shape the home screen needs
	var first *Release
	for _, r := range got {
		if r.ID == oldAlbum {
			first = r
		}
	}
	if first.Artist == nil || first.Artist.ID != artistID || first.Artist.Name == "" || first.TrackCount != 1 || first.Type != "album" {
		t.Fatalf("release shape: %+v", first)
	}

	var ve *ValidationError
	if _, err := e.store.Browse(ctx, "loudest", "album", 10, 0); !errors.As(err, &ve) {
		t.Fatalf("bad sort = %v", err)
	}
	if _, err := e.store.Browse(ctx, "new", "ep", 10, 0); !errors.As(err, &ve) {
		t.Fatalf("bad type = %v", err)
	}
}

func TestBrowseAlbumsEndpoint(t *testing.T) {
	e := setup(t)
	tokens := auth.NewTokenService(testutil.Secret, time.Minute)
	r := chi.NewRouter()
	r.Use(auth.Require(tokens))
	NewHandler(e.store).Routes(r)
	tok := testutil.Token(t, testutil.User(t, e.pool, "user", true))

	get := func(q string) (int, map[string]any) {
		req := httptest.NewRequest("GET", "/browse/albums"+q, bytes.NewReader(nil))
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	if code, b := get("?sort=popular&type=all&limit=5"); code != 200 || b["releases"] == nil {
		t.Fatalf("ok request = %d %v", code, b)
	}
	if code, _ := get("?sort=trending"); code != http.StatusBadRequest {
		t.Fatalf("bad sort = %d, want 400", code)
	}
	if code, _ := get("?type=ep"); code != http.StatusBadRequest {
		t.Fatalf("bad type = %d, want 400", code)
	}
}
