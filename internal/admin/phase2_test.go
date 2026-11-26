package admin

import (
	"context"
	"strings"
	"testing"

	"github.com/blacmusic/backend/internal/release"
	"github.com/blacmusic/backend/internal/testutil"
)

func TestReleaseListDetailAndEdits(t *testing.T) {
	f := setupFull(t)
	pool, ctx := f.h.pool, context.Background()
	tag := strings.ToLower(testutil.Unique("rel"))
	owner := testutil.User(t, pool, "user", true)
	art := testutil.Artist(t, pool, owner, "Band "+tag, testutil.GiB)
	pub := testutil.Release(t, pool, art, "Public "+tag, "album", "public")
	draft := testutil.Release(t, pool, art, "Draft "+tag, "single", "draft")
	t1, _ := testutil.ReadyTrack(t, pool, owner, art, pub, "One", 1, "public")
	t2, _ := testutil.ReadyTrack(t, pool, owner, art, pub, "Two", 1, "public")
	testutil.ReadyTrack(t, pool, owner, art, draft, "Hidden", 1, "draft")

	code, b := call(f.r, "GET", "/admin/releases?q="+tag, f.adminT, nil)
	rels, _ := b["releases"].([]any)
	if code != 200 || len(rels) != 2 || b["total"] != float64(2) {
		t.Fatalf("list = %d %v (drafts are included)", code, b)
	}
	first := rels[0].(map[string]any)
	for _, k := range []string{"id", "title", "type", "kind", "artistId", "artistName", "visibility", "takenDown", "trackCount", "publishedAt", "createdAt"} {
		if _, ok := first[k]; !ok {
			t.Fatalf("row lacks %q: %v", k, first)
		}
	}
	if _, b = call(f.r, "GET", "/admin/releases?q="+tag+"&visibility=draft", f.adminT, nil); b["total"] != float64(1) {
		t.Fatalf("visibility filter = %v", b)
	}
	if _, b = call(f.r, "GET", "/admin/releases?q="+tag+"&type=album&artistId="+art, f.adminT, nil); b["total"] != float64(1) {
		t.Fatalf("type + artist filter = %v", b)
	}
	if code, _ = call(f.r, "GET", "/admin/releases?visibility=bogus", f.adminT, nil); code != 400 {
		t.Fatalf("bad visibility = %d", code)
	}

	code, b = call(f.r, "GET", "/admin/releases/"+pub, f.adminT, nil)
	tracks, _ := b["tracks"].([]any)
	if code != 200 || len(tracks) != 2 || b["takedown"] != nil || b["artist"].(map[string]any)["id"] != art || b["kind"] != "ep" {
		t.Fatalf("detail = %d %v", code, b)
	}
	if code, _ = call(f.r, "GET", "/admin/releases/alb_nope", f.adminT, nil); code != 404 {
		t.Fatalf("unknown release = %d", code)
	}

	// edits bypass the draft-only rules and are validated
	code, b = call(f.r, "PATCH", "/admin/releases/"+pub, f.adminT, map[string]any{"title": "Better title", "description": "d", "genre": "bongo flava"})
	if code != 200 || b["title"] != "Better title" || b["genre"] != "Bongo Flava" {
		t.Fatalf("patch = %d %v", code, b)
	}
	for name, body := range map[string]map[string]any{
		"empty":         {},
		"blank title":   {"title": " "},
		"bad genre":     {"genre": "nope"},
		"single of two": {"releaseType": "single"},
		"bad type":      {"releaseType": "lp"},
	} {
		if code, _ = call(f.r, "PATCH", "/admin/releases/"+pub, f.adminT, body); code != 422 {
			t.Errorf("%s = %d, want 422", name, code)
		}
	}
	if code, _ = call(f.r, "PATCH", "/admin/releases/"+draft, f.adminT, map[string]any{"releaseType": "album"}); code != 200 {
		t.Fatalf("a single becomes an album = %d", code)
	}
	if f.audit("release.update", pub) != 1 {
		t.Fatal("release edits must be audited")
	}

	code, b = call(f.r, "PATCH", "/admin/tracks/"+t1, f.adminT, map[string]any{"title": "Renamed", "explicit": true, "trackNumber": 2, "genre": "gospel"})
	if code != 200 || b["title"] != "Renamed" || b["explicit"] != true || b["trackNumber"] != float64(2) || b["genre"] != "Gospel" {
		t.Fatalf("track patch = %d %v", code, b)
	}
	if code, b = call(f.r, "PATCH", "/admin/tracks/"+t1, f.adminT, map[string]any{"trackNumber": 0}); code != 200 || b["trackNumber"] != nil {
		t.Fatalf("trackNumber 0 clears it: %d %v", code, b)
	}
	if code, _ = call(f.r, "PATCH", "/admin/tracks/"+t2, f.adminT, map[string]any{"trackNumber": 1000}); code != 422 {
		t.Fatalf("track number 1000 = %d", code)
	}
	if code, _ = call(f.r, "PATCH", "/admin/tracks/trk_nope", f.adminT, map[string]any{"title": "x"}); code != 404 {
		t.Fatalf("unknown track = %d", code)
	}
	if f.audit("track.update", t1) != 2 {
		t.Fatal("track edits must be audited")
	}
	_ = ctx
}

