package showcase

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/testutil"
)

type env struct {
	t     *testing.T
	pool  *pgxpool.Pool
	store *Store
	admin string
}

func setup(t *testing.T) *env {
	t.Helper()
	pool := testutil.Pool(t)
	// the list is global: start empty, and leave it empty
	pool.Exec(context.Background(), `DELETE FROM showcase_items`)                       //nolint:errcheck
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM showcase_items`) }) //nolint:errcheck
	return &env{t: t, pool: pool, store: NewStore(pool, "http://api.test"), admin: testutil.User(t, pool, "admin", true)}
}

// artist makes an artist profile with an owner picture (when withPicture) and one release.
func (e *env) artist(name string, withPicture bool, visibility string) (artistID, releaseID string) {
	e.t.Helper()
	ctx := context.Background()
	owner := testutil.User(e.t, e.pool, "user", true)
	artistID = testutil.Artist(e.t, e.pool, owner, testutil.Unique(name), 1<<30)
	if withPicture {
		var img string
		if err := e.pool.QueryRow(ctx, `INSERT INTO images (id, owner_id, purpose, original_key, size_bytes, status)
			VALUES ('img_' || replace(gen_random_uuid()::text, '-', ''), $1, 'avatar', 'o', 1, 'ready') RETURNING id`, owner).Scan(&img); err != nil {
			e.t.Fatal(err)
		}
		e.pool.Exec(ctx, `UPDATE users SET avatar_image_id = $2 WHERE id = $1`, owner, img) //nolint:errcheck
	}
	releaseID = testutil.Release(e.t, e.pool, artistID, name+" release", "single", visibility)
	trackVis := "public"
	if visibility != "public" {
		trackVis = "draft"
	}
	testutil.ReadyTrack(e.t, e.pool, owner, artistID, releaseID, "t", 1, trackVis)
	return artistID, releaseID
}

func (e *env) setCover(releaseID string) {
	e.pool.Exec(context.Background(), `UPDATE albums SET artwork_url = 'http://api.test/v1/media/img_x/medium.jpg' WHERE id = $1`, releaseID) //nolint:errcheck
}

func (e *env) put(items ...Input) error { return e.store.Replace(context.Background(), e.admin, items) }

func TestCurationRules(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	good, goodRel := e.artist("Good", true, "public")
	e.setCover(goodRel)
	noPic, _ := e.artist("NoPic", false, "public")
	draft, draftRel := e.artist("Draft", true, "draft")
	e.setCover(draftRel)
	coverless := e.pool
	_ = coverless
	_, bareRel := e.artist("Bare", true, "public") // album without a cover

	var ve *ValidationError
	for name, items := range map[string][]Input{
		"unknown kind":        {{Kind: "song", ID: good, ConsentNote: "agreed"}},
		"empty id":            {{Kind: "artist", ID: " ", ConsentNote: "agreed"}},
		"unknown artist":      {{Kind: "artist", ID: "art_nope", ConsentNote: "agreed"}},
		"unknown album":       {{Kind: "album", ID: "alb_nope", ConsentNote: "agreed"}},
		"no picture":          {{Kind: "artist", ID: noPic, ConsentNote: "agreed"}},
		"no public release":   {{Kind: "artist", ID: draft, ConsentNote: "agreed"}},
		"draft album":         {{Kind: "album", ID: draftRel, ConsentNote: "agreed"}},
		"album without cover": {{Kind: "album", ID: bareRel, ConsentNote: "agreed"}},
		"duplicate":           {{Kind: "artist", ID: good, ConsentNote: "agreed"}, {Kind: "artist", ID: good, ConsentNote: "agreed"}},
		"no consent note":     {{Kind: "artist", ID: good}},
		"too short a note":    {{Kind: "artist", ID: good, ConsentNote: "ok"}},
	} {
		if err := e.put(items...); !errors.As(err, &ve) {
			t.Errorf("%s: err = %v, want a validation error", name, err)
		}
	}
	var n int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM showcase_items`).Scan(&n) //nolint:errcheck
	if n != 0 {
		t.Fatalf("a refused request stored %d rows", n)
	}
	// the message names the item at fault
	err := e.put(Input{Kind: "artist", ID: good, ConsentNote: "agreed on WhatsApp"}, Input{Kind: "artist", ID: noPic, ConsentNote: "agreed"})
	if !errors.As(err, &ve) || !strings.Contains(ve.Msg, "item 2") || !strings.Contains(ve.Msg, "picture") {
		t.Fatalf("message = %v", err)
	}
	// more than 12
	var many []Input
	for i := 0; i < 13; i++ {
		many = append(many, Input{Kind: "artist", ID: good, ConsentNote: "agreed"})
	}
	if err := e.put(many...); !errors.As(err, &ve) || !strings.Contains(ve.Msg, "12") {
		t.Fatalf("13 items = %v", err)
	}
}

