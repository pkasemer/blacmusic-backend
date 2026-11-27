package importer_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/admin"
	"github.com/blacmusic/backend/internal/artist"
	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/blobtest"
	"github.com/blacmusic/backend/internal/importer"
	"github.com/blacmusic/backend/internal/playlist"
	"github.com/blacmusic/backend/internal/release"
	"github.com/blacmusic/backend/internal/testutil"
	"github.com/blacmusic/backend/internal/upload"
)

const part = 5000

type fixture struct {
	t       *testing.T
	pool    *pgxpool.Pool
	blobs   *blobtest.Mem
	r       http.Handler
	runner  *importer.Runner
	uploads *upload.Store
	admin   string
	adminT  string
	free    int64
}

func setup(t *testing.T) *fixture {
	t.Helper()
	pool := testutil.Pool(t)
	f := &fixture{t: t, pool: pool, blobs: blobtest.New(), uploads: upload.NewStore(pool), free: 100 << 30}
	tokens := auth.NewTokenService(testutil.Secret, time.Minute)
	h := importer.NewHandler(pool, f.blobs, tokens, importer.Options{
		MaxAudioBytes: 10 << 20, PartSize: part, URLTTL: time.Hour, MaxTotalBytes: 20 << 20, MinFreeBytes: 1 << 30,
		AdminSiteURL: "https://admin.test", FreeBytes: func() (int64, bool) { return f.free, true },
	})
	adminH := admin.NewHandler(pool, artist.NewStore(pool, testutil.GiB, "http://api.test"), playlist.NewStore(pool, "http://api.test"))
	adminH.APIKeys = h
	// mounted as the server does it
	r := chi.NewRouter()
	r.Route("/v1", func(v1 chi.Router) {
		v1.Route("/admin/imports", h.Routes)
		v1.Group(func(priv chi.Router) {
			priv.Use(auth.RequireWith(tokens, auth.NewBanCache(pool, time.Millisecond)))
			priv.Route("/admin", adminH.Routes)
		})
	})
	f.r = r
	f.runner = importer.NewRunner(pool, f.blobs, release.NewStore(pool, f.blobs, "http://api.test"), f.uploads)
	f.admin = testutil.User(t, pool, "admin", true)
	f.adminT = testutil.Token(t, f.admin)
	return f
}

// forget removes the catalog artists a test made (they have no owner, so nothing else would).
func (f *fixture) forget(nameLike string) {
	f.t.Cleanup(func() {
		c := context.Background()
		for _, q := range []string{
			`DELETE FROM uploads WHERE artist_id IN (SELECT id FROM artists WHERE name ILIKE $1)`,
			`DELETE FROM tracks WHERE artist_id IN (SELECT id FROM artists WHERE name ILIKE $1)`,
			`DELETE FROM albums WHERE artist_id IN (SELECT id FROM artists WHERE name ILIKE $1)`,
			`DELETE FROM artists WHERE name ILIKE $1`,
		} {
			if _, err := f.pool.Exec(c, q, "%"+nameLike+"%"); err != nil {
				f.t.Log(err)
			}
		}
	})
}

func (f *fixture) call(method, path, token, idem string, body any) (int, map[string]any) {
	f.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			f.t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, "/v1"+path, &buf)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	w := httptest.NewRecorder()
	f.r.ServeHTTP(w, req)
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func (f *fixture) one(sql string, args ...any) string {
	f.t.Helper()
	var s string
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&s); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
	return s
}

func errCode(b map[string]any) string {
	m, _ := b["error"].(map[string]any)
	s, _ := m["code"].(string)
	return s
}

func list(v any) []any { l, _ := v.([]any); return l }

func obj(v any) map[string]any { m, _ := v.(map[string]any); return m }

// send plays the caller: every file the answer asks for is put where its link says. skip names files to hold back.
func (f *fixture) send(jobID string, view map[string]any, data map[string][]byte, skip ...string) {
	f.t.Helper()
	for _, raw := range list(view["files"]) {
		file := obj(raw)
		name := file["name"].(string)
		held := false
		for _, s := range skip {
			held = held || s == name
		}
		if held || file["uploaded"] == true {
			continue
		}
		target := obj(file["upload"])
		var key, multipart string
		f.pool.QueryRow(context.Background(), `SELECT storage_key, multipart_id FROM import_files WHERE job_id = $1 AND name = $2`, jobID, name).Scan(&key, &multipart) //nolint:errcheck
		if target["type"] == "single" {
			f.blobs.Put(key, data[name])
			continue
		}
		size := int(target["partSize"].(float64))
		for _, p := range list(target["parts"]) {
			n := int(obj(p)["number"].(float64))
			f.blobs.PutPart(multipart, n, data[name][(n-1)*size:min(n*size, len(data[name]))])
		}
	}
}

