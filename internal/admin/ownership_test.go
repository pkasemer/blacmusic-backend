package admin

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"slices"
	"strings"
	"testing"

	pictures "github.com/blacmusic/backend/internal/image"
	"github.com/blacmusic/backend/internal/testutil"
	"github.com/blacmusic/backend/internal/upload"
)

type cancelledTransfers struct{ users []string }

func (c *cancelledTransfers) CancelAllOpen(_ context.Context, userID string) error {
	c.users = append(c.users, userID)
	return nil
}

// world is a catalog artist and what happens to it.
type world struct {
	*full
	transfers *cancelledTransfers
}

func newWorld(t *testing.T) *world {
	f := setupFull(t)
	w := &world{full: f, transfers: &cancelledTransfers{}}
	f.h.Blobs, f.h.Transfers = f.blobs, w.transfers
	return w
}

func (w *world) exec(sql string, args ...any) {
	w.t.Helper()
	if _, err := w.h.pool.Exec(context.Background(), sql, args...); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) text(sql string, args ...any) (s string) {
	w.t.Helper()
	if err := w.h.pool.QueryRow(context.Background(), sql, args...).Scan(&s); err != nil {
		w.t.Fatalf("%s %v: %v", sql, args, err)
	}
	return s
}

func (w *world) catalogArtist(name string) string {
	w.t.Helper()
	id := w.text(`INSERT INTO artists (name) VALUES ($1) RETURNING id`, testutil.Unique(name))
	w.t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM uploads WHERE artist_id = $1`, `DELETE FROM tracks WHERE artist_id = $1`, `DELETE FROM albums WHERE artist_id = $1`, `DELETE FROM artists WHERE id = $1`} {
			w.h.pool.Exec(context.Background(), q, id) //nolint:errcheck
		}
	})
	return id
}

// picture makes a real stored picture of the owner.
func (w *world) picture(owner, purpose string) string {
	w.t.Helper()
	var raw bytes.Buffer
	if err := png.Encode(&raw, image.NewRGBA(image.Rect(0, 0, 400, 400))); err != nil {
		w.t.Fatal(err)
	}
	id, err := pictures.Ingest(context.Background(), w.h.pool, w.blobs, owner, purpose, raw.Bytes())
	if err != nil {
		w.t.Fatal(err)
	}
	return id
}

func (w *world) hasPicture(id string) bool {
	_, stored := w.blobs.Get("images/" + id + "/medium.jpg")
	row := w.text(`SELECT count(*)::text FROM images WHERE id = $1`, id) == "1"
	if stored != row {
		w.t.Fatalf("picture %s: file %v, row %v", id, stored, row)
	}
	return row
}

type song struct{ track, upload, original string }

// song makes a finished song with its files. team: an administrator uploaded it.
func (w *world) song(uploader, artistID, releaseID, title string, size int64, team bool) song {
	w.t.Helper()
	track, up := testutil.ReadyTrack(w.t, w.h.pool, uploader, artistID, releaseID, title, size, "public")
	w.exec(`UPDATE uploads SET by_team = $2, quota_exempt = $2 WHERE id = $1`, up, team)
	s := song{track, up, "originals/" + track}
	w.blobs.Put(s.original, []byte("original"))
	for _, q := range []string{"low", "medium", "high"} {
		w.blobs.Put("tracks/"+track+"/"+q+".m4a", []byte("copy"))
	}
	return s
}

// intact is true when the song and all its files are there; gone when none of it is. Anything between fails.
func (w *world) intact(s song) bool {
	w.t.Helper()
	have := []bool{w.text(`SELECT count(*)::text FROM tracks WHERE id = $1`, s.track) == "1", w.text(`SELECT count(*)::text FROM uploads WHERE id = $1`, s.upload) == "1"}
	for _, key := range []string{s.original, "tracks/" + s.track + "/medium.m4a"} {
		_, ok := w.blobs.Get(key)
		have = append(have, ok)
	}
	if slices.Contains(have, true) && slices.Contains(have, false) {
		w.t.Fatalf("song %s is half deleted: track, upload row, original, copy = %v", s.track, have)
	}
	return have[0]
}

func (w *world) release(artistID, title, visibility, cover string) string {
	w.t.Helper()
	id := testutil.Release(w.t, w.h.pool, artistID, title, "album", visibility)
	if cover != "" {
		w.exec(`UPDATE albums SET cover_image_id = $2 WHERE id = $1`, id, cover)
	}
	return id
}

func (w *world) assign(artistID, userID string) (int, map[string]any) {
	return call(w.r, "POST", "/admin/artists/"+artistID+"/owner", w.adminT, map[string]any{"userId": userID, "reason": "the artist signed up"})
}

func (w *world) unassign(artistID string) (int, map[string]any) {
	return call(w.r, "DELETE", "/admin/artists/"+artistID+"/owner", w.adminT, map[string]any{"reason": "wrong account"})
}

// person is a user with a verified email and no artist.
func (w *world) person() string { return testutil.User(w.t, w.h.pool, "user", true) }

func (w *world) deleteUser(userID string) {
	w.t.Helper()
	email := w.text(`SELECT email FROM users WHERE id = $1`, userID)
	if code, b := call(w.r, "DELETE", "/admin/users/"+userID, w.adminT, map[string]any{"reason": "asked to be removed", "confirmEmail": email}); code != 204 {
		w.t.Fatalf("delete user = %d %v", code, b)
	}
}

func (w *world) shape(artistID string) string {
	return w.text(`SELECT COALESCE(owner_user_id::text, 'catalog') || '/' || (assigned_at IS NOT NULL) || '/' || verification FROM artists WHERE id = $1`, artistID)
}

func TestHandingACatalogArtistToAUser(t *testing.T) {
	w := newWorld(t)
	pool, ctx := w.h.pool, context.Background()
	art := w.catalogArtist("Assign")
	rel := w.release(art, "Old tapes", "public", "")

	// a song the team uploads through the real admin route is marked as the team's and charged to nobody
	code, b := call(w.r, "POST", "/admin/releases/"+rel+"/uploads", w.adminT, map[string]any{"title": "Team song", "filename": "a.mp3", "sizeBytes": 700, "rightsConfirmed": true})
	if code != 201 {
		t.Fatalf("team upload = %d %v", code, b)
	}
	if got := w.text(`SELECT by_team || '/' || quota_exempt FROM uploads WHERE id = $1`, b["id"]); got != "true/true" {
		t.Fatalf("team upload marked %s", got)
	}
	w.song(w.admin, art, rel, "Older", 300, true)
	storage := func() map[string]any {
		t.Helper()
		_, d := call(w.r, "GET", "/admin/artists/"+art, w.adminT, nil)
		return d["storage"].(map[string]any)
	}
	if s := storage(); s["usedBytes"] != float64(0) || s["teamUploadBytes"] != float64(1000) || s["quotaBytes"] != float64(0) {
		t.Fatalf("catalog storage = %v", s)
	}

	// refusals
	user := w.person()
	other := w.person()
	testutil.Artist(t, pool, other, testutil.Unique("Has"), testutil.GiB)
	banned, unverified := w.person(), testutil.User(t, pool, "user", false)
	w.exec(`UPDATE users SET banned_at = now() WHERE id = $1`, banned)
	for want, id := range map[string]string{"user_not_found": "00000000-0000-0000-0000-000000000000", "user_has_artist": other, "user_banned": banned, "email_unverified": unverified} {
		if _, b := w.assign(art, id); errCode(b) != want {
			t.Errorf("assign: %v, want %s", b, want)
		}
	}
	if code, b = w.assign(art, "not-an-id"); code != 404 || errCode(b) != "user_not_found" {
		t.Errorf("bad user id = %d %v", code, b)
	}
	if code, b = w.assign("art_nope", user); code != 404 || errCode(b) != "not_found" {
		t.Errorf("unknown artist = %d %v", code, b)
	}
	if code, _ = call(w.r, "POST", "/admin/artists/"+art+"/owner", w.adminT, map[string]any{"userId": user, "reason": "x"}); code != 422 {
		t.Errorf("short reason = %d", code)
	}
	if code, _ = call(w.r, "POST", "/admin/artists/"+art+"/owner", testutil.Token(t, user), map[string]any{"userId": user, "reason": "give it to me"}); code != 403 {
		t.Errorf("a user assigning = %d", code)
	}
	w.exec(`UPDATE artists SET banned_at = now() WHERE id = $1`, art)
	if _, b = w.assign(art, user); errCode(b) != "artist_banned" {
		t.Errorf("banned artist = %v", b)
	}
	w.exec(`UPDATE artists SET banned_at = NULL WHERE id = $1`, art)
	if code, b = w.unassign(art); code != 409 || errCode(b) != "not_owned" {
		t.Errorf("removing nobody = %d %v", code, b)
	}

	// the transfer
	catalogPicture := w.picture(w.admin, "avatar")
	w.exec(`UPDATE artists SET avatar_image_id = $2 WHERE id = $1`, art, catalogPicture)
	code, b = w.assign(art, user)
	if code != 200 || b["owner"].(map[string]any)["id"] != user || !strings.Contains(b["imageUrl"].(string), catalogPicture) {
		t.Fatalf("assign = %d %v (a user without a picture keeps the catalog one showing)", code, b)
	}
	if s := storage(); s["usedBytes"] != float64(0) || s["teamUploadBytes"] != float64(1000) || s["quotaBytes"] != float64(10*testutil.GiB) {
		t.Fatalf("storage after = %v", s)
	}
	if w.audit("artist.assign", art) != 1 || w.text(`SELECT details->>'userId' FROM admin_audit_log WHERE action = 'artist.assign' AND target_id = $1`, art) != user {
		t.Fatal("the transfer must be audited")
	}
	if _, b = w.assign(art, w.person()); errCode(b) != "already_owned" {
		t.Errorf("second owner = %v", b)
	}

	// the owner's own uploads count, even if their app claims to be the team
	uploads := upload.NewStore(pool)
	mine, err := uploads.Create(ctx, upload.NewUpload{UserID: user, ReleaseID: w.release(art, "New", "draft", ""), Title: "Mine", Filename: "m.mp3", SizeBytes: 250, TermsVersion: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if got := w.text(`SELECT by_team || '/' || quota_exempt FROM uploads WHERE id = $1`, mine.ID); got != "false/false" {
		t.Fatalf("an owner's upload marked %s", got)
	}
	s := storage()
	if s["usedBytes"] != float64(250) || s["teamUploadBytes"] != float64(1000) {
		t.Fatalf("storage with an own upload = %v", s)
	}
	parts := s["breakdown"].(map[string]any)
	sum := 0.0
	for _, name := range []string{"publishedAudio", "unpublishedAudio", "unfinishedUploads"} {
		sum += parts[name].(map[string]any)["bytes"].(float64)
	}
	if sum != 1250 {
		t.Fatalf("the audio parts = %v, want everything stored", sum)
	}

	// the welcome allowance is given once in the artist's life
	if code, b = w.unassign(art); code != 200 || b["owner"] != nil {
		t.Fatalf("remove = %d %v", code, b)
	}
	if code, b = w.assign(art, w.person()); code != 200 {
		t.Fatalf("assign again = %d %v", code, b)
	}
	if n := w.text(`SELECT count(*)::text FROM storage_grants WHERE artist_id = $1`, art); n != "1" {
		t.Fatalf("welcome grants = %s", n)
	}
	if s := storage(); s["usedBytes"] != float64(0) {
		t.Fatalf("a second owner is not charged for what was there: %v", s)
	}
}

func TestRemovingAnArtistFromAUserAndThenDeletingThem(t *testing.T) {
	w := newWorld(t)
	art := w.catalogArtist("Remove")
	user := w.person()
	if code, b := w.assign(art, user); code != 200 {
		t.Fatalf("assign = %d %v", code, b)
	}
	// everything the owner made: their picture, a public release with their cover and song, a draft, unfinished files
	face := w.picture(user, "avatar")
	w.exec(`UPDATE users SET avatar_image_id = $2 WHERE id = $1`, user, face)
	cover := w.picture(user, "cover")
	loose := w.picture(user, "cover") // uploaded and never used
	live := w.release(art, "Live", "public", cover)
	kept := w.song(user, art, live, "Kept", 100, false)
	draft := w.release(art, "Draft", "draft", "")
	for status, key := range map[string]string{"pending": "wip/pending", "failed": "wip/failed", "queued": "wip/queued", "processing": "wip/processing"} {
		w.blobs.Put(key, []byte("x"))
		w.exec(`INSERT INTO uploads (user_id, artist_id, release_id, track_id, title, artist_name, filename, size_bytes, original_key, status)
			VALUES ($1, $2, $3, 'trk_' || $4, 't', 'a', 'f.mp3', 1, $5, $4)`, user, art, draft, status, key)
	}
	w.exec(`UPDATE artists SET verification = 'pending', verification_message = 'me', verification_requested_at = now() WHERE id = $1`, art)

	if code, b := w.unassign(art); code != 409 || errCode(b) != "busy" {
		t.Fatalf("while a file is processing = %d %v", code, b)
	}
	w.exec(`UPDATE uploads SET status = 'queued' WHERE artist_id = $1 AND status = 'processing'`, art)
	code, b := w.unassign(art)
	if code != 200 || b["owner"] != nil || !strings.Contains(b["imageUrl"].(string), face) {
		t.Fatalf("remove = %d %v (the artist keeps the picture it showed)", code, b)
	}
	if got := w.shape(art); got != "catalog/false/unverified" {
		t.Fatalf("artist = %s", got)
	}
	if got := w.text(`SELECT visibility FROM albums WHERE id = $1`, live); got != "public" || !w.intact(kept) {
		t.Fatalf("releases stay as they are: %s", got)
	}
	if got := w.text(`SELECT string_agg(status, ',' ORDER BY status) FROM uploads WHERE artist_id = $1 AND status <> 'ready'`, art); got != "queued,queued" {
		t.Fatalf("unfinished uploads left = %s, want only the ones already waiting for the processor", got)
	}
	if _, there := w.blobs.Get("wip/pending"); there {
		t.Fatal("the unsent file must be removed from storage")
	}
	if !slices.Equal(w.transfers.users, []string{user}) || w.audit("artist.unassign", art) != 1 {
		t.Fatalf("transfers cancelled for %v", w.transfers.users)
	}

	// the former owner leaves later: nothing of the artist goes with them
	w.deleteUser(user)
	if !w.intact(kept) || !w.hasPicture(cover) || !w.hasPicture(face) {
		t.Fatalf("lost with the former owner: song %v, cover %v, picture %v", w.intact(kept), w.hasPicture(cover), w.hasPicture(face))
	}
	if w.hasPicture(loose) {
		t.Fatal("a picture nothing uses goes with its owner")
	}
	if got := w.shape(art); got != "catalog/false/unverified" || w.text(`SELECT avatar_image_id FROM artists WHERE id = $1`, art) != face {
		t.Fatalf("artist = %s", got)
	}
}

func TestAReceivedArtistGoesBackToTheCatalog(t *testing.T) {
	w := newWorld(t)
	art := w.catalogArtist("Return")
	catalogPicture := w.picture(w.admin, "avatar")
	w.exec(`UPDATE artists SET avatar_image_id = $2, verification = 'verified' WHERE id = $1`, art, catalogPicture)

	// there before the transfer: an album the team made
	teamCover := w.picture(w.admin, "cover")
	teamAlbum := w.release(art, "Team album", "public", teamCover)
	teamSong := w.song(w.admin, art, teamAlbum, "Team", 1000, true)

	user := w.person()
	if code, b := w.assign(art, user); code != 200 {
		t.Fatalf("assign = %d %v", code, b)
	}
	face := w.picture(user, "avatar")
	w.exec(`UPDATE users SET avatar_image_id = $2 WHERE id = $1`, user, face)
	// the user adds a song to the team's album, makes a release of their own, puts a cover of theirs on a team release
	theirsOnTeamAlbum := w.song(user, art, teamAlbum, "Bonus", 20, false)
	ownCover := w.picture(user, "cover")
	ownSingle := w.release(art, "My single", "public", ownCover)
	ownSong := w.song(user, art, ownSingle, "My song", 300, false)
	sharedCover := w.picture(user, "cover")
	teamDraft := w.release(art, "Team draft", "draft", sharedCover)
	teamDraftSong := w.song(w.admin, art, teamDraft, "Unreleased", 5, true)
	// a release where the team added a song to the user's: it stays, without the user's song
	mixed := w.release(art, "Mixed", "public", "")
	mixedTheirs := w.song(user, art, mixed, "Theirs", 40, false)
	mixedTeam := w.song(w.admin, art, mixed, "Added by the team", 6, true)
	w.blobs.Put("wip/mine", []byte("x"))
	w.exec(`INSERT INTO uploads (user_id, artist_id, release_id, track_id, title, artist_name, filename, size_bytes, original_key, status)
		VALUES ($1, $2, $3, $4, 't', 'a', 'f.mp3', 7, 'wip/mine', 'pending')`, user, art, teamAlbum, testutil.Unique("trk_wip"))

	// the preview says what is really deleted
	code, p := call(w.r, "GET", "/admin/users/"+user+"/deletion-preview", w.adminT, nil)
	a := p["artist"].(map[string]any)
	if code != 200 || a["returnsToCatalog"] != true || p["releases"] != float64(1) || p["tracks"] != float64(3) || p["uploads"] != float64(4) || p["storageBytes"] != float64(20+300+40+7) {
		t.Fatalf("preview = %d %v", code, p)
	}

	w.deleteUser(user)

	if got := w.shape(art); got != "catalog/false/verified" {
		t.Fatalf("artist = %s", got)
	}
	for name, s := range map[string]song{"team song": teamSong, "team draft song": teamDraftSong, "team song on the mixed release": mixedTeam} {
		if !w.intact(s) {
			t.Errorf("%s was deleted", name)
		}
	}
	for name, s := range map[string]song{"their bonus song": theirsOnTeamAlbum, "their single": ownSong, "their song on the mixed release": mixedTheirs} {
		if w.intact(s) {
			t.Errorf("%s is still there", name)
		}
	}
	if got := w.text(`SELECT string_agg(title || ':' || visibility, ', ' ORDER BY title) FROM albums WHERE artist_id = $1`, art); got != "Mixed:public, Team album:public, Team draft:draft" {
		t.Fatalf("releases = %s", got)
	}
	for name, want := range map[string]struct {
		id   string
		kept bool
	}{"catalog picture": {catalogPicture, true}, "team cover": {teamCover, true}, "their cover on a release that stays": {sharedCover, true},
		"their own photo": {face, false}, "the cover of their deleted single": {ownCover, false}} {
		if w.hasPicture(want.id) != want.kept {
			t.Errorf("%s kept = %v, want %v", name, !want.kept, want.kept)
		}
	}
	if _, there := w.blobs.Get("wip/mine"); there {
		t.Error("their unfinished upload is still in storage")
	}
	// and it can be handed to somebody again
	if code, b := w.assign(art, w.person()); code != 200 {
		t.Fatalf("assign again = %d %v", code, b)
	}
}

func TestAnArtistAUserCreatedStillGoesWithThem(t *testing.T) {
	w := newWorld(t)
	user := w.person()
	art := testutil.Artist(t, w.h.pool, user, testutil.Unique("Own"), testutil.GiB)
	cover := w.picture(user, "cover")
	rel := w.release(art, "Mine", "public", cover)
	theirs := w.song(user, art, rel, "One", 10, false)
	addedByTeam := w.song(w.admin, art, rel, "Two", 20, false)
	w.exec(`UPDATE uploads SET by_team = true WHERE id = $1`, addedByTeam.upload)

	_, p := call(w.r, "GET", "/admin/users/"+user+"/deletion-preview", w.adminT, nil)
	if a := p["artist"].(map[string]any); a["returnsToCatalog"] != false || p["releases"] != float64(1) || p["tracks"] != float64(2) || p["uploads"] != float64(2) || p["storageBytes"] != float64(30) {
		t.Fatalf("preview = %v", p)
	}
	w.deleteUser(user)
	if w.intact(theirs) || w.intact(addedByTeam) || w.hasPicture(cover) || w.text(`SELECT count(*)::text FROM artists WHERE id = $1`, art) != "0" {
		t.Fatal("an artist the user created goes with everything in it")
	}
}

func TestDeletingAnAdministratorKeepsTheCatalog(t *testing.T) {
	w := newWorld(t)
	art := w.catalogArtist("Admins")
	leaving := testutil.User(t, w.h.pool, "admin", true)
	face, cover, unused := w.picture(leaving, "avatar"), w.picture(leaving, "cover"), w.picture(leaving, "cover")
	w.exec(`UPDATE artists SET avatar_image_id = $2 WHERE id = $1`, art, face)
	rel := w.release(art, "Catalog", "public", cover)
	s := w.song(leaving, art, rel, "Song", 10, true)

	if err := w.h.Accounts.AdminDelete(context.Background(), leaving, nil); err != nil {
		t.Fatal(err)
	}
	if !w.intact(s) || !w.hasPicture(face) || !w.hasPicture(cover) || w.hasPicture(unused) {
		t.Fatalf("after the administrator left: song %v, picture %v, cover %v, unused %v", w.intact(s), w.hasPicture(face), w.hasPicture(cover), w.hasPicture(unused))
	}
}
