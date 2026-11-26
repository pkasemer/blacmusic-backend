package stats

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/testutil"
)

func TestCounts(t *testing.T) {
	for _, c := range []struct {
		listened, duration int
		want               bool
	}{
		{29, 180, false}, {30, 180, true}, {200, 180, true},
		{19, 40, false}, {20, 40, true}, // a short track counts after half of it
		{29, 0, false}, {30, 0, true}, // unknown length: the plain rule
		{0, 10, false},
	} {
		if got := Counts(c.listened, c.duration); got != c.want {
			t.Errorf("Counts(%d, %d) = %v, want %v", c.listened, c.duration, got, c.want)
		}
	}
}

type fixture struct {
	t        *testing.T
	pool     *pgxpool.Pool
	store    *Store
	owner    string // the artist's user
	artist   string
	release  string
	track    string // 180 s, public
	short    string // 40 s, public
	draft    string
	listener string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	pool := testutil.Pool(t)
	f := &fixture{t: t, pool: pool, store: NewStore(pool)}
	f.owner = testutil.User(t, pool, "user", true)
	f.artist = testutil.Artist(t, pool, f.owner, testutil.Unique("Stat"), testutil.GiB)
	f.release = testutil.Release(t, pool, f.artist, "Numbers", "album", "public")
	f.track, _ = testutil.ReadyTrack(t, pool, f.owner, f.artist, f.release, "Long one", 1, "public")
	f.short, _ = testutil.ReadyTrack(t, pool, f.owner, f.artist, f.release, "Short one", 1, "public")
	pool.Exec(context.Background(), `UPDATE tracks SET duration_ms = 40000 WHERE id = $1`, f.short) //nolint:errcheck
	draftRel := testutil.Release(t, pool, f.artist, "Hidden", "single", "draft")
	f.draft, _ = testutil.ReadyTrack(t, pool, f.owner, f.artist, draftRel, "Draft", 1, "draft")
	f.listener = testutil.User(t, pool, "user", true)
	return f
}

// play inserts a counted play at the given time.
func (f *fixture) play(user, track, source string, listened int, completed bool, at time.Time) {
	f.t.Helper()
	_, err := f.pool.Exec(context.Background(), `
		INSERT INTO plays (user_id, track_id, release_id, artist_id, listened_sec, completed, source, at)
		SELECT NULLIF($1, '')::uuid, t.id, t.album_id, t.artist_id, $3, $4, $5, $6 FROM tracks t WHERE t.id = $2`,
		user, track, listened, completed, source, at)
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) plays(track string) int {
	f.t.Helper()
	var n int
	f.pool.QueryRow(context.Background(), `SELECT count(*) FROM plays WHERE track_id = $1`, track).Scan(&n) //nolint:errcheck
	return n
}

