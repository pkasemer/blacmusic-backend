package playlist

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/db"
)

// Integration test: needs a Postgres, e.g.
//
//	TEST_DATABASE_URL=postgres://blac:blac@localhost:5432/blac?sslmode=disable go test ./internal/playlist
func setup(t *testing.T) (*Store, *pgxpool.Pool, string, []string) {
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

	suffix := fmt.Sprint(time.Now().UnixNano())
	var uid string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users(email, password_hash, display_name) VALUES ($1,'x','T') RETURNING id::text`,
		"plt-"+suffix+"@test.local").Scan(&uid); err != nil {
		t.Fatal(err)
	}
	var tracks []string
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("trk_plt%s_%d", suffix, i)
		if _, err := pool.Exec(ctx, `INSERT INTO tracks(id, title) VALUES ($1, $2)`, id, "T"+fmt.Sprint(i)); err != nil {
			t.Fatal(err)
		}
		tracks = append(tracks, id)
	}
	t.Cleanup(func() {
		pool.Exec(ctx, `DELETE FROM users WHERE id::text = $1`, uid)    //nolint:errcheck
		pool.Exec(ctx, `DELETE FROM tracks WHERE id = ANY($1)`, tracks) //nolint:errcheck
	})
	return NewStore(pool), pool, uid, tracks
}

func order(t *testing.T, s *Store, id, viewer string) []string {
	t.Helper()
	d, err := s.Get(context.Background(), id, viewer, 500, 0)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(d.Items))
	for i, it := range d.Items {
		if it.Position != i {
			t.Fatalf("item %d has position %d (positions must be contiguous)", i, it.Position)
		}
		ids[i] = it.Track.ID
	}
	return ids
}

func TestOrderingAddMoveRemove(t *testing.T) {
	s, _, uid, tr := setup(t)
	ctx := context.Background()
	p, err := s.Create(ctx, uid, "Mix", "", false)
	if err != nil {
		t.Fatal(err)
	}

	var items []*Item
	for _, id := range tr[:4] { // a b c d
		it, err := s.AddItem(ctx, p.ID, uid, id, nil)
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, it)
	}
	if got := order(t, s, p.ID, uid); !slices.Equal(got, tr[:4]) {
		t.Fatalf("after append: %v", got)
	}

	// insert e at index 1 -> a e b c d
	e, err := s.AddItem(ctx, p.ID, uid, tr[4], ptr(1))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{tr[0], tr[4], tr[1], tr[2], tr[3]}
	if got := order(t, s, p.ID, uid); !slices.Equal(got, want) {
		t.Fatalf("after insert: %v want %v", got, want)
	}

	// move e to the end -> a b c d e
	if err := s.MoveItem(ctx, p.ID, uid, e.ID, 4); err != nil {
		t.Fatal(err)
	}
	want = []string{tr[0], tr[1], tr[2], tr[3], tr[4]}
	if got := order(t, s, p.ID, uid); !slices.Equal(got, want) {
		t.Fatalf("after move to end: %v want %v", got, want)
	}

	// move e to the front -> e a b c d
	if err := s.MoveItem(ctx, p.ID, uid, e.ID, 0); err != nil {
		t.Fatal(err)
	}
	want = []string{tr[4], tr[0], tr[1], tr[2], tr[3]}
	if got := order(t, s, p.ID, uid); !slices.Equal(got, want) {
		t.Fatalf("after move to front: %v want %v", got, want)
	}

	// out-of-range position clamps to the end
	if err := s.MoveItem(ctx, p.ID, uid, e.ID, 99); err != nil {
		t.Fatal(err)
	}
	want = []string{tr[0], tr[1], tr[2], tr[3], tr[4]}
	if got := order(t, s, p.ID, uid); !slices.Equal(got, want) {
		t.Fatalf("after clamped move: %v want %v", got, want)
	}

	// remove b (items[1]) -> a c d e
	if err := s.RemoveItem(ctx, p.ID, uid, items[1].ID); err != nil {
		t.Fatal(err)
	}
	want = []string{tr[0], tr[2], tr[3], tr[4]}
	if got := order(t, s, p.ID, uid); !slices.Equal(got, want) {
		t.Fatalf("after remove: %v want %v", got, want)
	}
	if err := s.RemoveItem(ctx, p.ID, uid, items[1].ID); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("second remove = %v, want ErrItemNotFound", err)
	}
}

func TestDuplicateTracksAllowed(t *testing.T) {
	s, _, uid, tr := setup(t)
	ctx := context.Background()
	p, _ := s.Create(ctx, uid, "Dups", "", false)
	for i := 0; i < 3; i++ {
		if _, err := s.AddItem(ctx, p.ID, uid, tr[0], nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := order(t, s, p.ID, uid); len(got) != 3 {
		t.Fatalf("want 3 items, got %v", got)
	}
}

func TestAccessControl(t *testing.T) {
	s, pool, uid, tr := setup(t)
	ctx := context.Background()
	var other string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users(email, password_hash, display_name) VALUES ($1,'x','O') RETURNING id::text`,
		fmt.Sprintf("plt-other-%d@test.local", time.Now().UnixNano())).Scan(&other); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Exec(ctx, `DELETE FROM users WHERE id::text = $1`, other) }) //nolint:errcheck

	p, _ := s.Create(ctx, uid, "Private", "", false)
	if _, err := s.Get(ctx, p.ID, other, 10, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other viewing private = %v, want ErrNotFound", err)
	}
	if _, err := s.AddItem(ctx, p.ID, other, tr[0], nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other adding = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, p.ID, other); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other deleting = %v, want ErrNotFound", err)
	}

	pub := true
	if _, err := s.Update(ctx, p.ID, uid, Patch{IsPublic: &pub}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, p.ID, other, 10, 0); err != nil {
		t.Fatalf("other viewing public = %v", err)
	}
	if _, err := s.AddItem(ctx, p.ID, other, tr[0], nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other adding to public = %v, want ErrNotFound", err)
	}
}

func TestValidationAndUnknownTrack(t *testing.T) {
	s, _, uid, _ := setup(t)
	ctx := context.Background()
	var ve *ValidationError
	if _, err := s.Create(ctx, uid, "", "", false); !errors.As(err, &ve) {
		t.Fatalf("empty name = %v, want ValidationError", err)
	}
	p, _ := s.Create(ctx, uid, "X", "", false)
	if _, err := s.AddItem(ctx, p.ID, uid, "trk_does_not_exist", nil); !errors.Is(err, ErrTrackNotFound) {
		t.Fatalf("unknown track = %v, want ErrTrackNotFound", err)
	}
	if got := order(t, s, p.ID, uid); len(got) != 0 {
		t.Fatalf("failed add must not leave items: %v", got)
	}
}

func TestConcurrentAddsKeepPositionsContiguous(t *testing.T) {
	s, _, uid, tr := setup(t)
	ctx := context.Background()
	p, _ := s.Create(ctx, uid, "Race", "", false)

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var pos *int
			if i%2 == 0 {
				pos = ptr(0) // half insert at the front, half append
			}
			_, err := s.AddItem(ctx, p.ID, uid, tr[i%len(tr)], pos)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := order(t, s, p.ID, uid); len(got) != n { // order() also asserts contiguity
		t.Fatalf("want %d items, got %d", n, len(got))
	}
}

func ptr(i int) *int { return &i }
