package admin

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/blacmusic/backend/internal/audit"
	"github.com/blacmusic/backend/internal/httpx"
)

// Bulk changes touch only the songs and the lists the administrator names. A dry run does the whole change and
// takes it back, so what it shows is what will happen.

const bulkMax = 100

type bulkRef struct {
	Category string `json:"category"`
	ID       string `json:"id"`
}

type bulkRequest struct {
	TrackIDs []string          `json:"trackIds"`
	DryRun   bool              `json:"dryRun"`
	Versions map[string]string `json:"versions"`
}

// tracksError names the songs that stopped a bulk change.
type tracksError struct {
	status    int
	code, msg string
	trackIDs  []string
}

func (e *tracksError) Error() string { return e.msg }

// inBulk locks the songs, checks that the caller has seen their latest versions, runs one on each in the order asked,
// and keeps the result unless this is a dry run. All or nothing.
func (h *Handler) inBulk(ctx context.Context, in bulkRequest, one func(tx pgx.Tx, now *trackMetadata) error) error {
	unique := map[string]bool{}
	for _, id := range in.TrackIDs {
		unique[id] = true
	}
	if len(in.TrackIDs) == 0 || len(in.TrackIDs) > bulkMax || len(unique) != len(in.TrackIDs) {
		return invalid("trackIds must list 1-100 different songs")
	}
	if !in.DryRun {
		for _, id := range in.TrackIDs {
			if in.Versions[id] == "" {
				return invalid("versions must give the version of every song, from the preview")
			}
		}
	}
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// locked in one order, so two bulk changes cannot block each other for good
	rows, err := tx.Query(ctx, `SELECT id FROM tracks WHERE id = ANY($1) ORDER BY id FOR UPDATE`, in.TrackIDs)
	if err != nil {
		return err
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	if len(found) != len(unique) {
		for _, id := range found {
			delete(unique, id)
		}
		var missing []string
		for id := range unique {
			missing = append(missing, id)
		}
		slices.Sort(missing)
		return &tracksError{http.StatusNotFound, "not_found", "some songs were not found", missing}
	}
	songs := make([]*trackMetadata, len(in.TrackIDs))
	var stale []string
	for i, id := range in.TrackIDs {
		list, err := loadMetadata(ctx, tx, "t.id = $1", id)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			return errNotFound
		}
		songs[i] = list[0]
		if !in.DryRun && in.Versions[id] != list[0].Version {
			stale = append(stale, id)
		}
	}
	if len(stale) > 0 {
		return &tracksError{http.StatusConflict, "stale_version", "some songs changed since the preview: preview again", stale}
	}
	for _, now := range songs {
		if err := one(tx, now); err != nil {
			return err
		}
	}
	if in.DryRun {
		return nil // rolled back
	}
	return tx.Commit(ctx)
}

func replyBulk(w http.ResponseWriter, err error) {
	var te *tracksError
	if errors.As(err, &te) {
		httpx.ErrorDetails(w, te.status, te.code, te.msg, map[string]any{"trackIds": te.trackIDs})
		return
	}
	reply(w, err)
}

func versionOf(ctx context.Context, tx pgx.Tx, trackID string) (version string, err error) {
	err = tx.QueryRow(ctx, `SELECT COALESCE((SELECT version FROM track_metadata WHERE track_id = $1), 0)::text`, trackID).Scan(&version)
	return version, err
}

type bulkValue struct {
	ID      string
	Primary bool
}

// bulkList is the change to one list.
type bulkList struct {
	name    string
	replace bool
	values  []bulkValue
}

// apply gives the song's list as it will be: adding keeps everything there, with its main one.
func (l bulkList) apply(have map[string]bool) map[string]bool {
	want := map[string]bool{}
	if l.replace {
		for _, v := range l.values {
			want[v.ID] = v.Primary
		}
		return want
	}
	for id, main := range have {
		want[id] = main
	}
	hasMain := mainOf(have) != ""
	for _, v := range l.values {
		if _, there := want[v.ID]; there {
			continue
		}
		want[v.ID] = v.Primary && !hasMain
		hasMain = hasMain || want[v.ID]
	}
	return want
}

var bulkCategories = map[string]category{
	"languages": {name: "languages", table: "track_languages", column: "language_id", terms: "tax_languages"},
	"genres":    {name: "genres", table: "track_genres", column: "genre_id", terms: "tax_genres"},
	"regions":   {name: "regions", table: "track_regions", column: "region_id", terms: "tax_regions"},
}

func currentList(now *trackMetadata, name string) map[string]bool {
	have := map[string]bool{}
	switch name {
	case "languages":
		for _, l := range now.Languages {
			have[l.ID] = l.Role == "primary"
		}
	case "genres":
		for _, g := range now.Genres {
			have[g.ID] = g.Primary
		}
	case "regions":
		for _, l := range now.Regions {
			have[l.ID] = false
		}
	}
	return have
}

