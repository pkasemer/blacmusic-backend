package library

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/playlist"
	"github.com/blacmusic/backend/internal/testutil"
)

func TestSavePlaylists(t *testing.T) {
	pool := testutil.Pool(t)
	store := playlist.NewStore(pool, "http://test")
	tokens := auth.NewTokenService(testutil.Secret, time.Minute)
	r := chi.NewRouter()
	r.Use(auth.Require(tokens))
	NewHandler(pool, store).Routes(r)

	owner := testutil.User(t, pool, "user", true)
	fan := testutil.User(t, pool, "user", true)
	pub, err := store.Create(context.Background(), owner, testutil.Unique("pub"), "", true)
	if err != nil {
		t.Fatal(err)
	}
	priv, err := store.Create(context.Background(), owner, testutil.Unique("priv"), "", false)
	if err != nil {
		t.Fatal(err)
	}

	call := func(method, path string) (int, map[string]any) {
		req := httptest.NewRequest(method, path, bytes.NewReader(nil))
		req.Header.Set("Authorization", "Bearer "+testutil.Token(t, fan))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	if code, _ := call("PUT", "/playlists/"+pub.ID); code != 204 {
		t.Fatalf("save public = %d", code)
	}
	if code, _ := call("PUT", "/playlists/"+pub.ID); code != 204 {
		t.Fatalf("saving twice = %d, want 204 (idempotent)", code)
	}
	if code, _ := call("PUT", "/playlists/"+priv.ID); code != 404 {
		t.Fatalf("save private = %d, want 404", code)
	}
	if code, _ := call("PUT", "/playlists/pl_nope"); code != 404 {
		t.Fatalf("save unknown = %d, want 404", code)
	}

	_, b := call("GET", "/playlists/contains?ids="+pub.ID+","+priv.ID)
	if c, _ := b["contains"].(map[string]any); c[pub.ID] != true || c[priv.ID] != false {
		t.Fatalf("contains = %v", b)
	}
	_, b = call("GET", "/playlists")
	list, _ := b["playlists"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["id"] != pub.ID || list[0].(map[string]any)["saved"] != true || list[0].(map[string]any)["saveCount"] != float64(1) {
		t.Fatalf("saved list = %v", b)
	}

	if code, _ := call("DELETE", "/playlists/"+pub.ID); code != 204 {
		t.Fatalf("unsave = %d", code)
	}
	if _, b = call("GET", "/playlists"); len(b["playlists"].([]any)) != 0 {
		t.Fatalf("after unsave = %v", b)
	}
}
