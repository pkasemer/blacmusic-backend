package image

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/storage"
)

// storedSize is what the processed sizes of a picture take together.
func storedSize(variants map[string][]byte) (n int64) {
	for _, v := range variants {
		n += int64(len(v))
	}
	return n
}

// MeasureStored records the stored size of up to limit finished pictures that have none yet (those made before it
// was recorded), by asking storage. A size that is missing counts as nothing; any other storage error stops the run,
// and the picture is tried again later.
func MeasureStored(ctx context.Context, pool *pgxpool.Pool, blobs interface {
	Stat(ctx context.Context, key string) (int64, error)
}, limit int) (int, error) {
	rows, err := pool.Query(ctx, `SELECT id FROM images WHERE status = 'ready' AND stored_bytes IS NULL ORDER BY created_at, id LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	done := 0
	for _, id := range ids {
		var total int64
		for _, size := range Sizes {
			n, err := blobs.Stat(ctx, variantKey(id, size))
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			if err != nil {
				return done, err
			}
			total += n
		}
		if _, err := pool.Exec(ctx, `UPDATE images SET stored_bytes = $2 WHERE id = $1`, id, total); err != nil {
			return done, err
		}
		done++
	}
	return done, nil
}