type bulkBody struct {
	bulkRequest
	Changes struct {
		Languages *struct {
			Mode   string `json:"mode"`
			Values []struct {
				ID   string `json:"id"`
				Role string `json:"role"`
			} `json:"values"`
		} `json:"languages"`
		Genres *struct {
			Mode   string `json:"mode"`
			Values []struct {
				ID      string `json:"id"`
				Primary bool   `json:"primary"`
			} `json:"values"`
		} `json:"genres"`
		Regions *struct {
			Mode string   `json:"mode"`
			IDs  []string `json:"ids"`
		} `json:"regions"`
	} `json:"changes"`
}

// lists checks the changes asked for.
func (b *bulkBody) lists() ([]bulkList, error) {
	var out []bulkList
	var err error
	add := func(name, mode string, values []bulkValue) {
		seen, mains := map[string]bool{}, 0
		for _, v := range values {
			if v.ID == "" || seen[v.ID] {
				err = invalid(name + " has an empty or repeated value")
			}
			seen[v.ID] = true
			if v.Primary {
				mains++
			}
		}
		switch {
		case mode != "add" && mode != "replace":
			err = invalid(name + ": mode is add or replace")
		case len(values) == 0:
			err = invalid(name + " needs at least one value: nothing is cleared in bulk")
		case mains > 1:
			err = invalid(name + " takes at most one primary")
		}
		out = append(out, bulkList{name, mode == "replace", values})
	}
	c := b.Changes
	if c.Languages != nil {
		var values []bulkValue
		for _, v := range c.Languages.Values {
			if v.Role != "primary" && v.Role != "secondary" {
				return nil, invalid("a language's role is primary or secondary")
			}
			values = append(values, bulkValue{v.ID, v.Role == "primary"})
		}
		add("languages", c.Languages.Mode, values)
	}
	if c.Genres != nil {
		var values []bulkValue
		for _, v := range c.Genres.Values {
			values = append(values, bulkValue{v.ID, v.Primary})
		}
		add("genres", c.Genres.Mode, values)
	}
	if c.Regions != nil {
		var values []bulkValue
		for _, id := range c.Regions.IDs {
			values = append(values, bulkValue{ID: id})
		}
		add("regions", c.Regions.Mode, values)
	}
	if len(out) == 0 {
		return nil, invalid("changes must name languages, genres or regions")
	}
	return out, err
}

// POST /v1/admin/metadata/bulk
func (h *Handler) bulkMetadata(w http.ResponseWriter, r *http.Request) {
	var in bulkBody
	if err := decodeBody(w, r, &in, false); err != nil {
		reply(w, err)
		return
	}
	lists, err := in.lists()
	if err != nil {
		reply(w, err)
		return
	}
	ctx := r.Context()
	// an id that does not exist is refused even when every song would be skipped
	for _, l := range lists {
		ids := make([]string, len(l.values))
		for i, v := range l.values {
			ids[i] = v.ID
		}
		var known int
		if err := h.pool.QueryRow(ctx, "SELECT count(*) FROM "+bulkCategories[l.name].terms+" WHERE id = ANY($1)", ids).Scan(&known); err != nil {
			reply(w, err)
			return
		}
		if known != len(ids) {
			reply(w, fail4(http.StatusUnprocessableEntity, "invalid_taxonomy", l.name+": a value that does not exist"))
			return
		}
	}

	type result struct {
		TrackID        string    `json:"trackId"`
		Title          string    `json:"title"`
		Version        string    `json:"version"`
		Outcome        string    `json:"outcome"`
		Reason         string    `json:"reason"`
		Added          []bulkRef `json:"added"`
		Removed        []bulkRef `json:"removed"`
		PrimaryChanged []string  `json:"primaryChanged"`
	}
	results, counts := []result{}, map[string]int{}
	err = h.inBulk(ctx, in.bulkRequest, func(tx pgx.Tx, now *trackMetadata) error {
		res := result{TrackID: now.TrackID, Title: now.Title, Version: now.Version, Outcome: "unchanged",
			Added: []bulkRef{}, Removed: []bulkRef{}, PrimaryChanged: []string{}}
		defer func() { results, counts[res.Outcome] = append(results, res), counts[res.Outcome]+1 }()
		unknown := now.LanguageUnknown
		var cats []*category
		for _, l := range lists {
			if l.name == "languages" {
				if !l.replace && now.LanguageUnknown {
					res.Outcome, res.Reason = "skipped", "language_unknown"
					return nil
				}
				unknown = false
			}
			c := bulkCategories[l.name]
			c.have = currentList(now, l.name)
			c.want = l.apply(c.have)
			cats = append(cats, &c)
		}
		for _, c := range cats {
			for id := range c.want {
				if _, had := c.have[id]; !had {
					res.Added = append(res.Added, bulkRef{c.name, id})
				}
			}
			for id := range c.have {
				if _, stays := c.want[id]; !stays {
					res.Removed = append(res.Removed, bulkRef{c.name, id})
				}
			}
			if mainOf(c.have) != mainOf(c.want) {
				res.PrimaryChanged = append(res.PrimaryChanged, c.name)
			}
		}
		for _, list := range [][]bulkRef{res.Added, res.Removed} {
			slices.SortFunc(list, func(a, b bulkRef) int { return compare(a.Category+" "+a.ID, b.Category+" "+b.ID) })
		}
		changed, err := h.applyMetadata(ctx, tx, r, now, cats, unknown, true)
		if err != nil || !changed {
			return err
		}
		res.Outcome = "changed"
		if !in.DryRun {
			res.Version, err = versionOf(ctx, tx, now.TrackID)
		}
		return err
	})
	if err != nil {
		replyBulk(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"results": results, "changed": counts["changed"], "unchanged": counts["unchanged"], "skipped": counts["skipped"]})
}

