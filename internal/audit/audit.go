// Package audit is the append-only log of what administrators did. Rows are written inside the
// transaction of the change they record, so the log and the data can never disagree.
package audit

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Exec is what Write needs; a pool and a transaction both satisfy it.
type Exec interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Entry is one thing an administrator did.
type Entry struct {
	ActorID     string // the administrator's user id ("" when nobody was signed in, e.g. a failed login)
	ActorEmail  string // used when ActorID is empty
	Action      string // user.ban, release.takedown, login_failed, ...
	TargetType  string // user, artist, release, track, playlist, ""
	TargetID    string
	TargetLabel string // the name or email at the time
	Reason      string
	Details     map[string]any
	IP          string
}

// Write appends an entry.
func Write(ctx context.Context, db Exec, e Entry) error {
	details := []byte("{}")
	if len(e.Details) > 0 {
		b, err := json.Marshal(e.Details)
		if err != nil {
			return err
		}
		details = b
	}
	var actor *string
	if e.ActorID != "" {
		actor = &e.ActorID
	}
	_, err := db.Exec(ctx, `
		INSERT INTO admin_audit_log (actor_user_id, actor_email, action, target_type, target_id, target_label, reason, details, ip)
		VALUES ($1::uuid, COALESCE((SELECT email FROM users WHERE id = $1::uuid), $2), $3, $4, $5, $6, $7, $8, $9)`,
		actor, e.ActorEmail, e.Action, e.TargetType, e.TargetID, e.TargetLabel, e.Reason, details, e.IP)
	return err
}

// IP is the address a request came from (chi's RealIP has already applied X-Forwarded-For when the proxy is trusted).
func IP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Row is an entry as read back.
type Row struct {
	ID          int64          `json:"id"`
	At          time.Time      `json:"at"`
	Actor       *Actor         `json:"actor"`
	Action      string         `json:"action"`
	TargetType  string         `json:"targetType"`
	TargetID    string         `json:"targetId"`
	TargetLabel string         `json:"targetLabel"`
	Reason      string         `json:"reason"`
	Details     map[string]any `json:"details"`
	IP          string         `json:"ip"`
}

// Actor is who did it.
type Actor struct {
	ID    string `json:"id,omitempty"`
	Email string `json:"email"`
}

// Filter narrows a listing; empty fields match everything.
type Filter struct {
	Actor      string // an administrator's user id or (part of) their email
	Action     string
	TargetType string
	TargetID   string
	From, To   *time.Time
}

// List returns entries newest first and how many match the filter.
func List(ctx context.Context, pool *pgxpool.Pool, f Filter, limit, offset int) ([]Row, int, error) {
	where := ` WHERE ($1 = '' OR actor_user_id::text = $1 OR actor_email ILIKE '%' || $1 || '%')
		AND ($2 = '' OR action = $2 OR action LIKE $2 || '.%') AND ($3 = '' OR target_type = $3) AND ($4 = '' OR target_id = $4)
		AND ($5::timestamptz IS NULL OR at >= $5) AND ($6::timestamptz IS NULL OR at < $6)`
	args := []any{f.Actor, f.Action, f.TargetType, f.TargetID, f.From, f.To}
	var total int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM admin_audit_log`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := pool.Query(ctx, `
		SELECT id, at, COALESCE(actor_user_id::text, ''), actor_email, action, target_type, target_id, target_label, reason, details, ip
		FROM admin_audit_log`+where+` ORDER BY at DESC, id DESC LIMIT $7 OFFSET $8`, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Row{}
	for rows.Next() {
		var r Row
		var actorID, actorEmail string
		var raw []byte
		if err := rows.Scan(&r.ID, &r.At, &actorID, &actorEmail, &r.Action, &r.TargetType, &r.TargetID, &r.TargetLabel, &r.Reason, &raw, &r.IP); err != nil {
			return nil, 0, err
		}
		r.Actor = &Actor{ID: actorID, Email: actorEmail}
		r.Details = map[string]any{}
		_ = json.Unmarshal(raw, &r.Details)
		out = append(out, r)
	}
	return out, total, rows.Err()
}
