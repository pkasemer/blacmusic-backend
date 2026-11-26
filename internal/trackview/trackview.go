// Package trackview is the one place that knows how a catalog track is read
// from the database and turned into the API's track shape, so search, browse,
// artist pages and track lookup all return the same fields.
package trackview

import (
	"github.com/jackc/pgx/v5"

	"github.com/blacmusic/backend/internal/image"
	"github.com/blacmusic/backend/internal/source"
)

// SourceName is the provider id of tracks we host ourselves.
const SourceName = "native"

const columns = `t.id, t.title, COALESCE(t.artist_id, ''), COALESCE(a.name, ''), COALESCE(t.album_id, ''), COALESCE(al.title, ''),
	t.duration_ms, COALESCE(t.artwork_url, al.artwork_url, ''), t.loudness_lufs, t.true_peak_db,
	COALESCE(au.avatar_image_id, a.avatar_image_id), COALESCE(a.image_url, ''),
	COALESCE(NULLIF(t.genre, ''), al.genre, ''), t.explicit,
	COALESCE(to_char(al.release_date, 'YYYY-MM-DD'), to_char(al.published_at AT TIME ZONE 'Africa/Kampala', 'YYYY-MM-DD'), ''),
	COALESCE(a.verification = 'verified', false),
	EXISTS (SELECT 1 FROM track_lyrics tl WHERE tl.track_id = t.id),
	EXISTS (SELECT 1 FROM release_credits rc WHERE rc.track_id = t.id OR (rc.track_id IS NULL AND rc.release_id = t.album_id))`

const from = `
	FROM tracks t
	LEFT JOIN artists a  ON a.id  = t.artist_id
	LEFT JOIN users au   ON au.id = a.owner_user_id
	LEFT JOIN albums  al ON al.id = t.album_id`

// Select returns "SELECT <track columns>[, extra] FROM tracks t ..." with the
// aliases t (track), a (artist), au (artist's user) and al (album) available to
// the WHERE and ORDER BY that the caller appends. extra is an optional
// additional column expression (Scan then needs a matching destination).
func Select(extra string) string {
	cols := columns
	if extra != "" {
		cols += ", " + extra
	}
	return "SELECT " + cols + from
}

// Scan reads one row produced by Select. extraDest receives the optional extra column.
func Scan(row pgx.Row, publicURL string, extraDest ...any) (source.Track, error) {
	var t source.Track
	var avatarID *string
	var legacyImage string
	dest := []any{&t.ID, &t.Title, &t.ArtistID, &t.ArtistName, &t.AlbumID, &t.AlbumTitle,
		&t.DurationMs, &t.ArtworkURL, &t.LoudnessLufs, &t.TruePeakDb,
		&avatarID, &legacyImage, &t.Genre, &t.Explicit, &t.ReleaseDate, &t.ArtistVerified, &t.HasLyrics, &t.HasCredits}
	if err := row.Scan(append(dest, extraDest...)...); err != nil {
		return t, err
	}
	t.ArtistImageURL = image.PersonURL(publicURL, avatarID, legacyImage)
	t.ArtworkLargeURL = image.LargeURL(t.ArtworkURL)
	t.Source, t.SourceID = SourceName, t.ID
	return t, nil
}
