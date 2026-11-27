package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/blobtest"
	"github.com/blacmusic/backend/internal/media"
	"github.com/blacmusic/backend/internal/release"
	"github.com/blacmusic/backend/internal/testutil"
	"github.com/blacmusic/backend/internal/upload"
)

// Integration test: needs Postgres and ffmpeg/ffprobe, e.g. inside Docker:
//
//	apk add ffmpeg && TEST_DATABASE_URL=postgres://... go test ./internal/worker
type env struct {
	t        *testing.T
	pool     *pgxpool.Pool
	store    *upload.Store
	blobs    *blobtest.Mem
	w        *Worker
	uid      string
	artistID string
}

func setup(t *testing.T) *env {
	t.Helper()
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
	pool := testutil.Pool(t)
	uid := testutil.User(t, pool, "user", true)
	artistID := testutil.Artist(t, pool, uid, testutil.Unique("Worker Artist"), testutil.GiB)

	store := upload.NewStore(pool)
	blobs := blobtest.New()
	w := New(store, blobs, Config{
		FFmpeg: "ffmpeg", FFprobe: "ffprobe",
		MaxBytes: 50 << 20, MaxDuration: time.Hour, TranscodeTimeout: time.Minute,
	})
	return &env{t: t, pool: pool, store: store, blobs: blobs, w: w, uid: uid, artistID: artistID}
}

// submit creates an upload (into releaseID, or a new draft single when empty),
// "uploads" data as its original and claims it.
func (e *env) submit(releaseID string, data []byte) *upload.Upload {
	e.t.Helper()
	ctx := context.Background()
	u, err := e.store.Create(ctx, upload.NewUpload{
		UserID: e.uid, ReleaseID: releaseID, Title: "Test Song", Filename: "x",
		SizeBytes: int64(len(data)), TermsVersion: "test",
	})
	if err != nil {
		e.t.Fatal(err)
	}
	e.blobs.Put(u.OriginalKey, data)
	if _, err := e.store.Queue(ctx, u.ID, u.SizeBytes); err != nil {
		e.t.Fatal(err)
	}
	return e.claim(u.ID)
}

// claim takes the queued upload id. The oldest queued job is claimed first; on a
// shared DB it may belong to someone else, so those are put back afterwards.
func (e *env) claim(id string) *upload.Upload {
	e.t.Helper()
	ctx := context.Background()
	var others []string
	defer func() {
		for _, o := range others {
			_ = e.store.Requeue(ctx, o)
		}
	}()
	for i := 0; i < 50; i++ {
		j, err := e.store.Claim(ctx, time.Hour, 3)
		if err != nil {
			e.t.Fatal(err)
		}
		if j == nil {
			break
		}
		if j.ID == id {
			return j
		}
		others = append(others, j.ID)
	}
	e.t.Fatal("could not claim the test upload")
	return nil
}

// tone makes pink noise as WAV (noise keeps AAC near its target bitrate).
func tone(t *testing.T, seconds int, channels int) []byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "in.wav")
	cmd := exec.Command("ffmpeg", "-nostdin", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("anoisesrc=d=%d:c=pink:r=44100:a=0.3", seconds),
		"-ac", strconv.Itoa(channels), "-c:a", "pcm_s16le", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make test audio: %v\n%s", err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type probed struct {
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
	Streams []struct {
		CodecName  string `json:"codec_name"`
		Channels   int    `json:"channels"`
		SampleRate string `json:"sample_rate"`
	} `json:"streams"`
}

func probe(t *testing.T, data []byte) probed {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f.m4a")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_format", "-show_streams", p).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	var pr probed
	if err := json.Unmarshal(out, &pr); err != nil {
		t.Fatal(err)
	}
	return pr
}

