package upload

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/artist"
	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/blobtest"
	"github.com/blacmusic/backend/internal/release"
	"github.com/blacmusic/backend/internal/testutil"
)

// Integration tests: need a Postgres, e.g.
//
//	TEST_DATABASE_URL=postgres://blac:blac@localhost:5432/blac_test?sslmode=disable go test ./internal/upload
type env struct {
	t      *testing.T
	pool   *pgxpool.Pool
	blobs  *blobtest.Mem
	store  *Store
	router http.Handler
}

const (
	testMax  = 10 << 20
	testPart = 5000 // files over twice this size use resumable uploads
)

func setup(t *testing.T) *env {
	t.Helper()
	pool := testutil.Pool(t)
	blobs := blobtest.New()
	store := NewStore(pool)
	h := NewHandler(store, blobs, Options{MaxBytes: testMax, URLTTL: time.Hour, PartSize: testPart})
	h.Tracks = release.NewStore(pool, blobs, "http://api.test")
	r := chi.NewRouter()
	r.Group(func(g chi.Router) {
		g.Use(auth.Require(auth.NewTokenService(testutil.Secret, time.Minute)))
		g.Route("/uploads", h.Routes)
		h.TrackRoutes(g)
	})
	return &env{t: t, pool: pool, blobs: blobs, store: store, router: r}
}

// artistUser creates a user with an artist profile and `free` bytes of storage.
func (e *env) artistUser(free int64) (token, userID, artistID string) {
	e.t.Helper()
	userID = testutil.User(e.t, e.pool, "user", true)
	artistID = testutil.Artist(e.t, e.pool, userID, testutil.Unique("Band"), free)
	return testutil.Token(e.t, userID), userID, artistID
}

