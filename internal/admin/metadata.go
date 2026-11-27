package admin

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/blacmusic/backend/internal/audit"
	"github.com/blacmusic/backend/internal/httpx"
	"github.com/blacmusic/backend/internal/release"
)

// Catalogue metadata: the team describes songs. Nothing here is guessed or worked out.
//
// Administrators only for now: these routes sit behind requireAdmin like the rest of this package. When catalogue
// managers and reviewers get roles, this group is where their check goes.
func (h *Handler) metadataRoutes(r chi.Router) {
	r.Get("/taxonomy", h.taxonomy)
	r.Post("/taxonomy/{kind}", h.createTerm)
	r.Patch("/taxonomy/{kind}/{id}", h.updateTerm)
	r.Get("/releases/{id}/track-metadata", h.releaseMetadata)
	r.Get("/tracks/{id}/metadata", h.getTrackMetadata)
	r.Put("/tracks/{id}/metadata", h.putTrackMetadata)
	r.Post("/tracks/{id}/metadata/submit", h.submitMetadata)
	r.Post("/tracks/{id}/metadata/review", h.reviewMetadata)
	r.Get("/metadata/tracks", h.metadataQueue)
	r.Post("/metadata/bulk", h.bulkMetadata)
	r.Post("/metadata/bulk-verify", h.bulkVerify)
}

// ---------------------------------------------------------------- the terms

type term struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Active bool   `json:"active"`
	Usage  *int   `json:"usage,omitempty"` // songs that have it, when asked for
}

var attributeKinds = []string{"mood", "energy", "danceability", "instrumentation"}

// GET /v1/admin/taxonomy: every term, the switched-off ones too.
func (h *Handler) taxonomy(w http.ResponseWriter, r *http.Request) {
	type group struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	}
	type language struct {
		term
		GroupID *string `json:"groupId"`
	}
	type genreTerm struct {
		term
		ParentID *string `json:"parentId"`
	}
	out := struct {
		LanguageGroups []group           `json:"languageGroups"`
		Languages      []language        `json:"languages"`
		Cultures       []term            `json:"cultures"`
		Regions        []term            `json:"regions"`
		Genres         []genreTerm       `json:"genres"`
		Attributes     map[string][]term `json:"attributes"`
		RequiredFields []string          `json:"requiredFields"`
	}{[]group{}, []language{}, []term{}, []term{}, []genreTerm{}, map[string][]term{}, []string{}}
	if err := h.pool.QueryRow(r.Context(), `SELECT required_fields FROM metadata_settings`).Scan(&out.RequiredFields); err != nil {
		reply(w, err)
		return
	}
	for _, k := range attributeKinds {
		out.Attributes[k] = []term{}
	}
	// one query: list, id, label, active, the group or parent
	rows, err := h.pool.Query(r.Context(), `
		SELECT list, id, label, active, up FROM (
			SELECT 'group' AS list, id, label, true AS active, NULL::text AS up, position FROM tax_language_groups
			UNION ALL SELECT 'language', id, label, active, group_id, position FROM tax_languages
			UNION ALL SELECT 'culture', id, label, active, NULL, position FROM tax_cultures
			UNION ALL SELECT 'region', id, label, active, NULL, position FROM tax_regions
			UNION ALL SELECT 'genre', id, label, active, parent_id, position FROM tax_genres
			UNION ALL SELECT kind, id, label, active, NULL, position FROM tax_attributes
		) x ORDER BY list, position, label, id`)
	if err != nil {
		reply(w, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var list string
		var t term
		var up *string
		if err := rows.Scan(&list, &t.ID, &t.Label, &t.Active, &up); err != nil {
			reply(w, err)
			return
		}
		switch list {
		case "group":
			out.LanguageGroups = append(out.LanguageGroups, group{t.ID, t.Label})
		case "language":
			out.Languages = append(out.Languages, language{t, up})
		case "culture":
			out.Cultures = append(out.Cultures, t)
		case "region":
			out.Regions = append(out.Regions, t)
		case "genre":
			out.Genres = append(out.Genres, genreTerm{t, up})
		default:
			out.Attributes[list] = append(out.Attributes[list], t)
		}
	}
	if err := rows.Err(); err != nil {
		reply(w, err)
		return
	}
	if r.URL.Query().Get("usage") == "true" {
		rows.Close()
		used, err := h.termUsage(r.Context())
		if err != nil {
			reply(w, err)
			return
		}
		count := func(list string, t *term) {
			n := used[list+"/"+t.ID]
			t.Usage = &n
		}
		for i := range out.Languages {
			count("language", &out.Languages[i].term)
		}
		for i := range out.Cultures {
			count("culture", &out.Cultures[i])
		}
		for i := range out.Regions {
			count("region", &out.Regions[i])
		}
		for i := range out.Genres {
			count("genre", &out.Genres[i].term)
		}
	}
	httpx.JSON(w, http.StatusOK, out)
}

