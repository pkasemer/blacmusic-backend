package podcast

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/db"
)

// Integration tests: need a Postgres, e.g.
//
//	TEST_DATABASE_URL=postgres://blac:blac@localhost:5432/blac_test?sslmode=disable go test ./internal/podcast

// feedServer serves a feed whose content tests can change.
type feedServer struct {
	srv    *httptest.Server
	mu     sync.Mutex
	body   string
	etag   string
	status int // 0 = normal
	hits   atomic.Int32
}

func newFeedServer(t *testing.T) *feedServer {
	t.Helper()
	fs := &feedServer{}
	fs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fs.hits.Add(1)
		fs.mu.Lock()
		body, etag, status := fs.body, fs.etag, fs.status
		fs.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		if etag != "" && r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if etag != "" {
			w.Header().Set("ETag", etag)
		}
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(fs.srv.Close)
	return fs
}

func (fs *feedServer) set(body, etag string, status int) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.body, fs.etag, fs.status = body, etag, status
}

func (fs *feedServer) url() string { return fs.srv.URL + "/feed.xml" }

// rss builds a feed with the given episode numbers (higher = newer).
func rss(title string, eps ...int) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<?xml version="1.0"?><rss version="2.0" xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd"><channel><title>%s</title><itunes:author>Host</itunes:author>`, title)
	for _, n := range eps {
		fmt.Fprintf(&b, `<item><title>Episode %d</title><guid>g%d</guid><pubDate>%s</pubDate>`+
			`<enclosure url="https://cdn.example.com/%d.mp3" length="1000" type="audio/mpeg"/><itunes:duration>%d</itunes:duration></item>`,
			n, n, time.Date(2024, 1, n, 9, 0, 0, 0, time.UTC).Format(time.RFC1123Z), n, 600)
	}
	b.WriteString(`</channel></rss>`)
	return b.String()
}

type env struct {
	t     *testing.T
	pool  *pgxpool.Pool
	store *Store
	feed  *feedServer
}

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
	e := &env{t: t, pool: pool, feed: newFeedServer(t)}
	e.store = NewStore(pool, NewFetcher(true, 5*time.Second), time.Hour)
	return e
}

func (e *env) user() string {
	e.t.Helper()
	var id string
	if err := e.pool.QueryRow(context.Background(),
		`INSERT INTO users(email, password_hash, display_name) VALUES ($1,'x','P') RETURNING id::text`,
		fmt.Sprintf("pod-%d@test.local", time.Now().UnixNano())).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { e.pool.Exec(context.Background(), `DELETE FROM users WHERE id::text = $1`, id) }) //nolint:errcheck
	return id
}

// addShow serves body, adds the feed for uid and registers cleanup.
func (e *env) addShow(uid, body string) *Show {
	e.t.Helper()
	e.feed.set(body, "", 0)
	// A unique URL per call: the same URL would resolve to the show added earlier.
	url := fmt.Sprintf("%s/show-%d.xml", e.feed.srv.URL, time.Now().UnixNano())
	show, _, err := e.store.AddFeed(context.Background(), uid, url)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { e.pool.Exec(context.Background(), `DELETE FROM shows WHERE id = $1`, show.ID) }) //nolint:errcheck
	return show
}

