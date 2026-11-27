package admin

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/blacmusic/backend/internal/audit"
	"github.com/blacmusic/backend/internal/httpx"
)

// Review: a song is sent for review, and its values are verified or rejected one by one.

// changeMetadata runs change on a song's current metadata with the song locked, provided the caller has seen the
// latest version, and answers with the result. change returns false when it changed nothing.
func (h *Handler) changeMetadata(w http.ResponseWriter, r *http.Request, version string,
	change func(ctx context.Context, tx pgx.Tx, now *trackMetadata) (bool, error)) {
	ctx, id := r.Context(), param(r, "id")
	out, err := func() (*trackMetadata, error) {
		tx, err := h.pool.Begin(ctx)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		now, err := lockMetadata(ctx, tx, id, version)
		if err != nil {
			return nil, err
		}
		changed, err := change(ctx, tx, now)
		if err != nil || !changed {
			return now, err
		}
		if _, err := tx.Exec(ctx, `UPDATE track_metadata SET version = version + 1, updated_at = now(), updated_by = $2 WHERE track_id = $1`,
			id, actorID(r)); err != nil {
			return nil, err
		}
		list, err := loadMetadata(ctx, tx, "t.id = $1", id)
		if err != nil {
			return nil, err
		}
		return list[0], tx.Commit(ctx)
	}()
	if missing, ok := err.(missingRequired); ok {
		httpx.ErrorDetails(w, http.StatusUnprocessableEntity, "missing_required", "required fields are not filled in",
			map[string]any{"missing": []string(missing)})
		return
	}
	if err != nil {
		reply(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

type missingRequired []string

func (m missingRequired) Error() string { return "missing " + strings.Join(m, ", ") }

// lockMetadata reads a song's metadata with its row locked and checks the version the caller saw.
func lockMetadata(ctx context.Context, tx pgx.Tx, id, version string) (*trackMetadata, error) {
	var one int
	// the song's row is the lock: changes of one song's metadata, and of its genre elsewhere, take turns
	if err := tx.QueryRow(ctx, `SELECT 1 FROM tracks WHERE id = $1 FOR UPDATE`, id).Scan(&one); err != nil {
		return nil, err
	}
	list, err := loadMetadata(ctx, tx, "t.id = $1", id)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, errNotFound
	}
	if version != list[0].Version {
		return nil, fail4(http.StatusConflict, "stale_version", "someone else changed this song's metadata: reload it and try again")
	}
	return list[0], nil
}

// POST /v1/admin/tracks/{id}/metadata/submit {version}
func (h *Handler) submitMetadata(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Version string `json:"version"`
	}
	if err := decodeBody(w, r, &in, false); err != nil {
		reply(w, err)
		return
	}
	if in.Version == "" {
		reply(w, invalid("version is required: send the one you read"))
		return
	}
	h.changeMetadata(w, r, in.Version, func(ctx context.Context, tx pgx.Tx, now *trackMetadata) (bool, error) {
		if len(now.MissingRequired) > 0 {
			return false, missingRequired(now.MissingRequired)
		}
		if now.SubmittedAt != nil {
			return false, nil
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO track_metadata (track_id, submitted_at, submitted_by) VALUES ($1, now(), $2)
			ON CONFLICT (track_id) DO UPDATE SET submitted_at = now(), submitted_by = $2, review_note = ''`, now.TrackID, actorID(r)); err != nil {
			return false, err
		}
		return true, h.log(ctx, tx, r, audit.Entry{Action: "track.metadata_submit", TargetType: "track", TargetID: now.TrackID, TargetLabel: now.Title})
	})
}

type reviewItem struct {
	Category string `json:"category"`
	ID       string `json:"id"`
}

// reviewTables says where each category's values live.
var reviewTables = map[string]struct{ table, column string }{
	"languages": {"track_languages", "language_id"}, "cultures": {"track_cultures", "culture_id"},
	"regions": {"track_regions", "region_id"}, "genres": {"track_genres", "genre_id"},
	"mood": {"track_attributes", "attribute_id"}, "energy": {"track_attributes", "attribute_id"},
	"danceability": {"track_attributes", "attribute_id"}, "instrumentation": {"track_attributes", "attribute_id"},
}

// setVerification marks one saved value of a song.
func setVerification(ctx context.Context, tx pgx.Tx, trackID string, it reviewItem, verdict, actor string) error {
	var query string
	args := []any{trackID, verdict, actor}
	t, known := reviewTables[it.Category]
	switch {
	case it.Category == "languageUnknown" && it.ID == "":
		query = `UPDATE track_metadata SET unknown_verification = $2, unknown_verified_by = $3, unknown_verified_at = now()
			WHERE track_id = $1 AND language_unknown`
	case known:
		query = "UPDATE " + t.table + " SET verification = $2, verified_by = $3, verified_at = now() WHERE track_id = $1 AND " + t.column + " = $4"
		args = append(args, it.ID)
		if t.table == "track_attributes" {
			query, args = query+" AND kind = $5", append(args, it.Category)
		}
	default:
		return invalid("unknown item " + strconv.Quote(it.Category))
	}
	tag, err := tx.Exec(ctx, query, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return invalid(it.Category + " " + strconv.Quote(it.ID) + " is not on this song")
	}
	return nil
}

// POST /v1/admin/tracks/{id}/metadata/review {version, decision: "verify"|"reject", items: [{category, id}], note?}
// Only the listed values change, all or none. A rejected value stays, marked rejected; rejecting a genre leaves
// tracks.genre alone (the genre is changed by editing).
func (h *Handler) reviewMetadata(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Version  string       `json:"version"`
		Decision string       `json:"decision"`
		Items    []reviewItem `json:"items"`
		Note     string       `json:"note"`
	}
	if err := decodeBody(w, r, &in, false); err != nil {
		reply(w, err)
		return
	}
	note, err := optionalNote(in.Note)
	if in.Decision == "reject" {
		note, err = reason(in.Note)
		if err != nil {
			err = invalid("a rejection needs a note of 3-500 characters")
		}
	}
	switch {
	case err != nil:
	case in.Version == "":
		err = invalid("version is required: send the one you read")
	case in.Decision != "verify" && in.Decision != "reject":
		err = invalid("decision must be verify or reject")
	case len(in.Items) == 0 || len(in.Items) > 200:
		err = invalid("items must list 1-200 values")
	}
	if err != nil {
		reply(w, err)
		return
	}
	verdict := map[string]string{"verify": "verified", "reject": "rejected"}[in.Decision]
	h.changeMetadata(w, r, in.Version, func(ctx context.Context, tx pgx.Tx, now *trackMetadata) (bool, error) {
		actor := actorID(r)
		if _, err := tx.Exec(ctx, `INSERT INTO track_metadata (track_id) VALUES ($1) ON CONFLICT (track_id) DO NOTHING`, now.TrackID); err != nil {
			return false, err
		}
		for _, it := range in.Items {
			if err := setVerification(ctx, tx, now.TrackID, it, verdict, actor); err != nil {
				return false, err
			}
		}
		// a rejection takes the song out of review and says why
		after := `UPDATE track_metadata SET review_note = '' WHERE track_id = $1 AND $2 = ''`
		if in.Decision == "reject" {
			after = `UPDATE track_metadata SET review_note = $2, submitted_at = NULL, submitted_by = NULL WHERE track_id = $1`
		}
		arg := ""
		if in.Decision == "reject" {
			arg = note
		}
		if _, err := tx.Exec(ctx, after, now.TrackID, arg); err != nil {
			return false, err
		}
		return true, h.log(ctx, tx, r, audit.Entry{Action: "track.metadata_" + in.Decision, TargetType: "track", TargetID: now.TrackID,
			TargetLabel: now.Title, Reason: note, Details: map[string]any{"items": in.Items}})
	})
}

// ---------------------------------------------------------------- the review queues

// metadataQueues are the lists of songs that need work, over track_metadata_state s.
var metadataQueues = []struct{ name, condition string }{
	{"pending_review", `s.status = 'pending_review'`},
	{"no_language", `NOT (s.has_language OR (s.language_unknown AND s.unknown_verification <> 'rejected'))`},
	{"unverified_language", `(s.language_pending OR (s.language_unknown AND s.unknown_verification = 'pending'))`},
	{"no_genre", `NOT s.has_genre`},
	{"culture_region_review", `(s.culture_pending OR s.region_pending)`},
	{"incomplete", `cardinality(s.missing) > 0`},
}

// GET /v1/admin/metadata/tracks?queue=&artistId=&releaseType=single|album&q=&limit=&offset=, oldest release first.
// counts covers every queue under the same filters.
func (h *Handler) metadataQueue(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	condition := ""
	counting := make([]string, len(metadataQueues))
	for i, m := range metadataQueues {
		counting[i] = "count(*) FILTER (WHERE " + m.condition + ")"
		if m.name == q.Get("queue") {
			condition = m.condition
		}
	}
	if condition == "" {
		reply(w, fail4(http.StatusBadRequest, "bad_request",
			"queue must be pending_review, no_language, unverified_language, no_genre, culture_region_review or incomplete"))
		return
	}
	if t := q.Get("releaseType"); t != "" && t != "single" && t != "album" {
		reply(w, fail4(http.StatusBadRequest, "bad_request", "releaseType must be single or album"))
		return
	}
	limit, offset := 25, 0
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		limit = min(n, 100)
	}
	if n, err := strconv.Atoi(q.Get("offset")); err == nil && n > 0 {
		offset = n
	}
	search := strings.TrimSpace(q.Get("q"))
	from := ` FROM tracks t JOIN track_metadata_state s ON s.track_id = t.id JOIN albums al ON al.id = t.album_id
		LEFT JOIN artists a ON a.id = t.artist_id
		WHERE ($1 = '' OR t.artist_id = $1) AND ($2 = '' OR al.release_type = $2)
		  AND ($3 = '' OR t.title ILIKE $4 OR al.title ILIKE $4 OR a.name ILIKE $4 OR t.id = $3)`
	args := []any{q.Get("artistId"), q.Get("releaseType"), search, like(search)}
	ctx := r.Context()

	values := make([]int, len(metadataQueues))
	dest := make([]any, len(values))
	for i := range values {
		dest[i] = &values[i]
	}
	if err := h.pool.QueryRow(ctx, "SELECT "+strings.Join(counting, ", ")+from, args...).Scan(dest...); err != nil {
		reply(w, err)
		return
	}
	counts, total := map[string]int{}, 0
	for i, m := range metadataQueues {
		counts[m.name] = values[i]
		if m.name == q.Get("queue") {
			total = values[i]
		}
	}

	type queued struct {
		TrackID         string   `json:"trackId"`
		Title           string   `json:"title"`
		ReleaseID       string   `json:"releaseId"`
		ReleaseTitle    string   `json:"releaseTitle"`
		ReleaseType     string   `json:"releaseType"`
		Artist          idName   `json:"artist"`
		Status          string   `json:"status"`
		MissingRequired []string `json:"missingRequired"`
		LanguageUnknown bool     `json:"languageUnknown"`
	}
	rows, err := h.pool.Query(ctx, `SELECT t.id, t.title, al.id, al.title, al.release_type, COALESCE(a.id, ''), COALESCE(a.name, ''),
		s.status, s.missing, s.language_unknown`+from+` AND `+condition+`
		ORDER BY al.release_date NULLS LAST, al.created_at, al.id, t.track_number NULLS LAST, t.created_at, t.id LIMIT $5 OFFSET $6`,
		append(args, limit, offset)...)
	if err != nil {
		reply(w, err)
		return
	}
	defer rows.Close()
	tracks := []queued{}
	for rows.Next() {
		var x queued
		if err := rows.Scan(&x.TrackID, &x.Title, &x.ReleaseID, &x.ReleaseTitle, &x.ReleaseType, &x.Artist.ID, &x.Artist.Name,
			&x.Status, &x.MissingRequired, &x.LanguageUnknown); err != nil {
			reply(w, err)
			return
		}
		tracks = append(tracks, x)
	}
	if err := rows.Err(); err != nil {
		reply(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"tracks": tracks, "total": total, "counts": counts})
}
