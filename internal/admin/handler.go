// Package admin holds the endpoints only administrators may call. For now:
// the verification review queue and manual storage grants.
package admin

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/artist"
	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/httpx"
)

// Handler serves /v1/admin/*.
type Handler struct {
	pool    *pgxpool.Pool
	artists *artist.Store
}

// NewHandler creates the handler.
func NewHandler(pool *pgxpool.Pool, artists *artist.Store) *Handler {
	return &Handler{pool: pool, artists: artists}
}

// Routes registers the routes on r (already authenticated).
func (h *Handler) Routes(r chi.Router) {
	r.Use(h.requireAdmin)
	r.Get("/artists", h.queue)
	r.Post("/artists/{id}/verification", h.decide)
	r.Post("/artists/{id}/storage-grants", h.grant)
}

// The role is read from the database on every request, so demoting an admin
// takes effect immediately instead of when their token expires.
func (h *Handler) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid, _ := auth.UserID(r.Context())
		var role string
		err := h.pool.QueryRow(r.Context(), `SELECT role FROM users WHERE id = $1`, uid).Scan(&role)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && role != "admin") {
			httpx.Error(w, http.StatusForbidden, "forbidden", "administrators only")
			return
		}
		if err != nil {
			slog.Error("admin role lookup", "err", err)
			httpx.Error(w, http.StatusInternalServerError, "internal", "something went wrong")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// GET /v1/admin/artists?status=pending|flagged
func (h *Handler) queue(w http.ResponseWriter, r *http.Request) {
	filter := r.URL.Query().Get("status")
	if filter == "" {
		filter = "pending"
	}
	if filter != "pending" && filter != "flagged" {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "status must be pending or flagged")
		return
	}
	limit, offset := 50, 0
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = min(n, 200)
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && n > 0 {
		offset = min(n, 100000)
	}
	list, err := h.artists.ReviewQueue(r.Context(), filter, limit, offset)
	if err != nil {
		artist.Fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"artists": list})
}

// POST /v1/admin/artists/{id}/verification {decision: approve|reject, note}
func (h *Handler) decide(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Decision string `json:"decision"`
		Note     string `json:"note"`
	}
	if err := httpx.Decode(w, r, &req); err != nil || (req.Decision != "approve" && req.Decision != "reject") {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "decision must be approve or reject")
		return
	}
	if err := h.artists.Decide(r.Context(), chi.URLParam(r, "id"), req.Decision == "approve", req.Note); err != nil {
		artist.Fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /v1/admin/artists/{id}/storage-grants {kind, bytes, note, reference, expiresAt}
// A repeated reference changes nothing (200), so payment webhooks can retry safely.
func (h *Handler) grant(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind      string     `json:"kind"`
		Bytes     int64      `json:"bytes"`
		Note      string     `json:"note"`
		Reference string     `json:"reference"`
		ExpiresAt *time.Time `json:"expiresAt"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	created, err := h.artists.Grant(r.Context(), chi.URLParam(r, "id"), req.Kind, req.Bytes, req.Note, req.Reference, req.ExpiresAt)
	if err != nil {
		artist.Fail(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	httpx.JSON(w, status, map[string]any{"created": created})
}
