// Package importer brings a whole artist catalogue in at once: a manifest of tracks, the files sent straight to
// storage, and a job that creates the artist, the releases and the tracks, and publishes them.
package importer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/blacmusic/backend/internal/audit"
	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/httpx"
)

// KeyPrefix starts every import key, so it is told apart from a sign-in token.
const KeyPrefix = "blac_imp_"

const (
	defaultKeyDays = 30
	maxKeyDays     = 365
)

type principalKey struct{}

// principal is who is calling the import routes: an administrator, directly or through one of their keys.
type principal struct {
	UserID  string
	KeyID   string
	KeyName string
}

func who(ctx context.Context) principal {
	p, _ := ctx.Value(principalKey{}).(principal)
	return p
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

func unauthorized(w http.ResponseWriter) {
	httpx.Error(w, http.StatusUnauthorized, "unauthorized", "Please sign in again.")
}

// Authenticate lets in an administrator's access token or an import key (Authorization: Bearer ...). A key works
// only while it is not expired or revoked and the administrator who made it still is one.
func (h *Handler) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || raw == "" {
			unauthorized(w)
			return
		}
		var p principal
		var err error
		if strings.HasPrefix(raw, KeyPrefix) {
			err = h.pool.QueryRow(r.Context(), `
				UPDATE api_keys k SET last_used_at = now() FROM users u
				WHERE k.token_hash = $1 AND k.revoked_at IS NULL AND k.expires_at > now()
				  AND u.id = k.created_by AND u.role = 'admin' AND u.banned_at IS NULL
				RETURNING k.created_by::text, k.id, k.name`, hashToken(raw)).Scan(&p.UserID, &p.KeyID, &p.KeyName)
			if errors.Is(err, pgx.ErrNoRows) {
				unauthorized(w)
				return
			}
		} else {
			uid, perr := h.tokens.ParseAccess(raw)
			if perr != nil {
				unauthorized(w)
				return
			}
			var admin bool
			err = h.pool.QueryRow(r.Context(), `SELECT role = 'admin' AND banned_at IS NULL FROM users WHERE id::text = $1`, uid).Scan(&admin)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && !admin) {
				httpx.Error(w, http.StatusForbidden, "forbidden", "administrators only")
				return
			}
			p.UserID = uid
		}
		if err != nil {
			serverError(w, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}

// KeyRoutes registers, on the admin router (a signed-in administrator, never a key):
//
//	GET    /api-keys
//	POST   /api-keys {name, expiresInDays?}   -> 201 with the key, shown this once
//	DELETE /api-keys/{id}                     -> 204 (revoked at once)
func (h *Handler) KeyRoutes(r chi.Router) {
	r.Get("/api-keys", h.listKeys)
	r.Post("/api-keys", h.createKey)
	r.Delete("/api-keys/{id}", h.revokeKey)
}

type keyView struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedBy  string     `json:"createdBy"`
	CreatedAt  time.Time  `json:"createdAt"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	RevokedAt  *time.Time `json:"revokedAt"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
}

func (h *Handler) listKeys(w http.ResponseWriter, r *http.Request) {
	rows, err := h.pool.Query(r.Context(), `
		SELECT k.id, k.name, k.prefix, u.email, k.created_at, k.expires_at, k.revoked_at, k.last_used_at
		FROM api_keys k JOIN users u ON u.id = k.created_by ORDER BY k.created_at DESC LIMIT 100`)
	if err != nil {
		serverError(w, err)
		return
	}
	defer rows.Close()
	out := []keyView{}
	for rows.Next() {
		var k keyView
		if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &k.CreatedBy, &k.CreatedAt, &k.ExpiresAt, &k.RevokedAt, &k.LastUsedAt); err != nil {
			serverError(w, err)
			return
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		serverError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"keys": out})
}

func (h *Handler) createKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name          string `json:"name"`
		ExpiresInDays *int   `json:"expiresInDays"`
	}
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if n := utf8.RuneCountInString(in.Name); n < 1 || n > 60 {
		invalid(w, "name must be 1-60 characters")
		return
	}
	days := defaultKeyDays
	if in.ExpiresInDays != nil {
		days = *in.ExpiresInDays
	}
	if days < 1 || days > maxKeyDays {
		invalid(w, "expiresInDays must be between 1 and 365")
		return
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		serverError(w, err)
		return
	}
	token := KeyPrefix + base64.RawURLEncoding.EncodeToString(secret)
	admin, _ := auth.UserID(r.Context())

	tx, err := h.pool.Begin(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck
	var id string
	var expires time.Time
	if err := tx.QueryRow(r.Context(), `
		INSERT INTO api_keys (name, prefix, token_hash, created_by, expires_at)
		VALUES ($1, $2, $3, $4::uuid, now() + make_interval(days => $5)) RETURNING id, expires_at`,
		in.Name, token[:len(KeyPrefix)+6], hashToken(token), admin, days).Scan(&id, &expires); err != nil {
		serverError(w, err)
		return
	}
	if err := audit.Write(r.Context(), tx, audit.Entry{ActorID: admin, Action: "apikey.create", TargetType: "apikey", TargetID: id, TargetLabel: in.Name,
		Details: map[string]any{"expiresAt": expires, "scope": "import"}, IP: audit.IP(r)}); err != nil {
		serverError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		serverError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusCreated, map[string]any{"id": id, "name": in.Name, "prefix": token[:len(KeyPrefix)+6], "expiresAt": expires, "scope": "import", "key": token})
}

func (h *Handler) revokeKey(w http.ResponseWriter, r *http.Request) {
	admin, _ := auth.UserID(r.Context())
	tx, err := h.pool.Begin(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck
	id := chi.URLParam(r, "id")
	var name string
	err = tx.QueryRow(r.Context(), `UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL RETURNING name`, id).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, http.StatusNotFound, "not_found", "no such active key")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	if err := audit.Write(r.Context(), tx, audit.Entry{ActorID: admin, Action: "apikey.revoke", TargetType: "apikey", TargetID: id, TargetLabel: name, IP: audit.IP(r)}); err != nil {
		serverError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func invalid(w http.ResponseWriter, msg string) {
	httpx.Error(w, http.StatusUnprocessableEntity, "validation_error", msg)
}

func serverError(w http.ResponseWriter, err error) {
	slog.Error("import", "err", err)
	httpx.Error(w, http.StatusInternalServerError, "internal", "something went wrong")
}
