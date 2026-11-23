package podcast

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound = errors.New("podcast: not found")
	ErrLimit    = errors.New("podcast: subscription limit reached")
)

// MaxSubscriptions caps how many shows one user can follow.
const MaxSubscriptions = 1000

// completionSlack: an episode counts as finished when playback gets this close to the end.
const completionSlackMs = 20_000

// Store owns shows, episodes, subscriptions and progress.
type Store struct {
	pool     *pgxpool.Pool
	fetcher  *Fetcher
	interval time.Duration // how often a subscribed show is re-fetched
}

// NewStore creates the store.
func NewStore(pool *pgxpool.Pool, fetcher *Fetcher, refreshInterval time.Duration) *Store {
	return &Store{pool: pool, fetcher: fetcher, interval: refreshInterval}
}

// Show is a podcast.
type Show struct {
	ID            string     `json:"id"`
	Title         string     `json:"title"`
	Author        string     `json:"author,omitempty"`
	Description   string     `json:"description,omitempty"`
	ImageURL      string     `json:"imageUrl,omitempty"`
	WebsiteURL    string     `json:"websiteUrl,omitempty"`
	Language      string     `json:"language,omitempty"`
	Explicit      bool       `json:"explicit"`
	Subscribed    bool       `json:"subscribed"`
	EpisodeCount  int        `json:"episodeCount"`
	LatestEpisode *time.Time `json:"latestEpisodeAt,omitempty"`
	UnplayedCount *int       `json:"unplayedCount,omitempty"` // only in the subscription list; capped at 100
	SubscribedAt  *time.Time `json:"subscribedAt,omitempty"`  // only in the subscription list
}

// Progress is a user's place in an episode.
type Progress struct {
	PositionMs int       `json:"positionMs"`
	Completed  bool      `json:"completed"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// EpisodeView is an episode as the API returns it.
type EpisodeView struct {
	ID            string    `json:"id"`
	ShowID        string    `json:"showId"`
	ShowTitle     string    `json:"showTitle"`
	Title         string    `json:"title"`
	Description   string    `json:"description,omitempty"`
	PublishedAt   time.Time `json:"publishedAt"`
	DurationMs    int       `json:"durationMs"`
	SizeBytes     int64     `json:"sizeBytes,omitempty"`
	ImageURL      string    `json:"imageUrl,omitempty"` // episode art, else the show's
	Explicit      bool      `json:"explicit"`
	Season        *int      `json:"season,omitempty"`
	EpisodeNumber *int      `json:"episodeNumber,omitempty"`
	EpisodeType   string    `json:"episodeType"`
	Source        string    `json:"source"` // always "podcast": play it with GET /v1/playback/{id}
	Progress      *Progress `json:"progress,omitempty"`
}

// ---------------------------------------------------------------- adding shows

// AddFeed makes sure the feed is a known show and subscribes the user to it.
// created reports whether the show was new.
func (s *Store) AddFeed(ctx context.Context, userID, rawURL string) (show *Show, created bool, err error) {
	feedURL, err := NormalizeFeedURL(rawURL)
	if err != nil {
		return nil, false, err
	}

	var id string
	err = s.pool.QueryRow(ctx, `SELECT id FROM shows WHERE feed_url = $1`, feedURL).Scan(&id)
	switch {
	case err == nil:
		// already known
	case errors.Is(err, pgx.ErrNoRows):
		res, ferr := s.fetcher.Fetch(ctx, feedURL, "", "")
		if ferr != nil {
			return nil, false, ferr
		}
		feed, perr := ParseFeed(res.Body)
		if perr != nil {
			return nil, false, perr
		}
		id, created, err = s.insertShow(ctx, feedURL, feed, res)
		if err != nil {
			return nil, false, err
		}
	default:
		return nil, false, err
	}

	if err := s.Subscribe(ctx, userID, id); err != nil {
		return nil, false, err
	}
	show, err = s.GetShow(ctx, id, userID)
	return show, created, err
}

func (s *Store) insertShow(ctx context.Context, feedURL string, f *Feed, res *Result) (id string, created bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	err = tx.QueryRow(ctx, `
		INSERT INTO shows (feed_url, title, author, description, image_url, website_url, language, explicit,
		                   etag, last_modified, last_fetched_at, next_fetch_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10, now(), now() + make_interval(secs => $11))
		ON CONFLICT (feed_url) DO NOTHING
		RETURNING id`,
		feedURL, f.Title, f.Author, f.Description, f.ImageURL, f.Link, f.Language, f.Explicit,
		res.ETag, res.LastModified, s.interval.Seconds()).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) { // someone added it a moment ago
		return id, false, s.pool.QueryRow(ctx, `SELECT id FROM shows WHERE feed_url = $1`, feedURL).Scan(&id)
	}
	if err != nil {
		return "", false, err
	}
	if err := upsertEpisodes(ctx, tx, id, f.Episodes); err != nil {
		return "", false, err
	}
	return id, true, tx.Commit(ctx)
}

// upsertEpisodes inserts new episodes and refreshes the ones already stored.
// An episode keeps its stored date when the feed supplies none.
func upsertEpisodes(ctx context.Context, tx pgx.Tx, showID string, eps []Episode) error {
	if len(eps) == 0 {
		return nil
	}
	const q = `
		INSERT INTO episodes (show_id, guid, title, description, audio_url, mime_type, size_bytes, duration_ms,
		                      published_at, season, episode_number, episode_type, image_url, explicit)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8, COALESCE($9::timestamptz, now()), $10,$11,$12,$13,$14)
		ON CONFLICT (show_id, guid) DO UPDATE SET
			title = EXCLUDED.title, description = EXCLUDED.description, audio_url = EXCLUDED.audio_url,
			mime_type = EXCLUDED.mime_type, size_bytes = EXCLUDED.size_bytes, duration_ms = EXCLUDED.duration_ms,
			published_at = COALESCE($9::timestamptz, episodes.published_at),
			season = EXCLUDED.season, episode_number = EXCLUDED.episode_number,
			episode_type = EXCLUDED.episode_type, image_url = EXCLUDED.image_url, explicit = EXCLUDED.explicit`
	batch := &pgx.Batch{}
	for _, e := range eps {
		var pub *time.Time
		if !e.PublishedAt.IsZero() {
			t := e.PublishedAt
			pub = &t
		}
		batch.Queue(q, showID, e.GUID, e.Title, e.Description, e.AudioURL, e.MimeType, e.SizeBytes, e.DurationMs,
			pub, e.Season, e.EpisodeNumber, e.EpisodeType, e.ImageURL, e.Explicit)
	}
	br := tx.SendBatch(ctx, batch)
	for range eps {
		if _, err := br.Exec(); err != nil {
			br.Close()
			return err
		}
	}
	return br.Close()
}

// ------------------------------------------------------------------ refreshing

// ClaimDue leases up to n subscribed shows whose refresh time has come. The
// lease pushes next_fetch_at ten minutes out, so a crashed worker's shows come
// back later and two workers never fetch the same show at once.
func (s *Store) ClaimDue(ctx context.Context, n int) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE shows SET next_fetch_at = now() + interval '10 minutes'
		WHERE id IN (
			SELECT sh.id FROM shows sh
			WHERE sh.next_fetch_at <= now()
			  AND EXISTS (SELECT 1 FROM show_subscriptions ss WHERE ss.show_id = sh.id)
			ORDER BY sh.next_fetch_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED)
		RETURNING id`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// Refresh re-fetches one show and records the outcome. Failures back off
