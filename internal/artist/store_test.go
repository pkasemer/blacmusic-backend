package artist

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/testutil"
)

// Integration tests: need a Postgres (TEST_DATABASE_URL).
type env struct {
	t     *testing.T
	pool  *pgxpool.Pool
	store *Store
}

func setup(t *testing.T) *env {
	t.Helper()
	pool := testutil.Pool(t)
	return &env{t: t, pool: pool, store: NewStore(pool, 10*testutil.GiB, "http://api.test")}
}

// name makes artist names unique so tests never collide with each other.
func name(prefix string) string { return testutil.Unique(prefix) }

func TestCreateRequiresVerifiedEmailAndOnlyOnce(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	unverified := testutil.User(t, e.pool, "user", false)
	if _, err := e.store.Create(ctx, unverified, name("A"), ""); !errors.Is(err, ErrEmailNotVerified) {
		t.Fatalf("unverified email = %v, want ErrEmailNotVerified", err)
	}

	uid := testutil.User(t, e.pool, "user", true)
	a, err := e.store.Create(ctx, uid, "  "+name("Kiwanuka")+"  ", " Kampala-based ")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a.ID, "art_") || !a.Mine || a.Verification != "unverified" || a.Verified || a.Bio != "Kampala-based" {
		t.Fatalf("created artist: %+v", a)
	}
	if strings.HasPrefix(a.Name, " ") {
		t.Fatal("the name must be trimmed")
	}
	if _, err := e.store.Create(ctx, uid, name("Second"), ""); !errors.Is(err, ErrExists) {
		t.Fatalf("second profile = %v, want ErrExists", err)
	}

	// The welcome allowance is 10 GiB.
	q, err := e.store.Quota(ctx, a.ID, uid)
	if err != nil {
		t.Fatal(err)
	}
	if q.FreeStorageBytes != 10*testutil.GiB || q.TotalStorageBytes != 10*testutil.GiB || q.UsedStorageBytes != 0 ||
		q.RemainingStorageBytes != 10*testutil.GiB || q.Plan != "free" || q.PlanStatus != "active" {
		t.Fatalf("quota: %+v", q)
	}

	var ve *ValidationError
	for _, bad := range []string{"", "   ", strings.Repeat("x", 101)} {
		if _, err := e.store.Create(ctx, testutil.User(t, e.pool, "user", true), bad, ""); !errors.As(err, &ve) {
			t.Errorf("name %q = %v, want ValidationError", bad, err)
		}
	}
	if _, err := e.store.Create(ctx, testutil.User(t, e.pool, "user", true), name("B"), strings.Repeat("b", 1001)); !errors.As(err, &ve) {
		t.Errorf("long bio = %v, want ValidationError", err)
	}
}

func TestOwnerSeesWorkflowStateOthersDoNot(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid := testutil.User(t, e.pool, "user", true)
	a, _ := e.store.Create(ctx, uid, name("Visible"), "")
	if err := e.store.RequestVerification(ctx, a.ID, uid, "I am real"); err != nil {
		t.Fatal(err)
	}
	stranger := testutil.User(t, e.pool, "user", true)
	seen, err := e.store.Get(ctx, a.ID, stranger)
	if err != nil {
		t.Fatal(err)
	}
	if seen.Mine || seen.Verification != "" || seen.VerificationNote != "" || seen.Verified {
		t.Fatalf("a stranger must not see the workflow state: %+v", seen)
	}
	mine, _ := e.store.GetByOwner(ctx, uid)
	if !mine.Mine || mine.Verification != "pending" {
		t.Fatalf("owner view: %+v", mine)
	}
	if _, err := e.store.Get(ctx, "art_nope", uid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown = %v", err)
	}
}

