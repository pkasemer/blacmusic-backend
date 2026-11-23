package image

import (
	"bytes"
	"context"
	"encoding/json"
	"image/color"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/blobtest"
	"github.com/blacmusic/backend/internal/testutil"
)

var colorBlue = color.RGBA{0, 0, 255, 255}

type env struct {
	t      *testing.T
	pool   *pgxpool.Pool
	blobs  *blobtest.Mem
	router http.Handler
}

func setup(t *testing.T) *env {
	t.Helper()
	pool := testutil.Pool(t)
	blobs := blobtest.New()
	h := NewHandler(pool, blobs, "http://api.test")
	r := chi.NewRouter()
	r.Route("/v1/media", h.ServeRoute)
	r.Group(func(g chi.Router) {
		g.Use(auth.Require(auth.NewTokenService(testutil.Secret, time.Minute)))
		g.Route("/images", h.Routes)
	})
	return &env{t: t, pool: pool, blobs: blobs, router: r}
}

func (e *env) do(method, path, token string, body any) (int, map[string]any, http.Header, []byte) {
	e.t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec.Header(), rec.Body.Bytes()
}

func errCode(m map[string]any) string {
	if e, ok := m["error"].(map[string]any); ok {
		s, _ := e["code"].(string)
		return s
	}
	return ""
}

// start creates an image upload and returns its id.
func (e *env) start(token, purpose string, size int) string {
	e.t.Helper()
	code, b, _, _ := e.do("POST", "/images", token, map[string]any{"purpose": purpose, "sizeBytes": size})
	if code != 201 {
		e.t.Fatalf("create image = %d %v", code, b)
	}
	return b["id"].(string)
}

func TestImageLifecycle(t *testing.T) {
	e := setup(t)
	uid := testutil.User(t, e.pool, "user", true)
	tok := testutil.Token(t, uid)
	raw := solidPNG(t, 900, 900, colorBlue)

	code, b, _, _ := e.do("POST", "/images", tok, map[string]any{"purpose": "cover", "sizeBytes": len(raw)})
	if code != 201 || b["status"] != "pending" {
		t.Fatalf("create = %d %v", code, b)
	}
	id := b["id"].(string)
	up := b["upload"].(map[string]any)
	if up["method"] != "PUT" || !strings.Contains(up["url"].(string), "images/originals/"+id) {
		t.Fatalf("upload target: %v", up)
	}

	// Completing before the file arrives is a clear conflict.
	if code, b, _, _ = e.do("POST", "/images/"+id+"/complete", tok, nil); code != 409 || errCode(b) != "not_uploaded" {
		t.Fatalf("complete before PUT = %d %v", code, b)
	}

	e.blobs.Put("images/originals/"+id, raw)
	code, b, _, _ = e.do("POST", "/images/"+id+"/complete", tok, nil)
	if code != 200 || b["status"] != "ready" || b["width"].(float64) != 900 {
		t.Fatalf("complete = %d %v", code, b)
	}
	urls := b["urls"].(map[string]any)
	if urls["medium"] != "http://api.test/v1/media/"+id+"/medium.jpg" || len(urls) != 3 {
		t.Fatalf("urls = %v", urls)
	}
	if _, ok := e.blobs.Get("images/originals/" + id); ok {
		t.Fatal("the raw upload must be deleted once processed")
	}
	// Completing again is harmless.
	if code, b, _, _ = e.do("POST", "/images/"+id+"/complete", tok, nil); code != 200 || b["status"] != "ready" {
		t.Fatalf("second complete = %d %v", code, b)
	}

	// Anyone can fetch it (no token), cached for a year, with the right type.
	code, _, h, body := e.do("GET", "/v1/media/"+id+"/medium.jpg", "", nil)
	if code != 200 || h.Get("Content-Type") != "image/jpeg" || !strings.Contains(h.Get("Cache-Control"), "immutable") ||
		h.Get("X-Content-Type-Options") != "nosniff" || len(body) < 100 || body[0] != 0xFF || body[1] != 0xD8 {
		t.Fatalf("serve = %d %v (%d bytes)", code, h, len(body))
	}
	etag := h.Get("ETag")
	req := httptest.NewRequest("GET", "/v1/media/"+id+"/medium.jpg", nil)
	req.Header.Set("If-None-Match", etag)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if rec.Code != 304 {
		t.Fatalf("conditional GET = %d, want 304", rec.Code)
	}
	for _, p := range []string{"/v1/media/" + id + "/huge.jpg", "/v1/media/" + id + "/medium.png", "/v1/media/img_nope/medium.jpg",
		"/v1/media/..%2Fx/medium.jpg", "/v1/media/" + id + "/medium"} {
		if code, _, _, _ := e.do("GET", p, "", nil); code != 404 {
			t.Errorf("GET %s = %d, want 404", p, code)
		}
	}
	// The raw upload path must never be reachable through the public route.
	if code, _, _, _ := e.do("GET", "/v1/media/originals/"+id, "", nil); code != 404 {
		t.Errorf("originals reachable: %d", code)
	}
}

