package recommend

import (
	"context"
	"slices"
	"strconv"
	"testing"

	"github.com/blacmusic/backend/internal/testutil"
)

func TestTheDataFoundation(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	owner := testutil.User(t, pool, "user", true)
	art := testutil.Artist(t, pool, owner, testutil.Unique("Foundation"), testutil.GiB)
	rel := testutil.Release(t, pool, art, "R", "album", "public")
	genre := testutil.Unique("fd")
	songs := []string{}
	for range 6 {
		id, _ := testutil.ReadyTrack(t, pool, owner, art, rel, "S", 10, "public")
		exec(`UPDATE tracks SET genre = $2 WHERE id = $1`, id, genre)
		songs = append(songs, id)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM plays WHERE artist_id = $1`, art) }) //nolint:errcheck
	// only a verified language counts; a pending or rejected one says nothing
	exec(`INSERT INTO track_languages (track_id, language_id, role, verification) VALUES ($1, 'luganda', 'primary', 'verified'),
		($2, 'luganda', 'primary', 'pending'), ($3, 'luganda', 'primary', 'rejected')`, songs[0], songs[1], songs[2])
	for i, want := range []int{1, 0, 0} {
		s, err := Song(ctx, pool, songs[i])
		if err != nil || len(s.PrimaryLanguages) != want || s.Genre != genre {
			t.Fatalf("song %d = %+v %v", i, s, err)
		}
	}

	listener := testutil.User(t, pool, "user", true)
	staff := testutil.User(t, pool, "admin", true)
	n := 0
	listen := func(user, track, outcome string, capture, completion int, daysAgo int, source string) {
		t.Helper()
		n++
		exec(`INSERT INTO listening_events (user_id, event_id, type, at, track_id, outcome, capture, completion, listened_sec, duration_sec, source)
			VALUES ($1, $2, 'listen', now() - make_interval(days => $7), $3, $4, $5, $6, 100, 180, $8)`,
			user, "f"+strconv.Itoa(n), track, outcome, capture, completion, daysAgo, source)
	}
	listen(listener, songs[0], "completed", 2, 100, 0, "")
	listen(listener, songs[0], "completed", 2, 100, 0, "")
	listen(listener, songs[1], "skipped", 2, 10, 1, "")
	listen(listener, songs[1], "skipped", 0, 10, 1, "")
	listen(listener, songs[2], "completed", 2, 100, 400, "") // too old to count
	listen(staff, songs[0], "completed", 2, 100, 0, "")
	listen(owner, songs[0], "completed", 2, 100, 0, "") // the artist's own
	exec(`INSERT INTO listening_events (user_id, event_id, type, at, track_id) VALUES ($1, 'k1', 'like', now() - interval '2 days', $2), ($1, 'k2', 'unlike', now(), $2)`, listener, songs[3])
	exec(`INSERT INTO library_tracks (user_id, track_id) VALUES ($1, $2)`, listener, songs[4])
	var mine, auto string
	if err := pool.QueryRow(ctx, `INSERT INTO playlists (owner_id, name) VALUES ($1, 'Mine') RETURNING id`, listener).Scan(&mine); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO playlists (owner_id, name) VALUES ($1, 'Your sound') RETURNING id`, listener).Scan(&auto); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO playlist_items (playlist_id, track_id, position, added_by, by_server) VALUES
		($1, $3, 0, $5, false), ($1, $4, 1, $5, true), ($2, $4, 0, $5, false)`, mine, auto, songs[0], songs[5], listener)

	p, err := LoadProfile(ctx, pool, listener)
	if err != nil {
		t.Fatal(err)
	}
	a, b := p.History[songs[0]], p.History[songs[1]]
	if a.Completes != 2 || a.PlaylistAdds != 1 || b.Skips != 1 || b.WeakSkips != 1 || b.SkipsW <= 0.9 {
		t.Fatalf("history = %+v %+v", a, b)
	}
	if _, old := p.History[songs[2]]; old || !p.History[songs[3]].Unliked || !p.History[songs[4]].Saved || p.History[songs[5]].PlaylistAdds != 0 {
		t.Fatalf("history = %+v", p.History)
	}
	if q, err := LoadProfile(ctx, pool, owner); err != nil || len(q.History) != 0 {
		t.Fatalf("an artist's own listening is not their taste: %v %v", q.History, err)
	}

	// statistics: staff, test accounts and the artist are left out, and a repeat on the same day counts once
	if err := RefreshStats(ctx, pool); err != nil {
		t.Fatal(err)
	}
	s, err := Song(ctx, pool, songs[0])
	if err != nil || s.Stats.Listeners30d != 1 || s.Stats.Completes90d != 1 {
		t.Fatalf("stats = %+v %v", s.Stats, err)
	}
	exec(`UPDATE users SET signals_excluded_at = now() WHERE id = $1`, listener)
	if err := RefreshStats(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if s, _ := Song(ctx, pool, songs[0]); s.Stats.Listeners30d != 0 {
		t.Fatalf("a test account still counts: %+v", s.Stats)
	}
	exec(`UPDATE users SET signals_excluded_at = NULL WHERE id = $1`, listener)

	// a song with no artist is still one people can play, mix from and be offered
	lone, _ := testutil.ReadyTrack(t, pool, owner, art, rel, "Lone", 10, "public")
	exec(`UPDATE tracks SET artist_id = NULL, genre = $2 WHERE id = $1`, lone, genre)
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM tracks WHERE id = $1`, lone) }) //nolint:errcheck
	if l, err := Song(ctx, pool, lone); err != nil || l.ArtistID != "none:"+lone {
		t.Fatalf("no artist = %+v %v", l, err)
	}
	if from, err := Mix(ctx, pool, listener, lone, nil, 10, "x"); err != nil || len(from.Picks) == 0 {
		t.Fatalf("a mix from it = %v %v", from, err)
	}
	exec(`UPDATE tracks SET visibility = 'draft' WHERE id = $1`, lone)

	// a mix from the first song: the others of its genre, without what was taken back; recorded, and judged by what follows
	res, err := Mix(ctx, pool, listener, songs[0], []string{songs[5]}, 10, "x")
	if err != nil {
		t.Fatal(err)
	}
	got := ids(res.Picks)
	if slices.Contains(got, songs[0]) || slices.Contains(got, songs[3]) || slices.Contains(got, songs[5]) || !slices.Contains(got, songs[4]) || len(got) != 3 {
		t.Fatalf("mix = %v of %v", got, songs)
	}
	if mineOnly, err := Mix(ctx, pool, owner, songs[0], nil, 10, "x"); err != nil || len(mineOnly.Picks) != 0 {
		t.Fatalf("an artist is not offered their own songs: %v %v", mineOnly, err)
	}
	if err := Record(ctx, pool, listener, "mix", res.Mode, songs[0], res.Picks); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE rec_serves SET at = now() - interval '1 hour' WHERE user_id = $1`, listener)
	listen(listener, res.Picks[0].TrackID, "completed", 2, 100, 0, "mix")
	listen(listener, res.Picks[1].TrackID, "completed", 2, 100, 0, "search") // found some other way
	var served, completed, listened int
	if err := pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE outcome = 'completed'), count(outcome) FROM rec_outcomes WHERE user_id = $1`, listener).
		Scan(&served, &completed, &listened); err != nil || served != 3 || completed != 1 || listened != 1 {
		t.Fatalf("outcomes = %d %d %d %v", served, completed, listened, err)
	}
	var avoided []string
	rows, err := pool.Query(ctx, Avoided, listener)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		avoided = append(avoided, id)
	}
	if !slices.Equal(avoided, []string{songs[3]}) {
		t.Fatalf("avoided = %v, want the unliked song only", avoided)
	}
	exec(`DELETE FROM users WHERE id = $1`, listener)
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM rec_serves WHERE user_id = $1`, listener).Scan(&left); err != nil || left != 0 {
		t.Fatalf("suggestions go with the account: %d %v", left, err)
	}
}
