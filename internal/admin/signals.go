package admin

import (
	"net/http"

	"github.com/blacmusic/backend/internal/audit"
)

// Staff and test accounts are marked here so that what they do does not shape what other listeners are offered
// (the signal_* views leave them out). Nothing else about the account changes: it works as before, and its own
// recommendations still follow what it does. Administrators are always left out.

// PUT /v1/admin/users/{id}/signals {excluded} -> 204
func (h *Handler) setUserSignals(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Excluded *bool `json:"excluded"`
	}
	if err := decodeBody(w, r, &in, false); err != nil {
		reply(w, err)
		return
	}
	if in.Excluded == nil {
		reply(w, fail4(http.StatusBadRequest, "bad_request", "send {excluded: true or false}"))
		return
	}
	ctx := r.Context()
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		reply(w, err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	id, email, role, _, err := lockUser(ctx, tx, param(r, "id"))
	if err != nil {
		reply(w, err)
		return
	}
	if role == "admin" && !*in.Excluded {
		reply(w, fail4(http.StatusConflict, "is_admin", "administrators are always left out of recommendation signals"))
		return
	}
	tag, err := tx.Exec(ctx, `
		UPDATE users SET signals_excluded_at = CASE WHEN $2 THEN now() END, signals_excluded_by = CASE WHEN $2 THEN $3::uuid END
		WHERE id = $1::uuid AND (signals_excluded_at IS NOT NULL) <> $2`, id, *in.Excluded, actorID(r))
	if err != nil {
		reply(w, err)
		return
	}
	if tag.RowsAffected() > 0 { // asking for what is already so changes nothing
		action := "user.signals_include"
		if *in.Excluded {
			action = "user.signals_exclude"
		}
		if err := h.log(ctx, tx, r, audit.Entry{Action: action, TargetType: "user", TargetID: id, TargetLabel: email}); err != nil {
			reply(w, err)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		reply(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