func (e *env) episodeIDs(showID string) []string {
	e.t.Helper()
	eps, err := e.store.ListEpisodes(context.Background(), showID, e.user(), false, 500, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	ids := make([]string, len(eps))
	for i, ep := range eps {
		ids[i] = ep.ID
	}
	return ids
}

func TestAddFeedCreatesShowEpisodesAndSubscription(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid := e.user()
	e.feed.set(rss("Alpha Show", 1, 2, 3), "", 0)

	show, created, err := e.store.AddFeed(ctx, uid, e.feed.url())
	if err != nil || !created {
		t.Fatalf("AddFeed: created=%v err=%v", created, err)
	}
	t.Cleanup(func() { e.pool.Exec(ctx, `DELETE FROM shows WHERE id = $1`, show.ID) }) //nolint:errcheck
	if !strings.HasPrefix(show.ID, "shw_") || show.Title != "Alpha Show" || show.Author != "Host" ||
		!show.Subscribed || show.EpisodeCount != 3 || show.LatestEpisode == nil {
		t.Fatalf("show: %+v", show)
	}

	eps, err := e.store.ListEpisodes(ctx, show.ID, uid, false, 50, 0)
	if err != nil || len(eps) != 3 {
		t.Fatalf("episodes: %d %v", len(eps), err)
	}
	if eps[0].Title != "Episode 3" || eps[2].Title != "Episode 1" || !strings.HasPrefix(eps[0].ID, "epi_") || eps[0].DurationMs != 600000 {
		t.Fatalf("newest-first listing wrong: %+v", eps[0])
	}
	oldest, _ := e.store.ListEpisodes(ctx, show.ID, uid, true, 1, 0)
	if oldest[0].Title != "Episode 1" {
		t.Fatalf("oldest-first = %s", oldest[0].Title)
	}
	page2, _ := e.store.ListEpisodes(ctx, show.ID, uid, false, 2, 2)
	if len(page2) != 1 || page2[0].Title != "Episode 1" {
		t.Fatalf("pagination: %+v", page2)
	}

	// Adding the same feed again (even by another user) reuses the show.
	other := e.user()
	again, created, err := e.store.AddFeed(ctx, other, e.feed.url())
	if err != nil || created || again.ID != show.ID || !again.Subscribed {
		t.Fatalf("second add: created=%v id=%s err=%v", created, again.ID, err)
	}
	if n := len(e.episodeIDs(show.ID)); n != 3 {
		t.Fatalf("episodes duplicated: %d", n)
	}
}

func TestAddFeedRejectsBadFeedsWithoutSideEffects(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid := e.user()

	e.feed.set(`<html><body>not a feed</body></html>`, "", 0)
	_, _, err := e.store.AddFeed(ctx, uid, e.feed.url())
	var fe *FeedError
	if !errors.As(err, &fe) {
		t.Fatalf("html: err = %v, want FeedError", err)
	}
	e.feed.set("", "", 500)
	if _, _, err = e.store.AddFeed(ctx, uid, e.feed.url()); !errors.As(err, &fe) {
		t.Fatalf("500: err = %v, want FeedError", err)
	}
	if _, _, err = e.store.AddFeed(ctx, uid, "ftp://example.com/feed"); !errors.As(err, &fe) {
		t.Fatalf("ftp: err = %v, want FeedError", err)
	}
	var n int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM shows WHERE feed_url = $1`, e.feed.url()).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a rejected feed must not create a show (n=%d err=%v)", n, err)
	}

	// The default fetcher (guard on) refuses a loopback feed even though it is valid.
	strict := NewStore(e.pool, NewFetcher(false, 5*time.Second), time.Hour)
	e.feed.set(rss("Valid", 1), "", 0)
	if _, _, err = strict.AddFeed(ctx, uid, e.feed.url()); !errors.As(err, &fe) {
		t.Fatalf("guarded fetcher: err = %v, want FeedError", err)
	}
}

func TestRefreshAddsNewEpisodesAndKeepsExistingOnes(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid := e.user()
	show := e.addShow(uid, rss("Beta", 1, 2))
	before := e.episodeIDs(show.ID)

	// A listener has progress on episode 2.
	var ep2 string
	if err := e.pool.QueryRow(ctx, `SELECT id FROM episodes WHERE show_id=$1 AND guid='g2'`, show.ID).Scan(&ep2); err != nil {
		t.Fatal(err)
	}
	if err := e.store.SetProgress(ctx, uid, ep2, ProgressUpdate{PositionMs: 123000}); err != nil {
		t.Fatal(err)
	}

	e.feed.set(rss("Beta Renamed", 1, 2, 3), "", 0)
	if err := e.store.Refresh(ctx, show.ID); err != nil {
		t.Fatal(err)
	}
	after := e.episodeIDs(show.ID)
	if len(after) != 3 {
		t.Fatalf("want 3 episodes after refresh, got %d", len(after))
	}
	for _, id := range before {
		found := false
		for _, a := range after {
			found = found || a == id
		}
		if !found {
			t.Fatalf("episode %s changed identity across a refresh", id)
		}
	}
	got, _ := e.store.GetShow(ctx, show.ID, uid)
	if got.Title != "Beta Renamed" {
		t.Fatalf("show metadata not refreshed: %q", got.Title)
	}
	ep, _ := e.store.GetEpisode(ctx, ep2, uid)
	if ep.Progress == nil || ep.Progress.PositionMs != 123000 {
		t.Fatalf("progress lost on refresh: %+v", ep.Progress)
	}
}

func TestRefreshUsesConditionalGetAndBacksOffOnFailure(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid := e.user()
	e.feed.set(rss("Gamma", 1), `"v1"`, 0)
	show, _, err := e.store.AddFeed(ctx, uid, e.feed.url())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.pool.Exec(ctx, `DELETE FROM shows WHERE id = $1`, show.ID) }) //nolint:errcheck

	// Unchanged feed: answered 304, nothing re-parsed, bookkeeping reset.
	if err := e.store.Refresh(ctx, show.ID); err != nil {
		t.Fatal(err)
	}
	var fails int
	var next time.Time
	var lastErr string
	read := func() {
		if err := e.pool.QueryRow(ctx, `SELECT fail_count, next_fetch_at, fetch_error FROM shows WHERE id=$1`, show.ID).
			Scan(&fails, &next, &lastErr); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if fails != 0 || lastErr != "" || time.Until(next) < 50*time.Minute {
		t.Fatalf("after 304: fails=%d err=%q next in %v", fails, lastErr, time.Until(next))
	}

	// The server breaks: failures are recorded and retried with growing delays.
	e.feed.set("", "", 503)
	if err := e.store.Refresh(ctx, show.ID); err != nil {
		t.Fatalf("a feed failure is recorded, not returned: %v", err)
	}
	read()
	if fails != 1 || !strings.Contains(lastErr, "503") || time.Until(next) < 90*time.Minute {
		t.Fatalf("after 1st failure: fails=%d err=%q next in %v", fails, lastErr, time.Until(next))
	}
	if err := e.store.Refresh(ctx, show.ID); err != nil {
		t.Fatal(err)
	}
	prev := time.Until(next)
	read()
	if fails != 2 || time.Until(next) <= prev {
		t.Fatalf("backoff must grow: fails=%d next in %v (was %v)", fails, time.Until(next), prev)
	}
	// The show keeps working for listeners meanwhile.
	if n := len(e.episodeIDs(show.ID)); n != 1 {
		t.Fatalf("episodes must survive a failed refresh, got %d", n)
	}

	// Recovery clears the error.
	e.feed.set(rss("Gamma", 1, 2), `"v2"`, 0)
	if err := e.store.Refresh(ctx, show.ID); err != nil {
		t.Fatal(err)
	}
	read()
	if fails != 0 || lastErr != "" || len(e.episodeIDs(show.ID)) != 2 {
		t.Fatalf("after recovery: fails=%d err=%q", fails, lastErr)
	}
}

func TestClaimDueOnlySubscribedShowsAndLeases(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid := e.user()
	show := e.addShow(uid, rss("Delta", 1))

	if ids, err := e.store.ClaimDue(ctx, 50); err != nil || contains(ids, show.ID) {
		t.Fatalf("a freshly fetched show is not due yet: %v %v", ids, err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE shows SET next_fetch_at = now() - interval '1 minute' WHERE id=$1`, show.ID); err != nil {
		t.Fatal(err)
	}
	ids, err := e.store.ClaimDue(ctx, 50)
	if err != nil || !contains(ids, show.ID) {
		t.Fatalf("due subscribed show not claimed: %v %v", ids, err)
	}
	if ids, _ = e.store.ClaimDue(ctx, 50); contains(ids, show.ID) {
		t.Fatal("a claimed show must be leased, not handed out twice")
	}

	// With no subscribers a show is left alone.
	if err := e.store.Unsubscribe(ctx, uid, show.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE shows SET next_fetch_at = now() - interval '1 minute' WHERE id=$1`, show.ID); err != nil {
		t.Fatal(err)
	}
	if ids, _ = e.store.ClaimDue(ctx, 50); contains(ids, show.ID) {
		t.Fatal("shows nobody follows must not be refreshed")
	}
	// Following it again makes a stale show due immediately.
	if _, err := e.pool.Exec(ctx, `UPDATE shows SET last_fetched_at = now() - interval '3 hours', next_fetch_at = now() + interval '1 hour' WHERE id=$1`, show.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Subscribe(ctx, uid, show.ID); err != nil {
		t.Fatal(err)
	}
	if ids, _ = e.store.ClaimDue(ctx, 50); !contains(ids, show.ID) {
		t.Fatal("subscribing to a stale show must queue a refresh")
	}
}

func contains(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func TestSubscriptions(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid, other := e.user(), e.user()
	e.feed.set(rss("Epsilon", 1, 2, 3), "", 0)
	show := e.addShow(uid, rss("Epsilon", 1, 2, 3))

	if err := e.store.Subscribe(ctx, other, "shw_nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("subscribe unknown = %v, want ErrNotFound", err)
	}
	if err := e.store.Subscribe(ctx, other, show.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Subscribe(ctx, other, show.ID); err != nil { // idempotent
		t.Fatal(err)
	}

	list, err := e.store.ListSubscriptions(ctx, other, 50, 0)
	if err != nil || len(list) != 1 || list[0].ID != show.ID {
		t.Fatalf("list: %v %v", list, err)
	}
	if list[0].UnplayedCount == nil || *list[0].UnplayedCount != 3 || list[0].SubscribedAt == nil {
		t.Fatalf("unplayed/subscribedAt: %+v", list[0])
	}

	// Finishing an episode lowers the unplayed count.
	ids := e.episodeIDs(show.ID)
	if err := e.store.SetProgress(ctx, other, ids[0], ProgressUpdate{PositionMs: 0, Completed: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	list, _ = e.store.ListSubscriptions(ctx, other, 50, 0)
	if *list[0].UnplayedCount != 2 {
		t.Fatalf("unplayed = %d, want 2", *list[0].UnplayedCount)
	}

	if err := e.store.Unsubscribe(ctx, other, show.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Unsubscribe(ctx, other, show.ID); err != nil { // idempotent
		t.Fatal(err)
	}
	if list, _ = e.store.ListSubscriptions(ctx, other, 50, 0); len(list) != 0 {
		t.Fatalf("still subscribed: %v", list)
	}
	got, _ := e.store.GetShow(ctx, show.ID, other)
	if got.Subscribed {
		t.Fatal("subscribed flag must be per user")
	}

	found, err := e.store.SearchShows(ctx, other, "epsil", 10, 0)
	if err != nil || len(found) != 1 || found[0].ID != show.ID {
		t.Fatalf("search: %v %v", found, err)
	}
	if found, _ = e.store.SearchShows(ctx, other, "100%_", 10, 0); len(found) != 0 {
		t.Fatal("LIKE wildcards in the query must be escaped")
	}
}

func ptr[T any](v T) *T { return &v }

func TestProgressRules(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid := e.user()
	show := e.addShow(uid, rss("Zeta", 1)) // the episode is 600 s long
	ep := e.episodeIDs(show.ID)[0]

	state := func() *Progress {
		v, err := e.store.GetEpisode(ctx, ep, uid)
		if err != nil {
			t.Fatal(err)
		}
		return v.Progress
	}
	if state() != nil {
		t.Fatal("no progress before listening")
	}

	set := func(u ProgressUpdate) {
		if err := e.store.SetProgress(ctx, uid, ep, u); err != nil {
			t.Fatal(err)
		}
	}
	set(ProgressUpdate{PositionMs: 90_000})
	if p := state(); p.PositionMs != 90_000 || p.Completed {
		t.Fatalf("midway: %+v", p)
	}
	set(ProgressUpdate{PositionMs: 585_000}) // within 20 s of the end
	if p := state(); !p.Completed {
		t.Fatalf("near the end must count as played: %+v", p)
	}
	set(ProgressUpdate{PositionMs: 10_000}) // started again
	if p := state(); p.Completed {
		t.Fatalf("restarting must clear the played flag: %+v", p)
	}
	set(ProgressUpdate{PositionMs: 10_000, Completed: ptr(true)}) // "mark as played"
	if p := state(); !p.Completed || p.PositionMs != 10_000 {
		t.Fatalf("explicit played: %+v", p)
	}
	set(ProgressUpdate{PositionMs: 10_000, Completed: ptr(false)}) // "mark as unplayed"
	if p := state(); p.Completed {
		t.Fatalf("explicit unplayed: %+v", p)
	}

	// Unknown episode.
	if err := e.store.SetProgress(ctx, uid, "epi_nope", ProgressUpdate{PositionMs: 1}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown episode = %v, want ErrNotFound", err)
	}

	// Continue-listening list: started and unfinished only, latest first.
	show2 := e.addShow(uid, rss("Eta", 1, 2))
	ids2 := e.episodeIDs(show2.ID)
	set2 := func(id string, pos int, done *bool) {
		if err := e.store.SetProgress(ctx, uid, id, ProgressUpdate{PositionMs: pos, Completed: done}); err != nil {
			t.Fatal(err)
		}
	}
	set2(ids2[0], 30_000, nil)
	set2(ids2[1], 40_000, ptr(true)) // finished: excluded
	set(ProgressUpdate{PositionMs: 50_000})
	in, err := e.store.InProgress(ctx, uid, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(in) != 2 || in[0].ID != ep || in[1].ID != ids2[0] {
		t.Fatalf("in-progress = %v", titles(in))
	}
	if in[0].ShowTitle != "Zeta" {
		t.Fatalf("show title missing: %+v", in[0])
	}
}

func titles(eps []*EpisodeView) []string {
	out := make([]string, len(eps))
	for i, e := range eps {
		out[i] = e.ShowTitle + "/" + e.Title
	}
	return out
}

func TestUnknownShowAndEpisode(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid := e.user()
	if _, err := e.store.GetShow(ctx, "shw_nope", uid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetShow = %v", err)
	}
	if _, err := e.store.GetEpisode(ctx, "epi_nope", uid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetEpisode = %v", err)
	}
	if _, err := e.store.ListEpisodes(ctx, "shw_nope", uid, false, 10, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ListEpisodes = %v", err)
	}
}

func TestRefresherRunsDueShows(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid := e.user()
	show := e.addShow(uid, rss("Theta", 1))
	e.feed.set(rss("Theta", 1, 2), "", 0)
	if _, err := e.pool.Exec(ctx, `UPDATE shows SET next_fetch_at = now() - interval '1 minute' WHERE id=$1`, show.ID); err != nil {
		t.Fatal(err)
	}

	r := NewRefresher(e.store, 2)
	// Other tests' shows may also be due in a shared database; run until ours is done.
	for i := 0; i < 5 && len(e.episodeIDs(show.ID)) != 2; i++ {
		r.runOnce(ctx)
	}
	if n := len(e.episodeIDs(show.ID)); n != 2 {
		t.Fatalf("refresher did not pick up the new episode: %d", n)
	}
}
