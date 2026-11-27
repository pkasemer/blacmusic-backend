package admin

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/blacmusic/backend/internal/httpx"
	"github.com/blacmusic/backend/internal/image"
)

// The preference graph of one listener: what they chose and what they did, never anything worked out about the
// person. Languages and regions come only from their own choices. It holds totals only, no history.

const (
	graphDays    = 90
	graphGenres  = 10 // genres added for activity, beyond the chosen ones
	graphArtists = 12 // artists added for activity
	graphPicked  = 20 // the most chosen artists, and the most followed ones, shown
)

type activity struct {
	Plays        int `json:"plays"`
	Finished     int `json:"finished"`
	Skipped      int `json:"skipped"`
	Replays      int `json:"replays"`
	Likes        int `json:"likes"` // likes less unlikes
	PlaylistAdds int `json:"playlistAdds"`
	SavedSongs   int `json:"savedSongs"`
	unlikes      int
}

func (a *activity) add(b activity) {
	a.Plays += b.Plays
	a.Finished += b.Finished
	a.Skipped += b.Skipped
	a.Replays += b.Replays
	a.Likes += b.Likes
	a.PlaylistAdds += b.PlaylistAdds
	a.SavedSongs += b.SavedSongs
	a.unlikes += b.unlikes
}

// interest is how much they took to it; skipping is not interest.
func (a activity) interest() int {
	return a.Plays + a.Finished + a.Replays + max(a.Likes-a.unlikes, 0) + a.PlaylistAdds + a.SavedSongs
}

func (a activity) shown() activity {
	a.Likes = max(a.Likes-a.unlikes, 0)
	return a
}

type graphNode struct {
	ID         string   `json:"id"`
	Type       string   `json:"type"`
	Label      string   `json:"label"`
	ImageURL   string   `json:"imageUrl,omitempty"`
	Chosen     bool     `json:"chosen"`
	Understood *bool    `json:"understood,omitempty"`
	Followed   *bool    `json:"followed,omitempty"`
	Activity   activity `json:"activity"`
}

type graphLink struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Kind   string `json:"kind"`
	Weight int    `json:"weight"`
}

