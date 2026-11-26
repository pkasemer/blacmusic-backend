package transfer

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/blacmusic/backend/internal/testutil"
	"github.com/blacmusic/backend/internal/upload"
	"github.com/blacmusic/backend/internal/youtube"
)

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// manualSong plans a one-song transfer for a song that is not on YouTube and returns it with its item.
func (e *env) manualSong(title string) (*Transfer, Item) {
	e.t.Helper()
	tr := e.create(ItemRequest{Title: title})
	return tr, tr.Items[0]
}

func TestManualSongsNeedNoYouTubeConnection(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	user := testutil.User(t, e.pool, "user", true)
	testutil.Artist(t, e.pool, user, testutil.Unique("Plain"), testutil.GiB)

	tr, err := e.m.Create(ctx, user, "", []ItemRequest{{Title: "Single one"}, {Title: "Album track", AlbumTitle: "My Album"}})
	if err != nil {
		t.Fatalf("manual songs without a connection: %v", err)
	}
	if tr.Status != StatusCollecting || len(tr.Items) != 2 {
		t.Fatalf("transfer = %s with %d songs", tr.Status, len(tr.Items))
	}
	for _, it := range tr.Items {
		if it.Status != ItemAwaitingAudio || it.AudioSource != "upload" || it.ReleaseID == "" {
			t.Fatalf("song = %+v", it)
		}
	}
	// a song of the channel still needs the connection
	if _, err := e.m.Cancel(ctx, user, tr.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Create(ctx, user, "", []ItemRequest{{VideoID: "demoMukeneAud"}}); !errors.Is(err, youtube.ErrNotConnected) {
		t.Fatalf("a YouTube song without a connection = %v, want ErrNotConnected", err)
	}
}

func TestRemovingAndReplacingTheFileOfASong(t *testing.T) {
	e := setup(t)
	e.m.Blobs = e.blobs
	ctx := context.Background()
	tr, it := e.manualSong("Mine")

	if _, err := e.m.RemoveAudio(ctx, e.user, tr.ID, it.ID); !errors.As(err, new(*ValidationError)) {
		t.Fatalf("removing a file that is not there = %v, want a validation error", err)
	}
	if _, err := e.m.RemoveAudio(ctx, e.user, tr.ID, "tri_nope"); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("unknown song = %v", err)
	}

	// a file that was only reserved or queued: removing deletes it and its stored bytes
	first := e.attach(tr, it, "queued")
	var key string
	if err := e.pool.QueryRow(ctx, `SELECT original_key FROM uploads WHERE id = $1`, first).Scan(&key); err != nil {
		t.Fatal(err)
	}
	e.blobs.Put(key, []byte("first file"))
	got, err := e.m.RemoveAudio(ctx, e.user, tr.ID, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if m := got.Items[0]; m.Status != ItemAwaitingAudio || m.UploadID != "" || m.AudioSource != "upload" {
		t.Fatalf("after removing = %+v", m)
	}
	if e.count(`SELECT count(*) FROM uploads WHERE id = $1`, first) != 0 {
		t.Fatal("the removed upload should be deleted")
	}
	if _, ok := e.blobs.Get(key); ok {
		t.Fatal("the removed file should be gone from storage")
	}

	// attaching a second file replaces the first
	a := e.attach(got, it, "queued")
	b := e.upload(it, "queued")
	if err := e.m.AttachAudio(ctx, e.user, tr.ID, it.ID, b); err != nil {
		t.Fatalf("replace: %v", err)
	}
	cur := e.get(tr.ID).Items[0]
	if cur.UploadID != b || cur.Status != ItemUploading {
		t.Fatalf("after replacing = %+v", cur)
	}
	if e.count(`SELECT count(*) FROM uploads WHERE id = $1`, a) != 0 {
		t.Fatal("the replaced upload should be deleted")
	}

	// a replacement that is not valid leaves the current file alone
	other := e.newReleaseUpload("queued")
	if err := e.m.AttachAudio(ctx, e.user, tr.ID, it.ID, other); !errors.As(err, new(*ValidationError)) {
		t.Fatalf("an upload for another release = %v, want a validation error", err)
	}
	if e.get(tr.ID).Items[0].UploadID != b || e.count(`SELECT count(*) FROM uploads WHERE id = $1`, b) != 1 {
		t.Fatal("a rejected replacement must keep the current file")
	}
}

// newReleaseUpload makes an upload on a release that belongs to no transfer.
func (e *env) newReleaseUpload(status string) string {
	e.t.Helper()
	rel := testutil.Release(e.t, e.pool, e.artist, "Elsewhere", "single", "draft")
	return e.upload(Item{ReleaseID: rel, Title: "Elsewhere"}, status)
}

func TestRemovingAFinishedFileDeletesItsDraftTrack(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	tr, it := e.manualSong("Done already")
	up := e.attach(tr, it, "ready")
	var track string
	if err := e.pool.QueryRow(ctx, `SELECT track_id FROM uploads WHERE id = $1`, up).Scan(&track); err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT count(*) FROM tracks WHERE id = $1`, track) != 1 {
		t.Fatal("setup: the track should exist")
	}
	got, err := e.m.RemoveAudio(ctx, e.user, tr.ID, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Items[0].Status != ItemAwaitingAudio || got.Items[0].UploadID != "" {
		t.Fatalf("after removing = %+v", got.Items[0])
	}
	if e.count(`SELECT count(*) FROM tracks WHERE id = $1`, track) != 0 || e.count(`SELECT count(*) FROM uploads WHERE id = $1`, up) != 0 {
		t.Fatal("the draft track and its upload should be deleted")
	}
	// and the song takes a new file
	e.attach(got, it, "queued")
	if e.get(tr.ID).Items[0].Status != ItemUploading {
		t.Fatal("the song should take a new file")
	}
}

func TestAFileBeingProcessedCannotBeRemovedOrReplaced(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	tr, it := e.manualSong("Busy")
	busy := e.attach(tr, it, "processing")

	if _, err := e.m.RemoveAudio(ctx, e.user, tr.ID, it.ID); !errors.Is(err, ErrAudioBusy) {
		t.Fatalf("remove while processing = %v, want ErrAudioBusy", err)
	}
	next := e.upload(it, "queued")
	if err := e.m.AttachAudio(ctx, e.user, tr.ID, it.ID, next); !errors.Is(err, ErrAudioBusy) {
		t.Fatalf("replace while processing = %v, want ErrAudioBusy", err)
	}
	cur := e.get(tr.ID).Items[0]
	if cur.UploadID != busy || e.count(`SELECT count(*) FROM uploads WHERE id = $1`, busy) != 1 {
		t.Fatalf("the file being processed must stay: %+v", cur)
	}
}

func TestFilesCanOnlyBeRemovedWhileCollecting(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	tr, it := e.manualSong("Locked in")
	e.attach(tr, it, "queued")
	if _, err := e.m.Submit(ctx, e.user, tr.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.RemoveAudio(ctx, e.user, tr.ID, it.ID); !errors.Is(err, ErrWrongState) {
		t.Fatalf("remove after submitting = %v, want ErrWrongState", err)
	}
}

func TestRemovingAFileOverHTTP(t *testing.T) {
	e := setup(t)
	r := e.router()
	tr, it := e.manualSong("Over the wire")
	path := "/transfers/" + tr.ID + "/items/" + it.ID + "/audio"

	if code, b := e.call(r, "DELETE", path, "", nil); code != 422 {
		t.Fatalf("nothing to remove = %d %v, want 422", code, b)
	}
	e.attach(tr, it, "queued")
	code, b := e.call(r, "DELETE", path, "", nil)
	if code != 200 {
		t.Fatalf("remove = %d %v", code, b)
	}
	first := b["items"].([]any)[0].(map[string]any)
	hint, _ := first["upload"].(map[string]any)
	if first["status"] != "awaiting_audio" || hint["releaseId"] != it.ReleaseID {
		t.Fatalf("after removing = %v (the app needs the upload hint to add a new file)", first)
	}
	if code, b := e.call(r, "DELETE", "/transfers/"+tr.ID+"/items/tri_nope/audio", "", nil); code != 404 {
		t.Fatalf("unknown song = %d %v, want 404", code, b)
	}

	e.attach(e.get(tr.ID), it, "processing")
	if code, b := e.call(r, "DELETE", path, "", nil); code != 409 || errCode(b) != "busy" {
		t.Fatalf("remove while processing = %d %v, want 409 busy", code, b)
	}
}

func (e *env) text(query string, args ...any) string {
	e.t.Helper()
	var s string
	if err := e.pool.QueryRow(context.Background(), query, args...).Scan(&s); err != nil {
		e.t.Fatal(err)
	}
	return s
}

func strp(s string) *string { return &s }

func TestEditingASongBeforeSubmitting(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	tr := e.create(ItemRequest{Title: "Old title"}, ItemRequest{Title: "On the album", AlbumTitle: "The Album"})
	single, album := tr.Items[0], tr.Items[1]
	up := e.attach(tr, single, "queued")
	yes := true

	got, err := e.m.UpdateItem(ctx, e.user, tr.ID, single.ID, ItemPatch{Title: strp("  New title "), Genre: strp("bongo flava"), Explicit: &yes})
	if err != nil {
		t.Fatal(err)
	}
	if it := got.Items[0]; it.Title != "New title" || it.Genre != "Bongo Flava" || !it.Explicit {
		t.Fatalf("item = %+v", it)
	}
	if e.text(`SELECT title FROM albums WHERE id = $1`, single.ReleaseID) != "New title" || e.text(`SELECT genre FROM albums WHERE id = $1`, single.ReleaseID) != "Bongo Flava" {
		t.Fatal("the single's draft release should follow the new title and genre")
	}
	if e.text(`SELECT title || '|' || genre || '|' || explicit::text FROM uploads WHERE id = $1`, up) != "New title|Bongo Flava|true" {
		t.Fatal("the file's upload should carry the new details")
	}
	// a single field leaves the others alone
	got, err = e.m.UpdateItem(ctx, e.user, tr.ID, single.ID, ItemPatch{Genre: strp("")})
	if err != nil || got.Items[0].Genre != "" || got.Items[0].Title != "New title" || !got.Items[0].Explicit {
		t.Fatalf("clearing the genre = %+v %v", got.Items[0], err)
	}

	// an album song keeps the album's own title
	if _, err := e.m.UpdateItem(ctx, e.user, tr.ID, album.ID, ItemPatch{Title: strp("Renamed track")}); err != nil {
		t.Fatal(err)
	}
	if e.text(`SELECT title FROM albums WHERE id = $1`, album.ReleaseID) != "The Album" {
		t.Fatal("renaming a song must not rename its album")
	}
	// the hint the app uses for the next upload shows the new title
	if v := ToView(e.get(tr.ID)); v.Items[1].Upload == nil || v.Items[1].Upload.Title != "Renamed track" {
		t.Fatalf("upload hint = %+v", v.Items[1].Upload)
	}

	// a song whose file has already been processed: the draft track follows
	done := e.create2("Finished song")
	doneUp := e.attach(done, done.Items[0], "ready")
	if _, err := e.m.UpdateItem(ctx, e.user, done.ID, done.Items[0].ID, ItemPatch{Title: strp("Finished, renamed")}); err != nil {
		t.Fatal(err)
	}
	if e.text(`SELECT t.title FROM tracks t JOIN uploads u ON u.track_id = t.id WHERE u.id = $1`, doneUp) != "Finished, renamed" {
		t.Fatal("the draft track should follow the new title")
	}
}

// create2 starts a transfer for another artist-less manual song of the same user once the first one is done.
func (e *env) create2(title string) *Transfer {
	e.t.Helper()
	e.pool.Exec(context.Background(), `UPDATE transfers SET status = 'cancelled' WHERE user_id = $1 AND status = 'collecting'`, e.user) //nolint:errcheck
	return e.create(ItemRequest{Title: title})
}

func TestEditingASongIsCheckedAndOnlyWhileCollecting(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	tr := e.create(ItemRequest{Title: "One"}, ItemRequest{Title: "Two"})
	one, two := tr.Items[0], tr.Items[1]

	for name, p := range map[string]ItemPatch{
		"nothing":       {},
		"empty title":   {Title: strp("   ")},
		"long title":    {Title: strp(string(make([]byte, 201)) + "x")},
		"unknown genre": {Genre: strp("not a genre")},
	} {
		if _, err := e.m.UpdateItem(ctx, e.user, tr.ID, one.ID, p); !errors.As(err, new(*ValidationError)) {
			t.Errorf("%s = %v, want a validation error", name, err)
		}
	}
	if _, err := e.m.UpdateItem(ctx, e.user, tr.ID, "tri_nope", ItemPatch{Title: strp("x")}); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("unknown song = %v", err)
	}
	if err := e.m.DropItem(ctx, e.user, tr.ID, two.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.UpdateItem(ctx, e.user, tr.ID, two.ID, ItemPatch{Title: strp("x")}); !errors.As(err, new(*ValidationError)) {
		t.Fatalf("a dropped song = %v, want a validation error", err)
	}
	e.attach(tr, one, "queued")
	if _, err := e.m.Submit(ctx, e.user, tr.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.UpdateItem(ctx, e.user, tr.ID, one.ID, ItemPatch{Title: strp("late")}); !errors.Is(err, ErrWrongState) {
		t.Fatalf("after submitting = %v, want ErrWrongState", err)
	}
}

func TestEditingASongOverHTTP(t *testing.T) {
	e := setup(t)
	r := e.router()
	tr, it := e.manualSong("Before")
	path := "/transfers/" + tr.ID + "/items/" + it.ID

	code, b := e.call(r, "PATCH", path, "", map[string]any{"title": "After", "genre": "Gospel", "explicit": true})
	if code != 200 {
		t.Fatalf("patch = %d %v", code, b)
	}
	first := b["items"].([]any)[0].(map[string]any)
	if first["title"] != "After" || first["genre"] != "Gospel" || first["explicit"] != true {
		t.Fatalf("item = %v", first)
	}
	if code, b := e.call(r, "PATCH", path, "", map[string]any{}); code != 422 {
		t.Fatalf("empty patch = %d %v", code, b)
	}
	if code, b := e.call(r, "PATCH", path, "", map[string]any{"genre": "nope"}); code != 422 {
		t.Fatalf("bad genre = %d %v", code, b)
	}
	if code, b := e.call(r, "PATCH", "/transfers/"+tr.ID+"/items/tri_nope", "", map[string]any{"title": "x"}); code != 404 {
		t.Fatalf("unknown song = %d %v", code, b)
	}
	e.attach(tr, it, "queued")
	e.m.Submit(context.Background(), e.user, tr.ID) //nolint:errcheck
	if code, b := e.call(r, "PATCH", path, "", map[string]any{"title": "late"}); code != 409 || errCode(b) != "wrong_state" {
		t.Fatalf("after submit = %d %v", code, b)
	}
}

func TestSongsOfATransferDoNotSendReleaseReadyNotices(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	var events []upload.ReadyEvent
	e.uploads.OnReady = func(_ context.Context, _ pgx.Tx, ev upload.ReadyEvent) error {
		events = append(events, ev)
		return nil
	}
	_, it := e.manualSong("In a transfer")
	id := e.upload(it, "processing")
	u, err := e.uploads.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	res := upload.Result{DurationMs: 1000, Renditions: []upload.Rendition{{Quality: "medium", MediaKey: "tracks/" + u.TrackID + "/medium.m4a", BitrateKbps: 128, SizeBytes: 10}}}
	if err := e.uploads.Finish(ctx, u, res); err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("a transfer reports on its own, yet %+v", events)
	}
}

type fakeOptions map[string]bool

func (f fakeOptions) Enabled(_ context.Context, id string) (bool, error) { return f[id], nil }

func TestImportOptionsDecideWhatACreateMayUse(t *testing.T) {
	e := setup(t)
	r := e.router()
	ctx := context.Background()
	var disabled *ImportDisabledError

	// YouTube off, manual on (production today): a channel song is refused, a manual one is fine
	e.m.Options = fakeOptions{"manual": true, "youtube": false}
	if _, err := e.m.Create(ctx, e.user, "", []ItemRequest{{VideoID: "demoMukeneAud"}}); !errors.As(err, &disabled) || disabled.Option != "youtube" {
		t.Fatalf("a YouTube song with youtube off = %v, want ImportDisabledError(youtube)", err)
	}
	code, b := e.call(r, "POST", "/transfers", "", map[string]any{"items": []map[string]any{{"videoId": "demoMukeneAud"}}})
	if code != 403 || errCode(b) != "import_disabled" {
		t.Fatalf("over HTTP = %d %v, want 403 import_disabled", code, b)
	}
	tr, err := e.m.Create(ctx, e.user, "", []ItemRequest{{Title: "By hand"}})
	if err != nil {
		t.Fatalf("manual with manual on: %v", err)
	}
	if _, err := e.m.Cancel(ctx, e.user, tr.ID); err != nil {
		t.Fatal(err)
	}

	// manual off: nothing by hand
	e.m.Options = fakeOptions{"manual": false, "youtube": true}
	if _, err := e.m.Create(ctx, e.user, "", []ItemRequest{{Title: "By hand"}}); !errors.As(err, &disabled) || disabled.Option != "manual" {
		t.Fatalf("manual with manual off = %v, want ImportDisabledError(manual)", err)
	}
	if code, b = e.call(r, "POST", "/transfers", "", map[string]any{"items": []map[string]any{{"title": "By hand"}}}); code != 403 || errCode(b) != "import_disabled" {
		t.Fatalf("manual over HTTP = %d %v", code, b)
	}
	// a mixed request needs both
	e.m.Options = fakeOptions{"manual": true, "youtube": false}
	if _, err := e.m.Create(ctx, e.user, "", []ItemRequest{{VideoID: "demoMukeneAud"}, {Title: "By hand"}}); !errors.As(err, &disabled) {
		t.Fatalf("mixed with youtube off = %v", err)
	}
	// everything on, or no options configured: as before
	e.m.Options = fakeOptions{"manual": true, "youtube": true}
	if tr, err = e.m.Create(ctx, e.user, "", []ItemRequest{{VideoID: "demoMukeneAud"}}); err != nil {
		t.Fatalf("youtube on: %v", err)
	}
	e.m.Cancel(ctx, e.user, tr.ID) //nolint:errcheck
	e.m.Options = nil
	if _, err = e.m.Create(ctx, e.user, "", []ItemRequest{{Title: "By hand"}}); err != nil {
		t.Fatalf("no options configured: %v", err)
	}
}