// exponentially (interval, 2x, 4x ... up to a day) and are recorded on the show.
func (s *Store) Refresh(ctx context.Context, showID string) error {
	var feedURL, etag, lastMod string
	var fails int
	err := s.pool.QueryRow(ctx,
		`SELECT feed_url, etag, last_modified, fail_count FROM shows WHERE id = $1`, showID).
		Scan(&feedURL, &etag, &lastMod, &fails)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}

	res, ferr := s.fetcher.Fetch(ctx, feedURL, etag, lastMod)
	if ferr != nil {
		return s.recordFailure(ctx, showID, fails, ferr)
	}
	if res.NotModified {
		_, err := s.pool.Exec(ctx, `
			UPDATE shows SET last_fetched_at = now(), next_fetch_at = now() + make_interval(secs => $2),
			                 fail_count = 0, fetch_error = ''
			WHERE id = $1`, showID, s.interval.Seconds())
		return err
	}
	feed, perr := ParseFeed(res.Body)
	if perr != nil {
		return s.recordFailure(ctx, showID, fails, perr)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `
		UPDATE shows SET title=$2, author=$3, description=$4, image_url=$5, website_url=$6, language=$7, explicit=$8,
		                 etag=$9, last_modified=$10, last_fetched_at=now(),
		                 next_fetch_at = now() + make_interval(secs => $11), fail_count=0, fetch_error=''
		WHERE id = $1`,
		showID, feed.Title, feed.Author, feed.Description, feed.ImageURL, feed.Link, feed.Language, feed.Explicit,
		res.ETag, res.LastModified, s.interval.Seconds()); err != nil {
		return err
	}
	if err := upsertEpisodes(ctx, tx, showID, feed.Episodes); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) recordFailure(ctx context.Context, showID string, fails int, cause error) error {
	if ctx.Err() != nil { // shutting down or timed out: not the feed's fault
		return ctx.Err()
	}
	msg := "the feed could not be refreshed"
	var fe *FeedError
	if errors.As(cause, &fe) {
		msg = fe.Msg
	}
	backoff := s.interval << min(fails+1, 6) // 2x, 4x ... 64x
	if backoff > 24*time.Hour {
		backoff = 24 * time.Hour
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE shows SET fail_count = fail_count + 1, fetch_error = $2,
		                 next_fetch_at = now() + make_interval(secs => $3)
		WHERE id = $1`, showID, msg, backoff.Seconds()); err != nil {
		return err
	}
	slog.Warn("feed refresh failed", "show", showID, "reason", msg, "retry_in", backoff.Round(time.Minute))
	return nil
}

// --------------------------------------------------------------------- reading

const showSelect = `
	SELECT s.id, s.title, s.author, s.description, s.image_url, s.website_url, s.language, s.explicit,
	       EXISTS (SELECT 1 FROM show_subscriptions x WHERE x.show_id = s.id AND x.user_id = $1),
	       (SELECT count(*) FROM episodes e WHERE e.show_id = s.id),
	       (SELECT max(e.published_at) FROM episodes e WHERE e.show_id = s.id)
	FROM shows s`

func scanShow(row pgx.Row) (*Show, error) {
	var sh Show
	err := row.Scan(&sh.ID, &sh.Title, &sh.Author, &sh.Description, &sh.ImageURL, &sh.WebsiteURL, &sh.Language,
		&sh.Explicit, &sh.Subscribed, &sh.EpisodeCount, &sh.LatestEpisode)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &sh, err
}

// GetShow returns a show as seen by userID.
func (s *Store) GetShow(ctx context.Context, id, userID string) (*Show, error) {
	return scanShow(s.pool.QueryRow(ctx, showSelect+` WHERE s.id = $2`, userID, id))
}

// SearchShows finds known shows by title or author.
func (s *Store) SearchShows(ctx context.Context, userID, q string, limit, offset int) ([]*Show, error) {
	pattern := "%" + escapeLike(q) + "%"
	rows, err := s.pool.Query(ctx, showSelect+`
		WHERE s.title ILIKE $2 OR s.author ILIKE $2
		ORDER BY (SELECT count(*) FROM show_subscriptions x WHERE x.show_id = s.id) DESC, s.title, s.id
		LIMIT $3 OFFSET $4`, userID, pattern, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Show{}
	for rows.Next() {
		sh, err := scanShow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sh)
	}
	return out, rows.Err()
}

// ListSubscriptions returns the user's shows, most recently updated first.
func (s *Store) ListSubscriptions(ctx context.Context, userID string, limit, offset int) ([]*Show, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT s.id, s.title, s.author, s.description, s.image_url, s.website_url, s.language, s.explicit,
		       true,
		       (SELECT count(*) FROM episodes e WHERE e.show_id = s.id),
		       (SELECT max(e.published_at) FROM episodes e WHERE e.show_id = s.id),
		       (SELECT count(*) FROM (
		            SELECT 1 FROM episodes e
		            WHERE e.show_id = s.id
		              AND NOT EXISTS (SELECT 1 FROM episode_progress p
		                              WHERE p.user_id = $1 AND p.episode_id = e.id AND p.completed)
		            LIMIT 100) u),
		       ss.added_at
		FROM show_subscriptions ss
		JOIN shows s ON s.id = ss.show_id
		WHERE ss.user_id = $1
		ORDER BY (SELECT max(e.published_at) FROM episodes e WHERE e.show_id = s.id) DESC NULLS LAST, s.id
		LIMIT $2 OFFSET $3`, userID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Show{}
	for rows.Next() {
		var sh Show
		var unplayed int
		var added time.Time
		if err := rows.Scan(&sh.ID, &sh.Title, &sh.Author, &sh.Description, &sh.ImageURL, &sh.WebsiteURL, &sh.Language,
			&sh.Explicit, &sh.Subscribed, &sh.EpisodeCount, &sh.LatestEpisode, &unplayed, &added); err != nil {
			return nil, err
		}
		sh.UnplayedCount, sh.SubscribedAt = &unplayed, &added
		out = append(out, &sh)
	}
	return out, rows.Err()
}

const episodeSelect = `
	SELECT e.id, e.show_id, s.title, e.title, e.description, e.published_at, e.duration_ms, e.size_bytes,
	       COALESCE(NULLIF(e.image_url, ''), s.image_url), e.explicit, e.season, e.episode_number, e.episode_type,
	       p.position_ms, p.completed, p.updated_at
	FROM episodes e
	JOIN shows s ON s.id = e.show_id
	LEFT JOIN episode_progress p ON p.episode_id = e.id AND p.user_id = $1`

func scanEpisode(row pgx.Row) (*EpisodeView, error) {
	var e EpisodeView
	var pos *int
	var done *bool
	var upd *time.Time
	err := row.Scan(&e.ID, &e.ShowID, &e.ShowTitle, &e.Title, &e.Description, &e.PublishedAt, &e.DurationMs, &e.SizeBytes,
		&e.ImageURL, &e.Explicit, &e.Season, &e.EpisodeNumber, &e.EpisodeType, &pos, &done, &upd)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	e.Source = "podcast"
	if pos != nil {
		e.Progress = &Progress{PositionMs: *pos, Completed: *done, UpdatedAt: *upd}
	}
	return &e, nil
}

// GetEpisode returns one episode with the user's progress.
func (s *Store) GetEpisode(ctx context.Context, id, userID string) (*EpisodeView, error) {
	return scanEpisode(s.pool.QueryRow(ctx, episodeSelect+` WHERE e.id = $2`, userID, id))
}

// ListEpisodes returns a show's episodes, newest first unless oldestFirst.
func (s *Store) ListEpisodes(ctx context.Context, showID, userID string, oldestFirst bool, limit, offset int) ([]*EpisodeView, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM shows WHERE id = $1)`, showID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	order := "e.published_at DESC, e.id"
	if oldestFirst {
		order = "e.published_at ASC, e.id"
	}
	rows, err := s.pool.Query(ctx, episodeSelect+` WHERE e.show_id = $2 ORDER BY `+order+` LIMIT $3 OFFSET $4`,
		userID, showID, limit, offset)
	if err != nil {
		return nil, err
	}
	return collectEpisodes(rows)
}

// InProgress returns episodes the user started and has not finished, latest first.
func (s *Store) InProgress(ctx context.Context, userID string, limit int) ([]*EpisodeView, error) {
	rows, err := s.pool.Query(ctx, episodeSelect+`
		WHERE p.user_id = $1 AND NOT p.completed AND p.position_ms > 0
		ORDER BY p.updated_at DESC LIMIT $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	return collectEpisodes(rows)
}

