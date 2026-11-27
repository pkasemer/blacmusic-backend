package admin

import (
	"testing"

	"github.com/blacmusic/backend/internal/testutil"
)

func TestMakeASongOfAnAlbumASingle(t *testing.T) {
	w := newWorld(t)
	art := w.catalogArtist("Split Artist")
	pool := w.h.pool
	album := testutil.Release(t, pool, art, "Whole", "album", "public")
	var songs []string
	for i := 1; i <= 3; i++ {
		id, _ := testutil.ReadyTrack(t, pool, w.admin, art, album, "S", 100, "public")
		w.exec(`UPDATE tracks SET track_number = $2 WHERE id = $1`, id, i)
		songs = append(songs, id)
	}
	cover := w.picture(w.admin, "cover")
	w.exec(`UPDATE albums SET cover_image_id = $2, artwork_url = 'http://x/cover', published_at = now() - interval '5 days' WHERE id = $1`, album, cover)
	fan := testutil.User(t, pool, "user", true)
	w.exec(`INSERT INTO library_albums (user_id, album_id) VALUES ($1, $2)`, fan, album)
	for _, s := range songs[:2] {
		w.exec(`INSERT INTO plays (track_id, release_id, artist_id, listened_sec) VALUES ($1, $2, $3, 40)`, s, album, art)
	}
	w.exec(`INSERT INTO release_credits (release_id, name, role) VALUES ($1, 'Album', 'producer')`, album)
	w.exec(`INSERT INTO release_credits (release_id, track_id, name, role) VALUES ($1, $2, 'Song', 'writer')`, album, songs[1])
	t.Cleanup(func() { w.exec(`DELETE FROM plays WHERE artist_id = $1`, art) })

	split := func(track string, change map[string]any) (int, map[string]any) {
		body := map[string]any{"title": "Alone", "useAlbumCover": true, "visibility": "public"}
		for k, v := range change {
			body[k] = v
		}
		return call(w.r, "POST", "/admin/tracks/"+track+"/single", w.adminT, body)
	}
	if code, got := split(songs[1], map[string]any{"coverImageId": cover}); code != 422 {
		t.Fatalf("two covers = %d %v", code, got)
	}
	if code, got := split("trk_nothing", nil); code != 404 {
		t.Fatalf("unknown song = %d %v", code, got)
	}
	pending, _ := w.queueSong(art, album, `status = 'queued'`)
	if code, got := split(songs[1], nil); code != 409 || errCode(got) != "uploads_pending" {
		t.Fatalf("pending upload = %d %v", code, got)
	}
	w.exec(`DELETE FROM uploads WHERE id = $1`, pending)
	files := len(w.blobs.Keys())
	is := func(what, q string, args ...any) {
		t.Helper()
		if w.text(q, args...) != "true" {
			t.Fatalf("not true: %s", what)
		}
	}

	code, got := split(songs[1], nil)
	if code != 201 || got["type"] != "single" || got["title"] != "Alone" || got["visibility"] != "public" || got["coverUrl"] != "http://x/cover" || got["trackCount"] != float64(1) {
		t.Fatalf("split = %d %v", code, got)
	}
	single := got["id"].(string)
	if row := got["tracks"].([]any)[0].(map[string]any); row["id"] != songs[1] || row["trackNumber"] != float64(1) {
		t.Fatalf("track = %v", row)
	}
	is("the rest renumbered", `SELECT (string_agg(id || ':' || track_number, ',' ORDER BY track_number) = $2 || ':1,' || $3 || ':2')::text FROM tracks WHERE album_id = $1`, album, songs[0], songs[2])
	is("only its plays follow", `SELECT ((SELECT count(*) FROM plays WHERE release_id = $1) = 1 AND (SELECT count(*) FROM plays WHERE release_id = $2) = 1)::text`, single, album)
	is("credits", `SELECT (string_agg(name || ':' || (track_id IS NULL), ',' ORDER BY name) = 'Album:true,Song:false')::text FROM release_credits WHERE release_id = $1`, single)
	is("saved album stays saved, old date kept", `SELECT ((SELECT count(*) FROM library_albums WHERE album_id = $1) = 1 AND (SELECT published_at < now() - interval '4 days' FROM albums WHERE id = $2))::text`, album, single)
	if w.audit("release.split", single) != 1 {
		t.Fatal("not recorded")
	}
	if code, got := split(songs[1], nil); code != 409 || errCode(got) != "already_single" {
		t.Fatalf("again = %d %v", code, got)
	}

	// the last song takes the album with it
	if code, got = split(songs[0], map[string]any{"visibility": "draft", "useAlbumCover": false}); code != 201 || got["visibility"] != "draft" || got["coverUrl"] != nil {
		t.Fatalf("second = %d %v", code, got)
	}
	code, got = split(songs[2], nil)
	if code != 201 {
		t.Fatalf("last = %d %v", code, got)
	}
	last := got["id"].(string)
	is("album gone", `SELECT (count(*) = 0)::text FROM albums WHERE id = $1`, album)
	is("saved entry moved", `SELECT (count(*) = 1)::text FROM library_albums WHERE album_id = $1`, last)
	if len(w.blobs.Keys()) != files {
		t.Fatal("splitting must not delete any file")
	}
}
