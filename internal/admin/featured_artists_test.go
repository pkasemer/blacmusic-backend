package admin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/blacmusic/backend/internal/credits"
	"github.com/blacmusic/backend/internal/testutil"
	"github.com/blacmusic/backend/internal/trackview"
)

func TestFeaturedArtists(t *testing.T) {
	w := newWorld(t)
	pool, ctx := w.h.pool, context.Background()
	owner := testutil.User(t, pool, "user", true)
	main := testutil.Artist(t, pool, owner, testutil.Unique("Main"), testutil.GiB)
	guest, other, banned := w.catalogArtist("Guest"), w.catalogArtist("Other"), w.catalogArtist("Banned")
	nameOf := func(id string) string { return w.text(`SELECT name FROM artists WHERE id = $1`, id) }
	w.exec(`UPDATE artists SET verification = 'verified' WHERE id = $1`, guest)
	rel := testutil.Release(t, pool, main, "Featuring", "album", "public")
	first, _ := testutil.ReadyTrack(t, pool, owner, main, rel, "One", 1, "public")
	second, _ := testutil.ReadyTrack(t, pool, owner, main, rel, "Two", 1, "public")

	// what a listener's track shows
	shown := func(track string) string {
		t.Helper()
		got, err := trackview.Scan(pool.QueryRow(ctx, trackview.Select("")+` WHERE t.id = $1`, track), "http://api.test")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(got.FeaturedArtists)
		release, err := w.rel.Get(ctx, rel, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range release.Tracks {
			if x.ID == track {
				if again, _ := json.Marshal(x.FeaturedArtists); string(again) != string(b) {
					t.Fatalf("release track %s != %s", again, b)
				}
			}
		}
		return string(b)
	}
	if shown(first) != "null" {
		t.Fatal("none to start with")
	}
	// a release: an album shows only what is on the whole release, a single also its song's
	onRelease := func(id string) string {
		t.Helper()
		got, err := w.rel.Get(ctx, id, "")
		if err != nil {
			t.Fatal(err)
		}
		list, err := w.rel.ListByArtist(ctx, main, "", "", 50, 0)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(got.FeaturedArtists)
		for _, x := range list {
			if x.ID == id {
				if again, _ := json.Marshal(x.FeaturedArtists); string(again) != string(b) {
					t.Fatalf("listed %s != %s", again, b)
				}
			}
		}
		return string(b)
	}
	single := testutil.Release(t, pool, main, "Alone", "single", "public")
	lone, _ := testutil.ReadyTrack(t, pool, owner, main, single, "Alone", 1, "public")
	if code, b := call(w.r, "POST", "/admin/tracks/"+lone+"/featured-artists", w.adminT, map[string]any{"artistId": guest}); code != 200 {
		t.Fatalf("feature on a single = %d %v", code, b)
	}
	if got := onRelease(single); !strings.Contains(got, guest) {
		t.Fatalf("single = %s", got)
	}
	if code, b := call(w.r, "POST", "/admin/tracks/"+second+"/featured-artists", w.adminT, map[string]any{"artistId": other}); code != 200 {
		t.Fatalf("feature on one album song = %d %v", code, b)
	}
	if got := onRelease(rel); got != "null" {
		t.Fatalf("an album does not show one song's feature: %s", got)
	}
	w.exec(`DELETE FROM release_credits WHERE release_id = $1`, rel)

	// what the artist writes shows as a name only, whoever they point at; never themselves, never a banned artist
	own := credits.NewStore(pool)
	written := []credits.Input{
		{Name: "Borrowed Name", Role: "featured_artist", ArtistID: other, TrackID: first},
		{Name: strings.ToUpper(nameOf(main)), Role: "featured_artist"},
		{Name: "Gone", Role: "featured_artist", ArtistID: banned},
		{Name: "A Producer", Role: "producer"},
	}
	w.exec(`UPDATE artists SET banned_at = now() WHERE id = $1`, banned)
	if _, err := own.Replace(ctx, rel, owner, written); err != nil {
		t.Fatal(err)
	}
	if got := shown(first); got != `[{"name":"Borrowed Name","verified":false}]` {
		t.Fatalf("artist's own = %s", got)
	}

	attach := func(path, artist string) (int, map[string]any) {
		return call(w.r, "POST", "/admin/"+path+"/featured-artists", w.adminT, map[string]any{"artistId": artist})
	}
	for what, got := range map[string]func() (int, map[string]any){
		"unknown": func() (int, map[string]any) { return attach("releases/"+rel, "art_nothing") },
		"itself":  func() (int, map[string]any) { return attach("releases/"+rel, main) },
	} {
		if code, b := got(); code != 422 {
			t.Fatalf("%s = %d %v", what, code, b)
		}
	}
	if code, b := attach("releases/"+rel, banned); code != 409 || errCode(b) != "artist_banned" {
		t.Fatalf("banned = %d %v", code, b)
	}
	if code, _ := attach("tracks/trk_nothing", guest); code != 404 {
		t.Fatalf("unknown track = %d", code)
	}
	var listing map[string]any
	for range 2 { // twice changes nothing
		code, b := attach("releases/"+rel, guest)
		if code != 200 || len(b["release"].([]any)) != 3 {
			t.Fatalf("attach = %d %v", code, b)
		}
		listing = b
	}
	linked := `{"artistId":"` + guest + `","name":"` + nameOf(guest) + `","verified":true}`
	if got := shown(second); got != "["+linked+"]" {
		t.Fatalf("on every song = %s", got)
	}
	// attaching what the artist had written makes it link
	if code, b := attach("tracks/"+first, other); code != 200 {
		t.Fatalf("approve = %d %v", code, b)
	}
	approved := `{"artistId":"` + other + `","name":"` + nameOf(other) + `","verified":false}`
	if got := shown(first); got != "["+approved+","+linked+"]" {
		t.Fatalf("in the order saved = %s", got)
	}
	if w.text(`SELECT count(*)::text FROM release_credits WHERE release_id = $1 AND role = 'producer'`, rel) != "1" {
		t.Fatal("other credits stay")
	}
	// the artist saving the list again keeps the team's links
	current, err := own.ForRelease(ctx, rel, owner)
	if err != nil {
		t.Fatal(err)
	}
	var again []credits.Input
	for _, c := range current {
		again = append(again, credits.Input{Name: c.Name, Role: c.Role, ArtistID: c.ArtistID, TrackID: c.TrackID})
	}
	if _, err := own.Replace(ctx, rel, owner, again); err != nil {
		t.Fatal(err)
	}
	if got := shown(first); !strings.Contains(got, linked) || !strings.Contains(got, approved) {
		t.Fatalf("after the artist saved = %s", got)
	}

	_, listing = call(w.r, "GET", "/admin/releases/"+rel+"/featured-artists", w.adminT, nil)
	var credit string
	for _, x := range listing["release"].([]any) {
		if m := x.(map[string]any); m["artistId"] == guest {
			credit = m["creditId"].(string)
		}
	}
	if code, b := call(w.r, "DELETE", "/admin/featured-artists/"+credit, w.adminT, nil); code != 200 || shown(second) != "null" {
		t.Fatalf("remove = %d %v %s", code, b, shown(second))
	}
	if code, _ := call(w.r, "DELETE", "/admin/featured-artists/"+credit, w.adminT, nil); code != 404 {
		t.Fatalf("remove again = %d", code)
	}
	if w.audit("release.feature_artist", rel) != 3 || w.audit("release.unfeature_artist", rel) != 1 {
		t.Fatal("changes must be recorded")
	}
}
