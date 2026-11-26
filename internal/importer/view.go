package importer

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/blacmusic/backend/internal/storage"
)

func sortedParts(parts []storage.Part) []storage.Part {
	out := slices.Clone(parts)
	slices.SortFunc(out, func(a, b storage.Part) int { return a.Number - b.Number })
	return out
}

type resultRow struct {
	Title           string `json:"title"`
	Status          string `json:"status"` // waiting | processing | created | skippedDuplicate | failed
	ReleaseID       string `json:"releaseId,omitempty"`
	ReleaseURL      string `json:"releaseUrl,omitempty"`
	TrackID         string `json:"trackId,omitempty"`
	DurationSeconds int    `json:"durationSeconds,omitempty"`
	Duration        string `json:"duration,omitempty"`
	Note            string `json:"note,omitempty"`
}

type errorRow struct {
	Title   string `json:"title,omitempty"`
	File    string `json:"file,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type jobView struct {
	JobID            string      `json:"jobId"`
	Status           string      `json:"status"` // pending (waiting for files) | processing | completed | completed_with_errors | failed | cancelled
	Artist           string      `json:"artist"`
	ArtistID         string      `json:"artistId,omitempty"`
	ArtistURL        string      `json:"artistUrl,omitempty"`
	Type             string      `json:"type"`
	Visibility       string      `json:"visibility"`
	TotalTracks      int         `json:"totalTracks"`
	Created          int         `json:"created"`
	SkippedDuplicate int         `json:"skippedDuplicate"`
	Failed           int         `json:"failed"`
	Remaining        int         `json:"remaining"`
	BytesStored      int64       `json:"bytesStored"`
	StatusURL        string      `json:"statusUrl,omitempty"`
	CreatedAt        time.Time   `json:"createdAt"`
	StartedAt        *time.Time  `json:"startedAt"`
	FinishedAt       *time.Time  `json:"finishedAt"`
	Results          []resultRow `json:"results,omitempty"`
	Errors           []errorRow  `json:"errors"`
	Warnings         []string    `json:"warnings,omitempty"`
	Files            []fileView  `json:"files,omitempty"` // while waiting: where to send each file
}

func (h *Handler) siteURL(path, id string) string {
	if h.opt.AdminSiteURL == "" || id == "" {
		return ""
	}
	return h.opt.AdminSiteURL + path + id
}

// view is the whole state of a job.
func (h *Handler) view(ctx context.Context, id string) (*jobView, error) {
	v := &jobView{JobID: id, Errors: []errorRow{}, Results: []resultRow{}}
	var status, jobError string
	var artistID *string
	if err := h.pool.QueryRow(ctx, `SELECT status, artist_id, artist_name, release_type, publish, error, warnings, created_at, started_at, finished_at FROM import_jobs WHERE id = $1`, id).
		Scan(&status, &artistID, &v.Artist, &v.Type, &v.Visibility, &jobError, &v.Warnings, &v.CreatedAt, &v.StartedAt, &v.FinishedAt); err != nil {
		return nil, err
	}
	v.Status = status
	if status == "awaiting_files" {
		v.Status = "pending"
	}
	if artistID != nil {
		v.ArtistID = *artistID
		v.ArtistURL = h.siteURL("/artists/", v.ArtistID)
	}
	rows, err := h.pool.Query(ctx, `
		SELECT i.title, i.status, i.release_id, i.track_id, i.note, i.audio_file, i.error_code, i.error_message, COALESCE(t.duration_ms, 0),
		       COALESCE((SELECT u.size_bytes FROM uploads u WHERE u.id = i.upload_id AND u.status = 'ready'), 0)
		       + COALESCE((SELECT sum(size_bytes) FROM track_renditions tr WHERE tr.track_id = NULLIF(i.track_id, '')), 0)
		FROM import_items i LEFT JOIN tracks t ON t.id = NULLIF(i.track_id, '')
		WHERE i.job_id = $1 ORDER BY i.position`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r resultRow
		var audioFile, code, msg string
		var ms int
		var stored int64
		if err := rows.Scan(&r.Title, &r.Status, &r.ReleaseID, &r.TrackID, &r.Note, &audioFile, &code, &msg, &ms, &stored); err != nil {
			return nil, err
		}
		v.TotalTracks++
		v.BytesStored += stored
		switch r.Status {
		case "created":
			v.Created++
		case "skippedDuplicate":
			v.SkippedDuplicate++
		case "failed":
			v.Failed++
			v.Errors = append(v.Errors, errorRow{Title: r.Title, File: audioFile, Code: code, Message: msg})
		default:
			v.Remaining++
			if r.Status == "pending" {
				r.Status = "waiting"
			}
		}
		if ms > 0 {
			r.DurationSeconds = (ms + 500) / 1000
			r.Duration = fmt.Sprintf("%d:%02d", r.DurationSeconds/60, r.DurationSeconds%60)
		}
		r.ReleaseURL = h.siteURL("/releases/", r.ReleaseID)
		v.Results = append(v.Results, r)
	}
	if jobError != "" {
		v.Errors = append(v.Errors, errorRow{Code: "import_error", Message: jobError})
	}
	return v, rows.Err()
}