func compare(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// pending lists a song's values still waiting to be checked in one category; rejected ones are not among them.
func pending(now *trackMetadata, name string) []bulkRef {
	var out []bulkRef
	add := func(id string, p provenance) {
		if p.Verification == "pending" {
			out = append(out, bulkRef{name, id})
		}
	}
	switch name {
	case "languages":
		for _, l := range now.Languages {
			add(l.ID, l.provenance)
		}
		if now.LanguageUnknownReview != nil {
			add("", *now.LanguageUnknownReview)
		}
	case "genres":
		for _, g := range now.Genres {
			add(g.ID, g.provenance)
		}
	case "cultures", "regions":
		list := now.Cultures
		if name == "regions" {
			list = now.Regions
		}
		for _, l := range list {
			add(l.ID, l.provenance)
		}
	default:
		for _, l := range now.Attributes[name] {
			add(l.ID, l.provenance)
		}
	}
	return out
}

// POST /v1/admin/metadata/bulk-verify {trackIds, categories, dryRun, versions?}
func (h *Handler) bulkVerify(w http.ResponseWriter, r *http.Request) {
	var in struct {
		bulkRequest
		Categories []string `json:"categories"`
	}
	if err := decodeBody(w, r, &in, false); err != nil {
		reply(w, err)
		return
	}
	seen := map[string]bool{}
	for _, c := range in.Categories {
		if _, ok := reviewTables[c]; !ok || seen[c] {
			reply(w, invalid("unknown or repeated category "+strconv.Quote(c)))
			return
		}
		seen[c] = true
	}
	if len(in.Categories) == 0 {
		reply(w, invalid("categories must name at least one list"))
		return
	}
	type result struct {
		TrackID  string    `json:"trackId"`
		Title    string    `json:"title"`
		Version  string    `json:"version"`
		Outcome  string    `json:"outcome"`
		Verified []bulkRef `json:"verified"`
	}
	results, changed := []result{}, 0
	ctx, actor := r.Context(), actorID(r)
	err := h.inBulk(ctx, in.bulkRequest, func(tx pgx.Tx, now *trackMetadata) error {
		res := result{TrackID: now.TrackID, Title: now.Title, Version: now.Version, Outcome: "unchanged", Verified: []bulkRef{}}
		defer func() { results = append(results, res) }()
		for _, c := range in.Categories {
			res.Verified = append(res.Verified, pending(now, c)...)
		}
		if len(res.Verified) == 0 {
			return nil
		}
		if _, err := tx.Exec(ctx, `INSERT INTO track_metadata (track_id) VALUES ($1) ON CONFLICT (track_id) DO NOTHING`, now.TrackID); err != nil {
			return err
		}
		for _, it := range res.Verified {
			category := it.Category
			if category == "languages" && it.ID == "" {
				category = "languageUnknown"
			}
			if err := setVerification(ctx, tx, now.TrackID, reviewItem{category, it.ID}, "verified", actor); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE track_metadata SET review_note = '', version = version + 1, updated_at = now(), updated_by = $2
			WHERE track_id = $1`, now.TrackID, actor); err != nil {
			return err
		}
		if err := h.log(ctx, tx, r, audit.Entry{Action: "track.metadata_verify", TargetType: "track", TargetID: now.TrackID, TargetLabel: now.Title,
			Details: map[string]any{"items": res.Verified, "bulk": true}}); err != nil {
			return err
		}
		res.Outcome, changed = "changed", changed+1
		var err error
		if !in.DryRun {
			res.Version, err = versionOf(ctx, tx, now.TrackID)
		}
		return err
	})
	if err != nil {
		replyBulk(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"results": results, "changed": changed, "unchanged": len(results) - changed, "skipped": 0})
}
