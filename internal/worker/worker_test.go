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
	"github.com/blacmusic/backend/internal/db"
	"github.com/blacmusic/backend/internal/media"
	"github.com/blacmusic/backend/internal/upload"
)

// Integration test: needs Postgres and ffmpeg/ffprobe, e.g. inside Docker:
//
//	apk add ffmpeg && TEST_DATABASE_URL=postgres://... go test ./internal/worker
type env struct {
	t     *testing.T
	pool  *pgxpool.Pool
	store *upload.Store
	blobs *blobtest.Mem
	w     *Worker
	uid   string
}

func setup(t *testing.T) *env {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var uid string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users(email, password_hash, display_name, role) VALUES ($1,'x','W','creator') RETURNING id::text`,
		fmt.Sprintf("wk-%d@test.local", time.Now().UnixNano())).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Exec(ctx, `DELETE FROM users WHERE id::text = $1`, uid) }) //nolint:errcheck

	store := upload.NewStore(pool)
	blobs := blobtest.New()
	w := New(store, blobs, Config{
		FFmpeg: "ffmpeg", FFprobe: "ffprobe",
		MaxBytes: 50 << 20, MaxDuration: time.Hour, TranscodeTimeout: time.Minute,
	})
	return &env{t: t, pool: pool, store: store, blobs: blobs, w: w, uid: uid}
}

// submit creates an upload, "uploads" data as its original and claims it.
func (e *env) submit(artist, album string, data []byte) *upload.Upload {
	e.t.Helper()
	ctx := context.Background()
	u, err := e.store.Create(ctx, upload.NewUpload{
		UserID: e.uid, Title: "Test Song", ArtistName: artist, AlbumTitle: album,
		Filename: "x", SizeBytes: int64(len(data)),
	})
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() {
		e.pool.Exec(ctx, `DELETE FROM tracks WHERE id = $1`, u.TrackID)                                          //nolint:errcheck
		e.pool.Exec(ctx, `DELETE FROM albums WHERE artist_id IN (SELECT id FROM artists WHERE name=$1)`, artist) //nolint:errcheck
		e.pool.Exec(ctx, `DELETE FROM artists WHERE name = $1`, artist)                                          //nolint:errcheck
	})
	e.blobs.Put(u.OriginalKey, data)
	if _, err := e.store.Queue(ctx, u.ID); err != nil {
		e.t.Fatal(err)
	}
	// Claim the oldest queued job; on a shared DB it may belong to someone
	// else, so put those back and keep looking.
	for i := 0; i < 50; i++ {
		j, err := e.store.Claim(ctx, time.Hour, 3)
		if err != nil {
			e.t.Fatal(err)
		}
		if j == nil {
			break
		}
		if j.ID == u.ID {
			return j
		}
		_ = e.store.Requeue(ctx, j.ID)
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
	artist := fmt.Sprintf("Artist %d", time.Now().UnixNano())
	job := e.submit(artist, "The Album", tone(t, 6, 2))

	if err := e.w.Process(ctx, job); err != nil {
		t.Fatal(err)
	}

	// Track, artist, album and renditions exist, and the upload is ready.
	var title, artistName, albumTitle string
	var durMs int
	if err := e.pool.QueryRow(ctx, `
		SELECT t.title, a.name, al.title, t.duration_ms FROM tracks t
		JOIN artists a ON a.id=t.artist_id JOIN albums al ON al.id=t.album_id WHERE t.id=$1`, job.TrackID).
		Scan(&title, &artistName, &albumTitle, &durMs); err != nil {
		t.Fatal(err)
	}
	if title != "Test Song" || artistName != artist || albumTitle != "The Album" {
		t.Fatalf("metadata = %q %q %q", title, artistName, albumTitle)
	}
	if durMs < 5900 || durMs > 6300 {
		t.Fatalf("duration_ms = %d, want about 6000", durMs)
	}
	if st, _ := e.store.Get(ctx, job.ID); st.Status != upload.StatusReady {
		t.Fatalf("status = %s, want ready", st.Status)
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
	job := e.submit(fmt.Sprintf("Mono %d", time.Now().UnixNano()), "", tone(t, 3, 1))
	if err := e.w.Process(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	data, _ := e.blobs.Get("tracks/" + job.TrackID + "/low.m4a")
	if p := probe(t, data); p.Streams[0].Channels != 1 {
		t.Fatalf("mono source became %d channels", p.Streams[0].Channels)
	}
}

func TestReusesArtistAndAlbum(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	artist := fmt.Sprintf("Same Artist %d", time.Now().UnixNano())
	audio := tone(t, 2, 2)
	a := e.submit(artist, "Same Album", audio)
	if err := e.w.Process(ctx, a); err != nil {
		t.Fatal(err)
	}
	b := e.submit(artist, "same album", audio) // different case
	if err := e.w.Process(ctx, b); err != nil {
		t.Fatal(err)
	}
	var artists, albums int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM artists WHERE name=$1`, artist).Scan(&artists); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx, `SELECT count(DISTINCT album_id) FROM tracks WHERE id = ANY($1)`, []string{a.TrackID, b.TrackID}).Scan(&albums); err != nil {
		t.Fatal(err)
	}
	if artists != 1 || albums != 1 {
		t.Fatalf("artists=%d albums=%d, want 1 and 1", artists, albums)
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
		job := e.submit(fmt.Sprintf("Bad %s %d", name, time.Now().UnixNano()), "", data)
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

func TestHandleMarksInvalidUploadsFailed(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	job := e.submit(fmt.Sprintf("Handle %d", time.Now().UnixNano()), "", []byte("not audio"))
	e.w.handle(ctx, job)
	st, err := e.store.Get(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != upload.StatusFailed || st.Error == "" {
		t.Fatalf("status=%s error=%q, want failed with a message", st.Status, st.Error)
	}
}

func TestMissingOriginalIsRejected(t *testing.T) {
	e := setup(t)
	job := e.submit(fmt.Sprintf("Gone %d", time.Now().UnixNano()), "", []byte("x"))
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
	job := e.submit(fmt.Sprintf("Flaky %d", time.Now().UnixNano()), "", tone(t, 2, 2))

	e.w.handle(ctx, job)
	if st, _ := e.store.Get(ctx, job.ID); st.Status != upload.StatusQueued {
		t.Fatalf("after 1st failure status = %s, want queued (retry)", st.Status)
	}
	job.Attempts = e.w.cfg.MaxAttempts // pretend this was the last allowed try
	e.w.handle(ctx, job)
	if st, _ := e.store.Get(ctx, job.ID); st.Status != upload.StatusFailed {
		t.Fatalf("after final failure status = %s, want failed", st.Status)
	}
}