// process plays the audio processor for this test's queued uploads; fail maps a title to what is wrong with its file.
func (f *fixture) process(fail map[string]string) int {
	f.t.Helper()
	ctx := context.Background()
	rows, err := f.pool.Query(ctx, `UPDATE uploads SET status = 'processing' WHERE user_id::text = $1 AND status = 'queued' RETURNING id`, f.admin)
	if err != nil {
		f.t.Fatal(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id) //nolint:errcheck
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		u, err := f.uploads.Get(ctx, id)
		if err != nil {
			f.t.Fatal(err)
		}
		if _, ok := f.blobs.Get(u.OriginalKey); !ok {
			f.t.Fatalf("%s: the audio is not at %s", u.Title, u.OriginalKey)
		}
		if msg := fail[u.Title]; msg != "" {
			if err := f.uploads.Fail(ctx, id, msg); err != nil {
				f.t.Fatal(err)
			}
			f.blobs.Remove(ctx, u.OriginalKey) //nolint:errcheck
			continue
		}
		res := upload.Result{DurationMs: 185400}
		for _, q := range []string{"low", "medium", "high"} {
			res.Renditions = append(res.Renditions, upload.Rendition{Quality: q, MediaKey: "tracks/" + u.TrackID + "/" + q + ".m4a", BitrateKbps: 128, SizeBytes: 1000})
		}
		if err := f.uploads.Finish(ctx, u, res); err != nil {
			f.t.Fatal(err)
		}
	}
	return len(ids)
}

// drive runs the runner and the processor until the job ends.
func (f *fixture) drive(jobID string, fail map[string]string) map[string]any {
	f.t.Helper()
	for range 40 {
		if err := f.runner.Step(context.Background()); err != nil {
			f.t.Fatal(err)
		}
		if n := f.process(fail); n > 2 {
			f.t.Fatalf("%d files of one import were queued at once, want at most 2", n)
		}
		_, v := f.call("GET", "/admin/imports/"+jobID, f.adminT, "", nil)
		if v["status"] != "processing" {
			return v
		}
	}
	f.t.Fatal("the import never ended")
	return nil
}

func picture(t *testing.T) []byte {
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 300, 300))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func manifest(artistName string, data map[string][]byte, tracks ...map[string]any) map[string]any {
	var files []map[string]any
	for name, b := range data {
		files = append(files, map[string]any{"name": name, "sizeBytes": len(b)})
	}
	return map[string]any{
		"artist": map[string]any{"name": artistName}, "defaults": map[string]any{"type": "single", "visibility": "public"},
		"tracks": tracks, "files": files, "rightsConfirmed": true, "rightsBasis": "permission from the artist",
	}
}

func TestAnImportKeyOnlyImports(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	code, b := f.call("POST", "/admin/api-keys", f.adminT, "", map[string]any{"name": "loader"})
	key, _ := b["key"].(string)
	id, _ := b["id"].(string)
	if code != 201 || !strings.HasPrefix(key, importer.KeyPrefix) || len(key) < 40 {
		t.Fatalf("create key = %d %v", code, b)
	}
	if stored := f.one(`SELECT token_hash || prefix FROM api_keys WHERE id = $1`, id); strings.Contains(stored, key) {
		t.Fatal("the key itself must not be stored")
	}
	code, b = f.call("GET", "/admin/api-keys", f.adminT, "", nil)
	if raw, _ := json.Marshal(b); code != 200 || strings.Contains(string(raw), key) {
		t.Fatalf("list = %d %s", code, raw)
	}
	for name, body := range map[string]map[string]any{"no name": {}, "too long": {"name": "x", "expiresInDays": 400}} {
		if code, _ = f.call("POST", "/admin/api-keys", f.adminT, "", body); code != 422 {
			t.Errorf("%s = %d", name, code)
		}
	}

	if code, _ = f.call("GET", "/admin/imports", key, "", nil); code != 200 {
		t.Fatalf("the key on the import routes = %d", code)
	}
	for _, path := range []string{"/admin/users", "/admin/artists", "/admin/api-keys", "/admin/audit-log", "/admin/stats"} {
		if code, _ = f.call("GET", path, key, "", nil); code != 401 {
			t.Errorf("a key on %s = %d, want 401", path, code)
		}
	}
	if code, _ = f.call("POST", "/admin/api-keys", key, "", map[string]any{"name": "more"}); code != 401 {
		t.Fatalf("a key making keys = %d", code)
	}
	user := testutil.Token(t, testutil.User(t, f.pool, "user", true))
	if code, _ = f.call("GET", "/admin/imports", user, "", nil); code != 403 {
		t.Fatalf("a normal user = %d", code)
	}
	for _, bad := range []string{"", "nonsense", importer.KeyPrefix + "wrong"} {
		if code, _ = f.call("GET", "/admin/imports", bad, "", nil); code != 401 {
			t.Errorf("token %q = %d", bad, code)
		}
	}

	// a demoted administrator's key stops working, and so do expired and revoked keys
	f.pool.Exec(ctx, `UPDATE users SET role = 'user' WHERE id = $1`, f.admin) //nolint:errcheck
	if code, _ = f.call("GET", "/admin/imports", key, "", nil); code != 401 {
		t.Fatalf("after demotion = %d", code)
	}
	f.pool.Exec(ctx, `UPDATE users SET role = 'admin' WHERE id = $1`, f.admin)                         //nolint:errcheck
	f.pool.Exec(ctx, `UPDATE api_keys SET expires_at = now() - interval '1 second' WHERE id = $1`, id) //nolint:errcheck
	if code, _ = f.call("GET", "/admin/imports", key, "", nil); code != 401 {
		t.Fatalf("expired = %d", code)
	}
	f.pool.Exec(ctx, `UPDATE api_keys SET expires_at = now() + interval '1 day' WHERE id = $1`, id) //nolint:errcheck
	if code, _ = f.call("DELETE", "/admin/api-keys/"+id, f.adminT, "", nil); code != 204 {
		t.Fatalf("revoke = %d", code)
	}
	if code, _ = f.call("GET", "/admin/imports", key, "", nil); code != 401 {
		t.Fatalf("revoked = %d", code)
	}
	if n := f.one(`SELECT count(*)::text FROM admin_audit_log WHERE target_id = $1`, id); n != "2" {
		t.Fatalf("audit rows for the key = %s, want 2", n)
	}
}

