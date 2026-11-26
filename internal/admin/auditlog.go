package admin

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/blacmusic/backend/internal/audit"
	"github.com/blacmusic/backend/internal/httpx"
)

func (h *Handler) auditRoutes(r chi.Router) {
	r.Get("/audit-log", h.auditLog)
}

// GET /v1/admin/audit-log?actor=&action=&targetType=&targetId=&from=&to=&limit=&offset=
// Newest first. actor is an administrator's id or part of their email; action matches exactly or as a
// group ("user" matches user.ban and user.unban); from/to are YYYY-MM-DD (to is included).
func (h *Handler) auditLog(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := audit.Filter{Actor: q.Get("actor"), Action: q.Get("action"), TargetType: q.Get("targetType"), TargetID: q.Get("targetId")}
	if v := q.Get("from"); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			reply(w, fail4(http.StatusBadRequest, "bad_request", "from must look like 2026-11-01"))
			return
		}
		f.From = &t
	}
	if v := q.Get("to"); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			reply(w, fail4(http.StatusBadRequest, "bad_request", "to must look like 2026-11-30"))
			return
		}
		t = t.AddDate(0, 0, 1)
		f.To = &t
	}
	limit, offset := page(r)
	entries, total, err := audit.List(r.Context(), h.pool, f, limit, offset)
	if err != nil {
		reply(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"entries": entries, "total": total})
}
