// Package catalog serves track metadata, aggregated search and the browse
// lists behind the home screen.
package catalog

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/auth"
	genrelist "github.com/blacmusic/backend/internal/genre"
	"github.com/blacmusic/backend/internal/httpx"
	"github.com/blacmusic/backend/internal/image"
	"github.com/blacmusic/backend/internal/source"
	"github.com/blacmusic/backend/internal/trackview"
)

// Handler serves catalog endpoints.
type Handler struct {
	pool      *pgxpool.Pool
	reg       *source.Registry
	publicURL string // API base URL, for picture links
}

// NewHandler creates the catalog handler.
func NewHandler(pool *pgxpool.Pool, reg *source.Registry, publicURL string) *Handler {
	return &Handler{pool: pool, reg: reg, publicURL: publicURL}
}

// Search fans out to every search-capable provider (optionally filtered with
// ?sources=native,audius) and concatenates the results.
func (h *Handler) Search(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "q is required")
		return
	}
	limit := intParam(r, "limit", 20, 1, 50)
	offset := intParam(r, "offset", 0, 0, 10000)

	var only map[string]bool
	if s := r.URL.Query().Get("sources"); s != "" {
		only = map[string]bool{}
		for _, name := range strings.Split(s, ",") {
			only[strings.TrimSpace(name)] = true
		}
	}

	tracks := []source.Track{}
	for _, p := range h.reg.All() {
		if only != nil && !only[p.Name()] {
			continue
		}
		if !source.Has(p, source.CapSearch) {
			continue
		}
		res, err := p.Search(r.Context(), q, limit, offset)
		if err != nil {
			slog.Warn("provider search failed", "source", p.Name(), "err", err)
			continue
		}
		tracks = append(tracks, res...)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"tracks": tracks})
}

const publicNativeTrack = `t.visibility = 'public'
	  AND EXISTS (SELECT 1 FROM track_sources ts WHERE ts.track_id = t.id AND ts.source = 'native')`

const savesColumn = `(SELECT count(*) FROM library_tracks l WHERE l.track_id = t.id) AS saves`

// trackOrder is the ORDER BY for a track list: "new" is by when the release
// went live; "popular" is by how many listeners saved the track (real play
// counts will replace this once play events exist), newest first among equals.
func trackOrder(sort string) (string, bool) {
	newest := "COALESCE(al.published_at, t.created_at) DESC, t.id"
	switch sort {
	case "", "new":
		return newest, true
	case "popular":
		return "saves DESC, " + newest, true
	}
	return "", false
}

// queryTracks runs a track list query. where may use $1..$len(args).
func (h *Handler) queryTracks(ctx context.Context, where string, args []any, order string, limit, offset int) ([]source.Track, error) {
	n := len(args)
	q := trackview.Select(savesColumn) + " WHERE " + where + " ORDER BY " + order +
		" LIMIT $" + strconv.Itoa(n+1) + " OFFSET $" + strconv.Itoa(n+2)
	rows, err := h.pool.Query(ctx, q, append(args, limit, offset)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tracks := []source.Track{}
	for rows.Next() {
		var saves int
		t, err := trackview.Scan(rows, h.publicURL, &saves)
		if err != nil {
			return nil, err
		}
		tracks = append(tracks, t)
	}
	return tracks, rows.Err()
}

func internalError(w http.ResponseWriter, what string, err error) {
	slog.Error(what, "err", err)
	httpx.Error(w, http.StatusInternalServerError, "internal", "something went wrong")
}

// Browse lists public tracks for the Home screen, in the same shape as search.
//
//	GET /v1/browse/tracks?sort=new|popular&genre=&limit=&offset=
func (h *Handler) Browse(w http.ResponseWriter, r *http.Request) {
	order, ok := trackOrder(r.URL.Query().Get("sort"))
	if !ok {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "sort must be new or popular")
		return
	}
	limit := intParam(r, "limit", 20, 1, 50)
	offset := intParam(r, "offset", 0, 0, 10000)
	genreFilter := strings.TrimSpace(r.URL.Query().Get("genre"))
	if g, ok := genrelist.Lookup(genreFilter); ok { // accept an id ("hip-hop-rap") or any capitalisation
		genreFilter = g.Name
	}
	tracks, err := h.queryTracks(r.Context(),
		publicNativeTrack+` AND ($1 = '' OR lower(t.genre) = lower($1) OR lower(al.genre) = lower($1))`,
		[]any{genreFilter}, order, limit, offset)
	if err != nil {
		internalError(w, "browse tracks", err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"tracks": tracks})
}

