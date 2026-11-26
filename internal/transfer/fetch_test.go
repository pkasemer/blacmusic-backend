package transfer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/blacmusic/backend/internal/upload"
	"github.com/blacmusic/backend/internal/ytdlp"
)

type fakeFetcher struct {
	err   map[string]error // by video id
	calls []string
	data  []byte
}

func (f *fakeFetcher) Fetch(_ context.Context, id, dir string) (*ytdlp.File, error) {
	f.calls = append(f.calls, id)
	if err := f.err[id]; err != nil {
		return nil, err
	}
	data := f.data
	if data == nil {
		data = []byte("fake audio bytes")
	}
	path := filepath.Join(dir, id+".m4a")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, err
	}
	return &ytdlp.File{Path: path, Name: id + ".m4a", ContentType: "audio/mp4", Size: int64(len(data))}, nil
}

var rights = Rights{Accepted: true, TermsVersion: "2026-11"}

func (e *env) withFetch(f *fakeFetcher) {
	e.m.FetchEnabled = true
	e.runner.SetFetcher(f)
}

func (e *env) createFetch(items ...ItemRequest) *Transfer {
	e.t.Helper()
	tr, err := e.m.CreateWith(context.Background(), e.user, "", items, rights)
	if err != nil {
		e.t.Fatalf("create: %v", err)
	}
	return tr
}

// finish plays the transcoding worker for an upload the transfer created: the upload becomes a ready draft track.
func (e *env) finish(uploadID string) {
	e.t.Helper()
	ctx := context.Background()
	var trackID, title, releaseID string
	if err := e.pool.QueryRow(ctx, `SELECT track_id, title, release_id FROM uploads WHERE id = $1`, uploadID).Scan(&trackID, &title, &releaseID); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO tracks (id, title, artist_id, album_id, duration_ms, visibility) VALUES ($1, $2, $3, $4, 180000, 'draft')`, trackID, title, e.artist, releaseID); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE uploads SET status = 'ready' WHERE id = $1`, uploadID); err != nil {
		e.t.Fatal(err)
	}
}

func uploadIDs(tr *Transfer) []string {
	var out []string
	for _, it := range tr.Items {
		if it.UploadID != "" {
			out = append(out, it.UploadID)
		}
	}
	return out
}

