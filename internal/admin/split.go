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

// POST /v1/admin/tracks/{id}/single {title, genre?, releaseDate?, coverImageId? | useAlbumCover?,
// visibility: "draft"|"public"} -> 201 the new single, which the song has moved to.
func (h *Handler) splitTrack(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title         string `json:"title"`
		Genre         string `json:"genre"`
		ReleaseDate   string `json:"releaseDate"`
		CoverImageID  string `json:"coverImageId"`
		UseAlbumCover bool   `json:"useAlbumCover"`
		Visibility    string `json:"visibility"`
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
	trackID := param(r, "id")
	id, err := h.Releases.AdminSplit(ctx, actorID(r), release.SplitInput{
		TrackID: trackID, Title: in.Title, Genre: in.Genre, ReleaseDate: in.ReleaseDate,
		CoverImageID: in.CoverImageID, UseAlbumCover: in.UseAlbumCover, Public: in.Visibility == "public",
	}, func(ctx context.Context, tx pgx.Tx, id string, from release.SplitFrom) error {
		return h.log(ctx, tx, r, audit.Entry{Action: "release.split", TargetType: "release", TargetID: id, TargetLabel: strings.TrimSpace(in.Title),
			Details: map[string]any{"artistId": from.ArtistID, "fromReleaseId": from.ReleaseID, "fromTitle": from.Title, "fromRemoved": from.Removed,
				"trackId": trackID, "visibility": in.Visibility}})
	})
	if errors.Is(err, release.ErrAlreadySingle) {
		err = fail4(http.StatusConflict, "already_single", "this song is already a single")
	}
	if err != nil {
		reply(w, err)
		return
	}
	chi.RouteContext(ctx).URLParams.Add("id", id)
	h.getRelease(created(w, "/v1/admin/releases/"+id), r)
}