func TestTakedownAndRestore(t *testing.T) {
	f := setupFull(t)
	pool, ctx := f.h.pool, context.Background()
	owner := testutil.User(t, pool, "user", true)
	art := testutil.Artist(t, pool, owner, testutil.Unique("Down"), testutil.GiB)
	rel := testutil.Release(t, pool, art, "Contested", "single", "public")
	tr, _ := testutil.ReadyTrack(t, pool, owner, art, rel, "Song", 1, "public")

	if code, _ := call(f.r, "POST", "/admin/releases/"+rel+"/takedown", f.adminT, map[string]any{"reason": "x"}); code != 422 {
		t.Fatalf("short reason = %d", code)
	}
	if code, b := call(f.r, "POST", "/admin/releases/"+rel+"/takedown", f.adminT, map[string]any{"reason": "copyright complaint"}); code != 204 {
		t.Fatalf("takedown = %d %v", code, b)
	}
	if code, b := call(f.r, "POST", "/admin/releases/"+rel+"/takedown", f.adminT, map[string]any{"reason": "copyright complaint"}); code != 409 || errCode(b) != "already_taken_down" {
		t.Fatalf("second takedown = %d %v", code, b)
	}
	var vis, trackVis string
	pool.QueryRow(ctx, `SELECT visibility FROM albums WHERE id = $1`, rel).Scan(&vis)     //nolint:errcheck
	pool.QueryRow(ctx, `SELECT visibility FROM tracks WHERE id = $1`, tr).Scan(&trackVis) //nolint:errcheck
	if vis != "draft" || trackVis != "draft" {
		t.Fatalf("hidden like a draft: release %s, track %s", vis, trackVis)
	}
	// the owner sees why and cannot bring it back; strangers do not see the reason
	owned, err := f.rel.Get(ctx, rel, owner)
	if err != nil || !owned.TakenDown || owned.TakedownReason != "copyright complaint" {
		t.Fatalf("owner view = %+v %v", owned, err)
	}
	if _, err := f.rel.Publish(ctx, rel, owner, nil); err != release.ErrTakenDown {
		t.Fatalf("owner publish = %v, want ErrTakenDown", err)
	}
	if _, err := f.rel.Get(ctx, rel, ""); err != release.ErrNotFound {
		t.Fatalf("stranger view = %v, want ErrNotFound", err)
	}
	code, b := call(f.r, "GET", "/admin/releases/"+rel, f.adminT, nil)
	td, _ := b["takedown"].(map[string]any)
	if code != 200 || td == nil || td["reason"] != "copyright complaint" || td["previousVisibility"] != "public" || td["byBan"] != false {
		t.Fatalf("detail takedown = %d %v", code, b)
	}
	if _, b = call(f.r, "GET", "/admin/releases?takenDown=true&artistId="+art, f.adminT, nil); b["total"] != float64(1) {
		t.Fatalf("takenDown filter = %v", b)
	}
	if code, _ := call(f.r, "POST", "/admin/releases/"+rel+"/restore", f.adminT, nil); code != 204 {
		t.Fatalf("restore = %d", code)
	}
	if code, b := call(f.r, "POST", "/admin/releases/"+rel+"/restore", f.adminT, nil); code != 409 || errCode(b) != "not_taken_down" {
		t.Fatalf("second restore = %d %v", code, b)
	}
	pool.QueryRow(ctx, `SELECT visibility FROM albums WHERE id = $1`, rel).Scan(&vis)     //nolint:errcheck
	pool.QueryRow(ctx, `SELECT visibility FROM tracks WHERE id = $1`, tr).Scan(&trackVis) //nolint:errcheck
	if vis != "public" || trackVis != "public" {
		t.Fatalf("restored: release %s, track %s", vis, trackVis)
	}
	if f.audit("release.takedown", rel) != 1 || f.audit("release.restore", rel) != 1 {
		t.Fatal("takedown and restore must be audited")
	}

	// a release hidden because its artist was banned is restored by unbanning, not alone
	if code, _ := call(f.r, "POST", "/admin/artists/"+art+"/ban", f.adminT, map[string]any{"reason": "banned for test"}); code != 204 {
		t.Fatalf("ban = %d", code)
	}
	if code, b := call(f.r, "POST", "/admin/releases/"+rel+"/restore", f.adminT, nil); code != 409 || errCode(b) != "artist_banned" {
		t.Fatalf("restoring a ban-hidden release = %d %v", code, b)
	}
}

