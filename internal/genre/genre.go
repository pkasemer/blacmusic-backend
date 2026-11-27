// Package genre holds the list of music genres artists can choose from and the
// endpoint that serves it.
package genre

import (
	"errors"
	"net/http"
	"strings"

	"github.com/blacmusic/backend/internal/httpx"
)

// ErrInvalid is returned for a genre that is not in the list.
var ErrInvalid = errors.New("genre must be one of the values from GET /v1/genres")

// Genre is one entry of the list.
type Genre struct {
	ID   string `json:"id"`   // stable slug
	Name string `json:"name"` // display text; this is what is stored and sent in `genre` fields
}

// All is ordered roughly by popularity (global mainstream first, then genres
// that matter for the East African audience), with "Other" last.
var All = []Genre{
	{"pop", "Pop"},
	{"hip-hop-rap", "Hip-Hop/Rap"},
	{"rap", "Rap"},
	{"afrobeats", "Afrobeats"},
	{"afrobeat", "Afrobeat"},
	{"rnb-soul", "R&B/Soul"},
	{"rnb", "R&B"},
	{"amapiano", "Amapiano"},
	{"dancehall", "Dancehall"},
	{"gospel", "Gospel"},
	{"rock", "Rock"},
	{"electronic-dance", "Electronic/Dance"},
	{"dance", "Dance"},
	{"house", "House"},
	{"reggae", "Reggae"},
	{"latin", "Latin"},
	{"bongo-flava", "Bongo Flava"},
	{"kidandali", "Kidandali"},
	{"afro-house", "Afro House"},
	{"trap", "Trap"},
	{"afropop", "Afropop"},
	{"alternative", "Alternative"},
	{"indie", "Indie"},
	{"country", "Country"},
	{"jazz", "Jazz"},
	{"blues", "Blues"},
	{"classical", "Classical"},
	{"folk", "Folk"},
	{"metal", "Metal"},
	{"punk", "Punk"},
	{"k-pop", "K-Pop"},
	{"funk", "Funk"},
	{"disco", "Disco"},
	{"techno", "Techno"},
	{"drill", "Drill"},
	{"soukous", "Soukous"},
	{"zouk", "Zouk"},
	{"highlife", "Highlife"},
	{"gengetone", "Gengetone"},
	{"gqom", "Gqom"},
	{"kwaito", "Kwaito"},
	{"taarab", "Taarab"},
	{"lo-fi", "Lo-fi"},
	{"ambient", "Ambient"},
	{"kadongo-kamu", "Kadongo Kamu"},
	{"traditional", "Traditional"},
	{"other", "Other"},
}

// Lookup finds a genre by display name or id, ignoring case and surrounding space.
func Lookup(s string) (Genre, bool) {
	e, ok := current.Load().byKey[strings.ToLower(strings.TrimSpace(s))]
	return e.Genre, ok
}

// Normalize returns the canonical display name for a genre given by name or id.
// An empty string means "no genre" and is allowed. Anything else must be in the list.
func Normalize(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	e, ok := current.Load().byKey[strings.ToLower(s)]
	if !ok || !e.active {
		return "", ErrInvalid
	}
	return e.Name, nil
}

// List serves GET /v1/genres. The list rarely changes, so clients may cache it.
func List(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "private, max-age=3600")
	httpx.JSON(w, http.StatusOK, map[string]any{"genres": current.Load().list})
}
