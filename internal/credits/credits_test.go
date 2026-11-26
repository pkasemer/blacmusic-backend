package credits

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

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/blobtest"
	"github.com/blacmusic/backend/internal/release"
	"github.com/blacmusic/backend/internal/testutil"
	"github.com/blacmusic/backend/internal/trackview"
)

func TestCreditsEndToEnd(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()
	r := chi.NewRouter()
	r.Group(func(g chi.Router) {
		g.Use(auth.Require(auth.NewTokenService(testutil.Secret, time.Minute)))
		NewHandler(NewStore(pool)).Routes(g)
	})
	call := func(method, path, user string, body any) (int, map[string]any) {
		var rdr *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		} else {
			rdr = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(method, path, rdr)
		req.Header.Set("Authorization", "Bearer "+testutil.Token(t, user))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	list := func(b map[string]any) []map[string]any {
		var out []map[string]any
		for _, c := range b["credits"].([]any) {
			out = append(out, c.(map[string]any))
		}
		return out
	}

	owner := testutil.User(t, pool, "user", true)
	artist := testutil.Artist(t, pool, owner, testutil.Unique("Cred"), testutil.GiB)
	friendUser := testutil.User(t, pool, "user", true)
	friend := testutil.Artist(t, pool, friendUser, testutil.Unique("Friend"), testutil.GiB)
	rel := testutil.Release(t, pool, artist, "Credited", "album", "public")
	t1, _ := testutil.ReadyTrack(t, pool, owner, artist, rel, "First", 1, "public")
	t2, _ := testutil.ReadyTrack(t, pool, owner, artist, rel, "Second", 1, "public")
	pool.Exec(ctx, `UPDATE tracks SET track_number = 1 WHERE id = $1`, t1) //nolint:errcheck
	pool.Exec(ctx, `UPDATE tracks SET track_number = 2 WHERE id = $1`, t2) //nolint:errcheck
	otherRel := testutil.Release(t, pool, artist, "Elsewhere", "single", "public")
	otherTrack, _ := testutil.ReadyTrack(t, pool, owner, artist, otherRel, "Other", 1, "public")
	fan := testutil.User(t, pool, "user", true)

	// nothing yet: an empty list, not an error
	if code, b := call("GET", "/releases/"+rel+"/credits", fan, nil); code != 200 || len(list(b)) != 0 {
		t.Fatalf("empty = %d %v", code, b)
	}

	put := []map[string]any{
		{"name": "  Second   Writer ", "role": "songwriter", "trackId": t2},
		{"name": "Ace Producer", "role": "producer"},
		{"name": "First Writer", "role": "writer", "trackId": t1, "artistId": friend},
		{"name": "ace producer", "role": "producer"}, // the same person and role: collapsed
		{"name": "Sound Guild", "role": "label", "roleLabel": "ignored"},
		{"name": "Vocal Coach", "role": "other", "roleLabel": "Vocal coaching"},
	}
	code, b := call("PUT", "/releases/"+rel+"/credits", owner, map[string]any{"credits": put})
	if code != 200 {
		t.Fatalf("put = %d %v", code, b)
	}
	got := list(b)
	var names []string
	for _, c := range got {
		names = append(names, c["name"].(string))
	}
	// release-level first (in the order sent), then by track number
	if strings.Join(names, ",") != "Ace Producer,Sound Guild,Vocal Coach,First Writer,Second Writer" {
		t.Fatalf("order = %v", names)
	}
	if got[3]["trackId"] != t1 || got[3]["trackTitle"] != "First" || got[3]["artistId"] != friend {
		t.Fatalf("track credit = %v", got[3])
	}
	if _, has := got[1]["roleLabel"]; has {
		t.Fatalf("roleLabel only belongs to role other: %v", got[1])
	}
	if got[2]["roleLabel"] != "Vocal coaching" || got[2]["id"] == "" {
		t.Fatalf("other role = %v", got[2])
	}
	if _, has := got[0]["trackId"]; has {
		t.Fatalf("a release-level credit has no trackId: %v", got[0])
	}

	// any listener reads them; the track endpoint adds the scope
	if code, b = call("GET", "/releases/"+rel+"/credits", fan, nil); code != 200 || len(list(b)) != 5 {
		t.Fatalf("listener get = %d %v", code, b)
	}
	code, b = call("GET", "/tracks/"+t1+"/credits", fan, nil)
	tc := list(b)
	if code != 200 || len(tc) != 4 || tc[0]["scope"] != "track" || tc[0]["name"] != "First Writer" || tc[1]["scope"] != "release" {
		t.Fatalf("track credits = %d %v", code, b)
	}
	if code, b = call("GET", "/tracks/"+otherTrack+"/credits", fan, nil); code != 200 || len(list(b)) != 0 {
		t.Fatalf("a track of another release = %d %v", code, b)
	}

	// hasCredits follows
	tr, err := trackview.Scan(pool.QueryRow(ctx, trackview.Select("")+` WHERE t.id = $1`, t2), "http://api.test")
	if err != nil || !tr.HasCredits {
		t.Fatalf("track hasCredits = %+v %v", tr.HasCredits, err)
	}
	tr, _ = trackview.Scan(pool.QueryRow(ctx, trackview.Select("")+` WHERE t.id = $1`, otherTrack), "http://api.test")
	if tr.HasCredits {
		t.Fatal("a track of a release without credits claims credits")
	}
	rs := release.NewStore(pool, blobtest.New(), "http://api.test")
	if rr, err := rs.Get(ctx, rel, ""); err != nil || !rr.HasCredits || !rr.Tracks[0].HasCredits {
		t.Fatalf("release hasCredits = %+v %v", rr, err)
	}
	if rr, _ := rs.Get(ctx, otherRel, ""); rr.HasCredits {
		t.Fatal("a release without credits claims credits")
	}

	// validation
	bad := map[string][]map[string]any{
		"unknown role":    {{"name": "X", "role": "dj"}},
		"empty name":      {{"name": "  ", "role": "producer"}},
		"long name":       {{"name": strings.Repeat("n", 101), "role": "producer"}},
		"long role label": {{"name": "X", "role": "other", "roleLabel": strings.Repeat("l", 41)}},
		"foreign track":   {{"name": "X", "role": "producer", "trackId": otherTrack}},
		"unknown artist":  {{"name": "X", "role": "producer", "artistId": "art_nope"}},
	}
	for name, c := range bad {
		if code, b := call("PUT", "/releases/"+rel+"/credits", owner, map[string]any{"credits": c}); code != 422 {
			t.Errorf("%s = %d %v, want 422", name, code, b)
		}
	}
	many := make([]map[string]any, 101)
	for i := range many {
		many[i] = map[string]any{"name": "Person " + strings.Repeat("x", i%5) + string(rune('a'+i%26)) + string(rune('a'+i/26)), "role": "producer"}
	}
	if code, _ := call("PUT", "/releases/"+rel+"/credits", owner, map[string]any{"credits": many}); code != 422 {
		t.Errorf("101 credits = %d, want 422", code)
	}
	if code, _ := call("PUT", "/releases/"+rel+"/credits", owner, map[string]any{}); code != 400 {
		t.Errorf("no credits key = %d, want 400", code)
	}
	if code, _ := call("PUT", "/releases/"+rel+"/credits", owner, map[string]any{"credits": []any{}, "extra": 1}); code != 400 {
		t.Errorf("unknown field = %d, want 400", code)
	}
	// a rejected save changes nothing
	if _, b = call("GET", "/releases/"+rel+"/credits", fan, nil); len(list(b)) != 5 {
		t.Fatalf("a rejected save changed the list: %v", b)
	}

	// somebody else's release
	if code, _ := call("PUT", "/releases/"+rel+"/credits", fan, map[string]any{"credits": []any{}}); code != 404 {
		t.Fatalf("stranger put = %d, want 404", code)
	}

	// names used before, most used first; only for artists
	pool.Exec(ctx, `INSERT INTO release_credits (release_id, name, role) VALUES ($1, 'ace producer', 'producer')`, otherRel) //nolint:errcheck
	code, b = call("GET", "/credits/suggest?q=ace", owner, nil)
	names2, _ := b["names"].([]any)
	if code != 200 || len(names2) != 1 || names2[0] != "Ace Producer" {
		t.Fatalf("suggest = %d %v", code, b)
	}
	if code, b = call("GET", "/credits/suggest", owner, nil); code != 200 || len(b["names"].([]any)) != 5 {
		t.Fatalf("suggest with no q = %d %v", code, b)
	}
	if code, _ = call("GET", "/credits/suggest?q=a", fan, nil); code != 403 {
		t.Fatalf("suggest as a non-artist = %d, want 403", code)
	}

	// a draft release is private; deleting a track takes its credits along; an empty list clears all
	draftRel := testutil.Release(t, pool, artist, "Draft", "single", "draft")
	if code, _ := call("PUT", "/releases/"+draftRel+"/credits", owner, map[string]any{"credits": []map[string]any{{"name": "Me", "role": "producer"}}}); code != 200 {
		t.Fatalf("credits on a draft = %d", code)
	}
	if code, _ := call("GET", "/releases/"+draftRel+"/credits", fan, nil); code != 404 {
		t.Fatalf("a stranger reading a draft's credits = %d, want 404", code)
	}
	if code, _ := call("GET", "/releases/"+draftRel+"/credits", owner, nil); code != 200 {
		t.Fatalf("the owner reading a draft's credits = %d", code)
	}
	pool.Exec(ctx, `DELETE FROM tracks WHERE id = $1`, t1) //nolint:errcheck
	if code, b = call("GET", "/releases/"+rel+"/credits", fan, nil); code != 200 || len(list(b)) != 4 {
		t.Fatalf("after deleting a track = %d %v, want its credit gone", code, b)
	}
	if code, b = call("PUT", "/releases/"+rel+"/credits", owner, map[string]any{"credits": []any{}}); code != 200 || len(list(b)) != 0 {
		t.Fatalf("clear = %d %v", code, b)
	}
	if code, _ = call("GET", "/tracks/trk_nope/credits", fan, nil); code != http.StatusNotFound {
		t.Fatalf("unknown track = %d", code)
	}
}
