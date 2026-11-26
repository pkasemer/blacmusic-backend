package storage

import (
	"context"
	"strings"

	"github.com/minio/minio-go/v7"
)

// Usage is how much a group of stored objects takes.
type Usage struct {
	Objects int64 `json:"objects"`
	Bytes   int64 `json:"bytes"`
}

// UsageByPrefix counts what is really in storage, grouped by the first part of each key ("tracks", "images", ...).
// It lists the whole bucket, so it is for an occasional measurement, not for every request.
func (s *Store) UsageByPrefix(ctx context.Context) (map[string]Usage, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // stops the listing when we leave early
	out := map[string]Usage{}
	for obj := range s.internal.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Recursive: true}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		prefix, _, found := strings.Cut(obj.Key, "/")
		if !found {
			prefix = ""
		}
		u := out[prefix]
		u.Objects++
		u.Bytes += obj.Size
		out[prefix] = u
	}
	return out, ctx.Err()
}
