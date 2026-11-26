package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/blobtest"
	"github.com/blacmusic/backend/internal/release"
	"github.com/blacmusic/backend/internal/testutil"
)

type env struct {
	t     *testing.T
	pool  *pgxpool.Pool
	blobs *blobtest.Mem
	svc   *Service
	r     http.Handler
}

func setup(t *testing.T) *env {
	t.Helper()
	pool := testutil.Pool(t)
	blobs := blobtest.New()
	e := &env{t: t, pool: pool, blobs: blobs}
	e.svc = NewService(pool, release.NewStore(pool, blobs, "http://api.test"), blobs)
	r := chi.NewRouter()
	r.Group(func(g chi.Router) {
		g.Use(auth.Require(auth.NewTokenService(testutil.Secret, time.Minute)))
		g.Delete("/me", NewHandler(e.svc).Delete)
	})
	e.r = r
	return e
}

func (e *env) del(userID string, body any) (int, map[string]any) {
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest("DELETE", "/me", rdr)
	req.Header.Set("Authorization", "Bearer "+testutil.Token(e.t, userID))
	rec := httptest.NewRecorder()
	e.r.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) withPassword(userID, password string) {
	e.t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE users SET password_hash = $2 WHERE id = $1`, userID, hash); err != nil {
		e.t.Fatal(err)
	}
}

func errCode(b map[string]any) string {
	m, _ := b["error"].(map[string]any)
	s, _ := m["code"].(string)
	return s
}

func TestDeleteRemovesTheAccountAndItsData(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	user := testutil.User(t, e.pool, "user", true)
	e.withPassword(user, "supersecret1")
	artist := testutil.Artist(t, e.pool, user, testutil.Unique("art"), 1<<30)
	rel := testutil.Release(t, e.pool, artist, "Album", "album", "public")
	track, _ := testutil.ReadyTrack(t, e.pool, user, artist, rel, "Song", 1, "public")
	e.blobs.Put("tracks/"+track+"/medium.m4a", []byte("audio"))
	// an upload that never finished, with its file
	e.pool.Exec(ctx, `INSERT INTO uploads (user_id, artist_id, title, artist_name, filename, size_bytes, original_key, status)
		VALUES ($1, $2, 'half', 'x', 'f', 1, 'originals/half', 'pending')`, user, artist) //nolint:errcheck
	e.blobs.Put("originals/half", []byte("partial"))
	// a picture, a playlist, library entries, a YouTube connection, an identity
	var img string
	e.pool.QueryRow(ctx, `INSERT INTO images (id, owner_id, purpose, original_key, size_bytes, status) VALUES ('img_' || replace(gen_random_uuid()::text, '-', ''), $1, 'avatar', 'o', 1, 'ready') RETURNING id`, user).Scan(&img) //nolint:errcheck
	e.blobs.Put("images/"+img+"/medium.jpg", []byte("jpg"))
	e.pool.Exec(ctx, `INSERT INTO playlists (owner_id, name) VALUES ($1, 'mine')`, user)                                                                          //nolint:errcheck
	e.pool.Exec(ctx, `INSERT INTO youtube_connections (user_id, channel_id, channel_title, refresh_token_enc, scopes) VALUES ($1, 'UC', 'c', '\x00', 's')`, user) //nolint:errcheck
	e.pool.Exec(ctx, `INSERT INTO auth_identities (provider, subject, user_id) VALUES ('google', $1, $2)`, "sub-"+user, user)                                     //nolint:errcheck

	// someone else saved the track, put it in a playlist and follows the artist
	fan := testutil.User(t, e.pool, "user", true)
	var pl string
	e.pool.QueryRow(ctx, `INSERT INTO playlists (owner_id, name, is_public) VALUES ($1, 'fan list', true) RETURNING id`, fan).Scan(&pl) //nolint:errcheck
	e.pool.Exec(ctx, `INSERT INTO playlist_items (playlist_id, track_id, position) VALUES ($1, $2, 0)`, pl, track)                      //nolint:errcheck
	e.pool.Exec(ctx, `INSERT INTO library_tracks (user_id, track_id) VALUES ($1, $2)`, fan, track)                                      //nolint:errcheck
	e.pool.Exec(ctx, `INSERT INTO library_artists (user_id, artist_id) VALUES ($1, $2)`, fan, artist)                                   //nolint:errcheck

	// a password account must confirm with the password
	if st, b := e.del(user, nil); st != 403 || errCode(b) != "password_required" {
		t.Fatalf("no password = %d %v", st, b)
	}
	if st, b := e.del(user, map[string]string{"password": "wrong-password"}); st != 403 || errCode(b) != "invalid_password" {
		t.Fatalf("wrong password = %d %v", st, b)
	}
	if e.count(`SELECT count(*) FROM users WHERE id = $1`, user) != 1 {
		t.Fatal("a refused deletion removed the account")
	}
	if st, b := e.del(user, map[string]string{"password": "supersecret1"}); st != 204 {
		t.Fatalf("delete = %d %v", st, b)
	}

	for what, q := range map[string]string{
		"user":         `SELECT count(*) FROM users WHERE id = $1`,
		"artist":       `SELECT count(*) FROM artists WHERE owner_user_id = $1`,
		"releases":     `SELECT count(*) FROM albums WHERE artist_id = '` + artist + `' AND $1 <> ''`,
		"tracks":       `SELECT count(*) FROM tracks WHERE artist_id = '` + artist + `' AND $1 <> ''`,
		"uploads":      `SELECT count(*) FROM uploads WHERE user_id = $1`,
		"images":       `SELECT count(*) FROM images WHERE owner_id = $1`,
		"playlists":    `SELECT count(*) FROM playlists WHERE owner_id = $1`,
		"youtube":      `SELECT count(*) FROM youtube_connections WHERE user_id = $1`,
		"identities":   `SELECT count(*) FROM auth_identities WHERE user_id = $1`,
		"sessions":     `SELECT count(*) FROM refresh_tokens WHERE user_id = $1`,
		"others' list": `SELECT count(*) FROM playlist_items WHERE track_id = '` + track + `' AND $1 <> ''`,
		"others' lib":  `SELECT count(*) FROM library_tracks WHERE track_id = '` + track + `' AND $1 <> ''`,
		"followers":    `SELECT count(*) FROM library_artists WHERE artist_id = '` + artist + `' AND $1 <> ''`,
	} {
		if n := e.count(q, user); n != 0 {
			t.Errorf("%s: %d rows left", what, n)
		}
	}
	for _, k := range []string{"tracks/" + track + "/medium.m4a", "originals/half", "images/" + img + "/medium.jpg"} {
		if _, ok := e.blobs.Get(k); ok {
			t.Errorf("file %s is still in storage", k)
		}
	}
	// other people's own data stays
	if e.count(`SELECT count(*) FROM playlists WHERE id = $1`, pl) != 1 {
		t.Fatal("someone else's playlist was deleted")
	}
	// deleting again: the account is gone
	if st, _ := e.del(user, map[string]string{"password": "supersecret1"}); st != 401 {
		t.Fatalf("second delete = %d", st)
	}
}

func TestProviderOnlyAccountNeedsNoPassword(t *testing.T) {
	e := setup(t)
	user := testutil.User(t, e.pool, "user", true)
	e.pool.Exec(context.Background(), `UPDATE users SET password_hash = '!' WHERE id = $1`, user) //nolint:errcheck
	if st, b := e.del(user, nil); st != 204 {
		t.Fatalf("delete = %d %v", st, b)
	}
	if e.count(`SELECT count(*) FROM users WHERE id = $1`, user) != 0 {
		t.Fatal("account still exists")
	}
	// and a password sent anyway is not an error
	other := testutil.User(t, e.pool, "user", true)
	e.pool.Exec(context.Background(), `UPDATE users SET password_hash = '!' WHERE id = $1`, other) //nolint:errcheck
	if st, _ := e.del(other, map[string]string{"password": "whatever"}); st != 204 {
		t.Fatalf("delete with a stray password = %d", st)
	}
}

func TestDeleteWaitsForFilesBeingProcessed(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	user := testutil.User(t, e.pool, "user", true)
	e.pool.Exec(ctx, `UPDATE users SET password_hash = '!' WHERE id = $1`, user) //nolint:errcheck
	artist := testutil.Artist(t, e.pool, user, testutil.Unique("art"), 1<<30)
	rel := testutil.Release(t, e.pool, artist, "R", "single", "draft")
	e.pool.Exec(ctx, `INSERT INTO uploads (user_id, artist_id, release_id, title, artist_name, filename, size_bytes, original_key, status)
		VALUES ($1, $2, $3, 't', 'x', 'f', 1, 'originals/busy', 'processing')`, user, artist, rel) //nolint:errcheck
	if st, b := e.del(user, nil); st != 409 || errCode(b) != "busy" {
		t.Fatalf("while processing = %d %v", st, b)
	}
	if e.count(`SELECT count(*) FROM users WHERE id = $1`, user) != 1 {
		t.Fatal("account removed while a file was being processed")
	}
	// once the worker is done the same call succeeds (it is safe to repeat)
	e.pool.Exec(ctx, `UPDATE uploads SET status = 'failed' WHERE user_id = $1`, user) //nolint:errcheck
	if st, b := e.del(user, nil); st != 204 {
		t.Fatalf("after processing = %d %v", st, b)
	}
}

func TestBeforeHooksRunAndCannotBlock(t *testing.T) {
	e := setup(t)
	user := testutil.User(t, e.pool, "user", true)
	e.pool.Exec(context.Background(), `UPDATE users SET password_hash = '!' WHERE id = $1`, user) //nolint:errcheck
	var ran []string
	e.svc.Before = append(e.svc.Before,
		func(_ context.Context, id string) error { ran = append(ran, id); return nil },
		func(context.Context, string) error { return errors.New("Google is down") })
	if st, _ := e.del(user, nil); st != 204 || len(ran) != 1 || ran[0] != user {
		t.Fatalf("hooks: status %d, ran %v", st, ran)
	}
}
