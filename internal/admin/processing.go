package admin

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/blacmusic/backend/internal/audit"
	"github.com/blacmusic/backend/internal/httpx"
	"github.com/blacmusic/backend/internal/storage"
	"github.com/blacmusic/backend/internal/upload"
)

// The songs being processed, and the way to get a stuck one going again. These are not under /uploads, which is
// where administrators send files.
func (h *Handler) processingRoutes(r chi.Router) {
	r.Get("/processing", h.processingList)
	r.Post("/processing/{id}/retry", h.processingRetry)
}

// The states of an upload on the processing list.
const (
	stateStuck   = "stuck"   // marked as processing, but no worker has said it is on it for upload.StuckAfter
	stateWorking = "working" // a worker is on it now
	stateWaiting = "waiting" // in the queue
	stateFailed  = "failed"
)

const (
	processingMax = 200
	failedMax     = 50
	failedDays    = 7
)

type processingImport struct {
	JobID      string `json:"jobId"`
	ArtistName string `json:"artistName"`
}

type processingItem struct {
	UploadID     string     `json:"uploadId"`
	Title        string     `json:"title"`
	ArtistID     string     `json:"artistId"`
	ArtistName   string     `json:"artistName"`
	ReleaseID    string     `json:"releaseId"`
	ReleaseTitle string     `json:"releaseTitle"`
	Status       string     `json:"status"`
	State        string     `json:"state"`
	Attempts     int        `json:"attempts"`
	SizeBytes    int64      `json:"sizeBytes"`
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`    // the last change of status
	StartedAt    *time.Time `json:"startedAt"`    // when a worker picked it up
	LastSignalAt *time.Time `json:"lastSignalAt"` // when that worker last said it was still on it
	// WaitingOnTransfer: it is queued, but held until the transfer filling its release is being worked on.
	WaitingOnTransfer bool              `json:"waitingOnTransfer"`
	Import            *processingImport `json:"import"`
	Error             string            `json:"error"`
	CanRetry          bool              `json:"canRetry"`
	// CanForce: it is being worked on, and may still be restarted with force when it has clearly gone wrong.
	CanForce bool `json:"canForce"`

	originalKey string
}

// processingSelect takes $1, the seconds without a signal after which a job is stuck.
const processingSelect = `
	SELECT u.id, u.title, COALESCE(u.artist_id, ''), COALESCE(a.name, u.artist_name), COALESCE(al.id, ''), COALESCE(al.title, u.album_title),
	       u.status, u.attempts, u.size_bytes, u.created_at, u.updated_at, u.claimed_at, u.locked_at, COALESCE(u.error, ''), u.original_key,
	       CASE u.status WHEN 'queued' THEN '` + stateWaiting + `' WHEN 'failed' THEN '` + stateFailed + `'
	            WHEN 'processing' THEN CASE WHEN u.locked_at IS NULL OR u.locked_at < now() - make_interval(secs => $1)
	                 THEN '` + stateStuck + `' ELSE '` + stateWorking + `' END
	            ELSE u.status END AS state,
	       u.status = 'queued' AND EXISTS (SELECT 1 FROM transfer_items ti JOIN transfers tr ON tr.id = ti.transfer_id
	            WHERE ti.release_id = u.release_id AND ti.status NOT IN ('dropped', 'skipped_duplicate')
	              AND tr.status IN ('collecting', 'queued', 'retrying')),
	       im.job_id, im.artist_name
	FROM uploads u
	LEFT JOIN artists a ON a.id = u.artist_id
	LEFT JOIN albums al ON al.id = u.release_id
	LEFT JOIN LATERAL (SELECT j.id AS job_id, j.artist_name FROM import_items it JOIN import_jobs j ON j.id = it.job_id
	                   WHERE it.upload_id = u.id LIMIT 1) im ON true`

func (h *Handler) processingItems(ctx context.Context, rest string, args ...any) ([]*processingItem, error) {
	rows, err := h.pool.Query(ctx, processingSelect+" "+rest, append([]any{upload.StuckAfter.Seconds()}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*processingItem{}
	for rows.Next() {
		var p processingItem
		var jobID, jobArtist *string
		if err := rows.Scan(&p.UploadID, &p.Title, &p.ArtistID, &p.ArtistName, &p.ReleaseID, &p.ReleaseTitle,
			&p.Status, &p.Attempts, &p.SizeBytes, &p.CreatedAt, &p.UpdatedAt, &p.StartedAt, &p.LastSignalAt, &p.Error, &p.originalKey,
			&p.State, &p.WaitingOnTransfer, &jobID, &jobArtist); err != nil {
			return nil, err
		}
		if jobID != nil {
			p.Import = &processingImport{JobID: *jobID, ArtistName: *jobArtist}
		}
		p.CanRetry, p.CanForce = p.State == stateStuck, p.State == stateWorking
		out = append(out, &p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for _, p := range out {
		if p.State == stateFailed {
			// a failed song can be tried again only while its file is still there
			err := h.restartable(ctx, p.ReleaseID, p.originalKey)
			var refused *apiError
			if err != nil && !errors.As(err, &refused) {
				return nil, err
			}
			p.CanRetry = err == nil
		}
	}
	return out, nil
}

// restartable says why a failed upload cannot be processed again, or nil when it can.
func (h *Handler) restartable(ctx context.Context, releaseID, originalKey string) error {
	if releaseID == "" {
		return fail4(http.StatusConflict, "release_gone", "the release this song belonged to was deleted")
	}
	gone := fail4(http.StatusConflict, "file_gone", "the uploaded file is no longer stored: the song has to be uploaded again")
	if h.Blobs == nil {
		return gone
	}
	_, err := h.Blobs.Stat(ctx, originalKey)
	if errors.Is(err, storage.ErrNotFound) {
		return gone
	}
	return err
}

// GET /v1/admin/processing: every song waiting or being processed (stuck ones first, at most 200), then the ones
// that failed in the last 7 days (at most 50).
func (h *Handler) processingList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var counts struct {
		Stuck   int `json:"stuck"`
		Working int `json:"working"`
		Waiting int `json:"waiting"`
		Failed  int `json:"failed"`
	}
	var workerSeen *time.Time
	if err := h.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'processing' AND (locked_at IS NULL OR locked_at < now() - make_interval(secs => $1))),
		       count(*) FILTER (WHERE status = 'processing' AND locked_at >= now() - make_interval(secs => $1)),
		       count(*) FILTER (WHERE status = 'queued'),
		       count(*) FILTER (WHERE status = 'failed' AND updated_at > now() - make_interval(days => $2)),
		       (SELECT max(at) FROM server_samples)
		FROM uploads`, upload.StuckAfter.Seconds(), failedDays).
		Scan(&counts.Stuck, &counts.Working, &counts.Waiting, &counts.Failed, &workerSeen); err != nil {
		reply(w, err)
		return
	}
	items, err := h.processingItems(ctx, `WHERE u.status IN ('queued', 'processing')
		ORDER BY u.status = 'queued', (u.locked_at >= now() - make_interval(secs => $1)) IS TRUE, u.created_at, u.id LIMIT $2`, processingMax)
	if err != nil {
		reply(w, err)
		return
	}
	failed, err := h.processingItems(ctx, `WHERE u.status = 'failed' AND u.updated_at > now() - make_interval(days => $2)
		ORDER BY u.updated_at DESC, u.id LIMIT $3`, failedDays, failedMax)
	if err != nil {
		reply(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"measuredAt": time.Now().UTC(), "stuckAfterSeconds": int(upload.StuckAfter.Seconds()), "workerLastSeenAt": workerSeen,
		"counts": counts, "uploads": append(items, failed...),
	})
}