func collectEpisodes(rows pgx.Rows) ([]*EpisodeView, error) {
	defer rows.Close()
	out := []*EpisodeView{}
	for rows.Next() {
		e, err := scanEpisode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// -------------------------------------------------------------- user state

// Subscribe follows a show. Doing it twice is fine. A show that has not been
// refreshed for a while is queued for an immediate refresh.
func (s *Store) Subscribe(ctx context.Context, userID, showID string) error {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM show_subscriptions WHERE user_id = $1`, userID).Scan(&n); err != nil {
		return err
	}
	if n >= MaxSubscriptions {
		var already bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM show_subscriptions WHERE user_id = $1 AND show_id = $2)`, userID, showID).Scan(&already); err != nil {
			return err
		}
		if !already {
			return ErrLimit
		}
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO show_subscriptions (user_id, show_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, userID, showID)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	// Subscribing to a stale show should not wait for its next scheduled refresh.
	_, err = s.pool.Exec(ctx, `
		UPDATE shows SET next_fetch_at = now()
		WHERE id = $1 AND next_fetch_at > now() AND last_fetched_at < now() - make_interval(secs => $2)`,
		showID, s.interval.Seconds())
	return err
}

// Unsubscribe unfollows a show (idempotent). Progress is kept.
func (s *Store) Unsubscribe(ctx context.Context, userID, showID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM show_subscriptions WHERE user_id = $1 AND show_id = $2`, userID, showID)
	return err
}

// ProgressUpdate is a client's report of where it is in an episode.
type ProgressUpdate struct {
	PositionMs int
	DurationMs *int  // the player's idea of the length, used when the feed gave none
	Completed  *bool // explicit "mark played / unplayed"
}

// SetProgress stores the user's position. The episode counts as played when the
// client says so, or when playback gets within 20 seconds of the end; starting
// it again from earlier clears the flag.
func (s *Store) SetProgress(ctx context.Context, userID, episodeID string, u ProgressUpdate) error {
	var knownDur int
	err := s.pool.QueryRow(ctx, `SELECT duration_ms FROM episodes WHERE id = $1`, episodeID).Scan(&knownDur)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}

	dur := knownDur
	if dur == 0 && u.DurationMs != nil {
		dur = *u.DurationMs
	}
	completed := dur > 0 && u.PositionMs >= dur-completionSlackMs
	if u.Completed != nil {
		completed = *u.Completed
	}

	_, err = s.pool.Exec(ctx, `
		INSERT INTO episode_progress (user_id, episode_id, position_ms, completed, updated_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (user_id, episode_id) DO UPDATE
		SET position_ms = EXCLUDED.position_ms, completed = EXCLUDED.completed, updated_at = now()`,
		userID, episodeID, u.PositionMs, completed)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		return ErrNotFound
	}
	return err
}

func escapeLike(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\', '%', '_':
			out = append(out, '\\')
		}
		out = append(out, s[i])
	}
	return string(out)
}