// termUsage counts the songs linked to each term, however the link has been reviewed.
func (h *Handler) termUsage(ctx context.Context) (map[string]int, error) {
	rows, err := h.pool.Query(ctx, `
		SELECT 'language/' || language_id, count(*) FROM track_languages GROUP BY 1
		UNION ALL SELECT 'culture/' || culture_id, count(*) FROM track_cultures GROUP BY 1
		UNION ALL SELECT 'region/' || region_id, count(*) FROM track_regions GROUP BY 1
		UNION ALL SELECT 'genre/' || genre_id, count(*) FROM track_genres GROUP BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var key string
		var n int
		if err := rows.Scan(&key, &n); err != nil {
			return nil, err
		}
		out[key] = n
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- a song's metadata

type person struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

// provenance says where one piece of metadata came from and whether it has been checked.
type provenance struct {
	Source       string     `json:"source"`
	Verification string     `json:"verification"`
	Confidence   *float64   `json:"confidence"`
	VerifiedBy   *person    `json:"verifiedBy"`
	VerifiedAt   *time.Time `json:"verifiedAt"`
}

type metaLink struct {
	ID string `json:"id"`
	provenance
}

type languageLink struct {
	ID   string `json:"id"`
	Role string `json:"role"`
	provenance
}

type genreLink struct {
	ID      string `json:"id"`
	Primary bool   `json:"primary"`
	provenance
}

type trackMetadata struct {
	TrackID         string   `json:"trackId"`
	Title           string   `json:"title"`
	TrackNumber     *int     `json:"trackNumber"`
	DurationMs      int      `json:"durationMs"`
	Artist          idName   `json:"artist"`
	FeaturedArtists []string `json:"featuredArtists"`
	Release         struct {
		ID          string  `json:"id"`
		Title       string  `json:"title"`
		Type        string  `json:"type"`
		Kind        string  `json:"kind"`
		ReleaseDate *string `json:"releaseDate"`
	} `json:"release"`
	ISRC            *string               `json:"isrc"` // not collected yet
	Status          string                `json:"status"`
	LanguageUnknown bool                  `json:"languageUnknown"`
	Languages       []languageLink        `json:"languages"`
	Cultures        []metaLink            `json:"cultures"`
	Regions         []metaLink            `json:"regions"`
	Genres          []genreLink           `json:"genres"`
	Attributes      map[string][]metaLink `json:"attributes"`
	Version         string                `json:"version"`
	UpdatedAt       *time.Time            `json:"updatedAt"`
	UpdatedBy       *person               `json:"updatedBy"`
	SubmittedAt     *time.Time            `json:"submittedAt"`
	SubmittedBy     *person               `json:"submittedBy"`
	// LanguageUnknownReview: "unknown" is checked like any value; nil unless the language is unknown.
	LanguageUnknownReview *provenance `json:"languageUnknownReview"`
	MissingRequired       []string    `json:"missingRequired"`
	ReviewNote            string      `json:"reviewNote"`
}

type idName struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// loadMetadata reads the metadata of the songs that where selects (over tracks t), in release order.
func loadMetadata(ctx context.Context, db querier, where string, arg string) ([]*trackMetadata, error) {
	rows, err := db.Query(ctx, `
		SELECT t.id, t.title, t.track_number, t.duration_ms, COALESCE(a.id, ''), COALESCE(a.name, ''),
		       ARRAY(SELECT c.name FROM release_credits c WHERE c.role = 'featured_artist'
		             AND (c.track_id = t.id OR (c.track_id IS NULL AND c.release_id = t.album_id))
		             GROUP BY c.name ORDER BY min(c.position), c.name),
		       COALESCE(al.id, ''), COALESCE(al.title, ''), COALESCE(al.release_type, ''), to_char(al.release_date, 'YYYY-MM-DD'),
		       (SELECT count(*) FROM tracks o WHERE o.album_id = t.album_id),
		       s.status, s.language_unknown, COALESCE(m.version, 0), m.updated_at, u.id::text, u.email,
		       m.submitted_at, su.id::text, su.email, s.missing, COALESCE(m.review_note, ''), s.unknown_verification,
		       vu.id::text, vu.email, m.unknown_verified_at
		FROM tracks t
		JOIN track_metadata_state s ON s.track_id = t.id
		LEFT JOIN artists a ON a.id = t.artist_id
		LEFT JOIN albums al ON al.id = t.album_id
		LEFT JOIN track_metadata m ON m.track_id = t.id
		LEFT JOIN users u ON u.id = m.updated_by
		LEFT JOIN users su ON su.id = m.submitted_by
		LEFT JOIN users vu ON vu.id = m.unknown_verified_by
		WHERE `+where+` ORDER BY t.track_number NULLS LAST, t.created_at, t.id`, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*trackMetadata{}
	byID := map[string]*trackMetadata{}
	ids := []string{}
	for rows.Next() {
		m := &trackMetadata{Languages: []languageLink{}, Cultures: []metaLink{}, Regions: []metaLink{}, Genres: []genreLink{},
			Attributes: map[string][]metaLink{}}
		for _, k := range attributeKinds {
			m.Attributes[k] = []metaLink{}
		}
		var tracks int
		var version int64
		var byUser, email, subUser, subEmail, checkUser, checkEmail *string
		var unknownState string
		var unknownAt *time.Time
		if err := rows.Scan(&m.TrackID, &m.Title, &m.TrackNumber, &m.DurationMs, &m.Artist.ID, &m.Artist.Name, &m.FeaturedArtists,
			&m.Release.ID, &m.Release.Title, &m.Release.Type, &m.Release.ReleaseDate, &tracks,
			&m.Status, &m.LanguageUnknown, &version, &m.UpdatedAt, &byUser, &email,
			&m.SubmittedAt, &subUser, &subEmail, &m.MissingRequired, &m.ReviewNote, &unknownState, &checkUser, &checkEmail, &unknownAt); err != nil {
			return nil, err
		}
		if subUser != nil {
			m.SubmittedBy = &person{*subUser, deref(subEmail)}
		}
		if m.LanguageUnknown {
			m.LanguageUnknownReview = &provenance{Source: "admin", Verification: unknownState, VerifiedAt: unknownAt}
			if checkUser != nil {
				m.LanguageUnknownReview.VerifiedBy = &person{*checkUser, deref(checkEmail)}
			}
		}
		m.Release.Kind = release.KindOf(m.Release.Type, tracks)
		m.Version = strconv.FormatInt(version, 10)
		if byUser != nil {
			m.UpdatedBy = &person{*byUser, deref(email)}
		}
		out, ids, byID[m.TrackID] = append(out, m), append(ids, m.TrackID), m
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	const cols = `source, verification, confidence, verified_by, verified_at`
	links, err := db.Query(ctx, `
		SELECT l.track_id, l.kind, l.term, l.main, l.source, l.verification, l.confidence::float8, u.id::text, u.email, l.verified_at FROM (
			SELECT track_id, 'language' AS kind, language_id AS term, role = 'primary' AS main, `+cols+`,
			       (SELECT position FROM tax_languages x WHERE x.id = language_id) AS position FROM track_languages WHERE track_id = ANY($1)
			UNION ALL SELECT track_id, 'culture', culture_id, false, `+cols+`,
			       (SELECT position FROM tax_cultures x WHERE x.id = culture_id) FROM track_cultures WHERE track_id = ANY($1)
			UNION ALL SELECT track_id, 'region', region_id, false, `+cols+`,
			       (SELECT position FROM tax_regions x WHERE x.id = region_id) FROM track_regions WHERE track_id = ANY($1)
			UNION ALL SELECT track_id, 'genre', genre_id, is_primary, `+cols+`,
			       (SELECT position FROM tax_genres x WHERE x.id = genre_id) FROM track_genres WHERE track_id = ANY($1)
			UNION ALL SELECT track_id, kind, attribute_id, false, `+cols+`,
			       (SELECT position FROM tax_attributes x WHERE x.kind = y.kind AND x.id = y.attribute_id) FROM track_attributes y WHERE track_id = ANY($1)
		) l LEFT JOIN users u ON u.id = l.verified_by
		ORDER BY l.track_id, l.kind, l.main DESC, l.position, l.term`, ids)
	if err != nil {
		return nil, err
	}
	defer links.Close()
	for links.Next() {
		var trackID, kind, id string
		var main bool
		var p provenance
		var byUser, email *string
		if err := links.Scan(&trackID, &kind, &id, &main, &p.Source, &p.Verification, &p.Confidence, &byUser, &email, &p.VerifiedAt); err != nil {
			return nil, err
		}
		if byUser != nil {
			p.VerifiedBy = &person{*byUser, deref(email)}
		}
		m := byID[trackID]
		switch kind {
		case "language":
			role := "secondary"
			if main {
				role = "primary"
			}
			m.Languages = append(m.Languages, languageLink{id, role, p})
		case "culture":
			m.Cultures = append(m.Cultures, metaLink{id, p})
		case "region":
			m.Regions = append(m.Regions, metaLink{id, p})
		case "genre":
			m.Genres = append(m.Genres, genreLink{id, main, p})
		default:
			m.Attributes[kind] = append(m.Attributes[kind], metaLink{id, p})
		}
	}
	return out, links.Err()
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// GET /v1/admin/releases/{id}/track-metadata
func (h *Handler) releaseMetadata(w http.ResponseWriter, r *http.Request) {
	id := param(r, "id")
	var one int
	if err := h.pool.QueryRow(r.Context(), `SELECT 1 FROM albums WHERE id = $1`, id).Scan(&one); err != nil {
		reply(w, err)
		return
	}
	list, err := loadMetadata(r.Context(), h.pool, "t.album_id = $1", id)
	if err != nil {
		reply(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"releaseId": id, "tracks": list})
}

// GET /v1/admin/tracks/{id}/metadata
func (h *Handler) getTrackMetadata(w http.ResponseWriter, r *http.Request) {
	list, err := loadMetadata(r.Context(), h.pool, "t.id = $1", param(r, "id"))
	if err == nil && len(list) == 0 {
		err = errNotFound
	}
	if err != nil {
		reply(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, list[0])
}

// ---------------------------------------------------------------- saving

type metadataInput struct {
	Version         string `json:"version"`
	LanguageUnknown bool   `json:"languageUnknown"`
	Languages       []struct {
		ID   string `json:"id"`
		Role string `json:"role"`
	} `json:"languages"`
	Cultures []string `json:"cultures"`
	Regions  []string `json:"regions"`
	Genres   []struct {
		ID      string `json:"id"`
		Primary bool   `json:"primary"`
	} `json:"genres"`
	Attributes struct {
		Mood            []string `json:"mood"`
		Energy          []string `json:"energy"`
		Danceability    []string `json:"danceability"`
		Instrumentation []string `json:"instrumentation"`
	} `json:"attributes"`
}

// category is one list of a song's terms: its table and, for each chosen term, whether it is the main one.
type category struct {
	name   string // as the API calls it
	table  string
	column string
	kind   string // the attribute kind; "" for the other lists
	terms  string // the table of allowed terms
	single bool   // at most one term
	want   map[string]bool
	have   map[string]bool
}

// mainSet is the SQL that marks a term as the main one or not, for the lists that have such a mark.
func (c *category) mainSet() (column string, value func(bool) any) {
	switch c.table {
	case "track_languages":
		return "role", func(main bool) any {
			if main {
				return "primary"
			}
			return "secondary"
		}
	case "track_genres":
		return "is_primary", func(main bool) any { return main }
	}
	return "", nil
}

// describe lists the terms for the audit log; the main one is marked with a star.
func describe(m map[string]bool) []string {
	out := []string{}
	for id, main := range m {
		if main {
			id += "*"
		}
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// categories turns a request and the song's current metadata into the lists to compare, checking the request.
func categories(in metadataInput, now *trackMetadata) ([]*category, error) {
	cats := []*category{
		{name: "languages", table: "track_languages", column: "language_id", terms: "tax_languages"},
		{name: "cultures", table: "track_cultures", column: "culture_id", terms: "tax_cultures"},
		{name: "regions", table: "track_regions", column: "region_id", terms: "tax_regions"},
		{name: "genres", table: "track_genres", column: "genre_id", terms: "tax_genres"},
	}
	for _, k := range attributeKinds {
		cats = append(cats, &category{name: k, table: "track_attributes", column: "attribute_id", kind: k, terms: "tax_attributes",
			single: k == "energy" || k == "danceability"})
	}
	for _, c := range cats {
		c.want, c.have = map[string]bool{}, map[string]bool{}
	}
	add := func(c *category, id string, main bool) error {
		if _, twice := c.want[id]; twice {
			return invalid(c.name + " lists " + strconv.Quote(id) + " twice")
		}
		c.want[id] = main
		return nil
	}
	var errs []error
	languageMains, genreMains := 0, 0
	for _, l := range in.Languages {
		if l.Role != "primary" && l.Role != "secondary" {
			return nil, invalid("a language's role is primary or secondary")
		}
		if l.Role == "primary" {
			languageMains++
		}
		errs = append(errs, add(cats[0], l.ID, l.Role == "primary"))
	}
	for _, id := range in.Cultures {
		errs = append(errs, add(cats[1], id, false))
	}
	for _, id := range in.Regions {
		errs = append(errs, add(cats[2], id, false))
	}
	for _, g := range in.Genres {
		if g.Primary {
			genreMains++
		}
		errs = append(errs, add(cats[3], g.ID, g.Primary))
	}
	for i, list := range [][]string{in.Attributes.Mood, in.Attributes.Energy, in.Attributes.Danceability, in.Attributes.Instrumentation} {
		for _, id := range list {
			errs = append(errs, add(cats[4+i], id, false))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, invalid(strings.ReplaceAll(err.Error(), "\n", "; "))
	}
	switch {
	case languageMains > 1:
		return nil, invalid("a song has at most one primary language")
	case genreMains > 1:
		return nil, invalid("a song has at most one primary genre")
	case in.LanguageUnknown && len(in.Languages) > 0:
		return nil, invalid("an unknown language cannot come with languages")
	}
	for _, c := range cats {
		if c.single && len(c.want) > 1 {
			return nil, invalid(c.name + " takes one value")
		}
	}
	for _, l := range now.Languages {
		cats[0].have[l.ID] = l.Role == "primary"
	}
	for _, l := range now.Cultures {
		cats[1].have[l.ID] = false
	}
	for _, l := range now.Regions {
		cats[2].have[l.ID] = false
	}
	for _, g := range now.Genres {
		cats[3].have[g.ID] = g.Primary
	}
	for i, k := range attributeKinds {
		for _, l := range now.Attributes[k] {
			cats[4+i].have[l.ID] = false
		}
	}
	return cats, nil
}

// save makes the stored list match the wanted one. Terms that stay keep where they came from; changing whether one
// is the main one sends it back to be verified.
func (c *category) save(ctx context.Context, tx pgx.Tx, trackID string) error {
	where, args := "track_id = $1 AND "+c.column+" = $2", func(id string) []any { return []any{trackID, id} }
	if c.kind != "" {
		where, args = where+" AND kind = $3", func(id string) []any { return []any{trackID, id, c.kind} }
	}
	for id := range c.have {
		if _, stays := c.want[id]; !stays {
			if _, err := tx.Exec(ctx, "DELETE FROM "+c.table+" WHERE "+where, args(id)...); err != nil {
				return err
			}
		}
	}
	mark, value := c.mainSet()
	// the old main one steps down before another takes its place
	for _, main := range []bool{false, true} {
		for id, want := range c.want {
			if had, kept := c.have[id]; kept && want == main && had != want {
				if _, err := tx.Exec(ctx, "UPDATE "+c.table+" SET "+mark+" = $3, verification = 'pending', verified_by = NULL, verified_at = NULL WHERE "+where,
					trackID, id, value(want)); err != nil {
					return err
				}
			}
		}
	}
	for id, main := range c.want {
		if _, kept := c.have[id]; kept {
			continue
		}
		check, checkArgs := "SELECT active FROM "+c.terms+" WHERE id = $1", []any{id}
		if c.kind != "" {
			check, checkArgs = check+" AND kind = $2", []any{id, c.kind}
		}
		var active bool
		err := tx.QueryRow(ctx, check, checkArgs...).Scan(&active)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !active) {
			return fail4(http.StatusUnprocessableEntity, "invalid_taxonomy", c.name+": "+strconv.Quote(id)+" is not a value that can be chosen")
		}
		if err != nil {
			return err
		}
		columns, values, insert := "track_id, "+c.column, "$1, $2", []any{trackID, id}
		switch {
		case c.kind != "":
			columns, values, insert = columns+", kind", values+", $3", append(insert, c.kind)
		case mark != "":
			columns, values, insert = columns+", "+mark, values+", $3", append(insert, value(main))
		}
		if _, err := tx.Exec(ctx, "INSERT INTO "+c.table+" ("+columns+", source) VALUES ("+values+", 'admin')", insert...); err != nil {
			return err
		}
	}
	return nil
}

// PUT /v1/admin/tracks/{id}/metadata: replaces the song's metadata with what is sent.
func (h *Handler) putTrackMetadata(w http.ResponseWriter, r *http.Request) {
	var in metadataInput
	if err := decodeBody(w, r, &in, false); err != nil {
		reply(w, err)
		return
	}
	if in.Version == "" {
		reply(w, invalid("version is required: send the one you read"))
		return
	}
	ctx, id := r.Context(), param(r, "id")
	saved, err := h.saveMetadata(ctx, r, id, in)
	if err != nil {
		reply(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, saved)
}

func (h *Handler) saveMetadata(ctx context.Context, r *http.Request, id string, in metadataInput) (*trackMetadata, error) {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	now, err := lockMetadata(ctx, tx, id, in.Version)
	if err != nil {
		return nil, err
	}
	cats, err := categories(in, now)
	if err != nil {
		return nil, err
	}
	changed, err := h.applyMetadata(ctx, tx, r, now, cats, in.LanguageUnknown, false)
	if err != nil {
		return nil, err
	}
	if !changed {
		return now, nil
	}
	list, err := loadMetadata(ctx, tx, "t.id = $1", id)
	if err != nil {
		return nil, err
	}
	return list[0], tx.Commit(ctx)
}

// applyMetadata brings the song's lists in cats (only those are touched) and "language unknown" to what is wanted,
// with everything that goes with a change. It says whether anything changed.
func (h *Handler) applyMetadata(ctx context.Context, tx pgx.Tx, r *http.Request, now *trackMetadata, cats []*category,
	languageUnknown, bulk bool) (bool, error) {
	id := now.TrackID
	changed, before, after := []string{}, map[string]any{}, map[string]any{}
	note := func(name string, was, is any) {
		changed, before[name], after[name] = append(changed, name), was, is
	}
	if languageUnknown != now.LanguageUnknown {
		note("languageUnknown", now.LanguageUnknown, languageUnknown)
	}
	var genres *category
	for _, c := range cats {
		if c.name == "genres" {
			genres = c
		}
		was, is := describe(c.have), describe(c.want)
		if slices.Equal(was, is) {
			continue
		}
		note(c.name, was, is)
		if err := c.save(ctx, tx, id); err != nil {
			return false, err
		}
	}
	if len(changed) == 0 {
		return false, nil
	}
	// tracks.genre holds the main genre's name; it is touched only when the main genre changed
	if genres != nil && mainOf(genres.have) != mainOf(genres.want) {
		if _, err := tx.Exec(ctx, `UPDATE tracks SET genre = COALESCE((SELECT label FROM tax_genres WHERE id = $2), '') WHERE id = $1`,
			id, mainOf(genres.want)); err != nil {
			return false, err
		}
	}
	// data that was only imported does not count as started
	if _, err := tx.Exec(ctx, `
		INSERT INTO track_metadata (track_id, language_unknown, version, updated_at, updated_by) VALUES ($1, $2, 1, now(), $3)
		ON CONFLICT (track_id) DO UPDATE SET language_unknown = $2, version = track_metadata.version + 1, updated_at = now(), updated_by = $3,
		  -- "unknown" said anew has to be checked anew
		  unknown_verification = CASE WHEN track_metadata.language_unknown = $2 THEN track_metadata.unknown_verification ELSE 'pending' END,
		  unknown_verified_by = CASE WHEN track_metadata.language_unknown = $2 THEN track_metadata.unknown_verified_by END,
		  unknown_verified_at = CASE WHEN track_metadata.language_unknown = $2 THEN track_metadata.unknown_verified_at END`,
		id, languageUnknown, actorID(r)); err != nil {
		return false, err
	}
	// a song with something required missing is no longer up for review
	if _, err := tx.Exec(ctx, `UPDATE track_metadata SET submitted_at = NULL, submitted_by = NULL
		WHERE track_id = $1 AND cardinality((SELECT missing FROM track_metadata_state WHERE track_id = $1)) > 0`, id); err != nil {
		return false, err
	}
	details := map[string]any{"changed": changed, "before": before, "after": after}
	if bulk {
		details["bulk"] = true
	}
	if err := h.log(ctx, tx, r, audit.Entry{Action: "track.metadata_update", TargetType: "track", TargetID: id, TargetLabel: now.Title,
		Details: details}); err != nil {
		return false, err
	}
	return true, nil
}

func mainOf(m map[string]bool) string {
	for id, main := range m {
		if main {
			return id
		}
	}
	return ""
}