func (e *env) do(method, path, token string, body any) (int, map[string]any) {
	e.t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func body(size int64) map[string]any {
	return map[string]any{
		"title": "Song", "filename": "song.flac", "sizeBytes": size,
		"rightsAccepted": true, "termsVersion": "2024-01",
	}
}

func errCode(m map[string]any) string {
	if e, ok := m["error"].(map[string]any); ok {
		s, _ := e["code"].(string)
		return s
	}
	return ""
}

func (e *env) quota(artistID string) *artist.Quota {
	e.t.Helper()
	q, err := artist.QuotaFor(context.Background(), e.pool, artistID)
	if err != nil {
		e.t.Fatal(err)
	}
	return q
}

func TestUploadNeedsAnArtistProfile(t *testing.T) {
	e := setup(t)
	plain := testutil.Token(t, testutil.User(t, e.pool, "user", true))
	if code, b := e.do("POST", "/uploads", plain, body(1234)); code != 403 || errCode(b) != "artist_required" {
		t.Fatalf("no artist = %d %v, want 403 artist_required", code, b)
	}
	// Listing your (empty) uploads needs no profile.
	if code, _ := e.do("GET", "/uploads", plain, nil); code != 200 {
		t.Fatalf("list = %d", code)
	}
	tok, _, _ := e.artistUser(testutil.GiB)
	if code, b := e.do("POST", "/uploads", tok, body(1234)); code != 201 {
		t.Fatalf("artist = %d %v, want 201", code, b)
	}
}

func TestCreateMakesADraftSingleAndHidesTrackID(t *testing.T) {
	e := setup(t)
	tok, _, artistID := e.artistUser(testutil.GiB)
	code, b := e.do("POST", "/uploads", tok, body(1234))
	if code != 201 {
		t.Fatalf("create = %d %v", code, b)
	}
	id, _ := b["id"].(string)
	target, _ := b["upload"].(map[string]any)
	if !strings.HasPrefix(id, "upl_") || target["type"] != "single" || target["method"] != "PUT" || !strings.Contains(target["url"].(string), "originals/"+id) {
		t.Fatalf("bad response: %v", b)
	}
	if b["status"] != "pending" || b["trackId"] != nil || b["maxBytes"].(float64) != testMax {
		t.Fatalf("status/trackId/maxBytes wrong: %v", b)
	}
	rel, _ := b["releaseId"].(string)
	var typ, vis, art string
	if err := e.pool.QueryRow(context.Background(), `SELECT release_type, visibility, artist_id FROM albums WHERE id=$1`, rel).Scan(&typ, &vis, &art); err != nil {
		t.Fatal(err)
	}
	if typ != "single" || vis != "draft" || art != artistID {
		t.Fatalf("auto release = %s %s %s", typ, vis, art)
	}
	var terms string
	var at *time.Time
	if err := e.pool.QueryRow(context.Background(), `SELECT rights_terms_version, rights_accepted_at FROM uploads WHERE id=$1`, id).Scan(&terms, &at); err != nil {
		t.Fatal(err)
	}
	if terms != "2024-01" || at == nil {
		t.Fatalf("rights acceptance not stored: %q %v", terms, at)
	}
}

func TestRightsMustBeAccepted(t *testing.T) {
	e := setup(t)
	tok, _, artistID := e.artistUser(testutil.GiB)
	b := body(100)
	b["rightsAccepted"] = false
	if code, resp := e.do("POST", "/uploads", tok, b); code != 422 || errCode(resp) != "rights_not_accepted" {
		t.Fatalf("not accepted = %d %v", code, resp)
	}
	delete(b, "rightsAccepted")
	if code, resp := e.do("POST", "/uploads", tok, b); code != 422 || errCode(resp) != "rights_not_accepted" {
		t.Fatalf("missing = %d %v", code, resp)
	}
	if q := e.quota(artistID); q.UsedStorageBytes != 0 {
		t.Fatal("a refused upload must not reserve storage")
	}
}

func TestValidation(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.artistUser(testutil.GiB)
	cases := map[string]func(m map[string]any){
		"empty title":     func(m map[string]any) { m["title"] = "  " },
		"long title":      func(m map[string]any) { m["title"] = strings.Repeat("x", 201) },
		"no filename":     func(m map[string]any) { m["filename"] = "" },
		"zero size":       func(m map[string]any) { m["sizeBytes"] = 0 },
		"over size limit": func(m map[string]any) { m["sizeBytes"] = testMax + 1 },
		"long genre":      func(m map[string]any) { m["genre"] = strings.Repeat("g", 41) },
		"track number 0":  func(m map[string]any) { m["trackNumber"] = 0 },
		"no terms":        func(m map[string]any) { m["termsVersion"] = "" },
	}
	for name, mutate := range cases {
		b := body(100)
		mutate(b)
		if code, resp := e.do("POST", "/uploads", tok, b); code != 422 || errCode(resp) != "validation_error" {
			t.Errorf("%s = %d %v, want 422", name, code, resp)
		}
	}
	if code, _ := e.do("POST", "/uploads", tok, map[string]any{"title": "x", "bogus": 1}); code != 400 {
		t.Errorf("unknown field = %d, want 400", code)
	}
}

func TestCompleteFlow(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.artistUser(testutil.GiB)
	_, created := e.do("POST", "/uploads", tok, body(21))
	id := created["id"].(string)

	if code, b := e.do("POST", "/uploads/"+id+"/complete", tok, nil); code != 409 || errCode(b) != "not_uploaded" {
		t.Fatalf("complete before PUT = %d %v, want 409 not_uploaded", code, b)
	}
	e.blobs.Put("originals/"+id, []byte("pretend this is audio"))
	if code, b := e.do("POST", "/uploads/"+id+"/complete", tok, nil); code != 202 || b["status"] != "queued" {
		t.Fatalf("complete = %d %v, want 202 queued", code, b)
	}
	if code, b := e.do("POST", "/uploads/"+id+"/complete", tok, nil); code != 200 || b["status"] != "queued" {
		t.Fatalf("second complete = %d %v, want 200 queued", code, b)
	}
	if code, b := e.do("GET", "/uploads/"+id, tok, nil); code != 200 || b["status"] != "queued" {
		t.Fatalf("get = %d %v", code, b)
	}
	code, list := e.do("GET", "/uploads", tok, nil)
	if items, _ := list["uploads"].([]any); code != 200 || len(items) != 1 {
		t.Fatalf("list = %d %v", code, list)
	}
}

func TestEmptyOrOversizedFileIsRejectedAndFreesQuota(t *testing.T) {
	e := setup(t)
	tok, _, artistID := e.artistUser(testutil.GiB)

	_, created := e.do("POST", "/uploads", tok, body(1000))
	id := created["id"].(string)
	if e.quota(artistID).UsedStorageBytes != 1000 {
		t.Fatalf("the declared size should be reserved, used = %d", e.quota(artistID).UsedStorageBytes)
	}
	e.blobs.Put("originals/"+id, nil)
	if code, _ := e.do("POST", "/uploads/"+id+"/complete", tok, nil); code != 422 {
		t.Fatalf("empty file = %d, want 422", code)
	}
	if _, ok := e.blobs.Get("originals/" + id); ok {
		t.Fatal("rejected file must be removed from storage")
	}
	if _, b := e.do("GET", "/uploads/"+id, tok, nil); b["status"] != "failed" || b["error"] == "" {
		t.Fatalf("status = %v, want failed with a message", b)
	}
	if e.quota(artistID).UsedStorageBytes != 0 {
		t.Fatal("a failed upload must free its reserved storage")
	}

	_, created = e.do("POST", "/uploads", tok, body(10))
	id = created["id"].(string)
	e.blobs.Put("originals/"+id, make([]byte, testMax+1)) // the client lied about the size
	if code, _ := e.do("POST", "/uploads/"+id+"/complete", tok, nil); code != 422 {
		t.Fatalf("oversized file = %d, want 422", code)
	}
}

func TestOtherUsersCannotSeeUploads(t *testing.T) {
	e := setup(t)
	owner, _, _ := e.artistUser(testutil.GiB)
	other, _, _ := e.artistUser(testutil.GiB)
	_, created := e.do("POST", "/uploads", owner, body(1))
	id := created["id"].(string)
	e.blobs.Put("originals/"+id, []byte("x"))

	for _, c := range []struct{ m, p string }{{"GET", "/uploads/" + id}, {"POST", "/uploads/" + id + "/complete"}, {"DELETE", "/uploads/" + id}} {
		if code, _ := e.do(c.m, c.p, other, nil); code != 404 {
			t.Errorf("%s %s as other user = %d, want 404", c.m, c.p, code)
		}
	}
	if _, b := e.do("GET", "/uploads/"+id, owner, nil); b["status"] != "pending" {
		t.Fatal("another user's calls must not change the upload")
	}
}

func TestStorageQuota(t *testing.T) {
	e := setup(t)
	tok, _, artistID := e.artistUser(1000)

	code, first := e.do("POST", "/uploads", tok, body(600))
	if code != 201 {
		t.Fatalf("first = %d %v", code, first)
	}
	// 600 of 1000 reserved: a 500-byte file no longer fits.
	var releasesBefore int
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM albums WHERE artist_id=$1`, artistID).Scan(&releasesBefore) //nolint:errcheck
	code, b := e.do("POST", "/uploads", tok, body(500))
	if code != 402 || errCode(b) != "quota_exceeded" {
		t.Fatalf("over quota = %d %v, want 402 quota_exceeded", code, b)
	}
	d := b["error"].(map[string]any)["details"].(map[string]any)
	if d["remainingBytes"].(float64) != 400 || d["limitBytes"].(float64) != 1000 || d["usedBytes"].(float64) != 600 || d["requiredBytes"].(float64) != 500 {
		t.Fatalf("details = %v", d)
	}
	var releasesAfter int
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM albums WHERE artist_id=$1`, artistID).Scan(&releasesAfter) //nolint:errcheck
	if releasesAfter != releasesBefore {
		t.Fatal("a refused upload must not leave a stray draft release behind")
	}
	if code, _ := e.do("POST", "/uploads", tok, body(400)); code != 201 { // exactly fits
		t.Fatalf("exact fit = %d, want 201", code)
	}

	// Cancelling a pending upload hands its storage back.
	id := first["id"].(string)
	e.blobs.Put("originals/"+id, []byte("partial"))
	if code, _ := e.do("DELETE", "/uploads/"+id, tok, nil); code != 204 {
		t.Fatalf("delete pending = %d", code)
	}
	if _, ok := e.blobs.Get("originals/" + id); ok {
		t.Fatal("deleting an upload must remove its file")
	}
	if got := e.quota(artistID).UsedStorageBytes; got != 400 {
		t.Fatalf("used = %d, want 400 after cancelling the 600-byte upload", got)
	}

	// A queued upload cannot be cancelled this way.
	_, q := e.do("POST", "/uploads", tok, body(10))
	qid := q["id"].(string)
	e.blobs.Put("originals/"+qid, []byte("x"))
	e.do("POST", "/uploads/"+qid+"/complete", tok, nil)
	if code, b := e.do("DELETE", "/uploads/"+qid, tok, nil); code != 409 || errCode(b) != "in_progress" {
		t.Fatalf("delete queued = %d %v, want 409 in_progress", code, b)
	}
}

