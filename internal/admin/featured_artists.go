package admin

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/blacmusic/backend/internal/audit"
	"github.com/blacmusic/backend/internal/credits"
	"github.com/blacmusic/backend/internal/httpx"
	"github.com/blacmusic/backend/internal/image"
)

// The team attaches artists as features to a song or to a whole release. Only these link to the featured artist's
// profile. Each change touches one credit, never the others.
func (h *Handler) featuredArtistRoutes(r chi.Router) {
	r.Get("/releases/{id}/featured-artists", h.listFeaturedArtists)
	r.Post("/releases/{id}/featured-artists", h.featureOnRelease)
	r.Post("/tracks/{id}/featured-artists", h.featureOnTrack)
	r.Delete("/featured-artists/{creditId}", h.removeFeaturedArtist)
}

type featureCredit struct {
	CreditID string `json:"creditId"`
	ArtistID string `json:"artistId,omitempty"`
	Name     string `json:"name"`
	ImageURL string `json:"imageUrl,omitempty"`
	Verified bool   `json:"verified"`
	// AddedByTeam credits link to the artist's profile; one the release's own artist wrote shows as a name only.
	AddedByTeam bool `json:"addedByTeam"`
	// ArtistBanned: not shown to anyone while the ban lasts.
	ArtistBanned bool `json:"artistBanned"`
}

type featuredTrack struct {
	TrackID string          `json:"trackId"`
	Title   string          `json:"title"`
	Artists []featureCredit `json:"artists"` // attached to this song alone
}

type featuredView struct {
	ReleaseID string          `json:"releaseId"`
	Release   []featureCredit `json:"release"` // attached to the whole release: on every song
	Tracks    []featuredTrack `json:"tracks"`
}

