package genre

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/blacmusic/backend/internal/auth"
)

func TestListIsSaneAndEndsWithOther(t *testing.T) {
	if n := len(All); n < 35 || n > 50 {
		t.Fatalf("%d genres, want about 40", n)
	}
	ids, names := map[string]bool{}, map[string]bool{}
	for _, g := range All {
		if g.ID == "" || g.Name == "" || g.ID != strings.ToLower(g.ID) || strings.ContainsAny(g.ID, " /&") {
			t.Errorf("bad entry %+v (ids are lower-case slugs)", g)
		}
		if len(g.Name) > 40 {
			t.Errorf("%q is longer than the 40 characters a genre field allows", g.Name)
		}
		if ids[g.ID] || names[strings.ToLower(g.Name)] {
			t.Errorf("duplicate entry %+v", g)
		}
		ids[g.ID], names[strings.ToLower(g.Name)] = true, true
	}
	if last := All[len(All)-1]; last.Name != "Other" {
		t.Errorf("last entry = %q, want Other", last.Name)
	}
	for _, want := range []string{"Afrobeats", "Amapiano", "Bongo Flava", "Kidandali", "Gengetone", "Soukous", "Highlife", "Gospel", "Hip-Hop/Rap"} {
		if _, ok := Lookup(want); !ok {
			t.Errorf("%s missing", want)
		}
	}
}

func TestNormalize(t *testing.T) {
	ok := map[string]string{
		"Afrobeats": "Afrobeats", "afrobeats": "Afrobeats", "  AFROBEATS ": "Afrobeats",
		"hip-hop/rap": "Hip-Hop/Rap", "hip-hop-rap": "Hip-Hop/Rap", // by name or by id
		"r&b/soul": "R&B/Soul", "rnb-soul": "R&B/Soul", "": "", "   ": "",
		"Hip hop": "Hip-Hop/Rap", "HIP-HOP": "Hip-Hop/Rap", "Afrobeat": "Afrobeat", "zouk": "Zouk", "Dance": "Dance", "rap": "Rap", "R&B": "R&B", "rnb": "R&B",
	}
	for in, want := range ok {
		got, err := Normalize(in)
		if err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"Afrobeatz", "Rock & Roll", "<script>", "Pop; DROP TABLE", "Afro", strings.Repeat("x", 100)} {
		if _, err := Normalize(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("Normalize(%q) = %v, want ErrInvalid", bad, err)
		}
	}
}

func TestEndpoint(t *testing.T) {
	tokens := auth.NewTokenService("test-secret-test-secret-test-secret!", time.Minute)
	r := chi.NewRouter()
	r.Use(auth.Require(tokens))
	r.Get("/genres", List)

	tok, _, _ := tokens.NewAccess("11111111-1111-1111-1111-111111111111")
	req := httptest.NewRequest("GET", "/genres", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Cache-Control"), "max-age=") {
		t.Fatalf("GET /genres = %d, cache = %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	var body struct {
		Genres []map[string]string `json:"genres"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Genres) != len(All) || body.Genres[0]["id"] != "pop" || body.Genres[0]["name"] != "Pop" {
		t.Fatalf("body: %v", body.Genres[:2])
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/genres", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("without a token = %d, want 401", rec.Code)
	}
}
