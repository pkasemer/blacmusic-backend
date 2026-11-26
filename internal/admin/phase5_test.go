package admin

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/blacmusic/backend/internal/testutil"
)

// image makes a finished image of the given purpose owned by the administrator.
func (f *full) image(purpose string) string {
	f.t.Helper()
	var id string
	if err := f.h.pool.QueryRow(context.Background(), `INSERT INTO images (owner_id, purpose, status, original_key, size_bytes) VALUES ($1, $2, 'ready', 'k', 1) RETURNING id`,
		f.admin, purpose).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func TestAdminCreatesAnArtistAndARelease(t *testing.T) {
	f := setupFull(t)
	pool, ctx := f.h.pool, context.Background()
	tag := strings.ToLower(testutil.Unique("cat"))

	// the artist: no account, optionally a picture
	for name, body := range map[string]map[string]any{
		"no name": {}, "long name": {"name": strings.Repeat("n", 101)}, "bad picture": {"name": "X " + tag, "imageId": "img_" + strings.Repeat("0", 32)},
	} {
		if code, _ := call(f.r, "POST", "/admin/artists", f.adminT, body); code != 422 {
			t.Errorf("%s = %d, want 422", name, code)
		}
	}
	avatar := f.image("avatar")
	code, b := call(f.r, "POST", "/admin/artists", f.adminT, map[string]any{"name": "Catalog " + tag, "bio": "from the archive", "imageId": avatar})
	art, _ := b["id"].(string)
	if code != 201 || art == "" || b["owner"] != nil || b["bio"] != "from the archive" || !strings.Contains(b["imageUrl"].(string), "/v1/media/"+avatar+"/medium.jpg") {
		t.Fatalf("create artist = %d %v", code, b)
	}
	if v := b["verification"].(map[string]any); v["state"] != "unverified" {
		t.Fatalf("verification = %v", v)
	}
	if f.audit("artist.create", art) != 1 {
		t.Fatal("creating an artist must be audited")
	}
	// the picture shows wherever the artist does
	var browsed string
	pool.QueryRow(ctx, `SELECT COALESCE(u.avatar_image_id, a.avatar_image_id) FROM artists a LEFT JOIN users u ON u.id = a.owner_user_id WHERE a.id = $1`, art).Scan(&browsed) //nolint:errcheck
	if browsed != avatar {
		t.Fatalf("the picture is stored on the artist: %q", browsed)
	}
	// replace and remove the picture; an artist with an account keeps its owner's picture
	other := f.image("avatar")
	if code, b = call(f.r, "PATCH", "/admin/artists/"+art, f.adminT, map[string]any{"imageId": other}); code != 200 || !strings.Contains(b["imageUrl"].(string), other) {
		t.Fatalf("replace picture = %d %v", code, b)
	}
	if code, b = call(f.r, "PATCH", "/admin/artists/"+art, f.adminT, map[string]any{"imageId": ""}); code != 200 || b["imageUrl"] != nil {
		t.Fatalf("remove picture = %d %v", code, b)
	}
	if code, _ = call(f.r, "PATCH", "/admin/artists/"+art, f.adminT, map[string]any{"imageId": f.image("cover")}); code != 422 {
		t.Fatalf("a cover image as a picture = %d, want 422", code)
	}
	owner := testutil.User(t, pool, "user", true)
	owned := testutil.Artist(t, pool, owner, testutil.Unique("Owned"), testutil.GiB)
	if code, b = call(f.r, "PATCH", "/admin/artists/"+owned, f.adminT, map[string]any{"imageId": f.image("avatar")}); code != 422 || errCode(b) != "owned_artist" {
		t.Fatalf("a picture for an artist with an account = %d %v", code, b)
	}
	// a name that matches a verified artist is flagged for review
	pool.Exec(ctx, `UPDATE artists SET verification = 'verified' WHERE id = $1`, owned) //nolint:errcheck
	var ownedName string
	pool.QueryRow(ctx, `SELECT name FROM artists WHERE id = $1`, owned).Scan(&ownedName) //nolint:errcheck
	if code, b = call(f.r, "POST", "/admin/artists", f.adminT, map[string]any{"name": strings.ToUpper(ownedName)}); code != 201 || b["flagged"] != true {
		t.Fatalf("same name as a verified artist = %d %v", code, b)
	}

	// the release
	cover := f.image("cover")
	for name, body := range map[string]map[string]any{
		"bad type": {"type": "lp", "title": "X"}, "no title": {"type": "single"}, "bad genre": {"type": "single", "title": "X", "genre": "nope"},
		"bad date": {"type": "single", "title": "X", "releaseDate": "soon"}, "bad cover": {"type": "single", "title": "X", "coverImageId": "img_" + strings.Repeat("1", 32)},
	} {
		if code, _ = call(f.r, "POST", "/admin/artists/"+art+"/releases", f.adminT, body); code != 422 {
			t.Errorf("%s = %d, want 422", name, code)
		}
	}
	if code, _ = call(f.r, "POST", "/admin/artists/art_nope/releases", f.adminT, map[string]any{"type": "single", "title": "X"}); code != 404 {
		t.Fatalf("unknown artist = %d", code)
	}
	code, b = call(f.r, "POST", "/admin/artists/"+art+"/releases", f.adminT, map[string]any{
		"type": "album", "title": "Archive " + tag, "description": "old tapes", "genre": "gospel", "releaseDate": "2019-05-01", "coverImageId": cover})
	rel, _ := b["id"].(string)
	if code != 201 || rel == "" || b["visibility"] != "draft" || b["genre"] != "Gospel" || b["releaseDate"] != "2019-05-01" ||
		!strings.Contains(b["coverUrl"].(string), cover) || len(b["tracks"].([]any)) != 0 || len(b["uploads"].([]any)) != 0 || b["artist"].(map[string]any)["id"] != art {
		t.Fatalf("create release = %d %v", code, b)
	}
	if f.audit("release.create", rel) != 1 {
		t.Fatal("creating a release must be audited")
	}
	if code, b = call(f.r, "PATCH", "/admin/releases/"+rel, f.adminT, map[string]any{"releaseDate": "2020-01-02", "coverImageId": ""}); code != 200 || b["releaseDate"] != "2020-01-02" || b["coverUrl"] != nil {
		t.Fatalf("patch date and cover = %d %v", code, b)
	}
	if code, _ = call(f.r, "PATCH", "/admin/releases/"+rel, f.adminT, map[string]any{"releaseDate": "2020-13-45"}); code != 422 {
		t.Fatalf("bad date = %d", code)
	}
	if code, b = call(f.r, "PATCH", "/admin/releases/"+rel, f.adminT, map[string]any{"releaseDate": ""}); code != 200 || b["releaseDate"] != nil && b["releaseDate"] != "" {
		t.Fatalf("clearing the date = %d %v", code, b)
	}
	// a banned artist cannot get new releases
	pool.Exec(ctx, `UPDATE artists SET banned_at = now() WHERE id = $1`, art) //nolint:errcheck
	if code, b = call(f.r, "POST", "/admin/artists/"+art+"/releases", f.adminT, map[string]any{"type": "single", "title": "X"}); code != 409 || errCode(b) != "artist_banned" {
		t.Fatalf("banned artist = %d %v", code, b)
	}
}

func TestAdminUploadsAudioAndPublishes(t *testing.T) {
	f := setupFull(t)
	pool, ctx := f.h.pool, context.Background()
	tag := strings.ToLower(testutil.Unique("up"))
	_, b := call(f.r, "POST", "/admin/artists", f.adminT, map[string]any{"name": "Archive " + tag})
	art := b["id"].(string)
	_, b = call(f.r, "POST", "/admin/artists/"+art+"/releases", f.adminT, map[string]any{"type": "album", "title": "Tapes " + tag})
	rel := b["id"].(string)
	plan := func(body map[string]any) (int, map[string]any) {
		return call(f.r, "POST", "/admin/releases/"+rel+"/uploads", f.adminT, body)
	}
	good := map[string]any{"title": "Track one", "genre": "gospel", "trackNumber": 1, "filename": "one.flac", "sizeBytes": 21, "rightsConfirmed": true}

	// the rights attestation is required; nothing else is touched
	bad := map[string]any{"title": "x", "filename": "x.flac", "sizeBytes": 21}
	if code, b := plan(bad); code != 422 || errCode(b) != "rights_not_accepted" {
		t.Fatalf("without rightsConfirmed = %d %v", code, b)
	}
	if code, _ := plan(map[string]any{"title": "", "filename": "x.flac", "sizeBytes": 21, "rightsConfirmed": true}); code != 422 {
		t.Fatalf("empty title = %d", code)
	}
	if code, _ := call(f.r, "POST", "/admin/releases/alb_nope/uploads", f.adminT, good); code != 404 {
		t.Fatalf("unknown release = %d", code)
	}

	// a small file: one PUT. No quota: the artist has no account and no allowance.
	code, b := plan(good)
	id, _ := b["id"].(string)
	target, _ := b["upload"].(map[string]any)
	if code != 201 || id == "" || target["type"] != "single" || target["method"] != "PUT" || target["url"] == "" || b["status"] != "pending" || b["maxBytes"] == nil {
		t.Fatalf("plan = %d %v", code, b)
	}
	var uploader, artistID, terms string
	pool.QueryRow(ctx, `SELECT user_id::text, artist_id, rights_terms_version FROM uploads WHERE id = $1`, id).Scan(&uploader, &artistID, &terms) //nolint:errcheck
	if uploader != f.admin || artistID != art || terms != "admin" {
		t.Fatalf("stored as uploader %s artist %s terms %q", uploader, artistID, terms)
	}
	if f.audit("upload.create", rel) != 1 {
		t.Fatal("starting an upload must be audited")
	}
	// progress shows on the release page after a reload
	_, b = call(f.r, "GET", "/admin/releases/"+rel, f.adminT, nil)
	ups := b["uploads"].([]any)
	if len(ups) != 1 || ups[0].(map[string]any)["id"] != id || ups[0].(map[string]any)["status"] != "pending" || ups[0].(map[string]any)["filename"] != "one.flac" || ups[0].(map[string]any)["sizeBytes"] != float64(21) {
		t.Fatalf("detail uploads = %v", ups)
	}
	// status, then complete: refused until the file is there
	if code, b = call(f.r, "GET", "/admin/uploads/"+id, f.adminT, nil); code != 200 || b["status"] != "pending" {
		t.Fatalf("status = %d %v", code, b)
	}
	if code, b = call(f.r, "POST", "/admin/uploads/"+id+"/complete", f.adminT, nil); code != 409 || errCode(b) != "not_uploaded" {
		t.Fatalf("complete before the file = %d %v", code, b)
	}
	f.blobs.Put("originals/"+id, []byte("pretend this is audio"))
	if code, b = call(f.r, "POST", "/admin/uploads/"+id+"/complete", f.adminT, nil); code != 202 || b["status"] != "queued" {
		t.Fatalf("complete = %d %v", code, b)
	}
	if f.audit("track.upload", rel) != 1 {
		t.Fatal("finishing an upload must be audited")
	}
	if code, _ = call(f.r, "DELETE", "/admin/uploads/"+id, f.adminT, nil); code != 409 {
		t.Fatalf("deleting a queued upload = %d, want 409", code)
	}
	// a normal user cannot reach any of it
	user := testutil.Token(t, testutil.User(t, pool, "user", true))
	if code, _ = call(f.r, "GET", "/admin/uploads/"+id, user, nil); code != 403 {
		t.Fatalf("a normal user = %d, want 403", code)
	}

	// a big file: parts, resumable; a cancelled upload goes away
	code, b = plan(map[string]any{"title": "Long take", "filename": "long.wav", "sizeBytes": 12000, "rightsConfirmed": true})
	bigID, _ := b["id"].(string)
	bigTarget, _ := b["upload"].(map[string]any)
	if code != 201 || bigTarget["type"] != "multipart" || bigTarget["partCount"] != float64(3) || len(bigTarget["parts"].([]any)) != 3 {
		t.Fatalf("multipart plan = %d %v", code, b)
	}
	if code, b = call(f.r, "GET", "/admin/uploads/"+bigID+"/parts", f.adminT, nil); code != 200 || len(b["parts"].([]any)) != 3 {
		t.Fatalf("parts = %d %v", code, b)
	}
	if code, _ = call(f.r, "DELETE", "/admin/uploads/"+bigID, f.adminT, nil); code != 204 {
		t.Fatalf("cancel = %d", code)
	}
	if code, _ = call(f.r, "GET", "/admin/uploads/"+bigID, f.adminT, nil); code != 404 {
		t.Fatalf("a cancelled upload = %d, want 404", code)
	}

	// an artist with an account keeps its quota rules
	owner := testutil.User(t, pool, "user", true)
	tiny := testutil.Artist(t, pool, owner, testutil.Unique("Tiny"), 10)
	ownedRel := testutil.Release(t, pool, tiny, "Small", "single", "draft")
	if code, b = call(f.r, "POST", "/admin/releases/"+ownedRel+"/uploads", f.adminT, map[string]any{"title": "Big", "filename": "b.flac", "sizeBytes": 500, "rightsConfirmed": true}); code != 402 || errCode(b) != "quota_exceeded" {
		t.Fatalf("over the quota of an artist with an account = %d %v", code, b)
	}

	// publishing: nothing finished yet, then a track, then schedule and unpublish
	if code, b = call(f.r, "POST", "/admin/releases/"+rel+"/publish", f.adminT, nil); code != 409 || errCode(b) != "uploads_pending" {
		t.Fatalf("publish with a queued upload = %d %v", code, b)
	}
	pool.Exec(ctx, `UPDATE uploads SET status = 'failed', error = 'test' WHERE id = $1`, id) //nolint:errcheck
	if code, b = call(f.r, "POST", "/admin/releases/"+rel+"/publish", f.adminT, nil); code != 422 || errCode(b) != "no_tracks" {
		t.Fatalf("publish without a track = %d %v", code, b)
	}
	track, _ := testutil.ReadyTrack(t, pool, f.admin, art, rel, "Track one", 21, "draft")
	var emailsBefore int
	pool.QueryRow(ctx, `SELECT count(*) FROM email_messages`).Scan(&emailsBefore) //nolint:errcheck
	when := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	if code, b = call(f.r, "POST", "/admin/releases/"+rel+"/publish", f.adminT, map[string]any{"publishAt": when}); code != 200 || b["visibility"] != "scheduled" || b["publishAt"] == nil {
		t.Fatalf("schedule = %d %v", code, b)
	}
	if code, b = call(f.r, "POST", "/admin/releases/"+rel+"/unpublish", f.adminT, nil); code != 200 || b["visibility"] != "draft" {
		t.Fatalf("unpublish = %d %v", code, b)
	}
	if code, b = call(f.r, "POST", "/admin/releases/"+rel+"/publish", f.adminT, nil); code != 200 || b["visibility"] != "public" || b["tracks"].([]any)[0].(map[string]any)["visibility"] != "public" {
		t.Fatalf("publish = %d %v", code, b)
	}
	var vis string
	pool.QueryRow(ctx, `SELECT visibility FROM tracks WHERE id = $1`, track).Scan(&vis) //nolint:errcheck
	if vis != "public" {
		t.Fatalf("the track is %s", vis)
	}
	var emailsAfter int
	pool.QueryRow(ctx, `SELECT count(*) FROM email_messages`).Scan(&emailsAfter) //nolint:errcheck
	if emailsAfter != emailsBefore {
		t.Fatalf("an artist without an account was emailed (%d new)", emailsAfter-emailsBefore)
	}
	if f.audit("release.publish", rel) != 2 || f.audit("release.unpublish", rel) != 1 {
		t.Fatal("publishing and unpublishing must be audited")
	}
	// the owner rules still hold
	pool.Exec(ctx, `UPDATE albums SET visibility = 'draft' WHERE id = $1`, rel) //nolint:errcheck
	if code, _ = call(f.r, "POST", "/admin/releases/"+rel+"/takedown", f.adminT, map[string]any{"reason": "testing taken down"}); code != 204 {
		t.Fatalf("takedown = %d", code)
	}
	if code, b = call(f.r, "POST", "/admin/releases/"+rel+"/publish", f.adminT, nil); code != 409 || errCode(b) != "taken_down" {
		t.Fatalf("publishing a taken-down release = %d %v", code, b)
	}
}
