package trackview

import (
	"encoding/json"

	"github.com/blacmusic/backend/internal/image"
	"github.com/blacmusic/backend/internal/source"
)

// ReleaseFeatured is the SQL for a release's featured artists, over albums al: a single shows its song's and the
// release's own; an EP or album only those of the whole release.
const ReleaseFeatured = `featured_artists(CASE WHEN al.release_type = 'single' THEN
	(SELECT st.id FROM tracks st WHERE st.album_id = al.id ORDER BY st.created_at, st.id LIMIT 1) END, al.id, al.artist_id)`

// Featured reads what the featured_artists SQL function gives; nil when a song has none.
func Featured(raw []byte, publicURL string) ([]source.FeaturedArtist, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var rows []struct {
		ArtistID *string `json:"artistId"`
		Name     string  `json:"name"`
		AvatarID *string `json:"avatarId"`
		ImageURL *string `json:"imageUrl"`
		Verified bool    `json:"verified"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	out := make([]source.FeaturedArtist, len(rows))
	for i, r := range rows {
		f := source.FeaturedArtist{Name: r.Name, Verified: r.Verified}
		legacy := ""
		if r.ImageURL != nil {
			legacy = *r.ImageURL
		}
		if r.ArtistID != nil {
			f.ArtistID, f.ImageURL = *r.ArtistID, image.PersonURL(publicURL, r.AvatarID, legacy)
		}
		out[i] = f
	}
	return out, nil
}
