package catalog

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/source"
	"github.com/blacmusic/backend/internal/testutil"
)

func TestTrendingIsRealActivityNotTheNewestSongs(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()
	r := chi.NewRouter()
	r.Use(auth.Require(auth.NewTokenService(testutil.Secret, time.Minute)))
	r.Get("/browse/tracks", NewHandler(pool, source.NewRegistry(), "http://api.test").Browse)

	genre := testutil.Unique("trend")
	uid := testutil.User(t, pool, "user", true)
	artistID := testutil.Artist(t, pool, uid, testutil.Unique("Trend Band"), testutil.GiB)
	rel := testutil.Release(t, pool, artistID, "Trends", "album", "public")
	var songs []string
	for range 10 {
		id, _ := testutil.ReadyTrack(t, pool, uid, artistID, rel, "T", 10, "public")
		if _, err := pool.Exec(ctx, `UPDATE tracks SET genre = $2 WHERE id = $1`, id, genre); err != nil {
			t.Fatal(err)
		}
		songs = append(songs, id)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM plays WHERE artist_id = $1`, artistID) }) //nolint:errcheck
	fans := []string{}
	for range 3 {
		fans = append(fans, testutil.User(t, pool, "user", true))
	}
	play := func(song int, fan, times, daysAgo int) {
		for range times {
			if _, err := pool.Exec(ctx, `INSERT INTO plays (user_id, track_id, artist_id, listened_sec, at)
				VALUES ($1, $2, $3, 40, date_trunc('day', now() - make_interval(days => $4)))`, fans[fan], songs[song], artistID, daysAgo); err != nil {
				t.Fatal(err)
			}
		}
	}
	play(0, 0, 1, 1) // three listeners this week
	play(0, 1, 1, 1)
	play(0, 2, 1, 1)
	play(1, 0, 20, 1) // one listener on a loop counts once
	play(1, 1, 1, 1)
	play(2, 0, 1, 1)
	play(3, 0, 1, 20) // only earlier this month
	play(3, 1, 1, 20)
	play(4, 0, 1, 60) // too long ago
	if _, err := pool.Exec(ctx, `INSERT INTO library_tracks (user_id, track_id) VALUES ($1, $2)`, fans[0], songs[5]); err != nil {
		t.Fatal(err) // a new save counts double
	}
	// songs 6-9 are the newest and nobody has touched them

	token := testutil.Token(t, testutil.User(t, pool, "user", true))
	code, got := browse(t, r, token, "?sort=trending&genre="+genre)
	var ids []string
	for _, tr := range got {
		ids = append(ids, tr["id"].(string))
	}
	// songs 5 and 1 both score 2: song 1 has more plays
	want := []string{songs[0], songs[1], songs[5], songs[2], songs[3]}
	if code != http.StatusOK || !slices.Equal(ids, want) {
		t.Fatalf("trending = %d %v, want %v", code, ids, want)
	}
	// the request installed apps make for "Trending now": the same songs lead, and the rest still follow
	_, got = browse(t, r, token, "?sort=popular&limit=24&genre="+genre)
	ids = ids[:0]
	for _, tr := range got {
		ids = append(ids, tr["id"].(string))
	}
	if len(ids) != len(songs) || !slices.Equal(ids[:len(want)], want) {
		t.Fatalf("popular = %v, want %v first", ids, want)
	}
	for i := 6; i < 10; i++ {
		play(i, 0, 1, 1)
	}
	if _, got = browse(t, r, token, "?sort=trending&genre="+genre); len(got) != 6 || got[0]["id"] != songs[0] {
		t.Fatalf("trending is a top 6: %d", len(got))
	}
}
