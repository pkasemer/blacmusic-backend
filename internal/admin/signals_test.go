package admin

import (
	"context"
	"testing"

	"github.com/blacmusic/backend/internal/testutil"
)

func TestStaffAndTestAccountsAreLeftOutOfSharedSignals(t *testing.T) {
	f := setupFull(t)
	pool, ctx := f.h.pool, context.Background()
	owner := testutil.User(t, pool, "user", true)
	art := testutil.Artist(t, pool, owner, testutil.Unique("Signals"), testutil.GiB)
	rel := testutil.Release(t, pool, art, "R", "single", "public")
	song, _ := testutil.ReadyTrack(t, pool, owner, art, rel, "S", 1, "public")
	tester := testutil.User(t, pool, "user", true)
	listener := testutil.User(t, pool, "user", true)
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM plays WHERE artist_id = $1`, art) }) //nolint:errcheck

	for _, u := range []string{tester, listener, f.admin, owner} {
		for _, q := range []string{
			`INSERT INTO plays (user_id, track_id, artist_id, release_id, listened_sec) SELECT $1, id, artist_id, album_id, 40 FROM tracks WHERE id = $2`,
			`INSERT INTO library_tracks (user_id, track_id) VALUES ($1, $2)`,
			`INSERT INTO listening_events (user_id, event_id, type, at, track_id) VALUES ($1, 'one', 'listen', now(), $2)`,
		} {
			if _, err := pool.Exec(ctx, q, u, song); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := pool.Exec(ctx, `INSERT INTO library_artists (user_id, artist_id) VALUES ($1, $2)`, u, art); err != nil {
			t.Fatal(err)
		}
	}
	counts := func() (plays, saves, follows, listens int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM signal_plays WHERE track_id = $1), (SELECT count(*) FROM signal_saves WHERE track_id = $1),
			(SELECT count(*) FROM signal_follows WHERE artist_id = $2), (SELECT count(*) FROM signal_listens WHERE track_id = $1)`,
			song, art).Scan(&plays, &saves, &follows, &listens); err != nil {
			t.Fatal(err)
		}
		return
	}
	// the administrator is out from the start; the artist's own listening is out of signal_listens only
	if p, s, fo, l := counts(); p != 3 || s != 3 || fo != 3 || l != 2 {
		t.Fatalf("before = %d %d %d %d", p, s, fo, l)
	}

	set := func(user string, excluded bool) (int, string) {
		code, b := call(f.r, "PUT", "/admin/users/"+user+"/signals", f.adminT, map[string]any{"excluded": excluded})
		return code, errCode(b)
	}
	for range 2 { // asking twice is fine and is logged once
		if code, e := set(tester, true); code != 204 {
			t.Fatalf("exclude = %d %s", code, e)
		}
	}
	if p, s, fo, l := counts(); p != 2 || s != 2 || fo != 2 || l != 1 {
		t.Fatalf("excluded = %d %d %d %d", p, s, fo, l)
	}
	if n := f.audit("user.signals_exclude", tester); n != 1 {
		t.Fatalf("audit = %d", n)
	}
	if _, b := call(f.r, "GET", "/admin/users/"+tester, f.adminT, nil); b["signalsExcluded"] != true {
		t.Fatalf("user = %v", b["signalsExcluded"])
	}
	if _, b := call(f.r, "GET", "/admin/users/"+listener, f.adminT, nil); b["signalsExcluded"] != false {
		t.Fatalf("listener = %v", b["signalsExcluded"])
	}
	if code, _ := set(tester, false); code != 204 || f.audit("user.signals_include", tester) != 1 {
		t.Fatalf("include = %d", code)
	}
	if p, _, _, _ := counts(); p != 3 {
		t.Fatalf("included again = %d", p)
	}
	if code, e := set(f.admin, false); code != 409 || e != "is_admin" {
		t.Fatalf("admin include = %d %s", code, e)
	}
	if code, _ := call(f.r, "PUT", "/admin/users/"+tester+"/signals", f.adminT, map[string]any{}); code != 400 {
		t.Fatalf("nothing sent = %d", code)
	}
}