func TestNotAnImageIsRejected(t *testing.T) {
	e := setup(t)
	tok := testutil.Token(t, testutil.User(t, e.pool, "user", true))
	id := e.start(tok, "avatar", 20)
	e.blobs.Put("images/originals/"+id, []byte("this is a text file"))
	code, b, _, _ := e.do("POST", "/images/"+id+"/complete", tok, nil)
	if code != 422 || errCode(b) != "invalid_image" {
		t.Fatalf("complete = %d %v, want 422 invalid_image", code, b)
	}
	if _, ok := e.blobs.Get("images/originals/" + id); ok {
		t.Fatal("a rejected file must be deleted")
	}
	// A rejected upload stays rejected.
	if code, _, _, _ = e.do("POST", "/images/"+id+"/complete", tok, nil); code != 422 {
		t.Fatalf("retry of a rejected upload = %d, want 422", code)
	}
}

func TestOversizedFileIsRejected(t *testing.T) {
	e := setup(t)
	tok := testutil.Token(t, testutil.User(t, e.pool, "user", true))
	id := e.start(tok, "cover", 1000)
	e.blobs.Put("images/originals/"+id, make([]byte, MaxUploadBytes+1)) // the client lied about the size
	if code, b, _, _ := e.do("POST", "/images/"+id+"/complete", tok, nil); code != 422 {
		t.Fatalf("oversized = %d %v, want 422", code, b)
	}
}

func TestValidationOwnershipAndCap(t *testing.T) {
	e := setup(t)
	uid := testutil.User(t, e.pool, "user", true)
	tok := testutil.Token(t, uid)
	for name, body := range map[string]map[string]any{
		"bad purpose": {"purpose": "banner", "sizeBytes": 10},
		"zero size":   {"purpose": "cover", "sizeBytes": 0},
		"too big":     {"purpose": "cover", "sizeBytes": MaxUploadBytes + 1},
	} {
		if code, _, _, _ := e.do("POST", "/images", tok, body); code != 422 {
			t.Errorf("%s = %d, want 422", name, code)
		}
	}

	id := e.start(tok, "cover", 10)
	other := testutil.Token(t, testutil.User(t, e.pool, "user", true))
	if code, _, _, _ := e.do("POST", "/images/"+id+"/complete", other, nil); code != 404 {
		t.Fatalf("someone else's complete = %d, want 404", code)
	}
	if code, _, _, _ := e.do("POST", "/images/not-an-id/complete", tok, nil); code != 404 {
		t.Fatalf("malformed id = %d, want 404", code)
	}
	if code, _, _, _ := e.do("POST", "/images", "garbage", nil); code != 401 {
		t.Fatalf("bad token = %d, want 401", code)
	}

	for i := 1; i < MaxImagesPerUser; i++ {
		e.start(tok, "cover", 10)
	}
	if code, b, _, _ := e.do("POST", "/images", tok, map[string]any{"purpose": "cover", "sizeBytes": 10}); code != 409 || errCode(b) != "limit_reached" {
		t.Fatalf("over the cap = %d %v, want 409 limit_reached", code, b)
	}
}

func TestExpirePendingRemovesAbandonedUploads(t *testing.T) {
	e := setup(t)
	tok := testutil.Token(t, testutil.User(t, e.pool, "user", true))
	id := e.start(tok, "cover", 10)
	e.blobs.Put("images/originals/"+id, []byte("partial"))
	if _, err := e.pool.Exec(context.Background(), `UPDATE images SET created_at = now() - interval '5 hours' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	n, err := ExpirePending(context.Background(), e.pool, e.blobs, 2*time.Hour)
	if err != nil || n < 1 {
		t.Fatalf("expired %d, %v", n, err)
	}
	if _, ok := e.blobs.Get("images/originals/" + id); ok {
		t.Fatal("the partial file must be deleted")
	}
	var left int
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM images WHERE id=$1`, id).Scan(&left) //nolint:errcheck
	if left != 0 {
		t.Fatal("the row must be deleted")
	}
}

func TestCheckOwned(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid := testutil.User(t, e.pool, "user", true)
	other := testutil.User(t, e.pool, "user", true)
	var id string
	if err := e.pool.QueryRow(ctx, `INSERT INTO images (owner_id, purpose, status, original_key, size_bytes)
		VALUES ($1,'cover','ready','k',1) RETURNING id`, uid).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := CheckOwned(ctx, e.pool, id, uid, "cover"); err != nil {
		t.Fatalf("owner: %v", err)
	}
	for name, err := range map[string]error{
		"other user":    CheckOwned(ctx, e.pool, id, other, "cover"),
		"wrong purpose": CheckOwned(ctx, e.pool, id, uid, "avatar"),
		"unknown":       CheckOwned(ctx, e.pool, "img_00000000000000000000000000000000", uid, "cover"),
		"malformed":     CheckOwned(ctx, e.pool, "x'; drop table images;--", uid, "cover"),
	} {
		if err == nil {
			t.Errorf("%s: CheckOwned must fail", name)
		}
	}
}
