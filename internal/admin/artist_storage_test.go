package admin

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"testing"

	pictures "github.com/blacmusic/backend/internal/image"
	"github.com/blacmusic/backend/internal/testutil"
)

func TestArtistStorageBreakdown(t *testing.T) {
	f := setupFull(t)
	pool, ctx := f.h.pool, context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	var art string
	if err := pool.QueryRow(ctx, `INSERT INTO artists (name) VALUES ($1) RETURNING id`, testutil.Unique("Store")).Scan(&art); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM uploads WHERE artist_id = $1`, `DELETE FROM tracks WHERE artist_id = $1`, `DELETE FROM albums WHERE artist_id = $1`, `DELETE FROM artists WHERE id = $1`} {
			pool.Exec(context.Background(), q, art) //nolint:errcheck
		}
	})
	storageOf := func() map[string]any {
		t.Helper()
		code, b := call(f.r, "GET", "/admin/artists/"+art, f.adminT, nil)
		if code != 200 {
			t.Fatalf("artist = %d %v", code, b)
		}
		return b["storage"].(map[string]any)
	}
	part := func(s map[string]any, name string) (files, size int) {
		p := s["breakdown"].(map[string]any)[name].(map[string]any)
		return int(p["files"].(float64)), int(p["bytes"].(float64))
	}
	expect := func(s map[string]any, want map[string][2]int) {
		t.Helper()
		for name, w := range want {
			if files, size := part(s, name); files != w[0] || size != w[1] {
				t.Errorf("%s = %d files, %d bytes; want %d, %d", name, files, size, w[0], w[1])
			}
		}
	}

	empty := storageOf()
	expect(empty, map[string][2]int{"publishedAudio": {0, 0}, "unpublishedAudio": {0, 0}, "unfinishedUploads": {0, 0}, "playbackAudio": {0, 0}, "pictures": {0, 0}})
	if empty["usedBytes"] != float64(0) || empty["quotaBytes"] != float64(0) {
		t.Fatalf("empty = %v", empty)
	}

	// public, draft, scheduled and taken-down audio, uploads on their way, and a failed one that counts nowhere
	release := func(visibility string) string {
		return testutil.Release(t, pool, art, "R "+visibility, "album", visibility)
	}
	live, draft, scheduled, removed := release("public"), release("draft"), release("scheduled"), release("draft")
	exec(`UPDATE albums SET taken_down_at = now() WHERE id = $1`, removed)
	for rel, size := range map[string]int64{live: 1000, draft: 200, scheduled: 30, removed: 4} {
		testutil.ReadyTrack(t, pool, f.admin, art, rel, "T", size, "draft")
	}
	for status, size := range map[string]int{"pending": 5, "queued": 60, "processing": 700, "failed": 100000} {
		exec(`INSERT INTO uploads (user_id, artist_id, release_id, track_id, title, artist_name, filename, size_bytes, original_key, status)
			VALUES ($1, $2, $3, 'trk_' || $4, 't', 'a', 'f.mp3', $5, 'k', $4)`, f.admin, art, draft, status, size)
	}

	// pictures: one the artist uses, one cover shared by two releases, one on a draft, and one nothing uses
	picture := func(stored any) (id string) {
		t.Helper()
		if err := pool.QueryRow(ctx, `INSERT INTO images (owner_id, purpose, status, original_key, size_bytes, stored_bytes) VALUES ($1, 'cover', 'ready', 'k', 999999, $2) RETURNING id`,
			f.admin, stored).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	avatar, shared, onDraft := picture(11), picture(22), picture(nil) // the last one is from before sizes were recorded
	picture(5000)
	exec(`UPDATE artists SET avatar_image_id = $2 WHERE id = $1`, art, avatar)
	exec(`UPDATE albums SET cover_image_id = $2 WHERE id IN ($1, $3)`, live, shared, scheduled)
	exec(`UPDATE albums SET cover_image_id = $2 WHERE id = $1`, draft, onDraft)

	s := storageOf()
	expect(s, map[string][2]int{"publishedAudio": {1, 1000}, "unpublishedAudio": {3, 234}, "unfinishedUploads": {3, 765}, "playbackAudio": {12, 12000}, "pictures": {3, 33}})
	if s["usedBytes"] != float64(1000+234+765) {
		t.Fatalf("usedBytes = %v, want the three audio parts together", s["usedBytes"])
	}

	// the worker measures what is stored for older pictures
	for size, n := range map[string]int{"small": 1, "medium": 20, "large": 300} {
		f.blobs.Put("images/"+onDraft+"/"+size+".jpg", make([]byte, n))
	}
	for {
		n, err := pictures.MeasureStored(ctx, pool, f.blobs, 100)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	expect(storageOf(), map[string][2]int{"pictures": {3, 33 + 321}})

	// a new picture records what its three sizes take
	var raw bytes.Buffer
	if err := png.Encode(&raw, image.NewRGBA(image.Rect(0, 0, 1500, 1500))); err != nil {
		t.Fatal(err)
	}
	id, err := pictures.Ingest(ctx, pool, f.blobs, f.admin, "cover", raw.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	var recorded, actual int64
	pool.QueryRow(ctx, `SELECT stored_bytes FROM images WHERE id = $1`, id).Scan(&recorded) //nolint:errcheck
	for _, size := range pictures.Sizes {
		n, err := f.blobs.Stat(ctx, "images/"+id+"/"+size+".jpg")
		if err != nil {
			t.Fatal(err)
		}
		actual += n
	}
	if recorded != actual || recorded == 0 || recorded == int64(raw.Len()) {
		t.Fatalf("recorded %d bytes, storage has %d (the upload was %d)", recorded, actual, raw.Len())
	}
}