func (h *Handler) featuredOf(ctx context.Context, releaseID string) (*featuredView, error) {
	var one int
	if err := h.pool.QueryRow(ctx, `SELECT 1 FROM albums WHERE id = $1`, releaseID).Scan(&one); err != nil {
		return nil, err
	}
	out := &featuredView{ReleaseID: releaseID, Release: []featureCredit{}, Tracks: []featuredTrack{}}
	rows, err := h.pool.Query(ctx, `SELECT id, title FROM tracks WHERE album_id = $1 ORDER BY track_number NULLS LAST, created_at, id`, releaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	at := map[string]int{}
	for rows.Next() {
		t := featuredTrack{Artists: []featureCredit{}}
		if err := rows.Scan(&t.TrackID, &t.Title); err != nil {
			return nil, err
		}
		at[t.TrackID], out.Tracks = len(out.Tracks), append(out.Tracks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	rows, err = h.pool.Query(ctx, `
		SELECT c.id, COALESCE(c.track_id, ''), COALESCE(a.id, ''), COALESCE(a.name, c.name),
		       COALESCE(u.avatar_image_id, a.avatar_image_id), COALESCE(a.image_url, ''),
		       COALESCE(a.verification = 'verified', false), c.by_team, a.banned_at IS NOT NULL
		FROM release_credits c LEFT JOIN artists a ON a.id = c.artist_id LEFT JOIN users u ON u.id = a.owner_user_id
		WHERE c.release_id = $1 AND c.role = 'featured_artist' ORDER BY c.position, c.created_at, c.id`, releaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c featureCredit
		var track, legacy string
		var avatar *string
		if err := rows.Scan(&c.CreditID, &track, &c.ArtistID, &c.Name, &avatar, &legacy, &c.Verified, &c.AddedByTeam, &c.ArtistBanned); err != nil {
			return nil, err
		}
		if c.ArtistID != "" {
			c.ImageURL = image.PersonURL(h.PublicURL, avatar, legacy)
		}
		if track == "" {
			out.Release = append(out.Release, c)
		} else if i, ok := at[track]; ok {
			out.Tracks[i].Artists = append(out.Tracks[i].Artists, c)
		}
	}
	return out, rows.Err()
}

func (h *Handler) replyFeatured(w http.ResponseWriter, r *http.Request, releaseID string) {
	v, err := h.featuredOf(r.Context(), releaseID)
	if err != nil {
		reply(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, v)
}

// GET /v1/admin/releases/{id}/featured-artists
func (h *Handler) listFeaturedArtists(w http.ResponseWriter, r *http.Request) {
	h.replyFeatured(w, r, param(r, "id"))
}

// POST /v1/admin/releases/{id}/featured-artists {artistId}
func (h *Handler) featureOnRelease(w http.ResponseWriter, r *http.Request) {
	h.attach(w, r, param(r, "id"), "")
}

// POST /v1/admin/tracks/{id}/featured-artists {artistId}
func (h *Handler) featureOnTrack(w http.ResponseWriter, r *http.Request) {
	var releaseID string
	if err := h.pool.QueryRow(r.Context(), `SELECT album_id FROM tracks WHERE id = $1 AND album_id IS NOT NULL`, param(r, "id")).Scan(&releaseID); err != nil {
		reply(w, err)
		return
	}
	h.attach(w, r, releaseID, param(r, "id"))
}

// attach adds an artist as a feature (trackID "" for the whole release). Attaching one the release's artist already
// named there makes that credit the team's, so it links; attaching twice changes nothing.
func (h *Handler) attach(w http.ResponseWriter, r *http.Request, releaseID, trackID string) {
	var in struct {
		ArtistID string `json:"artistId"`
	}
	if err := decodeBody(w, r, &in, false); err != nil {
		reply(w, err)
		return
	}
	ctx := r.Context()
	err := func() error {
		tx, err := h.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		var main, title string
		if err := tx.QueryRow(ctx, `SELECT COALESCE(artist_id, ''), title FROM albums WHERE id = $1 FOR UPDATE`, releaseID).Scan(&main, &title); err != nil {
			return err
		}
		var name string
		var banned bool
		err = tx.QueryRow(ctx, `SELECT name, banned_at IS NOT NULL FROM artists WHERE id = $1`, in.ArtistID).Scan(&name, &banned)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return invalid("artistId must be a Blac Music artist")
		case err != nil:
			return err
		case banned:
			return fail4(http.StatusConflict, "artist_banned", "this artist is banned")
		case in.ArtistID == main:
			return invalid("the release's own artist cannot be a feature on it")
		}
		const here = `release_id = $1 AND role = 'featured_artist' AND COALESCE(track_id, '') = $2 AND artist_id = $3`
		var byTeam bool
		err = tx.QueryRow(ctx, `SELECT bool_or(by_team) FROM release_credits WHERE `+here+` HAVING count(*) > 0`, releaseID, trackID, in.ArtistID).Scan(&byTeam)
		switch {
		case err == nil && byTeam:
			return nil
		case err == nil:
			if _, err := tx.Exec(ctx, `UPDATE release_credits SET by_team = true WHERE `+here, releaseID, trackID, in.ArtistID); err != nil {
				return err
			}
		case errors.Is(err, pgx.ErrNoRows):
			var count int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM release_credits WHERE release_id = $1`, releaseID).Scan(&count); err != nil {
				return err
			}
			if count >= credits.MaxCredits {
				return fail4(http.StatusConflict, "too_many_credits", "this release already has 100 credits")
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO release_credits (release_id, track_id, name, role, artist_id, by_team, position)
				SELECT $1, NULLIF($2, ''), $4, 'featured_artist', $3, true, COALESCE(max(position) + 1, 0) FROM release_credits WHERE release_id = $1`,
				releaseID, trackID, in.ArtistID, name); err != nil {
				return err
			}
		default:
			return err
		}
		if err := h.log(ctx, tx, r, audit.Entry{Action: "release.feature_artist", TargetType: "release", TargetID: releaseID, TargetLabel: title,
			Details: map[string]any{"artistId": in.ArtistID, "artistName": name, "trackId": trackID}}); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}()
	if err != nil {
		reply(w, err)
		return
	}
	h.replyFeatured(w, r, releaseID)
}

// DELETE /v1/admin/featured-artists/{creditId}: removes that one feature, whoever added it.
func (h *Handler) removeFeaturedArtist(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var releaseID string
	err := func() error {
		tx, err := h.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		var name, artistID, trackID, title string
		if err := tx.QueryRow(ctx, `DELETE FROM release_credits c WHERE c.id = $1 AND c.role = 'featured_artist'
			RETURNING c.release_id, c.name, COALESCE(c.artist_id, ''), COALESCE(c.track_id, ''),
			          (SELECT title FROM albums WHERE id = c.release_id)`, param(r, "creditId")).Scan(&releaseID, &name, &artistID, &trackID, &title); err != nil {
			return err
		}
		if err := h.log(ctx, tx, r, audit.Entry{Action: "release.unfeature_artist", TargetType: "release", TargetID: releaseID, TargetLabel: title,
			Details: map[string]any{"artistId": artistID, "artistName": name, "trackId": trackID}}); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}()
	if err != nil {
		reply(w, err)
		return
	}
	h.replyFeatured(w, r, releaseID)
}