func TestRecordRules(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	// too short: accepted, not counted
	if ok, err := f.store.Record(ctx, f.listener, PlayInput{TrackID: f.track, ListenedSec: 12, Source: "search"}); err != nil || ok {
		t.Fatalf("12 s = %v %v", ok, err)
	}
	// counted, with where it came from and whether it was finished
	if ok, err := f.store.Record(ctx, f.listener, PlayInput{TrackID: f.track, ListenedSec: 175, Source: "album"}); err != nil || !ok {
		t.Fatalf("175 s = %v %v", ok, err)
	}
	var source string
	var done bool
	f.pool.QueryRow(ctx, `SELECT source, completed FROM plays WHERE track_id = $1`, f.track).Scan(&source, &done) //nolint:errcheck
	if source != "album" || !done {
		t.Fatalf("stored source %q completed %v", source, done)
	}
	// the same listener again within 30 s is the same play
	if ok, _ := f.store.Record(ctx, f.listener, PlayInput{TrackID: f.track, ListenedSec: 60}); ok {
		t.Fatal("a repeat within 30 s was counted")
	}
	f.pool.Exec(ctx, `UPDATE plays SET at = now() - interval '2 minutes' WHERE track_id = $1`, f.track) //nolint:errcheck
	if ok, _ := f.store.Record(ctx, f.listener, PlayInput{TrackID: f.track, ListenedSec: 60, Source: "bogus"}); !ok {
		t.Fatal("a later repeat should count")
	}
	f.pool.QueryRow(ctx, `SELECT source FROM plays WHERE track_id = $1 ORDER BY id DESC LIMIT 1`, f.track).Scan(&source) //nolint:errcheck
	if source != "other" {
		t.Fatalf("an unknown source is stored as %q, want other", source)
	}
	// a short track counts after half of it
	if ok, _ := f.store.Record(ctx, f.listener, PlayInput{TrackID: f.short, ListenedSec: 20}); !ok {
		t.Fatal("half of a 40 s track should count")
	}
	// the artist playing their own music is not an audience
	if ok, _ := f.store.Record(ctx, f.owner, PlayInput{TrackID: f.track, ListenedSec: 100}); ok {
		t.Fatal("the artist's own play was counted")
	}
	// nothing for drafts or unknown tracks; bad input
	if _, err := f.store.Record(ctx, f.listener, PlayInput{TrackID: f.draft, ListenedSec: 100}); err != ErrTrackNotFound {
		t.Fatalf("draft = %v", err)
	}
	if _, err := f.store.Record(ctx, f.listener, PlayInput{TrackID: "trk_nope", ListenedSec: 100}); err != ErrTrackNotFound {
		t.Fatalf("unknown = %v", err)
	}
	for _, in := range []PlayInput{{ListenedSec: 40}, {TrackID: f.track, ListenedSec: -1}, {TrackID: f.track, ListenedSec: MaxListenSec + 1}} {
		if _, err := f.store.Record(ctx, f.listener, in); err == nil {
			t.Errorf("%+v accepted", in)
		}
	}
	// deleting the account keeps the count but forgets who it was
	if _, err := f.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, f.listener); err != nil {
		t.Fatal(err)
	}
	var withUser int
	f.pool.QueryRow(ctx, `SELECT count(*) FROM plays WHERE track_id = $1 AND user_id IS NOT NULL`, f.track).Scan(&withUser) //nolint:errcheck
	if f.plays(f.track) != 2 || withUser != 0 {
		t.Fatalf("after deleting the account: %d plays, %d with a user", f.plays(f.track), withUser)
	}
}

