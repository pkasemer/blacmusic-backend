package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/image"
	"github.com/blacmusic/backend/internal/release"
	"github.com/blacmusic/backend/internal/upload"
	"github.com/blacmusic/backend/internal/ytdlp"
)

// Fetch downloads a thumbnail. An error wrapping ErrPermanent means retrying
// will not help (the image is gone or unusable): the release just gets no cover.
type Fetch func(ctx context.Context, rawURL string) ([]byte, error)

// ErrPermanent marks a failure that retrying cannot fix.
var ErrPermanent = errors.New("permanent")

// thumbnailHosts are the only hosts a thumbnail is fetched from: the addresses
// the YouTube API hands out. Anything else is refused so a crafted URL cannot
// make the server call arbitrary hosts.
var thumbnailHosts = []string{"i.ytimg.com", "i9.ytimg.com", "img.youtube.com", "yt3.ggpht.com", "yt3.googleusercontent.com", "lh3.googleusercontent.com"}

// DefaultFetch fetches thumbnails over HTTPS from the allowed YouTube hosts.
func DefaultFetch(client *http.Client) Fetch {
	return func(ctx context.Context, raw string) ([]byte, error) {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || !hostAllowed(u.Hostname()) {
			return nil, fmt.Errorf("%w: thumbnail host not allowed", ErrPermanent)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return nil, fmt.Errorf("%w: thumbnail answered %d", ErrPermanent, resp.StatusCode)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("thumbnail answered %d", resp.StatusCode)
		}
		return io.ReadAll(io.LimitReader(resp.Body, image.MaxUploadBytes))
	}
}

func hostAllowed(h string) bool {
	h = strings.ToLower(h)
	for _, a := range thumbnailHosts {
		if h == a {
			return true
		}
	}
	return false
}

// Runner works through the queue: it starts waiting transfers (up to the
// concurrency limit), and advances running ones a little on every tick. Every
// step records its result before moving on, so a restart continues where it stopped.
type Runner struct {
	m        *Manager
	pool     *pgxpool.Pool
	blobs    image.Blobs
	releases Releases
	fetch    Fetch
	fetcher  ytdlp.Fetcher // nil: songs cannot be fetched from YouTube

	WorkerID    string
	Concurrency int
	Interval    time.Duration
	Lease       time.Duration
}

// NewRunner creates the runner.
func NewRunner(m *Manager, pool *pgxpool.Pool, blobs image.Blobs, releases Releases, fetch Fetch, workerID string, concurrency int) *Runner {
	return &Runner{m: m, pool: pool, blobs: blobs, releases: releases, fetch: fetch,
		WorkerID: workerID, Concurrency: max(1, concurrency), Interval: 2 * time.Second, Lease: 2 * time.Minute}
}

// SetFetcher turns on fetching audio from the artist's YouTube videos.
func (r *Runner) SetFetcher(f ytdlp.Fetcher) { r.fetcher = f }

