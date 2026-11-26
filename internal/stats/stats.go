package stats

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrNotFound means there is no such artist profile or release for this user.
var ErrNotFound = errors.New("stats: not found")

// ErrNoArtist means the user has no artist profile.
var ErrNoArtist = errors.New("stats: this account has no artist profile")

// Range is a span of whole UTC days, From and To both included.
type Range struct {
	Name     string
	From, To time.Time
}

func (r Range) start() time.Time { return r.From }
func (r Range) end() time.Time   { return r.To.AddDate(0, 0, 1) } // exclusive

// ParseRange reads 7d, 28d, 90d or all (default 28d). "all" starts when the artist (or release) was created,
// but never more than 365 days back, so the daily series stays small.
func ParseRange(name string, now, created time.Time) (Range, error) {
	today := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	days := 0
	switch name {
	case "", "28d":
		name, days = "28d", 28
	case "7d":
		days = 7
	case "90d":
		days = 90
	case "all":
		from := time.Date(created.UTC().Year(), created.UTC().Month(), created.UTC().Day(), 0, 0, 0, 0, time.UTC)
		if oldest := today.AddDate(0, 0, -364); from.Before(oldest) {
			from = oldest
		}
		if from.After(today) {
			from = today
		}
		return Range{Name: "all", From: from, To: today}, nil
	default:
		return Range{}, &ValidationError{"range must be 7d, 28d, 90d or all"}
	}
	return Range{Name: name, From: today.AddDate(0, 0, -(days - 1)), To: today}, nil
}

// Point is one day of the series.
type Point struct {
	Date      string `json:"date"`
	Plays     int    `json:"plays"`
	Listeners int    `json:"listeners"`
}

// ArtistTotals are the key numbers of an artist.
type ArtistTotals struct {
	Plays                  int `json:"plays"`
	Listeners              int `json:"listeners"`
	MonthlyListeners       int `json:"monthlyListeners"`
	MonthlyListenersChange int `json:"monthlyListenersChange"`
	Followers              int `json:"followers"`
	FollowersChange        int `json:"followersChange"`
	Likes                  int `json:"likes"`
	LikesChange            int `json:"likesChange"`
	Downloads              int `json:"downloads"`
	PlaylistAdds           int `json:"playlistAdds"`
}

// TopTrack is a track with its numbers.
type TopTrack struct {
	TrackID    string `json:"trackId"`
	Title      string `json:"title"`
	ArtworkURL string `json:"artworkUrl,omitempty"`
	ReleaseID  string `json:"releaseId,omitempty"`
	Plays      int    `json:"plays"`
	Listeners  int    `json:"listeners"`
}

// ReleaseRow is a release with its numbers.
type ReleaseRow struct {
	ReleaseID  string `json:"releaseId"`
	Title      string `json:"title"`
	Type       string `json:"type"`
	Kind       string `json:"kind"`
	ArtworkURL string `json:"artworkUrl,omitempty"`
	Plays      int    `json:"plays"`
	Listeners  int    `json:"listeners"`
}

// ArtistStats is the answer of GET /v1/me/artist/stats.
type ArtistStats struct {
	Range     string       `json:"range"`
	From      string       `json:"from"`
	To        string       `json:"to"`
	Totals    ArtistTotals `json:"totals"`
	Series    []Point      `json:"series"`
	TopTracks []TopTrack   `json:"topTracks"`
	Releases  []ReleaseRow `json:"releases"`
}

const day = "2006-01-02"

