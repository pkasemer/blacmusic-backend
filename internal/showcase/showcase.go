// Package showcase is the short, hand-curated list of artists and albums shown
// to signed-out visitors. It is curated by the Blac team, only from catalogue
// items whose picture is already on Blac, and each entry records how its owner
// agreed to appear.
package showcase

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/httpx"
	"github.com/blacmusic/backend/internal/image"
)

// MaxItems is the longest the list can be.
const MaxItems = 12

// ValidationError is a bad curation request; Msg names the item at fault.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// Item is what visitors see.
type Item struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"` // artist | album
	Name     string `json:"name"`
	Subtitle string `json:"subtitle,omitempty"`
	ImageURL string `json:"imageUrl"`
	Position int    `json:"position"`
}

// AdminItem is an entry as the team manages it.
type AdminItem struct {
	Item
	ConsentNote string    `json:"consentNote"`
	AddedBy     string    `json:"addedBy,omitempty"`
	AddedAt     time.Time `json:"addedAt"`
	// Listed says whether visitors see it now; Problem says why not.
	Listed  bool   `json:"listed"`
	Problem string `json:"problem,omitempty"`
}

// Input is one entry of a curation request.
type Input struct {
	Kind        string `json:"kind"`
	ID          string `json:"id"`
	ConsentNote string `json:"consentNote"`
}

// Store reads and curates the list.
type Store struct {
	pool      *pgxpool.Pool
	publicURL string
}

// NewStore creates the store. publicURL is the API's public base URL (picture links).
func NewStore(pool *pgxpool.Pool, publicURL string) *Store {
	return &Store{pool: pool, publicURL: publicURL}
}

type row struct {
	position    int
	kind, id    string
	consent     string
	addedBy     *string
	addedAt     time.Time
	name, sub   string
	imageURL    string
	exists      bool
	hasPlayable bool
}