func TestVerificationWorkflow(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid := testutil.User(t, e.pool, "user", true)
	other := testutil.User(t, e.pool, "user", true)
	a, _ := e.store.Create(ctx, uid, name("Badge"), "")

	if err := e.store.RequestVerification(ctx, a.ID, other, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("someone else's request = %v, want ErrNotFound", err)
	}
	if err := e.store.RequestVerification(ctx, a.ID, uid, "Please verify me"); err != nil {
		t.Fatal(err)
	}
	if err := e.store.RequestVerification(ctx, a.ID, uid, "again"); err != nil { // idempotent while pending
		t.Fatalf("second request = %v", err)
	}

	queue, err := e.store.ReviewQueue(ctx, "pending", 200, 0)
	if err != nil {
		t.Fatal(err)
	}
	var item *AdminArtist
	for _, q := range queue {
		if q.ID == a.ID {
			item = q
		}
	}
	if item == nil || item.Message != "Please verify me" || item.OwnerEmail == "" || item.RequestedAt == nil {
		t.Fatalf("queue item: %+v", item)
	}

	// A rejection needs a reason, and the artist may ask again afterwards.
	var ve *ValidationError
	if err := e.store.Decide(ctx, a.ID, false, " "); !errors.As(err, &ve) {
		t.Fatalf("reject without reason = %v", err)
	}
	if err := e.store.Decide(ctx, a.ID, false, "Not enough evidence"); err != nil {
		t.Fatal(err)
	}
	mine, _ := e.store.GetByOwner(ctx, uid)
	if mine.Verification != "rejected" || mine.VerificationNote != "Not enough evidence" {
		t.Fatalf("after rejection: %+v", mine)
	}
	if err := e.store.Decide(ctx, a.ID, true, ""); !errors.Is(err, ErrNotPending) {
		t.Fatalf("deciding a non-pending request = %v, want ErrNotPending", err)
	}
	if err := e.store.RequestVerification(ctx, a.ID, uid, "Here is my proof"); err != nil {
		t.Fatal(err)
	}
	if mine, _ = e.store.GetByOwner(ctx, uid); mine.Verification != "pending" || mine.VerificationNote != "" {
		t.Fatalf("re-request: %+v", mine)
	}
	if err := e.store.Decide(ctx, a.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	if mine, _ = e.store.GetByOwner(ctx, uid); !mine.Verified || mine.Verification != "verified" {
		t.Fatalf("after approval: %+v", mine)
	}
	if err := e.store.RequestVerification(ctx, a.ID, uid, ""); !errors.Is(err, ErrAlreadyVerified) {
		t.Fatalf("request when verified = %v, want ErrAlreadyVerified", err)
	}
	if err := e.store.Decide(ctx, "art_nope", true, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("decide unknown = %v, want ErrNotFound", err)
	}
}

func TestRenamingAVerifiedArtistRemovesTheBadge(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid := testutil.User(t, e.pool, "user", true)
	a, _ := e.store.Create(ctx, uid, name("Original"), "")
	e.store.RequestVerification(ctx, a.ID, uid, "") //nolint:errcheck
	if err := e.store.Decide(ctx, a.ID, true, ""); err != nil {
		t.Fatal(err)
	}

	bio := "A new bio"
	if got, err := e.store.Update(ctx, a.ID, uid, Patch{Bio: &bio}); err != nil || !got.Verified {
		t.Fatalf("editing the bio keeps the badge: %+v %v", got, err)
	}
	newName := name("Renamed")
	got, err := e.store.Update(ctx, a.ID, uid, Patch{Name: &newName})
	if err != nil {
		t.Fatal(err)
	}
	if got.Verified || got.Verification != "unverified" || got.Name != newName {
		t.Fatalf("after rename: %+v", got)
	}
	other := testutil.User(t, e.pool, "user", true)
	if _, err := e.store.Update(ctx, a.ID, other, Patch{Bio: &bio}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("someone else's edit = %v, want ErrNotFound", err)
	}
	empty := ""
	var ve *ValidationError
	if _, err := e.store.Update(ctx, a.ID, uid, Patch{Name: &empty}); !errors.As(err, &ve) {
		t.Fatalf("empty name = %v, want ValidationError", err)
	}
}

func TestNameMatchingAVerifiedArtistIsFlagged(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	famous := name("Famous Star")
	v := testutil.User(t, e.pool, "user", true)
	a, _ := e.store.Create(ctx, v, famous, "")
	e.store.RequestVerification(ctx, a.ID, v, "") //nolint:errcheck
	if err := e.store.Decide(ctx, a.ID, true, ""); err != nil {
		t.Fatal(err)
	}

	impostor := testutil.User(t, e.pool, "user", true)
	b, err := e.store.Create(ctx, impostor, strings.ToUpper(famous), "")
	if err != nil {
		t.Fatalf("a matching name is allowed but flagged, got %v", err)
	}
	flagged, err := e.store.ReviewQueue(ctx, "flagged", 200, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range flagged {
		found = found || (f.ID == b.ID && f.Flagged)
		if f.ID == a.ID {
			t.Fatal("the verified artist itself must not be flagged")
		}
	}
	if !found {
		t.Fatal("the look-alike must appear in the flagged queue")
	}
}

func TestAvatarMustBeAFinishedOwnedAvatarImage(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid := testutil.User(t, e.pool, "user", true)
	other := testutil.User(t, e.pool, "user", true)
	a, _ := e.store.Create(ctx, uid, name("Avatar"), "")

	insert := func(owner, purpose, status string) string {
		var id string
		if err := e.pool.QueryRow(ctx, `INSERT INTO images (owner_id, purpose, status, original_key, size_bytes)
			VALUES ($1,$2,$3,'k',1) RETURNING id`, owner, purpose, status).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	var ve *ValidationError
	for name, id := range map[string]string{
		"someone else's": insert(other, "avatar", "ready"),
		"a cover":        insert(uid, "cover", "ready"),
		"unfinished":     insert(uid, "avatar", "pending"),
		"nonexistent":    "img_00000000000000000000000000000000",
		"malformed":      "../etc",
	} {
		id := id
		if _, err := e.store.Update(ctx, a.ID, uid, Patch{AvatarImageID: &id}); !errors.As(err, &ve) {
			t.Errorf("%s image = %v, want ValidationError", name, err)
		}
	}
	good := insert(uid, "avatar", "ready")
	got, err := e.store.Update(ctx, a.ID, uid, Patch{AvatarImageID: &good})
	if err != nil {
		t.Fatal(err)
	}
	if want := "http://api.test/v1/media/" + good + "/medium.jpg"; got.ImageURL != want {
		t.Fatalf("imageUrl = %q, want %q", got.ImageURL, want)
	}
	none := ""
	if got, _ = e.store.Update(ctx, a.ID, uid, Patch{AvatarImageID: &none}); got.ImageURL != "" {
		t.Fatalf("clearing the avatar: %q", got.ImageURL)
	}
}

func TestQuotaLedger(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	uid := testutil.User(t, e.pool, "user", true)
	a, _ := e.store.Create(ctx, uid, name("Ledger"), "")

	granted, err := e.store.Grant(ctx, a.ID, "purchase", 5*testutil.GiB, "5 GB pack", "pay_123", nil)
	if err != nil || !granted {
		t.Fatalf("grant: %v %v", granted, err)
	}
	if again, err := e.store.Grant(ctx, a.ID, "purchase", 5*testutil.GiB, "5 GB pack", "pay_123", nil); err != nil || again {
		t.Fatalf("a repeated reference must change nothing: created=%v err=%v", again, err)
	}
	q, _ := e.store.Quota(ctx, a.ID, uid)
	if q.PurchasedStorageBytes != 5*testutil.GiB || q.TotalStorageBytes != 15*testutil.GiB || q.Plan != "paid" {
		t.Fatalf("after one purchase: %+v", q)
	}

	// An expired grant stops counting; a negative adjustment (refund) reduces it.
	past := time.Now().Add(-time.Hour)
	if _, err := e.store.Grant(ctx, a.ID, "promo", 100*testutil.GiB, "expired promo", "", &past); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Grant(ctx, a.ID, "adjustment", -2*testutil.GiB, "refund", "", nil); err != nil {
		t.Fatal(err)
	}
	q, _ = e.store.Quota(ctx, a.ID, uid)
	if q.TotalStorageBytes != 13*testutil.GiB {
		t.Fatalf("total = %d GiB, want 13", q.TotalStorageBytes/testutil.GiB)
	}

	var ve *ValidationError
	if _, err := e.store.Grant(ctx, a.ID, "free", 1, "", "", nil); !errors.As(err, &ve) {
		t.Fatalf("kind free via admin = %v", err)
	}
	if _, err := e.store.Grant(ctx, a.ID, "purchase", -1, "", "", nil); !errors.As(err, &ve) {
		t.Fatalf("negative purchase = %v", err)
	}
	if _, err := e.store.Grant(ctx, a.ID, "purchase", 0, "", "", nil); !errors.As(err, &ve) {
		t.Fatalf("zero grant = %v", err)
	}
	if _, err := e.store.Grant(ctx, "art_nope", "promo", 1, "", "", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown artist = %v", err)
	}
	// Someone else cannot read the quota.
	other := testutil.User(t, e.pool, "user", true)
	if _, err := e.store.Quota(ctx, a.ID, other); !errors.Is(err, ErrNotFound) {
		t.Fatalf("quota of another artist = %v, want ErrNotFound", err)
	}
}
