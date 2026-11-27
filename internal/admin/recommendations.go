package admin

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/blacmusic/backend/internal/httpx"
	"github.com/blacmusic/backend/internal/recommend"
)

// Looking into the recommendation engine: why a listener would be offered what, what people did with what they were
// offered, and how much of the catalogue says enough about itself to be matched. Read only.

// GET /v1/admin/users/{id}/recommendations?surface=your_sound|mix&seed=trk_...&limit=
// The list the engine would make for this listener now, with each song's reasons and score. Nothing is changed or
// recorded. A mix needs seed, the song it starts from.
func (h *Handler) explainRecommendations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var userID string
	err := h.pool.QueryRow(ctx, `SELECT id::text FROM users WHERE id::text = $1`, param(r, "id")).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		reply(w, errNotFound)
		return
	}
	if err != nil {
		reply(w, err)
		return
	}
	limit := 25
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = min(n, 50)
	}
	var res *recommend.Result
	switch surface := r.URL.Query().Get("surface"); surface {
	case "", "your_sound":
		var playlist string
		err = h.pool.QueryRow(ctx, `SELECT COALESCE((SELECT playlist_id FROM your_sound WHERE user_id = $1), '')`, userID).Scan(&playlist)
		if err == nil {
			res, err = recommend.ForSound(ctx, h.pool, userID, playlist, limit)
		}
	case "mix":
		res, err = recommend.Mix(ctx, h.pool, userID, r.URL.Query().Get("seed"), nil, limit, "explain")
		if errors.Is(err, recommend.ErrNoSong) {
			reply(w, fail4(http.StatusUnprocessableEntity, "validation_error", "seed must be a song people can play"))
			return
		}
	default:
		reply(w, fail4(http.StatusBadRequest, "bad_request", "surface must be your_sound or mix"))
		return
	}
	if err != nil {
		reply(w, err)
		return
	}
	ids := make([]string, len(res.Picks))
	for i, p := range res.Picks {
		ids[i] = p.TrackID
	}
	rows, err := h.pool.Query(ctx, `SELECT t.id, t.title, COALESCE(a.name, '') FROM tracks t LEFT JOIN artists a ON a.id = t.artist_id WHERE t.id = ANY($1)`, ids)
	if err != nil {
		reply(w, err)
		return
	}
	defer rows.Close()
	names := map[string][2]string{}
	for rows.Next() {
		var id, title, artist string
		if err := rows.Scan(&id, &title, &artist); err != nil {
			reply(w, err)
			return
		}
		names[id] = [2]string{title, artist}
	}
	if err := rows.Err(); err != nil {
		reply(w, err)
		return
	}
	type item struct {
		recommend.Pick
		Title      string `json:"title"`
		ArtistName string `json:"artistName"`
	}
	items := []item{}
	for _, p := range res.Picks {
		items = append(items, item{p, names[p.TrackID][0], names[p.TrackID][1]})
	}
	s := res.Profile.Stated
	list := func(v []string) []string {
		if v == nil {
			return []string{}
		}
		return v
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"algorithm": recommend.Version, "discoveryMode": res.Mode, "candidates": res.Candidates,
		"basis": map[string]any{
			"preferredLanguages": list(s.PreferredLanguages), "understoodLanguages": list(s.UnderstoodLanguages),
			"regions": list(s.Regions), "genres": list(s.Genres), "favouriteArtists": len(s.FavouriteArtists),
			"followedArtists": len(s.FollowedArtists), "songsWithHistory": len(res.Profile.History),
		},
		"items": items,
	})
}

// GET /v1/admin/recommendations/outcomes?days=14
// What listeners did with what they were suggested, staff and test accounts left out: by surface, version and kind of
// place. A suggestion counts once it is old enough to have been acted on or not, so recent ones lower the rates.
func (h *Handler) recommendationOutcomes(w http.ResponseWriter, r *http.Request) {
	days := 14
	if n, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && n > 0 {
		days = min(n, 90)
	}
	type row struct {
		Surface   string `json:"surface"`
		Algorithm string `json:"algorithm"`
		Slot      string `json:"slot"`
		Suggested int    `json:"suggested"`
		Listened  int    `json:"listened"`
		Completed int    `json:"completed"`
		Skipped   int    `json:"skipped"`
		Kept      int    `json:"kept"` // liked or added to a playlist
		Listeners int    `json:"listeners"`
	}
	rows, err := h.pool.Query(r.Context(), `
		SELECT surface, algorithm, slot, count(*), count(outcome), count(*) FILTER (WHERE outcome = 'completed'),
		       count(*) FILTER (WHERE outcome = 'skipped'), count(*) FILTER (WHERE kept), count(DISTINCT user_id)
		FROM rec_outcomes WHERE at > now() - make_interval(days => $1)
		GROUP BY surface, algorithm, slot ORDER BY surface, algorithm, slot`, days)
	if err != nil {
		reply(w, err)
		return
	}
	defer rows.Close()
	out := []row{}
	for rows.Next() {
		var x row
		if err := rows.Scan(&x.Surface, &x.Algorithm, &x.Slot, &x.Suggested, &x.Listened, &x.Completed, &x.Skipped, &x.Kept, &x.Listeners); err != nil {
			reply(w, err)
			return
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		reply(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"days": days, "rows": out})
}

// GET /v1/admin/recommendations/readiness
// How much there is to rank with: songs with each kind of verified metadata, and listeners with enough to go on.
func (h *Handler) recommendationReadiness(w http.ResponseWriter, r *http.Request) {
	var songs, genre, language, region, culture, sound, listeners, chose, withTen, excluded int
	err := h.pool.QueryRow(r.Context(), `
		SELECT count(*), count(*) FILTER (WHERE genre <> '' OR cardinality(genres) > 0),
		       count(*) FILTER (WHERE cardinality(primary_languages) + cardinality(other_languages) > 0),
		       count(*) FILTER (WHERE cardinality(regions) > 0), count(*) FILTER (WHERE cardinality(cultures) > 0),
		       count(*) FILTER (WHERE cardinality(attributes) > 0),
		       (SELECT count(*) FROM users u WHERE NOT EXISTS (SELECT 1 FROM signal_excluded_users x WHERE x.id = u.id)),
		       (SELECT count(*) FROM listener_preferences p WHERE cardinality(p.preferred_languages) + cardinality(p.genres) + cardinality(p.artist_ids) > 0
		           AND NOT EXISTS (SELECT 1 FROM signal_excluded_users x WHERE x.id = p.user_id)),
		       (SELECT count(*) FROM (SELECT user_id FROM signal_listens WHERE type = 'listen' AND at > now() - interval '180 days'
		           GROUP BY user_id HAVING count(DISTINCT track_id) >= 10) x),
		       (SELECT count(*) FROM signal_excluded_users)
		FROM rec_track_features`).
		Scan(&songs, &genre, &language, &region, &culture, &sound, &listeners, &chose, &withTen, &excluded)
	if err != nil {
		reply(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"algorithm": recommend.Version,
		"songs":     map[string]int{"playable": songs, "withGenre": genre, "withVerifiedLanguage": language, "withVerifiedRegion": region, "withVerifiedCulture": culture, "withVerifiedSound": sound},
		"listeners": map[string]int{"counted": listeners, "withChoices": chose, "heardTenSongs": withTen, "leftOut": excluded},
	})
}
