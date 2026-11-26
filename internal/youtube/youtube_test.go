package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestVault(t *testing.T) {
	key := make([]byte, 32)
	v, err := NewVault(key)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := v.Seal("refresh-token-123", "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed), "refresh-token") {
		t.Fatal("ciphertext contains the secret")
	}
	if got, err := v.Open(sealed, "user-1"); err != nil || got != "refresh-token-123" {
		t.Fatalf("open = %q %v", got, err)
	}
	if _, err := v.Open(sealed, "user-2"); err == nil {
		t.Fatal("a value sealed for one user must not open for another")
	}
	other, _ := NewVault(append([]byte{1}, key[1:]...))
	if _, err := other.Open(sealed, "user-1"); err == nil {
		t.Fatal("wrong key must not open")
	}
	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 1
	if _, err := v.Open(tampered, "user-1"); err == nil {
		t.Fatal("tampered ciphertext must not open")
	}
	again, _ := v.Seal("refresh-token-123", "user-1")
	if string(again) == string(sealed) {
		t.Fatal("two seals of one value must differ (random nonce)")
	}
	if _, err := NewVault(make([]byte, 16)); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]int{"PT3M12S": 192, "PT1H2M3S": 3723, "PT45S": 45, "P1DT1H": 90000, "PT0S": 0, "bogus": 0, "": 0} {
		if got := ParseDuration(in); got != want {
			t.Errorf("ParseDuration(%q) = %d, want %d", in, got, want)
		}
	}
}

