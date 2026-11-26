package library

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
	"github.com/blacmusic/backend/internal/playlist"
	"github.com/blacmusic/backend/internal/testutil"
)

// Saved tracks come in the same shape as search results (with artistVerified), saved albums and followed
// artists say whether the artist is verified.
func TestSavedItemsCarryTheVerifiedFlag(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()
	h := NewHandler(pool, playlist.NewStore(pool, "http://test"))
	h.PublicURL = "http://test"
	r := chi.NewRouter()
	r.Use(auth.Require(auth.NewTokenService(testutil.Secret, time.Minute)))
	h.Routes(r)

	owner := testutil.User(t, pool, "user", true)
	artist := testutil.Artist(t, pool, owner, testutil.Unique("Checked"), testutil.GiB)
	rel := testutil.Release(t, pool, artist, "Public Album", "album", "public")
	track, _ := testutil.ReadyTrack(t, pool, owner, artist, rel, "Public Song", 10, "public")
	fan := testutil.Token(t, testutil.User(t, pool, "user", true))

	call := func(method, path string) (int, map[string]any) {
		req := httptest.NewRequest(method, path, bytes.NewReader(nil))
		req.Header.Set("Authorization", "Bearer "+fan)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	for _, p := range []string{"/tracks/" + track, "/albums/" + rel, "/artists/" + artist} {
		if code, _ := call("PUT", p); code != http.StatusNoContent {
			t.Fatalf("save %s = %d", p, code)
		}
	}
	first := func(list string) map[string]any {
		t.Helper()
		code, b := call("GET", "/"+list)
		items, _ := b[list].([]any)
		if code != 200 || len(items) != 1 {
			t.Fatalf("%s = %d %v", list, code, b)
		}
		return items[0].(map[string]any)
	}

	tr, al, ar := first("tracks"), first("albums"), first("artists")
	if tr["artistVerified"] != false || al["artistVerified"] != false || ar["verified"] != false {
		t.Fatalf("unverified artist: track %v album %v artist %v (the flag must be sent, as false)", tr["artistVerified"], al["artistVerified"], ar["verified"])
	}
	// the saved track has the full track shape now, not just a few fields
	if tr["id"] != track || tr["artistId"] != artist || tr["albumId"] != rel || tr["source"] != "native" || tr["addedAt"] == nil || tr["durationMs"] == nil {
		t.Fatalf("saved track = %v", tr)
	}
	if al["artistId"] != artist {
		t.Fatalf("saved album = %v", al)
	}

	if _, err := pool.Exec(ctx, `UPDATE artists SET verification = 'verified' WHERE id = $1`, artist); err != nil {
		t.Fatal(err)
	}
	tr, al, ar = first("tracks"), first("albums"), first("artists")
	if tr["artistVerified"] != true || al["artistVerified"] != true || ar["verified"] != true {
		t.Fatalf("verified artist: track %v album %v artist %v", tr["artistVerified"], al["artistVerified"], ar["verified"])
	}
}
