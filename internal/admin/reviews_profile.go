package admin

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/blacmusic/backend/internal/artist"
	"github.com/blacmusic/backend/internal/audit"
	"github.com/blacmusic/backend/internal/httpx"
	"github.com/blacmusic/backend/internal/image"
	"github.com/blacmusic/backend/internal/review"
)

// Artist profiles in the review queue.

const profilePrefix = "arv_"

// profileRows reads profile submissions as queue rows.
func (h *Handler) profileRows(ctx context.Context, ids []string) (map[string]*reviewRow, error) {
	out := map[string]*reviewRow{}
	for _, id := range ids {
		p, err := review.GetProfile(ctx, h.pool, id)
		if err != nil || p == nil {
			return nil, errors.Join(err, errNotFound)
		}
		x := &reviewRow{ID: p.ID, Subject: "profile", Type: "profile", Kind: p.Kind, Status: p.Status, Revision: p.Revision,
			SubmittedAt: p.SubmittedAt, DecidedAt: p.DecidedAt, Reason: p.Reason, Changes: p.Changes.Labels()}
		var avatar *string
		var legacy string
		if err := h.pool.QueryRow(ctx, `SELECT a.id, a.name, COALESCE(u.avatar_image_id, a.avatar_image_id), COALESCE(a.image_url, '')
			FROM artists a LEFT JOIN users u ON u.id = a.owner_user_id WHERE a.id = $1`, p.ArtistID).
			Scan(&x.Artist.ID, &x.Artist.Name, &avatar, &legacy); err != nil {
			return nil, err
		}
		x.Title, x.CoverURL = x.Artist.Name, image.PersonURL(h.PublicURL, avatar, legacy)
		out[id] = x
	}
	return out, nil
}

// queuePage reads the submissions whose ids query selects, in that order, releases and profiles together.
func (h *Handler) queuePage(ctx context.Context, query string, args ...any) ([]*reviewRow, error) {
	rows, err := h.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	var releases, profiles []string
	for _, id := range ids {
		if len(id) > len(profilePrefix) && id[:len(profilePrefix)] == profilePrefix {
			profiles = append(profiles, id)
		} else {
			releases = append(releases, id)
		}
	}
	byID, err := h.profileRows(ctx, profiles)
	if err != nil {
		return nil, err
	}
	list, err := h.reviewRows(ctx, `WHERE rr.id = ANY($1)`, releases)
	if err != nil {
		return nil, err
	}
	for _, x := range list {
		byID[x.ID] = x
	}
	out := []*reviewRow{}
	for _, id := range ids {
		if x := byID[id]; x != nil {
			out = append(out, x)
		}
	}
	return out, nil
}

type profileSide struct {
	Name     string `json:"name"`
	Bio      string `json:"bio"`
	ImageURL string `json:"imageUrl,omitempty"`
}

type profileDetail struct {
	*reviewRow
	DecidedBy *person `json:"decidedBy"`
	// Approved is what listeners see now (for a new profile: what is to be checked). Proposed is what it would be;
	// the same for a new profile.
	Approved    profileSide    `json:"approved"`
	Proposed    profileSide    `json:"proposed"`
	ChangeItems []reviewChange `json:"changeItems"`
	// Releases are this artist's releases waiting too: they can be approved once the profile is.
	Releases []string  `json:"releaseReviewIds"`
	Created  time.Time `json:"artistCreatedAt"`
}

func (h *Handler) profileDetail(ctx context.Context, id string) (*profileDetail, error) {
	rows, err := h.profileRows(ctx, []string{id})
	if err != nil {
		return nil, err
	}
	d := &profileDetail{reviewRow: rows[id], ChangeItems: []reviewChange{}, Releases: []string{}}
	p, err := review.GetProfile(ctx, h.pool, id)
	if err != nil {
		return nil, err
	}
	var by, email *string
	if err := h.pool.QueryRow(ctx, `SELECT a.bio, a.created_at, u.id::text, u.email,
		ARRAY(SELECT rr.id FROM release_reviews rr JOIN albums al ON al.id = rr.release_id WHERE al.artist_id = a.id AND rr.status = 'pending' ORDER BY rr.submitted_at)
		FROM artist_reviews r JOIN artists a ON a.id = r.artist_id LEFT JOIN users u ON u.id = r.decided_by WHERE r.id = $1`, id).
		Scan(&d.Approved.Bio, &d.Created, &by, &email, &d.Releases); err != nil {
		return nil, err
	}
	if by != nil {
		d.DecidedBy = &person{*by, deref(email)}
	}
	d.Approved.Name, d.Approved.ImageURL = d.Artist.Name, d.CoverURL
	d.Proposed = d.Approved
	if p.Status != "pending" || p.Kind != "update" {
		return d, nil
	}
	add := func(field, label string, was, is any) {
		d.ChangeItems = append(d.ChangeItems, reviewChange{Kind: "profile", Label: label, Field: field, Approved: was, Proposed: is})
	}
	if v := p.Changes.Name; v != nil {
		d.Proposed.Name = *v
		add("name", "Name", d.Approved.Name, *v)
	}
	if v := p.Changes.AvatarImageID; v != nil {
		d.Proposed.ImageURL = image.URL(h.PublicURL, *v, image.SizeMedium)
		add("photo", "Photo", d.Approved.ImageURL, d.Proposed.ImageURL)
	}
	if v := p.Changes.Bio; v != nil {
		d.Proposed.Bio = *v
		add("bio", "Bio", d.Approved.Bio, *v)
	}
	return d, nil
}

func (h *Handler) getProfileReview(w http.ResponseWriter, r *http.Request) {
	d, err := h.profileDetail(r.Context(), param(r, "id"))
	if err != nil {
		reply(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, d)
}

func (h *Handler) decideProfile(w http.ResponseWriter, r *http.Request, id string, revision int, approve bool, why, action string) {
	err := h.artists.DecideProfile(r.Context(), id, revision, actorID(r), approve, why,
		func(ctx context.Context, tx pgx.Tx, p *review.ProfileRow, name string) error {
			return h.log(ctx, tx, r, audit.Entry{Action: action, TargetType: "artist", TargetID: p.ArtistID, TargetLabel: name, Reason: why,
				Details: map[string]any{"reviewId": p.ID, "kind": p.Kind, "revision": p.Revision}})
		})
	switch {
	case errors.Is(err, artist.ErrNoReview):
		err = fail4(http.StatusConflict, "not_pending", "this is no longer waiting: it was withdrawn or already decided")
	case errors.Is(err, artist.ErrReviewChanged):
		err = fail4(http.StatusConflict, "review_changed", "the artist changed it since you opened it: look again")
	case errors.Is(err, artist.ErrBanned):
		err = fail4(http.StatusConflict, "artist_banned", "this artist is banned")
	case errors.Is(err, artist.ErrNotFound):
		err = errNotFound
	}
	if err != nil {
		reply(w, err)
		return
	}
	h.getProfileReview(w, r)
}