func TestImportingSingles(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	tag := testutil.Unique("Singles")
	f.forget(tag)
	name := "Judas " + tag
	_, k := f.call("POST", "/admin/api-keys", f.adminT, "", map[string]any{"name": "loader"})
	key := k["key"].(string)

	// the artist is already there without an account, with one song
	var artistID string
	if err := f.pool.QueryRow(ctx, `INSERT INTO artists (name) VALUES ($1) RETURNING id`, "  "+strings.ToUpper(name)+" ").Scan(&artistID); err != nil {
		t.Fatal(err)
	}
	old := testutil.Release(t, f.pool, artistID, "Old  Song", "single", "public")

	big := bytes.Repeat([]byte("a"), 2*part+123) // sent in 3 parts
	data := map[string][]byte{"one.mp3": big, "two.m4a": []byte("audio two"), "bad.mp3": []byte("not audio"), "cover.png": picture(t)}
	body := manifest(name, data,
		map[string]any{"title": "First", "genre": "rap", "audioFile": "one.mp3", "coverFile": "cover.png", "releaseDate": "2024-03-01", "explicit": true},
		map[string]any{"title": "Second", "genre": nil, "audioFile": "two.m4a", "coverFile": "cover.png"},
		map[string]any{"title": " old song ", "audioFile": "gone.mp3"},
		map[string]any{"title": "Broken", "audioFile": "bad.mp3"},
		map[string]any{"title": "second", "audioFile": "again.mp3"},
	)
	// files that only skipped songs use must be listed, but are never asked for
	body["files"] = append(body["files"].([]map[string]any), map[string]any{"name": "gone.mp3", "sizeBytes": 5}, map[string]any{"name": "again.mp3", "sizeBytes": 5})

	idem := testutil.Unique("idem")
	code, v := f.call("POST", "/admin/imports", key, idem, body)
	jobID, _ := v["jobId"].(string)
	if code != 201 || v["status"] != "pending" || v["totalTracks"] != float64(5) || v["skippedDuplicate"] != float64(2) || v["artistId"] != artistID || len(list(v["files"])) != 4 {
		t.Fatalf("create = %d %v", code, v)
	}
	if code, again := f.call("POST", "/admin/imports", key, idem, body); code != 200 || again["jobId"] != jobID || len(list(again["files"])) != 4 {
		t.Fatalf("the same key again = %d %v", code, again)
	}

	// nothing, then everything but one part
	if code, b := f.call("POST", "/admin/imports/"+jobID+"/start", key, "", nil); code != 409 || errCode(b) != "files_missing" || len(list(obj(obj(b["error"])["details"])["missing"])) != 4 {
		t.Fatalf("start without files = %d %v", code, b)
	}
	f.send(jobID, v, data, "one.mp3")
	var multipart string
	f.pool.QueryRow(ctx, `SELECT multipart_id FROM import_files WHERE job_id = $1 AND name = 'one.mp3'`, jobID).Scan(&multipart) //nolint:errcheck
	if multipart == "" {
		t.Fatal("a big file must be sent in parts")
	}
	f.blobs.PutPart(multipart, 1, big[:part])
	f.blobs.PutPart(multipart, 2, big[part:part+10]) // cut short
	code, v = f.call("POST", "/admin/imports/"+jobID+"/links", key, "", nil)
	var missingParts []any
	for _, raw := range list(v["files"]) {
		if obj(raw)["name"] == "one.mp3" {
			missingParts = list(obj(obj(raw)["upload"])["parts"])
		} else if obj(raw)["uploaded"] != true {
			t.Fatalf("%v should count as uploaded", obj(raw)["name"])
		}
	}
	if code != 200 || len(missingParts) != 2 {
		t.Fatalf("links = %d, parts still wanted %v", code, missingParts)
	}
	f.send(jobID, v, data)
	if code, v = f.call("POST", "/admin/imports/"+jobID+"/start", key, "", nil); code != 202 || v["status"] != "processing" {
		t.Fatalf("start = %d %v", code, v)
	}
	if code, _ = f.call("DELETE", "/admin/imports/"+jobID, key, "", nil); code != 409 {
		t.Fatalf("cancel after start = %d", code)
	}
	var mails string
	mails = f.one(`SELECT count(*)::text FROM email_messages`)

	v = f.drive(jobID, map[string]string{"Broken": "unsupported file type (use mp3, m4a/aac, flac, wav, ogg/opus or aiff)"})
	if v["status"] != "completed_with_errors" || v["created"] != float64(2) || v["skippedDuplicate"] != float64(2) || v["failed"] != float64(1) || v["remaining"] != float64(0) {
		t.Fatalf("end = %v", v)
	}
	results := list(v["results"])
	first, third, broken := obj(results[0]), obj(results[2]), obj(results[3])
	if first["status"] != "created" || first["duration"] != "3:05" || first["releaseUrl"] != "https://admin.test/releases/"+first["releaseId"].(string) {
		t.Fatalf("first = %v", first)
	}
	if third["status"] != "skippedDuplicate" || third["releaseId"] != old {
		t.Fatalf("duplicate = %v", third)
	}
	if broken["status"] != "failed" || broken["releaseId"] != nil || obj(list(v["errors"])[0])["code"] != "unsupported_audio" {
		t.Fatalf("broken = %v %v", broken, v["errors"])
	}

	rel := first["releaseId"].(string)
	if got := f.one(`SELECT al.visibility || '/' || al.release_date || '/' || al.genre || '/' || al.release_type || '/' || t.visibility || '/' || t.explicit
		FROM albums al JOIN tracks t ON t.album_id = al.id WHERE al.id = $1`, rel); got != "public/2024-03-01/Rap/single/public/true" {
		t.Fatalf("first release = %s", got)
	}
	second := obj(results[1])["releaseId"].(string)
	if got := f.one(`SELECT (release_date = (now() AT TIME ZONE 'Africa/Kampala')::date)::text || '/' || genre FROM albums WHERE id = $1`, second); got != "true/" {
		t.Fatalf("second release = %s", got)
	}
	if f.one(`SELECT cover_image_id FROM albums WHERE id = $1`, rel) != f.one(`SELECT cover_image_id FROM albums WHERE id = $1`, second) {
		t.Fatal("one cover file is stored once")
	}
	if n := f.one(`SELECT count(*)::text FROM albums WHERE artist_id = $1`, artistID); n != "3" {
		t.Fatalf("releases = %s, want the old one and two new ones (no empty draft left)", n)
	}
	for file, want := range map[string]bool{"one.mp3": true, "two.m4a": true, "bad.mp3": false, "cover.png": false} {
		if _, there := f.blobs.Get(f.one(`SELECT storage_key FROM import_files WHERE job_id = $1 AND name = $2`, jobID, file)); there != want {
			t.Errorf("staged %s kept = %v, want %v", file, there, want)
		}
	}
	if got := f.one(`SELECT count(*)::text FROM email_messages`); got != mails {
		t.Fatalf("an import must not send mail (%s -> %s)", mails, got)
	}
	if n := f.one(`SELECT count(*)::text FROM admin_audit_log WHERE action IN ('release.create', 'release.publish') AND details->>'importId' = $1`, jobID); n != "5" {
		t.Fatalf("audit rows = %s, want 3 creations and 2 publications", n)
	}
	if v["bytesStored"].(float64) != float64(len(big)+len(data["two.m4a"])+6000) {
		t.Fatalf("bytes stored = %v", v["bytesStored"])
	}

	// sending the catalogue again only brings the song that failed
	code, v = f.call("POST", "/admin/imports", key, testutil.Unique("idem"), body)
	if code != 201 || v["skippedDuplicate"] != float64(4) || len(list(v["files"])) != 1 || obj(list(v["files"])[0])["name"] != "bad.mp3" {
		t.Fatalf("second run = %d %v", code, v)
	}
	// a key sees only its administrator's jobs
	other := testutil.User(t, f.pool, "admin", true)
	_, ok := f.call("POST", "/admin/api-keys", testutil.Token(t, other), "", map[string]any{"name": "other"})
	if code, _ = f.call("GET", "/admin/imports/"+jobID, ok["key"].(string), "", nil); code != 404 {
		t.Fatalf("another administrator's key = %d", code)
	}
}