// ArtistTracks lists an artist's public tracks (the "Popular" list of the artist page).
//
//	GET /v1/artists/{id}/tracks?sort=popular|new&limit=&offset=
//
// The default is "popular".
func (h *Handler) ArtistTracks(w http.ResponseWriter, r *http.Request) {
	sort := r.URL.Query().Get("sort")
	if sort == "" {
		sort = "popular"
	}
	order, ok := trackOrder(sort)
	if !ok {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "sort must be popular or new")
		return
	}
	id := chi.URLParam(r, "id")
	var exists bool
	if err := h.pool.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM artists WHERE id = $1)`, id).Scan(&exists); err != nil {
		internalError(w, "artist tracks", err)
		return
	}
	if !exists {
		httpx.Error(w, http.StatusNotFound, "not_found", "artist not found")
		return
	}
	tracks, err := h.queryTracks(r.Context(), publicNativeTrack+` AND t.artist_id = $1`, []any{id}, order,
		intParam(r, "limit", 20, 1, 50), intParam(r, "offset", 0, 0, 10000))
	if err != nil {
		internalError(w, "artist tracks", err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"tracks": tracks})
}

type browsedArtist struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ImageURL      string `json:"imageUrl,omitempty"`
	ImageLargeURL string `json:"imageLargeUrl,omitempty"`
	Verified      bool   `json:"verified"`
	FollowerCount int    `json:"followerCount"`
	ReleaseCount  int    `json:"releaseCount"`
}

// publicRelease is true for an album that is live and has something to play.
const publicRelease = `al.artist_id = a.id AND al.visibility = 'public'
	AND EXISTS (SELECT 1 FROM tracks t WHERE t.album_id = al.id AND t.visibility = 'public')`

// BrowseArtists lists artists that have at least one public release.
//
//	GET /v1/browse/artists?sort=featured|new&limit=&offset=
//
// "featured": verified artists first, then the most followed, then the newest
// account. "new": the artist whose latest release went live most recently first.
func (h *Handler) BrowseArtists(w http.ResponseWriter, r *http.Request) {
	var order string
	switch r.URL.Query().Get("sort") {
	case "", "featured":
		order = "(a.verification = 'verified') DESC, followers DESC, a.created_at DESC, a.id"
	case "new":
		order = `(SELECT max(al.published_at) FROM albums al WHERE ` + publicRelease + `) DESC NULLS LAST, a.created_at DESC, a.id`
	default:
		httpx.Error(w, http.StatusBadRequest, "bad_request", "sort must be featured or new")
		return
	}
	rows, err := h.pool.Query(r.Context(), `
		SELECT a.id, a.name, COALESCE(u.avatar_image_id, a.avatar_image_id), COALESCE(a.image_url, ''), a.verification = 'verified',
		       (SELECT count(*) FROM library_artists l WHERE l.artist_id = a.id) AS followers,
		       (SELECT count(*) FROM albums al WHERE `+publicRelease+`) AS releases
		FROM artists a LEFT JOIN users u ON u.id = a.owner_user_id
		WHERE EXISTS (SELECT 1 FROM albums al WHERE `+publicRelease+`)
		ORDER BY `+order+`
		LIMIT $1 OFFSET $2`, intParam(r, "limit", 20, 1, 50), intParam(r, "offset", 0, 0, 10000))
	if err != nil {
		internalError(w, "browse artists", err)
		return
	}
	defer rows.Close()
	artists := []browsedArtist{}
	for rows.Next() {
		var a browsedArtist
		var avatarID *string
		var legacy string
		if err := rows.Scan(&a.ID, &a.Name, &avatarID, &legacy, &a.Verified, &a.FollowerCount, &a.ReleaseCount); err != nil {
			internalError(w, "browse artists", err)
			return
		}
		a.ImageURL = image.PersonURL(h.publicURL, avatarID, legacy)
		a.ImageLargeURL = image.PersonLargeURL(h.publicURL, avatarID)
		artists = append(artists, a)
	}
	if err := rows.Err(); err != nil {
		internalError(w, "browse artists", err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"artists": artists})
}

// SearchArtists finds artists with at least one public release by name.
//
//	GET /v1/search/artists?q=&limit=&offset=
//
// Names equal to q come first, then names starting with q, then a word starting with q, then names
// containing q; ties go to verified artists, then the most followed.
func (h *Handler) SearchArtists(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "q is required")
		return
	}
	esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q)
	rows, err := h.pool.Query(r.Context(), `
		SELECT a.id, a.name, COALESCE(u.avatar_image_id, a.avatar_image_id), COALESCE(a.image_url, ''), a.verification = 'verified',
		       (SELECT count(*) FROM library_artists l WHERE l.artist_id = a.id) AS followers,
		       (SELECT count(*) FROM albums al WHERE `+publicRelease+`) AS releases
		FROM artists a LEFT JOIN users u ON u.id = a.owner_user_id
		WHERE a.name ILIKE $4 AND EXISTS (SELECT 1 FROM albums al WHERE `+publicRelease+`)
		ORDER BY CASE WHEN lower(a.name) = lower($1) THEN 0 WHEN a.name ILIKE $2 THEN 1 WHEN a.name ILIKE $3 THEN 2 ELSE 3 END,
		         (a.verification = 'verified') DESC, followers DESC, a.name, a.id
		LIMIT $5 OFFSET $6`, q, esc+"%", "% "+esc+"%", "%"+esc+"%",
		intParam(r, "limit", 20, 1, 50), intParam(r, "offset", 0, 0, 10000))
	if err != nil {
		internalError(w, "search artists", err)
		return
	}
	defer rows.Close()
	artists := []browsedArtist{}
	for rows.Next() {
		var a browsedArtist
		var avatarID *string
		var legacy string
		if err := rows.Scan(&a.ID, &a.Name, &avatarID, &legacy, &a.Verified, &a.FollowerCount, &a.ReleaseCount); err != nil {
			internalError(w, "search artists", err)
			return
		}
		a.ImageURL = image.PersonURL(h.publicURL, avatarID, legacy)
		a.ImageLargeURL = image.PersonLargeURL(h.publicURL, avatarID)
		artists = append(artists, a)
	}
	if err := rows.Err(); err != nil {
		internalError(w, "search artists", err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"artists": artists})
}

// RelatedArtists suggests artists similar to one: those with public releases in the same genres, most shared
// genres first; artists with a similar number of followers, then verified and more followed ones, come
// first among equals, and artists sharing no genre fill the rest. Never the artist itself.
//
//	GET /v1/artists/{id}/related?limit=   (default 12, at most 50)
func (h *Handler) RelatedArtists(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var genres []string
	var followers int
	err := h.pool.QueryRow(r.Context(), `
		SELECT COALESCE((SELECT array_agg(DISTINCT g) FROM (
		           SELECT lower(al.genre) AS g FROM albums al WHERE al.artist_id = a.id AND al.visibility = 'public'
		           UNION SELECT lower(t.genre) FROM tracks t WHERE t.artist_id = a.id AND t.visibility = 'public') x WHERE g <> ''), '{}'),
		       (SELECT count(*) FROM library_artists l WHERE l.artist_id = a.id)
		FROM artists a WHERE a.id = $1`, id).Scan(&genres, &followers)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, http.StatusNotFound, "not_found", "artist not found")
		return
	}
	if err != nil {
		internalError(w, "related artists", err)
		return
	}
	rows, err := h.pool.Query(r.Context(), `
		SELECT a.id, a.name, COALESCE(u.avatar_image_id, a.avatar_image_id), COALESCE(a.image_url, ''), a.verification = 'verified',
		       (SELECT count(*) FROM library_artists l WHERE l.artist_id = a.id) AS followers,
		       (SELECT count(*) FROM albums al WHERE `+publicRelease+`) AS releases,
		       (SELECT count(*) FROM (
		            SELECT lower(al.genre) AS g FROM albums al WHERE al.artist_id = a.id AND al.visibility = 'public'
		            UNION SELECT lower(t.genre) FROM tracks t WHERE t.artist_id = a.id AND t.visibility = 'public') og
		         WHERE og.g <> '' AND og.g = ANY($2::text[])) AS overlap
		FROM artists a LEFT JOIN users u ON u.id = a.owner_user_id
		WHERE a.id <> $1 AND EXISTS (SELECT 1 FROM albums al WHERE `+publicRelease+`)
		ORDER BY overlap DESC, abs((SELECT count(*) FROM library_artists l WHERE l.artist_id = a.id) - $3::int),
		         (a.verification = 'verified') DESC, followers DESC, a.name, a.id
		LIMIT $4`, id, genres, followers, intParam(r, "limit", 12, 1, 50))
	if err != nil {
		internalError(w, "related artists", err)
		return
	}
	defer rows.Close()
	artists := []browsedArtist{}
	for rows.Next() {
		var a browsedArtist
		var avatarID *string
		var legacy string
		var overlap int
		if err := rows.Scan(&a.ID, &a.Name, &avatarID, &legacy, &a.Verified, &a.FollowerCount, &a.ReleaseCount, &overlap); err != nil {
			internalError(w, "related artists", err)
			return
		}
		a.ImageURL = image.PersonURL(h.publicURL, avatarID, legacy)
		a.ImageLargeURL = image.PersonLargeURL(h.publicURL, avatarID)
		artists = append(artists, a)
	}
	if err := rows.Err(); err != nil {
		internalError(w, "related artists", err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"artists": artists})
}

// GetTrack returns a canonical track by Blac Music id. Drafts are visible to the
// artist who owns them, and to nobody else.
func (h *Handler) GetTrack(w http.ResponseWriter, r *http.Request) {
	uid, _ := auth.UserID(r.Context())
	t, err := trackview.Scan(h.pool.QueryRow(r.Context(),
		trackview.Select("")+` WHERE t.id = $1 AND (t.visibility = 'public' OR a.owner_user_id::text = $2)`,
		chi.URLParam(r, "id"), uid), h.publicURL)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, http.StatusNotFound, "not_found", "track not found")
		return
	}
	if err != nil {
		internalError(w, "get track", err)
		return
	}
	httpx.JSON(w, http.StatusOK, t)
}

func intParam(r *http.Request, key string, def, min, max int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < min {
		return def
	}
	if n > max {
		return max
	}
	return n
}
