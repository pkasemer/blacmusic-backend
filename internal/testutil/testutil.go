// Package testutil holds helpers for the integration tests that need a real
// Postgres. Tests skip themselves when TEST_DATABASE_URL is not set.
package testutil

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/db"
)

// Secret signs the access tokens that Token creates.
const Secret = "test-secret-test-secret-test-secret!"

// GiB is 2^30 bytes.
const GiB = int64(1) << 30

var seq atomic.Int64

// Unique returns a string that is unique within and across test runs.
func Unique(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), seq.Add(1))
}

// Pool connects to the test database and applies the migrations.
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
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
	return pool
}

// User creates an account (email verified) with the given role and removes
// everything it owns when the test ends.
func User(t *testing.T, pool *pgxpool.Pool, role string, verified bool) string {
	t.Helper()
	ctx := context.Background()
	var id string
	err := pool.QueryRow(ctx, `
		INSERT INTO users (email, password_hash, display_name, role, email_verified_at)
		VALUES ($1, 'x', 'Test', $2, CASE WHEN $3 THEN now() END) RETURNING id::text`,
		Unique("user")+"@test.local", role, verified).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		// Content is not deleted with the account (ON DELETE SET NULL), so do it here.
		pool.Exec(c, `DELETE FROM tracks WHERE artist_id IN (SELECT id FROM artists WHERE owner_user_id = $1)`, id) //nolint:errcheck
		pool.Exec(c, `DELETE FROM albums WHERE artist_id IN (SELECT id FROM artists WHERE owner_user_id = $1)`, id) //nolint:errcheck
		pool.Exec(c, `DELETE FROM artists WHERE owner_user_id = $1`, id)                                            //nolint:errcheck
		pool.Exec(c, `DELETE FROM users WHERE id = $1`, id)                                                         //nolint:errcheck
	})
	return id
}

// Token returns a bearer token for the user.
func Token(t *testing.T, userID string) string {
	t.Helper()
	tok, _, err := auth.NewTokenService(Secret, time.Minute).NewAccess(userID)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// Artist creates an artist profile owned by userID with the given free storage.
func Artist(t *testing.T, pool *pgxpool.Pool, userID, name string, freeBytes int64) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := pool.QueryRow(ctx, `INSERT INTO artists (name, owner_user_id) VALUES ($1, $2) RETURNING id`, name, userID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if freeBytes > 0 {
		if _, err := pool.Exec(ctx, `INSERT INTO storage_grants (artist_id, kind, bytes) VALUES ($1, 'free', $2)`, id, freeBytes); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// Release creates a release for an artist with the given type and visibility.
func Release(t *testing.T, pool *pgxpool.Pool, artistID, title, typ, visibility string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(), `
		INSERT INTO albums (title, artist_id, release_type, visibility) VALUES ($1, $2, $3, $4) RETURNING id`,
		title, artistID, typ, visibility).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// ReadyTrack inserts a finished track (with renditions and a ready upload of
// sizeBytes) the way the worker would, and returns the track and upload ids.
func ReadyTrack(t *testing.T, pool *pgxpool.Pool, userID, artistID, releaseID, title string, sizeBytes int64, visibility string) (trackID, uploadID string) {
	t.Helper()
	ctx := context.Background()
	err := pool.QueryRow(ctx, `
		INSERT INTO tracks (title, artist_id, album_id, duration_ms, visibility) VALUES ($1, $2, $3, 180000, $4) RETURNING id`,
		title, artistID, releaseID, visibility).Scan(&trackID)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct {
		quality string
		kbps    int
	}{{"low", 48}, {"medium", 128}, {"high", 256}} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO track_renditions (track_id, quality, media_key, codec, container, bitrate_kbps, size_bytes)
			VALUES ($1, $2, $3, 'aac', 'm4a', $4, 1000)`,
			trackID, q.quality, "tracks/"+trackID+"/"+q.quality+".m4a", q.kbps); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO track_sources (track_id, source, source_id, media_key, codec, bitrate_kbps)
		VALUES ($1, 'native', $1, $2, 'aac', 128)`, trackID, "tracks/"+trackID+"/medium.m4a"); err != nil {
		t.Fatal(err)
	}
	err = pool.QueryRow(ctx, `
		INSERT INTO uploads (user_id, artist_id, release_id, track_id, title, artist_name, filename, size_bytes, original_key, status)
		VALUES ($1, $2, $3, $4, $5, 'x', 'f.flac', $6, 'originals/' || $4, 'ready') RETURNING id`,
		userID, artistID, releaseID, trackID, title, sizeBytes).Scan(&uploadID)
	if err != nil {
		t.Fatal(err)
	}
	return trackID, uploadID
}

// SerializeOutbox makes the calling test the only one touching the global push outbox
// (the sender works every queued message, so tests of different packages must not overlap).
// The lock is released when the test ends.
func SerializeOutbox(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(7770001)`); err != nil {
		conn.Release()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		conn.Exec(context.Background(), `SELECT pg_advisory_unlock(7770001)`) //nolint:errcheck
		conn.Release()
	})
}