func TestProcessProducesLowMediumHigh(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	rel := testutil.Release(t, e.pool, e.artistID, "The Album", "album", "draft")
	job := e.submit(rel, tone(t, 6, 2))

	if err := e.w.Process(ctx, job); err != nil {
		t.Fatal(err)
	}

	// The track sits in the release under the artist's profile, as a draft.
	var title, artistName, albumTitle, visibility string
	var durMs int
	if err := e.pool.QueryRow(ctx, `
		SELECT t.title, a.name, al.title, t.duration_ms, t.visibility FROM tracks t
		JOIN artists a ON a.id=t.artist_id JOIN albums al ON al.id=t.album_id WHERE t.id=$1`, job.TrackID).
		Scan(&title, &artistName, &albumTitle, &durMs, &visibility); err != nil {
		t.Fatal(err)
	}
	if title != "Test Song" || albumTitle != "The Album" || artistName == "" || visibility != "draft" {
		t.Fatalf("metadata = %q %q %q %q", title, artistName, albumTitle, visibility)
	}
	if durMs < 5900 || durMs > 6300 {
		t.Fatalf("duration_ms = %d, want about 6000", durMs)
	}
	if st, _ := e.store.Get(ctx, job.ID); st.Status != upload.StatusReady {
		t.Fatalf("status = %s, want ready", st.Status)
	}

	// Loudness was measured on the original and stored.
	var lufs, peak *float64
	var checked *time.Time
	if err := e.pool.QueryRow(ctx, `SELECT loudness_lufs, true_peak_db, loudness_checked_at FROM tracks WHERE id=$1`, job.TrackID).
		Scan(&lufs, &peak, &checked); err != nil {
		t.Fatal(err)
	}
	if lufs == nil || *lufs > -5 || *lufs < -45 || peak == nil || *peak > 3 || checked == nil {
		t.Fatalf("loudness = %v / peak %v / checked %v, want plausible values for 0.3-amplitude pink noise", lufs, peak, checked)
	}

	// The legacy pointer exists (native search needs it) and targets the medium file.
	var key string
	if err := e.pool.QueryRow(ctx, `SELECT media_key FROM track_sources WHERE track_id=$1 AND source='native'`, job.TrackID).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if want := "tracks/" + job.TrackID + "/medium.m4a"; key != want {
		t.Fatalf("legacy media_key = %s, want %s", key, want)
	}

	// Each rendition is real AAC at about its target bitrate, and they grow.
	sizes := map[media.Quality]int{}
	for _, rung := range media.Ladder {
		data, ok := e.blobs.Get(fmt.Sprintf("tracks/%s/%s.m4a", job.TrackID, rung.Quality))
		if !ok {
			t.Fatalf("missing %s rendition", rung.Quality)
		}
		sizes[rung.Quality] = len(data)
		p := probe(t, data)
		if len(p.Streams) != 1 || p.Streams[0].CodecName != "aac" || p.Streams[0].SampleRate != "44100" || p.Streams[0].Channels != 2 {
			t.Fatalf("%s: unexpected streams %+v", rung.Quality, p.Streams)
		}
		d, _ := strconv.ParseFloat(p.Format.Duration, 64)
		if math.Abs(d-6) > 0.3 {
			t.Fatalf("%s: duration %.2fs, want about 6s", rung.Quality, d)
		}
		kbps := float64(len(data)) * 8 / d / 1000
		if kbps < float64(rung.BitrateKbps)*0.6 || kbps > float64(rung.BitrateKbps)*1.3 {
			t.Fatalf("%s: measured %.0f kbps, target %d", rung.Quality, kbps, rung.BitrateKbps)
		}

		var dbKbps int
		var dbSize int64
		if err := e.pool.QueryRow(ctx, `SELECT bitrate_kbps, size_bytes FROM track_renditions WHERE track_id=$1 AND quality=$2`,
			job.TrackID, string(rung.Quality)).Scan(&dbKbps, &dbSize); err != nil {
			t.Fatal(err)
		}
		if dbKbps != rung.BitrateKbps || dbSize != int64(len(data)) {
			t.Fatalf("%s: db row %d kbps %d bytes, want %d / %d", rung.Quality, dbKbps, dbSize, rung.BitrateKbps, len(data))
		}
	}
	if !(sizes[media.Low] < sizes[media.Medium] && sizes[media.Medium] < sizes[media.High]) {
		t.Fatalf("sizes must grow with quality: %v", sizes)
	}
	t.Logf("6s of noise -> low %d B, medium %d B, high %d B", sizes[media.Low], sizes[media.Medium], sizes[media.High])
}