func TestPlanningWithAutomaticAudio(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	f := &fakeFetcher{}

	// the feature is off: asking for it is refused, and the default is a file the artist uploads
	var ve *ValidationError
	if _, err := e.m.CreateWith(ctx, e.user, "", []ItemRequest{{VideoID: "demoMukeneAud", Audio: "youtube"}}, rights); !errors.As(err, &ve) {
		t.Fatalf("fetch while off = %v", err)
	}
	tr := e.create(ItemRequest{VideoID: "demoMukeneAud"})
	if tr.Status != StatusCollecting || tr.Items[0].AudioSource != "upload" || tr.Items[0].Status != ItemAwaitingAudio {
		t.Fatalf("default while off = %+v", tr.Items[0])
	}
	e.m.Cancel(ctx, e.user, tr.ID) //nolint:errcheck

	e.withFetch(f)
	// fetching needs the artist's confirmation that they own the rights
	if _, err := e.m.Create(ctx, e.user, "", []ItemRequest{{VideoID: "demoMukeneAud"}}); !errors.Is(err, ErrRightsNotAccepted) {
		t.Fatalf("no rights confirmation = %v", err)
	}
	if _, err := e.m.CreateWith(ctx, e.user, "", []ItemRequest{{VideoID: "demoMukeneAud"}}, Rights{Accepted: true}); !errors.As(err, &ve) {
		t.Fatalf("no terms version = %v", err)
	}
	var open int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM transfers WHERE user_id = $1 AND status IN ('collecting','queued')`, e.user).Scan(&open) //nolint:errcheck
	if open != 0 {
		t.Fatal("a refused request left a transfer behind")
	}

	// everything fetched by the server goes straight into the queue, with the confirmation recorded
	tr = e.createFetch(ItemRequest{VideoID: "demoMukeneAud"}, ItemRequest{VideoID: "demoBlessedLyr"})
	if tr.Status != StatusQueued || tr.QueuePosition != 1 || tr.TermsVersion != "2026-11" {
		t.Fatalf("all-fetch transfer = %s position %d terms %q", tr.Status, tr.QueuePosition, tr.TermsVersion)
	}
	for _, it := range tr.Items {
		if it.AudioSource != "youtube" || it.Status != ItemFetching || it.ReleaseID == "" {
			t.Fatalf("item = %+v", it)
		}
	}
	var at *time.Time
	e.pool.QueryRow(ctx, `SELECT rights_accepted_at FROM transfers WHERE id = $1`, tr.ID).Scan(&at) //nolint:errcheck
	if at == nil {
		t.Fatal("the rights confirmation was not recorded")
	}
	v := ToView(tr)
	if v.Items[0].Status != ItemFetching || v.Items[0].AudioSource != "youtube" || v.Items[0].Percent != 8 {
		t.Fatalf("view = %+v", v.Items[0])
	}
	// a song being fetched carries the upload hint too, for "use my own file instead"
	if h := v.Items[0].Upload; h == nil || h.ReleaseID != v.Items[0].ReleaseID || h.Title != "Mukene" {
		t.Fatalf("fetching item has no upload hint: %+v", v.Items[0].Upload)
	}
	e.m.Cancel(ctx, e.user, tr.ID) //nolint:errcheck

	// a mix: one song fetched, one the artist uploads, one not on YouTube at all
	tr = e.createFetch(ItemRequest{VideoID: "demoMukeneAud"}, ItemRequest{VideoID: "demoBetterVis", Audio: "upload"}, ItemRequest{Title: "Unreleased Demo", Genre: "Afropop"})
	if tr.Status != StatusCollecting {
		t.Fatalf("mixed transfer = %s, want collecting (two songs need a file)", tr.Status)
	}
	got := map[string]string{}
	for _, it := range tr.Items {
		got[it.Title] = it.AudioSource + "/" + it.Status
	}
	if got["Mukene"] != "youtube/fetching" || got["Better (ft Wana Benjamin)"] != "upload/awaiting_audio" || got["Unreleased Demo"] != "upload/awaiting_audio" {
		t.Fatalf("items = %v", got)
	}
	manual := e.item(tr, "manual:2")
	if manual.ThumbnailURL != "" || manual.ReleaseID == "" || manual.ReleaseDate == "" {
		t.Fatalf("manual song = %+v", manual)
	}
	e.m.Cancel(ctx, e.user, tr.ID) //nolint:errcheck

	// manual songs need a title and a file, and cannot be fetched
	if _, err := e.m.CreateWith(ctx, e.user, "", []ItemRequest{{}}, rights); !errors.As(err, &ve) {
		t.Fatalf("manual song without a title = %v", err)
	}
	if _, err := e.m.CreateWith(ctx, e.user, "", []ItemRequest{{Title: "X", Audio: "youtube"}}, rights); !errors.As(err, &ve) {
		t.Fatalf("manual song asked to be fetched = %v", err)
	}
	if _, err := e.m.CreateWith(ctx, e.user, "", []ItemRequest{{VideoID: "demoMukeneAud", Audio: "dropbox"}}, rights); !errors.As(err, &ve) {
		t.Fatalf("unknown audio source = %v", err)
	}
	// a transfer of manual songs only needs no rights confirmation (nothing is fetched)
	tr, err := e.m.Create(ctx, e.user, "", []ItemRequest{{Title: "Only Mine"}})
	if err != nil || tr.Status != StatusCollecting {
		t.Fatalf("manual-only = %v %v", tr, err)
	}
	e.m.Cancel(ctx, e.user, tr.ID) //nolint:errcheck
	// the same song is not added twice
	tr = e.createFetch(ItemRequest{VideoID: "demoMukeneAud"}, ItemRequest{Title: "Mukene", Audio: "upload"})
	if e.item(tr, "manual:1").Status != ItemSkipped {
		t.Fatalf("duplicate manual song = %+v", e.item(tr, "manual:1"))
	}
}

func TestTheServerFetchesAndPublishes(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	f := &fakeFetcher{}
	e.withFetch(f)
	var ended []Ended
	e.m.OnEnd = func(_ context.Context, ev Ended) error { ended = append(ended, ev); return nil }

	tr := e.createFetch(ItemRequest{VideoID: "demoMukeneAud"}, ItemRequest{VideoID: "demoBetterVis", AlbumTitle: "EP"}, ItemRequest{VideoID: "demoBlessedLyr", AlbumTitle: "EP"})
	e.tick() // claims, fetches ONE song
	if len(f.calls) != 1 {
		t.Fatalf("fetched %v in the first round, want one song per round", f.calls)
	}
	e.tick()
	e.tick()
	if len(f.calls) != 3 {
		t.Fatalf("fetched %v", f.calls)
	}
	cur := e.get(tr.ID)
	if cur.Status != StatusInProgress {
		t.Fatalf("status = %s", cur.Status)
	}
	for _, it := range cur.Items {
		if it.UploadID == "" || it.UploadStatus != "queued" {
			t.Fatalf("item = %+v: the fetched audio must be an ordinary queued upload", it)
		}
	}
	// the files are in storage and the uploads are the artist's own, reserved like any upload
	var u struct {
		user, artist, release, key, filename string
		size                                 int64
	}
	id := cur.Items[0].UploadID
	e.pool.QueryRow(ctx, `SELECT user_id::text, artist_id, release_id, original_key, filename, size_bytes FROM uploads WHERE id = $1`, id).Scan(&u.user, &u.artist, &u.release, &u.key, &u.filename, &u.size) //nolint:errcheck
	if u.user != e.user || u.artist != e.artist || u.release != cur.Items[0].ReleaseID || u.filename != "demoMukeneAud.m4a" || u.size != 16 {
		t.Fatalf("upload = %+v", u)
	}
	if b, ok := e.blobs.Get(u.key); !ok || string(b) != "fake audio bytes" {
		t.Fatalf("the audio is not in storage under %s", u.key)
	}
	// the transcoder finishes; the transfer covers and publishes
	for _, uid := range uploadIDs(cur) {
		e.finish(uid)
	}
	e.tick()
	done := e.get(tr.ID)
	if done.Status != StatusCompleted || len(ended) != 1 || ended[0].Done != 3 {
		t.Fatalf("final = %s, ended %+v", done.Status, ended)
	}
	for _, it := range done.Items {
		if it.Status != ItemDone {
			t.Fatalf("item = %+v", it)
		}
	}
	var public int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM albums WHERE visibility = 'public' AND id IN (SELECT release_id FROM transfer_items WHERE transfer_id = $1)`, tr.ID).Scan(&public) //nolint:errcheck
	if public != 2 {
		t.Fatalf("%d public releases, want 2", public)
	}
}