func TestImportingANewArtistAndAnAlbum(t *testing.T) {
	f := setup(t)
	tag := testutil.Unique("Album")
	f.forget(tag)
	name := "lubisky " + tag
	data := map[string][]byte{"1.mp3": []byte("a1"), "2.mp3": []byte("a2"), "me.png": picture(t), "front.png": []byte("not a picture")}
	body := manifest(name, data,
		map[string]any{"title": "Intro", "audioFile": "1.mp3"},
		map[string]any{"title": "Outro", "audioFile": "2.mp3"})
	body["artist"] = map[string]any{"name": name, "bio": "from Gulu", "pictureFile": "me.png"}
	body["defaults"] = map[string]any{"type": "album", "visibility": "public", "genre": "afrobeat", "releaseDate": "2025-01-31"}
	body["album"] = map[string]any{"title": "Home " + tag, "coverFile": "front.png"}

	// a bad cover fails the songs, leaves no album behind, and the artist stays
	code, v := f.call("POST", "/admin/imports", f.adminT, testutil.Unique("idem"), body)
	if code != 201 || v["artistId"] != nil {
		t.Fatalf("create = %d %v", code, v)
	}
	jobID := v["jobId"].(string)
	f.send(jobID, v, data)
	f.call("POST", "/admin/imports/"+jobID+"/start", f.adminT, "", nil)
	v = f.drive(jobID, nil)
	if v["status"] != "failed" || v["failed"] != float64(2) || obj(list(v["errors"])[0])["code"] != "invalid_cover" {
		t.Fatalf("bad cover = %v", v)
	}
	artistID := v["artistId"].(string)
	if got := f.one(`SELECT name || '/' || bio || '/' || (owner_user_id IS NULL)::text || '/' || (avatar_image_id IS NOT NULL)::text FROM artists WHERE id = $1`, artistID); got != name+"/from Gulu/true/true" {
		t.Fatalf("artist = %s", got)
	}
	if n := f.one(`SELECT count(*)::text FROM albums WHERE artist_id = $1`, artistID); n != "0" {
		t.Fatalf("empty album left behind (%s)", n)
	}

	// with a good cover, but one broken song: the album stays a draft
	data["front.png"] = picture(t)
	run := func(fail map[string]string) map[string]any {
		m := manifest(name, data, map[string]any{"title": "Intro", "audioFile": "1.mp3"}, map[string]any{"title": "Outro", "audioFile": "2.mp3"})
		m["defaults"], m["album"] = body["defaults"], body["album"]
		code, v := f.call("POST", "/admin/imports", f.adminT, testutil.Unique("idem"), m)
		if code != 201 || v["artistId"] != artistID {
			t.Fatalf("create = %d %v", code, v)
		}
		id := v["jobId"].(string)
		f.send(id, v, data)
		if code, v = f.call("POST", "/admin/imports/"+id+"/start", f.adminT, "", nil); code != 202 {
			t.Fatalf("start = %d %v", code, v)
		}
		return f.drive(id, fail)
	}
	v = run(map[string]string{"Outro": "could not read this file as audio"})
	if v["status"] != "completed_with_errors" || v["created"] != float64(1) || obj(list(v["errors"])[0])["code"] != "corrupt_audio" {
		t.Fatalf("one broken = %v", v)
	}
	album := obj(list(v["results"])[0])["releaseId"].(string)
	if got := f.one(`SELECT visibility FROM albums WHERE id = $1`, album); got != "draft" {
		t.Fatalf("an album with a missing song is %s", got)
	}
	if n := f.one(`SELECT count(*)::text FROM uploads WHERE release_id = $1`, album); n != "1" {
		t.Fatalf("uploads on the album = %s, the failed one should be gone", n)
	}

	// all good: published with both songs in order
	f.pool.Exec(context.Background(), `DELETE FROM uploads WHERE release_id = $1`, album) //nolint:errcheck
	f.pool.Exec(context.Background(), `DELETE FROM tracks WHERE album_id = $1`, album)    //nolint:errcheck
	f.pool.Exec(context.Background(), `DELETE FROM albums WHERE id = $1`, album)          //nolint:errcheck
	v = run(nil)
	if v["status"] != "completed" || v["created"] != float64(2) {
		t.Fatalf("all good = %v", v)
	}
	album = obj(list(v["results"])[0])["releaseId"].(string)
	if got := f.one(`SELECT visibility || '/' || release_type || '/' || genre || '/' || release_date || '/' ||
		(SELECT string_agg(title || ':' || track_number || ':' || visibility, ',' ORDER BY track_number) FROM tracks WHERE album_id = al.id)
		FROM albums al WHERE id = $1`, album); got != "public/album/Afrobeat/2025-01-31/Intro:1:public,Outro:2:public" {
		t.Fatalf("album = %s", got)
	}
	// the same album again is skipped whole, without files
	m := manifest(name, data, map[string]any{"title": "Intro", "audioFile": "1.mp3"}, map[string]any{"title": "Outro", "audioFile": "2.mp3"})
	m["defaults"], m["album"] = body["defaults"], body["album"]
	if code, v = f.call("POST", "/admin/imports", f.adminT, testutil.Unique("idem"), m); code != 201 || v["status"] != "completed" || v["skippedDuplicate"] != float64(2) || v["files"] != nil {
		t.Fatalf("again = %d %v", code, v)
	}
}

