package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/blacmusic/backend/internal/httpx"
	"github.com/blacmusic/backend/internal/storage"
	"github.com/blacmusic/backend/internal/sysmetrics"
)

// How the server is doing: its load, memory and disk, what the app stores, and the work that is waiting.
func (h *Handler) systemRoutes(r chi.Router) {
	r.Get("/system", h.system)
	r.Get("/system/history", h.systemHistory)
}

type usedOf struct {
	TotalBytes  int64   `json:"totalBytes"`
	UsedBytes   int64   `json:"usedBytes"`
	FreeBytes   int64   `json:"freeBytes"`
	UsedPercent float64 `json:"usedPercent"`
}

type serverView struct {
	CPUCount int `json:"cpuCount"`
	Load     struct {
		One     float64 `json:"1m"`
		Five    float64 `json:"5m"`
		Fifteen float64 `json:"15m"`
	} `json:"load"`
	Memory        usedOf `json:"memory"`
	Swap          usedOf `json:"swap"`
	Disk          usedOf `json:"disk"`
	UptimeSeconds int64  `json:"uptimeSeconds"`
}

type storageView struct {
	// MeasuredAt, Objects, ObjectBytes and ByKind come from the worker's hourly count; null until the first one.
	MeasuredAt    *time.Time               `json:"measuredAt"`
	Objects       *int64                   `json:"objects"`
	ObjectBytes   *int64                   `json:"objectBytes"`
	ByKind        map[string]storage.Usage `json:"byKind"`
	DatabaseBytes int64                    `json:"databaseBytes"`
	// OtherBytes is the rest of the used disk: the system, program images, logs and anything else on the server.
	OtherBytes *int64 `json:"otherBytes"`
}

type artistUse struct {
	ArtistID string `json:"artistId"`
	Name     string `json:"name"`
	Tracks   int    `json:"tracks"`
	Bytes    int64  `json:"bytes"`
}

type queueView struct {
	Waiting      int `json:"waiting"`
	FailedLast24 int `json:"failedLast24h"`
}

type workView struct {
	Uploads struct {
		Pending             int `json:"pending"` // started, file not sent yet
		Queued              int `json:"queued"`
		Processing          int `json:"processing"`
		FailedLast24        int `json:"failedLast24h"`
		OldestQueuedSeconds int `json:"oldestQueuedSeconds"`
	} `json:"uploads"`
	Imports struct {
		AwaitingFiles int `json:"awaitingFiles"`
		Processing    int `json:"processing"`
	} `json:"imports"`
	Emails queueView `json:"emails"`
	Push   queueView `json:"push"`
}

type alert struct {
	Level   string `json:"level"` // warning | critical
	Code    string `json:"code"`
	Message string `json:"message"`
}

type systemView struct {
	MeasuredAt   time.Time   `json:"measuredAt"`
	Server       *serverView `json:"server"` // null where the machine cannot be measured
	Storage      storageView `json:"storage"`
	TopArtists   []artistUse `json:"topArtists"`
	Work         workView    `json:"work"`
	LastSampleAt *time.Time  `json:"lastSampleAt"` // the worker's latest reading
	Alerts       []alert     `json:"alerts"`
}

const gib = 1 << 30

// alerts says what needs attention.
func alerts(host *sysmetrics.Host, w workView, lastSample *time.Time, now time.Time) []alert {
	out := []alert{}
	add := func(level, code, msg string) { out = append(out, alert{level, code, msg}) }
	if host != nil {
		free := fmt.Sprintf("%.1f GB of disk space is free (%.0f%% used).", float64(host.DiskFree)/gib, host.DiskUsedPercent())
		switch used := host.DiskUsedPercent(); {
		case host.DiskFree < 2*gib || used >= 92:
			add("critical", "disk_low", free+" Uploads will soon fail.")
		case host.DiskFree < 5*gib || used >= 80:
			add("warning", "disk_low", free)
		}
		switch avail := 100 - host.MemUsedPercent(); {
		case avail < 10:
			add("critical", "memory_low", fmt.Sprintf("Only %.0f%% of memory is available.", avail))
		case avail < 20:
			add("warning", "memory_low", fmt.Sprintf("%.0f%% of memory is available.", avail))
		}
		if host.Load15 > 2*float64(host.CPUCount) {
			add("warning", "load_high", fmt.Sprintf("The load has averaged %.1f over 15 minutes on %d processor(s).", host.Load15, host.CPUCount))
		}
		// the worker takes the readings, so silence means it is probably not running
		if lastSample == nil || now.Sub(*lastSample) > 3*sysmetrics.SampleEvery {
			add("critical", "worker_silent", "The background worker has not reported for 15 minutes or more: uploads may not be processed.")
		}
	}
	if w.Uploads.OldestQueuedSeconds > 30*60 {
		add("warning", "queue_slow", fmt.Sprintf("An upload has been waiting %d minutes to be processed.", w.Uploads.OldestQueuedSeconds/60))
	}
	return out
}

