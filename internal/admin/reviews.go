package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/blacmusic/backend/internal/audit"
	"github.com/blacmusic/backend/internal/httpx"
	"github.com/blacmusic/backend/internal/lyrics"
	"github.com/blacmusic/backend/internal/release"
	"github.com/blacmusic/backend/internal/review"
)

// The team's queue of what artists have sent: new releases, and changes to releases that are online.
func (h *Handler) reviewRoutes(r chi.Router) {
	r.Get("/reviews", h.listReviews)
	r.Get("/reviews/{id}", h.getReview)
	r.Post("/reviews/{id}/approve", h.approveReview)
	r.Post("/reviews/{id}/reject", h.rejectReview)
}

// audioTTL is how long a link to listen to a song under review works.
const audioTTL = time.Hour

type reviewRow struct {
	ID          string     `json:"id"`
	Subject     string     `json:"subject"` // release | profile
	ReleaseID   string     `json:"releaseId"`
	Title       string     `json:"title"`
	Type        string     `json:"type"`
	CoverURL    string     `json:"coverUrl,omitempty"`
	Kind        string     `json:"kind"`
	Status      string     `json:"status"`
	Revision    int        `json:"revision"`
	Artist      idName     `json:"artist"`
	SubmittedAt time.Time  `json:"submittedAt"`
	PublishAt   *time.Time `json:"publishAt"`
	DecidedAt   *time.Time `json:"decidedAt"`
	Reason      string     `json:"reason"`
	Changes     []string   `json:"changes"` // what is to be checked, for an update
}

