package transfer

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/blobtest"
	"github.com/blacmusic/backend/internal/release"
	"github.com/blacmusic/backend/internal/testutil"
	"github.com/blacmusic/backend/internal/upload"
	"github.com/blacmusic/backend/internal/youtube"
)

type env struct {
	t        *testing.T
	pool     *pgxpool.Pool
	m        *Manager
	runner   *Runner
	yt       *youtube.Store
	uploads  *upload.Store
	releases *release.Store
	blobs    *blobtest.Mem
	fetches  []string
	fetchErr error // returned by the thumbnail fetch while set
	user     string
	artist   string
}

func pngBytes() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 400, 400))
	for x := 0; x < 400; x++ {
		for y := 0; y < 400; y++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 120, 255})
		}
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return b.Bytes()
}

func setup(t *testing.T) *env {
	t.Helper()
	pool := testutil.Pool(t)
	ctx := context.Background()
	// the queue is global; start every test from an empty one
	if _, err := pool.Exec(ctx, `DELETE FROM transfers`); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, pool: pool, blobs: blobtest.New(), yt: youtube.NewStore(pool), uploads: upload.NewStore(pool)}
	e.releases = release.NewStore(pool, e.blobs, "http://api.test")
	e.m = NewManager(pool, e.yt, e.releases, e.uploads)
	e.runner = NewRunner(e.m, pool, e.blobs, e.releases, func(_ context.Context, u string) ([]byte, error) {
		e.fetches = append(e.fetches, u)
		if e.fetchErr != nil {
			return nil, e.fetchErr
		}
		return pngBytes(), nil
	}, "w1", 1)
	e.user, e.artist = e.newArtist()
	return e
}

// newArtist makes an artist whose YouTube channel is connected and synced with the fake channel.
func (e *env) newArtist() (user, artistID string) {
	e.t.Helper()
	user = testutil.User(e.t, e.pool, "user", true)
	artistID = testutil.Artist(e.t, e.pool, user, testutil.Unique("Akeine"), 10*testutil.GiB)
	f := youtube.NewFake("http://x")
	if err := e.yt.Upsert(context.Background(), user, &f.Chan, []byte("sealed"), youtube.ReadOnlyScope); err != nil {
		e.t.Fatal(err)
	}
	for i := range f.List {
		f.List[i].ThumbnailURL = "https://i.ytimg.com/vi/" + f.List[i].ID + "/maxresdefault.jpg"
	}
	if err := e.yt.ReplaceVideos(context.Background(), user, f.List); err != nil {
		e.t.Fatal(err)
	}
	return user, artistID
}

func (e *env) create(items ...ItemRequest) *Transfer {
	e.t.Helper()
	tr, err := e.m.Create(context.Background(), e.user, "", items)
	if err != nil {
		e.t.Fatalf("create: %v", err)
	}
	return tr
}

func (e *env) get(id string) *Transfer {
	e.t.Helper()
	tr, err := e.m.Get(context.Background(), id, e.user)
	if err != nil {
		e.t.Fatal(err)
	}
	return tr
}

// upload inserts an upload row with the given status on the item's release.
func (e *env) upload(it Item, status string) string {
	e.t.Helper()
	var id string
	if status == "ready" {
		_, id = testutil.ReadyTrack(e.t, e.pool, e.user, e.artist, it.ReleaseID, it.Title, 1, "draft")
		return id
	}
	err := e.pool.QueryRow(context.Background(), `
		INSERT INTO uploads (user_id, artist_id, release_id, title, artist_name, filename, size_bytes, original_key, status, error)
		VALUES ($1, $2, $3, $4, 'x', 'f.flac', 1, 'originals/' || gen_random_uuid(), $5, CASE WHEN $5 = 'failed' THEN 'not audio' ELSE '' END) RETURNING id`,
		e.user, e.artist, it.ReleaseID, it.Title, status).Scan(&id)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) attach(tr *Transfer, it Item, status string) string {
	e.t.Helper()
	id := e.upload(it, status)
	if err := e.m.AttachAudio(context.Background(), e.user, tr.ID, it.ID, id); err != nil {
		e.t.Fatalf("attach: %v", err)
	}
	return id
}

func (e *env) item(tr *Transfer, videoID string) Item {
	e.t.Helper()
	for _, it := range tr.Items {
		if it.VideoID == videoID {
			return it
		}
	}
	e.t.Fatalf("no item for %s", videoID)
	return Item{}
}

func (e *env) tick() { e.runner.Tick(context.Background()) }

func statuses(tr *Transfer) map[string]string {
	out := map[string]string{}
	for _, it := range tr.Items {
		out[it.Title] = it.Status
	}
	return out
}

// --------------------------------------------------------------------- tests

