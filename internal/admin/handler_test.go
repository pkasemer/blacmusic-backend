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
	"github.com/blacmusic/backend/internal/testutil"
)

func setupRouter(t *testing.T) (http.Handler, *artist.Store) {
	t.Helper()
	pool := testutil.Pool(t)
	store := artist.NewStore(pool, 10*testutil.GiB, "http://api.test")
	h := NewHandler(pool, store)
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
