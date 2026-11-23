package upload

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/blobtest"
	"github.com/blacmusic/backend/internal/db"
)

// Integration test: needs a Postgres, e.g.
//
//	TEST_DATABASE_URL=postgres://blac:blac@localhost:5432/blac?sslmode=disable go test ./internal/upload
type env struct {
	t      *testing.T
	pool   *pgxpool.Pool
	blobs  *blobtest.Mem
	router http.Handler
	tokens *auth.TokenService
}

const testMax = 10 << 20

func setup(t *testing.T) *env {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	blobs := blobtest.New()
	tokens := auth.NewTokenService("test-secret-test-secret-test-secret!", time.Minute)
	h := NewHandler(pool, blobs, testMax, time.Hour)
	r := chi.NewRouter()
	r.Group(func(g chi.Router) {
		g.Use(auth.Require(tokens))
		g.Route("/uploads", h.Routes)
	})
	return &env{t: t, pool: pool, blobs: blobs, router: r, tokens: tokens}
}

// user creates an account with the given role and returns its bearer token.
func (e *env) user(role string) string {
	e.t.Helper()
	var id string
	err := e.pool.QueryRow(context.Background(),
		`INSERT INTO users(email, password_hash, display_name, role) VALUES ($1,'x','U',$2) RETURNING id::text`,
		fmt.Sprintf("up-%d@test.local", time.Now().UnixNano()), role).Scan(&id)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { e.pool.Exec(context.Background(), `DELETE FROM users WHERE id::text = $1`, id) }) //nolint:errcheck
	tok, _, err := e.tokens.NewAccess(id)
	if err != nil {
		e.t.Fatal(err)
	}
	return tok
}

