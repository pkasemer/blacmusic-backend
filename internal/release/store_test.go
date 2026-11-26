package release

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/artist"
	"github.com/blacmusic/backend/internal/blobtest"
	"github.com/blacmusic/backend/internal/genre"
	"github.com/blacmusic/backend/internal/playlist"
	"github.com/blacmusic/backend/internal/testutil"
)

// Integration tests: need a Postgres (TEST_DATABASE_URL).
type env struct {
	t     *testing.T
	pool  *pgxpool.Pool
	blobs *blobtest.Mem
	store *Store
}

func setup(t *testing.T) *env {
	t.Helper()
	pool := testutil.Pool(t)
	blobs := blobtest.New()
	return &env{t: t, pool: pool, blobs: blobs, store: NewStore(pool, blobs, "http://api.test")}
}

// artistUser returns a user with an artist profile (1 GiB free).
func (e *env) artistUser() (userID, artistID string) {
	e.t.Helper()
	userID = testutil.User(e.t, e.pool, "user", true)
	return userID, testutil.Artist(e.t, e.pool, userID, testutil.Unique("Band"), testutil.GiB)
}

func (e *env) draftRelease(userID, artistID, typ string, tracks int) (string, []string) {
	e.t.Helper()
	rel := testutil.Release(e.t, e.pool, artistID, testutil.Unique("Rel"), typ, "draft")
	var ids []string
	for i := 0; i < tracks; i++ {
		id, _ := testutil.ReadyTrack(e.t, e.pool, userID, artistID, rel, testutil.Unique("Track"), 1000, "draft")
		ids = append(ids, id)
	}
	return rel, ids
}

