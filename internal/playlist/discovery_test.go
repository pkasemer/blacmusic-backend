package playlist

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/testutil"
)

type discovery struct {
	store              *Store
	pool               *pgxpool.Pool
	owner, admin, fan  string
	artistID           string
	pub, fresh         string // two public playlists with public tracks
	draftOnly, private string
	t1, t2             string
}

func readyCover(t *testing.T, pool *pgxpool.Pool, owner string) string {
	t.Helper()
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	id := "img_" + hex.EncodeToString(raw)
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO images (id, owner_id, purpose, original_key, size_bytes, status) VALUES ($1,$2,'cover','k',1,'ready')`,
		id, owner); err != nil {
		t.Fatal(err)
	}
	return id
}

func setupDiscovery(t *testing.T) *discovery {
	t.Helper()
	pool := testutil.Pool(t)
	ctx := context.Background()
	d := &discovery{store: NewStore(pool, "http://test"), pool: pool}
	d.owner = testutil.User(t, pool, "user", true)
	d.admin = testutil.User(t, pool, "admin", true)
	d.fan = testutil.User(t, pool, "user", true)
	d.artistID = testutil.Artist(t, pool, d.owner, testutil.Unique("artist"), 1<<30)
	rel := testutil.Release(t, pool, d.artistID, testutil.Unique("rel"), "album", "public")
	if _, err := pool.Exec(ctx, `UPDATE albums SET published_at = now(), genre = 'Gospel' WHERE id = $1`, rel); err != nil {
		t.Fatal(err)
	}
	d.t1, _ = testutil.ReadyTrack(t, pool, d.owner, d.artistID, rel, "one", 1, "public")
	d.t2, _ = testutil.ReadyTrack(t, pool, d.owner, d.artistID, rel, "two", 1, "public")
	draft, _ := testutil.ReadyTrack(t, pool, d.owner, d.artistID, rel, "wip", 1, "draft")
	for id, url := range map[string]string{d.t1: "http://x/1.jpg", d.t2: "http://x/2.jpg"} {
		if _, err := pool.Exec(ctx, `UPDATE tracks SET artwork_url = $2 WHERE id = $1`, id, url); err != nil {
			t.Fatal(err)
		}
	}

	mk := func(public bool, tracks ...string) string {
		p, err := d.store.Create(ctx, d.owner, testutil.Unique("pl"), "", public)
		if err != nil {
			t.Fatal(err)
		}
		for _, tr := range tracks {
			if _, err := d.store.AddItem(ctx, p.ID, d.owner, tr, nil); err != nil {
				t.Fatal(err)
			}
		}
		return p.ID
	}
	d.pub = mk(true, d.t1, d.t2)
	d.fresh = mk(true, d.t1)
	d.draftOnly = mk(true, draft)
	d.private = mk(false, d.t1)
	mk(true) // empty
	return d
}

func (d *discovery) mine(ps []Playlist) []string {
	own := []string{d.pub, d.fresh, d.draftOnly, d.private}
	out := []string{}
	for _, p := range ps {
		if slices.Contains(own, p.ID) {
			out = append(out, p.ID)
		}
	}
	return out
}

func TestBrowsePlaylists(t *testing.T) {
	d := setupDiscovery(t)
	ctx := context.Background()

	// new: newest first; private, empty and draft-only playlists never show up
	got, err := d.store.Browse(ctx, d.fan, "new", 50, 0)
	if err != nil || !slices.Equal(d.mine(got), []string{d.fresh, d.pub}) {
		t.Fatalf("new = %v %v, want [fresh pub]", d.mine(got), err)
	}

	// the shape the home screen draws
	var p Playlist
	for _, g := range got {
		if g.ID == d.pub {
			p = g
		}
	}
	if p.Owner.ID != d.owner || p.Owner.DisplayName == "" || p.OwnerID != d.owner {
		t.Fatalf("owner = %+v", p.Owner)
	}
	if !slices.Equal(p.ArtworkURLs, []string{"http://x/1.jpg", "http://x/2.jpg"}) || p.ArtworkURL != "" {
		t.Fatalf("artwork = %q %v", p.ArtworkURL, p.ArtworkURLs)
	}
	if p.TrackCount != 2 || !p.IsPublic || p.IsFeatured || p.Saved || p.SaveCount != 0 {
		t.Fatalf("summary = %+v", p)
	}

	// popular: most saved first
	for i := 0; i < 2; i++ {
		u := testutil.User(t, d.pool, "user", true)
		if _, err := d.pool.Exec(ctx, `INSERT INTO library_playlists (user_id, playlist_id) VALUES ($1,$2)`, u, d.fresh); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.pool.Exec(ctx, `INSERT INTO library_playlists (user_id, playlist_id) VALUES ($1,$2)`, d.fan, d.fresh); err != nil {
		t.Fatal(err)
	}
	got, _ = d.store.Browse(ctx, d.fan, "popular", 50, 0)
	if !slices.Equal(d.mine(got), []string{d.fresh, d.pub}) {
		t.Fatalf("popular = %v", d.mine(got))
	}
	for _, g := range got {
		if g.ID == d.fresh && (g.SaveCount != 3 || !g.Saved) {
			t.Fatalf("fresh save state = %d %v", g.SaveCount, g.Saved)
		}
	}
	saved, _ := d.store.ListSaved(ctx, d.fan, 50, 0)
	if !slices.Equal(d.mine(saved), []string{d.fresh}) {
		t.Fatalf("saved = %v", d.mine(saved))
	}

	// featured: the team's pick comes first even though it has fewer saves
	feat := true
	if _, err := d.store.Update(ctx, d.pub, d.owner, Patch{IsFeatured: &feat}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("owner featuring = %v, want forbidden", err)
	}
	if _, err := d.store.Update(ctx, d.pub, d.fan, Patch{IsFeatured: &feat}); !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrForbidden) {
		t.Fatalf("stranger featuring = %v", err)
	}
	up, err := d.store.Update(ctx, d.pub, d.admin, Patch{IsFeatured: &feat})
	if err != nil || !up.IsFeatured {
		t.Fatalf("admin featuring = %+v %v", up, err)
	}
	got, _ = d.store.Browse(ctx, d.fan, "featured", 50, 0)
	if !slices.Equal(d.mine(got), []string{d.pub, d.fresh}) {
		t.Fatalf("featured = %v, want [pub fresh]", d.mine(got))
	}
	if got2, _ := d.store.Browse(ctx, d.fan, "", 50, 0); !slices.Equal(d.mine(got2), d.mine(got)) {
		t.Fatalf("default sort must be featured: %v", d.mine(got2))
	}

	// paging and bad input
	if one, _ := d.store.Browse(ctx, d.fan, "new", 1, 0); len(one) != 1 {
		t.Fatalf("limit 1 returned %d", len(one))
	}
	var ve *ValidationError
	if _, err := d.store.Browse(ctx, d.fan, "loudest", 10, 0); !errors.As(err, &ve) {
		t.Fatalf("bad sort = %v", err)
	}

	// going private takes the playlist off the shelf and clears the feature
	priv := false
	if up, err = d.store.Update(ctx, d.pub, d.owner, Patch{IsPublic: &priv}); err != nil || up.IsFeatured {
		t.Fatalf("private = %+v %v", up, err)
	}
	if got, _ = d.store.Browse(ctx, d.fan, "featured", 50, 0); slices.Contains(d.mine(got), d.pub) {
		t.Fatal("a private playlist is still browsable")
	}
	if _, err := d.store.Update(ctx, d.pub, d.admin, Patch{IsFeatured: &feat}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("admin on a private playlist = %v, want not found", err)
	}
	pub := true
	if _, err := d.store.Update(ctx, d.pub, d.owner, Patch{IsPublic: &pub}); err != nil {
		t.Fatal(err)
	}
	if up, _ = d.store.Update(ctx, d.pub, d.owner, Patch{IsPublic: &pub}); up.IsFeatured {
		t.Fatal("republishing must not bring the feature back")
	}
}

func TestPlaylistEditing(t *testing.T) {
	d := setupDiscovery(t)
	ctx := context.Background()
	var ve *ValidationError

	// an administrator maintains editorial playlists; others cannot touch them
	name := "Blac Picks"
	up, err := d.store.Update(ctx, d.fresh, d.admin, Patch{Name: &name})
	if err != nil || up.Name != name || up.OwnerID != d.owner {
		t.Fatalf("admin rename = %+v %v", up, err)
	}
	if _, err := d.store.Update(ctx, d.fresh, d.fan, Patch{Name: &name}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stranger rename = %v, want not found", err)
	}
	if _, err := d.store.Update(ctx, d.private, d.admin, Patch{Name: &name}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("admin on private = %v, want not found", err)
	}
	pub := false
	if _, err := d.store.Update(ctx, d.fresh, d.admin, Patch{IsPublic: &pub}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("admin changing visibility of another's playlist = %v", err)
	}
	if _, err := d.store.Update(ctx, d.private, d.owner, Patch{IsFeatured: ref(true)}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("owner featuring = %v", err)
	}
	if _, err := d.store.Update(ctx, d.private, d.admin, Patch{IsFeatured: ref(true)}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("featuring a private playlist = %v", err)
	}

	// covers: your own finished cover image, "" removes it
	stranger := readyCover(t, d.pool, d.fan)
	if _, err := d.store.Update(ctx, d.fresh, d.owner, Patch{CoverImageID: &stranger}); !errors.As(err, &ve) {
		t.Fatalf("someone else's image = %v", err)
	}
	if _, err := d.store.Update(ctx, d.fresh, d.owner, Patch{CoverImageID: ref("img_00000000000000000000000000000000")}); !errors.As(err, &ve) {
		t.Fatalf("unknown image = %v", err)
	}
	mine := readyCover(t, d.pool, d.owner)
	up, err = d.store.Update(ctx, d.fresh, d.owner, Patch{CoverImageID: &mine})
	if err != nil || up.ArtworkURL != "http://test/v1/media/"+mine+"/medium.jpg" {
		t.Fatalf("cover = %+v %v", up, err)
	}
	if up.Name != name {
		t.Fatal("setting a cover must not change other fields")
	}
	adminCover := readyCover(t, d.pool, d.admin)
	if up, err = d.store.Update(ctx, d.fresh, d.admin, Patch{CoverImageID: &adminCover}); err != nil || up.ArtworkURL == "" {
		t.Fatalf("admin cover = %+v %v", up, err)
	}
	if up, err = d.store.Update(ctx, d.fresh, d.owner, Patch{CoverImageID: ref("")}); err != nil || up.ArtworkURL != "" {
		t.Fatalf("removing the cover = %+v %v", up, err)
	}

	// the mosaic follows the playlist's order and skips tracks without artwork
	p, _ := d.store.Get(ctx, d.pub, d.fan, 50, 0)
	if !slices.Equal(p.ArtworkURLs, []string{"http://x/1.jpg", "http://x/2.jpg"}) {
		t.Fatalf("mosaic = %v", p.ArtworkURLs)
	}
}

func TestPlaylistItemsAreRichTracks(t *testing.T) {
	d := setupDiscovery(t)
	ctx := context.Background()

	got, err := d.store.Get(ctx, d.pub, d.fan, 50, 0)
	if err != nil || len(got.Items) != 2 {
		t.Fatalf("items = %v %v", got, err)
	}
	tr := got.Items[0].Track
	if tr.ID != d.t1 || tr.ArtistID != d.artistID || tr.ArtistName == "" || tr.AlbumID == "" || tr.AlbumTitle == "" ||
		tr.ArtworkURL != "http://x/1.jpg" || tr.ReleaseDate == "" || tr.Genre != "Gospel" || tr.Source != "native" {
		t.Fatalf("track shape = %+v", tr)
	}
	if got.Items[0].Position != 0 || got.Items[1].Position != 1 || got.Items[0].ID == "" || got.Items[0].AddedAt.IsZero() {
		t.Fatalf("item fields = %+v", got.Items)
	}

	// the owner sees (and counts) their own unreleased track, everyone else does not
	own, _ := d.store.Get(ctx, d.draftOnly, d.owner, 50, 0)
	other, err := d.store.Get(ctx, d.draftOnly, d.fan, 50, 0)
	if err != nil || len(own.Items) != 1 || own.TrackCount != 1 || len(other.Items) != 0 || other.TrackCount != 0 {
		t.Fatalf("draft visibility: owner %d/%d, other %d/%d (%v)", len(own.Items), own.TrackCount, len(other.Items), other.TrackCount, err)
	}
	if _, err := d.store.Get(ctx, d.private, d.fan, 50, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("private playlist = %v", err)
	}
	it, err := d.store.AddItem(ctx, d.fresh, d.owner, d.t2, nil)
	if err != nil || it.Track.ArtistName == "" || it.Track.AlbumID == "" {
		t.Fatalf("added item = %+v %v", it, err)
	}
}

func TestPlaylistEndpoints(t *testing.T) {
	d := setupDiscovery(t)
	tokens := auth.NewTokenService(testutil.Secret, time.Minute)
	r := chi.NewRouter()
	r.Use(auth.Require(tokens))
	h := NewHandler(d.store)
	r.Get("/browse/playlists", h.Browse)
	r.Get("/me/playlists", h.ListMine)
	r.Get("/playlists/{id}", h.Get)
	r.Patch("/playlists/{id}", h.Update)

	call := func(method, path, user, body string) (int, map[string]any) {
		req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
		req.Header.Set("Authorization", "Bearer "+testutil.Token(t, user))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	if code, b := call("GET", "/browse/playlists?sort=popular&limit=5", d.fan, ""); code != 200 || b["playlists"] == nil {
		t.Fatalf("browse = %d %v", code, b)
	}
	if code, _ := call("GET", "/browse/playlists?sort=hot", d.fan, ""); code != http.StatusBadRequest && code != http.StatusUnprocessableEntity {
		t.Fatalf("bad sort = %d", code)
	}
	if code, b := call("PATCH", "/playlists/"+d.fresh, d.owner, `{"isFeatured":true}`); code != http.StatusForbidden {
		t.Fatalf("owner featuring = %d %v", code, b)
	}
	code, b := call("PATCH", "/playlists/"+d.fresh, d.admin, `{"isFeatured":true,"description":"Hand picked"}`)
	if code != 200 || b["isFeatured"] != true || b["description"] != "Hand picked" {
		t.Fatalf("admin featuring = %d %v", code, b)
	}
	if _, ok := b["owner"].(map[string]any); !ok {
		t.Fatalf("PATCH response lacks owner: %v", b)
	}

	// every playlist shape carries the new fields, including the detail with its items
	for _, path := range []string{"/playlists/" + d.fresh, "/me/playlists"} {
		_, b := call("GET", path, d.owner, "")
		obj := b
		if list, ok := b["playlists"].([]any); ok {
			obj = list[0].(map[string]any)
		}
		for _, k := range []string{"owner", "artworkUrls", "trackCount", "isPublic", "isFeatured", "saveCount", "saved"} {
			if _, ok := obj[k]; !ok {
				t.Fatalf("%s lacks %q: %v", path, k, obj)
			}
		}
	}
	_, b = call("GET", "/playlists/"+d.fresh, d.fan, "")
	items, _ := b["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("detail = %v", b)
	}
	track := items[0].(map[string]any)["track"].(map[string]any)
	for _, k := range []string{"albumId", "artistId", "releaseDate", "durationMs", "explicit"} {
		if _, ok := track[k]; !ok {
			t.Fatalf("track lacks %q: %v", k, track)
		}
	}
}

func TestSearchPlaylists(t *testing.T) {
	d := setupDiscovery(t)
	ctx := context.Background()
	tag := "kzx" + testutil.Unique("q") // a word nothing else contains

	set := func(id, name, desc string) {
		if _, err := d.pool.Exec(ctx, `UPDATE playlists SET name = $2, description = $3 WHERE id = $1`, id, name, desc); err != nil {
			t.Fatal(err)
		}
	}
	exact := d.pub
	set(exact, tag, "")
	starts := d.fresh
	set(starts, tag+" mix", "")
	// matches only through its description
	byDesc, err := d.store.Create(ctx, d.owner, "Plain name", "great "+tag+" songs", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.store.AddItem(ctx, byDesc.ID, d.owner, d.t1, nil); err != nil {
		t.Fatal(err)
	}
	set(d.private, tag+" private", "")  // never returned
	set(d.draftOnly, tag+" drafts", "") // nothing public to play

	ids := func(ps []Playlist) []string {
		out := []string{}
		for _, p := range ps {
			out = append(out, p.ID)
		}
		return out
	}
	got, err := d.store.Search(ctx, d.owner, tag, 20, 0)
	if err != nil || !slices.Equal(ids(got), []string{exact, starts, byDesc.ID}) {
		t.Fatalf("search = %v %v, want exact, prefix, description match; private (even the caller's own) and draft-only excluded", ids(got), err)
	}
	if got[0].Owner.ID != d.owner || len(got[0].ArtworkURLs) == 0 || got[0].TrackCount == 0 {
		t.Fatalf("shape = %+v", got[0])
	}

	// case-insensitive, and matches on the owner's display name
	if up, _ := d.store.Search(ctx, d.fan, "KZX"+tag[3:], 20, 0); len(up) != 3 {
		t.Fatalf("case-insensitive search found %d", len(up))
	}
	if _, err := d.pool.Exec(ctx, `UPDATE users SET display_name = $2 WHERE id = $1`, d.owner, "Curator "+tag); err != nil {
		t.Fatal(err)
	}
	if got, _ = d.store.Search(ctx, d.fan, "curator "+tag, 20, 0); len(got) != 3 {
		t.Fatalf("owner-name search found %d", len(got))
	}

	// featured outranks a better name match; more saves outrank fewer
	if _, err := d.store.Update(ctx, starts, d.admin, Patch{IsFeatured: ref(true)}); err != nil {
		t.Fatal(err)
	}
	if got, _ = d.store.Search(ctx, d.fan, tag, 20, 0); ids(got)[0] != starts {
		t.Fatalf("featured must come first: %v", ids(got))
	}
	// wildcard characters are literal
	if got, _ = d.store.Search(ctx, d.fan, "%", 20, 0); len(got) != 0 {
		t.Fatalf("%% matched %d playlists", len(got))
	}
	if one, _ := d.store.Search(ctx, d.fan, tag, 1, 0); len(one) != 1 {
		t.Fatal("limit not applied")
	}
}

func TestListMineContainsTrack(t *testing.T) {
	d := setupDiscovery(t)
	ctx := context.Background()
	mine, err := d.store.ListByOwner(ctx, d.owner, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range mine {
		if p.ContainsTrack != nil {
			t.Fatal("containsTrack must be absent unless asked for")
		}
	}
	if err := d.store.MarkContaining(ctx, mine, d.t2); err != nil {
		t.Fatal(err)
	}
	has := map[string]bool{}
	for _, p := range mine {
		has[p.ID] = *p.ContainsTrack
	}
	// t2 is only in "pub"
	if !has[d.pub] || has[d.fresh] || has[d.private] {
		t.Fatalf("containsTrack = %v", has)
	}
	if err := d.store.MarkContaining(ctx, nil, d.t2); err != nil {
		t.Fatal(err)
	}
}

func ref[T any](v T) *T { return &v }
