package admin

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/blacmusic/backend/internal/httpx"
)

func (h *Handler) statsRoutes(r chi.Router) {
	r.Get("/stats/overview", h.overview)
	r.Get("/stats/timeseries", h.timeseries)
	r.Get("/stats/top", h.top)
}

// GET /v1/admin/stats/overview
func (h *Handler) overview(w http.ResponseWriter, r *http.Request) {
	var o struct {
		Users    map[string]int `json:"users"`
		Artists  map[string]int `json:"artists"`
		Releases map[string]int `json:"releases"`
		Tracks   map[string]int `json:"tracks"`
		Shows    map[string]int `json:"shows"`
		Plays    map[string]int `json:"plays"`
		Storage  int64          `json:"storageBytes"`
	}
	var u, uv, ub, u7, u30, a, av, ap, ab, rt, rp, rd, rs, rtd, tr, sh, pt, p7, p30, pu int
	err := h.pool.QueryRow(r.Context(), `
		SELECT (SELECT count(*) FROM users), (SELECT count(*) FROM users WHERE email_verified_at IS NOT NULL),
		       (SELECT count(*) FROM users WHERE banned_at IS NOT NULL),
		       (SELECT count(*) FROM users WHERE created_at >= now() - interval '7 days'),
		       (SELECT count(*) FROM users WHERE created_at >= now() - interval '30 days'),
		       (SELECT count(*) FROM artists), (SELECT count(*) FROM artists WHERE verification = 'verified'),
		       (SELECT count(*) FROM artists WHERE verification = 'pending'), (SELECT count(*) FROM artists WHERE banned_at IS NOT NULL),
		       (SELECT count(*) FROM albums), (SELECT count(*) FROM albums WHERE visibility = 'public'),
		       (SELECT count(*) FROM albums WHERE visibility = 'draft' AND taken_down_at IS NULL),
		       (SELECT count(*) FROM albums WHERE visibility = 'scheduled'),
		       (SELECT count(*) FROM albums WHERE taken_down_at IS NOT NULL),
		       (SELECT count(*) FROM tracks), (SELECT count(*) FROM shows),
		       (SELECT count(*) FROM plays), (SELECT count(*) FROM plays WHERE at >= now() - interval '7 days'),
		       (SELECT count(*) FROM plays WHERE at >= now() - interval '30 days'),
		       (SELECT count(DISTINCT user_id) FROM plays WHERE at >= now() - interval '30 days'),
		       (SELECT COALESCE(sum(size_bytes), 0) FROM uploads WHERE status <> 'failed')`).
		Scan(&u, &uv, &ub, &u7, &u30, &a, &av, &ap, &ab, &rt, &rp, &rd, &rs, &rtd, &tr, &sh, &pt, &p7, &p30, &pu, &o.Storage)
	if err != nil {
		reply(w, err)
		return
	}
	o.Users = map[string]int{"total": u, "verifiedEmail": uv, "banned": ub, "new7d": u7, "new30d": u30}
	o.Artists = map[string]int{"total": a, "verified": av, "pendingVerification": ap, "banned": ab}
	o.Releases = map[string]int{"total": rt, "public": rp, "draft": rd, "scheduled": rs, "takenDown": rtd}
	o.Tracks = map[string]int{"total": tr}
	o.Shows = map[string]int{"total": sh}
	o.Plays = map[string]int{"total": pt, "last7d": p7, "last30d": p30, "uniqueListeners30d": pu}
	httpx.JSON(w, http.StatusOK, o)
}