func TestPublicList(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if items, err := e.store.Public(ctx); err != nil || items == nil || len(items) != 0 {
		t.Fatalf("empty list = %v %v (must be an empty array, not null)", items, err)
	}
	a1, a1Rel := e.artist("Akeine", true, "public")
	a2, _ := e.artist("Second", true, "public")
	_, albumRel := e.artist("Third", true, "public")
	e.setCover(albumRel)
	e.setCover(a1Rel)
	if err := e.put(
		Input{Kind: "album", ID: albumRel, ConsentNote: "owner uploaded the cover and agreed by message, 2026-11-24"},
		Input{Kind: "artist", ID: a2, ConsentNote: "agreed in person"},
		Input{Kind: "artist", ID: a1, ConsentNote: "manager agreed by email"},
	); err != nil {
		t.Fatal(err)
	}
	items, err := e.store.Public(ctx)
	if err != nil || len(items) != 3 {
		t.Fatalf("items = %+v %v", items, err)
	}
	if items[0].Kind != "album" || items[0].ID != albumRel || items[0].Position != 1 || items[0].ImageURL != "http://api.test/v1/media/img_x/medium.jpg" || items[0].Subtitle == "" {
		t.Fatalf("album item = %+v", items[0])
	}
	if items[1].ID != a2 || items[2].ID != a1 || items[2].Position != 3 || !strings.HasPrefix(items[2].ImageURL, "http://api.test/v1/media/img_") || !strings.HasSuffix(items[2].ImageURL, "/medium.jpg") {
		t.Fatalf("artist items = %+v", items[1:])
	}
	if !strings.HasPrefix(items[2].Subtitle, "Latest: ") {
		t.Fatalf("artist subtitle = %q", items[2].Subtitle)
	}

	// an item that stops qualifying disappears for visitors, the rest keep their order and positions close up
	e.pool.Exec(ctx, `UPDATE albums SET visibility = 'draft' WHERE id = $1`, albumRel) //nolint:errcheck
	items, _ = e.store.Public(ctx)
	if len(items) != 2 || items[0].ID != a2 || items[0].Position != 1 || items[1].Position != 2 {
		t.Fatalf("after unpublishing = %+v", items)
	}
	// ...but the team still sees it, with the reason and the consent record
	admin, _ := e.store.Admin(ctx)
	if len(admin) != 3 || admin[0].Listed || admin[0].Problem == "" || !strings.Contains(admin[0].ConsentNote, "agreed by message") || admin[0].AddedBy != e.admin || admin[0].AddedAt.IsZero() {
		t.Fatalf("admin view = %+v", admin)
	}
	if !admin[1].Listed || admin[1].Problem != "" {
		t.Fatalf("admin view of a listed item = %+v", admin[1])
	}

	// a consent note is kept for items that stay when the list is re-sent without one
	e.pool.Exec(ctx, `UPDATE albums SET visibility = 'public' WHERE id = $1`, albumRel) //nolint:errcheck
	if err := e.put(Input{Kind: "artist", ID: a1}, Input{Kind: "album", ID: albumRel}); err != nil {
		t.Fatalf("re-sending without notes: %v", err)
	}
	admin, _ = e.store.Admin(ctx)
	if len(admin) != 2 || admin[0].ID != a1 || admin[0].ConsentNote != "manager agreed by email" || !strings.Contains(admin[1].ConsentNote, "agreed by message") {
		t.Fatalf("notes after reorder = %+v", admin)
	}
	// clearing the list
	if err := e.put(); err != nil {
		t.Fatal(err)
	}
	if items, _ = e.store.Public(ctx); len(items) != 0 {
		t.Fatal("list not cleared")
	}
}

func TestPublicHandler(t *testing.T) {
	e := setup(t)
	a, rel := e.artist("H", true, "public")
	e.setCover(rel)
	if err := e.put(Input{Kind: "artist", ID: a, ConsentNote: "agreed"}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	NewHandler(e.store).List(rec, httptest.NewRequest("GET", "/showcase", nil)) // no Authorization header
	var out struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "public, max-age=3600" || len(out.Items) != 1 {
		t.Fatalf("%d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	for _, k := range []string{"id", "kind", "name", "imageUrl", "position"} {
		if _, ok := out.Items[0][k]; !ok {
			t.Fatalf("item lacks %q: %v", k, out.Items[0])
		}
	}
	if _, leaked := out.Items[0]["consentNote"]; leaked {
		t.Fatal("the consent record is internal and must not be public")
	}
	e.put() //nolint:errcheck
	rec = httptest.NewRecorder()
	NewHandler(e.store).List(rec, httptest.NewRequest("GET", "/showcase", nil))
	if !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("empty = %s", rec.Body)
	}
}
