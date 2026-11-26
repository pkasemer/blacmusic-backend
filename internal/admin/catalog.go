package admin

import (
	"context"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/blacmusic/backend/internal/audit"
	"github.com/blacmusic/backend/internal/image"
	"github.com/blacmusic/backend/internal/release"
	"github.com/blacmusic/backend/internal/upload"
)

// Catalog work for administrators: create an artist that has no account (a catalog artist), create and publish
// releases for any artist, and upload their audio.
func (h *Handler) catalogRoutes(r chi.Router) {
	r.Post("/artists", h.createArtist)
	r.Post("/artists/{id}/releases", h.createRelease)
	r.Post("/releases/{id}/publish", h.publishRelease)
	r.Post("/releases/{id}/unpublish", h.unpublishRelease)
	r.Post("/releases/{id}/uploads", h.createUpload)
	if h.Uploads != nil {
		r.Route("/uploads", h.Uploads.AdminRoutes)
	}
}

// created answers 201 with what a GET of the new thing answers.
func created(w http.ResponseWriter, location string) http.ResponseWriter {
	w.Header().Set("Location", location)
	return &statusWriter{ResponseWriter: w, status: http.StatusCreated}
}

// POST /v1/admin/artists {name (1-100), bio? (at most 1000), imageId?} -> 201, the artist detail: a catalog artist with
// no owner account, no storage allowance and not verified. imageId is a finished avatar image the administrator uploaded.
func (h *Handler) createArtist(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name    string `json:"name"`
		Bio     string `json:"bio"`
		ImageID string `json:"imageId"`
	}
	if err := decodeBody(w, r, &in, false); err != nil {
		reply(w, err)
		return
	}
	in.Name, in.Bio, in.ImageID = strings.TrimSpace(in.Name), strings.TrimSpace(in.Bio), strings.TrimSpace(in.ImageID)
	if n := utf8.RuneCountInString(in.Name); n < 1 || n > 100 {
		reply(w, invalid("name must be 1-100 characters"))
		return
	}
	if utf8.RuneCountInString(in.Bio) > 1000 {
		reply(w, invalid("bio must be at most 1000 characters"))
		return
	}
	ctx := r.Context()
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		reply(w, err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var avatar *string
	if in.ImageID != "" {
		if err := image.CheckOwned(ctx, tx, in.ImageID, actorID(r), "avatar"); err != nil {
			reply(w, invalid("imageId must be a finished avatar image that you uploaded"))
			return
		}
		avatar = &in.ImageID
	}
	var id string
	if err := tx.QueryRow(ctx, `
		INSERT INTO artists (name, bio, owner_user_id, avatar_image_id, flagged)
		VALUES ($1, $2, NULL, $3, EXISTS (SELECT 1 FROM artists WHERE verification = 'verified' AND lower(name) = lower($1)))
		RETURNING id`, in.Name, in.Bio, avatar).Scan(&id); err != nil {
		reply(w, err)
		return
	}
	if err := h.log(ctx, tx, r, audit.Entry{Action: "artist.create", TargetType: "artist", TargetID: id, TargetLabel: in.Name,
		Details: map[string]any{"owner": nil, "imageId": avatar}}); err != nil {
		reply(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		reply(w, err)
		return
	}
	chi.RouteContext(ctx).URLParams.Add("id", id)
	h.getArtist(created(w, "/v1/admin/artists/"+id), r)
}

// POST /v1/admin/artists/{id}/releases {type: single|album, title, description?, genre?, releaseDate?, coverImageId?} -> 201,
// the release detail (a draft with no tracks). The cover is a finished cover image the administrator uploaded.
func (h *Handler) createRelease(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Type         string `json:"type"`
		Title        string `json:"title"`
		Description  string `json:"description"`
		Genre        string `json:"genre"`
		ReleaseDate  string `json:"releaseDate"`
		CoverImageID string `json:"coverImageId"`
	}
	if err := decodeBody(w, r, &in, false); err != nil {
		reply(w, err)
		return
	}
	ctx := r.Context()
	artistID := param(r, "id")
	var artistName string
	id, err := h.Releases.AdminCreate(ctx, artistID, actorID(r), release.Input{
		Type: in.Type, Title: in.Title, Description: in.Description, Genre: in.Genre, ReleaseDate: in.ReleaseDate, CoverImageID: in.CoverImageID,
	}, func(ctx context.Context, tx pgx.Tx, id string) error {
		_ = tx.QueryRow(ctx, `SELECT name FROM artists WHERE id = $1`, artistID).Scan(&artistName)
		return h.log(ctx, tx, r, audit.Entry{Action: "release.create", TargetType: "release", TargetID: id, TargetLabel: strings.TrimSpace(in.Title),
			Details: map[string]any{"artistId": artistID, "artistName": artistName, "type": in.Type}})
	})
	if err != nil {
		reply(w, err)
		return
	}
	chi.RouteContext(ctx).URLParams.Add("id", id)
	h.getRelease(created(w, "/v1/admin/releases/"+id), r)
}