func TestFailedFetchFallsBackToAFile(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	f := &fakeFetcher{err: map[string]error{"demoBlessedLyr": &ytdlp.Error{Permanent: true, Reason: "the video is private", Detail: "Private video"}}}
	e.withFetch(f)
	var asked []NeedsAudio
	e.m.OnNeedsAudio = func(_ context.Context, ev NeedsAudio) error { asked = append(asked, ev); return nil }

	tr := e.createFetch(ItemRequest{VideoID: "demoMukeneAud"}, ItemRequest{VideoID: "demoBlessedLyr"})
	for i := 0; i < 4; i++ {
		e.tick()
	}
	cur := e.get(tr.ID)
	if cur.Status != StatusCollecting {
		t.Fatalf("status = %s, want collecting: the artist has to supply a file", cur.Status)
	}
	if len(asked) != 1 || asked[0].Songs != 1 || asked[0].UserID != e.user || asked[0].TransferID != tr.ID {
		t.Fatalf("needs-audio event = %+v", asked)
	}
	bad, good := e.item(cur, "demoBlessedLyr"), e.item(cur, "demoMukeneAud")
	if bad.Status != ItemAwaitingAudio || bad.AudioSource != "upload" || bad.FetchError != "the video is private. Upload a file for this song instead." || bad.UploadID != "" {
		t.Fatalf("failed song = %+v", bad)
	}
	if good.UploadID == "" {
		t.Fatal("the song that could be fetched lost its audio")
	}
	v := ToView(cur)
	for _, it := range v.Items {
		if it.VideoID == "demoBlessedLyr" && (it.Status != ItemAwaitingAudio || it.Upload == nil || it.Error == nil || *it.Error != bad.FetchError || it.AudioSource != "upload") {
			t.Fatalf("view of the failed song = %+v", it)
		}
	}
	// the fetched song's upload waits (the transfer is collecting); the artist adds their own file and submits
	e.finish(good.UploadID)
	e.attach(cur, bad, "ready")
	if _, err := e.m.Submit(ctx, e.user, tr.ID); err != nil {
		t.Fatal(err)
	}
	e.tick()
	e.tick()
	if s := e.get(tr.ID); s.Status != StatusCompleted {
		t.Fatalf("after the artist's file: %s %v", s.Status, statuses(s))
	}
	if len(f.calls) != 2 {
		t.Fatalf("fetch calls = %v: the failed song must not be fetched again", f.calls)
	}
}