func TestStorageUsedIgnoresFailedAndCountsReady(t *testing.T) {
	e := setup(t)
	_, userID, artistID := e.artistUser(10_000)
	rel := testutil.Release(t, e.pool, artistID, "R", "album", "draft")
	testutil.ReadyTrack(t, e.pool, userID, artistID, rel, "T", 3000, "draft")
	if _, err := e.pool.Exec(context.Background(), `
		INSERT INTO uploads (user_id, artist_id, release_id, title, artist_name, filename, size_bytes, original_key, status)
		VALUES ($1,$2,$3,'f','x','f',5000,'originals/failed-test','failed')`, userID, artistID, rel); err != nil {
		t.Fatal(err)
	}
	q := e.quota(artistID)
	if q.UsedStorageBytes != 3000 || q.RemainingStorageBytes != 7000 {
		t.Fatalf("used=%d remaining=%d, want 3000 / 7000", q.UsedStorageBytes, q.RemainingStorageBytes)
	}
}

func TestParallelUploadsCannotOvershootQuota(t *testing.T) {
	e := setup(t)
	_, userID, artistID := e.artistUser(1000)
	const n = 8
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := e.store.Create(context.Background(), NewUpload{
				UserID: userID, Title: "T", Filename: "f", SizeBytes: 400, TermsVersion: "v1",
			})
			results <- err
		}()
	}
	ok := 0
	for i := 0; i < n; i++ {
		if err := <-results; err == nil {
			ok++
		}
	}
	if ok != 2 {
		t.Fatalf("%d of %d parallel 400-byte uploads were accepted into 1000 bytes, want exactly 2", ok, n)
	}
	if q := e.quota(artistID); q.UsedStorageBytes != 800 {
		t.Fatalf("used = %d, want 800", q.UsedStorageBytes)
	}
}