// releaseTitle reads a release title inside a transaction (for audit labels).
func releaseTitle(ctx context.Context, tx pgx.Tx, id string) string {
	var t string
	_ = tx.QueryRow(ctx, `SELECT title FROM albums WHERE id = $1`, id).Scan(&t)
	return t
}

// POST /v1/admin/releases/{id}/publish {publishAt?} -> the release detail. The owner rules: at least one finished
// track (422 no_tracks), no file still being processed (409 uploads_pending), a single has exactly one track,
// publishAt (RFC 3339, within a year) schedules it, and a taken-down release or a banned artist cannot publish
// (409 taken_down / artist_banned). No "release is live" email goes out when the artist has no account.
func (h *Handler) publishRelease(w http.ResponseWriter, r *http.Request) {
	var in struct {
		PublishAt *time.Time `json:"publishAt"`
	}
	if err := decodeBody(w, r, &in, true); err != nil {
		reply(w, err)
		return
	}
	id := param(r, "id")
	err := h.Releases.AdminPublish(r.Context(), id, in.PublishAt, func(ctx context.Context, tx pgx.Tx) error {
		return h.log(ctx, tx, r, audit.Entry{Action: "release.publish", TargetType: "release", TargetID: id, TargetLabel: releaseTitle(ctx, tx, id),
			Details: map[string]any{"publishAt": in.PublishAt}})
	})
	if err != nil {
		reply(w, err)
		return
	}
	h.getRelease(w, r)
}

// POST /v1/admin/releases/{id}/unpublish -> the release detail, back to a draft.
func (h *Handler) unpublishRelease(w http.ResponseWriter, r *http.Request) {
	id := param(r, "id")
	err := h.Releases.AdminUnpublish(r.Context(), id, func(ctx context.Context, tx pgx.Tx) error {
		return h.log(ctx, tx, r, audit.Entry{Action: "release.unpublish", TargetType: "release", TargetID: id, TargetLabel: releaseTitle(ctx, tx, id)})
	})
	if err != nil {
		reply(w, err)
		return
	}
	h.getRelease(w, r)
}

// POST /v1/admin/releases/{id}/uploads {title, genre?, trackNumber?, explicit?, filename, sizeBytes, rightsConfirmed: true} -> 201,
// the same body as POST /v1/uploads (where and how to send the file: one PUT, or presigned parts). The administrator
// attests that the rights to the audio are held; that attestation is stored with the upload and in the audit log.
// No storage quota is charged when the artist has no account.
func (h *Handler) createUpload(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title           string `json:"title"`
		Genre           string `json:"genre"`
		TrackNumber     *int   `json:"trackNumber"`
		Explicit        bool   `json:"explicit"`
		Filename        string `json:"filename"`
		SizeBytes       int64  `json:"sizeBytes"`
		RightsConfirmed bool   `json:"rightsConfirmed"`
	}
	if err := decodeBody(w, r, &in, false); err != nil {
		reply(w, err)
		return
	}
	if !in.RightsConfirmed {
		reply(w, fail4(http.StatusUnprocessableEntity, "rights_not_accepted", "confirm that you hold the rights to this audio (rightsConfirmed: true)"))
		return
	}
	if h.Uploads == nil {
		reply(w, fail4(http.StatusNotImplemented, "unavailable", "uploading is not available"))
		return
	}
	releaseID := param(r, "id")
	id, send, ok := h.Uploads.AdminCreate(w, r, actorID(r), upload.AdminCreateInput{
		ReleaseID: releaseID, Title: in.Title, Genre: in.Genre, TrackNumber: in.TrackNumber, Explicit: in.Explicit, Filename: in.Filename, SizeBytes: in.SizeBytes,
	})
	if !ok {
		return
	}
	if err := audit.Write(r.Context(), h.pool, audit.Entry{ActorID: actorID(r), Action: "upload.create", TargetType: "release", TargetID: releaseID,
		TargetLabel: strings.TrimSpace(in.Title), IP: audit.IP(r),
		Details: map[string]any{"uploadId": id, "filename": in.Filename, "sizeBytes": in.SizeBytes, "rightsConfirmed": true}}); err != nil {
		reply(w, err) // the upload stays reserved; it is cleaned up like any abandoned one
		return
	}
	send()
}

// OnUploadComplete is the audit row for an upload an administrator finished (set as upload.Handler.OnAdminComplete).
func (h *Handler) OnUploadComplete(r *http.Request, u *upload.Upload) {
	_ = audit.Write(r.Context(), h.pool, audit.Entry{ActorID: actorID(r), Action: "track.upload", TargetType: "release", TargetID: u.ReleaseID,
		TargetLabel: u.Title, IP: audit.IP(r),
		Details: map[string]any{"uploadId": u.ID, "filename": u.Filename, "sizeBytes": u.SizeBytes, "artistId": u.ArtistID}})
}
