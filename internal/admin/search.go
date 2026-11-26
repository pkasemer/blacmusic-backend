package admin

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/blacmusic/backend/internal/httpx"
)

func (h *Handler) searchRoutes(r chi.Router) {
	r.Get("/search/overview", h.searchOverview)
	r.Get("/search/top", h.searchTop)
	r.Get("/search/no-results", h.searchNoResults)
	r.Get("/search/kinds", h.searchKinds)
}

// searchWindow reads range=7d|30d|90d (default 30d) and gives the current period and the one right before it.
func searchWindow(r *http.Request) (cur, prev time.Time, err error) {
	days := map[string]int{"": 30, "7d": 7, "30d": 30, "90d": 90}[r.URL.Query().Get("range")]
	if days == 0 {
		return cur, prev, fail4(http.StatusBadRequest, "bad_request", "range must be 7d, 30d or 90d")
	}
	now := time.Now().UTC()
	return now.AddDate(0, 0, -days), now.AddDate(0, 0, -2*days), nil
}

func searchKind(r *http.Request) (string, error) {
	k := r.URL.Query().Get("kind")
	switch k {
	case "", "tracks", "artists", "playlists", "albums":
		return k, nil
	}
	return "", fail4(http.StatusBadRequest, "bad_request", "kind must be tracks, artists, playlists or albums")
}

// GET /v1/admin/search/overview?range=7d|30d|90d
func (h *Handler) searchOverview(w http.ResponseWriter, r *http.Request) {
	cur, prev, err := searchWindow(r)
	if err != nil {
		reply(w, err)
		return
	}
	var searches, unique, noRes, pSearches, pNoRes int
	err = h.pool.QueryRow(r.Context(), `
		SELECT count(*) FILTER (WHERE at >= $1), count(DISTINCT query) FILTER (WHERE at >= $1),
		       count(*) FILTER (WHERE at >= $1 AND results = 0),
		       count(*) FILTER (WHERE at < $1), count(*) FILTER (WHERE at < $1 AND results = 0)
		FROM search_queries WHERE at >= $2`, cur, prev).Scan(&searches, &unique, &noRes, &pSearches, &pNoRes)
	if err != nil {
		reply(w, err)
		return
	}
	rate := 0.0
	if searches > 0 {
		rate = float64(int(float64(noRes)/float64(searches)*1000+0.5)) / 1000
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"searches": searches, "uniqueQueries": unique, "noResultSearches": noRes, "noResultRate": rate,
		"previous": map[string]int{"searches": pSearches, "noResultSearches": pNoRes},
	})
}

