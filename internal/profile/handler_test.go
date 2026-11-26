package profile

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/artist"
	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/image"
	"github.com/blacmusic/backend/internal/testutil"
)

const base = "http://api.test"

type env struct {
	t      *testing.T
	pool   *pgxpool.Pool
	svc    *auth.Service
	router http.Handler
	art    *artist.Store
}

func setup(t *testing.T) *env {
	t.Helper()
	pool := testutil.Pool(t)
	tokens := auth.NewTokenService(testutil.Secret, time.Minute)
	svc := auth.NewService(pool, tokens, time.Hour)
	svc.SetAvatarURL(func(id string) string { return image.URL(base, id, image.SizeMedium) })
	authH := auth.NewHandler(svc)
	h := NewHandler(pool, svc)
	r := chi.NewRouter()
	r.Group(func(g chi.Router) {
		g.Use(auth.Require(tokens))
		g.Get("/me", authH.Me)
		g.Patch("/me", h.Update)
	})
	return &env{t: t, pool: pool, svc: svc, router: r, art: artist.NewStore(pool, testutil.GiB, base)}
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

func (e *env) image(owner, purpose, status string) string {
	e.t.Helper()
	var id string
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO images (owner_id, purpose, status, original_key, size_bytes)
		VALUES ($1,$2,$3,'k',1) RETURNING id`, owner, purpose, status).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func errCode(m map[string]any) string {
	if e, ok := m["error"].(map[string]any); ok {
		s, _ := e["code"].(string)
		return s
	}
	return ""
}

func TestSetAndClearTheProfilePicture(t *testing.T) {
	e := setup(t)
	uid := testutil.User(t, e.pool, "user", false) // no verified email needed
	tok := testutil.Token(t, uid)

	if code, me := e.do("GET", "/me", tok, nil); code != 200 || me["avatarUrl"] != nil {
		t.Fatalf("a new user has no avatar: %d %v", code, me)
	}

	img := e.image(uid, "avatar", "ready")
	code, me := e.do("PATCH", "/me", tok, map[string]string{"avatarImageId": img})
	want := base + "/v1/media/" + img + "/medium.jpg"
	if code != 200 || me["avatarUrl"] != want || me["emailVerified"] != false {
		t.Fatalf("set avatar = %d %v, want avatarUrl %s", code, me, want)
	}
	if _, me = e.do("GET", "/me", tok, nil); me["avatarUrl"] != want {
		t.Fatalf("GET /me must show the picture: %v", me)
	}

	// Changing the name alone keeps the picture.
	code, me = e.do("PATCH", "/me", tok, map[string]string{"displayName": "  New Name "})
	if code != 200 || me["displayName"] != "New Name" || me["avatarUrl"] != want {
		t.Fatalf("rename = %d %v", code, me)
	}

	// "" removes it.
	code, me = e.do("PATCH", "/me", tok, map[string]string{"avatarImageId": ""})
	if code != 200 || me["avatarUrl"] != nil {
		t.Fatalf("clear = %d %v", code, me)
	}
}

func TestOnlyYourOwnFinishedAvatarImagesAreAccepted(t *testing.T) {
	e := setup(t)
	uid := testutil.User(t, e.pool, "user", true)
	other := testutil.User(t, e.pool, "user", true)
	tok := testutil.Token(t, uid)
	for name, id := range map[string]string{
		"someone else's": e.image(other, "avatar", "ready"),
		"a cover":        e.image(uid, "cover", "ready"),
		"unfinished":     e.image(uid, "avatar", "pending"),
		"rejected":       e.image(uid, "avatar", "failed"),
		"unknown":        "img_00000000000000000000000000000000",
		"malformed":      "../../etc/passwd",
	} {
		if code, b := e.do("PATCH", "/me", tok, map[string]string{"avatarImageId": id}); code != 422 || errCode(b) != "validation_error" {
			t.Errorf("%s = %d %v, want 422", name, code, b)
		}
	}
	if _, me := e.do("GET", "/me", tok, nil); me["avatarUrl"] != nil {
		t.Fatal("a refused update must not change anything")
	}
}

func TestValidationAndAuth(t *testing.T) {
	e := setup(t)
	tok := testutil.Token(t, testutil.User(t, e.pool, "user", true))
	for name, body := range map[string]any{
		"empty body":       map[string]any{},
		"blank name":       map[string]string{"displayName": "   "},
		"long name":        map[string]string{"displayName": strings.Repeat("n", 51)},
		"unknown field":    map[string]string{"email": "x@y.z"},
		"wrong type":       map[string]any{"displayName": 5},
		"role escalation?": map[string]string{"role": "admin"},
	} {
		if code, _ := e.do("PATCH", "/me", tok, body); code != 400 && code != 422 {
			t.Errorf("%s = %d, want 400/422", name, code)
		}
	}
	if code, _ := e.do("PATCH", "/me", "garbage", map[string]string{"displayName": "x"}); code != 401 {
		t.Fatalf("no token = %d, want 401", code)
	}
}

// The picture is one per person: the artist profile shows the user's avatar.
func TestTheArtistImageIsTheUsersPicture(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid := testutil.User(t, e.pool, "user", true)
	tok := testutil.Token(t, uid)

	// An avatar set BEFORE becoming an artist is the artist's picture from the start.
	first := e.image(uid, "avatar", "ready")
	e.do("PATCH", "/me", tok, map[string]string{"avatarImageId": first})
	a, err := e.art.Create(ctx, uid, testutil.Unique("One Picture"), "")
	if err != nil {
		t.Fatal(err)
	}
	if want := base + "/v1/media/" + first + "/medium.jpg"; a.ImageURL != want {
		t.Fatalf("new artist image = %q, want the user's %q", a.ImageURL, want)
	}

	// Changing the user's picture changes the artist's, everywhere.
	second := e.image(uid, "avatar", "ready")
	e.do("PATCH", "/me", tok, map[string]string{"avatarImageId": second})
	got, _ := e.art.Get(ctx, a.ID, "")
	if want := base + "/v1/media/" + second + "/medium.jpg"; got.ImageURL != want {
		t.Fatalf("artist image after the user changed theirs = %q, want %q", got.ImageURL, want)
	}

	// The old artist-side field is an alias for the same picture.
	third := e.image(uid, "avatar", "ready")
	if _, err := e.art.Update(ctx, a.ID, uid, artist.Patch{AvatarImageID: &third}); err != nil {
		t.Fatal(err)
	}
	_, me := e.do("GET", "/me", tok, nil)
	if want := base + "/v1/media/" + third + "/medium.jpg"; me["avatarUrl"] != want {
		t.Fatalf("PATCH /artists avatarImageId must set the user's picture too: %v", me["avatarUrl"])
	}

	// Removing the picture removes it from the artist as well.
	e.do("PATCH", "/me", tok, map[string]string{"avatarImageId": ""})
	if got, _ = e.art.Get(ctx, a.ID, ""); got.ImageURL != "" {
		t.Fatalf("artist image after clearing = %q, want none", got.ImageURL)
	}
}

func TestLoginReturnsTheAvatar(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	email := testutil.Unique("login") + "@test.local"
	u, _, err := e.svc.Register(ctx, email, "supersecret1", "Singer")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, u.ID) }) //nolint:errcheck
	if u.AvatarURL != "" {
		t.Fatalf("register: %q", u.AvatarURL)
	}
	img := e.image(u.ID, "avatar", "ready")
	if _, err := e.pool.Exec(ctx, `UPDATE users SET avatar_image_id = $2 WHERE id = $1`, u.ID, img); err != nil {
		t.Fatal(err)
	}
	got, _, err := e.svc.Login(ctx, email, "supersecret1")
	if err != nil {
		t.Fatal(err)
	}
	if want := base + "/v1/media/" + img + "/medium.jpg"; got.AvatarURL != want {
		t.Fatalf("login avatarUrl = %q, want %q", got.AvatarURL, want)
	}
}