func TestCreateValidatesAndNeedsAnArtist(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	plain := testutil.User(t, e.pool, "user", true)
	if _, err := e.store.Create(ctx, plain, Input{Type: "album", Title: "X"}); !errors.Is(err, ErrNoArtist) {
		t.Fatalf("no artist = %v, want ErrNoArtist", err)
	}

	uid, _ := e.artistUser()
	r, err := e.store.Create(ctx, uid, Input{Type: "album", Title: "  Debut  ", Genre: "Afrobeats", ReleaseDate: "2024-03-31"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(r.ID, "alb_") || r.Title != "Debut" || r.Visibility != "draft" || r.ReleaseDate != "2024-03-31" ||
		r.Genre != "Afrobeats" || !r.Mine || r.Artist == nil || r.Type != "album" {
		t.Fatalf("created: %+v", r)
	}

	var ve *ValidationError
	for name, in := range map[string]Input{
		"bad type":    {Type: "podcast", Title: "X"},
		"empty title": {Type: "single", Title: " "},
		"long title":  {Type: "single", Title: strings.Repeat("t", 201)},
		"bad date":    {Type: "single", Title: "X", ReleaseDate: "31/03/2024"},
		"long genre":  {Type: "single", Title: "X", Genre: strings.Repeat("g", 41)},
		"bad cover":   {Type: "single", Title: "X", CoverImageID: "img_00000000000000000000000000000000"},
	} {
		if _, err := e.store.Create(ctx, uid, in); !errors.As(err, &ve) {
			t.Errorf("%s = %v, want ValidationError", name, err)
		}
	}
}

func TestDraftsArePrivateUntilPublished(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid, artistID := e.artistUser()
	stranger := testutil.User(t, e.pool, "user", true)
	rel, tracks := e.draftRelease(uid, artistID, "album", 2)

	if _, err := e.store.Get(ctx, rel, stranger); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stranger reading a draft = %v, want ErrNotFound", err)
	}
	if list, _ := e.store.ListByArtist(ctx, artistID, stranger, 50, 0); len(list) != 0 {
		t.Fatalf("a draft must not appear on the artist page: %v", list)
	}
	mine, err := e.store.Get(ctx, rel, uid)
	if err != nil || len(mine.Tracks) != 2 || mine.Tracks[0].Visibility != "draft" {
		t.Fatalf("owner view: %+v %v", mine, err)
	}

	pub, err := e.store.Publish(ctx, rel, uid, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pub.Visibility != "public" || pub.PublishedAt == nil || pub.ReleaseDate == "" {
		t.Fatalf("published: %+v", pub)
	}
	seen, err := e.store.Get(ctx, rel, stranger)
	if err != nil || len(seen.Tracks) != 2 || seen.Mine || seen.Tracks[0].Visibility != "" {
		t.Fatalf("stranger after publish: %+v %v", seen, err)
	}
	// The artist's picture is the owner's avatar.
	var avatar string
	if err := e.pool.QueryRow(ctx, `INSERT INTO images (owner_id, purpose, status, original_key, size_bytes)
		VALUES ($1,'avatar','ready','k',1) RETURNING id`, uid).Scan(&avatar); err != nil {
		t.Fatal(err)
	}
	e.pool.Exec(ctx, `UPDATE users SET avatar_image_id = $2 WHERE id = $1`, uid, avatar) //nolint:errcheck
	if withPic, _ := e.store.Get(ctx, rel, stranger); withPic.Artist == nil || withPic.Artist.ImageURL != "http://api.test/v1/media/"+avatar+"/medium.jpg" {
		t.Fatalf("release.artist.imageUrl = %+v, want the owner's avatar", withPic.Artist)
	}
	if list, _ := e.store.ListByArtist(ctx, artistID, stranger, 50, 0); len(list) != 1 || list[0].ID != rel || list[0].TrackCount != 2 {
		t.Fatalf("artist page: %v", list)
	}
	var vis string
	e.pool.QueryRow(ctx, `SELECT visibility FROM tracks WHERE id=$1`, tracks[0]).Scan(&vis) //nolint:errcheck
	if vis != "public" {
		t.Fatalf("track visibility = %s, want public", vis)
	}
	// Publishing again changes nothing.
	again, err := e.store.Publish(ctx, rel, uid, nil)
	if err != nil || again.PublishedAt == nil || !again.PublishedAt.Equal(*pub.PublishedAt) {
		t.Fatalf("re-publish: %+v %v", again, err)
	}
}

func TestPublishRules(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid, artistID := e.artistUser()
	var ve *ValidationError

	empty := testutil.Release(t, e.pool, artistID, "Empty", "album", "draft")
	if _, err := e.store.Publish(ctx, empty, uid, nil); !errors.As(err, &ve) {
		t.Fatalf("no tracks = %v, want ValidationError", err)
	}

	twoSingle, _ := e.draftRelease(uid, artistID, "single", 2)
	if _, err := e.store.Publish(ctx, twoSingle, uid, nil); !errors.As(err, &ve) {
		t.Fatalf("a single with two tracks = %v, want ValidationError", err)
	}

	rel, _ := e.draftRelease(uid, artistID, "album", 1)
	insert := func(status string) {
		if _, err := e.pool.Exec(ctx, `
			INSERT INTO uploads (user_id, artist_id, release_id, title, artist_name, filename, size_bytes, original_key, status)
			VALUES ($1,$2,$3,'t','x','f',1,'originals/'||gen_random_uuid()::text,$4)`, uid, artistID, rel, status); err != nil {
			t.Fatal(err)
		}
	}
	for _, st := range []string{"pending", "queued", "processing"} {
		insert(st)
		if _, err := e.store.Publish(ctx, rel, uid, nil); !errors.Is(err, ErrUploadsPending) {
			t.Fatalf("with a %s upload = %v, want ErrUploadsPending", st, err)
		}
		e.pool.Exec(ctx, `DELETE FROM uploads WHERE release_id=$1 AND status=$2`, rel, st) //nolint:errcheck
	}
	insert("failed") // a failed file does not block publishing; Studio shows it to the artist
	got, err := e.store.Publish(ctx, rel, uid, nil)
	if err != nil || got.Visibility != "public" {
		t.Fatalf("with only a failed upload = %+v %v", got, err)
	}
	if len(got.Uploads) != 1 || got.Uploads[0].Status != "failed" {
		t.Fatalf("the owner should still see the failed upload: %+v", got.Uploads)
	}

	other := testutil.User(t, e.pool, "user", true)
	if _, err := e.store.Publish(ctx, rel, other, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("someone else publishing = %v, want ErrNotFound", err)
	}
}

func TestSchedulingAndPublishDue(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid, artistID := e.artistUser()
	stranger := testutil.User(t, e.pool, "user", true)
	rel, tracks := e.draftRelease(uid, artistID, "album", 1)

	var ve *ValidationError
	tooFar := time.Now().AddDate(2, 0, 0)
	if _, err := e.store.Publish(ctx, rel, uid, &tooFar); !errors.As(err, &ve) {
		t.Fatalf("more than a year ahead = %v, want ValidationError", err)
	}

	when := time.Now().Add(2 * time.Hour)
	sched, err := e.store.Publish(ctx, rel, uid, &when)
	if err != nil {
		t.Fatal(err)
	}
	if sched.Visibility != "scheduled" || sched.PublishAt == nil {
		t.Fatalf("scheduled: %+v", sched)
	}
	if _, err := e.store.Get(ctx, rel, stranger); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a scheduled release is not visible yet: %v", err)
	}
	var vis string
	e.pool.QueryRow(ctx, `SELECT visibility FROM tracks WHERE id=$1`, tracks[0]).Scan(&vis) //nolint:errcheck
	if vis != "draft" {
		t.Fatalf("tracks of a scheduled release stay draft, got %s", vis)
	}

	// Not yet due: nothing happens.
	if _, err := e.store.PublishDue(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.store.Get(ctx, rel, uid); got.Visibility != "scheduled" {
		t.Fatalf("flipped early: %s", got.Visibility)
	}
	// Due: it goes live with its tracks.
	if _, err := e.pool.Exec(ctx, `UPDATE albums SET publish_at = now() - interval '1 second' WHERE id=$1`, rel); err != nil {
		t.Fatal(err)
	}
	if n, err := e.store.PublishDue(ctx); err != nil || n < 1 {
		t.Fatalf("PublishDue = %d, %v", n, err)
	}
	got, err := e.store.Get(ctx, rel, stranger)
	if err != nil || got.Visibility != "public" || len(got.Tracks) != 1 {
		t.Fatalf("after going live: %+v %v", got, err)
	}

	// A scheduled release can be published right now instead.
	rel2, _ := e.draftRelease(uid, artistID, "album", 1)
	e.store.Publish(ctx, rel2, uid, &when) //nolint:errcheck
	if now, err := e.store.Publish(ctx, rel2, uid, nil); err != nil || now.Visibility != "public" {
		t.Fatalf("publish now: %+v %v", now, err)
	}
}

func TestUnpublishHidesTheRelease(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid, artistID := e.artistUser()
	stranger := testutil.User(t, e.pool, "user", true)
	rel, _ := e.draftRelease(uid, artistID, "album", 1)
	e.store.Publish(ctx, rel, uid, nil) //nolint:errcheck

	got, err := e.store.Unpublish(ctx, rel, uid)
	if err != nil || got.Visibility != "draft" {
		t.Fatalf("unpublish: %+v %v", got, err)
	}
	if _, err := e.store.Get(ctx, rel, stranger); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unpublished release must disappear: %v", err)
	}
	if _, err := e.store.Unpublish(ctx, rel, stranger); !errors.Is(err, ErrNotFound) {
		t.Fatalf("someone else unpublishing = %v", err)
	}
}

func TestUpdateRelease(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid, artistID := e.artistUser()
	rel, _ := e.draftRelease(uid, artistID, "album", 1)

	title, genre := "Renamed", "Gospel"
	got, err := e.store.Update(ctx, rel, uid, Patch{Title: &title, Genre: &genre})
	if err != nil || got.Title != "Renamed" || got.Genre != "Gospel" {
		t.Fatalf("update: %+v %v", got, err)
	}

	// The cover must be one of the caller's finished cover images.
	var cover string
	if err := e.pool.QueryRow(ctx, `INSERT INTO images (owner_id, purpose, status, original_key, size_bytes)
		VALUES ($1,'cover','ready','k',1) RETURNING id`, uid).Scan(&cover); err != nil {
		t.Fatal(err)
	}
	if got, err = e.store.Update(ctx, rel, uid, Patch{CoverImageID: &cover}); err != nil || got.ArtworkURL != "http://api.test/v1/media/"+cover+"/medium.jpg" {
		t.Fatalf("cover: %+v %v", got, err)
	}
	var ve *ValidationError
	bad := "img_00000000000000000000000000000000"
	if _, err := e.store.Update(ctx, rel, uid, Patch{CoverImageID: &bad}); !errors.As(err, &ve) {
		t.Fatalf("someone else's cover = %v", err)
	}
	none := ""
	if got, _ = e.store.Update(ctx, rel, uid, Patch{CoverImageID: &none}); got.ArtworkURL != "" {
		t.Fatalf("clearing the cover: %q", got.ArtworkURL)
	}
	// An unrelated edit keeps an existing cover.
	e.store.Update(ctx, rel, uid, Patch{CoverImageID: &cover}) //nolint:errcheck
	if got, _ = e.store.Update(ctx, rel, uid, Patch{Genre: &genre}); got.ArtworkURL == "" {
		t.Fatal("editing other fields must not drop the cover")
	}

	// The type can change only while the release is a draft.
	single := "single"
	if got, err = e.store.Update(ctx, rel, uid, Patch{Type: &single}); err != nil || got.Type != "single" {
		t.Fatalf("draft type change: %+v %v", got, err)
	}
	e.store.Publish(ctx, rel, uid, nil) //nolint:errcheck
	album := "album"
	if _, err := e.store.Update(ctx, rel, uid, Patch{Type: &album}); !errors.As(err, &ve) {
		t.Fatalf("type change after publishing = %v, want ValidationError", err)
	}
	other := testutil.User(t, e.pool, "user", true)
	if _, err := e.store.Update(ctx, rel, other, Patch{Title: &title}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("someone else's edit = %v, want ErrNotFound", err)
	}
	badDate := "yesterday"
	if _, err := e.store.Update(ctx, rel, uid, Patch{ReleaseDate: &badDate}); !errors.As(err, &ve) {
		t.Fatalf("bad date = %v", err)
	}
}

func TestEditAndDeleteATrack(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid, artistID := e.artistUser()
	other := testutil.User(t, e.pool, "user", true)
	rel, tracks := e.draftRelease(uid, artistID, "album", 3)
	e.store.Publish(ctx, rel, uid, nil) //nolint:errcheck
	victim := tracks[1]

	title, explicit, num := "Better Title", true, 7
	got, err := e.store.UpdateTrack(ctx, victim, uid, TrackPatch{Title: &title, Explicit: &explicit, TrackNumber: &num})
	if err != nil || got.Title != "Better Title" || !got.Explicit || got.TrackNumber == nil || *got.TrackNumber != 7 {
		t.Fatalf("update track: %+v %v", got, err)
	}
	zero := 0
	if got, _ = e.store.UpdateTrack(ctx, victim, uid, TrackPatch{TrackNumber: &zero}); got.TrackNumber != nil {
		t.Fatalf("trackNumber 0 clears it: %v", got.TrackNumber)
	}
	var ve *ValidationError
	empty := " "
	if _, err := e.store.UpdateTrack(ctx, victim, uid, TrackPatch{Title: &empty}); !errors.As(err, &ve) {
		t.Fatalf("empty title = %v", err)
	}
	if _, err := e.store.UpdateTrack(ctx, victim, other, TrackPatch{Title: &title}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("someone else's edit = %v", err)
	}
	if err := e.store.DeleteTrack(ctx, victim, other); !errors.Is(err, ErrNotFound) {
		t.Fatalf("someone else's delete = %v", err)
	}

	// The track sits in a listener's playlist between two others and in their library.
	listener := testutil.User(t, e.pool, "user", true)
	ps := playlist.NewStore(e.pool, "http://test")
	pl, err := ps.Create(ctx, listener, "Mix", "", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{tracks[0], victim, tracks[2]} {
		if _, err := ps.AddItem(ctx, pl.ID, listener, id, nil); err != nil {
			t.Fatal(err)
		}
	}
	e.pool.Exec(ctx, `INSERT INTO library_tracks (user_id, track_id) VALUES ($1,$2)`, listener, victim) //nolint:errcheck
	for _, k := range []string{"low", "medium", "high"} {
		e.blobs.Put("tracks/"+victim+"/"+k+".m4a", []byte("audio"))
	}
	e.blobs.Put("originals/upl_for_"+victim, []byte("original"))
	e.pool.Exec(ctx, `UPDATE uploads SET original_key=$2 WHERE track_id=$1`, victim, "originals/upl_for_"+victim) //nolint:errcheck
	usedBefore := used(t, e.pool, artistID)

	if err := e.store.DeleteTrack(ctx, victim, uid); err != nil {
		t.Fatal(err)
	}
	if len(e.blobs.Keys()) != 0 {
		t.Fatalf("storage objects left behind: %v", e.blobs.Keys())
	}
	if after := used(t, e.pool, artistID); after != usedBefore-1000 {
		t.Fatalf("used = %d, want %d (the track's original is no longer counted)", after, usedBefore-1000)
	}
	d, err := ps.Get(ctx, pl.ID, listener, 100, 0)
	if err != nil || len(d.Items) != 2 {
		t.Fatalf("playlist after delete: %v %v", d, err)
	}
	for i, it := range d.Items {
		if it.Position != i {
			t.Fatalf("playlist positions must stay contiguous, item %d has %d", i, it.Position)
		}
	}
	var n int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM library_tracks WHERE track_id=$1`, victim).Scan(&n) //nolint:errcheck
	if n != 0 {
		t.Fatal("the library entry must go with the track")
	}
	if err := e.store.DeleteTrack(ctx, victim, uid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete = %v, want ErrNotFound", err)
	}

	// Removing the last tracks takes a live release offline instead of leaving it empty.
	e.store.DeleteTrack(ctx, tracks[0], uid) //nolint:errcheck
	e.store.DeleteTrack(ctx, tracks[2], uid) //nolint:errcheck
	if r, _ := e.store.Get(ctx, rel, uid); r.Visibility != "draft" || r.TrackCount != 0 {
		t.Fatalf("emptied release: %+v", r)
	}
}

func used(t *testing.T, pool *pgxpool.Pool, artistID string) int64 {
	t.Helper()
	q, err := artist.QuotaFor(context.Background(), pool, artistID)
	if err != nil {
		t.Fatal(err)
	}
	return q.UsedStorageBytes
}

func TestDeleteReleaseRemovesEverything(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid, artistID := e.artistUser()
	rel, tracks := e.draftRelease(uid, artistID, "album", 2)
	for _, id := range tracks {
		e.blobs.Put("tracks/"+id+"/medium.m4a", []byte("audio"))
	}
	// A file still waiting for the worker is cleaned up too.
	var pendingKey string
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO uploads (user_id, artist_id, release_id, title, artist_name, filename, size_bytes, original_key, status)
		VALUES ($1,$2,$3,'t','x','f',500,'originals/pending-'||gen_random_uuid()::text,'queued') RETURNING original_key`,
		uid, artistID, rel).Scan(&pendingKey); err != nil {
		t.Fatal(err)
	}
	e.blobs.Put(pendingKey, []byte("queued file"))

	other := testutil.User(t, e.pool, "user", true)
	if err := e.store.Delete(ctx, rel, other); !errors.Is(err, ErrNotFound) {
		t.Fatalf("someone else's delete = %v", err)
	}
	if err := e.store.Delete(ctx, rel, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Get(ctx, rel, uid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("release still there: %v", err)
	}
	if used(t, e.pool, artistID) != 0 {
		t.Fatal("all storage must be freed")
	}
	if len(e.blobs.Keys()) != 0 {
		t.Fatalf("objects left: %v", e.blobs.Keys())
	}
	var n int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM tracks WHERE id = ANY($1)`, tracks).Scan(&n) //nolint:errcheck
	if n != 0 {
		t.Fatal("tracks must be deleted with their release")
	}
}

func TestDeleteReleaseRefusedWhileProcessing(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid, artistID := e.artistUser()
	rel, _ := e.draftRelease(uid, artistID, "album", 1)
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO uploads (user_id, artist_id, release_id, title, artist_name, filename, size_bytes, original_key, status)
		VALUES ($1,$2,$3,'t','x','f',1,'originals/busy-'||gen_random_uuid()::text,'processing')`, uid, artistID, rel); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Delete(ctx, rel, uid); !errors.Is(err, ErrProcessing) {
		t.Fatalf("delete during processing = %v, want ErrProcessing", err)
	}
	if _, err := e.store.Get(ctx, rel, uid); err != nil {
		t.Fatal("the release must still exist")
	}
}

func TestMineAndOwnerView(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid, artistID := e.artistUser()
	rel, _ := e.draftRelease(uid, artistID, "album", 1)
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO uploads (user_id, artist_id, release_id, title, artist_name, filename, size_bytes, original_key, status, error)
		VALUES ($1,$2,$3,'Broken','x','f',1,'originals/m-'||gen_random_uuid()::text,'failed','could not read this file as audio')`, uid, artistID, rel); err != nil {
		t.Fatal(err)
	}
	list, err := e.store.ListMine(ctx, uid, 50, 0)
	if err != nil || len(list) != 1 || list[0].ID != rel || list[0].Visibility != "draft" {
		t.Fatalf("ListMine: %v %v", list, err)
	}
	got, _ := e.store.Get(ctx, rel, uid)
	if len(got.Uploads) != 1 || got.Uploads[0].Error == "" {
		t.Fatalf("owner should see the failed upload and why: %+v", got.Uploads)
	}
	other := testutil.User(t, e.pool, "user", true)
	if list, _ = e.store.ListMine(ctx, other, 50, 0); len(list) != 0 {
		t.Fatalf("someone else's releases leaked: %v", list)
	}
}

func TestGenresMustComeFromTheList(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid, artistID := e.artistUser()

	// Names are matched without regard to case (or by id) and stored in their canonical form.
	r, err := e.store.Create(ctx, uid, Input{Type: "single", Title: "G", Genre: "  hip-hop/RAP "})
	if err != nil || r.Genre != "Hip-Hop/Rap" {
		t.Fatalf("create with a loose spelling: %+v %v", r, err)
	}
	for _, bad := range []string{"Afrobeatz", "Rock and Roll", "<b>x</b>"} {
		if _, err := e.store.Create(ctx, uid, Input{Type: "single", Title: "G", Genre: bad}); !errors.Is(err, genre.ErrInvalid) {
			t.Errorf("create with %q = %v, want ErrInvalid", bad, err)
		}
	}
	if r, err = e.store.Create(ctx, uid, Input{Type: "single", Title: "No genre"}); err != nil || r.Genre != "" {
		t.Fatalf("a release without a genre is fine: %+v %v", r, err)
	}

	// A genre saved before the list existed is returned unchanged and survives edits that do not touch it.
	rel := testutil.Release(t, e.pool, artistID, "Legacy", "album", "draft")
	if _, err := e.pool.Exec(ctx, `UPDATE albums SET genre = 'Kadongo Kamu (old)' WHERE id=$1`, rel); err != nil {
		t.Fatal(err)
	}
	title, same := "Renamed Legacy", "Kadongo Kamu (old)"
	got, err := e.store.Update(ctx, rel, uid, Patch{Title: &title})
	if err != nil || got.Genre != "Kadongo Kamu (old)" {
		t.Fatalf("an unrelated edit must keep the old genre: %+v %v", got, err)
	}
	if got, err = e.store.Update(ctx, rel, uid, Patch{Genre: &same}); err != nil || got.Genre != "Kadongo Kamu (old)" {
		t.Fatalf("re-sending the unchanged old genre must not fail: %+v %v", got, err)
	}
	bad := "Kadongo Kamu (new spelling)"
	if _, err := e.store.Update(ctx, rel, uid, Patch{Genre: &bad}); !errors.Is(err, genre.ErrInvalid) {
		t.Fatalf("changing it to another free-text value = %v, want ErrInvalid", err)
	}
	pick := "gospel"
	if got, err = e.store.Update(ctx, rel, uid, Patch{Genre: &pick}); err != nil || got.Genre != "Gospel" {
		t.Fatalf("switching to a list value: %+v %v", got, err)
	}
	clear := ""
	if got, err = e.store.Update(ctx, rel, uid, Patch{Genre: &clear}); err != nil || got.Genre != "" {
		t.Fatalf("clearing the genre: %+v %v", got, err)
	}

	// Same rules for tracks.
	_, tracks := e.draftRelease(uid, artistID, "album", 1)
	if _, err := e.pool.Exec(ctx, `UPDATE tracks SET genre = 'old free text' WHERE id=$1`, tracks[0]); err != nil {
		t.Fatal(err)
	}
	t1 := "New Title"
	if tv, err := e.store.UpdateTrack(ctx, tracks[0], uid, TrackPatch{Title: &t1}); err != nil || tv.Genre != "old free text" {
		t.Fatalf("track edit keeps its old genre: %+v %v", tv, err)
	}
	nope := "not a genre"
	if _, err := e.store.UpdateTrack(ctx, tracks[0], uid, TrackPatch{Genre: &nope}); !errors.Is(err, genre.ErrInvalid) {
		t.Fatalf("track genre not in the list = %v, want ErrInvalid", err)
	}
	ok := "AMAPIANO"
	if tv, err := e.store.UpdateTrack(ctx, tracks[0], uid, TrackPatch{Genre: &ok}); err != nil || tv.Genre != "Amapiano" {
		t.Fatalf("track genre from the list: %+v %v", tv, err)
	}
}

// Releases and the tracks inside them say whether the artist is verified: always present.
func TestReleasesCarryTheVerifiedFlag(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	user := testutil.User(t, e.pool, "user", true)
	artist := testutil.Artist(t, e.pool, user, testutil.Unique("Flag"), testutil.GiB)
	rel := testutil.Release(t, e.pool, artist, "Flagged", "single", "public")
	track, _ := testutil.ReadyTrack(t, e.pool, user, artist, rel, "Flagged song", 10, "public")

	view := func() (artistFlag, trackFlag any) {
		t.Helper()
		r, err := e.store.Get(ctx, rel, "")
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(r)
		var m struct {
			Artist map[string]any   `json:"artist"`
			Tracks []map[string]any `json:"tracks"`
		}
		if err := json.Unmarshal(raw, &m); err != nil || len(m.Tracks) != 1 {
			t.Fatalf("release json = %s", raw)
		}
		return m.Artist["verified"], m.Tracks[0]["artistVerified"]
	}
	if a, tr := view(); a != false || tr != false {
		t.Fatalf("unverified: artist.verified %v, track artistVerified %v, want false/false (always sent)", a, tr)
	}
	e.pool.Exec(ctx, `UPDATE artists SET verification = 'verified' WHERE id = $1`, artist) //nolint:errcheck
	if a, tr := view(); a != true || tr != true {
		t.Fatalf("verified: artist.verified %v, track artistVerified %v", a, tr)
	}
	// editing a track returns the flag too
	tv, err := e.store.UpdateTrack(ctx, track, user, TrackPatch{Title: ptrStr("Renamed")})
	if err != nil || !tv.ArtistVerified {
		t.Fatalf("UpdateTrack = %+v %v", tv, err)
	}
}

func ptrStr(s string) *string { return &s }

func TestSearchReleasesAndKinds(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	tok := "zr" + testutil.Unique("s")
	user := testutil.User(t, e.pool, "user", true)
	artist := testutil.Artist(t, e.pool, user, "Band "+tok, testutil.GiB)
	other := testutil.Artist(t, e.pool, testutil.User(t, e.pool, "user", true), testutil.Unique("Else"), testutil.GiB)
	mk := func(owner, artistID, title, typ, visibility string, tracks int) string {
		rel := testutil.Release(e.t, e.pool, artistID, title, typ, visibility)
		for i := 0; i < tracks; i++ {
			testutil.ReadyTrack(e.t, e.pool, owner, artistID, rel, "T", 1, visibility)
		}
		return rel
	}
	exact := mk(user, other, tok, "single", "public", 1)            // title equals q
	prefix := mk(user, other, tok+" Vol 1", "album", "public", 3)   // an EP
	word := mk(user, other, "Best of "+tok, "album", "public", 7)   // a full album
	byArtist := mk(user, artist, "Unrelated", "album", "public", 1) // matches only through the artist's name
	mk(user, other, tok+" draft", "album", "draft", 2)              // not public

	ids := func(typ string) []string {
		t.Helper()
		list, err := e.store.Search(ctx, tok, typ, 50, 0)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range list {
			out = append(out, r.ID)
		}
		return out
	}
	eq := func(name string, got, want []string) {
		t.Helper()
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s = %v, want %v", name, got, want)
		}
	}
	eq("all", ids(""), []string{exact, prefix, word, byArtist})
	eq("type=all", ids("all"), []string{exact, prefix, word, byArtist})
	eq("single", ids("single"), []string{exact})
	eq("ep", ids("ep"), []string{prefix, byArtist})
	eq("album", ids("album"), []string{word})
	if _, err := e.store.Search(ctx, tok, "lp", 10, 0); err == nil {
		t.Fatal("an unknown type was accepted")
	}

	// every release says what to tag it with
	list, _ := e.store.Search(ctx, tok, "all", 50, 0)
	kinds := map[string]string{}
	for _, r := range list {
		kinds[r.ID] = r.Kind
		if r.Artist == nil {
			t.Fatalf("release without an artist: %+v", r)
		}
	}
	if kinds[exact] != "single" || kinds[prefix] != "ep" || kinds[word] != "album" || kinds[byArtist] != "ep" {
		t.Fatalf("kinds = %v", kinds)
	}
	// and so does a single release lookup
	if r, err := e.store.Get(ctx, word, ""); err != nil || r.Kind != "album" {
		t.Fatalf("Get = %+v %v", r, err)
	}
	if got := KindOf("album", 6); got != "ep" {
		t.Fatalf("6 tracks = %s", got)
	}
	if got := KindOf("album", 7); got != "album" {
		t.Fatalf("7 tracks = %s", got)
	}
}

func TestReleasesCarryLargeCoverAndTotalDuration(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	user := testutil.User(t, e.pool, "user", true)
	artist := testutil.Artist(t, e.pool, user, testutil.Unique("Big"), testutil.GiB)
	rel := testutil.Release(t, e.pool, artist, "Big Cover", "album", "public")
	plain := testutil.Release(t, e.pool, artist, "External Cover", "single", "public")
	for i := 0; i < 2; i++ { // ReadyTrack makes 180 s tracks
		testutil.ReadyTrack(t, e.pool, user, artist, rel, "T", 1, "public")
	}
	testutil.ReadyTrack(t, e.pool, user, artist, plain, "T", 1, "public")
	e.pool.Exec(ctx, `UPDATE albums SET artwork_url = 'http://api.test/v1/media/img_0123456789abcdef0123456789abcdef/medium.jpg' WHERE id = $1`, rel) //nolint:errcheck
	e.pool.Exec(ctx, `UPDATE albums SET artwork_url = 'https://cdn.example.com/old.jpg' WHERE id = $1`, plain)                                        //nolint:errcheck

	get, err := e.store.Get(ctx, rel, "")
	if err != nil || get.TotalDurationSec != 360 || get.ArtworkLargeURL != "http://api.test/v1/media/img_0123456789abcdef0123456789abcdef/large.jpg" {
		t.Fatalf("Get = %+v %v", get, err)
	}
	found, err := e.store.Search(ctx, "Big Cover", "all", 5, 0)
	if err != nil || len(found) != 1 || found[0].TotalDurationSec != 360 || found[0].ArtworkLargeURL == "" {
		t.Fatalf("Search = %+v %v (lists carry the totals too)", found, err)
	}
	other, err := e.store.Get(ctx, plain, "")
	if err != nil || other.ArtworkLargeURL != "" || other.TotalDurationSec != 180 {
		t.Fatalf("a cover that is not ours has no large version: %+v %v", other, err)
	}
}

func TestRelatedReleases(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	user := testutil.User(t, e.pool, "user", true)
	artist := testutil.Artist(t, e.pool, user, testutil.Unique("Rel"), testutil.GiB)
	other := testutil.Artist(t, e.pool, testutil.User(t, e.pool, "user", true), testutil.Unique("Other"), testutil.GiB)
	g := testutil.Unique("genre")
	mk := func(owner, artistID, genre, visibility string) string {
		rel := testutil.Release(e.t, e.pool, artistID, "R "+genre, "single", visibility)
		e.pool.Exec(ctx, `UPDATE albums SET genre = $2 WHERE id = $1`, rel, genre) //nolint:errcheck
		testutil.ReadyTrack(e.t, e.pool, owner, artistID, rel, "T", 1, visibility)
		return rel
	}
	base := mk(user, artist, g, "public")
	sameGenre := mk(user, other, g, "public")
	sameArtist := mk(user, artist, "unrelated-"+g, "public")
	mk(user, other, g, "draft")               // not public
	mk(user, other, "different-"+g, "public") // nothing in common

	list, err := e.store.Related(ctx, base, "", 12)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range list {
		ids = append(ids, r.ID)
		if r.Kind == "" || r.Artist == nil {
			t.Fatalf("release shape = %+v", r)
		}
	}
	if len(ids) != 2 || ids[0] != sameGenre || ids[1] != sameArtist {
		t.Fatalf("related = %v, want the same genre %s, then the same artist %s (never %s itself)", ids, sameGenre, sameArtist, base)
	}
	if list, _ = e.store.Related(ctx, base, "", 1); len(list) != 1 || list[0].ID != sameGenre {
		t.Fatalf("limit = %+v", list)
	}
	// a draft release cannot be asked about by a stranger
	draft := mk(user, artist, g, "draft")
	if _, err := e.store.Related(ctx, draft, "", 12); err != ErrNotFound {
		t.Fatalf("draft by a stranger = %v, want ErrNotFound", err)
	}
	if _, err := e.store.Related(ctx, draft, user, 12); err != nil {
		t.Fatalf("draft by its owner = %v", err)
	}
}