func TestImportRefusalsAndCancelling(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	tag := testutil.Unique("Refuse")
	f.forget(tag)
	data := map[string][]byte{"a.mp3": []byte("a"), "big.mp3": bytes.Repeat([]byte("b"), 3*part)}
	good := func(name string) map[string]any {
		return manifest(name, data, map[string]any{"title": "A", "audioFile": "a.mp3"}, map[string]any{"title": "B", "audioFile": "big.mp3"})
	}
	post := func(body map[string]any) (int, map[string]any) {
		return f.call("POST", "/admin/imports", f.adminT, testutil.Unique("idem"), body)
	}
	if code, b := f.call("POST", "/admin/imports", f.adminT, "", good("N "+tag)); code != 400 || errCode(b) != "idempotency_key_required" {
		t.Fatalf("without a key = %d %v", code, b)
	}
	// all problems come back together
	bad := manifest("N "+tag, data,
		map[string]any{"title": "", "audioFile": "a.mp3", "genre": "nope"},
		map[string]any{"title": "B", "audioFile": "missing.mp3", "releaseDate": "soon"})
	bad["rightsConfirmed"], bad["rightsBasis"] = false, ""
	bad["defaults"] = map[string]any{"visibility": "unlisted"}
	code, b := post(bad)
	got := map[string]bool{}
	for _, p := range list(obj(obj(b["error"])["details"])["problems"]) {
		got[obj(p)["code"].(string)] = true
	}
	for _, want := range []string{"rights_not_accepted", "rights_basis_required", "invalid_visibility", "invalid_title", "invalid_genre", "missing_file", "invalid_date"} {
		if code != 422 || !got[want] {
			t.Errorf("%d: problem %s missing in %v", code, want, got)
		}
	}
	huge := good("N " + tag)
	huge["files"] = []map[string]any{{"name": "a.mp3", "sizeBytes": 9 << 20}, {"name": "big.mp3", "sizeBytes": 10 << 20}, {"name": "x", "sizeBytes": 1}}
	huge["tracks"] = append(huge["tracks"].([]map[string]any), map[string]any{"title": "C", "audioFile": "c.mp3"})
	huge["files"] = append(huge["files"].([]map[string]any), map[string]any{"name": "c.mp3", "sizeBytes": 5 << 20})
	if code, b = post(huge); code != 422 || errCode(b) != "import_too_large" {
		t.Fatalf("too large = %d %v", code, b)
	}
	f.free = 1 << 30
	if code, b = post(good("N " + tag)); code != 507 || errCode(b) != "insufficient_storage" {
		t.Fatalf("full disk = %d %v", code, b)
	}
	f.free = 100 << 30

	// an artist with an account is only used when named by id; a banned one never
	owner := testutil.User(t, f.pool, "user", true)
	owned := testutil.Artist(t, f.pool, owner, "Owned "+tag, testutil.GiB)
	if code, b = post(good("owned " + tag)); code != 409 || errCode(b) != "artist_owned" {
		t.Fatalf("owned by name = %d %v", code, b)
	}
	byID := good("")
	byID["artist"] = map[string]any{"artistId": owned}
	if code, b = post(byID); code != 201 || b["artist"] != "Owned "+tag || b["artistId"] != owned {
		t.Fatalf("owned by id = %d %v", code, b)
	}
	f.pool.Exec(ctx, `UPDATE artists SET banned_at = now() WHERE id = $1`, owned) //nolint:errcheck
	if code, b = post(byID); code != 409 || errCode(b) != "artist_banned" {
		t.Fatalf("banned = %d %v", code, b)
	}
	byID["artist"] = map[string]any{"artistId": "art_nope"}
	if code, b = post(byID); code != 404 || errCode(b) != "artist_not_found" {
		t.Fatalf("unknown id = %d %v", code, b)
	}

	// cancelling, and expiry, remove what was staged
	for _, how := range []string{"cancel", "expire"} {
		code, v := post(good("C " + how + " " + tag))
		if code != 201 {
			t.Fatalf("create = %d %v", code, v)
		}
		id := v["jobId"].(string)
		f.send(id, v, data, "big.mp3")
		key := f.one(`SELECT storage_key FROM import_files WHERE job_id = $1 AND name = 'a.mp3'`, id)
		open := f.blobs.OpenUploads()
		if how == "cancel" {
			if code, _ = f.call("DELETE", "/admin/imports/"+id, f.adminT, "", nil); code != 204 {
				t.Fatalf("cancel = %d", code)
			}
		} else {
			f.pool.Exec(ctx, `UPDATE import_jobs SET created_at = now() - interval '25 hours' WHERE id = $1`, id) //nolint:errcheck
			if err := f.runner.Step(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if _, there := f.blobs.Get(key); there || f.blobs.OpenUploads() != open-1 {
			t.Fatalf("%s left files behind", how)
		}
		if _, v = f.call("GET", "/admin/imports/"+id, f.adminT, "", nil); v["status"] != "cancelled" {
			t.Fatalf("%s: %v", how, v["status"])
		}
		if code, _ = f.call("POST", "/admin/imports/"+id+"/links", f.adminT, "", nil); code != 409 {
			t.Fatalf("links after %s = %d", how, code)
		}
	}

	// the artist is banned between the start and the work: the job ends with the reason
	code, v := post(good("Late " + tag))
	if code != 201 {
		t.Fatalf("create = %d %v", code, v)
	}
	id := v["jobId"].(string)
	f.send(id, v, data)
	f.call("POST", "/admin/imports/"+id+"/start", f.adminT, "", nil)
	f.pool.Exec(ctx, `INSERT INTO artists (name, banned_at) VALUES ($1, now())`, "Late "+tag) //nolint:errcheck
	v = f.drive(id, nil)
	if v["status"] != "failed" || obj(list(v["errors"])[0])["code"] != "artist_banned" {
		t.Fatalf("banned meanwhile = %v", v)
	}
}

func TestListingUnfinishedImports(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	// 25 jobs: more than the plain list shows. Started jobs get a start time out of step with their creation.
	insert := func(owner, status string, created, started int) (id string) {
		t.Helper()
		if err := f.pool.QueryRow(ctx, `INSERT INTO import_jobs (created_by, idempotency_key, artist_name, release_type, publish, rights_basis, status, created_at, started_at)
			VALUES ($1::uuid, $2, 'A', 'single', 'draft', 'test jobs', $3, now() - make_interval(mins => $4),
			        CASE WHEN $5::int > 0 THEN now() - make_interval(mins => $5) END) RETURNING id`,
			owner, testutil.Unique("list"), status, created, started).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	// the oldest-created jobs are the ones being worked on: the plain list would cut them off
	servedSecond := insert(f.admin, "processing", 900, 10)
	servedFirst := insert(f.admin, "processing", 800, 50)
	waitingOld := insert(f.admin, "awaiting_files", 700, 0)
	waitingNew := insert(f.admin, "awaiting_files", 600, 0)
	var finished []string
	for i := range 21 {
		finished = append(finished, insert(f.admin, []string{"completed", "completed_with_errors", "failed", "cancelled"}[i%4], 100-i, 90-i))
	}
	other := testutil.User(t, f.pool, "admin", true)
	theirs := insert(other, "processing", 5, 1)

	ids := func(token, path string) (int, []string, []any) {
		code, b := f.call("GET", path, token, "", nil)
		var out []string
		for _, raw := range list(b["imports"]) {
			out = append(out, obj(raw)["jobId"].(string))
		}
		return code, out, list(b["imports"])
	}
	// jobs of other tests may be around: look only at ours
	ours := map[string]bool{servedFirst: true, servedSecond: true, waitingOld: true, waitingNew: true, theirs: true}
	for _, id := range finished {
		ours[id] = true
	}
	only := func(all []string) (out []string) {
		for _, id := range all {
			if ours[id] {
				out = append(out, id)
			}
		}
		return out
	}
	_, plain, _ := ids(f.adminT, "/admin/imports")
	if len(plain) != 20 || strings.Contains(strings.Join(plain, " "), servedFirst) {
		t.Fatalf("the plain list = %d jobs", len(plain))
	}
	code, got, rows := ids(f.adminT, "/admin/imports?state=unfinished")
	if want := []string{servedFirst, servedSecond, theirs, waitingOld, waitingNew}; code != 200 || strings.Join(only(got), " ") != strings.Join(want, " ") {
		t.Fatalf("unfinished = %d %v, want %v", code, only(got), want)
	}
	for _, raw := range rows {
		row := obj(raw)
		if (row["status"] != "pending" && row["status"] != "processing") || row["results"] != nil {
			t.Fatalf("row = %v", row)
		}
	}
	// a key sees only its administrator's jobs
	_, k := f.call("POST", "/admin/api-keys", f.adminT, "", map[string]any{"name": "lister"})
	_, got, _ = ids(k["key"].(string), "/admin/imports?state=unfinished")
	if want := []string{servedFirst, servedSecond, waitingOld, waitingNew}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("with a key = %v, want %v", got, want)
	}
	if code, b := f.call("GET", "/admin/imports?state=nonsense", f.adminT, "", nil); code != 400 {
		t.Fatalf("an unknown state = %d %v", code, b)
	}
}

func TestTheDiskCheckCountsEveryUnfinishedImport(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `DELETE FROM import_jobs WHERE status IN ('awaiting_files', 'processing')`); err != nil {
		t.Fatal(err)
	}
	tag := testutil.Unique("Space")
	f.forget(tag)
	const margin = int64(1 << 30) // what the handler keeps free in these tests
	const song = 4000             // each job: one song of this many bytes, 14000 on disk once done
	data := map[string][]byte{"a.mp3": bytes.Repeat([]byte("a"), song)}
	body := func(n string) map[string]any {
		return manifest(n+" "+tag, data, map[string]any{"title": "T " + n, "audioFile": "a.mp3"})
	}
	post := func(n string) (int, map[string]any) {
		return f.call("POST", "/admin/imports", f.adminT, testutil.Unique("space"), body(n))
	}
	details := func(b map[string]any) map[string]any { return obj(obj(b["error"])["details"]) }

	// room for two whole songs: each alone fits many times over, the third does not fit beside the two waiting
	f.free = margin + 2*14000 + 100
	idem := testutil.Unique("space")
	code, a := f.call("POST", "/admin/imports", f.adminT, idem, body("A"))
	code2, b := post("B")
	if code != 201 || code2 != 201 {
		t.Fatalf("two that fit = %d %d %v %v", code, code2, a, b)
	}
	code, c := post("C")
	if code != 507 || errCode(c) != "insufficient_storage" || details(c)["reservedBytes"] != float64(2*14000) || details(c)["needBytes"] != float64(14000) {
		t.Fatalf("one too many = %d %v", code, c)
	}
	idA, idB := a["jobId"].(string), b["jobId"].(string)
	// cancelling a waiting job gives its place back
	if code, _ = f.call("DELETE", "/admin/imports/"+idB, f.adminT, "", nil); code != 204 {
		t.Fatalf("cancel = %d", code)
	}
	code, b = post("B again")
	if code != 201 {
		t.Fatalf("after a cancel = %d %v", code, b)
	}
	idB = b["jobId"].(string)

	// jobs that passed earlier, when there was more room (as under the old check), are checked again.
	// Too little even to send the file: no new links, by either way of asking
	f.free = margin + song - 1
	for name, ask := range map[string]func() (int, map[string]any){
		"links":          func() (int, map[string]any) { return f.call("POST", "/admin/imports/"+idA+"/links", f.adminT, "", nil) },
		"same key again": func() (int, map[string]any) { return f.call("POST", "/admin/imports", f.adminT, idem, body("A")) },
	} {
		if code, got := ask(); code != 507 || errCode(got) != "insufficient_storage" {
			t.Errorf("%s without room to send = %d %v", name, code, got)
		}
	}
	// room to send: links again, whatever else is waiting
	f.free = margin + song
	if code, got := f.call("POST", "/admin/imports/"+idA+"/links", f.adminT, "", nil); code != 200 {
		t.Fatalf("links with room = %d %v", code, got)
	}
	f.send(idA, a, data)
	f.send(idB, b, data)

	// room for one job's playback copies (10000), not two: the waiting one does not hold back the first start,
	// the started one does hold back the second
	f.free = margin + 15000
	if code, got := f.call("POST", "/admin/imports/"+idA+"/start", f.adminT, "", nil); code != 202 {
		t.Fatalf("first start = %d %v", code, got)
	}
	code, got := f.call("POST", "/admin/imports/"+idB+"/start", f.adminT, "", nil)
	if code != 507 || details(got)["reservedBytes"] != float64(10000) || details(got)["needBytes"] != float64(10000) {
		t.Fatalf("second start = %d %v", code, got)
	}
	if _, v := f.call("GET", "/admin/imports/"+idB, f.adminT, "", nil); v["status"] != "pending" {
		t.Fatalf("a refused start leaves the job waiting: %v", v["status"])
	}
	// with nothing left to send, links still answer, so the caller can see the job is complete
	f.free = margin
	if code, got := f.call("POST", "/admin/imports/"+idB+"/links", f.adminT, "", nil); code != 200 {
		t.Fatalf("links with everything sent = %d %v", code, got)
	}
	// when the first is done it no longer reserves anything
	f.free = margin + 15000
	if v := f.drive(idA, nil); v["status"] != "completed" {
		t.Fatalf("first job = %v", v)
	}
	if code, got = f.call("POST", "/admin/imports/"+idB+"/start", f.adminT, "", nil); code != 202 {
		t.Fatalf("second start after the first finished = %d %v", code, got)
	}
	f.drive(idB, nil)

	// several creations at the same moment cannot share the same free space
	f.free = margin + 14000 + 100
	codes := make(chan int, 6)
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, _ := post("Race " + strconv.Itoa(i))
			codes <- code
		}()
	}
	wg.Wait()
	close(codes)
	count := map[int]int{}
	for code := range codes {
		count[code]++
	}
	if count[201] != 1 || count[507] != 5 {
		t.Fatalf("six at once with room for one = %v", count)
	}
}