func TestMonoStaysMono(t *testing.T) {
	e := setup(t)
	job := e.submit("", tone(t, 3, 1))
	if err := e.w.Process(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	data, _ := e.blobs.Get("tracks/" + job.TrackID + "/low.m4a")
	if p := probe(t, data); p.Streams[0].Channels != 1 {
		t.Fatalf("mono source became %d channels", p.Streams[0].Channels)
	}
}

func TestTracksJoinTheirReleaseAndGoLiveWithIt(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	rel := testutil.Release(t, e.pool, e.artistID, "Two Tracks", "album", "draft")
	audio := tone(t, 2, 2)

	a := e.submit(rel, audio)
	if err := e.w.Process(ctx, a); err != nil {
		t.Fatal(err)
	}
	b := e.submit(rel, audio)
	if err := e.w.Process(ctx, b); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM tracks WHERE album_id=$1 AND visibility='draft'`, rel).Scan(&n); err != nil || n != 2 {
		t.Fatalf("draft tracks in the release = %d (%v), want 2", n, err)
	}

	// Publishing the release makes both public...
	rs := release.NewStore(e.pool, e.blobs, "http://api.test")
	if _, err := rs.Publish(ctx, rel, e.uid, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM tracks WHERE album_id=$1 AND visibility='public'`, rel).Scan(&n); err != nil || n != 2 {
		t.Fatalf("public tracks after publish = %d (%v), want 2", n, err)
	}
	// ...and a track added to a live release is public as soon as it is processed.
	c := e.submit(rel, audio)
	if err := e.w.Process(ctx, c); err != nil {
		t.Fatal(err)
	}
	var vis string
	if err := e.pool.QueryRow(ctx, `SELECT visibility FROM tracks WHERE id=$1`, c.TrackID).Scan(&vis); err != nil || vis != "public" {
		t.Fatalf("late track visibility = %q (%v), want public", vis, err)
	}
}

func TestReleaseDeletedWhileQueuedFailsTheUpload(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	rel := testutil.Release(t, e.pool, e.artistID, "Gone Soon", "album", "draft")
	job := e.submit(rel, tone(t, 2, 2))
	// The release disappears after the upload was claimed (e.g. an old claim).
	if _, err := e.pool.Exec(ctx, `UPDATE uploads SET release_id = NULL WHERE id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `DELETE FROM albums WHERE id=$1`, rel); err != nil {
		t.Fatal(err)
	}
	job.ReleaseID = rel
	var inv *media.InvalidMediaError
	if err := e.w.Process(ctx, job); !errors.As(err, &inv) {
		t.Fatalf("err = %v, want InvalidMediaError (permanent failure)", err)
	}
	var n int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM tracks WHERE id=$1`, job.TrackID).Scan(&n); err != nil || n != 0 {
		t.Fatal("no track may be created for a deleted release")
	}
}

func TestRejectsFilesThatAreNotAudio(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	cases := map[string][]byte{
		"text":        []byte("hello, this is not audio at all"),
		"hls list":    []byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:10\n#EXTINF:10,\nhttp://127.0.0.1:1/x.ts\n#EXT-X-ENDLIST\n"),
		"concat list": []byte("ffconcat version 1.0\nfile '/etc/passwd'\n"),
		"random":      append([]byte{0xff, 0x00, 0x13}, make([]byte, 4096)...),
	}
	for name, data := range cases {
		job := e.submit("", data)
		err := e.w.Process(ctx, job)
		var inv *media.InvalidMediaError
		if !errors.As(err, &inv) {
			t.Errorf("%s: err = %v, want InvalidMediaError", name, err)
		}
		var n int
		if e.pool.QueryRow(ctx, `SELECT count(*) FROM tracks WHERE id=$1`, job.TrackID).Scan(&n) != nil || n != 0 {
			t.Errorf("%s: a track must not be created for a rejected file", name)
		}
	}
}