func TestTemporaryFetchProblemsAreRetried(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	f := &fakeFetcher{err: map[string]error{"demoMukeneAud": &ytdlp.Error{Reason: "YouTube is blocking automatic downloads from our server right now", Detail: "not a bot"}}}
	e.withFetch(f)
	tr := e.createFetch(ItemRequest{VideoID: "demoMukeneAud"})

	e.tick()
	it := e.get(tr.ID).Items[0]
	wait := time.Until(*it.NextFetchAt)
	if it.Status != ItemFetching || it.FetchAttempts != 1 || wait < 50*time.Second || wait > 70*time.Second || it.FetchError == "" {
		t.Fatalf("after 1st failure: %+v wait %v", it, wait)
	}
	if v := ToView(e.get(tr.ID)); v.Items[0].Error == nil {
		t.Fatal("the artist is not told that it is being retried")
	}
	e.tick() // not due
	if len(f.calls) != 1 {
		t.Fatalf("tried again before the delay: %v", f.calls)
	}
	// 2nd failure waits longer, 3rd gives up and asks for a file
	e.pool.Exec(ctx, `UPDATE transfer_items SET next_fetch_at = now() WHERE transfer_id = $1`, tr.ID) //nolint:errcheck
	e.tick()
	if it = e.get(tr.ID).Items[0]; it.FetchAttempts != 2 || time.Until(*it.NextFetchAt) < 4*time.Minute {
		t.Fatalf("after 2nd failure: %+v", it)
	}
	e.pool.Exec(ctx, `UPDATE transfer_items SET next_fetch_at = now() WHERE transfer_id = $1`, tr.ID) //nolint:errcheck
	e.tick()
	it = e.get(tr.ID).Items[0]
	if it.Status != ItemAwaitingAudio || it.AudioSource != "upload" || len(f.calls) != 3 {
		t.Fatalf("after 3 failures: %+v, %d tries", it, len(f.calls))
	}
	e.tick()
	if s := e.get(tr.ID); s.Status != StatusCollecting {
		t.Fatalf("transfer = %s", s.Status)
	}

	// the problem goes away before the third try: the song is fetched
	e.m.Cancel(ctx, e.user, tr.ID) //nolint:errcheck
	f2 := &fakeFetcher{err: map[string]error{"demoWantedAud": &ytdlp.Error{Reason: "the download from YouTube failed", Detail: "x"}}}
	e.runner.SetFetcher(f2)
	tr = e.createFetch(ItemRequest{VideoID: "demoWantedAud"})
	e.tick()
	delete(f2.err, "demoWantedAud")
	e.pool.Exec(ctx, `UPDATE transfer_items SET next_fetch_at = now() WHERE transfer_id = $1`, tr.ID) //nolint:errcheck
	e.tick()
	if it = e.get(tr.ID).Items[0]; it.UploadID == "" || (it.Status != ItemUploading && it.Status != ItemProcessing) {
		t.Fatalf("after the retry: %+v", it)
	}
}