func TestAttachToReleaseChecksOwnership(t *testing.T) {
	e := setup(t)
	tok, _, artistID := e.artistUser(testutil.GiB)
	otherTok, otherUser, otherArtist := e.artistUser(testutil.GiB)
	_ = otherUser
	mine := testutil.Release(t, e.pool, artistID, "Mine", "album", "draft")
	theirs := testutil.Release(t, e.pool, otherArtist, "Theirs", "album", "draft")

	b := body(10)
	b["releaseId"], b["trackNumber"] = mine, 3
	code, resp := e.do("POST", "/uploads", tok, b)
	if code != 201 || resp["releaseId"] != mine || resp["trackNumber"].(float64) != 3 {
		t.Fatalf("attach = %d %v", code, resp)
	}
	b["releaseId"] = theirs
	if code, resp = e.do("POST", "/uploads", tok, b); code != 404 {
		t.Fatalf("attach to someone else's release = %d %v, want 404", code, resp)
	}
	b["releaseId"] = "alb_doesnotexist"
	if code, _ = e.do("POST", "/uploads", tok, b); code != 404 {
		t.Fatalf("attach to unknown release = %d, want 404", code)
	}
	_ = otherTok
}

func TestCaps(t *testing.T) {
	e := setup(t)
	tok, userID, artistID := e.artistUser(testutil.GiB)
	ctx := context.Background()

	// Unfinished uploads are capped per artist.
	for i := 0; i < MaxActivePerArtist; i++ {
		if code, b := e.do("POST", "/uploads", tok, body(1)); code != 201 {
			t.Fatalf("upload %d = %d %v", i, code, b)
		}
	}
	if code, b := e.do("POST", "/uploads", tok, body(1)); code != 409 || errCode(b) != "limit_reached" {
		t.Fatalf("over the unfinished cap = %d %v, want 409 limit_reached", code, b)
	}

	// Unverified artists also have a daily cap; the verified badge lifts it.
	tok2, user2, artist2 := e.artistUser(testutil.GiB)
	rel := testutil.Release(t, e.pool, artist2, "R", "album", "draft")
	for i := 0; i < MaxDailyUnverified; i++ {
		if _, err := e.pool.Exec(ctx, `
			INSERT INTO uploads (user_id, artist_id, release_id, title, artist_name, filename, size_bytes, original_key, status)
			VALUES ($1,$2,$3,'t','x','f',1,'originals/cap-'||$4::text,'failed')`, user2, artist2, rel, strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	if code, b := e.do("POST", "/uploads", tok2, body(1)); code != 429 || errCode(b) != "daily_limit" {
		t.Fatalf("daily cap = %d %v, want 429 daily_limit", code, b)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE artists SET verification='verified' WHERE id=$1`, artist2); err != nil {
		t.Fatal(err)
	}
	if code, b := e.do("POST", "/uploads", tok2, body(1)); code != 201 {
		t.Fatalf("verified artist past the daily cap = %d %v, want 201", code, b)
	}
	_, _ = userID, artistID
}

func TestExpirePendingFreesStorage(t *testing.T) {
	e := setup(t)
	tok, _, artistID := e.artistUser(testutil.GiB)
	_, created := e.do("POST", "/uploads", tok, body(700))
	id := created["id"].(string)
	if _, err := e.pool.Exec(context.Background(), `UPDATE uploads SET created_at = now() - interval '5 hours' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	keys, err := e.store.ExpirePending(context.Background(), 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, k := range keys {
		found = found || k.Key == "originals/"+id
	}
	if !found {
		t.Fatalf("expired keys %v do not include the abandoned upload", keys)
	}
	if got := e.quota(artistID).UsedStorageBytes; got != 0 {
		t.Fatalf("used = %d, want 0 after expiry", got)
	}
	if _, b := e.do("GET", "/uploads/"+id, tok, nil); b["status"] != "failed" {
		t.Fatalf("status = %v, want failed", b["status"])
	}
}

func TestUnauthenticated(t *testing.T) {
	e := setup(t)
	if code, _ := e.do("GET", "/uploads", "garbage", nil); code != 401 {
		t.Fatalf("bad token = %d, want 401", code)
	}
}

func TestUploadGenreMustComeFromTheList(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.artistUser(testutil.GiB)

	b := body(100)
	b["genre"] = "Afrobeatz"
	if code, resp := e.do("POST", "/uploads", tok, b); code != 422 || errCode(resp) != "invalid_genre" {
		t.Fatalf("unknown genre = %d %v, want 422 invalid_genre", code, resp)
	}

	b["genre"] = "bongo flava"
	code, resp := e.do("POST", "/uploads", tok, b)
	if code != 201 || resp["genre"] != "Bongo Flava" {
		t.Fatalf("a listed genre in any spelling = %d %v, want 201 with the canonical name", code, resp)
	}
	// The draft single created for the track carries the canonical genre too.
	var g string
	if err := e.pool.QueryRow(context.Background(), `SELECT genre FROM albums WHERE id=$1`, resp["releaseId"]).Scan(&g); err != nil || g != "Bongo Flava" {
		t.Fatalf("release genre = %q (%v)", g, err)
	}

	delete(b, "genre")
	if code, _ := e.do("POST", "/uploads", tok, b); code != 201 {
		t.Fatalf("no genre = %d, want 201", code)
	}
}

func TestReplaceAudioOfADraftTrack(t *testing.T) {
	e := setup(t)
	tok, userID, artistID := e.artistUser(testutil.GiB)
	ctx := context.Background()
	rel := testutil.Release(t, e.pool, artistID, "Draft EP", "album", "draft")
	track, _ := testutil.ReadyTrack(t, e.pool, userID, artistID, rel, "Track One", 4000, "draft")
	if _, err := e.pool.Exec(ctx, `UPDATE tracks SET genre='Bongo Flava', track_number=3, explicit=true WHERE id=$1`, track); err != nil {
		t.Fatal(err)
	}
	e.blobs.Put("originals/"+track, []byte("old"))
	before := e.quota(artistID).UsedStorageBytes

	code, b := e.do("POST", "/tracks/"+track+"/replace-audio", tok, map[string]any{
		"filename": "better.flac", "sizeBytes": 1000, "rightsAccepted": true, "termsVersion": "2024-01",
	})
	if code != 201 {
		t.Fatalf("replace = %d %v", code, b)
	}
	if b["replacedTrackId"] != track || b["releaseId"] != rel || b["title"] != "Track One" || b["status"] != "pending" {
		t.Fatalf("response = %v", b)
	}
	if up, _ := b["upload"].(map[string]any); up == nil || up["url"] == "" {
		t.Fatalf("no upload target: %v", b)
	}
	// The old track is gone with its storage, the new upload carries the old metadata.
	var n int
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM tracks WHERE id=$1`, track).Scan(&n)
	if n != 0 {
		t.Fatal("the old track should be deleted")
	}
	if _, ok := e.blobs.Get("originals/" + track); ok {
		t.Fatal("the old original should be removed from storage")
	}
	var genre string
	var num *int
	var explicit bool
	if err := e.pool.QueryRow(ctx, `SELECT genre, track_number, explicit FROM uploads WHERE id=$1`, b["id"]).Scan(&genre, &num, &explicit); err != nil {
		t.Fatal(err)
	}
	if genre != "Bongo Flava" || num == nil || *num != 3 || !explicit {
		t.Fatalf("metadata not carried over: %q %v %v", genre, num, explicit)
	}
	if used := e.quota(artistID).UsedStorageBytes; used != 1000 {
		t.Fatalf("used storage = %d (was %d), want only the new file's 1000", used, before)
	}
}

func TestReplaceAudioKeepsTheOldTrackWhenTheNewFileIsRejected(t *testing.T) {
	e := setup(t)
	tok, userID, artistID := e.artistUser(testutil.GiB)
	rel := testutil.Release(t, e.pool, artistID, "Draft", "single", "draft")
	track, _ := testutil.ReadyTrack(t, e.pool, userID, artistID, rel, "Keep me", 4000, "draft")

	for name, payload := range map[string]map[string]any{
		"rights not accepted": {"filename": "a.flac", "sizeBytes": 10, "rightsAccepted": false, "termsVersion": "2024-01"},
		"empty":               {"filename": "a.flac", "sizeBytes": 0, "rightsAccepted": true, "termsVersion": "2024-01"},
		"too large":           {"filename": "a.flac", "sizeBytes": testMax + 1, "rightsAccepted": true, "termsVersion": "2024-01"},
	} {
		if code, b := e.do("POST", "/tracks/"+track+"/replace-audio", tok, payload); code != 422 {
			t.Fatalf("%s = %d %v, want 422", name, code, b)
		}
	}
	var n int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM tracks WHERE id=$1`, track).Scan(&n)
	if n != 1 {
		t.Fatal("a rejected replacement must leave the track in place")
	}
}

func TestReplaceAudioOnlyForOwnDraftTracks(t *testing.T) {
	e := setup(t)
	tok, userID, artistID := e.artistUser(testutil.GiB)
	other, _, _ := e.artistUser(testutil.GiB)
	draftRel := testutil.Release(t, e.pool, artistID, "Draft", "single", "draft")
	liveRel := testutil.Release(t, e.pool, artistID, "Live", "single", "public")
	draft, _ := testutil.ReadyTrack(t, e.pool, userID, artistID, draftRel, "D", 100, "draft")
	live, _ := testutil.ReadyTrack(t, e.pool, userID, artistID, liveRel, "L", 100, "public")
	payload := map[string]any{"filename": "a.flac", "sizeBytes": 10, "rightsAccepted": true, "termsVersion": "2024-01"}

	if code, b := e.do("POST", "/tracks/"+live+"/replace-audio", tok, payload); code != 409 || errCode(b) != "not_draft" {
		t.Fatalf("live track = %d %v, want 409 not_draft", code, b)
	}
	if code, b := e.do("POST", "/tracks/"+draft+"/replace-audio", other, payload); code != 404 {
		t.Fatalf("someone else's track = %d %v, want 404", code, b)
	}
	if code, _ := e.do("POST", "/tracks/trk_missing/replace-audio", tok, payload); code != 404 {
		t.Fatalf("unknown track = %d, want 404", code)
	}
}
