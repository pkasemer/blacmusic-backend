package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/blacmusic/backend/internal/artist"
	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/importopt"
	"github.com/blacmusic/backend/internal/playlist"
	"github.com/blacmusic/backend/internal/showcase"
	"github.com/blacmusic/backend/internal/testutil"
)

func setupRouter(t *testing.T) (http.Handler, *artist.Store) {
	t.Helper()
	pool := testutil.Pool(t)
	store := artist.NewStore(pool, 10*testutil.GiB, "http://api.test")
	h := NewHandler(pool, store, playlist.NewStore(pool, "http://api.test"))
	h.Showcase = showcase.NewStore(pool, "http://api.test")
	h.ImportOptions = importopt.NewHandler(importopt.NewStore(pool))
	r := chi.NewRouter()
	r.Group(func(g chi.Router) {
		g.Use(auth.Require(auth.NewTokenService(testutil.Secret, time.Minute)))
		g.Route("/admin", h.Routes)
	})
	return r, store
}

func call(r http.Handler, method, path, token string, body any) (int, map[string]any) {
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
	r.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestOnlyAdminsGetIn(t *testing.T) {
	r, _ := setupRouter(t)
	pool := testutil.Pool(t)
	for _, role := range []string{"user", "creator"} {
		tok := testutil.Token(t, testutil.User(t, pool, role, true))
		if code, _ := call(r, "GET", "/admin/artists", tok, nil); code != 403 {
			t.Errorf("%s = %d, want 403", role, code)
		}
		if code, _ := call(r, "POST", "/admin/artists/art_x/verification", tok, map[string]string{"decision": "approve"}); code != 403 {
			t.Errorf("%s decide = %d, want 403", role, code)
		}
		if code, _ := call(r, "POST", "/admin/artists/art_x/storage-grants", tok, map[string]any{"kind": "promo", "bytes": 1}); code != 403 {
			t.Errorf("%s grant = %d, want 403", role, code)
		}
	}
	admin := testutil.Token(t, testutil.User(t, pool, "admin", true))
	if code, _ := call(r, "GET", "/admin/artists", admin, nil); code != 200 {
		t.Fatalf("admin = %d, want 200", code)
	}
	if code, _ := call(r, "GET", "/admin/artists", "garbage", nil); code != 401 {
		t.Fatalf("no token = %d, want 401", code)
	}
}

func TestReviewAndDecide(t *testing.T) {
	r, store := setupRouter(t)
	pool := testutil.Pool(t)
	ctx := context.Background()
	admin := testutil.Token(t, testutil.User(t, pool, "admin", true))

	uid := testutil.User(t, pool, "user", true)
	a, err := store.Create(ctx, uid, testutil.Unique("Queue"), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RequestVerification(ctx, a.ID, uid, "Please"); err != nil {
		t.Fatal(err)
	}

	code, b := call(r, "GET", "/admin/artists?status=pending&limit=200", admin, nil)
	found := false
	for _, x := range b["artists"].([]any) {
		found = found || x.(map[string]any)["id"] == a.ID
	}
	if code != 200 || !found {
		t.Fatalf("queue = %d, artist found = %v", code, found)
	}
	if code, _ = call(r, "GET", "/admin/artists?status=weird", admin, nil); code != 400 {
		t.Fatalf("bad status = %d, want 400", code)
	}

	path := "/admin/artists/" + a.ID + "/verification"
	if code, _ = call(r, "POST", path, admin, map[string]string{"decision": "maybe"}); code != 400 {
		t.Fatalf("bad decision = %d, want 400", code)
	}
	if code, b = call(r, "POST", path, admin, map[string]string{"decision": "reject"}); code != 422 {
		t.Fatalf("reject without a reason = %d %v, want 422", code, b)
	}
	if code, _ = call(r, "POST", path, admin, map[string]string{"decision": "approve"}); code != 204 {
		t.Fatalf("approve = %d, want 204", code)
	}
	if code, b = call(r, "POST", path, admin, map[string]string{"decision": "approve"}); code != 409 {
		t.Fatalf("approve twice = %d %v, want 409", code, b)
	}
	if mine, _ := store.GetByOwner(ctx, uid); !mine.Verified {
		t.Fatal("the artist should be verified now")
	}
	if code, _ = call(r, "POST", "/admin/artists/art_nope/verification", admin, map[string]string{"decision": "approve"}); code != 404 {
		t.Fatalf("unknown artist = %d, want 404", code)
	}
}

func TestStorageGrantsAreIdempotent(t *testing.T) {
	r, store := setupRouter(t)
	pool := testutil.Pool(t)
	ctx := context.Background()
	admin := testutil.Token(t, testutil.User(t, pool, "admin", true))
	uid := testutil.User(t, pool, "user", true)
	a, _ := store.Create(ctx, uid, testutil.Unique("Grant"), "")

	path := "/admin/artists/" + a.ID + "/storage-grants"
	body := map[string]any{"kind": "purchase", "bytes": 5 * testutil.GiB, "note": "5 GB pack", "reference": testutil.Unique("pay")}
	if code, b := call(r, "POST", path, admin, body); code != 201 || b["created"] != true {
		t.Fatalf("first grant = %d %v, want 201", code, b)
	}
	if code, b := call(r, "POST", path, admin, body); code != 200 || b["created"] != false {
		t.Fatalf("repeat = %d %v, want 200 created=false", code, b)
	}
	q, _ := store.Quota(ctx, a.ID, uid)
	if q.TotalStorageBytes != 15*testutil.GiB {
		t.Fatalf("total = %d GiB, want 15 (granted once)", q.TotalStorageBytes/testutil.GiB)
	}
	if code, _ := call(r, "POST", path, admin, map[string]any{"kind": "free", "bytes": 1}); code != 422 {
		t.Fatalf("kind free = %d, want 422", code)
	}
	if code, _ := call(r, "POST", "/admin/artists/art_nope/storage-grants", admin, map[string]any{"kind": "promo", "bytes": 1}); code != 404 {
		t.Fatalf("unknown artist = %d, want 404", code)
	}
}

func TestPlaylistGroupsAdminOnly(t *testing.T) {
	r, _ := setupRouter(t)
	pool := testutil.Pool(t)
	user := testutil.Token(t, testutil.User(t, pool, "user", true))
	admin := testutil.Token(t, testutil.User(t, pool, "admin", true))

	for _, c := range []struct{ method, path string }{
		{"GET", "/admin/playlist-groups"},
		{"POST", "/admin/playlist-groups"},
		{"PATCH", "/admin/playlist-groups/x"},
		{"DELETE", "/admin/playlist-groups/x"},
		{"PUT", "/admin/playlist-groups/x/playlists"},
	} {
		if code, _ := call(r, c.method, c.path, user, map[string]any{"title": "x", "playlistIds": []string{}}); code != 403 {
			t.Errorf("user %s %s = %d, want 403", c.method, c.path, code)
		}
	}

	id := testutil.Unique("grp")
	code, g := call(r, "POST", "/admin/playlist-groups", admin, map[string]any{"id": id, "title": "Workout", "description": "Push it", "position": 8_000_000})
	if code != 201 || g["id"] != id || g["isActive"] != true || g["description"] != "Push it" {
		t.Fatalf("create = %d %v", code, g)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM playlist_groups WHERE id = $1`, id) }) //nolint:errcheck
	if code, _ = call(r, "POST", "/admin/playlist-groups", admin, map[string]any{"id": id, "title": "again"}); code != 409 {
		t.Fatalf("duplicate = %d, want 409", code)
	}
	if code, _ = call(r, "POST", "/admin/playlist-groups", admin, map[string]any{"title": ""}); code != 422 {
		t.Fatalf("blank title = %d, want 422", code)
	}
	if code, g = call(r, "PATCH", "/admin/playlist-groups/"+id, admin, map[string]any{"isActive": false}); code != 200 || g["isActive"] != false {
		t.Fatalf("patch = %d %v", code, g)
	}
	if code, _ = call(r, "PUT", "/admin/playlist-groups/"+id+"/playlists", admin, map[string]any{"playlistIds": []string{"pl_nope"}}); code != 422 {
		t.Fatalf("unknown playlist = %d, want 422", code)
	}
	if code, _ = call(r, "PUT", "/admin/playlist-groups/"+id+"/playlists", admin, map[string]any{}); code != 400 {
		t.Fatalf("missing playlistIds = %d, want 400", code)
	}
	if code, g = call(r, "PUT", "/admin/playlist-groups/"+id+"/playlists", admin, map[string]any{"playlistIds": []string{}}); code != 200 {
		t.Fatalf("empty members = %d %v", code, g)
	}
	if code, _ = call(r, "PATCH", "/admin/playlist-groups/nope-"+id, admin, map[string]any{"title": "x"}); code != 404 {
		t.Fatalf("unknown group = %d, want 404", code)
	}
	if code, _ = call(r, "DELETE", "/admin/playlist-groups/"+id, admin, nil); code != 204 {
		t.Fatalf("delete = %d, want 204", code)
	}
}

func TestShowcaseAdminOnly(t *testing.T) {
	r, _ := setupRouter(t)
	pool := testutil.Pool(t)
	user := testutil.Token(t, testutil.User(t, pool, "user", true))
	admin := testutil.Token(t, testutil.User(t, pool, "admin", true))
	if code, _ := call(r, "GET", "/admin/showcase", user, nil); code != 403 {
		t.Fatalf("user GET = %d, want 403", code)
	}
	if code, _ := call(r, "PUT", "/admin/showcase", user, map[string]any{"items": []any{}}); code != 403 {
		t.Fatalf("user PUT = %d, want 403", code)
	}
	if code, b := call(r, "PUT", "/admin/showcase", admin, map[string]any{}); code != 400 {
		t.Fatalf("PUT without items = %d %v", code, b)
	}
	if code, b := call(r, "PUT", "/admin/showcase", admin, map[string]any{"items": []map[string]string{{"kind": "artist", "id": "art_nope", "consentNote": "agreed"}}}); code != 422 {
		t.Fatalf("PUT unknown artist = %d %v", code, b)
	}
	code, b := call(r, "PUT", "/admin/showcase", admin, map[string]any{"items": []any{}})
	if code != 200 || b["max"] != float64(12) {
		t.Fatalf("PUT empty = %d %v", code, b)
	}
	pool.Exec(context.Background(), `DELETE FROM showcase_items`) //nolint:errcheck
}

func TestImportOptionsAdminOnly(t *testing.T) {
	r, _ := setupRouter(t)
	pool := testutil.Pool(t)
	user := testutil.Token(t, testutil.User(t, pool, "user", true))
	admin := testutil.Token(t, testutil.User(t, pool, "admin", true))
	id := "t_" + time.Now().Format("150405") + "_adm"
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM import_options WHERE id = $1`, id) }) //nolint:errcheck

	if code, _ := call(r, "PUT", "/admin/import-options/"+id, user, map[string]any{"enabled": true}); code != 403 {
		t.Fatalf("user = %d, want 403", code)
	}
	code, b := call(r, "PUT", "/admin/import-options/"+id, admin, map[string]any{"title": "Test option", "note": "Coming soon"})
	if code != 200 || b["id"] != id || b["enabled"] != false || b["title"] != "Test option" {
		t.Fatalf("create = %d %v", code, b)
	}
	if code, b = call(r, "PUT", "/admin/import-options/"+id, admin, map[string]any{"enabled": true}); code != 200 || b["enabled"] != true || b["note"] != "Coming soon" {
		t.Fatalf("enable = %d %v", code, b)
	}
	if code, b = call(r, "PUT", "/admin/import-options/"+id, admin, map[string]any{}); code != 422 {
		t.Fatalf("empty = %d %v", code, b)
	}
	if code, b = call(r, "PUT", "/admin/import-options/Bad%20Id", admin, map[string]any{"enabled": true}); code != 422 {
		t.Fatalf("bad id = %d %v", code, b)
	}
	code, b = call(r, "GET", "/admin/import-options", admin, nil)
	if opts, _ := b["options"].([]any); code != 200 || len(opts) < 3 {
		t.Fatalf("list = %d %v", code, b)
	}
}
