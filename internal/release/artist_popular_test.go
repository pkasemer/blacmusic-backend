package release

import (
	"context"
	"slices"
	"testing"

	"github.com/blacmusic/backend/internal/testutil"
)

func TestAnArtistsReleasesRankedByThePopularityOfTheWholeRelease(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid, artistID := e.artistUser()
	mk := func(typ, daysAgo string, tracks int) (string, []string) {
		rel := testutil.Release(t, e.pool, artistID, testutil.Unique("R"), typ, "public")
		if _, err := e.pool.Exec(ctx, `UPDATE albums SET published_at = now() - $2::interval, release_date = (now() - $2::interval)::date WHERE id = $1`, rel, daysAgo+" days"); err != nil {
			t.Fatal(err)
		}
		var ids []string
		for range tracks {
			id, _ := testutil.ReadyTrack(t, e.pool, uid, artistID, rel, "t", 1, "public")
			ids = append(ids, id)
		}
		return rel, ids
	}
	play := func(rel, track string, n int) {
		for range n {
			if _, err := e.pool.Exec(ctx, `INSERT INTO plays (track_id, release_id, artist_id, listened_sec) VALUES ($1, $2, $3, 40)`, track, rel, artistID); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Cleanup(func() { e.pool.Exec(context.Background(), `DELETE FROM plays WHERE artist_id = $1`, artistID) }) //nolint:errcheck

	hit, hitTracks := mk("single", "30", 1)    // one song, played 3 times
	album, albumTracks := mk("album", "20", 3) // each song played less, 4 plays and a save of the album together
	quietOld, _ := mk("single", "10", 1)
	quietNew, _ := mk("single", "1", 1)
	play(hit, hitTracks[0], 3)
	play(album, albumTracks[0], 2)
	play(album, albumTracks[1], 1)
	play(album, albumTracks[2], 1)
	if _, err := e.pool.Exec(ctx, `INSERT INTO library_albums (user_id, album_id) VALUES ($1, $2)`, uid, album); err != nil {
		t.Fatal(err)
	}

	for sort, want := range map[string][]string{"popular": {album, hit, quietNew, quietOld}, "new": {quietNew, quietOld, album, hit}} {
		list, err := e.store.ListByArtist(ctx, artistID, uid, sort, 50, 0)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, r := range list {
			got = append(got, r.ID)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("%s = %v, want %v", sort, got, want)
		}
	}
	if _, err := e.store.ListByArtist(ctx, artistID, uid, "loud", 50, 0); err == nil {
		t.Fatal("an unknown sort must be refused")
	}
}
