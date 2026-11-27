package admin

import (
	"testing"

	"github.com/blacmusic/backend/internal/testutil"
)

func TestCombineReleasesIntoOneAlbum(t *testing.T) {
	w := newWorld(t)
	art := w.catalogArtist("Combine Artist")
	pool := w.h.pool
	single := func(title, visibility string) (rel, track string) {
		rel = testutil.Release(t, pool, art, title, "single", visibility)
		track, _ = testutil.ReadyTrack(t, pool, w.admin, art, rel, title, 100, visibility)
		return rel, track
	}
	a, ta := single("A", "public")
	b, tb := single("B", "draft")
	c, tc := single("C", "public")
	cover := w.picture(w.admin, "cover")
	w.exec(`UPDATE albums SET cover_image_id = $2, artwork_url = 'http://x/cover', genre = 'Rap' WHERE id = $1`, b, cover)
	w.exec(`UPDATE albums SET published_at = now() - interval '9 days' WHERE id = $1`, a)
	fan := testutil.User(t, pool, "user", true)
	w.exec(`INSERT INTO library_albums (user_id, album_id) VALUES ($1, $2), ($1, $3)`, fan, a, c)
	w.exec(`INSERT INTO plays (track_id, release_id, artist_id, listened_sec) VALUES ($1, $2, $3, 40)`, ta, a, art)
	// "Everyone" is on all three releases, "Only A" on one
	for _, rel := range []string{a, b, c} {
		w.exec(`INSERT INTO release_credits (release_id, name, role) VALUES ($1, 'Everyone', 'producer')`, rel)
	}
	w.exec(`INSERT INTO release_credits (release_id, name, role) VALUES ($1, 'Only A', 'writer')`, a)
	t.Cleanup(func() { w.exec(`DELETE FROM plays WHERE artist_id = $1`, art) })

	body := func(change map[string]any) map[string]any {
		out := map[string]any{"releaseIds": []string{a, b, c}, "trackIds": []string{tc, ta, tb}, "title": " The Album ", "coverFromReleaseId": b, "visibility": "public"}
		for k, v := range change {
			out[k] = v
		}
		return out
	}
	combine := func(change map[string]any) (int, map[string]any) {
		return call(w.r, "POST", "/admin/releases/combine", w.adminT, body(change))
	}

	other := w.catalogArtist("Other Artist")
	foreign := testutil.Release(t, pool, other, "F", "single", "draft")
	hollow := testutil.Release(t, pool, art, "Hollow", "single", "draft")
	for name, x := range map[string]struct {
		change map[string]any
		status int
		code   string
	}{
		"one release":      {map[string]any{"releaseIds": []string{a}}, 422, "validation_error"},
		"a track missing":  {map[string]any{"trackIds": []string{ta, tb}}, 422, "validation_error"},
		"a track twice":    {map[string]any{"trackIds": []string{ta, tb, tc, tc}}, 422, "validation_error"},
		"two covers":       {map[string]any{"coverImageId": cover}, 422, "validation_error"},
		"bad visibility":   {map[string]any{"visibility": "scheduled"}, 422, "validation_error"},
		"unknown release":  {map[string]any{"releaseIds": []string{a, b, "alb_nothing"}, "coverFromReleaseId": ""}, 404, "not_found"},
		"two artists":      {map[string]any{"releaseIds": []string{a, b, foreign}}, 409, "different_artists"},
		"an empty release": {map[string]any{"releaseIds": []string{a, b, c, hollow}}, 409, "no_tracks"},
	} {
		if code, got := combine(x.change); code != x.status || errCode(got) != x.code {
			t.Fatalf("%s = %d %v, want %d %s", name, code, got, x.status, x.code)
		}
	}
	w.exec(`UPDATE albums SET taken_down_at = now() WHERE id = $1`, c)
	if code, got := combine(nil); code != 409 || errCode(got) != "taken_down" {
		t.Fatalf("taken down = %d %v", code, got)
	}
	w.exec(`UPDATE albums SET taken_down_at = NULL WHERE id = $1`, c)
	pending, _ := w.queueSong(art, a, `status = 'queued'`)
	if code, got := combine(nil); code != 409 || errCode(got) != "uploads_pending" {
		t.Fatalf("pending upload = %d %v", code, got)
	}
	w.exec(`DELETE FROM uploads WHERE id = $1`, pending)
	files := len(w.blobs.Keys())

	code, got := combine(nil)
	if code != 201 || got["type"] != "album" || got["kind"] != "ep" || got["title"] != "The Album" || got["visibility"] != "public" || got["trackCount"] != float64(3) || got["coverUrl"] != "http://x/cover" {
		t.Fatalf("combine = %d %v", code, got)
	}
	id := got["id"].(string)
	for i, want := range []string{tc, ta, tb} {
		row := got["tracks"].([]any)[i].(map[string]any)
		if row["id"] != want || row["trackNumber"] != float64(i+1) || row["visibility"] != "public" {
			t.Fatalf("track %d = %v, want %s", i+1, row, want)
		}
	}
	for what, q := range map[string]string{
		"old releases gone":       `SELECT (count(*) = 0)::text FROM albums WHERE id IN ($2, $3, $4) AND $1 <> ''`,
		"cover kept":              `SELECT (cover_image_id IS NOT NULL AND published_at < now() - interval '8 days')::text FROM albums WHERE id = $1 AND $2 <> $3 AND $4 <> ''`,
		"saved once":              `SELECT (count(*) = 1)::text FROM library_albums WHERE album_id = $1 AND $2 <> $3 AND $4 <> ''`,
		"plays follow":            `SELECT (count(*) = 1)::text FROM plays WHERE release_id = $1 AND $2 <> $3 AND $4 <> ''`,
		"shared credit once":      `SELECT (count(*) = 1)::text FROM release_credits WHERE release_id = $1 AND track_id IS NULL AND name = 'Everyone' AND $2 <> $3 AND $4 <> ''`,
		"other credit on a track": `SELECT (count(*) = 1 AND bool_and(track_id IS NOT NULL AND name = 'Only A'))::text FROM release_credits WHERE release_id = $1 AND (track_id IS NOT NULL OR name = 'Only A') AND $2 <> $3 AND $4 <> ''`,
	} {
		if w.text(q, id, a, b, c) != "true" {
			t.Fatalf("not true after combining: %s", what)
		}
	}
	if len(w.blobs.Keys()) != files {
		t.Fatal("combining must not delete any file")
	}
	if w.audit("release.combine", id) != 1 {
		t.Fatal("combining was not recorded")
	}
}