// GET /v1/admin/stats/timeseries?metric=signups|artists|releases|plays&from=YYYY-MM-DD&to=YYYY-MM-DD&bucket=day|week
func (h *Handler) timeseries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sources := map[string][3]string{ // table, time column, extra condition
		"signups": {"users", "created_at", "true"}, "artists": {"artists", "created_at", "true"},
		"releases": {"albums", "created_at", "true"}, "plays": {"plays", "at", "true"},
		"searches": {"search_queries", "at", "true"}, "searches_no_results": {"search_queries", "at", "results = 0"},
	}
	src, ok := sources[q.Get("metric")]
	if !ok {
		reply(w, fail4(http.StatusBadRequest, "bad_request", "metric must be signups, artists, releases, plays, searches or searches_no_results"))
		return
	}
	bucket := q.Get("bucket")
	switch bucket {
	case "":
		bucket = "day"
	case "day", "week":
	default:
		reply(w, fail4(http.StatusBadRequest, "bad_request", "bucket must be day or week"))
		return
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	from, to := today.AddDate(0, 0, -29), today
	var err error
	if v := q.Get("from"); v != "" {
		if from, err = time.Parse("2006-01-02", v); err != nil {
			reply(w, fail4(http.StatusBadRequest, "bad_request", "from must look like 2026-11-01"))
			return
		}
	}
	if v := q.Get("to"); v != "" {
		if to, err = time.Parse("2006-01-02", v); err != nil {
			reply(w, fail4(http.StatusBadRequest, "bad_request", "to must look like 2026-11-30"))
			return
		}
	}
	if to.Before(from) {
		reply(w, fail4(http.StatusBadRequest, "bad_request", "to must not be before from"))
		return
	}
	if to.Sub(from) > 366*24*time.Hour {
		reply(w, fail4(http.StatusBadRequest, "bad_request", "the range is at most 366 days"))
		return
	}
	rows, err := h.pool.Query(r.Context(), `
		WITH s AS (SELECT generate_series(date_trunc($3, $1::timestamp), date_trunc($3, $2::timestamp), ('1 ' || $3)::interval) AS d)
		SELECT s.d::date, count(x.t)
		FROM s LEFT JOIN (SELECT `+src[1]+` AT TIME ZONE 'UTC' AS t FROM `+src[0]+`
		                  WHERE `+src[2]+` AND `+src[1]+` >= $1::timestamp AT TIME ZONE 'UTC' AND `+src[1]+` < ($2::timestamp + interval '1 day') AT TIME ZONE 'UTC') x
		  ON date_trunc($3, x.t) = s.d
		GROUP BY s.d ORDER BY s.d`, from, to, bucket)
	if err != nil {
		reply(w, err)
		return
	}
	defer rows.Close()
	type point struct {
		Date  string `json:"date"`
		Count int    `json:"count"`
	}
	points := []point{}
	for rows.Next() {
		var d time.Time
		var c int
		if err := rows.Scan(&d, &c); err != nil {
			reply(w, err)
			return
		}
		points = append(points, point{d.Format("2006-01-02"), c})
	}
	if err := rows.Err(); err != nil {
		reply(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"points": points})
}

// GET /v1/admin/stats/top?kind=artists|tracks&range=7d|30d|90d&limit=10
func (h *Handler) top(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	days := map[string]int{"": 30, "7d": 7, "30d": 30, "90d": 90}[q.Get("range")]
	if days == 0 {
		reply(w, fail4(http.StatusBadRequest, "bad_request", "range must be 7d, 30d or 90d"))
		return
	}
	limit := 10
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		limit = min(n, 50)
	}
	var sql string
	switch q.Get("kind") {
	case "artists":
		sql = `SELECT a.id, a.name, count(*) AS n FROM plays p JOIN artists a ON a.id = p.artist_id
		       WHERE p.at >= now() - make_interval(days => $1) GROUP BY a.id, a.name ORDER BY n DESC, a.name, a.id LIMIT $2`
	case "tracks":
		sql = `SELECT t.id, t.title, count(*) AS n FROM plays p JOIN tracks t ON t.id = p.track_id
		       WHERE p.at >= now() - make_interval(days => $1) GROUP BY t.id, t.title ORDER BY n DESC, t.title, t.id LIMIT $2`
	default:
		reply(w, fail4(http.StatusBadRequest, "bad_request", "kind must be artists or tracks"))
		return
	}
	rows, err := h.pool.Query(r.Context(), sql, days, limit)
	if err != nil {
		reply(w, err)
		return
	}
	defer rows.Close()
	type item struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Plays int    `json:"plays"`
	}
	items := []item{}
	for rows.Next() {
		var i item
		if err := rows.Scan(&i.ID, &i.Name, &i.Plays); err != nil {
			reply(w, err)
			return
		}
		items = append(items, i)
	}
	if err := rows.Err(); err != nil {
		reply(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
}