func TestDeletingReleasesAndTracks(t *testing.T) {
	f := setupFull(t)
	pool, ctx := f.h.pool, context.Background()
	owner := testutil.User(t, pool, "user", true)
	art := testutil.Artist(t, pool, owner, testutil.Unique("Del"), testutil.GiB)
	rel := testutil.Release(t, pool, art, "To delete", "album", "public")
	t1, up1 := testutil.ReadyTrack(t, pool, owner, art, rel, "A", 1000, "public")
	t2, _ := testutil.ReadyTrack(t, pool, owner, art, rel, "B", 500, "public")
	_ = up1
	fan := testutil.User(t, pool, "user", true)
	pool.Exec(ctx, `INSERT INTO library_tracks (user_id, track_id) VALUES ($1, $2)`, fan, t1)  //nolint:errcheck
	pool.Exec(ctx, `INSERT INTO library_albums (user_id, album_id) VALUES ($1, $2)`, fan, rel) //nolint:errcheck
	var pl string
	pool.QueryRow(ctx, `INSERT INTO playlists (owner_id, name) VALUES ($1, 'mix') RETURNING id`, fan).Scan(&pl) //nolint:errcheck
	pool.Exec(ctx, `INSERT INTO playlist_items (playlist_id, track_id, position) VALUES ($1, $2, 0)`, pl, t2)   //nolint:errcheck

	code, b := call(f.r, "GET", "/admin/releases/"+rel+"/deletion-preview", f.adminT, nil)
	if code != 200 || b["tracks"] != float64(2) || b["files"] != float64(2) || b["storageBytes"] != float64(1500) ||
		b["libraryReferences"] != float64(2) || b["playlistReferences"] != float64(1) {
		t.Fatalf("preview = %d %v", code, b)
	}

	// deleting one track of two keeps the release; deleting the last one takes a live release back to draft
	if code, _ = call(f.r, "DELETE", "/admin/tracks/"+t1, f.adminT, map[string]any{"reason": "x"}); code != 422 {
		t.Fatalf("short reason = %d", code)
	}
	if code, b = call(f.r, "DELETE", "/admin/tracks/"+t1, f.adminT, map[string]any{"reason": "bad upload"}); code != 204 {
		t.Fatalf("delete track = %d %v", code, b)
	}
	if f.audit("track.delete", t1) != 1 {
		t.Fatal("track deletion must be audited")
	}
	if code, _ = call(f.r, "DELETE", "/admin/tracks/"+t1, f.adminT, map[string]any{"reason": "again again"}); code != 404 {
		t.Fatalf("deleting it twice = %d", code)
	}
	var vis string
	pool.QueryRow(ctx, `SELECT visibility FROM albums WHERE id = $1`, rel).Scan(&vis) //nolint:errcheck
	if vis != "public" {
		t.Fatalf("a release that still has a track stays %s", vis)
	}
	if code, b = call(f.r, "DELETE", "/admin/releases/"+rel, f.adminT, map[string]any{"reason": "violates the terms"}); code != 204 {
		t.Fatalf("delete release = %d %v", code, b)
	}
	var n int
	pool.QueryRow(ctx, `SELECT count(*) FROM albums WHERE id = $1`, rel).Scan(&n) //nolint:errcheck
	if n != 0 {
		t.Fatal("the release is still there")
	}
	code, b = call(f.r, "GET", "/admin/audit-log?action=release.delete&targetId="+rel, f.adminT, nil)
	entries, _ := b["entries"].([]any)
	if code != 200 || len(entries) != 1 || entries[0].(map[string]any)["details"].(map[string]any)["tracks"] != float64(1) {
		t.Fatalf("audit of the deletion = %d %v", code, b)
	}
	if code, _ = call(f.r, "DELETE", "/admin/releases/"+rel, f.adminT, map[string]any{"reason": "already gone"}); code != 404 {
		t.Fatalf("deleting a deleted release = %d", code)
	}
}