func TestFetchingEdgeCases(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	// no fetcher on this worker: songs go straight to the artist's own file, with a reason
	e.m.FetchEnabled = true
	tr := e.createFetch(ItemRequest{VideoID: "demoMukeneAud"})
	e.tick()
	e.tick()
	it := e.get(tr.ID).Items[0]
	if it.Status != ItemAwaitingAudio || it.FetchError == "" {
		t.Fatalf("without a fetcher: %+v", it)
	}
	e.m.Cancel(ctx, e.user, tr.ID) //nolint:errcheck

	// a cancelled/aborted fetch is a transfer-level retry, not a verdict on the video
	f := &fakeFetcher{err: map[string]error{"demoMukeneAud": context.Canceled}}
	e.runner.SetFetcher(f)
	tr = e.createFetch(ItemRequest{VideoID: "demoMukeneAud"})
	e.tick()
	if s := e.get(tr.ID); s.Status != StatusRetrying || s.Items[0].Status != ItemFetching || s.Items[0].FetchAttempts != 0 {
		t.Fatalf("after an aborted fetch: %s %+v", s.Status, s.Items[0])
	}
	e.m.Cancel(ctx, e.user, tr.ID) //nolint:errcheck

	// not enough storage: that song fails
	small := &fakeFetcher{data: make([]byte, 2048)}
	e.runner.SetFetcher(small)
	e.pool.Exec(ctx, `UPDATE storage_grants SET bytes = 1000 WHERE artist_id = $1`, e.artist) //nolint:errcheck
	tr = e.createFetch(ItemRequest{VideoID: "demoWantedAud"})
	e.tick()
	e.tick()
	if s := e.get(tr.ID); s.Items[0].Status != ItemFailed || s.Status != StatusFailed || s.Items[0].Error == "" {
		t.Fatalf("storage full: %s %+v", s.Status, s.Items[0])
	}
	e.pool.Exec(ctx, `UPDATE storage_grants SET bytes = $2 WHERE artist_id = $1`, e.artist, int64(10)<<30) //nolint:errcheck

	// too many unfinished uploads: wait, without counting it as a failed attempt
	e.pool.Exec(ctx, `DELETE FROM transfers WHERE user_id = $1`, e.user) //nolint:errcheck
	for i := 0; i < upload.MaxActivePerArtist; i++ {
		e.pool.Exec(ctx, `INSERT INTO uploads (user_id, artist_id, title, artist_name, filename, size_bytes, original_key, status)
			VALUES ($1, $2, 'filler', 'x', 'f', 1, 'originals/filler-' || gen_random_uuid(), 'pending')`, e.user, e.artist) //nolint:errcheck
	}
	e.runner.SetFetcher(&fakeFetcher{})
	tr = e.createFetch(ItemRequest{VideoID: "demoBetterVis"})
	e.tick()
	it = e.get(tr.ID).Items[0]
	if it.Status != ItemFetching || it.FetchAttempts != 0 || it.NextFetchAt == nil || it.UploadID != "" {
		t.Fatalf("at the upload cap: %+v", it)
	}
}

func TestSwitchingASongToYourOwnFile(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	f := &fakeFetcher{}
	e.withFetch(f)
	// one song is fetched, one is uploaded: the transfer collects until the upload is attached
	tr := e.createFetch(ItemRequest{VideoID: "demoMukeneAud"}, ItemRequest{VideoID: "demoBlessedLyr", Audio: "upload"})
	if tr.Status != StatusCollecting {
		t.Fatalf("status = %s", tr.Status)
	}
	// submitting before the uploaded song has its file is refused, and the fetched song is not counted as missing
	var missing *AudioMissingError
	if _, err := e.m.Submit(ctx, e.user, tr.ID); !errors.As(err, &missing) || len(missing.ItemIDs) != 1 {
		t.Fatalf("early submit = %v", err)
	}
	// the artist decides to use their own file for the fetched song as well
	mk := e.item(tr, "demoMukeneAud")
	e.attach(tr, mk, "queued")
	e.attach(tr, e.item(tr, "demoBlessedLyr"), "queued")
	cur := e.get(tr.ID)
	if m := e.item(cur, "demoMukeneAud"); m.AudioSource != "upload" || m.FetchError != "" {
		t.Fatalf("after attaching a file = %+v", m)
	}
	if _, err := e.m.Submit(ctx, e.user, tr.ID); err != nil {
		t.Fatal(err)
	}
	e.tick()
	if len(f.calls) != 0 {
		t.Fatalf("the server still fetched %v although the artist supplied the file", f.calls)
	}
	// a failed transfer's retry hands failed songs back for a file, never for another fetch
	e.pool.Exec(ctx, `UPDATE transfer_items SET status = 'failed', error = 'bad', audio_source = 'youtube' WHERE id = $1`, mk.ID) //nolint:errcheck
	e.pool.Exec(ctx, `UPDATE transfers SET status = 'failed', reason = 'items_failed' WHERE id = $1`, tr.ID)                      //nolint:errcheck
	re, err := e.m.Retry(ctx, e.user, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if m := e.item(re, "demoMukeneAud"); m.Status != ItemAwaitingAudio || m.AudioSource != "upload" {
		t.Fatalf("after retry = %+v", m)
	}
}
