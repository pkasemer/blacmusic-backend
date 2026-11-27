package admin

import (
	"context"
	"slices"
	"strconv"
	"testing"

	"github.com/blacmusic/backend/internal/listening"
	"github.com/blacmusic/backend/internal/recommend"
	"github.com/blacmusic/backend/internal/testutil"
)

func TestBlockingASongOrArtistFromSuggestions(t *testing.T) {
	f := setupFull(t)
	pool, ctx := f.h.pool, context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	genre := testutil.Unique("blockable")
	// two artists with three songs each, one genre, so that only they can be in a mix of it
	var arts, songs []string
	for range 2 {
		owner := testutil.User(t, pool, "user", true)
		art := testutil.Artist(t, pool, owner, testutil.Unique("Blockable"), testutil.GiB)
		rel := testutil.Release(t, pool, art, "R", "album", "public")
		arts = append(arts, art)
		for range 3 {
			id, _ := testutil.ReadyTrack(t, pool, owner, art, rel, "S", 1, "public")
			exec(`UPDATE tracks SET genre = $2 WHERE id = $1`, id, genre)
			songs = append(songs, id)
		}
	}
	listener := testutil.User(t, pool, "user", true)
	mixed := func() []string {
		t.Helper()
		res, err := recommend.Mix(ctx, pool, listener, songs[0], nil, 25, "x")
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, p := range res.Picks {
			out = append(out, p.TrackID)
		}
		slices.Sort(out)
		return out
	}
	sorted := func(ids ...string) []string { s := slices.Clone(ids); slices.Sort(s); return s }
	if got := mixed(); !slices.Equal(got, sorted(songs[1:]...)) {
		t.Fatalf("before = %v", got)
	}

	// the listener's "Your sound" holds songs the server put in, and one of their own
	var playlist string
	if err := pool.QueryRow(ctx, `INSERT INTO playlists (owner_id, name) VALUES ($1, 'Your sound') RETURNING id`, listener).Scan(&playlist); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO your_sound (user_id, playlist_id, refreshed_at) VALUES ($1, $2, now())`, listener, playlist)
	for i, x := range []struct {
		song     string
		byServer bool
	}{{songs[1], true}, {songs[2], true}, {songs[1], false}, {songs[3], true}} {
		exec(`INSERT INTO playlist_items (playlist_id, track_id, position, added_by, by_server) VALUES ($1, $2, $3, $4, $5)`, playlist, x.song, i, listener, x.byServer)
		if x.byServer {
			exec(`INSERT INTO your_sound_offers (user_id, track_id) VALUES ($1, $2)`, listener, x.song)
		}
	}
	items := func() (out []string) {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT track_id || CASE WHEN by_server THEN ':server:' ELSE ':own:' END || position FROM playlist_items WHERE playlist_id = $1 ORDER BY position`, playlist)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		return out
	}

	block := func(body map[string]any) (int, map[string]any) {
		return call(f.r, "POST", "/admin/recommendations/blocks", f.adminT, body)
	}
	for name, c := range map[string]struct {
		body map[string]any
		code int
	}{
		"neither":   {map[string]any{"reason": "poor quality"}, 422},
		"both":      {map[string]any{"trackId": songs[1], "artistId": arts[1], "reason": "poor quality"}, 422},
		"no reason": {map[string]any{"trackId": songs[1]}, 422},
		"no song":   {map[string]any{"trackId": "trk_none", "reason": "poor quality"}, 404},
	} {
		if code, b := block(c.body); code != c.code {
			t.Errorf("%s = %d %v, want %d", name, code, b, c.code)
		}
	}

	// a song: gone from mixes and from what the server put in; their own copy stays; positions stay whole
	code, b := block(map[string]any{"trackId": songs[1], "reason": "wrong genre tag"})
	if code != 201 || b["removedFromPlaylists"] != float64(1) {
		t.Fatalf("block song = %d %v", code, b)
	}
	songBlock := strconv.FormatInt(int64(b["id"].(float64)), 10)
	if code, b := block(map[string]any{"trackId": songs[1], "reason": "again please"}); code != 409 || errCode(b) != "already_blocked" {
		t.Fatalf("twice = %d %v", code, b)
	}
	if got := mixed(); !slices.Equal(got, sorted(songs[2:]...)) {
		t.Fatalf("after the song = %v", got)
	}
	if want := []string{songs[2] + ":server:0", songs[1] + ":own:1", songs[3] + ":server:2"}; !slices.Equal(items(), want) {
		t.Fatalf("playlist = %v, want %v", items(), want)
	}
	var offered int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM your_sound_offers WHERE user_id = $1 AND track_id = $2`, listener, songs[1]).Scan(&offered); err != nil || offered != 0 {
		t.Fatalf("still offered = %d %v", offered, err)
	}
	// it stays a song people can play and find
	var visible int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM rec_track_features WHERE track_id = $1`, songs[1]).Scan(&visible); err != nil || visible != 1 {
		t.Fatalf("still playable = %d %v", visible, err)
	}

	// an artist: every song of theirs, one added afterwards too
	code, b = block(map[string]any{"artistId": arts[1], "reason": "rights are being checked"})
	if code != 201 || b["removedFromPlaylists"] != float64(1) {
		t.Fatalf("block artist = %d %v", code, b)
	}
	artistBlock := strconv.FormatInt(int64(b["id"].(float64)), 10)
	exec(`UPDATE tracks SET artist_id = $2 WHERE id = $1`, songs[2], arts[1]) // as if the artist added another
	if got := mixed(); len(got) != 0 {
		t.Fatalf("after the artist = %v", got)
	}
	// a refresh neither puts a blocked song back nor keeps one that slipped in
	exec(`INSERT INTO playlist_items (playlist_id, track_id, position, added_by, by_server) VALUES ($1, $2, 9, $3, true)`, playlist, songs[4], listener)
	exec(`UPDATE your_sound SET refreshed_at = NULL WHERE user_id = $1`, listener)
	if _, err := listening.Sound(ctx, pool, listener); err != nil {
		t.Fatal(err)
	}
	var blockedIn int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM playlist_items WHERE playlist_id = $1 AND by_server AND track_id IN (SELECT track_id FROM rec_blocked_tracks)`, playlist).
		Scan(&blockedIn); err != nil || blockedIn != 0 {
		t.Fatalf("blocked songs after a refresh = %d %v", blockedIn, err)
	}

	code, b = call(f.r, "GET", "/admin/recommendations/blocks", f.adminT, nil)
	list, _ := b["blocks"].([]any)
	found := map[string]map[string]any{}
	for _, x := range list {
		m := x.(map[string]any)
		found[m["targetId"].(string)] = m
	}
	if code != 200 || found[songs[1]]["kind"] != "track" || found[songs[1]]["reason"] != "wrong genre tag" || found[songs[1]]["blockedBy"] == nil ||
		found[arts[1]]["kind"] != "artist" || found[arts[1]]["songs"].(float64) < 3 {
		t.Fatalf("list = %d %v", code, b)
	}
	if code, _ := call(f.r, "POST", "/admin/recommendations/blocks", testutil.Token(t, listener), map[string]any{"trackId": songs[3], "reason": "not mine to do"}); code != 403 {
		t.Fatalf("a listener cannot block = %d", code)
	}

	// undo: suggested again, and logged
	for _, id := range []string{songBlock, artistBlock} {
		if code, b := call(f.r, "DELETE", "/admin/recommendations/blocks/"+id, f.adminT, nil); code != 204 {
			t.Fatalf("unblock = %d %v", code, b)
		}
	}
	if code, _ := call(f.r, "DELETE", "/admin/recommendations/blocks/"+songBlock, f.adminT, nil); code != 404 {
		t.Fatalf("unblock twice = %d", code)
	}
	if got := mixed(); !slices.Equal(got, sorted(songs[1:]...)) {
		t.Fatalf("after undo = %v", got)
	}
	if f.audit("recommendation.block", songs[1]) != 1 || f.audit("recommendation.unblock", arts[1]) != 1 {
		t.Fatal("blocks and unblocks are logged")
	}
}