func TestArtistStats(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	now := time.Now().UTC()
	u1, u2, u3 := testutil.User(t, f.pool, "user", true), testutil.User(t, f.pool, "user", true), testutil.User(t, f.pool, "user", true)
	ago := func(d int) time.Time { return now.AddDate(0, 0, -d).Add(-time.Minute) }

	f.play(u1, f.track, "search", 100, false, ago(1))
	f.play(u1, f.track, "search", 180, true, ago(2))
	f.play(u2, f.track, "album", 60, false, ago(2))
	f.play(u2, f.short, "playlist", 40, true, ago(10))
	f.play(u3, f.track, "library", 90, false, ago(40)) // last month: outside 28d, in the previous 30 days
	f.play(u3, f.short, "library", 25, false, ago(100))
	f.play("", f.track, "other", 70, false, ago(3)) // an account that was deleted: counts as a play, not as a listener
	// saves, followers, playlist adds
	f.pool.Exec(ctx, `INSERT INTO library_tracks (user_id, track_id) VALUES ($1, $2), ($3, $2)`, u1, f.track, u2)                            //nolint:errcheck
	f.pool.Exec(ctx, `INSERT INTO library_artists (user_id, artist_id) VALUES ($1, $2)`, u1, f.artist)                                       //nolint:errcheck
	f.pool.Exec(ctx, `INSERT INTO library_artists (user_id, artist_id, added_at) VALUES ($1, $2, now() - interval '60 days')`, u2, f.artist) //nolint:errcheck
	var pl string
	f.pool.QueryRow(ctx, `INSERT INTO playlists (owner_id, name) VALUES ($1, 'mix') RETURNING id`, u1).Scan(&pl)     //nolint:errcheck
	f.pool.Exec(ctx, `INSERT INTO playlist_items (playlist_id, track_id, position) VALUES ($1, $2, 0)`, pl, f.track) //nolint:errcheck

	s, err := f.store.Artist(ctx, f.owner, "28d", now)
	if err != nil {
		t.Fatal(err)
	}
	tot := s.Totals
	if tot.Plays != 5 || tot.Listeners != 2 {
		t.Fatalf("plays/listeners in 28d = %d/%d, want 5/2", tot.Plays, tot.Listeners)
	}
	if tot.MonthlyListeners != 2 || tot.MonthlyListenersChange != 1 { // u1, u2 now; u3 the month before
		t.Fatalf("monthly listeners %d change %d, want 2 and +1", tot.MonthlyListeners, tot.MonthlyListenersChange)
	}
	if tot.Followers != 2 || tot.FollowersChange != 1 || tot.Likes != 2 || tot.LikesChange != 2 || tot.PlaylistAdds != 1 || tot.Downloads != 0 {
		t.Fatalf("totals = %+v", tot)
	}
	if len(s.Series) != 28 || s.Series[len(s.Series)-1].Date != now.Format("2006-01-02") || s.Range != "28d" {
		t.Fatalf("series has %d points ending %s", len(s.Series), s.Series[len(s.Series)-1].Date)
	}
	sum := 0
	for _, p := range s.Series {
		sum += p.Plays
	}
	if sum != 5 {
		t.Fatalf("series adds up to %d plays, want 5", sum)
	}
	if len(s.TopTracks) != 2 || s.TopTracks[0].TrackID != f.track || s.TopTracks[0].Plays != 4 || s.TopTracks[0].Listeners != 2 {
		t.Fatalf("top tracks = %+v", s.TopTracks)
	}
	if len(s.Releases) < 2 || s.Releases[0].ReleaseID != f.release || s.Releases[0].Plays != 5 || s.Releases[0].Kind != "ep" {
		t.Fatalf("releases = %+v", s.Releases)
	}

	// other ranges
	if w, _ := f.store.Artist(ctx, f.owner, "7d", now); len(w.Series) != 7 || w.Totals.Plays != 4 {
		t.Fatalf("7d: %d points, %d plays, want 7 and 4", len(w.Series), w.Totals.Plays)
	}
	// "all" starts when the artist did (the test artist is new, so make it older than the oldest play)
	f.pool.Exec(ctx, `UPDATE artists SET created_at = now() - interval '200 days' WHERE id = $1`, f.artist) //nolint:errcheck
	if a, _ := f.store.Artist(ctx, f.owner, "all", now); a.Totals.Plays != 7 || a.Totals.Listeners != 3 {
		t.Fatalf("all: %+v", a.Totals)
	}
	if _, err := f.store.Artist(ctx, f.owner, "1y", now); err == nil {
		t.Fatal("an unknown range was accepted")
	}
	if _, err := f.store.Artist(ctx, f.listener, "28d", now); err != ErrNoArtist {
		t.Fatalf("a non-artist = %v", err)
	}
}

func TestReleaseStats(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	now := time.Now().UTC()
	u1, u2 := testutil.User(t, f.pool, "user", true), testutil.User(t, f.pool, "user", true)
	f.play(u1, f.track, "search", 180, true, now.Add(-time.Hour))
	f.play(u2, f.track, "search", 60, false, now.Add(-2*time.Hour))
	f.play(u1, f.short, "playlist", 30, false, now.Add(-3*time.Hour))
	f.pool.Exec(ctx, `INSERT INTO library_albums (user_id, album_id) VALUES ($1, $2)`, u1, f.release) //nolint:errcheck
	f.pool.Exec(ctx, `INSERT INTO library_tracks (user_id, track_id) VALUES ($1, $2)`, u2, f.short)   //nolint:errcheck

	s, err := f.store.Release(ctx, f.owner, f.release, "7d", now)
	if err != nil {
		t.Fatal(err)
	}
	tot := s.Totals
	if tot.Plays != 3 || tot.Listeners != 2 || tot.Likes != 2 || tot.AvgListenSec != 90 || tot.CompletionRate != 0.333 {
		t.Fatalf("totals = %+v (want 3 plays, 2 listeners, 2 likes, 90 s, 0.333)", tot)
	}
	if len(s.Series) != 7 {
		t.Fatalf("series = %d points", len(s.Series))
	}
	if len(s.Tracks) != 2 {
		t.Fatalf("tracks = %+v", s.Tracks)
	}
	byID := map[string]TrackRow{}
	for _, x := range s.Tracks {
		byID[x.TrackID] = x
	}
	if x := byID[f.track]; x.Plays != 2 || x.Listeners != 2 || x.CompletionRate != 0.5 || x.Likes != 0 {
		t.Fatalf("long track = %+v", x)
	}
	if x := byID[f.short]; x.Plays != 1 || x.Likes != 1 || x.CompletionRate != 0 {
		t.Fatalf("short track = %+v", x)
	}
	if len(s.Sources) != 2 || s.Sources[0].Source != "search" || s.Sources[0].Plays != 2 || s.Sources[1].Source != "playlist" {
		t.Fatalf("sources = %+v", s.Sources)
	}
	// only the owner may look
	if _, err := f.store.Release(ctx, f.listener, f.release, "7d", now); err != ErrNotFound {
		t.Fatalf("a stranger = %v, want ErrNotFound", err)
	}
	if _, err := f.store.Release(ctx, f.owner, "alb_nope", "7d", now); err != ErrNotFound {
		t.Fatalf("unknown release = %v", err)
	}
}