// GET /v1/admin/search/top?range=&kind=&q=&sort=count|rising&limit=&offset=
// sort=rising lists queries searched at least 3 times, ordered by growth against the period before.
func (h *Handler) searchTop(w http.ResponseWriter, r *http.Request) {
	cur, prev, err := searchWindow(r)
	if err != nil {
		reply(w, err)
		return
	}
	kind, err := searchKind(r)
	if err != nil {
		reply(w, err)
		return
	}
	sort := r.URL.Query().Get("sort")
	order := `cur.c DESC, cur.query`
	switch sort {
	case "", "count":
	case "rising":
		order = `cur.c::float / GREATEST(COALESCE(prev.c, 0), 1) DESC, cur.c DESC, cur.query`
	default:
		reply(w, fail4(http.StatusBadRequest, "bad_request", "sort must be count or rising"))
		return
	}
	limit, offset := page(r)
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	rows, err := h.pool.Query(r.Context(), `
		WITH cur AS (
		    SELECT query, count(*) AS c, count(*) FILTER (WHERE results = 0) AS nr, max(at) AS last
		    FROM search_queries WHERE at >= $1 AND ($3 = '' OR kind = $3) AND ($4 = '' OR query ILIKE $5) GROUP BY query),
		     prev AS (
		    SELECT query, count(*) AS c FROM search_queries WHERE at >= $2 AND at < $1 AND ($3 = '' OR kind = $3) GROUP BY query)
		SELECT cur.query, cur.c, cur.nr, COALESCE(prev.c, 0), cur.last, count(*) OVER ()
		FROM cur LEFT JOIN prev ON prev.query = cur.query
		WHERE $6 <> 'rising' OR cur.c >= 3
		ORDER BY `+order+` LIMIT $7 OFFSET $8`, cur, prev, kind, q, like(q), sort, limit, offset)
	if err != nil {
		reply(w, err)
		return
	}
	defer rows.Close()
	type item struct {
		Query         string    `json:"query"`
		Count         int       `json:"count"`
		NoResultCount int       `json:"noResultCount"`
		PreviousCount int       `json:"previousCount"`
		LastAt        time.Time `json:"lastAt"`
	}
	items, total := []item{}, 0
	for rows.Next() {
		var x item
		if err := rows.Scan(&x.Query, &x.Count, &x.NoResultCount, &x.PreviousCount, &x.LastAt, &total); err != nil {
			reply(w, err)
			return
		}
		items = append(items, x)
	}
	if err := rows.Err(); err != nil {
		reply(w, err)
		return
	}
	if len(items) == 0 && offset > 0 { // a page past the end: still say how many there are
		_ = h.pool.QueryRow(r.Context(), `SELECT count(DISTINCT query) FROM search_queries WHERE at >= $1 AND ($2 = '' OR kind = $2) AND ($3 = '' OR query ILIKE $4)`,
			cur, kind, q, like(q)).Scan(&total)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items, "total": total})
}

// GET /v1/admin/search/no-results?range=&kind=&limit=&offset=: queries whose first page was empty, most frequent first.
func (h *Handler) searchNoResults(w http.ResponseWriter, r *http.Request) {
	cur, _, err := searchWindow(r)
	if err != nil {
		reply(w, err)
		return
	}
	kind, err := searchKind(r)
	if err != nil {
		reply(w, err)
		return
	}
	limit, offset := page(r)
	rows, err := h.pool.Query(r.Context(), `
		SELECT query, count(*) AS c, max(at), count(*) OVER ()
		FROM search_queries WHERE at >= $1 AND results = 0 AND ($2 = '' OR kind = $2)
		GROUP BY query ORDER BY c DESC, query LIMIT $3 OFFSET $4`, cur, kind, limit, offset)
	if err != nil {
		reply(w, err)
		return
	}
	defer rows.Close()
	type item struct {
		Query  string    `json:"query"`
		Count  int       `json:"count"`
		LastAt time.Time `json:"lastAt"`
	}
	items, total := []item{}, 0
	for rows.Next() {
		var x item
		if err := rows.Scan(&x.Query, &x.Count, &x.LastAt, &total); err != nil {
			reply(w, err)
			return
		}
		items = append(items, x)
	}
	if err := rows.Err(); err != nil {
		reply(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items, "total": total})
}

// GET /v1/admin/search/kinds?range=: how many searches were for tracks, artists, playlists and albums.
func (h *Handler) searchKinds(w http.ResponseWriter, r *http.Request) {
	cur, _, err := searchWindow(r)
	if err != nil {
		reply(w, err)
		return
	}
	rows, err := h.pool.Query(r.Context(), `
		SELECT k.kind, count(s.id) FROM (VALUES ('tracks'), ('artists'), ('playlists'), ('albums')) k(kind)
		LEFT JOIN search_queries s ON s.kind = k.kind AND s.at >= $1 GROUP BY k.kind ORDER BY count(s.id) DESC, k.kind`, cur)
	if err != nil {
		reply(w, err)
		return
	}
	defer rows.Close()
	type item struct {
		Kind  string `json:"kind"`
		Count int    `json:"count"`
	}
	items := []item{}
	for rows.Next() {
		var x item
		if err := rows.Scan(&x.Kind, &x.Count); err != nil {
			reply(w, err)
			return
		}
		items = append(items, x)
	}
	if err := rows.Err(); err != nil {
		reply(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
}