func TestPagingThroughFinishedImports(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	insert := func(owner, status string, created, ended int) (id string) {
		t.Helper()
		if err := f.pool.QueryRow(ctx, `INSERT INTO import_jobs (created_by, idempotency_key, artist_name, release_type, publish, rights_basis, status, created_at, finished_at)
			VALUES ($1::uuid, $2, 'A', 'single', 'draft', 'test jobs', $3, now() - make_interval(mins => $4),
			        CASE WHEN $5::int >= 0 THEN now() - make_interval(mins => $5) END) RETURNING id`,
			owner, testutil.Unique("done"), status, created, ended).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	// a key's list holds only its administrator's jobs, so with a key the counts are exact whatever else is in the table
	_, k := f.call("POST", "/admin/api-keys", f.adminT, "", map[string]any{"name": "pager"})
	key := k["key"].(string)

	// 23 ended jobs, made in the opposite order to the one they ended in; two ended at the same moment
	statuses := []string{"completed", "completed_with_errors", "failed", "cancelled"}
	var want []string // latest ending first
	for i := range 21 {
		want = append(want, insert(f.admin, statuses[i%4], 500-i, 10+i))
	}
	tieA, tieB := insert(f.admin, "completed", 100, 200), ""
	if err := f.pool.QueryRow(ctx, `INSERT INTO import_jobs (created_by, idempotency_key, artist_name, release_type, publish, rights_basis, status, finished_at)
		SELECT created_by, $2, artist_name, release_type, publish, rights_basis, status, finished_at FROM import_jobs WHERE id = $1 RETURNING id`,
		tieA, testutil.Unique("tie")).Scan(&tieB); err != nil {
		t.Fatal(err)
	}
	if tieB > tieA {
		tieA, tieB = tieB, tieA
	}
	want = append(want, tieA, tieB)
	// newer jobs that are not finished must not hide them
	for i := range 25 {
		insert(f.admin, []string{"processing", "awaiting_files"}[i%2], 1, -1)
	}
	other := testutil.User(t, f.pool, "admin", true)
	theirs := insert(other, "completed", 1, 0)

	get := func(token, query string) (int, []string, float64, []any) {
		code, b := f.call("GET", "/admin/imports?"+query, token, "", nil)
		var ids []string
		for _, raw := range list(b["imports"]) {
			ids = append(ids, obj(raw)["jobId"].(string))
		}
		total, _ := b["total"].(float64)
		return code, ids, total, list(b["imports"])
	}
	if _, plain, _, _ := get(key, ""); len(plain) != 20 {
		t.Fatalf("plain = %d", len(plain))
	} else if _, b := f.call("GET", "/admin/imports", key, "", nil); b["total"] != nil {
		t.Fatal("the plain list has no total")
	}
	var paged []string
	for offset := 0; offset < 30; offset += 10 {
		code, ids, total, rows := get(key, "state=finished&limit=10&offset="+strconv.Itoa(offset))
		if code != 200 || total != 23 {
			t.Fatalf("page at %d = %d, total %v", offset, code, total)
		}
		for _, raw := range rows {
			row := obj(raw)
			if row["status"] == "pending" || row["status"] == "processing" || row["results"] != nil || row["finishedAt"] == nil {
				t.Fatalf("row = %v", row)
			}
		}
		paged = append(paged, ids...)
	}
	if strings.Join(paged, " ") != strings.Join(want, " ") {
		t.Fatalf("paged =\n%v\nwant\n%v", paged, want)
	}
	if _, ids, _, _ := get(key, "state=finished"); len(ids) != 20 {
		t.Fatalf("the default page = %d", len(ids))
	}
	if _, ids, total, rows := get(key, "state=finished&offset=500"); len(ids) != 0 || total != 23 || rows == nil {
		t.Fatalf("past the end = %v %v", ids, total)
	}
	// a signed-in administrator sees everybody's, the latest first
	if _, ids, total, _ := get(f.adminT, "state=finished&limit=100"); total < 24 || !slices.Contains(ids, theirs) {
		t.Fatalf("administrator: total %v", total)
	}
	for _, bad := range []string{"limit=0", "limit=101", "limit=x", "offset=-1", "offset=y"} {
		if code, _, _, _ := get(key, "state=finished&"+bad); code != 400 {
			t.Errorf("%s = %d, want 400", bad, code)
		}
	}
}