func (e *env) do(method, path, token string, body any) (int, map[string]any) {
	e.t.Helper()
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
	e.router.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func validBody() map[string]any {
	return map[string]any{"title": "Song", "artistName": "Band", "filename": "song.flac", "sizeBytes": 1234}
}

func errCode(m map[string]any) string {
	if e, ok := m["error"].(map[string]any); ok {
		s, _ := e["code"].(string)
		return s
	}
	return ""
}

func TestOnlyCreatorsMayUpload(t *testing.T) {
	e := setup(t)
	if code, body := e.do("POST", "/uploads", e.user("user"), validBody()); code != 403 || errCode(body) != "forbidden" {
		t.Fatalf("plain user = %d %v, want 403 forbidden", code, body)
	}
	if code, _ := e.do("GET", "/uploads", e.user("user"), nil); code != 403 {
		t.Fatalf("plain user list = %d, want 403", code)
	}
	for _, role := range []string{"creator", "admin"} {
		if code, body := e.do("POST", "/uploads", e.user(role), validBody()); code != 201 {
			t.Fatalf("%s = %d %v, want 201", role, code, body)
		}
	}
}

func TestCreateReturnsPresignedPutAndHidesTrackID(t *testing.T) {
	e := setup(t)
	tok := e.user("creator")
	code, body := e.do("POST", "/uploads", tok, validBody())
	if code != 201 {
		t.Fatalf("create = %d %v", code, body)
	}
	id, _ := body["id"].(string)
	target, _ := body["upload"].(map[string]any)
	if !strings.HasPrefix(id, "upl_") || target["method"] != "PUT" || !strings.Contains(target["url"].(string), "originals/"+id) {
		t.Fatalf("bad response: %v", body)
	}
	if body["status"] != "pending" {
		t.Fatalf("status = %v", body["status"])
	}
	if _, leaked := body["trackId"]; leaked {
		t.Fatal("trackId must not be exposed before the track exists")
	}
	if body["maxBytes"].(float64) != testMax {
		t.Fatalf("maxBytes = %v", body["maxBytes"])
	}
}

func TestValidation(t *testing.T) {
	e := setup(t)
	tok := e.user("creator")
	cases := map[string]func(m map[string]any){
		"empty title":     func(m map[string]any) { m["title"] = "  " },
		"long title":      func(m map[string]any) { m["title"] = strings.Repeat("x", 201) },
		"no artist":       func(m map[string]any) { m["artistName"] = "" },
		"no filename":     func(m map[string]any) { m["filename"] = "" },
		"zero size":       func(m map[string]any) { m["sizeBytes"] = 0 },
		"over size limit": func(m map[string]any) { m["sizeBytes"] = testMax + 1 },
	}
	for name, mutate := range cases {
		b := validBody()
		mutate(b)
		if code, body := e.do("POST", "/uploads", tok, b); code != 422 || errCode(body) != "validation_error" {
			t.Errorf("%s = %d %v, want 422", name, code, body)
		}
	}
	if code, _ := e.do("POST", "/uploads", tok, map[string]any{"title": "x", "bogus": 1}); code != 400 {
		t.Errorf("unknown field = %d, want 400", code)
	}
}

func TestCompleteFlow(t *testing.T) {
	e := setup(t)
	tok := e.user("creator")
	_, created := e.do("POST", "/uploads", tok, validBody())
	id := created["id"].(string)

	// Not uploaded yet.
	if code, body := e.do("POST", "/uploads/"+id+"/complete", tok, nil); code != 409 || errCode(body) != "not_uploaded" {
		t.Fatalf("complete before PUT = %d %v, want 409 not_uploaded", code, body)
	}

	e.blobs.Put("originals/"+id, []byte("pretend this is audio"))
	if code, body := e.do("POST", "/uploads/"+id+"/complete", tok, nil); code != 202 || body["status"] != "queued" {
		t.Fatalf("complete = %d %v, want 202 queued", code, body)
	}
	// Idempotent.
	if code, body := e.do("POST", "/uploads/"+id+"/complete", tok, nil); code != 200 || body["status"] != "queued" {
		t.Fatalf("second complete = %d %v, want 200 queued", code, body)
	}
	if code, body := e.do("GET", "/uploads/"+id, tok, nil); code != 200 || body["status"] != "queued" {
		t.Fatalf("get = %d %v", code, body)
	}
	code, list := e.do("GET", "/uploads", tok, nil)
	if items, _ := list["uploads"].([]any); code != 200 || len(items) != 1 {
		t.Fatalf("list = %d %v", code, list)
	}
}

func TestEmptyOrOversizedFileIsRejected(t *testing.T) {
	e := setup(t)
	tok := e.user("creator")

	_, created := e.do("POST", "/uploads", tok, validBody())
	id := created["id"].(string)
	e.blobs.Put("originals/"+id, nil)
	if code, _ := e.do("POST", "/uploads/"+id+"/complete", tok, nil); code != 422 {
		t.Fatalf("empty file = %d, want 422", code)
	}
	if _, ok := e.blobs.Get("originals/" + id); ok {
		t.Fatal("rejected file must be removed from storage")
	}
	if _, body := e.do("GET", "/uploads/"+id, tok, nil); body["status"] != "failed" || body["error"] == "" {
		t.Fatalf("status = %v, want failed with a message", body)
	}

	_, created = e.do("POST", "/uploads", tok, validBody())
	id = created["id"].(string)
	e.blobs.Put("originals/"+id, make([]byte, testMax+1)) // client lied about the size
	if code, _ := e.do("POST", "/uploads/"+id+"/complete", tok, nil); code != 422 {
		t.Fatalf("oversized file = %d, want 422", code)
	}
}

func TestOtherUsersCannotSeeUploads(t *testing.T) {
	e := setup(t)
	owner, other := e.user("creator"), e.user("creator")
	_, created := e.do("POST", "/uploads", owner, validBody())
	id := created["id"].(string)
	e.blobs.Put("originals/"+id, []byte("x"))

	for _, c := range []struct{ m, p string }{{"GET", "/uploads/" + id}, {"POST", "/uploads/" + id + "/complete"}} {
		if code, _ := e.do(c.m, c.p, other, nil); code != 404 {
			t.Errorf("%s %s as other user = %d, want 404", c.m, c.p, code)
		}
	}
	if _, body := e.do("GET", "/uploads/"+id, owner, nil); body["status"] != "pending" {
		t.Fatal("another user's complete call must not change the upload")
	}
	if code, _ := e.do("GET", "/uploads/upl_nope", owner, nil); code != 404 {
		t.Errorf("unknown id = %d, want 404", code)
	}
}

func TestUnfinishedUploadCap(t *testing.T) {
	e := setup(t)
	tok := e.user("creator")
	for i := 0; i < MaxActivePerUser; i++ {
		if code, body := e.do("POST", "/uploads", tok, validBody()); code != 201 {
			t.Fatalf("upload %d = %d %v", i, code, body)
		}
	}
	if code, body := e.do("POST", "/uploads", tok, validBody()); code != 409 || errCode(body) != "limit_reached" {
		t.Fatalf("over the cap = %d %v, want 409 limit_reached", code, body)
	}
}

func TestUnauthenticated(t *testing.T) {
	e := setup(t)
	if code, _ := e.do("GET", "/uploads", "garbage", nil); code != 401 {
		t.Fatalf("bad token = %d, want 401", code)
	}
}