// Run loops until ctx is cancelled.
func (r *Runner) Run(ctx context.Context) {
	t := time.NewTicker(r.Interval)
	defer t.Stop()
	for {
		r.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick runs one round: recover, claim, advance. Exposed for tests.
func (r *Runner) Tick(ctx context.Context) {
	if err := r.m.Reclaim(ctx); err != nil {
		slog.Error("transfer reclaim", "err", err)
		return
	}
	for {
		id, err := r.m.Claim(ctx, r.WorkerID, r.Concurrency, r.Lease)
		if err != nil {
			slog.Error("transfer claim", "err", err)
			break
		}
		if id == "" {
			break
		}
		slog.Info("transfer started", "id", id)
	}
	ids, err := r.m.Running(ctx, r.WorkerID)
	if err != nil {
		slog.Error("transfer list running", "err", err)
		return
	}
	for _, id := range ids {
		if err := r.advance(ctx, id); err != nil {
			t, lerr := r.m.loadByID(ctx, id)
			if lerr != nil {
				slog.Error("transfer advance", "id", id, "err", err)
				continue
			}
			slog.Warn("transfer step failed, will retry", "id", id, "attempt", t.Attempt, "err", err)
			if rerr := r.m.retrying(ctx, id, t.Attempt, t.MaxAttempts, err); rerr != nil {
				slog.Error("transfer schedule retry", "id", id, "err", rerr)
			}
		}
	}
}

func (r *Runner) setItem(ctx context.Context, id, status, errMsg string) error {
	_, err := r.pool.Exec(ctx, `UPDATE transfer_items SET status = $2, error = $3 WHERE id = $1`, id, status, errMsg)
	return err
}

func terminal(status string) bool {
	switch status {
	case ItemDone, ItemFailed, ItemSkipped, ItemDropped:
		return true
	}
	return false
}

// advance moves one running transfer forward. A returned error is transient.
func (r *Runner) advance(ctx context.Context, id string) error {
	if ok, err := r.m.Extend(ctx, id, r.WorkerID, r.Lease); err != nil || !ok {
		return err
	}
	t, err := r.m.loadByID(ctx, id)
	if err != nil {
		return err
	}

	// 0. songs whose audio the server fetches from YouTube: one per round
	if fetched, err := r.fetchNext(ctx, t); err != nil {
		return err
	} else if fetched {
		if t.Items, err = r.m.loadItems(ctx, id); err != nil {
			return err
		}
	}

	// 1. follow each song's upload
	needsAudio, stillFetching := 0, false
	for i := range t.Items {
		it := &t.Items[i]
		if terminal(it.Status) {
			continue
		}
		switch {
		case it.UploadID == "":
			if it.Status == ItemFetching {
				stillFetching = true
			} else {
				needsAudio++ // fetching failed: waits for a file from the artist
			}
			continue
		case it.UploadStatus == string(upload.StatusFailed):
			it.Status, it.Error = ItemFailed, orDefault(it.UploadError, "the audio could not be processed")
		case it.UploadStatus == string(upload.StatusReady):
			if it.Status != ItemCover && it.Status != ItemPublishing {
				it.Status = ItemCover
			}
			if it.TrackID == "" {
				it.TrackID = it.UploadTrackID
				if _, err := r.pool.Exec(ctx, `UPDATE transfer_items SET track_id = $2 WHERE id = $1`, it.ID, it.TrackID); err != nil {
					return err
				}
			}
		case it.UploadStatus == string(upload.StatusPending):
			it.Status = ItemUploading
		default:
			it.Status = ItemProcessing
		}
		if err := r.setItem(ctx, it.ID, it.Status, it.Error); err != nil {
			return err
		}
	}

	// songs that could not be fetched go back to the artist, who attaches files and submits again
	if needsAudio > 0 && !stillFetching {
		slog.Info("transfer needs audio from the artist", "id", id, "songs", needsAudio)
		return r.m.pauseForAudio(ctx, id, needsAudio)
	}

	// 2. per release: cover, then publish once every song of it is ready
	releases := map[string][]*Item{}
	var order []string
	for i := range t.Items {
		it := &t.Items[i]
		if it.ReleaseID == "" || it.Status == ItemSkipped || it.Status == ItemDropped {
			continue
		}
		if _, ok := releases[it.ReleaseID]; !ok {
			order = append(order, it.ReleaseID)
		}
		releases[it.ReleaseID] = append(releases[it.ReleaseID], it)
	}
	for _, rid := range order {
		items := releases[rid]
		ready, failed := true, false
		for _, it := range items {
			switch it.Status {
			case ItemFailed:
				failed = true
			case ItemCover, ItemPublishing, ItemDone:
			default:
				ready = false
			}
		}
		if failed || !ready {
			continue
		}
		if err := r.coverAndPublish(ctx, t, rid, items); err != nil {
			return err
		}
	}

	// 3. finished?
	reload, err := r.m.loadByID(ctx, id)
	if err != nil {
		return err
	}
	// a release with a failed song cannot be published, so its other songs are settled too
	blocked := map[string]bool{}
	for _, it := range reload.Items {
		if it.Status == ItemFailed && it.ReleaseID != "" {
			blocked[it.ReleaseID] = true
		}
	}
	done, failed := true, 0
	for _, it := range reload.Items {
		if !terminal(it.Status) && !blocked[it.ReleaseID] {
			done = false
		}
		if it.Status == ItemFailed {
			failed++
		}
	}
	if !done {
		return nil
	}
	if failed > 0 {
		return r.m.fail(ctx, id, "items_failed", fmt.Sprintf("%d song(s) failed; fix them and retry", failed))
	}
	slog.Info("transfer completed", "id", id)
	return r.m.complete(ctx, id)
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func (r *Runner) coverAndPublish(ctx context.Context, t *Transfer, releaseID string, items []*Item) error {
	// cover: once per release, from the first song's thumbnail
	if !items[0].CoverDone {
		coverID := ""
		thumb := ""
		for _, it := range items {
			if it.ThumbnailURL != "" {
				thumb = it.ThumbnailURL
				break
			}
		}
		if thumb != "" {
			raw, err := r.fetch(ctx, thumb)
			switch {
			case errors.Is(err, ErrPermanent):
				slog.Warn("thumbnail unusable, release gets no cover", "release", releaseID, "err", err)
			case err != nil:
				return err
			default:
				id, ierr := image.Ingest(ctx, r.pool, r.blobs, t.UserID, "cover", raw)
				var bad *image.InvalidImageError
				switch {
				case errors.As(ierr, &bad):
					slog.Warn("thumbnail is not a usable image, release gets no cover", "release", releaseID, "err", ierr)
				case ierr != nil:
					return ierr
				default:
					coverID = id
				}
			}
		}
		if coverID != "" {
			if _, err := r.releases.Update(ctx, releaseID, t.UserID, release.Patch{CoverImageID: &coverID}); err != nil {
				return err
			}
		}
		if _, err := r.pool.Exec(ctx, `UPDATE transfer_items SET cover_done = true, cover_image_id = NULLIF($2, '') WHERE release_id = $1 AND transfer_id = $3`,
			releaseID, coverID, t.ID); err != nil {
			return err
		}
	}
	// publish
	pending := false
	for _, it := range items {
		if it.Status != ItemDone {
			pending = true
		}
	}
	if !pending {
		return nil
	}
	for _, it := range items {
		if it.Status != ItemPublishing {
			if err := r.setItem(ctx, it.ID, ItemPublishing, ""); err != nil {
				return err
			}
		}
	}
	if _, err := r.releases.Publish(ctx, releaseID, t.UserID, nil); err != nil {
		var ve *release.ValidationError
		if errors.As(err, &ve) {
			for _, it := range items {
				if err := r.setItem(ctx, it.ID, ItemFailed, ve.Msg); err != nil {
					return err
				}
			}
			return nil
		}
		return err
	}
	for _, it := range items {
		if err := r.setItem(ctx, it.ID, ItemDone, ""); err != nil {
			return err
		}
	}
	return nil
}

const maxFetchAttempts = 3

// fetchBackoff is the wait before the next try of a song whose fetch failed for a temporary reason.
var fetchBackoff = []time.Duration{time.Minute, 5 * time.Minute}

// fetchNext fetches the audio of the first song that is due. It reports whether it did any work.
func (r *Runner) fetchNext(ctx context.Context, t *Transfer) (bool, error) {
	var item *Item
	for i := range t.Items {
		it := &t.Items[i]
		if it.Status == ItemFetching && it.UploadID == "" && (it.NextFetchAt == nil || !time.Now().Before(*it.NextFetchAt)) {
			item = it
			break
		}
	}
	if item == nil {
		return false, nil
	}
	return true, r.fetchItem(ctx, t, item)
}

func (r *Runner) toManual(ctx context.Context, itemID, reason string) error {
	_, err := r.pool.Exec(ctx, `UPDATE transfer_items SET status = $2, audio_source = 'upload', fetch_error = $3 WHERE id = $1`, itemID, ItemAwaitingAudio, reason)
	return err
}

func (r *Runner) delayFetch(ctx context.Context, itemID string, d time.Duration, countAttempt bool, reason string) error {
	_, err := r.pool.Exec(ctx, `UPDATE transfer_items SET fetch_attempts = fetch_attempts + CASE WHEN $4 THEN 1 ELSE 0 END,
		next_fetch_at = now() + make_interval(secs => $2), fetch_error = $3 WHERE id = $1`, itemID, d.Seconds(), reason, countAttempt)
	return err
}

// fetchItem downloads one song's audio with yt-dlp and hands it to the normal upload pipeline as if
// the artist had uploaded it. A song that cannot be fetched falls back to the artist's own file.
func (r *Runner) fetchItem(ctx context.Context, t *Transfer, it *Item) error {
	if r.fetcher == nil {
		return r.toManual(ctx, it.ID, "importing from YouTube is not available: please upload a file")
	}
	// a download can outlast the lease: keep it alive meanwhile
	hb, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		tick := time.NewTicker(20 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-hb.Done():
				return
			case <-tick.C:
				_, _ = r.m.Extend(hb, t.ID, r.WorkerID, r.Lease)
			}
		}
	}()

	dir, err := os.MkdirTemp("", "blac-yt-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	f, err := r.fetcher.Fetch(ctx, it.VideoID, dir)
	if err != nil {
		var fe *ytdlp.Error
		if !errors.As(err, &fe) {
			return err // cancelled: the transfer is retried
		}
		slog.Warn("fetching audio from YouTube failed", "item", it.ID, "video", it.VideoID, "permanent", fe.Permanent, "detail", fe.Detail)
		attempts := it.FetchAttempts + 1
		if fe.Permanent || attempts >= maxFetchAttempts {
			return r.toManual(ctx, it.ID, fe.Reason+". Upload a file for this song instead.")
		}
		return r.delayFetch(ctx, it.ID, fetchBackoff[min(attempts-1, len(fetchBackoff)-1)], true, fe.Reason+": trying again soon")
	}

	u, err := r.m.uploads.Create(ctx, upload.NewUpload{
		UserID: t.UserID, ReleaseID: it.ReleaseID, Title: it.Title, Genre: it.Genre, TrackNumber: it.TrackNumber,
		Explicit: it.Explicit, Filename: f.Name, SizeBytes: f.Size, TermsVersion: t.TermsVersion,
	})
	var quota *upload.QuotaError
	switch {
	case errors.As(err, &quota):
		return r.failItem(ctx, it.ID, "there is not enough storage left in your account for this song")
	case errors.Is(err, upload.ErrDailyLimit):
		return r.delayFetch(ctx, it.ID, time.Hour, false, "the daily upload limit was reached: this song will continue later")
	case errors.Is(err, upload.ErrLimit):
		return r.delayFetch(ctx, it.ID, 30*time.Second, false, "")
	case errors.Is(err, upload.ErrReleaseNotFound), errors.Is(err, upload.ErrNoArtist):
		return r.failItem(ctx, it.ID, "the release for this song no longer exists")
	case err != nil:
		return err
	}
	// remember the upload before sending the file, so a restart never creates a second one
	if _, err := r.pool.Exec(ctx, `UPDATE transfer_items SET upload_id = $2, status = $3, fetch_error = '' WHERE id = $1`, it.ID, u.ID, ItemUploading); err != nil {
		return err
	}
	if err := r.blobs.UploadFile(ctx, u.OriginalKey, f.Path, f.ContentType); err != nil {
		_ = r.m.uploads.Fail(ctx, u.ID, "the audio could not be stored")
		return nil // the song fails through its upload; the artist can retry
	}
	if _, err := r.m.uploads.Queue(ctx, u.ID, f.Size); err != nil {
		_ = r.m.uploads.Fail(ctx, u.ID, "the audio could not be queued")
	}
	return nil
}

func (r *Runner) failItem(ctx context.Context, itemID, msg string) error {
	return r.setItem(ctx, itemID, ItemFailed, msg)
}
