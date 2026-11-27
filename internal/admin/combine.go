package admin

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/blacmusic/backend/internal/audit"
	"github.com/blacmusic/backend/internal/release"
)

// POST /v1/admin/releases/combine {releaseIds, trackIds, title, genre?, releaseDate?, coverImageId? | coverFromReleaseId?,
// visibility: "draft"|"public"} -> 201 the new album. The old releases are replaced by it.
func (h *Handler) combineReleases(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ReleaseIDs         []string `json:"releaseIds"`
		TrackIDs           []string `json:"trackIds"`
		Title              string   `json:"title"`
		Genre              string   `json:"genre"`
		ReleaseDate        string   `json:"releaseDate"`
		CoverImageID       string   `json:"coverImageId"`
		CoverFromReleaseID string   `json:"coverFromReleaseId"`
		Visibility         string   `json:"visibility"`
	}
	if err := decodeBody(w, r, &in, false); err != nil {
		reply(w, err)
		return
	}
	if in.Visibility != "draft" && in.Visibility != "public" {
		reply(w, invalid("visibility must be draft or public"))
		return
	}
	ctx := r.Context()
	id, err := h.Releases.AdminCombine(ctx, actorID(r), release.CombineInput{
		ReleaseIDs: in.ReleaseIDs, TrackIDs: in.TrackIDs, Title: in.Title, Genre: in.Genre, ReleaseDate: in.ReleaseDate,
		CoverImageID: in.CoverImageID, CoverFromReleaseID: in.CoverFromReleaseID, Public: in.Visibility == "public",
	}, func(ctx context.Context, tx pgx.Tx, id, artistID string, from []release.Combined) error {
		var artistName string
		if err := tx.QueryRow(ctx, `SELECT name FROM artists WHERE id = $1`, artistID).Scan(&artistName); err != nil {
			return err
		}
		return h.log(ctx, tx, r, audit.Entry{Action: "release.combine", TargetType: "release", TargetID: id, TargetLabel: strings.TrimSpace(in.Title),
			Details: map[string]any{"artistId": artistID, "artistName": artistName, "from": from, "tracks": len(in.TrackIDs), "visibility": in.Visibility}})
	})
	switch {
	case errors.Is(err, release.ErrDifferentArtists):
		err = fail4(http.StatusConflict, "different_artists", "only releases of one artist can be combined")
	case errors.Is(err, release.ErrNoTracks):
		err = fail4(http.StatusConflict, "no_tracks", "every release being combined needs at least one finished track")
	}
	if err != nil {
		reply(w, err)
		return
	}
	chi.RouteContext(ctx).URLParams.Add("id", id)
	h.getRelease(created(w, "/v1/admin/releases/"+id), r)
}
