package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/source"
	"github.com/blacmusic/backend/internal/testutil"
)

func TestMixes(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()
	r := chi.NewRouter()
	r.Use(auth.Require(auth.NewTokenService(testutil.Secret, time.Minute)))
	r.Post("/mixes", NewHandler(pool, source.NewRegistry(), "http://api.test").Mix)

	genre := testutil.Unique("mix")
	uid := testutil.User(t, pool, "user", true)
	artistID := testutil.Artist(t, pool, uid, testutil.Unique("Mix Band"), testutil.GiB)
	rel := testutil.Release(t, pool, artistID, "Mix", "album", "public")
	song := func(g, visibility string) string {
		id, _ := testutil.ReadyTrack(t, pool, uid, artistID, rel, "M", 10, visibility)
		if _, err := pool.Exec(ctx, `UPDATE tracks SET genre = $2 WHERE id = $1`, id, g); err != nil {
			t.Fatal(err)
		}
		return id
	}
	start, a, b, c := song(genre, "public"), song(genre, "public"), song(genre, "public"), song(genre, "public")
	song(genre, "draft")
	song(testutil.Unique("elsewhere"), "public")
	lonely := song(testutil.Unique("lonely"), "public")

	listener := testutil.User(t, pool, "user", true)
	token := testutil.Token(t, listener)
	mix := func(body map[string]any) (int, []string) {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest("POST", "/mixes", bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var out struct {
			Tracks []struct {
				ID string `json:"id"`
			} `json:"tracks"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		ids := []string{}
		for _, tr := range out.Tracks {
			ids = append(ids, tr.ID)
		}
		slices.Sort(ids)
		return rec.Code, ids
	}
	want := []string{a, c}
	slices.Sort(want)
	code, got := mix(map[string]any{"trackId": start, "userId": uid, "reason": "mix", "excludeTrackIds": []string{b}, "limit": 25})
	if code != http.StatusOK || !slices.Equal(got, want) {
		t.Fatalf("mix = %d %v, want %v", code, got, want)
	}
	var kept int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM mix_requests WHERE user_id = $1 AND track_id = $2 AND cardinality(suggested_ids) = 2`, listener, start).Scan(&kept); err != nil || kept != 1 {
		t.Fatalf("kept under the token's user = %d %v", kept, err)
	}
	if code, got := mix(map[string]any{"trackId": lonely, "reason": "autoplay"}); code != http.StatusOK || len(got) != 0 {
		t.Fatalf("nothing else in the genre = %d %v", code, got)
	}
	for _, x := range []struct {
		body map[string]any
		code int
	}{
		{map[string]any{"trackId": "trk_nothing", "reason": "mix"}, 404},
		{map[string]any{"trackId": start, "reason": "shuffle"}, 422},
		{map[string]any{"trackId": start, "reason": "mix", "excludeTrackIds": make([]string, 201)}, 422},
	} {
		if code, _ := mix(x.body); code != x.code {
			t.Fatalf("%v = %d, want %d", x.body["reason"], code, x.code)
		}
	}
}