func TestPlanning(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	// an existing song in the catalog
	rel := testutil.Release(t, e.pool, e.artist, "old", "single", "public")
	testutil.ReadyTrack(t, e.pool, e.user, e.artist, rel, "Wanted", 1, "public")

	tr, err := e.m.Create(ctx, e.user, "key-1", []ItemRequest{
		{VideoID: "demoMukeneAud"},
		{VideoID: "demoMukeneLyr"}, // another video of the same song
		{VideoID: "demoBetterVis", AlbumTitle: "Demo EP", Genre: "Afropop"},
		{VideoID: "demoBlessedLyr", AlbumTitle: "Demo EP", Explicit: true},
		{VideoID: "demoWantedAud"}, // already in the catalog
	})
	if err != nil {
		t.Fatal(err)
	}
	if tr.Status != StatusCollecting || len(tr.Items) != 5 {
		t.Fatalf("transfer = %+v", tr)
	}
	by := map[string]Item{}
	for _, it := range tr.Items {
		by[it.VideoID] = it
	}
	mk := by["demoMukeneAud"]
	if mk.Title != "Mukene" || mk.ReleaseType != "single" || mk.Status != ItemAwaitingAudio || mk.ReleaseID == "" || mk.ThumbnailURL == "" {
		t.Fatalf("Mukene = %+v", mk)
	}
	if by["demoMukeneLyr"].Status != ItemSkipped || by["demoMukeneLyr"].DuplicateOf == "" {
		t.Fatalf("the lyric video of the same song must be skipped: %+v", by["demoMukeneLyr"])
	}
	if by["demoWantedAud"].Status != ItemSkipped || by["demoWantedAud"].DuplicateOf != "Wanted" {
		t.Fatalf("Wanted is in the catalog: %+v", by["demoWantedAud"])
	}
	b, bl := by["demoBetterVis"], by["demoBlessedLyr"]
	if b.ReleaseType != "album" || b.ReleaseID == "" || b.ReleaseID != bl.ReleaseID || b.TrackNumber == nil || *b.TrackNumber != 1 || *bl.TrackNumber != 2 || !bl.Explicit {
		t.Fatalf("album items = %+v / %+v", b, bl)
	}
	// drafts exist: one single, one album with the chosen title and an earliest-upload date
	var typ, title, vis, date string
	if err := e.pool.QueryRow(ctx, `SELECT release_type, title, visibility, to_char(release_date, 'YYYY-MM-DD') FROM albums WHERE id = $1`, b.ReleaseID).Scan(&typ, &title, &vis, &date); err != nil {
		t.Fatal(err)
	}
	if typ != "album" || title != "Demo EP" || vis != "draft" || date == "" {
		t.Fatalf("album = %s %s %s %s", typ, title, vis, date)
	}
	if mk.ReleaseDate == "" {
		t.Fatal("single has no release date")
	}

	// idempotent: the same key returns the same transfer and creates nothing more
	var before int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM albums WHERE artist_id = $1`, e.artist).Scan(&before) //nolint:errcheck
	again, err := e.m.Create(ctx, e.user, "key-1", []ItemRequest{{VideoID: "demoMukeneAud"}})
	if err != nil || again.ID != tr.ID {
		t.Fatalf("idempotent create = %v %v", again, err)
	}
	var after int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM albums WHERE artist_id = $1`, e.artist).Scan(&after) //nolint:errcheck
	if before != after {
		t.Fatal("a repeated key created more releases")
	}
	// one open transfer per artist
	if _, err := e.m.Create(ctx, e.user, "key-2", []ItemRequest{{VideoID: "demoBlessedLyr"}}); !errors.Is(err, ErrActive) {
		t.Fatalf("second open transfer = %v", err)
	}
	if _, err := e.m.Cancel(ctx, e.user, tr.ID); err != nil {
		t.Fatal(err)
	}

	var ve *ValidationError
	if _, err := e.m.Create(ctx, e.user, "", []ItemRequest{{VideoID: "nope"}}); !errors.As(err, &ve) {
		t.Fatalf("unknown video = %v", err)
	}
	if _, err := e.m.Create(ctx, e.user, "", []ItemRequest{{VideoID: "demoWantedAud"}}); !errors.As(err, &ve) {
		t.Fatalf("everything already imported = %v", err)
	}
	if _, err := e.m.Create(ctx, e.user, "", nil); !errors.As(err, &ve) {
		t.Fatalf("no items = %v", err)
	}
	if _, err := e.m.Create(ctx, e.user, "", []ItemRequest{{VideoID: "demoBlessedLyr", ReleaseType: "ep"}}); !errors.As(err, &ve) {
		t.Fatalf("bad type = %v", err)
	}
	if _, err := e.m.Create(ctx, e.user, "", []ItemRequest{{VideoID: "demoBlessedLyr", Genre: "Not A Genre"}}); err == nil {
		t.Fatal("invalid genre accepted")
	}
	var n int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM transfers WHERE user_id = $1 AND status = 'collecting'`, e.user).Scan(&n) //nolint:errcheck
	if n != 0 {
		t.Fatalf("a failed plan left %d open transfers behind", n)
	}

	// no profile / not connected
	loner := testutil.User(t, e.pool, "user", true)
	if _, err := e.m.Create(ctx, loner, "", []ItemRequest{{VideoID: "x"}}); !errors.Is(err, ErrNoArtist) {
		t.Fatalf("no artist = %v", err)
	}
	testutil.Artist(t, e.pool, loner, testutil.Unique("a"), 1<<30)
	if _, err := e.m.Create(ctx, loner, "", []ItemRequest{{VideoID: "x"}}); !errors.Is(err, youtube.ErrNotConnected) {
		t.Fatalf("not connected = %v", err)
	}
}

func TestAttachAndSubmit(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	tr := e.create(ItemRequest{VideoID: "demoMukeneAud"}, ItemRequest{VideoID: "demoBlessedLyr"})
	mk, bl := e.item(tr, "demoMukeneAud"), e.item(tr, "demoBlessedLyr")

	var ve *ValidationError
	other := testutil.Release(t, e.pool, e.artist, "elsewhere", "single", "draft")
	wrong := e.upload(Item{ReleaseID: other, Title: "x"}, "pending")
	if err := e.m.AttachAudio(ctx, e.user, tr.ID, mk.ID, wrong); !errors.As(err, &ve) {
		t.Fatalf("upload on another release = %v", err)
	}
	if err := e.m.AttachAudio(ctx, e.user, tr.ID, mk.ID, e.upload(mk, "failed")); !errors.As(err, &ve) {
		t.Fatalf("failed upload = %v", err)
	}
	// nothing attached yet
	var missing *AudioMissingError
	if _, err := e.m.Submit(ctx, e.user, tr.ID); !errors.As(err, &missing) || len(missing.ItemIDs) != 2 {
		t.Fatalf("submit with no audio = %v", err)
	}
	pending := e.attach(tr, mk, "pending")
	e.attach(tr, bl, "queued")
	// the same upload cannot serve two songs
	if err := e.m.AttachAudio(ctx, e.user, tr.ID, bl.ID, pending); !errors.As(err, &ve) {
		t.Fatalf("one upload for two songs = %v", err)
	}
	// a file that has not finished uploading blocks the submit
	if _, err := e.m.Submit(ctx, e.user, tr.ID); !errors.As(err, &missing) || !slices.Equal(missing.ItemIDs, []string{mk.ID}) {
		t.Fatalf("submit with an unfinished upload = %v", err)
	}
	// live per-item state while collecting
	v := ToView(e.get(tr.ID))
	got := map[string]string{}
	for _, it := range v.Items {
		got[it.Title] = it.Status
	}
	if got["Mukene"] != ItemUploading || got["Blessed"] != itemWaiting || v.Status != StatusCollecting || v.Queue != nil {
		t.Fatalf("view while collecting = %v queue=%v", got, v.Queue)
	}
	// finish the upload; drop nothing; submit
	e.pool.Exec(ctx, `UPDATE uploads SET status = 'queued' WHERE id = $1`, pending) //nolint:errcheck
	sub, err := e.m.Submit(ctx, e.user, tr.ID)
	if err != nil || sub.Status != StatusQueued || sub.QueuePosition != 1 || sub.QueueLength != 1 {
		t.Fatalf("submit = %+v %v", sub, err)
	}
	if err := e.m.AttachAudio(ctx, e.user, tr.ID, mk.ID, pending); !errors.Is(err, ErrWrongState) {
		t.Fatalf("attach after submit = %v", err)
	}
	if _, err := e.m.Submit(ctx, e.user, tr.ID); !errors.Is(err, ErrWrongState) {
		t.Fatalf("second submit = %v", err)
	}

	// dropping a song while collecting
	e2 := setup(t)
	t2 := e2.create(ItemRequest{VideoID: "demoMukeneAud"}, ItemRequest{VideoID: "demoBlessedLyr"})
	e2.attach(t2, e2.item(t2, "demoMukeneAud"), "queued")
	if err := e2.m.DropItem(ctx, e2.user, t2.ID, e2.item(t2, "demoBlessedLyr").ID); err != nil {
		t.Fatal(err)
	}
	if s, err := e2.m.Submit(ctx, e2.user, t2.ID); err != nil || s.Status != StatusQueued {
		t.Fatalf("submit after drop = %v %v", s, err)
	}
	if err := e2.m.DropItem(ctx, e2.user, t2.ID, e2.item(t2, "demoMukeneAud").ID); !errors.Is(err, ErrWrongState) {
		t.Fatalf("drop after submit = %v", err)
	}
	// another user cannot see it
	if _, err := e.m.Get(ctx, tr.ID, testutil.User(t, e.pool, "user", true)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("someone else's transfer = %v", err)
	}
}

func TestHappyPathThroughTheQueue(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	tr := e.create(ItemRequest{VideoID: "demoMukeneAud"}, ItemRequest{VideoID: "demoBetterVis", AlbumTitle: "Demo EP"}, ItemRequest{VideoID: "demoBlessedLyr", AlbumTitle: "Demo EP"})
	for _, it := range tr.Items {
		e.attach(tr, it, "ready")
	}
	if _, err := e.m.Submit(ctx, e.user, tr.ID); err != nil {
		t.Fatal(err)
	}
	e.tick() // claims, covers, publishes, completes
	done := e.get(tr.ID)
	if done.Status != StatusCompleted || done.FinishedAt == nil || done.Attempt != 1 {
		t.Fatalf("transfer = %s attempt %d (%v)", done.Status, done.Attempt, statuses(done))
	}
	for _, it := range done.Items {
		if it.Status != ItemDone || it.TrackID == "" || it.CoverImageID == "" {
			t.Fatalf("item = %+v", it)
		}
	}
	// the single and the album are public, with covers and the tracks inside
	var pub, covered int
	e.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE visibility = 'public'), count(*) FILTER (WHERE cover_image_id IS NOT NULL AND artwork_url <> '')
		FROM albums WHERE id IN (SELECT release_id FROM transfer_items WHERE transfer_id = $1)`, tr.ID).Scan(&pub, &covered) //nolint:errcheck
	if pub != 2 || covered != 2 {
		t.Fatalf("public releases %d, with cover %d (want 2 and 2)", pub, covered)
	}
	var tracks int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM tracks WHERE visibility = 'public' AND id IN (SELECT track_id FROM transfer_items WHERE transfer_id = $1)`, tr.ID).Scan(&tracks) //nolint:errcheck
	if tracks != 3 {
		t.Fatalf("public tracks = %d", tracks)
	}
	if len(e.fetches) != 2 { // one cover per release, not per song
		t.Fatalf("thumbnails fetched: %v", e.fetches)
	}
	v := ToView(done)
	if v.Progress.Percent != 100 || v.Progress.ItemsDone != 3 || v.Progress.ItemsTotal != 3 || v.Queue != nil {
		t.Fatalf("view = %+v", v.Progress)
	}
	// finished work is not repeated and the slot is free again
	e.tick()
	if len(e.fetches) != 2 {
		t.Fatal("a completed transfer was worked on again")
	}
	if _, err := e.m.Create(ctx, e.user, "", []ItemRequest{{VideoID: "demoWantedAud"}}); err != nil {
		t.Fatalf("new transfer after completion: %v", err)
	}
}

func TestUploadsWaitForTheirTurn(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	tr := e.create(ItemRequest{VideoID: "demoMukeneAud"})
	it := e.item(tr, "demoMukeneAud")
	up := e.upload(it, "queued") // not attached yet: it is for the transfer's release, so it is held back all the same

	claim := func() *upload.Upload {
		u, err := e.uploads.Claim(ctx, time.Hour, 3)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	// other queued uploads of the test database may exist: ours must not be the one claimed
	for u := claim(); u != nil; u = claim() {
		if u.ID == up {
			t.Fatal("an upload of a collecting transfer was handed to the transcoder")
		}
		e.uploads.Requeue(ctx, u.ID) //nolint:errcheck
		break
	}
	if err := e.m.AttachAudio(ctx, e.user, tr.ID, it.ID, up); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Submit(ctx, e.user, tr.ID); err != nil {
		t.Fatal(err)
	}
	for u := claim(); u != nil; u = claim() {
		if u.ID == up {
			t.Fatal("an upload of a queued transfer was handed to the transcoder")
		}
		e.uploads.Requeue(ctx, u.ID) //nolint:errcheck
		break
	}
	// claim the transfer (it then waits on the upload, which is not ready)
	e.tick()
	if got := e.get(tr.ID); got.Status != StatusInProgress {
		t.Fatalf("status = %s", got.Status)
	}
	var claimed bool
	for i := 0; i < 50 && !claimed; i++ {
		u := claim()
		if u == nil {
			break
		}
		if u.ID == up {
			claimed = true
		} else {
			e.uploads.Requeue(ctx, u.ID) //nolint:errcheck
		}
	}
	if !claimed {
		t.Fatal("once its transfer runs, the upload must be available to the transcoder")
	}
}

func TestQueueIsFairAndLimited(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	u2, a2 := e.newArtist()
	u3, a3 := e.newArtist()
	mk := func(user, artist string) (*Transfer, string) {
		tr, err := e.m.Create(ctx, user, "", []ItemRequest{{VideoID: "demoMukeneAud"}})
		if err != nil {
			t.Fatal(err)
		}
		it := tr.Items[0]
		uid := e.pool
		_ = uid
		var up string
		_, up = testutil.ReadyTrack(t, e.pool, user, artist, it.ReleaseID, it.Title, 1, "draft")
		if err := e.m.AttachAudio(ctx, user, tr.ID, it.ID, up); err != nil {
			t.Fatal(err)
		}
		sub, err := e.m.Submit(ctx, user, tr.ID)
		if err != nil {
			t.Fatal(err)
		}
		return sub, user
	}
	// submit in order: artist 3 first, then 1, then 2
	t3, _ := mk(u3, a3)
	t1, _ := mk(e.user, e.artist)
	t2, _ := mk(u2, a2)
	if t3.QueuePosition != 1 || t1.QueuePosition != 2 || t2.QueuePosition != 3 {
		t.Fatalf("positions = %d %d %d", t3.QueuePosition, t1.QueuePosition, t2.QueuePosition)
	}

	// concurrency 1: a tick starts only the oldest. Make its work fail so it goes to "retrying".
	e.fetchErr = errors.New("network down")
	e.tick()
	if g, _ := e.m.Get(ctx, t3.ID, u3); g.Status != StatusRetrying {
		t.Fatalf("oldest = %s", g.Status)
	}
	if g, _ := e.m.Get(ctx, t1.ID, e.user); g.Status != StatusQueued {
		t.Fatalf("second started in the same tick although the limit is 1: %s", g.Status)
	}
	e.tick() // the oldest is backing off, so the next in line goes now
	if g, _ := e.m.Get(ctx, t1.ID, e.user); g.Status != StatusRetrying {
		t.Fatalf("second = %s", g.Status)
	}
	e.fetchErr = nil

	// limit: with one transfer running, others wait
	e.pool.Exec(ctx, `UPDATE transfers SET next_attempt_at = now() WHERE id = $1`, t3.ID)                                                                //nolint:errcheck
	e.pool.Exec(ctx, `UPDATE transfers SET status = 'queued', next_attempt_at = now() WHERE id IN ($1, $2)`, t1.ID, t2.ID)                               //nolint:errcheck
	e.pool.Exec(ctx, `UPDATE transfers SET status = 'in_progress', lease_until = now() + interval '1 minute', worker_id = 'other' WHERE id = $1`, t3.ID) //nolint:errcheck
	e.tick()
	for _, id := range []string{t1.ID, t2.ID} {
		var st string
		e.pool.QueryRow(ctx, `SELECT status FROM transfers WHERE id = $1`, id).Scan(&st) //nolint:errcheck
		if st != StatusQueued {
			t.Fatalf("a transfer started although the concurrency limit was reached: %s", st)
		}
	}
	w1, _ := e.m.Get(ctx, t1.ID, e.user)
	w2, _ := e.m.Get(ctx, t2.ID, u2)
	if w1.QueuePosition != 1 || w2.QueuePosition != 2 || w1.QueueLength != 2 {
		t.Fatalf("waiting positions = %d, %d of %d", w1.QueuePosition, w2.QueuePosition, w1.QueueLength)
	}
	// when the running one ends, the next in line starts, in order
	e.pool.Exec(ctx, `UPDATE transfers SET status = 'completed' WHERE id = $1`, t3.ID) //nolint:errcheck
	e.tick()
	if s := e.get(t1.ID).Status; s != StatusCompleted {
		t.Fatalf("first in line = %s", s)
	}
	if s, _ := e.m.Get(ctx, t2.ID, u2); s.Status != StatusQueued || s.QueuePosition != 1 {
		t.Fatalf("the last one should now be first in line: %s position %d", s.Status, s.QueuePosition)
	}
	e.tick()
	if s, _ := e.m.Get(ctx, t2.ID, u2); s.Status != StatusCompleted {
		t.Fatalf("then the next = %s", s.Status)
	}
}

func TestFailedSongThenRetry(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	tr := e.create(ItemRequest{VideoID: "demoMukeneAud"}, ItemRequest{VideoID: "demoBlessedLyr"})
	good, bad := e.item(tr, "demoMukeneAud"), e.item(tr, "demoBlessedLyr")
	e.attach(tr, good, "ready")
	var ve *ValidationError
	if err := e.m.AttachAudio(ctx, e.user, tr.ID, bad.ID, e.upload(bad, "failed")); !errors.As(err, &ve) {
		t.Fatalf("attaching a failed upload = %v", err)
	}
	// the upload fails after submitting (processing rejects the file)
	failedLater := e.attach(tr, bad, "queued")
	if _, err := e.m.Submit(ctx, e.user, tr.ID); err != nil {
		t.Fatal(err)
	}
	e.pool.Exec(ctx, `UPDATE uploads SET status = 'failed', error = 'the file is not audio' WHERE id = $1`, failedLater) //nolint:errcheck
	e.tick()
	done := e.get(tr.ID)
	st := statuses(done)
	if done.Status != StatusFailed || done.Reason != "items_failed" || st["Mukene"] != ItemDone || st["Blessed"] != ItemFailed {
		t.Fatalf("transfer = %s/%s %v", done.Status, done.Reason, st)
	}
	if v := ToView(done); v.Progress.ItemsFailed != 1 || v.Progress.ItemsDone != 1 || v.Items[1].Error == nil || *v.Items[1].Error != "the file is not audio" {
		t.Fatalf("view = %+v %+v", v.Progress, v.Items[1])
	}
	var vis string
	e.pool.QueryRow(ctx, `SELECT visibility FROM albums WHERE id = $1`, good.ReleaseID).Scan(&vis) //nolint:errcheck
	if vis != "public" {
		t.Fatal("the song that worked must be published even though another failed")
	}

	// retry: only the failed song needs new audio
	re, err := e.m.Retry(ctx, e.user, tr.ID)
	if err != nil || re.Status != StatusCollecting || re.Attempt != 0 {
		t.Fatalf("retry = %+v %v", re, err)
	}
	st = statuses(re)
	if st["Mukene"] != ItemDone || st["Blessed"] != ItemAwaitingAudio {
		t.Fatalf("after retry = %v", st)
	}
	e.attach(re, e.item(re, "demoBlessedLyr"), "ready")
	if _, err := e.m.Submit(ctx, e.user, tr.ID); err != nil {
		t.Fatal(err)
	}
	e.tick()
	if s := e.get(tr.ID); s.Status != StatusCompleted {
		t.Fatalf("after the second run = %s %v", s.Status, statuses(s))
	}
	if _, err := e.m.Retry(ctx, e.user, tr.ID); !errors.Is(err, ErrWrongState) {
		t.Fatalf("retrying a completed transfer = %v", err)
	}
}

func TestAlbumWithAFailedSongIsNotPublished(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	tr := e.create(ItemRequest{VideoID: "demoBetterVis", AlbumTitle: "EP"}, ItemRequest{VideoID: "demoBlessedLyr", AlbumTitle: "EP"})
	ok, bad := e.item(tr, "demoBetterVis"), e.item(tr, "demoBlessedLyr")
	e.attach(tr, ok, "ready")
	up := e.attach(tr, bad, "queued")
	e.m.Submit(ctx, e.user, tr.ID)                                                            //nolint:errcheck
	e.pool.Exec(ctx, `UPDATE uploads SET status = 'failed', error = 'bad' WHERE id = $1`, up) //nolint:errcheck
	e.tick()
	done := e.get(tr.ID)
	var vis string
	e.pool.QueryRow(ctx, `SELECT visibility FROM albums WHERE id = $1`, ok.ReleaseID).Scan(&vis) //nolint:errcheck
	if done.Status != StatusFailed || vis != "draft" {
		t.Fatalf("status %s, album %s: a half-finished album must stay a draft", done.Status, vis)
	}
	// the good song keeps its audio through the retry
	re, _ := e.m.Retry(ctx, e.user, tr.ID)
	if re.Status != StatusCollecting || e.item(re, "demoBetterVis").UploadID == "" {
		t.Fatalf("retry lost the good song's audio: %+v", e.item(re, "demoBetterVis"))
	}
	e.attach(re, e.item(re, "demoBlessedLyr"), "ready")
	if _, err := e.m.Submit(ctx, e.user, tr.ID); err != nil {
		t.Fatal(err)
	}
	e.tick()
	e.pool.QueryRow(ctx, `SELECT visibility FROM albums WHERE id = $1`, ok.ReleaseID).Scan(&vis) //nolint:errcheck
	if s := e.get(tr.ID); s.Status != StatusCompleted || vis != "public" {
		t.Fatalf("after retry: %s, album %s", s.Status, vis)
	}
}

func TestTransientFailureBacksOffAndResumes(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	tr := e.create(ItemRequest{VideoID: "demoMukeneAud"})
	e.attach(tr, tr.Items[0], "ready")
	e.m.Submit(ctx, e.user, tr.ID) //nolint:errcheck

	e.fetchErr = errors.New("connection reset")
	e.tick()
	r := e.get(tr.ID)
	wait := time.Until(*r.NextAttemptAt)
	if r.Status != StatusRetrying || r.Attempt != 1 || wait < 25*time.Second || wait > 40*time.Second || r.Error == "" {
		t.Fatalf("after 1st failure: %s attempt %d wait %v err %q", r.Status, r.Attempt, wait, r.Error)
	}
	if v := ToView(r); v.Queue == nil || v.NextAttemptAt == nil || v.Error == nil {
		t.Fatalf("view of a retrying transfer = %+v", v)
	}
	calls := len(e.fetches)
	e.fetchErr = nil
	e.tick() // not due yet
	if got := e.get(tr.ID); got.Status != StatusRetrying || len(e.fetches) != calls {
		t.Fatalf("retried before the backoff passed: %s", got.Status)
	}
	// second failure waits longer
	e.pool.Exec(ctx, `UPDATE transfers SET next_attempt_at = now() WHERE id = $1`, tr.ID) //nolint:errcheck
	e.fetchErr = errors.New("again")
	e.tick()
	r = e.get(tr.ID)
	if wait = time.Until(*r.NextAttemptAt); r.Attempt != 2 || wait < 110*time.Second || wait > 135*time.Second {
		t.Fatalf("2nd failure: attempt %d wait %v", r.Attempt, wait)
	}
	// third time lucky: the work resumes and finishes
	e.pool.Exec(ctx, `UPDATE transfers SET next_attempt_at = now() WHERE id = $1`, tr.ID) //nolint:errcheck
	e.fetchErr = nil
	e.tick()
	if s := e.get(tr.ID); s.Status != StatusCompleted || s.Attempt != 3 || s.Error != "" {
		t.Fatalf("final = %s attempt %d err %q", s.Status, s.Attempt, s.Error)
	}
	var releases int
	e.pool.QueryRow(ctx, `SELECT count(DISTINCT release_id) FROM transfer_items WHERE transfer_id = $1`, tr.ID).Scan(&releases) //nolint:errcheck
	if releases != 1 {
		t.Fatalf("retries created %d releases", releases)
	}
}

func TestAttemptsRunOut(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	tr := e.create(ItemRequest{VideoID: "demoMukeneAud"})
	e.attach(tr, tr.Items[0], "ready")
	e.m.Submit(ctx, e.user, tr.ID)                                                 //nolint:errcheck
	e.pool.Exec(ctx, `UPDATE transfers SET max_attempts = 2 WHERE id = $1`, tr.ID) //nolint:errcheck
	e.fetchErr = errors.New("down")
	for i := 0; i < 2; i++ {
		e.pool.Exec(ctx, `UPDATE transfers SET next_attempt_at = now() WHERE id = $1`, tr.ID) //nolint:errcheck
		e.tick()
	}
	r := e.get(tr.ID)
	if r.Status != StatusFailed || r.Reason != "attempts_exhausted" || r.FinishedAt == nil {
		t.Fatalf("after exhausting attempts: %s %s", r.Status, r.Reason)
	}
	// an artist can try again
	if re, err := e.m.Retry(ctx, e.user, tr.ID); err != nil || re.Status != StatusQueued || re.Attempt != 0 {
		t.Fatalf("retry = %+v %v", re, err)
	}
	e.fetchErr = nil
	e.tick()
	if s := e.get(tr.ID); s.Status != StatusCompleted {
		t.Fatalf("after retry: %s", s.Status)
	}
}

func TestCrashRecovery(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	tr := e.create(ItemRequest{VideoID: "demoMukeneAud"}, ItemRequest{VideoID: "demoBlessedLyr"})
	for _, it := range tr.Items {
		e.attach(tr, it, "ready")
	}
	e.m.Submit(ctx, e.user, tr.ID) //nolint:errcheck
	// a worker claims the transfer, does part of the job (one cover), then dies
	id, err := e.m.Claim(ctx, "dead-worker", 1, time.Minute)
	if err != nil || id != tr.ID {
		t.Fatalf("claim = %q %v", id, err)
	}
	e.pool.Exec(ctx, `UPDATE transfer_items SET status = 'cover' WHERE transfer_id = $1`, tr.ID) //nolint:errcheck
	// while the lease is valid nobody else takes it
	e.tick()
	if len(e.fetches) != 0 {
		t.Fatal("another worker touched a transfer whose lease is valid")
	}
	e.pool.Exec(ctx, `UPDATE transfers SET lease_until = now() - interval '1 second' WHERE id = $1`, tr.ID) //nolint:errcheck
	var before int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM albums WHERE artist_id = $1`, e.artist).Scan(&before) //nolint:errcheck
	e.tick()
	r := e.get(tr.ID)
	if r.Status != StatusCompleted || r.Attempt != 2 {
		t.Fatalf("after recovery: %s attempt %d", r.Status, r.Attempt)
	}
	var after int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM albums WHERE artist_id = $1`, e.artist).Scan(&after) //nolint:errcheck
	if before != after {
		t.Fatal("resuming created extra releases")
	}
	// a stalled transfer that has used all its attempts is failed, not looped forever
	tr2 := e.create(ItemRequest{VideoID: "demoBetterVis"})
	e.attach(tr2, tr2.Items[0], "ready")
	e.m.Submit(ctx, e.user, tr2.ID)                                                                                                                                              //nolint:errcheck
	e.pool.Exec(ctx, `UPDATE transfers SET status = 'in_progress', attempt = max_attempts, lease_until = now() - interval '1 second', worker_id = 'dead' WHERE id = $1`, tr2.ID) //nolint:errcheck
	e.tick()
	if s := e.get(tr2.ID); s.Status != StatusFailed || s.Reason != "attempts_exhausted" {
		t.Fatalf("looping transfer = %s/%s", s.Status, s.Reason)
	}
}

func TestUnusableThumbnailStillPublishes(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.fetchErr = ErrPermanent
	tr := e.create(ItemRequest{VideoID: "demoMukeneAud"})
	e.attach(tr, tr.Items[0], "ready")
	e.m.Submit(ctx, e.user, tr.ID) //nolint:errcheck
	e.tick()
	r := e.get(tr.ID)
	var vis string
	var cover *string
	e.pool.QueryRow(ctx, `SELECT visibility, cover_image_id FROM albums WHERE id = $1`, r.Items[0].ReleaseID).Scan(&vis, &cover) //nolint:errcheck
	if r.Status != StatusCompleted || vis != "public" || cover != nil {
		t.Fatalf("status %s, release %s, cover %v", r.Status, vis, cover)
	}
}

func TestCancelAndDisconnect(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	tr := e.create(ItemRequest{VideoID: "demoMukeneAud"}, ItemRequest{VideoID: "demoBlessedLyr"})
	pend := e.attach(tr, tr.Items[0], "pending")
	c, err := e.m.Cancel(ctx, e.user, tr.ID)
	if err != nil || c.Status != StatusCancelled || c.FinishedAt == nil {
		t.Fatalf("cancel = %+v %v", c, err)
	}
	var st string
	e.pool.QueryRow(ctx, `SELECT status FROM uploads WHERE id = $1`, pend).Scan(&st) //nolint:errcheck
	if st != "failed" {
		t.Fatalf("the unfinished upload is %s, should be failed so its storage is released", st)
	}
	if _, err := e.m.Cancel(ctx, e.user, tr.ID); !errors.Is(err, ErrWrongState) {
		t.Fatalf("cancelling twice = %v", err)
	}
	// the artist can start over, and disconnecting YouTube cancels what is open
	tr2 := e.create(ItemRequest{VideoID: "demoMukeneAud"})
	if err := e.m.CancelAllOpen(ctx, e.user); err != nil {
		t.Fatal(err)
	}
	if s := e.get(tr2.ID); s.Status != StatusCancelled {
		t.Fatalf("after disconnect = %s", s.Status)
	}
	// a cancelled transfer is never claimed
	tr3 := e.create(ItemRequest{VideoID: "demoMukeneAud"})
	e.attach(tr3, tr3.Items[0], "ready")
	e.m.Submit(ctx, e.user, tr3.ID) //nolint:errcheck
	e.m.Cancel(ctx, e.user, tr3.ID) //nolint:errcheck
	e.tick()
	if len(e.fetches) != 0 {
		t.Fatal("a cancelled transfer was worked on")
	}
}

func TestViewShape(t *testing.T) {
	e := setup(t)
	tr := e.create(ItemRequest{VideoID: "demoMukeneAud"}, ItemRequest{VideoID: "demoMukeneLyr"})
	v := ToView(tr)
	if v.Status != StatusCollecting || v.Queue != nil || v.NextAttemptAt != nil || v.Error != nil || len(v.Items) != 2 {
		t.Fatalf("view = %+v", v)
	}
	if v.Progress.ItemsTotal != 1 || v.Progress.ItemsSkipped != 1 || v.Progress.Percent != 0 {
		t.Fatalf("progress = %+v", v.Progress)
	}
	first := v.Items[0]
	if first.Status != ItemAwaitingAudio || first.Upload == nil || first.Upload.ReleaseID != first.ReleaseID || first.Upload.Title != "Mukene" {
		t.Fatalf("item = %+v", first)
	}
	if v.Items[1].Status != ItemSkipped || v.Items[1].DuplicateOf == "" || v.Items[1].Upload != nil {
		t.Fatalf("skipped item = %+v", v.Items[1])
	}
}

func TestEndHookReportsCompletedAndFailedTransfers(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	var got []Ended
	e.m.OnEnd = func(_ context.Context, ev Ended) error { got = append(got, ev); return nil }

	// completed: two songs published
	tr := e.create(ItemRequest{VideoID: "demoMukeneAud"}, ItemRequest{VideoID: "demoBlessedLyr"})
	for _, it := range tr.Items {
		e.attach(tr, it, "ready")
	}
	e.m.Submit(ctx, e.user, tr.ID) //nolint:errcheck
	e.tick()
	if len(got) != 1 || got[0].Status != StatusCompleted || got[0].Done != 2 || got[0].Failed != 0 || got[0].UserID != e.user || got[0].TransferID != tr.ID {
		t.Fatalf("completed event = %+v", got)
	}
	e.tick()
	if len(got) != 1 {
		t.Fatal("the end of a transfer was reported twice")
	}

	// failed because a song failed
	tr2 := e.create(ItemRequest{VideoID: "demoBetterVis"}, ItemRequest{VideoID: "demoWantedAud"})
	e.attach(tr2, tr2.Items[0], "ready")
	bad := e.attach(tr2, tr2.Items[1], "queued")
	e.m.Submit(ctx, e.user, tr2.ID)                                                            //nolint:errcheck
	e.pool.Exec(ctx, `UPDATE uploads SET status = 'failed', error = 'bad' WHERE id = $1`, bad) //nolint:errcheck
	e.tick()
	if len(got) != 2 || got[1].Status != StatusFailed || got[1].Reason != "items_failed" || got[1].Done != 1 || got[1].Failed != 1 {
		t.Fatalf("failed event = %+v", got)
	}

	// failed because the attempts ran out (also through the stalled-transfer path)
	tr3 := e.create(ItemRequest{VideoID: "demoWantedAud"})
	e.attach(tr3, tr3.Items[0], "ready")
	e.m.Submit(ctx, e.user, tr3.ID)                                                                                                                                              //nolint:errcheck
	e.pool.Exec(ctx, `UPDATE transfers SET status = 'in_progress', attempt = max_attempts, lease_until = now() - interval '1 second', worker_id = 'dead' WHERE id = $1`, tr3.ID) //nolint:errcheck
	e.tick()
	if len(got) != 3 || got[2].Status != StatusFailed || got[2].Reason != "attempts_exhausted" || got[2].TransferID != tr3.ID {
		t.Fatalf("exhausted event = %+v", got)
	}

	// a failing hook does not disturb the transfer
	e.m.OnEnd = func(context.Context, Ended) error { return errors.New("push is down") }
	e.pool.Exec(ctx, `DELETE FROM tracks WHERE artist_id = $1 AND title = 'Wanted'`, e.artist) //nolint:errcheck // the stand-in upload of the stalled transfer
	tr4 := e.create(ItemRequest{VideoID: "demoWantedAud"})
	e.attach(tr4, tr4.Items[0], "ready")
	e.m.Submit(ctx, e.user, tr4.ID) //nolint:errcheck
	e.tick()
	if s := e.get(tr4.ID).Status; s != StatusCompleted {
		t.Fatalf("transfer with a failing hook = %s", s)
	}
}