// series gives one point per day of the range (zeros included) for plays matching col = id.
func (s *Store) series(ctx context.Context, col, id string, r Range) ([]Point, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT d::date, count(p.id), count(DISTINCT p.user_id)
		FROM generate_series($2::timestamptz, $3::timestamptz, interval '1 day') d
		LEFT JOIN plays p ON p.`+col+` = $1 AND p.at >= d AND p.at < d + interval '1 day'
		GROUP BY d ORDER BY d`, id, r.From, r.To)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Point{}
	for rows.Next() {
		var d time.Time
		var p Point
		if err := rows.Scan(&d, &p.Plays, &p.Listeners); err != nil {
			return nil, err
		}
		p.Date = d.Format(day)
		out = append(out, p)
	}
	return out, rows.Err()
}

// Artist returns the numbers of the user's artist profile.
func (s *Store) Artist(ctx context.Context, userID, rangeName string, now time.Time) (*ArtistStats, error) {
	var artistID string
	var created time.Time
	err := s.pool.QueryRow(ctx, `SELECT id, created_at FROM artists WHERE owner_user_id = $1`, userID).Scan(&artistID, &created)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoArtist
	}
	if err != nil {
		return nil, err
	}
	r, err := ParseRange(rangeName, now, created)
	if err != nil {
		return nil, err
	}
	out := &ArtistStats{Range: r.Name, From: r.From.Format(day), To: r.To.Format(day)}
	from, to := r.start(), r.end()
	t := &out.Totals

	if err := s.pool.QueryRow(ctx, `SELECT count(*), count(DISTINCT user_id) FROM plays WHERE artist_id = $1 AND at >= $2 AND at < $3`,
		artistID, from, to).Scan(&t.Plays, &t.Listeners); err != nil {
		return nil, err
	}
	var prev int
	if err := s.pool.QueryRow(ctx, `
		SELECT count(DISTINCT user_id) FILTER (WHERE at >= $2::timestamptz - interval '30 days'),
		       count(DISTINCT user_id) FILTER (WHERE at < $2::timestamptz - interval '30 days')
		FROM plays WHERE artist_id = $1 AND at >= $2::timestamptz - interval '60 days' AND at <= $2::timestamptz`,
		artistID, now).Scan(&t.MonthlyListeners, &prev); err != nil {
		return nil, err
	}
	t.MonthlyListenersChange = t.MonthlyListeners - prev
	if err := s.pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE added_at >= $2 AND added_at < $3) FROM library_artists WHERE artist_id = $1`,
		artistID, from, to).Scan(&t.Followers, &t.FollowersChange); err != nil {
		return nil, err
	}
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE l.added_at >= $2 AND l.added_at < $3)
		FROM library_tracks l JOIN tracks tr ON tr.id = l.track_id WHERE tr.artist_id = $1`,
		artistID, from, to).Scan(&t.Likes, &t.LikesChange); err != nil {
		return nil, err
	}
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM playlist_items i JOIN tracks tr ON tr.id = i.track_id
		JOIN playlists p ON p.id = i.playlist_id JOIN artists a ON a.id = tr.artist_id
		WHERE tr.artist_id = $1 AND p.owner_id IS DISTINCT FROM a.owner_user_id AND i.added_at >= $2 AND i.added_at < $3`,
		artistID, from, to).Scan(&t.PlaylistAdds); err != nil {
		return nil, err
	}

	if out.Series, err = s.series(ctx, "artist_id", artistID, r); err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx, `
		SELECT tr.id, tr.title, COALESCE(NULLIF(tr.artwork_url, ''), al.artwork_url, ''), COALESCE(tr.album_id, ''),
		       count(p.id) AS plays, count(DISTINCT p.user_id)
		FROM plays p JOIN tracks tr ON tr.id = p.track_id LEFT JOIN albums al ON al.id = tr.album_id
		WHERE p.artist_id = $1 AND p.at >= $2 AND p.at < $3
		GROUP BY tr.id, tr.title, tr.artwork_url, al.artwork_url, tr.album_id
		ORDER BY plays DESC, tr.title, tr.id LIMIT 5`, artistID, from, to)
	if err != nil {
		return nil, err
	}
	out.TopTracks = []TopTrack{}
	for rows.Next() {
		var x TopTrack
		if err := rows.Scan(&x.TrackID, &x.Title, &x.ArtworkURL, &x.ReleaseID, &x.Plays, &x.Listeners); err != nil {
			rows.Close()
			return nil, err
		}
		out.TopTracks = append(out.TopTracks, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rrows, err := s.pool.Query(ctx, `
		SELECT al.id, al.title, al.release_type, COALESCE(al.artwork_url, ''),
		       (SELECT count(*) FROM tracks tt WHERE tt.album_id = al.id),
		       count(p.id) AS plays, count(DISTINCT p.user_id)
		FROM albums al LEFT JOIN plays p ON p.release_id = al.id AND p.at >= $2 AND p.at < $3
		WHERE al.artist_id = $1
		GROUP BY al.id ORDER BY plays DESC, al.created_at DESC, al.id LIMIT 100`, artistID, from, to)
	if err != nil {
		return nil, err
	}
	defer rrows.Close()
	out.Releases = []ReleaseRow{}
	for rrows.Next() {
		var x ReleaseRow
		var tracks int
		if err := rrows.Scan(&x.ReleaseID, &x.Title, &x.Type, &x.ArtworkURL, &tracks, &x.Plays, &x.Listeners); err != nil {
			return nil, err
		}
		x.Kind = kindOf(x.Type, tracks)
		out.Releases = append(out.Releases, x)
	}
	return out, rrows.Err()
}

func kindOf(releaseType string, tracks int) string {
	switch {
	case releaseType == "single":
		return "single"
	case tracks <= 6:
		return "ep"
	}
	return "album"
}

// ReleaseTotals are the key numbers of one release.
type ReleaseTotals struct {
	Plays          int     `json:"plays"`
	Listeners      int     `json:"listeners"`
	Likes          int     `json:"likes"`
	Downloads      int     `json:"downloads"`
	PlaylistAdds   int     `json:"playlistAdds"`
	AvgListenSec   int     `json:"avgListenSec"`
	CompletionRate float64 `json:"completionRate"`
}

// TrackRow is a track of a release with its numbers.
type TrackRow struct {
	TrackID        string  `json:"trackId"`
	Title          string  `json:"title"`
	Plays          int     `json:"plays"`
	Listeners      int     `json:"listeners"`
	Likes          int     `json:"likes"`
	CompletionRate float64 `json:"completionRate"`
}

// SourceRow says how many plays started somewhere.
type SourceRow struct {
	Source string `json:"source"`
	Plays  int    `json:"plays"`
}

// ReleaseStats is the answer of GET /v1/releases/{id}/stats.
type ReleaseStats struct {
	Range   string        `json:"range"`
	From    string        `json:"from"`
	To      string        `json:"to"`
	Totals  ReleaseTotals `json:"totals"`
	Series  []Point       `json:"series"`
	Tracks  []TrackRow    `json:"tracks"`
	Sources []SourceRow   `json:"sources"`
}

func rate(n, of int) float64 {
	if of == 0 {
		return 0
	}
	return math.Round(float64(n)/float64(of)*1000) / 1000
}

// Release returns the numbers of one of the user's releases.
func (s *Store) Release(ctx context.Context, userID, releaseID, rangeName string, now time.Time) (*ReleaseStats, error) {
	var created time.Time
	err := s.pool.QueryRow(ctx, `SELECT al.created_at FROM albums al JOIN artists a ON a.id = al.artist_id WHERE al.id = $1 AND a.owner_user_id = $2`,
		releaseID, userID).Scan(&created)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r, err := ParseRange(rangeName, now, created)
	if err != nil {
		return nil, err
	}
	out := &ReleaseStats{Range: r.Name, From: r.From.Format(day), To: r.To.Format(day)}
	from, to := r.start(), r.end()
	t := &out.Totals

	var completed int
	var avg *float64
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*), count(DISTINCT user_id), count(*) FILTER (WHERE completed), avg(listened_sec)
		FROM plays WHERE release_id = $1 AND at >= $2 AND at < $3`, releaseID, from, to).
		Scan(&t.Plays, &t.Listeners, &completed, &avg); err != nil {
		return nil, err
	}
	if avg != nil {
		t.AvgListenSec = int(math.Round(*avg))
	}
	t.CompletionRate = rate(completed, t.Plays)
	if err := s.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM library_albums WHERE album_id = $1)
		     + (SELECT count(*) FROM library_tracks l JOIN tracks tr ON tr.id = l.track_id WHERE tr.album_id = $1)`, releaseID).Scan(&t.Likes); err != nil {
		return nil, err
	}
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM playlist_items i JOIN tracks tr ON tr.id = i.track_id
		JOIN playlists p ON p.id = i.playlist_id JOIN artists a ON a.id = tr.artist_id
		WHERE tr.album_id = $1 AND p.owner_id IS DISTINCT FROM a.owner_user_id AND i.added_at >= $2 AND i.added_at < $3`,
		releaseID, from, to).Scan(&t.PlaylistAdds); err != nil {
		return nil, err
	}
	if out.Series, err = s.series(ctx, "release_id", releaseID, r); err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx, `
		SELECT tr.id, tr.title, count(p.id), count(DISTINCT p.user_id), count(p.id) FILTER (WHERE p.completed),
		       (SELECT count(*) FROM library_tracks l WHERE l.track_id = tr.id)
		FROM tracks tr LEFT JOIN plays p ON p.track_id = tr.id AND p.at >= $2 AND p.at < $3
		WHERE tr.album_id = $1
		GROUP BY tr.id, tr.title, tr.track_number, tr.created_at
		ORDER BY tr.track_number NULLS LAST, tr.created_at, tr.id`, releaseID, from, to)
	if err != nil {
		return nil, err
	}
	out.Tracks = []TrackRow{}
	for rows.Next() {
		var x TrackRow
		var done int
		if err := rows.Scan(&x.TrackID, &x.Title, &x.Plays, &x.Listeners, &done, &x.Likes); err != nil {
			rows.Close()
			return nil, err
		}
		x.CompletionRate = rate(done, x.Plays)
		out.Tracks = append(out.Tracks, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	srows, err := s.pool.Query(ctx, `
		SELECT source, count(*) AS n FROM plays WHERE release_id = $1 AND at >= $2 AND at < $3
		GROUP BY source ORDER BY n DESC, source`, releaseID, from, to)
	if err != nil {
		return nil, err
	}
	defer srows.Close()
	out.Sources = []SourceRow{}
	for srows.Next() {
		var x SourceRow
		if err := srows.Scan(&x.Source, &x.Plays); err != nil {
			return nil, err
		}
		out.Sources = append(out.Sources, x)
	}
	return out, srows.Err()
}