// GET /v1/admin/system
func (h *Handler) system(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	v := systemView{MeasuredAt: time.Now().UTC(), TopArtists: []artistUse{}}
	read := h.ReadHost
	if read == nil {
		read = sysmetrics.Read
	}
	var host *sysmetrics.Host
	if m, ok := read(); ok {
		host = &m
		s := &serverView{CPUCount: m.CPUCount, UptimeSeconds: m.UptimeSeconds,
			Memory: usedOf{m.MemTotal, m.MemTotal - m.MemAvail, m.MemAvail, m.MemUsedPercent()},
			Swap:   usedOf{TotalBytes: m.SwapTotal, UsedBytes: m.SwapTotal - m.SwapFree, FreeBytes: m.SwapFree},
			Disk:   usedOf{m.DiskTotal, m.DiskUsed, m.DiskFree, m.DiskUsedPercent()}}
		if m.SwapTotal > 0 {
			s.Swap.UsedPercent = float64(int64(float64(s.Swap.UsedBytes)/float64(m.SwapTotal)*1000+0.5)) / 10
		}
		s.Load.One, s.Load.Five, s.Load.Fifteen = m.Load1, m.Load5, m.Load15
		v.Server = s
	}
	if err := h.systemData(ctx, &v); err != nil {
		reply(w, err)
		return
	}
	if host != nil && v.Storage.ObjectBytes != nil {
		other := max(host.DiskUsed-*v.Storage.ObjectBytes-v.Storage.DatabaseBytes, 0)
		v.Storage.OtherBytes = &other
	}
	v.Alerts = alerts(host, v.Work, v.LastSampleAt, v.MeasuredAt)
	httpx.JSON(w, http.StatusOK, v)
}

// storageKinds names the parts of the bucket. Originals brought in by a bulk import stay under "imports/".
var storageKinds = map[string]string{"originals": "audioOriginals", "imports": "audioOriginals", "tracks": "audioPlayback", "images": "images"}