func (h *Handler) reviewRows(ctx context.Context, rest string, args ...any) ([]*reviewRow, error) {
	rows, err := h.pool.Query(ctx, `
		SELECT rr.id, al.id, al.title, al.release_type, COALESCE(al.artwork_url, ''), rr.kind, rr.status, rr.revision,
		       COALESCE(a.id, ''), COALESCE(a.name, ''), rr.submitted_at, rr.publish_at, rr.decided_at, rr.reason, rr.changes
		FROM release_reviews rr JOIN albums al ON al.id = rr.release_id LEFT JOIN artists a ON a.id = al.artist_id `+rest, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*reviewRow{}
	var changes []review.Changes
	for rows.Next() {
		x := &reviewRow{Subject: "release", Changes: []string{}}
		var raw []byte
		if err := rows.Scan(&x.ID, &x.ReleaseID, &x.Title, &x.Type, &x.CoverURL, &x.Kind, &x.Status, &x.Revision,
			&x.Artist.ID, &x.Artist.Name, &x.SubmittedAt, &x.PublishAt, &x.DecidedAt, &x.Reason, &raw); err != nil {
			return nil, err
		}
		var c review.Changes
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, err
		}
		out, changes = append(out, x), append(changes, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i, x := range out {
		if x.Kind == "update" {
			if x.Changes, err = review.Labels(ctx, h.pool, changes[i]); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// GET /v1/admin/reviews?status=pending|approved|rejected|withdrawn|all&kind=new|update&limit=&offset=
// Waiting ones come oldest first, the others newest first. pending is how many wait, whatever the filters.
func (h *Handler) listReviews(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	status := q.Get("status")
	if status == "" {
		status = "pending"
	}
	kind := q.Get("kind")
	switch status {
	case "pending", "approved", "rejected", "withdrawn", "all":
	default:
		reply(w, fail4(http.StatusBadRequest, "bad_request", "status must be pending, approved, rejected, withdrawn or all"))
		return
	}
	if kind != "" && kind != "new" && kind != "update" {
		reply(w, fail4(http.StatusBadRequest, "bad_request", "kind must be new or update"))
		return
	}
	ctx := r.Context()
	subject := q.Get("subject")
	if subject != "" && subject != "release" && subject != "profile" {
		reply(w, fail4(http.StatusBadRequest, "bad_request", "subject must be release or profile"))
		return
	}
	// both kinds of submission as one list
	const both = `(SELECT id, 'release' AS subject, status, kind, submitted_at FROM release_reviews
		UNION ALL SELECT id, 'profile', status, kind, submitted_at FROM artist_reviews) x
		WHERE ($1 = 'all' OR x.status = $1) AND ($2 = '' OR x.kind = $2) AND ($3 = '' OR x.subject = $3)`
	var total, pending int
	if err := h.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM `+both+`),
		(SELECT count(*) FROM release_reviews WHERE status = 'pending') + (SELECT count(*) FROM artist_reviews WHERE status = 'pending')`,
		status, kind, subject).Scan(&total, &pending); err != nil {
		reply(w, err)
		return
	}
	order := "x.submitted_at DESC, x.id"
	if status == "pending" {
		order = "x.submitted_at, x.id"
	}
	limit, offset := page(r)
	list, err := h.queuePage(ctx, `SELECT x.id FROM `+both+` ORDER BY `+order+` LIMIT $4 OFFSET $5`, status, kind, subject, limit, offset)
	if err != nil {
		reply(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"reviews": list, "total": total, "pending": pending})
}

type reviewTrack struct {
	TrackID     string         `json:"trackId"`
	Title       string         `json:"title"`
	TrackNumber *int           `json:"trackNumber"`
	DurationMs  int            `json:"durationMs"`
	Explicit    bool           `json:"explicit"`
	Genre       string         `json:"genre"`
	Visibility  string         `json:"visibility"`
	IsNew       bool           `json:"isNew"` // added to an online release, hidden until approved
	AudioURL    string         `json:"audioUrl,omitempty"`
	Lyrics      *lyrics.Lyrics `json:"lyrics"` // as listeners have them now (a new release: what is to be checked)
	Featured    []string       `json:"featuredArtists"`
}

type reviewCredit struct {
	Name       string `json:"name"`
	Role       string `json:"role"`
	RoleLabel  string `json:"roleLabel,omitempty"`
	TrackID    string `json:"trackId,omitempty"`
	ArtistID   string `json:"artistId,omitempty"`
	ArtistName string `json:"artistName,omitempty"` // the Blac artist the credit points at
}

// reviewChange is one thing to be checked, as it is and as it would be.
type reviewChange struct {
	Kind     string `json:"kind"` // release | track | lyrics | credits | newTrack
	Label    string `json:"label"`
	Field    string `json:"field,omitempty"`
	TrackID  string `json:"trackId,omitempty"`
	Approved any    `json:"approved"`
	Proposed any    `json:"proposed"`
}

type reviewDetail struct {
	*reviewRow
	DecidedBy *person `json:"decidedBy"`
	Release   struct {
		Description string  `json:"description"`
		Genre       string  `json:"genre"`
		ReleaseDate *string `json:"releaseDate"`
		Visibility  string  `json:"visibility"`
	} `json:"release"`
	// ArtistProfile says whether the artist's profile is approved, and the submission about it when one waits.
	ArtistProfile struct {
		Approved bool   `json:"approved"`
		ReviewID string `json:"reviewId,omitempty"`
	} `json:"artistProfile"`
	Tracks      []*reviewTrack `json:"tracks"`
	Credits     []reviewCredit `json:"credits"` // as listeners have them now
	ChangeItems []reviewChange `json:"changeItems"`
}

func (h *Handler) reviewDetail(ctx context.Context, id string) (*reviewDetail, error) {
	list, err := h.reviewRows(ctx, `WHERE rr.id = $1`, id)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, errNotFound
	}
	d := &reviewDetail{reviewRow: list[0], Tracks: []*reviewTrack{}, Credits: []reviewCredit{}, ChangeItems: []reviewChange{}}
	row, err := review.Get(ctx, h.pool, id)
	if err != nil || row == nil {
		return nil, errors.Join(err, errNotFound)
	}
	var by, email *string
	var cover *string
	if err := h.pool.QueryRow(ctx, `
		SELECT al.description, al.genre, to_char(al.release_date, 'YYYY-MM-DD'), al.visibility, al.cover_image_id::text, u.id::text, u.email
		FROM release_reviews rr JOIN albums al ON al.id = rr.release_id LEFT JOIN users u ON u.id = rr.decided_by WHERE rr.id = $1`, id).
		Scan(&d.Release.Description, &d.Release.Genre, &d.Release.ReleaseDate, &d.Release.Visibility, &cover, &by, &email); err != nil {
		return nil, err
	}
	if by != nil {
		d.DecidedBy = &person{*by, deref(email)}
	}
	if err := h.pool.QueryRow(ctx, `SELECT COALESCE((SELECT approved_at IS NOT NULL FROM artists WHERE id = $1), true),
		COALESCE((SELECT id FROM artist_reviews WHERE artist_id = $1 AND status = 'pending'), '')`, d.Artist.ID).
		Scan(&d.ArtistProfile.Approved, &d.ArtistProfile.ReviewID); err != nil {
		return nil, err
	}

	rows, err := h.pool.Query(ctx, `
		SELECT t.id, t.title, t.track_number, t.duration_ms, t.explicit, t.genre, t.visibility, t.review_hold,
		       COALESCE((SELECT media_key FROM track_renditions x WHERE x.track_id = t.id ORDER BY (x.quality <> 'medium'), x.quality LIMIT 1), ''),
		       ARRAY(SELECT c.name FROM release_credits c WHERE c.role = 'featured_artist'
		             AND (c.track_id = t.id OR (c.track_id IS NULL AND c.release_id = t.album_id)) ORDER BY c.position, c.name)
		FROM tracks t WHERE t.album_id = $1 ORDER BY t.track_number NULLS LAST, t.created_at, t.id`, d.ReleaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byID := map[string]*reviewTrack{}
	keys := map[string]string{}
	for rows.Next() {
		t := &reviewTrack{Featured: []string{}}
		var key string
		if err := rows.Scan(&t.TrackID, &t.Title, &t.TrackNumber, &t.DurationMs, &t.Explicit, &t.Genre, &t.Visibility, &t.IsNew, &key, &t.Featured); err != nil {
			return nil, err
		}
		d.Tracks, byID[t.TrackID], keys[t.TrackID] = append(d.Tracks, t), t, key
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for _, t := range d.Tracks {
		if h.Media != nil && keys[t.TrackID] != "" {
			if t.AudioURL, err = h.Media.PresignGet(ctx, keys[t.TrackID], audioTTL); err != nil {
				return nil, err
			}
		}
		l, err := lyrics.Of(ctx, h.pool, t.TrackID)
		if err != nil && !errors.Is(err, lyrics.ErrNotFound) {
			return nil, err
		}
		t.Lyrics = l
	}

	names := map[string]string{}
	rows, err = h.pool.Query(ctx, `SELECT c.name, c.role, c.role_label, COALESCE(c.track_id, ''), COALESCE(c.artist_id, ''), COALESCE(a.name, '')
		FROM release_credits c LEFT JOIN artists a ON a.id = c.artist_id WHERE c.release_id = $1 ORDER BY c.position, c.created_at, c.id`, d.ReleaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c reviewCredit
		if err := rows.Scan(&c.Name, &c.Role, &c.RoleLabel, &c.TrackID, &c.ArtistID, &c.ArtistName); err != nil {
			return nil, err
		}
		names[c.ArtistID] = c.ArtistName
		d.Credits = append(d.Credits, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if d.Kind != "update" || d.Status != "pending" {
		return d, nil
	}

	c := row.Changes
	title := func(id string) string {
		if t := byID[id]; t != nil {
			return t.Title
		}
		return id
	}
	approved := map[string]any{"title": d.Title, "description": d.Release.Description, "genre": d.Release.Genre,
		"releaseDate": deref(d.Release.ReleaseDate), "coverImageId": d.CoverURL}
	for _, f := range []struct{ key, label string }{{"title", "Title"}, {"coverImageId", "Cover"}, {"description", "Description"},
		{"genre", "Genre"}, {"releaseDate", "Release date"}} {
		v, ok := c.Release[f.key]
		if !ok {
			continue
		}
		field := f.key
		if field == "coverImageId" { // the covers as addresses, to look at
			field = "cover"
			if v != "" {
				v = h.PublicURL + "/v1/media/" + v + "/medium.jpg"
			}
		}
		d.ChangeItems = append(d.ChangeItems, reviewChange{Kind: "release", Label: f.label, Field: field, Approved: approved[f.key], Proposed: v})
	}
	for _, id := range c.NewTracks {
		d.ChangeItems = append(d.ChangeItems, reviewChange{Kind: "newTrack", Label: "New track: " + title(id), TrackID: id, Proposed: byID[id]})
	}
	for _, t := range d.Tracks {
		if edit, ok := c.Tracks[t.TrackID]; ok {
			add := func(field string, was, is any) {
				d.ChangeItems = append(d.ChangeItems, reviewChange{Kind: "track", Label: "Track: " + t.Title, Field: field, TrackID: t.TrackID, Approved: was, Proposed: is})
			}
			if edit.Title != nil {
				add("title", t.Title, *edit.Title)
			}
			if edit.Explicit != nil {
				add("explicit", t.Explicit, *edit.Explicit)
			}
			if edit.Genre != nil {
				add("genre", t.Genre, *edit.Genre)
			}
			if edit.TrackNumber != nil {
				add("trackNumber", t.TrackNumber, *edit.TrackNumber)
			}
		}
		if raw, ok := c.Lyrics[t.TrackID]; ok {
			var proposed lyrics.Lyrics
			if err := json.Unmarshal(raw, &proposed); err != nil {
				return nil, err
			}
			d.ChangeItems = append(d.ChangeItems, reviewChange{Kind: "lyrics", Label: "Lyrics: " + t.Title, TrackID: t.TrackID, Approved: t.Lyrics, Proposed: &proposed})
		}
	}
	if len(c.Credits) > 0 {
		var proposed []reviewCredit
		if err := json.Unmarshal(c.Credits, &proposed); err != nil {
			return nil, err
		}
		var ids []string
		for _, p := range proposed {
			if _, known := names[p.ArtistID]; !known && p.ArtistID != "" {
				ids = append(ids, p.ArtistID)
			}
		}
		more, err := h.pool.Query(ctx, `SELECT id, name FROM artists WHERE id = ANY($1)`, ids)
		if err != nil {
			return nil, err
		}
		defer more.Close()
		for more.Next() {
			var id, name string
			if err := more.Scan(&id, &name); err != nil {
				return nil, err
			}
			names[id] = name
		}
		for i := range proposed {
			proposed[i].ArtistName = names[proposed[i].ArtistID]
		}
		d.ChangeItems = append(d.ChangeItems, reviewChange{Kind: "credits", Label: "Credits", Approved: d.Credits, Proposed: proposed})
	}
	return d, nil
}

// GET /v1/admin/reviews/{id}
func (h *Handler) getReview(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(param(r, "id"), profilePrefix) {
		h.getProfileReview(w, r)
		return
	}
	d, err := h.reviewDetail(r.Context(), param(r, "id"))
	if err != nil {
		reply(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, d)
}

// POST /v1/admin/reviews/{id}/approve {revision}
func (h *Handler) approveReview(w http.ResponseWriter, r *http.Request) { h.decideReview(w, r, true) }

// POST /v1/admin/reviews/{id}/reject {revision, reason}
func (h *Handler) rejectReview(w http.ResponseWriter, r *http.Request) { h.decideReview(w, r, false) }

func (h *Handler) decideReview(w http.ResponseWriter, r *http.Request, approve bool) {
	var in struct {
		Revision int    `json:"revision"`
		Reason   string `json:"reason"`
	}
	if err := decodeBody(w, r, &in, false); err != nil {
		reply(w, err)
		return
	}
	why := ""
	if !approve {
		var err error
		if why, err = reason(in.Reason); err != nil {
			reply(w, err)
			return
		}
	}
	if in.Revision < 1 {
		reply(w, invalid("revision is required: send the one you opened ("+strconv.Itoa(1)+" or more)"))
		return
	}
	action := "review.approve"
	if !approve {
		action = "review.reject"
	}
	id := param(r, "id")
	if strings.HasPrefix(id, profilePrefix) {
		h.decideProfile(w, r, id, in.Revision, approve, why, action)
		return
	}
	err := h.Releases.Decide(r.Context(), id, in.Revision, actorID(r), approve, why,
		func(ctx context.Context, tx pgx.Tx, row *review.Row, title string) error {
			return h.log(ctx, tx, r, audit.Entry{Action: action, TargetType: "release", TargetID: row.ReleaseID, TargetLabel: title, Reason: why,
				Details: map[string]any{"reviewId": row.ID, "kind": row.Kind, "revision": row.Revision}})
		})
	switch {
	case errors.Is(err, release.ErrNotPending):
		err = fail4(http.StatusConflict, "not_pending", "this is no longer waiting: it was withdrawn or already decided")
	case errors.Is(err, release.ErrProfilePending):
		err = fail4(http.StatusConflict, "profile_pending", "approve this artist's new profile first (it is in the same queue)")
	case errors.Is(err, release.ErrReviewChanged):
		err = fail4(http.StatusConflict, "review_changed", "the artist changed it since you opened it: look again")
	}
	if err != nil {
		reply(w, err)
		return
	}
	h.getReview(w, r)
}