// fakeGoogle serves the endpoints the real provider calls.
func fakeGoogle(t *testing.T) (*Google, *[]string) {
	t.Helper()
	var calls []string
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		calls = append(calls, "token:"+r.Form.Get("grant_type"))
		switch {
		case r.Form.Get("grant_type") == "authorization_code" && r.Form.Get("code") == "good" && r.Form.Get("code_verifier") == "ver":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "acc", "refresh_token": "ref", "expires_in": 3600, "scope": ReadOnlyScope})
		case r.Form.Get("grant_type") == "refresh_token" && r.Form.Get("refresh_token") == "ref":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "acc2", "expires_in": 3600})
		default:
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"})
		}
	})
	mux.HandleFunc("/revoke", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		calls = append(calls, "revoke:"+r.Form.Get("token"))
	})
	mux.HandleFunc("/yt/channels", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer acc" {
			w.WriteHeader(401)
			return
		}
		if r.URL.Query().Get("mine") != "true" {
			t.Error("channels must be asked with mine=true")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{
			"id": "UC1", "snippet": map[string]any{"title": "Akeine", "thumbnails": map[string]any{"default": map[string]any{"url": "https://t/d.jpg"}, "high": map[string]any{"url": "https://t/h.jpg"}}},
			"contentDetails": map[string]any{"relatedPlaylists": map[string]any{"uploads": "UU1"}}}}})
	})
	mux.HandleFunc("/yt/playlistItems", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("playlistId") != "UU1" {
			t.Error("uploads playlist expected")
		}
		items := func(ids ...string) []map[string]any {
			var out []map[string]any
			for _, id := range ids {
				out = append(out, map[string]any{"contentDetails": map[string]any{"videoId": id}})
			}
			return out
		}
		if r.URL.Query().Get("pageToken") == "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"nextPageToken": "p2", "items": items("v1", "v2")})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items("v3")})
	})
	mux.HandleFunc("/yt/videos", func(w http.ResponseWriter, r *http.Request) {
		var items []map[string]any
		for _, id := range strings.Split(r.URL.Query().Get("id"), ",") {
			items = append(items, map[string]any{"id": id,
				"snippet":        map[string]any{"title": "Song " + id, "publishedAt": "2023-04-14T10:00:00Z", "thumbnails": map[string]any{"maxres": map[string]any{"url": "https://t/" + id + ".jpg"}}},
				"contentDetails": map[string]any{"duration": "PT3M20S"}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	g := NewGoogle("cid", "secret", "https://api.example/cb")
	g.TokenURL, g.RevokeURL, g.APIBase = srv.URL+"/token", srv.URL+"/revoke", srv.URL+"/yt"
	return g, &calls
}

func TestGoogleProvider(t *testing.T) {
	g, calls := fakeGoogle(t)
	ctx := context.Background()

	u, _ := url.Parse(g.AuthURL("st", "chal"))
	q := u.Query()
	if q.Get("scope") != ReadOnlyScope || q.Get("access_type") != "offline" || q.Get("code_challenge") != "chal" ||
		q.Get("code_challenge_method") != "S256" || q.Get("state") != "st" || q.Get("redirect_uri") != "https://api.example/cb" {
		t.Fatalf("auth url = %v", q)
	}
	if strings.Contains(q.Get("scope"), " ") {
		t.Fatal("only the read-only scope may be requested")
	}

	tok, err := g.Exchange(ctx, "good", "ver")
	if err != nil || tok.AccessToken != "acc" || tok.RefreshToken != "ref" {
		t.Fatalf("exchange = %+v %v", tok, err)
	}
	if _, err := g.Exchange(ctx, "good", "wrong-verifier"); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("bad verifier = %v", err)
	}
	if _, err := g.Refresh(ctx, "revoked"); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("revoked refresh token = %v, want ErrInvalidGrant", err)
	}
	r, err := g.Refresh(ctx, "ref")
	if err != nil || r.AccessToken != "acc2" {
		t.Fatalf("refresh = %+v %v", r, err)
	}

	ch, err := g.Channel(ctx, "acc")
	if err != nil || ch.ID != "UC1" || ch.Title != "Akeine" || ch.ThumbnailURL != "https://t/h.jpg" {
		t.Fatalf("channel = %+v %v", ch, err)
	}
	vs, err := g.Videos(ctx, "acc", 10)
	if err != nil || len(vs) != 3 || vs[0].DurationSec != 200 || vs[2].ThumbnailURL != "https://t/v3.jpg" || vs[0].PublishedAt.Year() != 2023 {
		t.Fatalf("videos = %+v %v", vs, err)
	}
	if vs, _ = g.Videos(ctx, "acc", 2); len(vs) != 2 {
		t.Fatalf("max not honoured: %d", len(vs))
	}
	if _, err := g.Channel(ctx, "expired"); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("401 from the API = %v", err)
	}
	if err := g.Revoke(ctx, "ref"); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range *calls {
		found = found || c == "revoke:ref"
	}
	if !found {
		t.Fatal("revoke did not reach Google")
	}
}

func TestBuildViewsGroupsVariants(t *testing.T) {
	f := NewFake("http://x/cb")
	owned := map[string]string{"wanted": "Wanted"}
	views := BuildViews(f.List, "Akeine", owned)
	by := map[string]VideoView{}
	for _, v := range views {
		by[v.Suggested.Title] = v
	}
	mu := by["Mukene"]
	if mu.VideoID != "demoMukeneAud" || mu.Kind != "song" || len(mu.Variants) != 2 {
		t.Fatalf("Mukene = %+v (audio upload should win, lyric video and short folded in)", mu)
	}
	if mu.Suggested.ReleaseDate != f.List[0].PublishedAt.UTC().Format("2006-01-02") {
		t.Fatalf("release date should be the earliest upload: %v", mu.Suggested)
	}
	if b := by["Better (ft Wana Benjamin)"]; b.VideoID != "demoBetterVis" || b.Kind != "song" {
		t.Fatalf("Better = %+v", b)
	}
	if w := by["Wanted"]; !w.AlreadyImported || w.DuplicateOf != "Wanted" {
		t.Fatalf("Wanted should be flagged as already imported: %+v", w)
	}
	for _, v := range views {
		if v.Kind == "promo" && v.Suggested.Title == "Mukene" {
			t.Fatal("promo grouped with a song")
		}
	}
	songs := 0
	for _, v := range views {
		if v.Kind == "song" {
			songs++
		}
	}
	if songs != 4 { // Mukene, Blessed, Better, Wanted
		t.Fatalf("song groups = %d, want 4", songs)
	}
	for i := 1; i < len(views); i++ {
		if views[i].PublishedAt.After(views[i-1].PublishedAt) {
			t.Fatal("views must be newest first")
		}
	}
	_ = time.Now
}