func TestHandleMarksInvalidUploadsFailedAndRemovesTheOriginal(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	job := e.submit("", []byte("not audio"))
	e.w.handle(ctx, job)
	st, err := e.store.Get(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != upload.StatusFailed || st.Error == "" {
		t.Fatalf("status=%s error=%q, want failed with a message", st.Status, st.Error)
	}
	if _, ok := e.blobs.Get(job.OriginalKey); ok {
		t.Fatal("the original of a failed upload must be deleted so it stops using storage")
	}
}

func TestMissingOriginalIsRejected(t *testing.T) {
	e := setup(t)
	job := e.submit("", []byte("x"))
	_ = e.blobs.Remove(context.Background(), job.OriginalKey)
	var inv *media.InvalidMediaError
	if err := e.w.Process(context.Background(), job); !errors.As(err, &inv) {
		t.Fatalf("err = %v, want InvalidMediaError", err)
	}
}

func TestTransientErrorRequeuesThenFailsAfterMaxAttempts(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.w.cfg.FFmpeg = "/nonexistent/ffmpeg" // a system failure, not a bad file
	job := e.submit("", tone(t, 2, 2))

	e.w.handle(ctx, job)
	if st, _ := e.store.Get(ctx, job.ID); st.Status != upload.StatusQueued {
		t.Fatalf("after 1st failure status = %s, want queued (retry)", st.Status)
	}
	job = e.claim(job.ID)
	job.Attempts = e.w.cfg.MaxAttempts // pretend this was the last allowed try
	e.w.handle(ctx, job)
	if st, _ := e.store.Get(ctx, job.ID); st.Status != upload.StatusFailed {
		t.Fatalf("after final failure status = %s, want failed", st.Status)
	}
}

func TestMaintenanceGoesLiveAndExpires(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	rs := release.NewStore(e.pool, e.blobs, "http://api.test")
	rel := testutil.Release(t, e.pool, e.artistID, "Scheduled", "album", "draft")
	testutil.ReadyTrack(t, e.pool, e.uid, e.artistID, rel, "T", 100, "draft")
	if _, err := rs.Publish(ctx, rel, e.uid, ptrTime(time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE albums SET publish_at = now() - interval '1 minute' WHERE id=$1`, rel); err != nil {
		t.Fatal(err)
	}
	_, pendingID := func() (string, string) {
		u, err := e.store.Create(ctx, upload.NewUpload{UserID: e.uid, Title: "Abandoned", Filename: "f", SizeBytes: 10, TermsVersion: "v"})
		if err != nil {
			t.Fatal(err)
		}
		e.blobs.Put(u.OriginalKey, []byte("partial"))
		return u.OriginalKey, u.ID
	}()
	if _, err := e.pool.Exec(ctx, `UPDATE uploads SET created_at = now() - interval '3 hours' WHERE id=$1`, pendingID); err != nil {
		t.Fatal(err)
	}

	NewMaintenance(e.pool, e.store, rs, e.blobs, time.Hour, "ffmpeg").RunOnce(ctx)

	var vis, tvis string
	if err := e.pool.QueryRow(ctx, `SELECT al.visibility, t.visibility FROM albums al JOIN tracks t ON t.album_id=al.id WHERE al.id=$1`, rel).Scan(&vis, &tvis); err != nil {
		t.Fatal(err)
	}
	if vis != "public" || tvis != "public" {
		t.Fatalf("scheduled release = %s / track %s, want public / public", vis, tvis)
	}
	if st, _ := e.store.Get(ctx, pendingID); st.Status != upload.StatusFailed {
		t.Fatalf("abandoned upload status = %s, want failed", st.Status)
	}
	if _, ok := e.blobs.Get("originals/" + pendingID); ok {
		t.Fatal("the partial file of an abandoned upload must be deleted")
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

func TestBackfillMeasuresOldTracksFromTheirHighRendition(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	rs := release.NewStore(e.pool, e.blobs, "http://api.test")
	rel := testutil.Release(t, e.pool, e.artistID, "Old Album", "album", "public")

	// An old track whose rendition exists, one whose file is gone.
	withFile, _ := testutil.ReadyTrack(t, e.pool, e.uid, e.artistID, rel, "Has file", 100, "public")
	noFile, _ := testutil.ReadyTrack(t, e.pool, e.uid, e.artistID, rel, "No file", 100, "public")
	dir := t.TempDir()
	m4a := filepath.Join(dir, "high.m4a")
	if out, err := exec.Command("ffmpeg", "-nostdin", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "sine=f=1000:d=4",
		"-ac", "2", "-c:a", "aac", "-b:a", "256k", m4a).CombinedOutput(); err != nil {
		t.Fatalf("make rendition: %v\n%s", err, out)
	}
	data, _ := os.ReadFile(m4a)
	e.blobs.Put("tracks/"+withFile+"/high.m4a", data)

	maint := NewMaintenance(e.pool, e.store, rs, e.blobs, time.Hour, "ffmpeg")
	state := func(id string) (lufs *float64, checked *time.Time) {
		if err := e.pool.QueryRow(ctx, `SELECT loudness_lufs, loudness_checked_at FROM tracks WHERE id=$1`, id).Scan(&lufs, &checked); err != nil {
			t.Fatal(err)
		}
		return
	}
	for i := 0; i < 30; i++ { // a few tracks are measured per run; other tests' tracks may be ahead of ours
		maint.RunOnce(ctx)
		if _, c1 := state(withFile); c1 != nil {
			if _, c2 := state(noFile); c2 != nil {
				break
			}
		}
	}
	lufs, checked := state(withFile)
	if lufs == nil || *lufs < -22.5 || *lufs > -19.5 || checked == nil {
		t.Fatalf("measured track: %v / %v, want about -21 LUFS and a check time", lufs, checked)
	}
	lufs, checked = state(noFile)
	if lufs != nil || checked == nil {
		t.Fatalf("a track with no file must be marked checked (not retried forever) and stay unknown: %v / %v", lufs, checked)
	}
}

func TestExpiredMultipartUploadsAreAborted(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	u, err := e.store.Create(ctx, upload.NewUpload{
		UserID: e.uid, Title: "Big", Filename: "big.wav", SizeBytes: 30_000_000, TermsVersion: "v", PartSize: 8 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	mp, err := e.blobs.StartMultipart(ctx, u.OriginalKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.SetMultipart(ctx, u.ID, mp); err != nil {
		t.Fatal(err)
	}
	e.blobs.PutPart(mp, 1, make([]byte, 100))
	if _, err := e.pool.Exec(ctx, `UPDATE uploads SET created_at = now() - interval '5 hours' WHERE id=$1`, u.ID); err != nil {
		t.Fatal(err)
	}

	rs := release.NewStore(e.pool, e.blobs, "http://api.test")
	NewMaintenance(e.pool, e.store, rs, e.blobs, time.Hour, "ffmpeg").RunOnce(ctx)

	if e.blobs.OpenUploads() != 0 {
		t.Fatalf("an abandoned multipart upload must be aborted, %d still open", e.blobs.OpenUploads())
	}
	if st, _ := e.store.Get(ctx, u.ID); st.Status != upload.StatusFailed {
		t.Fatalf("status = %s, want failed", st.Status)
	}
}

func TestProcessRefusesAFileBiggerThanDeclared(t *testing.T) {
	e := setup(t)
	job := e.submit("", tone(t, 2, 2))
	e.blobs.Put(job.OriginalKey, append(tone(t, 2, 2), make([]byte, 5000)...)) // swapped for a bigger file after queueing
	var inv *media.InvalidMediaError
	if err := e.w.Process(context.Background(), job); !errors.As(err, &inv) {
		t.Fatalf("err = %v, want InvalidMediaError", err)
	}
}
