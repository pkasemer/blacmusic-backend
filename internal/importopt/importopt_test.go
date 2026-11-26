package importopt

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/testutil"
)

func TestSeededOptionsAndListShape(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()
	s := NewStore(pool)

	if on, err := s.Enabled(ctx, Manual); err != nil || !on {
		t.Fatalf("manual = %v %v, want on", on, err)
	}
	if on, err := s.Enabled(ctx, YouTube); err != nil || on {
		t.Fatalf("youtube = %v %v, want off", on, err)
	}
	if on, err := s.Enabled(ctx, "nope"); err != nil || on {
		t.Fatalf("unknown option = %v %v, want off", on, err)
	}

	r := chi.NewRouter()
	r.Group(func(g chi.Router) {
		g.Use(auth.Require(auth.NewTokenService(testutil.Secret, time.Minute)))
		g.Get("/import-options", NewHandler(s).List)
	})
	req := httptest.NewRequest("GET", "/import-options", nil)
	req.Header.Set("Authorization", "Bearer "+testutil.Token(t, testutil.User(t, pool, "user", true)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("list = %d", rec.Code)
	}
	var out struct{ Options []map[string]any }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Options) < 2 {
		t.Fatalf("body = %s", rec.Body.String())
	}
	m, y := out.Options[0], out.Options[1]
	if m["id"] != "manual" || m["enabled"] != true || y["id"] != "youtube" || y["enabled"] != false || y["note"] != "Coming soon" {
		t.Fatalf("options = %v", out.Options)
	}
	if _, has := m["note"]; has {
		t.Fatalf("empty text must be left out: %v", m)
	}
	// no token, no list
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/import-options", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d", rec.Code)
	}
}

func TestUpsertCreatesChangesAndChecks(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()
	s := NewStore(pool)
	id := "t" + strings.ToLower(strings.ReplaceAll(testutil.Unique("opt"), "-", "_"))
	if len(id) > 30 {
		id = id[:30]
	}
	t.Cleanup(func() { pool.Exec(ctx, `DELETE FROM import_options WHERE id = $1`, id) }) //nolint:errcheck

	on := true
	o, err := s.Upsert(ctx, id, Patch{Title: ptr("Spotify"), Note: ptr("Soon")})
	if err != nil || o.Enabled || o.Title != "Spotify" || o.Note != "Soon" {
		t.Fatalf("new option = %+v %v (new options start disabled)", o, err)
	}
	o, err = s.Upsert(ctx, id, Patch{Enabled: &on})
	if err != nil || !o.Enabled || o.Title != "Spotify" || o.Note != "Soon" {
		t.Fatalf("enable = %+v %v (other fields must stay)", o, err)
	}
	if got, _ := s.Enabled(ctx, id); !got {
		t.Fatal("Enabled should follow")
	}
	o, err = s.Upsert(ctx, id, Patch{Note: ptr("")})
	if err != nil || o.Note != "" {
		t.Fatalf("clear note = %+v %v", o, err)
	}
	for name, c := range map[string]struct {
		id string
		p  Patch
	}{
		"bad id":      {"Bad Id", Patch{Enabled: &on}},
		"empty id":    {"", Patch{Enabled: &on}},
		"nothing":     {id, Patch{}},
		"long title":  {id, Patch{Title: ptr(strings.Repeat("x", 61))}},
		"long note":   {id, Patch{Note: ptr(strings.Repeat("x", 101))}},
		"long detail": {id, Patch{Description: ptr(strings.Repeat("x", 301))}},
	} {
		if _, err := s.Upsert(ctx, c.id, c.p); err == nil {
			t.Errorf("%s accepted", name)
		} else if _, ok := err.(*ValidationError); !ok {
			t.Errorf("%s = %v, want a validation error", name, err)
		}
	}
}

func ptr(s string) *string { return &s }
