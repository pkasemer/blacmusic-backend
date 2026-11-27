package recommend

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DB is a pool or a transaction.
type DB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// ErrNoSong means the starting song of a mix is not one people can play.
var ErrNoSong = errors.New("recommend: no such song")

const featureColumns = `f.track_id, f.artist_id, f.genre, f.genres, f.primary_languages, f.other_languages, f.regions,
	f.cultures, f.attributes, f.released_at, COALESCE(s.listeners_7d, 0), COALESCE(s.listeners_30d, 0),
	COALESCE(s.completes_90d, 0), COALESCE(s.skips_90d, 0), COALESCE(s.savers, 0)`

const featureFrom = ` FROM rec_track_features f LEFT JOIN rec_track_stats s ON s.track_id = f.track_id`

func tracks(ctx context.Context, db DB, query string, args ...any) ([]Track, error) {
	rows, err := db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Track
	for rows.Next() {
		var t Track
		if err := rows.Scan(&t.ID, &t.ArtistID, &t.Genre, &t.Genres, &t.PrimaryLanguages, &t.OtherLanguages, &t.Regions,
			&t.Cultures, &t.Attributes, &t.Released, &t.Stats.Listeners7d, &t.Stats.Listeners30d,
			&t.Stats.Completes90d, &t.Stats.Skips90d, &t.Stats.Savers); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Song reads one available song.
func Song(ctx context.Context, db DB, id string) (Track, error) {
	found, err := tracks(ctx, db, "SELECT "+featureColumns+featureFrom+" WHERE f.track_id = $1", id)
	if err != nil {
		return Track{}, err
	}
	if len(found) == 0 {
		return Track{}, ErrNoSong
	}
	return found[0], nil
}

// LoadProfile reads what a listener said and did, and works out their taste.
func LoadProfile(ctx context.Context, db DB, userID string) (*Profile, error) {
	var s Stated
	err := db.QueryRow(ctx, `SELECT discovery_mode, preferred_languages, understood_languages, regions, genres, artist_ids
		FROM listener_preferences WHERE user_id = $1`, userID).
		Scan(&s.Mode, &s.PreferredLanguages, &s.UnderstoodLanguages, &s.Regions, &s.Genres, &s.FavouriteArtists)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	rows, err := db.Query(ctx, `SELECT artist_id FROM library_artists WHERE user_id = $1`, userID)
	if err != nil {
		return nil, err
	}
	if s.FollowedArtists, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return nil, err
	}

	// an artist's listening to their own music says nothing about their taste
	rows, err = db.Query(ctx, `
		SELECT h.track_id, h.listens, h.completes, h.skips, h.weak_skips, h.playlist_adds,
		       h.completes_w, h.replays_w, h.skips_w, h.weak_skips_w, h.plays_w, h.liked, h.unliked, h.saved,
		       h.last_heard_at, h.last_skipped_at, h.last_weak_skipped_at
		FROM rec_user_tracks h
		WHERE h.user_id = $1 AND NOT EXISTS (SELECT 1 FROM tracks t JOIN artists a ON a.id = t.artist_id
		                                     WHERE t.id = h.track_id AND a.owner_user_id = $1)`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	history, ids := map[string]History{}, []string{}
	for rows.Next() {
		var id string
		var h History
		var heard, skipped, weak *time.Time
		if err := rows.Scan(&id, &h.Listens, &h.Completes, &h.Skips, &h.WeakSkips, &h.PlaylistAdds,
			&h.CompletesW, &h.ReplaysW, &h.SkipsW, &h.WeakSkipsW, &h.PlaysW, &h.Liked, &h.Unliked, &h.Saved,
			&heard, &skipped, &weak); err != nil {
			return nil, err
		}
		for _, x := range []struct {
			from *time.Time
			to   *time.Time
		}{{heard, &h.LastHeard}, {skipped, &h.LastSkipped}, {weak, &h.LastWeakSkipped}} {
			if x.from != nil {
				*x.to = *x.from
			}
		}
		history[id] = h
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	songs, err := tracks(ctx, db, "SELECT "+featureColumns+featureFrom+" WHERE f.track_id = ANY($1)", ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]Track, len(songs))
	for _, t := range songs {
		byID[t.ID] = t
	}
	return NewProfile(s, history, byID), nil
}

// How many candidates each way of finding them may give.
const (
	fromArtists  = 300
	fromGenres   = 400
	fromLanguage = 250
	fromPlace    = 150
	fromPopular  = 150
	fromNewest   = 100
	fromAnywhere = 150
)

// topKeys gives the keys with the highest values above zero.
func topKeys(m map[string]float64, most int) []string {
	out := []string{}
	for k, v := range m {
		if v > 0 {
			out = append(out, k)
		}
	}
	sortByValue(out, m)
	return out[:min(len(out), most)]
}

// Candidates finds songs worth ranking: from the artists, genres, languages and places in the listener's taste (and
// in the starting song's, for a mix), plus what is popular, what is new and a handful from anywhere, so that songs
// nobody has found yet have a chance. The listener's own music is never among them.
func Candidates(ctx context.Context, db DB, userID string, p *Profile, seed *Track) ([]Track, error) {
	artists := topKeys(p.artist, 60)
	genres := topKeys(p.genre, 8)
	languages := append(append([]string{}, p.PreferredLanguages...), p.UnderstoodLanguages...)
	languages = append(languages, topKeys(p.heardLanguage, 5)...)
	regions, cultures := append(append([]string{}, p.Regions...), topKeys(p.heardRegion, 3)...), []string{}
	if seed != nil {
		// a mix is about the starting song first
		artists = append(artists, seed.ArtistID)
		genres = seed.genreKeys()
		languages = append(languages, seed.languages()...)
		regions = append(regions, seed.Regions...)
		cultures = seed.Cultures
	}
	for _, g := range append([]string{}, genres...) {
		genres = append(genres, related(g)...)
	}
	nonNil := func(s []string) []string {
		if s == nil {
			return []string{}
		}
		return s
	}
	return tracks(ctx, db, `
		WITH pool AS (
			(SELECT track_id FROM rec_track_features WHERE artist_id = ANY($2) ORDER BY random() LIMIT $7)
			UNION (SELECT track_id FROM rec_track_features WHERE genre = ANY($3) OR genres && $3 ORDER BY random() LIMIT $8)
			UNION (SELECT track_id FROM track_languages WHERE verification = 'verified' AND language_id = ANY($4) ORDER BY random() LIMIT $9)
			UNION (SELECT track_id FROM (
			           SELECT track_id FROM track_regions WHERE verification = 'verified' AND region_id = ANY($5)
			           UNION SELECT track_id FROM track_cultures WHERE verification = 'verified' AND culture_id = ANY($6)) place
			       ORDER BY random() LIMIT $10)
			UNION (SELECT track_id FROM rec_track_stats ORDER BY listeners_30d DESC, listeners_7d DESC, track_id LIMIT $11)
			UNION (SELECT id FROM tracks WHERE visibility = 'public' ORDER BY created_at DESC LIMIT $12)
			UNION (SELECT id FROM tracks WHERE visibility = 'public' ORDER BY random() LIMIT $13)
		)
		SELECT `+featureColumns+featureFrom+`
		JOIN pool ON pool.track_id = f.track_id
		WHERE f.artist_owner IS DISTINCT FROM $1::uuid
		  AND NOT EXISTS (SELECT 1 FROM rec_blocked_tracks b WHERE b.track_id = f.track_id)`,
		userID, nonNil(artists), nonNil(genres), nonNil(languages), nonNil(regions), nonNil(cultures),
		fromArtists, fromGenres, fromLanguage, fromPlace, fromPopular, fromNewest, fromAnywhere)
}

// Record keeps what was suggested.
func Record(ctx context.Context, db DB, userID, surface, mode, seedTrackID string, picks []Pick) error {
	if len(picks) == 0 {
		return nil
	}
	mode, _ = policyFor(mode)
	positions, ids, slots, scores := []int32{}, []string{}, []string{}, []float32{}
	reasons := []string{}
	for i, p := range picks {
		positions = append(positions, int32(i))
		ids = append(ids, p.TrackID)
		slots = append(slots, p.Slot)
		scores = append(scores, float32(p.Score))
		reasons = append(reasons, joinReasons(p.Reasons))
	}
	_, err := db.Exec(ctx, `
		WITH s AS (INSERT INTO rec_serves (user_id, surface, algorithm, discovery_mode, seed_track_id)
		           VALUES ($1, $2, $3, $4, $5) RETURNING id)
		INSERT INTO rec_serve_items (serve_id, position, track_id, slot, score, reasons)
		SELECT s.id, i.position, i.track_id, i.slot, i.score, string_to_array(i.reasons, ',')
		FROM s, unnest($6::int[], $7::text[], $8::text[], $9::real[], $10::text[]) AS i(position, track_id, slot, score, reasons)`,
		userID, surface, Version, mode, seedTrackID, positions, ids, slots, scores, reasons)
	return err
}

// keepServes is how long what was suggested is kept.
const keepServes = "90 days"

// RefreshStats rebuilds how every song is doing with listeners as a whole.
func RefreshStats(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// one rebuild at a time
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('rec_track_stats'))`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM rec_track_stats`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		WITH listened AS ( -- who heard enough of what, and when
			SELECT track_id, user_id, at FROM signal_plays WHERE at > now() - interval '30 days' AND user_id IS NOT NULL
			UNION ALL SELECT track_id, user_id, at FROM signal_listens
			WHERE type = 'listen' AND listened_sec >= 30 AND at > now() - interval '30 days'
		), heard AS (
			SELECT track_id, count(DISTINCT user_id) FILTER (WHERE at > now() - interval '7 days') AS week, count(DISTINCT user_id) AS month
			FROM listened GROUP BY track_id
		), judged AS (
			SELECT track_id, count(DISTINCT (user_id, at::date)) FILTER (WHERE outcome = 'completed') AS finished,
			       count(DISTINCT (user_id, at::date)) FILTER (WHERE outcome = 'skipped' AND capture >= 2) AS skipped
			FROM signal_listens WHERE type = 'listen' AND at > now() - interval '90 days' GROUP BY track_id
		), kept AS (
			SELECT track_id, count(*) AS savers FROM signal_saves GROUP BY track_id
		)
		INSERT INTO rec_track_stats (track_id, listeners_7d, listeners_30d, completes_90d, skips_90d, savers)
		SELECT t.id, COALESCE(h.week, 0), COALESCE(h.month, 0), COALESCE(j.finished, 0), COALESCE(j.skipped, 0), COALESCE(k.savers, 0)
		FROM tracks t LEFT JOIN heard h ON h.track_id = t.id LEFT JOIN judged j ON j.track_id = t.id LEFT JOIN kept k ON k.track_id = t.id
		WHERE h.track_id IS NOT NULL OR j.track_id IS NOT NULL OR k.track_id IS NOT NULL`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

const statsEvery = 15 * time.Minute

// Run keeps the song statistics fresh and deletes old records of suggestions. The worker calls it; it runs apart
// from the other upkeep, so a slow rebuild never delays a release going live.
func Run(ctx context.Context, pool *pgxpool.Pool) {
	for {
		start := time.Now()
		if err := RefreshStats(ctx, pool); err != nil && ctx.Err() == nil {
			slog.Error("recommendation statistics", "err", err)
		} else if took := time.Since(start); took > 10*time.Second {
			slog.Warn("recommendation statistics are slow", "took", took.Round(time.Millisecond))
		}
		if _, err := pool.Exec(ctx, `DELETE FROM rec_serves WHERE at < now() - interval '`+keepServes+`'`); err != nil && ctx.Err() == nil {
			slog.Error("delete old suggestion records", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(statsEvery):
		}
	}
}
