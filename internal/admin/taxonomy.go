package admin

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/blacmusic/backend/internal/audit"
	"github.com/blacmusic/backend/internal/genre"
	"github.com/blacmusic/backend/internal/httpx"
)

// Managing the catalogue's terms. Nothing is deleted: a term is switched off, and songs that have it keep it.
// A term's id never changes.

type termKind struct {
	table     string
	up        string // the column that points at a group or a parent
	upField   string // and its name in the API
	hasActive bool
	longest   int
}

var termKinds = map[string]termKind{
	"language-groups": {table: "tax_language_groups", longest: 60},
	"languages":       {table: "tax_languages", up: "group_id", upField: "groupId", hasActive: true, longest: 60},
	"cultures":        {table: "tax_cultures", hasActive: true, longest: 60},
	"regions":         {table: "tax_regions", hasActive: true, longest: 60},
	// a genre's name is also what songs, releases and uploads store, where it is at most 40 characters
	"genres": {table: "tax_genres", up: "parent_id", upField: "parentId", hasActive: true, longest: 40},
}

// genreColumns hold a genre by name, so a renamed genre is renamed in each.
var genreColumns = [][2]string{{"tracks", "genre"}, {"albums", "genre"}, {"uploads", "genre"}, {"transfer_items", "genre"},
	{"import_items", "genre"}, {"import_jobs", "album_genre"}}

type termInput struct {
	Label    *string         `json:"label"`
	GroupID  json.RawMessage `json:"groupId"`
	ParentID json.RawMessage `json:"parentId"`
	Active   *bool           `json:"active"`
}

type termState struct {
	label  string
	up     *string
	active bool
}

// view is the term as GET /taxonomy shows it.
func (k termKind) view(id string, s termState) map[string]any {
	out := map[string]any{"id": id, "label": s.label}
	if k.hasActive {
		out["active"] = s.active
	}
	if k.up != "" {
		out[k.upField] = s.up
	}
	return out
}

// slug makes an id from a name.
func slug(label, join string) string {
	words := strings.FieldsFunc(strings.ToLower(strings.ReplaceAll(label, "'", "")), func(r rune) bool {
		return !(unicode.IsDigit(r) || r >= 'a' && r <= 'z')
	})
	if len(words) == 0 {
		return "term"
	}
	return strings.Join(words, join)
}

func freeID(ctx context.Context, tx pgx.Tx, table, column, base, join string) (string, error) {
	for n := 1; ; n++ {
		id := base
		if n > 1 {
			id += join + strconv.Itoa(n)
		}
		var taken bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM "+table+" WHERE "+column+" = $1)", id).Scan(&taken); err != nil || !taken {
			return id, err
		}
	}
}

// check validates the request against the term it makes (id "" when new).
func (k termKind) check(ctx context.Context, tx pgx.Tx, id string, in termInput, s *termState) error {
	own, other := in.GroupID, in.ParentID
	if k.upField == "parentId" {
		own, other = in.ParentID, in.GroupID
	}
	if other != nil || (k.up == "" && own != nil) || (!k.hasActive && in.Active != nil) {
		return invalid("this kind of term does not have one of those fields")
	}
	if in.Label != nil {
		s.label = strings.TrimSpace(*in.Label)
		if n := utf8.RuneCountInString(s.label); n < 1 || n > k.longest {
			return invalid("label must be 1-" + strconv.Itoa(k.longest) + " characters")
		}
		var taken bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM "+k.table+" WHERE lower(label) = lower($1) AND id <> $2)", s.label, id).Scan(&taken); err != nil {
			return err
		}
		if taken {
			return fail4(http.StatusConflict, "duplicate_label", "there is already one with that name (it may be switched off)")
		}
	}
	if in.Active != nil {
		s.active = *in.Active
	}
	if own == nil {
		return nil
	}
	var up *string
	if err := json.Unmarshal(own, &up); err != nil || (up != nil && *up == "") {
		return invalid(k.upField + " is an id or null")
	}
	s.up = up
	if up == nil {
		return nil
	}
	if k.upField == "groupId" {
		var there bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tax_language_groups WHERE id = $1)`, *up).Scan(&there); err != nil {
			return err
		}
		if !there {
			return invalid("unknown language group")
		}
		return nil
	}
	// subgenres are one level deep
	var ok bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tax_genres WHERE id = $1 AND parent_id IS NULL AND id <> $2)
		AND NOT EXISTS (SELECT 1 FROM tax_genres WHERE parent_id = $2)`, *up, id).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return fail4(http.StatusUnprocessableEntity, "invalid_parent",
			"the parent must be another genre without a parent, and a genre with subgenres cannot have one")
	}
	return nil
}

