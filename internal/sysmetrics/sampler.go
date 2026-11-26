package sysmetrics

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/storage"
)

const (
	// SampleEvery is the time between two readings.
	SampleEvery = 5 * time.Minute
	// objectsEvery is the time between two counts of everything in storage.
	objectsEvery = time.Hour
	// keepSamples is how long readings are kept.
	keepSamples = 30 * 24 * time.Hour
)

// Objects counts what is in object storage.
type Objects interface {
	UsageByPrefix(ctx context.Context) (map[string]storage.Usage, error)
}

// Sampler records readings. It runs in the worker.
type Sampler struct {
	pool    *pgxpool.Pool
	objects Objects
	// Read measures the machine (replaced in tests).
	Read func() (Host, bool)
}

// NewSampler creates the sampler.
func NewSampler(pool *pgxpool.Pool, objects Objects) *Sampler {
	return &Sampler{pool: pool, objects: objects, Read: Read}
}

// Run takes a reading at once and then every few minutes until ctx ends.
func (s *Sampler) Run(ctx context.Context) {
	for {
		if err := s.Sample(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("server sample", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(SampleEvery):
		}
	}
}

// Sample takes one reading, counts storage when the last count is old, and forgets old readings. Where the machine
// cannot be measured it does nothing.
func (s *Sampler) Sample(ctx context.Context) error {
	h, ok := s.Read()
	if !ok {
		return nil
	}
	var counted bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM server_samples WHERE objects IS NOT NULL AND at > now() - make_interval(secs => $1))`,
		(objectsEvery - SampleEvery/2).Seconds()).Scan(&counted); err != nil {
		return err
	}
	var objects []byte
	if !counted {
		// a failed count must not lose the reading
		if usage, err := s.objects.UsageByPrefix(ctx); err != nil {
			slog.Warn("storage not counted", "err", err)
		} else if objects, err = json.Marshal(usage); err != nil {
			return err
		}
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO server_samples (load1, load5, mem_total, mem_available, disk_total, disk_used, disk_free, uploads_queued, uploads_processing, objects)
		SELECT $1, $2, $3, $4, $5, $6, $7, count(*) FILTER (WHERE status = 'queued'), count(*) FILTER (WHERE status = 'processing'), $8
		FROM uploads WHERE status IN ('queued', 'processing')`,
		h.Load1, h.Load5, h.MemTotal, h.MemAvail, h.DiskTotal, h.DiskUsed, h.DiskFree, objects); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM server_samples WHERE at < now() - make_interval(secs => $1)`, keepSamples.Seconds())
	return err
}
