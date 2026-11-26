package admin

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/blacmusic/backend/internal/artist"
	"github.com/blacmusic/backend/internal/audit"
	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/genre"
	"github.com/blacmusic/backend/internal/httpx"
	"github.com/blacmusic/backend/internal/release"
)

// apiError is an error with the HTTP answer to give.
type apiError struct {
	status int
	code   string
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func fail4(status int, code, msg string) error { return &apiError{status, code, msg} }

var (
	errNotFound      = fail4(http.StatusNotFound, "not_found", "not found")
	errIsAdmin       = fail4(http.StatusConflict, "is_admin", "administrators cannot be banned or deleted here: change their role first")
	errAlreadyBanned = fail4(http.StatusConflict, "already_banned", "already banned")
	errNotBanned     = fail4(http.StatusConflict, "not_banned", "not banned")
	errBusy          = fail4(http.StatusConflict, "busy", "a file is being processed right now; try again in a minute")
)

// reply writes err as the HTTP answer.
func reply(w http.ResponseWriter, err error) {
	var ae *apiError
	switch {
	case errors.As(err, &ae):
		httpx.Error(w, ae.status, ae.code, ae.msg)
	case errors.Is(err, pgx.ErrNoRows):
		httpx.Error(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, release.ErrProcessing):
		httpx.Error(w, http.StatusConflict, "busy", errBusy.Error())
	case errors.Is(err, release.ErrNotFound):
		httpx.Error(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, release.ErrNoTracks):
		httpx.Error(w, http.StatusUnprocessableEntity, "no_tracks", "the release has no finished tracks yet")
	case errors.Is(err, release.ErrUploadsPending):
		httpx.Error(w, http.StatusConflict, "uploads_pending", "some files are still uploading or processing; wait until they are ready")
	case errors.Is(err, release.ErrTakenDown):
		httpx.Error(w, http.StatusConflict, "taken_down", "this release was taken down: restore it first")
	case errors.Is(err, release.ErrArtistBanned):
		httpx.Error(w, http.StatusConflict, "artist_banned", "this artist is banned")
	case errors.Is(err, genre.ErrInvalid):
		httpx.Error(w, http.StatusUnprocessableEntity, "invalid_genre", err.Error())
	default:
		var ve *artist.ValidationError
		if errors.As(err, &ve) {
			httpx.Error(w, http.StatusUnprocessableEntity, "validation_error", ve.Msg)
			return
		}
		var rv *release.ValidationError
		if errors.As(err, &rv) {
			httpx.Error(w, http.StatusUnprocessableEntity, "validation_error", rv.Msg)
			return
		}
		slog.Error("admin", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal", "something went wrong")
	}
}

func invalid(msg string) error { return fail4(http.StatusUnprocessableEntity, "validation_error", msg) }

// reason checks the reason an administrator gives for a change (3-500 characters).
func reason(s string) (string, error) {
	s = strings.TrimSpace(s)
	if n := utf8.RuneCountInString(s); n < 3 || n > 500 {
		return "", invalid("reason must be 3-500 characters")
	}
	return s, nil
}

// optionalNote validates a note that may be empty (at most 500 characters).
func optionalNote(s string) (string, error) {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > 500 {
		return "", invalid("note must be at most 500 characters")
	}
	return s, nil
}

// decodeBody reads a JSON body. With optional, an empty body is fine.
func decodeBody(w http.ResponseWriter, r *http.Request, v any, optional bool) error {
	err := httpx.Decode(w, r, v)
	if err == nil || (optional && errors.Is(err, io.EOF)) {
		return nil
	}
	return fail4(http.StatusBadRequest, "bad_request", "invalid JSON body")
}

func page(r *http.Request) (limit, offset int) {
	limit = 25
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = min(n, 100)
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && n > 0 {
		offset = min(n, 1000000)
	}
	return
}

func param(r *http.Request, name string) string { return chi.URLParam(r, name) }

func like(s string) string {
	return "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.TrimSpace(s)) + "%"
}

func actorID(r *http.Request) string {
	id, _ := auth.UserID(r.Context())
	return id
}

// log writes an audit row inside tx on behalf of the signed-in administrator.
func (h *Handler) log(ctx context.Context, tx pgx.Tx, r *http.Request, e audit.Entry) error {
	e.ActorID = actorID(r)
	e.IP = audit.IP(r)
	return audit.Write(ctx, tx, e)
}

// ---------------------------------------------------------------- hiding and restoring releases

// takeDownArtistReleases hides every public or scheduled release of an artist like a draft, remembering what
// each was, because the artist was banned. Releases already taken down stay as they are.
func takeDownArtistReleases(ctx context.Context, tx pgx.Tx, artistID, adminID, why string) (int, error) {
	rows, err := tx.Query(ctx, `
		UPDATE albums SET taken_down_at = now(), takedown_reason = $3, taken_down_by = $2::uuid, taken_down_prev_visibility = visibility,
		       taken_down_prev_publish_at = publish_at, taken_down_by_ban = true, visibility = 'draft', publish_at = NULL, updated_at = now()
		WHERE artist_id = $1 AND visibility IN ('public', 'scheduled') AND taken_down_at IS NULL RETURNING id`, artistID, adminID, why)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(ids) > 0 {
		if _, err := tx.Exec(ctx, `UPDATE tracks SET visibility = 'draft' WHERE album_id = ANY($1)`, ids); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// restoreArtistReleases puts back the releases that were hidden because their artist was banned.
func restoreArtistReleases(ctx context.Context, tx pgx.Tx, artistID string) (int, error) {
	rows, err := tx.Query(ctx, `
		UPDATE albums SET visibility = COALESCE(taken_down_prev_visibility, 'draft'), publish_at = taken_down_prev_publish_at,
		       taken_down_at = NULL, takedown_reason = '', taken_down_by = NULL, taken_down_prev_visibility = NULL,
		       taken_down_prev_publish_at = NULL, taken_down_by_ban = false, updated_at = now()
		WHERE artist_id = $1 AND taken_down_by_ban RETURNING id, visibility`, artistID)
	if err != nil {
		return 0, err
	}
	var all, public []string
	for rows.Next() {
		var id, vis string
		if err := rows.Scan(&id, &vis); err != nil {
			rows.Close()
			return 0, err
		}
		all = append(all, id)
		if vis == "public" {
			public = append(public, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(public) > 0 { // a scheduled release's tracks stay hidden until it goes live
		if _, err := tx.Exec(ctx, `UPDATE tracks SET visibility = 'public' WHERE album_id = ANY($1)`, public); err != nil {
			return 0, err
		}
	}
	return len(all), nil
}
