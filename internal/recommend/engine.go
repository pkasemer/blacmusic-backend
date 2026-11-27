package recommend

import (
	"cmp"
	"context"
	"slices"
	"strconv"
	"strings"
	"time"
)

func sortByValue(keys []string, values map[string]float64) {
	slices.SortFunc(keys, func(a, b string) int { return cmp.Or(cmp.Compare(values[b], values[a]), cmp.Compare(a, b)) })
}

func joinReasons(reasons []string) string { return strings.Join(reasons, ",") }

// Result is a ranked list and what it was ranked with.
type Result struct {
	Mode       string
	Picks      []Pick
	Candidates int
	Profile    *Profile
}

func rankFor(ctx context.Context, db DB, userID string, seed *Track, req Request) (*Result, error) {
	p, err := LoadProfile(ctx, db, userID)
	if err != nil {
		return nil, err
	}
	candidates, err := Candidates(ctx, db, userID, p, seed)
	if err != nil {
		return nil, err
	}
	mode, _ := policyFor(p.Mode)
	req.Seed = seed
	return &Result{Mode: mode, Picks: Rank(p, candidates, req, time.Now()), Candidates: len(candidates), Profile: p}, nil
}

// Mix ranks songs to play after a starting song, for "Start mix" and autoplay: songs like it, ordered by how like it
// they are and by the listener's taste. exclude are songs already in the queue. variation changes the order.
func Mix(ctx context.Context, db DB, userID, seedTrackID string, exclude []string, limit int, variation string) (*Result, error) {
	seed, err := Song(ctx, db, seedTrackID)
	if err != nil {
		return nil, err
	}
	return rankFor(ctx, db, userID, &seed, Request{Limit: limit, Relax: true, Exclude: set(exclude), Variation: variation})
}

// How long before a song is new again for "Your sound".
const (
	soundNotHeardDays = 30
	soundNotAgainDays = 90
)

// ForSound ranks new songs for a listener's "Your sound" playlist: nothing already in it or saved, nothing heard in
// the last month, nothing offered in the last three, and only songs with something of the listener's in them. With
// nothing known about the listener it is empty. The order changes from day to day.
func ForSound(ctx context.Context, db DB, userID, playlistID string, limit int) (*Result, error) {
	rows, err := db.Query(ctx, `
		SELECT track_id FROM playlist_items WHERE playlist_id = $2
		UNION SELECT track_id FROM library_tracks WHERE user_id = $1
		UNION SELECT track_id FROM rec_user_tracks WHERE user_id = $1 AND last_heard_at > now() - make_interval(days => $3)
		UNION SELECT track_id FROM your_sound_offers WHERE user_id = $1 AND offered_at > now() - make_interval(days => $4)`,
		userID, playlistID, soundNotHeardDays, soundNotAgainDays)
	if err != nil {
		return nil, err
	}
	exclude := map[string]bool{}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		exclude[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	return rankFor(ctx, db, userID, nil, Request{
		Limit: limit, PersonalOnly: true, PerArtist: soundPerArtist, Exclude: exclude,
		Variation: userID + strconv.FormatInt(time.Now().Unix()/86400, 10),
	})
}

// Avoided is the SQL list of songs ($1 is the listener) to take out of what the server put in front of them: unliked,
// or passed over again and again, never finished and not kept. It is History.avoided as a query.
var Avoided = `
	SELECT h.track_id FROM rec_user_tracks h
	WHERE h.user_id = $1 AND NOT h.saved AND (h.unliked OR (
	      NOT h.liked AND h.completes = 0 AND h.playlist_adds = 0
	  AND GREATEST(h.last_skipped_at, h.last_weak_skipped_at) > now() - interval '` + strconv.Itoa(avoidDays) + ` days'
	  AND (h.skips >= ` + strconv.Itoa(avoidSkips) + ` OR h.skips + h.weak_skips >= ` + strconv.Itoa(avoidWeakSkips) + `)))`