// POST /v1/admin/processing/{id}/retry {force?}: puts a stuck song, or a failed one whose file is still stored, back
// in the queue with all its tries. force also restarts one that a worker says it is on; that worker drops it.
func (h *Handler) processingRetry(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Force bool `json:"force"`
	}
	if err := decodeBody(w, r, &req, true); err != nil {
		reply(w, err)
		return
	}
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		reply(w, err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// The lock keeps a worker from claiming, finishing or signalling while this decides.
	var title, status, releaseID, originalKey, lastError string
	var attempts int
	var stale bool
	if err := tx.QueryRow(ctx, `
		SELECT title, status, COALESCE(release_id, ''), original_key, COALESCE(error, ''), attempts,
		       locked_at IS NULL OR locked_at < now() - make_interval(secs => $2)
		FROM uploads WHERE id = $1 FOR UPDATE`, id, upload.StuckAfter.Seconds()).
		Scan(&title, &status, &releaseID, &originalKey, &lastError, &attempts, &stale); err != nil {
		reply(w, err)
		return
	}
	from := status
	switch status {
	case "processing":
		switch {
		case stale:
			from = stateStuck
		case !req.Force:
			reply(w, fail4(http.StatusConflict, "still_processing", "a worker is still working on this song"))
			return
		default:
			from = stateWorking
		}
	case "failed":
		if err := h.restartable(ctx, releaseID, originalKey); err != nil {
			reply(w, err)
			return
		}
	case "queued":
		reply(w, fail4(http.StatusConflict, "already_queued", "this song is already waiting to be processed"))
		return
	default:
		reply(w, fail4(http.StatusConflict, "not_stuck", "this upload does not need to be processed again"))
		return
	}
	if err := upload.Restart(ctx, tx, id); err != nil {
		reply(w, err)
		return
	}
	details := map[string]any{"was": from, "attempts": attempts}
	if lastError != "" {
		details["error"] = lastError
	}
	if err := h.log(ctx, tx, r, audit.Entry{Action: "upload.retry", TargetType: "upload", TargetID: id, TargetLabel: title, Details: details}); err != nil {
		reply(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		reply(w, err)
		return
	}
	items, err := h.processingItems(ctx, `WHERE u.id = $2`, id)
	if err != nil {
		reply(w, err)
		return
	}
	if len(items) == 0 {
		reply(w, errNotFound)
		return
	}
	httpx.JSON(w, http.StatusOK, items[0])
}
