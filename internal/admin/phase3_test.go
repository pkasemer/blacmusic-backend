package admin

import (
	"context"
	"strings"
	"testing"

	"github.com/blacmusic/backend/internal/testutil"
)

func TestEditorialPlaylists(t *testing.T) {
	f := setupFull(t)
	pool, ctx := f.h.pool, context.Background()

	// not configured, then configured but the account does not exist or is not an administrator
	f.h.EditorialEmail = ""
	if code, b := call(f.r, "GET", "/admin/playlists", f.adminT, nil); code != 409 || errCode(b) != "editorial_not_configured" {
		t.Fatalf("unset = %d %v", code, b)
	}
	email := strings.ToLower(testutil.Unique("editorial")) + "@example.com"
	f.h.EditorialEmail = email
	if code, b := call(f.r, "GET", "/admin/playlists", f.adminT, nil); code != 409 || errCode(b) != "editorial_account_missing" {
		t.Fatalf("no such account = %d %v", code, b)
	}
	pool.Exec(ctx, `UPDATE users SET email = $2 WHERE id = $1`, testutil.User(t, pool, "user", true), email) //nolint:errcheck
	if code, b := call(f.r, "GET", "/admin/playlists", f.adminT, nil); code != 409 || errCode(b) != "editorial_account_missing" {
		t.Fatalf("an account that is not an administrator = %d %v (anybody could register that address)", code, b)
	}
	pool.Exec(ctx, `UPDATE users SET role = 'admin' WHERE email = $1`, email) //nolint:errcheck

	// someone else's playlist is not reachable here
	other := testutil.User(t, pool, "user", true)
	var foreign string
	pool.QueryRow(ctx, `INSERT INTO playlists (owner_id, name, is_public) VALUES ($1, 'not editorial', true) RETURNING id`, other).Scan(&foreign) //nolint:errcheck
	if code, _ := call(f.r, "GET", "/admin/playlists/"+foreign, f.adminT, nil); code != 404 {
		t.Fatalf("a user's playlist = %d, want 404", code)
	}
	if code, _ := call(f.r, "DELETE", "/admin/playlists/"+foreign, f.adminT, nil); code != 404 {
		t.Fatalf("deleting a user's playlist = %d, want 404", code)
	}

	// create: private and not featured
	if code, _ := call(f.r, "POST", "/admin/playlists", f.adminT, map[string]any{"name": " "}); code != 422 {
		t.Fatalf("empty name = %d", code)
	}
	tag := strings.ToLower(testutil.Unique("pl"))
	code, b := call(f.r, "POST", "/admin/playlists", f.adminT, map[string]any{"name": "Kampala " + tag, "description": "fresh"})
	id, _ := b["id"].(string)
	if code != 201 || id == "" || b["isPublic"] != false || b["isFeatured"] != false || b["published"] != false || len(b["items"].([]any)) != 0 {
		t.Fatalf("create = %d %v", code, b)
	}
	if f.audit("playlist.create", id) != 1 {
		t.Fatal("creating must be audited")
	}

	// items: a hidden track is allowed (flagged), order is kept, repeats are allowed, unknown tracks are refused
	owner := testutil.User(t, pool, "user", true)
	art := testutil.Artist(t, pool, owner, testutil.Unique("Pl"), testutil.GiB)
	pubRel := testutil.Release(t, pool, art, "R1", "single", "public")
	draftRel := testutil.Release(t, pool, art, "R2", "single", "draft")
	pubTrack, _ := testutil.ReadyTrack(t, pool, owner, art, pubRel, "Hit", 1, "public")
	draftTrack, _ := testutil.ReadyTrack(t, pool, owner, art, draftRel, "Hidden", 1, "draft")

	if code, _ = call(f.r, "PUT", "/admin/playlists/"+id+"/items", f.adminT, map[string]any{"trackIds": []string{draftTrack}}); code != 200 {
		t.Fatalf("put hidden track = %d", code)
	}
	if code, b = call(f.r, "POST", "/admin/playlists/"+id+"/publish", f.adminT, nil); code != 422 || errCode(b) != "no_public_track" {
		t.Fatalf("publishing with only a hidden track = %d %v", code, b)
	}
	code, b = call(f.r, "PUT", "/admin/playlists/"+id+"/items", f.adminT, map[string]any{"trackIds": []string{pubTrack, draftTrack, pubTrack}})
	items, _ := b["items"].([]any)
	if code != 200 || len(items) != 3 || b["trackCount"] != float64(3) || b["playableTrackCount"] != float64(2) {
		t.Fatalf("put items = %d %v", code, b)
	}
	first, second := items[0].(map[string]any), items[1].(map[string]any)
	if first["trackId"] != pubTrack || first["position"] != float64(0) || first["public"] != true || second["public"] != false || first["title"] != "Hit" || first["itemId"] == "" {
		t.Fatalf("items = %v", items)
	}
	if code, _ = call(f.r, "PUT", "/admin/playlists/"+id+"/items", f.adminT, map[string]any{"trackIds": []string{"trk_nope"}}); code != 422 {
		t.Fatalf("unknown track = %d", code)
	}
	if code, _ = call(f.r, "PUT", "/admin/playlists/"+id+"/items", f.adminT, map[string]any{}); code != 400 {
		t.Fatalf("no trackIds = %d", code)
	}
	if _, b = call(f.r, "GET", "/admin/playlists/"+id, f.adminT, nil); len(b["items"].([]any)) != 3 {
		t.Fatalf("a refused change must leave the list alone: %v", b)
	}

	// edits
	if code, b = call(f.r, "PATCH", "/admin/playlists/"+id, f.adminT, map[string]any{"name": "Kampala Nights " + tag, "description": "after dark"}); code != 200 || b["description"] != "after dark" {
		t.Fatalf("patch = %d %v", code, b)
	}
	for name, body := range map[string]map[string]any{"empty": {}, "blank name": {"name": ""}, "bad cover": {"coverImageId": "img_" + strings.Repeat("0", 32)}} {
		if code, _ = call(f.r, "PATCH", "/admin/playlists/"+id, f.adminT, body); code != 422 {
			t.Errorf("%s = %d, want 422", name, code)
		}
	}

	// publish, groups, filters, unpublish
	var group string
	pool.QueryRow(ctx, `INSERT INTO playlist_groups (id, title) VALUES ('grp_'||replace(gen_random_uuid()::text, '-', ''), 'Top picks') RETURNING id`).Scan(&group) //nolint:errcheck
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM playlist_groups WHERE id = $1`, group) })                                                       //nolint:errcheck
	pool.Exec(ctx, `INSERT INTO playlist_group_items (group_id, playlist_id, position) VALUES ($1, $2, 1)`, group, id)                                              //nolint:errcheck
	if code, b = call(f.r, "POST", "/admin/playlists/"+id+"/publish", f.adminT, nil); code != 200 || b["published"] != true || b["isPublic"] != true || b["isFeatured"] != true {
		t.Fatalf("publish = %d %v", code, b)
	}
	code, b = call(f.r, "GET", "/admin/playlists?q="+tag+"&published=true", f.adminT, nil)
	list, _ := b["playlists"].([]any)
	if code != 200 || len(list) != 1 || b["total"] != float64(1) {
		t.Fatalf("list = %d %v", code, b)
	}
	row := list[0].(map[string]any)
	groups, _ := row["groups"].([]any)
	if row["id"] != id || len(groups) != 1 || groups[0].(map[string]any)["title"] != "Top picks" {
		t.Fatalf("row = %v", row)
	}
	if _, b = call(f.r, "GET", "/admin/playlists?q="+tag+"&published=false", f.adminT, nil); b["total"] != float64(0) {
		t.Fatalf("published=false = %v", b)
	}
	if code, _ = call(f.r, "GET", "/admin/playlists?featured=maybe", f.adminT, nil); code != 400 {
		t.Fatalf("bad filter = %d", code)
	}
	if code, b = call(f.r, "POST", "/admin/playlists/"+id+"/unpublish", f.adminT, nil); code != 200 || b["published"] != false || b["isFeatured"] != false {
		t.Fatalf("unpublish = %d %v", code, b)
	}
	if f.audit("playlist.publish", id) != 1 || f.audit("playlist.unpublish", id) != 1 || f.audit("playlist.items", id) != 2 {
		t.Fatal("publishing and item changes must be audited")
	}

	// delete
	if code, _ = call(f.r, "DELETE", "/admin/playlists/"+id, f.adminT, nil); code != 204 {
		t.Fatalf("delete = %d", code)
	}
	if code, _ = call(f.r, "GET", "/admin/playlists/"+id, f.adminT, nil); code != 404 {
		t.Fatalf("after delete = %d", code)
	}
	if f.audit("playlist.delete", id) != 1 {
		t.Fatal("deleting must be audited")
	}
}