// resolve joins the stored entries with the catalogue.
func (s *Store) resolve(ctx context.Context) ([]row, error) {
	rows, err := s.pool.Query(ctx, `SELECT position, kind, ref_id, consent_note, added_by::text, added_at FROM showcase_items ORDER BY position`)
	if err != nil {
		return nil, err
	}
	var out []row
	var artists, albums []string
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.position, &r.kind, &r.id, &r.consent, &r.addedBy, &r.addedAt); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
		if r.kind == "artist" {
			artists = append(artists, r.id)
		} else {
			albums = append(albums, r.id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	byKey := map[string]*row{}
	for i := range out {
		byKey[out[i].kind+"/"+out[i].id] = &out[i]
	}

	ar, err := s.pool.Query(ctx, `
		SELECT a.id, a.name, COALESCE(u.avatar_image_id, a.avatar_image_id), COALESCE(a.image_url, ''),
		       (SELECT al.title FROM albums al WHERE al.artist_id = a.id AND al.visibility = 'public'
		          AND EXISTS (SELECT 1 FROM tracks t WHERE t.album_id = al.id AND t.visibility = 'public')
		        ORDER BY al.published_at DESC NULLS LAST, al.created_at DESC LIMIT 1)
		FROM artists a LEFT JOIN users u ON u.id = a.owner_user_id WHERE a.id = ANY($1)`, artists)
	if err != nil {
		return nil, err
	}
	for ar.Next() {
		var id, name, legacy string
		var avatar *string
		var latest *string
		if err := ar.Scan(&id, &name, &avatar, &legacy, &latest); err != nil {
			ar.Close()
			return nil, err
		}
		r := byKey["artist/"+id]
		r.exists, r.name, r.imageURL = true, name, image.PersonURL(s.publicURL, avatar, legacy)
		if latest != nil {
			r.hasPlayable = true
			r.sub = "Latest: " + *latest
		}
	}
	ar.Close()
	if err := ar.Err(); err != nil {
		return nil, err
	}

	lr, err := s.pool.Query(ctx, `
		SELECT al.id, al.title, COALESCE(al.artwork_url, ''), COALESCE(a.name, ''),
		       al.visibility = 'public' AND EXISTS (SELECT 1 FROM tracks t WHERE t.album_id = al.id AND t.visibility = 'public')
		FROM albums al LEFT JOIN artists a ON a.id = al.artist_id WHERE al.id = ANY($1)`, albums)
	if err != nil {
		return nil, err
	}
	for lr.Next() {
		var id, title, art, artist string
		var playable bool
		if err := lr.Scan(&id, &title, &art, &artist, &playable); err != nil {
			lr.Close()
			return nil, err
		}
		r := byKey["album/"+id]
		r.exists, r.name, r.sub, r.imageURL, r.hasPlayable = true, title, artist, art, playable
	}
	lr.Close()
	return out, lr.Err()
}

func problem(r row) string {
	switch {
	case !r.exists:
		return "it no longer exists"
	case !r.hasPlayable && r.kind == "artist":
		return "the artist has no public release with a public track"
	case !r.hasPlayable:
		return "the album is not public or has no public track"
	case r.imageURL == "":
		return "it has no picture on Blac"
	}
	return ""
}

// Public returns what visitors see: entries that still qualify, in order.
func (s *Store) Public(ctx context.Context) ([]Item, error) {
	rs, err := s.resolve(ctx)
	if err != nil {
		return nil, err
	}
	out := []Item{}
	for _, r := range rs {
		if problem(r) != "" || len(out) >= MaxItems {
			continue
		}
		out = append(out, Item{ID: r.id, Kind: r.kind, Name: r.name, Subtitle: r.sub, ImageURL: r.imageURL, Position: len(out) + 1})
	}
	return out, nil
}

// Admin returns every stored entry with its consent record and whether it is shown.
func (s *Store) Admin(ctx context.Context) ([]AdminItem, error) {
	rs, err := s.resolve(ctx)
	if err != nil {
		return nil, err
	}
	out := []AdminItem{}
	for _, r := range rs {
		p := problem(r)
		it := AdminItem{
			Item:        Item{ID: r.id, Kind: r.kind, Name: r.name, Subtitle: r.sub, ImageURL: r.imageURL, Position: r.position},
			ConsentNote: r.consent, AddedAt: r.addedAt, Listed: p == "", Problem: p,
		}
		if r.addedBy != nil {
			it.AddedBy = *r.addedBy
		}
		out = append(out, it)
	}
	return out, nil
}

// Replace sets the whole list and its order. Every entry must be a catalogue
// item that qualifies now (a public release with a public track; a picture on
// Blac) and needs a consent note: who agreed, and how. An entry already in the
// list keeps its note when none is sent.
func (s *Store) Replace(ctx context.Context, adminID string, items []Input) error {
	if len(items) > MaxItems {
		return &ValidationError{"the showcase holds at most 12 items"}
	}
	existing := map[string]string{}
	cur, err := s.resolve(ctx)
	if err != nil {
		return err
	}
	for _, r := range cur {
		existing[r.kind+"/"+r.id] = r.consent
	}

	// check each entry against the catalogue as if it were stored
	seen := map[string]bool{}
	var artists, albums []string
	for i, it := range items {
		key := it.Kind + "/" + it.ID
		switch {
		case it.Kind != "artist" && it.Kind != "album":
			return &ValidationError{"item " + strconv.Itoa(i+1) + ": kind must be artist or album"}
		case strings.TrimSpace(it.ID) == "":
			return &ValidationError{"item " + strconv.Itoa(i+1) + ": id is required"}
		case seen[key]:
			return &ValidationError{"item " + strconv.Itoa(i+1) + ": " + key + " is listed twice"}
		}
		seen[key] = true
		if it.Kind == "artist" {
			artists = append(artists, it.ID)
		} else {
			albums = append(albums, it.ID)
		}
	}
	probe, err := s.probe(ctx, artists, albums)
	if err != nil {
		return err
	}
	stored := make([]Input, len(items))
	for i, it := range items {
		key := it.Kind + "/" + it.ID
		r, ok := probe[key]
		if !ok {
			r = row{kind: it.Kind, id: it.ID}
		}
		if p := problem(r); p != "" {
			return &ValidationError{"item " + strconv.Itoa(i+1) + " (" + key + "): " + p}
		}
		note := strings.TrimSpace(it.ConsentNote)
		if note == "" {
			note = existing[key]
		}
		if n := utf8.RuneCountInString(note); n < 3 || n > 300 {
			return &ValidationError{"item " + strconv.Itoa(i+1) + " (" + key + "): consentNote (who agreed to appear, and how) is required, 3-300 characters"}
		}
		stored[i] = Input{Kind: it.Kind, ID: it.ID, ConsentNote: note}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `DELETE FROM showcase_items`); err != nil {
		return err
	}
	for i, it := range stored {
		if _, err := tx.Exec(ctx, `INSERT INTO showcase_items (position, kind, ref_id, consent_note, added_by) VALUES ($1, $2, $3, $4, $5)`,
			i+1, it.Kind, it.ID, it.ConsentNote, adminID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// probe looks the candidates up in the catalogue the same way resolve does.
func (s *Store) probe(ctx context.Context, artists, albums []string) (map[string]row, error) {
	out := map[string]row{}
	ar, err := s.pool.Query(ctx, `
		SELECT a.id, a.name, COALESCE(u.avatar_image_id, a.avatar_image_id), COALESCE(a.image_url, ''),
		       EXISTS (SELECT 1 FROM albums al WHERE al.artist_id = a.id AND al.visibility = 'public'
		               AND EXISTS (SELECT 1 FROM tracks t WHERE t.album_id = al.id AND t.visibility = 'public'))
		FROM artists a LEFT JOIN users u ON u.id = a.owner_user_id WHERE a.id = ANY($1)`, artists)
	if err != nil {
		return nil, err
	}
	for ar.Next() {
		var r row
		var avatar *string
		var legacy string
		if err := ar.Scan(&r.id, &r.name, &avatar, &legacy, &r.hasPlayable); err != nil {
			ar.Close()
			return nil, err
		}
		r.kind, r.exists, r.imageURL = "artist", true, image.PersonURL(s.publicURL, avatar, legacy)
		out["artist/"+r.id] = r
	}
	ar.Close()
	if err := ar.Err(); err != nil {
		return nil, err
	}
	lr, err := s.pool.Query(ctx, `
		SELECT al.id, al.title, COALESCE(al.artwork_url, ''),
		       al.visibility = 'public' AND EXISTS (SELECT 1 FROM tracks t WHERE t.album_id = al.id AND t.visibility = 'public')
		FROM albums al WHERE al.id = ANY($1)`, albums)
	if err != nil {
		return nil, err
	}
	for lr.Next() {
		var r row
		if err := lr.Scan(&r.id, &r.name, &r.imageURL, &r.hasPlayable); err != nil {
			lr.Close()
			return nil, err
		}
		r.kind, r.exists = "album", true
		out["album/"+r.id] = r
	}
	lr.Close()
	return out, lr.Err()
}

// Handler serves GET /v1/showcase (public).
type Handler struct{ store *Store }

// NewHandler creates the handler.
func NewHandler(store *Store) *Handler { return &Handler{store: store} }

// List answers with the curated items. It is public (visitors are signed out)
// and cacheable for an hour.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	items, err := h.store.Public(r.Context())
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, http.StatusInternalServerError, "internal", "something went wrong")
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
}
