package admin

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/blacmusic/backend/internal/audit"
)

// The albums and EPs featured in the app. They are listed, in order, by GET /releases?featured=true.
func (h *Handler) featuredRoutes(r chi.Router) {
	r.Put("/releases/{id}/featured", h.feature)
	r.Delete("/releases/{id}/featured", h.unfeature)
	r.Put("/featured-releases/order", h.orderFeatured)
}

// inFeatured runs change with the featured list locked, so two administrators cannot hand out the same place.
func (h *Handler) inFeatured(ctx context.Context, change func(tx pgx.Tx) error) error {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('featured_releases'))`); err != nil {
		return err
	}
	if err := change(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// PUT /v1/admin/releases/{id}/featured: adds an album or EP to the end of the featured list.
func (h *Handler) feature(w http.ResponseWriter, r *http.Request) { h.setFeatured(w, r, true) }

// DELETE /v1/admin/releases/{id}/featured
func (h *Handler) unfeature(w http.ResponseWriter, r *http.Request) { h.setFeatured(w, r, false) }

func (h *Handler) setFeatured(w http.ResponseWriter, r *http.Request, on bool) {
	ctx, id := r.Context(), param(r, "id")
	err := h.inFeatured(ctx, func(tx pgx.Tx) error {
		var title, kind string
		var is, takenDown, banned bool
		if err := tx.QueryRow(ctx, `
			SELECT al.title, al.release_type, al.featured_position IS NOT NULL, al.taken_down_at IS NOT NULL, COALESCE(a.banned_at IS NOT NULL, false)
			FROM albums al LEFT JOIN artists a ON a.id = al.artist_id WHERE al.id = $1 FOR UPDATE OF al`, id).
			Scan(&title, &kind, &is, &takenDown, &banned); err != nil {
			return err
		}
		if is == on {
			return nil // already so: nothing to record
		}
		action, change := "release.unfeature", `UPDATE albums SET featured_position = NULL WHERE id = $1`
		if on {
			switch {
			case kind == "single":
				return fail4(http.StatusConflict, "not_eligible", "only an album or EP can be featured")
			case banned:
				return fail4(http.StatusConflict, "artist_banned", "this artist is banned")
			case takenDown:
				return fail4(http.StatusConflict, "taken_down", "this release was taken down: restore it first")
			}
			action, change = "release.feature", `UPDATE albums SET featured_position =
				(SELECT COALESCE(max(featured_position), 0) + 1 FROM albums) WHERE id = $1`
		}
		if _, err := tx.Exec(ctx, change, id); err != nil {
			return err
		}
		return h.log(ctx, tx, r, audit.Entry{Action: action, TargetType: "release", TargetID: id, TargetLabel: title})
	})
	if err != nil {
		reply(w, err)
		return
	}
	h.getRelease(w, r)
}

// PUT /v1/admin/featured-releases/order {releaseIds}: every featured release once, in the new order.
func (h *Handler) orderFeatured(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ReleaseIDs []string `json:"releaseIds"`
	}
	if err := decodeBody(w, r, &in, false); err != nil {
		reply(w, err)
		return
	}
	ctx := r.Context()
	err := h.inFeatured(ctx, func(tx pgx.Tx) error {
		var complete bool
		if err := tx.QueryRow(ctx, `
			WITH have AS (SELECT id FROM albums WHERE featured_position IS NOT NULL), named AS (SELECT unnest($1::text[]) AS id)
			SELECT (SELECT count(*) FROM named) = (SELECT count(*) FROM have)
			   AND (SELECT count(DISTINCT id) FROM named) = (SELECT count(*) FROM have)
			   AND NOT EXISTS (SELECT id FROM have EXCEPT SELECT id FROM named)`, in.ReleaseIDs).Scan(&complete); err != nil {
			return err
		}
		if !complete {
			return invalid("releaseIds must list every featured release exactly once")
		}
		if _, err := tx.Exec(ctx, `UPDATE albums al SET featured_position = n.number
			FROM unnest($1::text[]) WITH ORDINALITY AS n(id, number) WHERE al.id = n.id`, in.ReleaseIDs); err != nil {
			return err
		}
		return h.log(ctx, tx, r, audit.Entry{Action: "release.featured_order", TargetType: "release", TargetLabel: "Featured releases",
			Details: map[string]any{"releaseIds": in.ReleaseIDs}})
	})
	if err != nil {
		reply(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
