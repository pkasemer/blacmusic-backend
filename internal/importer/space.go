package importer

import (
	"context"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/blacmusic/backend/internal/httpx"
)

// The disk is shared with the database, so an import must never fill it. A finished song takes a little over three
// times the audio sent: the original and the playback copies (about 2.1 times the original, measured). The factors
// below leave room to spare. The check counts what other unfinished imports will still need, not only this one:
//
//   - creating a job: everything this job and every unfinished job will still put on disk;
//   - new upload links: the files this job has still to send, plus what the started jobs will still need;
//   - starting a job: the playback copies of this job (its originals are on disk by then) and of the started jobs.
//
// Jobs that still wait for their files do not count against a start: otherwise a full queue could never move.
const (
	wholeSong = 7 // halves of the audio size: original + playback copies, 3.5 times
	copiesDue = 5 // halves of the audio size: the playback copies still to be made, 2.5 times
)

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// lockSpace makes checks that reserve disk space take turns, so two requests cannot both count the same free space.
func lockSpace(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('import-disk-space', 24))`)
	return err
}

// reserved is the disk space unfinished jobs other than except will still take: the started ones, and with waiting
// also those whose files have not arrived.
func reserved(ctx context.Context, q querier, except string, waiting bool) (int64, error) {
	var n int64
	err := q.QueryRow(ctx, `
		SELECT COALESCE((
		           SELECT sum(GREATEST(f.actual_bytes, f.size_bytes)) * `+strconv.Itoa(copiesDue)+` / 2
		           FROM import_jobs j
		           JOIN import_items i ON i.job_id = j.id AND i.status IN ('pending', 'processing')
		           JOIN import_files f ON f.job_id = j.id AND f.name = i.audio_file
		           WHERE j.status = 'processing' AND j.id <> $1), 0)::bigint
		     + COALESCE((
		           SELECT sum(CASE WHEN f.kind = 'audio' THEN f.size_bytes * `+strconv.Itoa(wholeSong)+` / 2 ELSE f.size_bytes END)
		           FROM import_jobs j JOIN import_files f ON f.job_id = j.id
		           WHERE $2 AND j.status = 'awaiting_files' AND j.id <> $1), 0)::bigint`, except, waiting).Scan(&n)
	return n, err
}

// room says whether need more bytes fit beside what is reserved while keeping the margin free. When they do not it
// answers 507 and returns false. Where the disk cannot be measured, or the check is off, everything fits.
func (h *Handler) room(w http.ResponseWriter, need, others int64) bool {
	if h.opt.MinFreeBytes <= 0 {
		return true
	}
	free, ok := h.opt.FreeBytes()
	if !ok || free-others-need >= h.opt.MinFreeBytes {
		return true
	}
	httpx.ErrorDetails(w, http.StatusInsufficientStorage, "insufficient_storage",
		"the server does not have enough free disk space for this import, counting the imports already waiting or running",
		map[string]int64{"freeBytes": free, "needBytes": need, "reservedBytes": others, "keepFreeBytes": h.opt.MinFreeBytes})
	return false
}

// roomToSend checks, before upload links are handed out again, that what a job has still to send fits beside the
// started jobs. A job with nothing left to send always passes.
func (h *Handler) roomToSend(w http.ResponseWriter, r *http.Request, jobID string) bool {
	files, err := jobFiles(r.Context(), h.pool, jobID)
	if err != nil {
		serverError(w, err)
		return false
	}
	var unsent int64
	for _, f := range files {
		done, _, err := h.state(r.Context(), f)
		if err != nil {
			serverError(w, err)
			return false
		}
		if !done {
			unsent += f.size
		}
	}
	if unsent == 0 {
		return true
	}
	started, err := reserved(r.Context(), h.pool, jobID, false)
	if err != nil {
		serverError(w, err)
		return false
	}
	return h.room(w, unsent, started)
}
