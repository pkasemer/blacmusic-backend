package artist_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/blacmusic/backend/internal/artist"
	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/credits"
	"github.com/blacmusic/backend/internal/lyrics"
	"github.com/blacmusic/backend/internal/release"
	"github.com/blacmusic/backend/internal/testutil"
	"github.com/blacmusic/backend/internal/transfer"
	"github.com/blacmusic/backend/internal/upload"
	"github.com/blacmusic/backend/internal/youtube"
)

// studioRoutes mounts the studio's handlers as the server does; they are only listed, never called.
func studioRoutes() chi.Router {
	r := chi.NewRouter()
	r.Route("/v1", func(v1 chi.Router) {
		uploads := upload.NewHandler(nil, nil, upload.Options{})
		v1.Route("/uploads", uploads.Routes)
		uploads.TrackRoutes(v1)
		artist.NewHandler(nil).Routes(v1)
		release.NewHandler(nil).Routes(v1)
		lyrics.NewHandler(nil).Routes(v1)
		credits.NewHandler(nil).Routes(v1)
		v1.Route("/transfers", transfer.NewHandler(nil).Routes)
		v1.Route("/integrations/youtube", youtube.NewHandler(nil, nil, nil, "").Routes)
	})
	return r
}

func TestEveryStudioChangeIsGuarded(t *testing.T) {
	// the only changes under these routes that a suspended artist may still make
	allowed := map[string]bool{
		"POST /v1/artists/x/follow": true, "DELETE /v1/artists/x/follow": true, // listening
		"DELETE /v1/integrations/youtube": true, // taking their own data back
	}
	params := regexp.MustCompile(`\{[^}]+\}`)
	checked := 0
	err := chi.Walk(studioRoutes(), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == http.MethodGet {
			return nil
		}
		path := strings.TrimSuffix(params.ReplaceAllString(route, "x"), "/")
		checked++
		if got, want := artist.StudioWrite(method, path), !allowed[method+" "+path]; got != want {
			t.Errorf("%s %s: guarded = %v, want %v (see StudioWrite)", method, path, got, want)
		}
		return nil
	})
	if err != nil || checked < 25 {
		t.Fatalf("walked %d changing routes, %v", checked, err)
	}
	// what has nothing to do with the studio stays open, and so does all reading
	for _, p := range []string{"/v1/me", "/v1/me/plays", "/v1/me/library/tracks/x", "/v1/playlists", "/v1/playlists/x/items", "/v1/images", "/v1/auth/logout", "/v1/me/devices"} {
		if artist.StudioWrite(http.MethodPost, p) || artist.StudioWrite(http.MethodDelete, p) {
			t.Errorf("%s must stay open to a suspended artist", p)
		}
	}
	for _, p := range []string{"/v1/me/artist", "/v1/me/releases", "/v1/releases/x", "/v1/uploads", "/v1/me/artist/stats"} {
		if artist.StudioWrite(http.MethodGet, p) {
			t.Errorf("reading %s must stay open", p)
		}
	}
}

func TestSuspendedGuard(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()
	owner := testutil.User(t, pool, "user", true)
	art := testutil.Artist(t, pool, owner, testutil.Unique("Guard"), testutil.GiB)
	listener := testutil.User(t, pool, "user", true)
	r := chi.NewRouter()
	r.Group(func(g chi.Router) {
		g.Use(auth.Require(auth.NewTokenService(testutil.Secret, time.Minute)))
		g.Use(artist.SuspendedGuard(pool))
		g.HandleFunc("/*", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	})
	do := func(user, method, path string) (int, string) {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+testutil.Token(t, user))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var b struct {
			Error struct{ Code string } `json:"error"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &b)
		return w.Code, b.Error.Code
	}
	studio := [][2]string{{"PATCH", "/v1/artists/" + art}, {"POST", "/v1/releases"}, {"DELETE", "/v1/releases/x"}, {"DELETE", "/v1/tracks/x"},
		{"POST", "/v1/uploads"}, {"PUT", "/v1/tracks/x/lyrics"}, {"POST", "/v1/transfers"}}
	for _, c := range studio {
		if code, _ := do(owner, c[0], c[1]); code != 204 {
			t.Fatalf("before the ban %s %s = %d", c[0], c[1], code)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE artists SET banned_at = now() WHERE id = $1`, art); err != nil {
		t.Fatal(err)
	}
	for _, c := range studio {
		if code, errCode := do(owner, c[0], c[1]); code != 403 || errCode != "artist_banned" {
			t.Errorf("suspended %s %s = %d %q, want 403 artist_banned", c[0], c[1], code, errCode)
		}
		if code, _ := do(listener, c[0], c[1]); code != 204 {
			t.Errorf("another person %s %s = %d", c[0], c[1], code)
		}
	}
	for _, c := range [][2]string{{"GET", "/v1/me/artist"}, {"GET", "/v1/me/releases"}, {"PUT", "/v1/me/library/tracks/x"}, {"POST", "/v1/artists/other/follow"},
		{"POST", "/v1/playlists"}, {"DELETE", "/v1/me"}, {"DELETE", "/v1/integrations/youtube"}} {
		if code, _ := do(owner, c[0], c[1]); code != 204 {
			t.Errorf("a suspended artist's %s %s = %d, want it open", c[0], c[1], code)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE artists SET banned_at = NULL WHERE id = $1`, art); err != nil {
		t.Fatal(err)
	}
	if code, _ := do(owner, "POST", "/v1/releases"); code != 204 {
		t.Errorf("after the unban = %d", code)
	}
}