// GET /v1/admin/users/{id}/preference-graph
func (h *Handler) preferenceGraph(w http.ResponseWriter, r *http.Request) {
	out, err := h.buildGraph(r.Context(), param(r, "id"))
	if err != nil {
		reply(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) buildGraph(ctx context.Context, userID string) (map[string]any, error) {
	var one int
	if err := h.pool.QueryRow(ctx, `SELECT 1 FROM users WHERE id::text = $1`, userID).Scan(&one); err != nil {
		return nil, err
	}
	var completed bool
	var updatedAt *time.Time
	mode := ""
	var preferred, understood, regions, genres, picked []string
	err := h.pool.QueryRow(ctx, `SELECT completed_at IS NOT NULL, updated_at, discovery_mode, preferred_languages, understood_languages, regions, genres, artist_ids
		FROM listener_preferences WHERE user_id::text = $1`, userID).Scan(&completed, &updatedAt, &mode, &preferred, &understood, &regions, &genres, &picked)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	// what they did, song by song
	type song struct {
		artist, genre string
		did           activity
	}
	rows, err := h.pool.Query(ctx, `
		WITH did AS (
			SELECT track_id, count(*) AS plays, 0 AS finished, 0 AS skipped, 0 AS replays, 0 AS likes, 0 AS unlikes, 0 AS adds, 0 AS saved
			FROM plays WHERE user_id::text = $1 AND at > now() - make_interval(days => $2) GROUP BY track_id
			UNION ALL
			SELECT track_id, 0, count(*) FILTER (WHERE type = 'listen' AND outcome = 'completed'),
			       count(*) FILTER (WHERE type = 'listen' AND outcome = 'skipped'), count(*) FILTER (WHERE type = 'listen' AND replay),
			       count(*) FILTER (WHERE type = 'like'), count(*) FILTER (WHERE type = 'unlike'), count(*) FILTER (WHERE type = 'playlist_add'), 0
			FROM listening_events WHERE user_id::text = $1 AND track_id <> '' AND at > now() - make_interval(days => $2) GROUP BY track_id
			UNION ALL
			SELECT track_id, 0, 0, 0, 0, 0, 0, 0, 1 FROM library_tracks WHERE user_id::text = $1
		)
		SELECT COALESCE(t.artist_id, ''), COALESCE(g.id, ''), sum(plays)::int, sum(finished)::int, sum(skipped)::int, sum(replays)::int,
		       sum(likes)::int, sum(unlikes)::int, sum(adds)::int, sum(saved)::int
		FROM did JOIN tracks t ON t.id = did.track_id LEFT JOIN tax_genres g ON lower(g.label) = lower(t.genre)
		GROUP BY t.id, g.id`, userID, graphDays)
	if err != nil {
		return nil, err
	}
	byArtist, byGenre := map[string]*activity{}, map[string]*activity{}
	var songs []song
	total := func(m map[string]*activity, key string, a activity) {
		if key == "" {
			return
		}
		if m[key] == nil {
			m[key] = &activity{}
		}
		m[key].add(a)
	}
	defer rows.Close()
	for rows.Next() {
		var s song
		d := &s.did
		if err := rows.Scan(&s.artist, &s.genre, &d.Plays, &d.Finished, &d.Skipped, &d.Replays, &d.Likes, &d.unlikes, &d.PlaylistAdds, &d.SavedSongs); err != nil {
			return nil, err
		}
		songs = append(songs, s)
		total(byArtist, s.artist, s.did)
		total(byGenre, s.genre, s.did)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	got := func(m map[string]*activity, key string) activity {
		if m[key] == nil {
			return activity{}
		}
		return *m[key]
	}
	// mostActive orders ids by interest, most first
	mostActive := func(ids []string, m map[string]*activity) []string {
		slices.SortStableFunc(ids, func(a, b string) int { return got(m, b).interest() - got(m, a).interest() })
		return ids
	}
	extra := func(m map[string]*activity, have []string, most int) []string {
		var ids []string
		for id, a := range m {
			if a.interest() > 0 && !slices.Contains(have, id) {
				ids = append(ids, id)
			}
		}
		slices.Sort(ids)
		ids = mostActive(ids, m)
		return ids[:min(len(ids), most)]
	}

	labels := func(table string, ids []string) (map[string]string, error) {
		out := map[string]string{}
		rows, err := h.pool.Query(ctx, "SELECT id, label FROM "+table+" WHERE id = ANY($1)", ids)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var id, label string
			if err := rows.Scan(&id, &label); err != nil {
				return nil, err
			}
			out[id] = label
		}
		return out, rows.Err()
	}
	named := func(m map[string]string, id string) string {
		if l := m[id]; l != "" {
			return l
		}
		return id
	}

	nodes := []*graphNode{}
	spoken := slices.Clone(preferred)
	for _, id := range understood {
		if !slices.Contains(spoken, id) {
			spoken = append(spoken, id)
		}
	}
	languageNames, err := labels("tax_languages", spoken)
	if err != nil {
		return nil, err
	}
	for _, id := range spoken {
		also := slices.Contains(understood, id)
		nodes = append(nodes, &graphNode{ID: "language:" + id, Type: "language", Label: named(languageNames, id), Chosen: slices.Contains(preferred, id), Understood: &also})
	}
	regionNames, err := labels("tax_regions", regions)
	if err != nil {
		return nil, err
	}
	for _, id := range regions {
		nodes = append(nodes, &graphNode{ID: "region:" + id, Type: "region", Label: named(regionNames, id), Chosen: true})
	}
	genreIDs := append(slices.Clone(genres), extra(byGenre, genres, graphGenres)...)
	genreNames, err := labels("tax_genres", genreIDs)
	if err != nil {
		return nil, err
	}
	for _, id := range genreIDs {
		nodes = append(nodes, &graphNode{ID: "genre:" + id, Type: "genre", Label: named(genreNames, id),
			Chosen: slices.Contains(genres, id), Activity: got(byGenre, id).shown()})
	}

	rows, err = h.pool.Query(ctx, `SELECT artist_id FROM library_artists WHERE user_id::text = $1 ORDER BY added_at DESC, artist_id`, userID)
	if err != nil {
		return nil, err
	}
	followed, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	// banned artists are left out everywhere
	var candidates []string
	for id := range byArtist {
		candidates = append(candidates, id)
	}
	candidates = append(candidates, append(slices.Clone(picked), followed...)...)
	rows, err = h.pool.Query(ctx, `SELECT a.id, a.name, COALESCE(u.avatar_image_id, a.avatar_image_id), COALESCE(a.image_url, ''), a.banned_at IS NOT NULL
		FROM artists a LEFT JOIN users u ON u.id = a.owner_user_id WHERE a.id = ANY($1)`, candidates)
	if err != nil {
		return nil, err
	}
	type who struct{ name, image string }
	artists, banned := map[string]who{}, map[string]bool{}
	defer rows.Close()
	for rows.Next() {
		var id, name, legacy string
		var avatar *string
		var isBanned bool
		if err := rows.Scan(&id, &name, &avatar, &legacy, &isBanned); err != nil {
			return nil, err
		}
		artists[id], banned[id] = who{name, image.PersonURL(h.PublicURL, avatar, legacy)}, isBanned
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for id := range banned {
		if banned[id] {
			delete(byArtist, id)
		}
	}
	allowed := func(ids []string) []string {
		return slices.DeleteFunc(slices.Clone(ids), func(id string) bool { return banned[id] })
	}
	chosenShown, followedShown := mostActive(allowed(picked), byArtist), mostActive(allowed(followed), byArtist)
	artistIDs := slices.Clone(chosenShown[:min(len(chosenShown), graphPicked)])
	for _, id := range followedShown[:min(len(followedShown), graphPicked)] {
		if !slices.Contains(artistIDs, id) {
			artistIDs = append(artistIDs, id)
		}
	}
	artistIDs = append(artistIDs, extra(byArtist, append(slices.Clone(picked), followed...), graphArtists)...)
	for _, id := range artistIDs {
		follows := slices.Contains(followed, id)
		name := artists[id].name
		if name == "" {
			name = id
		}
		nodes = append(nodes, &graphNode{ID: "artist:" + id, Type: "artist", Label: name, ImageURL: artists[id].image,
			Chosen: slices.Contains(picked, id), Followed: &follows, Activity: got(byArtist, id).shown()})
	}

	// an artist and a genre are linked by the songs of both that the listener touched
	weights := map[[2]string]int{}
	linked := map[string]bool{}
	for _, s := range songs {
		if slices.Contains(artistIDs, s.artist) && slices.Contains(genreIDs, s.genre) {
			weights[[2]string{s.artist, s.genre}]++
			linked[s.artist] = true
		}
	}
	var quiet []string
	for _, id := range artistIDs {
		if got(byArtist, id) == (activity{}) && (slices.Contains(picked, id) || slices.Contains(followed, id)) && !linked[id] {
			quiet = append(quiet, id)
		}
	}
	rows, err = h.pool.Query(ctx, `SELECT DISTINCT t.artist_id, g.id FROM tracks t JOIN tax_genres g ON lower(g.label) = lower(t.genre)
		WHERE t.artist_id = ANY($1) AND t.visibility = 'public' AND g.id = ANY($2)`, quiet, genreIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a, g string
		if err := rows.Scan(&a, &g); err != nil {
			return nil, err
		}
		weights[[2]string{a, g}] = 1
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	links := []graphLink{}
	for k, n := range weights {
		links = append(links, graphLink{"artist:" + k[0], "genre:" + k[1], "artist_genre", n})
	}
	slices.SortFunc(links, func(a, b graphLink) int { return strings.Compare(a.Source+" "+a.Target, b.Source+" "+b.Target) })

	return map[string]any{
		"onboarding": map[string]any{"completed": completed, "updatedAt": updatedAt, "discoveryMode": mode},
		"windowDays": graphDays, "nodes": nodes, "links": links,
	}, nil
}