func TestStatsOverHTTP(t *testing.T) {
	f := setup(t)
	r := chi.NewRouter()
	r.Group(func(g chi.Router) {
		g.Use(auth.Require(auth.NewTokenService(testutil.Secret, time.Minute)))
		NewHandler(f.store).Routes(g, func(next http.Handler) http.Handler { return next })
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

	if code, b := call("POST", "/me/plays", f.listener, map[string]any{"trackId": f.track, "listenedSec": 45, "source": "search"}); code != 204 {
		t.Fatalf("play = %d %v", code, b)
	}
	if code, _ := call("POST", "/me/plays", f.listener, map[string]any{"trackId": f.track, "listenedSec": 3}); code != 204 {
		t.Fatalf("a too short listen = %d, want 204 (accepted, not counted)", code)
	}
	if code, _ := call("POST", "/me/plays", f.listener, map[string]any{"trackId": "trk_nope", "listenedSec": 45}); code != 404 {
		t.Fatalf("unknown track = %d", code)
	}
	if code, _ := call("POST", "/me/plays", f.listener, map[string]any{"trackId": f.track}); code != 422 && code != 204 {
		t.Fatalf("missing listenedSec = %d", code)
	}
	if code, _ := call("POST", "/me/plays", f.listener, map[string]any{"trackId": f.track, "listenedSec": -5}); code != 422 {
		t.Fatalf("negative = %d, want 422", code)
	}
	if code, _ := call("POST", "/me/plays", f.listener, map[string]any{"track": f.track}); code != 400 {
		t.Fatalf("unknown field = %d, want 400", code)
	}

	code, b := call("GET", "/me/artist/stats?range=7d", f.owner, nil)
	tot, _ := b["totals"].(map[string]any)
	if code != 200 || b["range"] != "7d" || tot["plays"] != float64(1) || len(b["series"].([]any)) != 7 {
		t.Fatalf("artist stats = %d %v", code, b)
	}
	for _, k := range []string{"from", "to", "topTracks", "releases"} {
		if _, ok := b[k]; !ok {
			t.Fatalf("artist stats lacks %q", k)
		}
	}
	if code, _ = call("GET", "/me/artist/stats?range=5y", f.owner, nil); code != 422 {
		t.Fatalf("bad range = %d", code)
	}
	if code, b = call("GET", "/me/artist/stats", f.listener, nil); code != 403 {
		t.Fatalf("non-artist = %d %v, want 403", code, b)
	}
	code, b = call("GET", "/releases/"+f.release+"/stats", f.owner, nil)
	if tot, _ = b["totals"].(map[string]any); code != 200 || b["range"] != "28d" || tot["plays"] != float64(1) || b["sources"] == nil {
		t.Fatalf("release stats = %d %v", code, b)
	}
	if code, _ = call("GET", "/releases/"+f.release+"/stats", f.listener, nil); code != 404 {
		t.Fatalf("release stats of someone else = %d, want 404", code)
	}
}