func (h *Handler) systemData(ctx context.Context, v *systemView) error {
	if err := h.pool.QueryRow(ctx, `SELECT pg_database_size(current_database()), (SELECT max(at) FROM server_samples)`).
		Scan(&v.Storage.DatabaseBytes, &v.LastSampleAt); err != nil {
		return err
	}
	var at time.Time
	var raw []byte
	err := h.pool.QueryRow(ctx, `SELECT at, objects FROM server_samples WHERE objects IS NOT NULL ORDER BY at DESC LIMIT 1`).Scan(&at, &raw)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return err
	default:
		var byPrefix map[string]storage.Usage
		if err := json.Unmarshal(raw, &byPrefix); err != nil {
			return err
		}
		var objects, bytes int64
		byKind := map[string]storage.Usage{"audioOriginals": {}, "audioPlayback": {}, "images": {}, "other": {}}
		for prefix, u := range byPrefix {
			kind := storageKinds[prefix]
			if kind == "" {
				kind = "other"
			}
			sum := byKind[kind]
			sum.Objects, sum.Bytes = sum.Objects+u.Objects, sum.Bytes+u.Bytes
			byKind[kind] = sum
			objects, bytes = objects+u.Objects, bytes+u.Bytes
		}
		v.Storage.MeasuredAt, v.Storage.Objects, v.Storage.ObjectBytes, v.Storage.ByKind = &at, &objects, &bytes, byKind
	}

	rows, err := h.pool.Query(ctx, `
		WITH use AS (
			SELECT artist_id, sum(size_bytes) AS bytes FROM uploads WHERE status = 'ready' AND artist_id IS NOT NULL GROUP BY artist_id
			UNION ALL
			SELECT t.artist_id, sum(tr.size_bytes) FROM track_renditions tr JOIN tracks t ON t.id = tr.track_id GROUP BY t.artist_id
		)
		SELECT a.id, a.name, (SELECT count(*) FROM tracks WHERE artist_id = a.id), sum(use.bytes)::bigint
		FROM use JOIN artists a ON a.id = use.artist_id GROUP BY a.id, a.name ORDER BY 4 DESC, a.id LIMIT 10`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var a artistUse
		if err := rows.Scan(&a.ArtistID, &a.Name, &a.Tracks, &a.Bytes); err != nil {
			return err
		}
		v.TopArtists = append(v.TopArtists, a)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	u := &v.Work.Uploads
	if err := h.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'pending'), count(*) FILTER (WHERE status = 'queued'), count(*) FILTER (WHERE status = 'processing'),
		       count(*) FILTER (WHERE status = 'failed' AND updated_at > now() - interval '24 hours'),
		       COALESCE(EXTRACT(EPOCH FROM now() - min(updated_at) FILTER (WHERE status = 'queued')), 0)::int
		FROM uploads`).Scan(&u.Pending, &u.Queued, &u.Processing, &u.FailedLast24, &u.OldestQueuedSeconds); err != nil {
		return err
	}
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status = 'awaiting_files'), count(*) FILTER (WHERE status = 'processing') FROM import_jobs`).
		Scan(&v.Work.Imports.AwaitingFiles, &v.Work.Imports.Processing); err != nil {
		return err
	}
	for table, q := range map[string]*queueView{"email_messages": &v.Work.Emails, "push_messages": &v.Work.Push} {
		if err := h.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status = 'pending'),
			count(*) FILTER (WHERE status = 'failed' AND created_at > now() - interval '24 hours') FROM `+table).Scan(&q.Waiting, &q.FailedLast24); err != nil {
			return err
		}
	}
	return nil
}

type historyPoint struct {
	At                time.Time `json:"at"`
	Load              float64   `json:"load"` // the highest 5-minute load in the step
	MemoryUsedPercent float64   `json:"memoryUsedPercent"`
	DiskUsedBytes     int64     `json:"diskUsedBytes"`
	DiskFreeBytes     int64     `json:"diskFreeBytes"`
	UploadsWaiting    int       `json:"uploadsWaiting"` // queued or being processed, the most in the step
}

// GET /v1/admin/system/history?hours=24 (1-720): at most about 300 points, each covering stepSeconds.
func (h *Handler) systemHistory(w http.ResponseWriter, r *http.Request) {
	hours := 24
	if s := r.URL.Query().Get("hours"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 720 {
			reply(w, invalid("hours must be between 1 and 720"))
			return
		}
		hours = n
	}
	step := max(int(sysmetrics.SampleEvery.Seconds()), hours*3600/300)
	rows, err := h.pool.Query(r.Context(), `
		SELECT date_bin(make_interval(secs => $2), at, TIMESTAMPTZ '2020-01-01 00:00:00+00') AS step,
		       max(load5)::float8,
		       max(CASE WHEN mem_total > 0 THEN round(100.0 * (mem_total - mem_available) / mem_total, 1) ELSE 0 END)::float8,
		       max(disk_used), min(disk_free), max(uploads_queued + uploads_processing)
		FROM server_samples WHERE at > now() - make_interval(hours => $1) GROUP BY step ORDER BY step`, hours, float64(step))
	if err != nil {
		reply(w, err)
		return
	}
	defer rows.Close()
	points := []historyPoint{}
	for rows.Next() {
		var p historyPoint
		if err := rows.Scan(&p.At, &p.Load, &p.MemoryUsedPercent, &p.DiskUsedBytes, &p.DiskFreeBytes, &p.UploadsWaiting); err != nil {
			reply(w, err)
			return
		}
		p.Load = float64(int64(p.Load*100+0.5)) / 100
		points = append(points, p)
	}
	if err := rows.Err(); err != nil {
		reply(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"hours": hours, "stepSeconds": step, "points": points})
}
