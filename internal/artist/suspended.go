package artist

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/httpx"
)

// CodeSuspended is the error code of a studio request from a suspended artist. It is not "account_banned": the
// account still works for listening, so an app must not sign the person out.
const CodeSuspended = "artist_banned"

// StudioWrite reports whether a request changes something in the artist studio. path is the URL path, with or
// without the /v1 prefix. Reading is never a studio write: the owner can still see what they have.
//
// Every non-GET route of the studio must be covered here (a test walks the routes).
func StudioWrite(method, path string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	path = strings.Trim(strings.TrimPrefix(path, "/v1/"), "/")
	seg := strings.Split(path, "/")
	switch seg[0] {
	case "releases", "albums", "tracks", "uploads", "transfers":
		return true
	case "me": // taking back profile changes sent for review
		return len(seg) > 1 && seg[1] == "artist"
	case "artists": // following and unfollowing other artists is listening, not studio work
		return len(seg) < 3 || seg[2] == "verification-request"
	case "integrations": // disconnecting YouTube (DELETE) must always work: it is the person's own data
		return len(seg) > 1 && seg[1] == "youtube" && method != http.MethodDelete
	}
	return false
}

// SuspendedGuard refuses studio changes from the owner of a suspended artist profile (403 artist_banned). It goes
// after authentication. The ban is read on every studio change, so it takes effect at once.
func SuspendedGuard(pool *pgxpool.Pool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !StudioWrite(r.Method, r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			uid, _ := auth.UserID(r.Context())
			var suspended bool
			if err := pool.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM artists WHERE owner_user_id::text = $1 AND banned_at IS NOT NULL)`, uid).
				Scan(&suspended); err != nil {
				slog.Error("suspended artist check", "err", err)
				httpx.Error(w, http.StatusInternalServerError, "internal", "something went wrong")
				return
			}
			if suspended {
				httpx.Error(w, http.StatusForbidden, CodeSuspended, "this artist profile has been suspended")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