// inTaxonomy runs change with the terms locked, so two administrators cannot make the same one.
func (h *Handler) inTaxonomy(ctx context.Context, change func(tx pgx.Tx) error) error {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('taxonomy'))`); err != nil {
		return err
	}
	if err := change(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// genresChanged makes this process use the new list at once; the others pick it up shortly.
func (h *Handler) genresChanged(ctx context.Context) {
	if err := genre.Reload(context.WithoutCancel(ctx), h.pool); err != nil {
		slog.Warn("genres not reloaded", "err", err)
	}
}

func (h *Handler) termRequest(w http.ResponseWriter, r *http.Request) (string, termKind, termInput, bool) {
	name := chi.URLParam(r, "kind")
	k, ok := termKinds[name]
	var in termInput
	if !ok {
		reply(w, errNotFound)
		return name, k, in, false
	}
	if err := decodeBody(w, r, &in, false); err != nil {
		reply(w, err)
		return name, k, in, false
	}
	return name, k, in, true
}

// POST /v1/admin/taxonomy/{kind} {label, groupId?, parentId?}
func (h *Handler) createTerm(w http.ResponseWriter, r *http.Request) {
	name, k, in, ok := h.termRequest(w, r)
	if !ok {
		return
	}
	if in.Label == nil || in.Active != nil {
		reply(w, invalid("send a label (new terms are active)"))
		return
	}
	ctx := r.Context()
	var id string
	s := termState{active: true}
	err := h.inTaxonomy(ctx, func(tx pgx.Tx) error {
		if err := k.check(ctx, tx, "", in, &s); err != nil {
			return err
		}
		var err error
		if id, err = freeID(ctx, tx, k.table, "id", slug(s.label, "_"), "_"); err != nil {
			return err
		}
		columns, values, args := "id, label", "$1, $2", []any{id, s.label}
		if k.up != "" {
			args = append(args, s.up)
			columns, values = columns+", "+k.up, values+", $"+strconv.Itoa(len(args))
		}
		if name == "genres" {
			own, err := freeID(ctx, tx, "tax_genres", "slug", slug(s.label, "-"), "-")
			if err != nil {
				return err
			}
			args = append(args, own)
			columns, values = columns+", slug", values+", $"+strconv.Itoa(len(args))
			// a new genre goes before "Other", which stays last, and its name is no longer another's earlier one
			if _, err := tx.Exec(ctx, `UPDATE tax_genres SET position = position + 1 WHERE id = 'other'`); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM tax_genre_aliases WHERE label = lower($1)`, s.label); err != nil {
				return err
			}
		}
		position := "(SELECT COALESCE(max(position), 0) + 1 FROM " + k.table + ")"
		if name == "genres" {
			position = "(SELECT COALESCE(max(position), 0) + 1 FROM tax_genres WHERE id <> 'other')"
		}
		if _, err := tx.Exec(ctx, "INSERT INTO "+k.table+" ("+columns+", position) VALUES ("+values+", "+position+")", args...); err != nil {
			return err
		}
		return h.log(ctx, tx, r, audit.Entry{Action: "taxonomy.create", TargetType: "taxonomy", TargetID: id, TargetLabel: s.label,
			Details: map[string]any{"kind": name, "term": k.view(id, s)}})
	})
	if err != nil {
		reply(w, err)
		return
	}
	if name == "genres" {
		h.genresChanged(ctx)
	}
	httpx.JSON(w, http.StatusCreated, k.view(id, s))
}

// PATCH /v1/admin/taxonomy/{kind}/{id} {label?, groupId?, parentId?, active?}: only what is sent changes.
func (h *Handler) updateTerm(w http.ResponseWriter, r *http.Request) {
	name, k, in, ok := h.termRequest(w, r)
	if !ok {
		return
	}
	ctx, id := r.Context(), param(r, "id")
	var s termState
	err := h.inTaxonomy(ctx, func(tx pgx.Tx) error {
		up, active := "NULL::text", "true"
		if k.up != "" {
			up = k.up
		}
		if k.hasActive {
			active = "active"
		}
		err := tx.QueryRow(ctx, "SELECT label, "+up+", "+active+" FROM "+k.table+" WHERE id = $1 FOR UPDATE", id).Scan(&s.label, &s.up, &s.active)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		was := s
		if err := k.check(ctx, tx, id, in, &s); err != nil {
			return err
		}
		changed := map[string]any{}
		set, args := []string{}, []any{id}
		change := func(field, column string, before, after any) {
			args = append(args, after)
			set = append(set, column+" = $"+strconv.Itoa(len(args)))
			changed[field] = map[string]any{"before": before, "after": after}
		}
		if s.label != was.label {
			change("label", "label", was.label, s.label)
		}
		if deref(s.up) != deref(was.up) {
			change(k.upField, k.up, was.up, s.up)
		}
		if s.active != was.active {
			change("active", "active", was.active, s.active)
		}
		if len(set) == 0 {
			return nil
		}
		if _, err := tx.Exec(ctx, "UPDATE "+k.table+" SET "+strings.Join(set, ", ")+" WHERE id = $1", args...); err != nil {
			return err
		}
		if name == "genres" && s.label != was.label {
			if err := renameGenre(ctx, tx, id, was.label, s.label); err != nil {
				return err
			}
		}
		return h.log(ctx, tx, r, audit.Entry{Action: "taxonomy.update", TargetType: "taxonomy", TargetID: id, TargetLabel: s.label,
			Details: map[string]any{"kind": name, "changed": changed}})
	})
	if err != nil {
		reply(w, err)
		return
	}
	if name == "genres" {
		h.genresChanged(ctx)
	}
	httpx.JSON(w, http.StatusOK, k.view(id, s))
}

// renameGenre carries a genre's new name to everything that stores it by name, and keeps the old one understood.
// The genre already has its new name, so the trigger on tracks sees the same genre.
func renameGenre(ctx context.Context, tx pgx.Tx, id, was, is string) error {
	for _, c := range genreColumns {
		if _, err := tx.Exec(ctx, "UPDATE "+c[0]+" SET "+c[1]+" = $2 WHERE lower("+c[1]+") = lower($1)", was, is); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO tax_genre_aliases (label, genre_id) VALUES (lower($1), $2)
		ON CONFLICT (label) DO UPDATE SET genre_id = $2`, was, id); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `DELETE FROM tax_genre_aliases WHERE label = lower($1)`, is)
	return err
}
