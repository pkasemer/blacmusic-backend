package lyrics

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/testutil"
	"github.com/blacmusic/backend/internal/trackview"
)

func TestParseLRC(t *testing.T) {
	src := "[ti:My Song]\n[ar:Someone]\n[offset:+500]\r\n" +
		"[00:12.34]First line\n" +
		"[01:05.5][00:40]Chorus <00:41.00>words\n" +
		"[00:20.123]  \n" + // an instrumental gap, kept
		"no time stamp here\n" +
		"[100:00.00]Very late\n" +
		"[200:00.00]\n" // trailing empty line, dropped
	lines, err := ParseLRC(src)
	if err != nil {
		t.Fatal(err)
	}
	want := []Line{{11840, "First line"}, {19623, ""}, {39500, "Chorus words"}, {65000, "Chorus words"}, {5999500, "Very late"}}
	if len(lines) != len(want) {
		t.Fatalf("lines = %+v, want %+v", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("line %d = %+v, want %+v", i, lines[i], want[i])
		}
	}
	for name, bad := range map[string]string{
		"nothing timed": "just words\nmore words",
		"empty":         "",
		"seconds":       "[00:75.00]x",
		"long line":     "[00:01.00]" + strings.Repeat("x", MaxLineRunes+1),
		"only tags":     "[ti:Song]\n[ar:Me]",
	} {
		if _, err := ParseLRC(bad); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	var many strings.Builder
	for i := 0; i <= MaxLines; i++ {
		many.WriteString("[00:01.00]x\n")
	}
	if _, err := ParseLRC(many.String()); err == nil {
		t.Error("too many lines accepted")
	}
}

func TestBuildPlainAndTimed(t *testing.T) {
	s := func(v string) *string { return &v }
	l, err := Build(Input{Text: s("line one  \r\nline two\n\n\nline four\n\n")})
	if err != nil || l.Synced || l.Text != "line one\nline two\n\n\nline four" || l.Lines == nil || len(l.Lines) != 0 {
		t.Fatalf("plain = %+v %v", l, err)
	}
	l, err = Build(Input{Text: s("ignored"), LRC: s("[00:01.00]hello\n[00:02.00]world")})
	if err != nil || !l.Synced || l.Text != "hello\nworld" || len(l.Lines) != 2 {
		t.Fatalf("timed = %+v %v (lrc must win over text)", l, err)
	}
	for name, in := range map[string]Input{
		"nothing":   {},
		"blank":     {Text: s("  \n ")},
		"too long":  {Text: s(strings.Repeat("a", MaxTextRunes+1))},
		"bad lrc":   {LRC: s("not lrc")},
		"blank lrc": {LRC: s("  ")},
	} {
		if _, err := Build(in); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestLyricsEndToEnd(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()
	r := chi.NewRouter()
	r.Group(func(g chi.Router) {
		g.Use(auth.Require(auth.NewTokenService(testutil.Secret, time.Minute)))
		NewHandler(NewStore(pool)).Routes(g)
	})
	call := func(method, path, token string, body any) (int, map[string]any) {
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
		r.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	owner := testutil.User(t, pool, "user", true)
	ownerTok := testutil.Token(t, owner)
	artist := testutil.Artist(t, pool, owner, testutil.Unique("Lyric"), testutil.GiB)
	draftRel := testutil.Release(t, pool, artist, "Draft", "single", "draft")
	pubRel := testutil.Release(t, pool, artist, "Live", "single", "public")
	draft, _ := testutil.ReadyTrack(t, pool, owner, artist, draftRel, "Draft song", 1, "draft")
	live, _ := testutil.ReadyTrack(t, pool, owner, artist, pubRel, "Live song", 1, "public")
	fan := testutil.Token(t, testutil.User(t, pool, "user", true))
	hasLyrics := func(id string) bool {
		t.Helper()
		tr, err := trackview.Scan(pool.QueryRow(ctx, trackview.Select("")+` WHERE t.id = $1`, id), "http://api.test")
		if err != nil {
			t.Fatal(err)
		}
		return tr.HasLyrics
	}

	// nothing yet
	if code, _ := call("GET", "/tracks/"+live+"/lyrics", fan, nil); code != 404 {
		t.Fatalf("no lyrics = %d, want 404", code)
	}
	if hasLyrics(live) {
		t.Fatal("hasLyrics before any lyrics")
	}

	// the artist adds plain lyrics to a live track, a listener reads them
	code, b := call("PUT", "/tracks/"+live+"/lyrics", ownerTok, map[string]any{"text": "Hello\nWorld"})
	ly, _ := b["lyrics"].(map[string]any)
	if code != 200 || ly["synced"] != false || ly["text"] != "Hello\nWorld" || len(ly["lines"].([]any)) != 0 {
		t.Fatalf("put plain = %d %v", code, b)
	}
	code, b = call("GET", "/tracks/"+live+"/lyrics", fan, nil)
	if ly, _ = b["lyrics"].(map[string]any); code != 200 || ly["text"] != "Hello\nWorld" {
		t.Fatalf("listener get = %d %v", code, b)
	}
	if !hasLyrics(live) {
		t.Fatal("hasLyrics should be true")
	}

	// timed lyrics replace them
	code, b = call("PUT", "/tracks/"+live+"/lyrics", ownerTok, map[string]any{"lrc": "[00:01.50]Hello\n[00:03.00]World"})
	ly, _ = b["lyrics"].(map[string]any)
	lines, _ := ly["lines"].([]any)
	if code != 200 || ly["synced"] != true || len(lines) != 2 || lines[0].(map[string]any)["timeMs"] != float64(1500) || lines[1].(map[string]any)["text"] != "World" {
		t.Fatalf("put timed = %d %v", code, b)
	}
	if code, b = call("GET", "/tracks/"+live+"/lyrics", fan, nil); code != 200 || b["lyrics"].(map[string]any)["synced"] != true {
		t.Fatalf("get timed = %d %v", code, b)
	}

	// validation
	if code, _ = call("PUT", "/tracks/"+live+"/lyrics", ownerTok, map[string]any{}); code != 422 {
		t.Fatalf("empty put = %d", code)
	}
	if code, _ = call("PUT", "/tracks/"+live+"/lyrics", ownerTok, map[string]any{"lrc": "no times"}); code != 422 {
		t.Fatalf("bad lrc = %d", code)
	}
	if code, _ = call("PUT", "/tracks/"+live+"/lyrics", ownerTok, map[string]any{"words": "x"}); code != 400 {
		t.Fatalf("unknown field = %d", code)
	}

	// somebody else cannot write or delete
	if code, _ = call("PUT", "/tracks/"+live+"/lyrics", fan, map[string]any{"text": "mine now"}); code != 404 {
		t.Fatalf("stranger put = %d, want 404", code)
	}
	if code, _ = call("DELETE", "/tracks/"+live+"/lyrics", fan, nil); code != 404 {
		t.Fatalf("stranger delete = %d, want 404", code)
	}
	if code, b = call("GET", "/tracks/"+live+"/lyrics", fan, nil); code != 200 || b["lyrics"].(map[string]any)["synced"] != true {
		t.Fatalf("the lyrics must be untouched: %d %v", code, b)
	}

	// a draft track's lyrics are the owner's alone
	if code, _ = call("PUT", "/tracks/"+draft+"/lyrics", ownerTok, map[string]any{"text": "work in progress"}); code != 200 {
		t.Fatalf("draft put = %d", code)
	}
	if code, _ = call("GET", "/tracks/"+draft+"/lyrics", ownerTok, nil); code != 200 {
		t.Fatalf("owner reads draft lyrics = %d", code)
	}
	if code, _ = call("GET", "/tracks/"+draft+"/lyrics", fan, nil); code != 404 {
		t.Fatalf("listener reads draft lyrics = %d, want 404", code)
	}

	// delete is idempotent for the owner
	if code, _ = call("DELETE", "/tracks/"+live+"/lyrics", ownerTok, nil); code != 204 {
		t.Fatalf("delete = %d", code)
	}
	if code, _ = call("DELETE", "/tracks/"+live+"/lyrics", ownerTok, nil); code != 204 {
		t.Fatalf("second delete = %d", code)
	}
	if code, _ = call("GET", "/tracks/"+live+"/lyrics", fan, nil); code != http.StatusNotFound || hasLyrics(live) {
		t.Fatalf("after delete = %d, hasLyrics %v", code, hasLyrics(live))
	}
	if code, _ = call("DELETE", "/tracks/trk_missing/lyrics", ownerTok, nil); code != 404 {
		t.Fatalf("unknown track = %d", code)
	}

	// deleting the track takes its lyrics along
	if _, err := pool.Exec(ctx, `DELETE FROM tracks WHERE id = $1`, draft); err != nil {
		t.Fatal(err)
	}
	var n int
	pool.QueryRow(ctx, `SELECT count(*) FROM track_lyrics WHERE track_id = $1`, draft).Scan(&n) //nolint:errcheck
	if n != 0 {
		t.Fatal("lyrics outlived their track")
	}
}
