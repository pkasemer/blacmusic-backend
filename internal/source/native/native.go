// Package native is the first-party provider: tracks we host ourselves in
// S3-compatible storage and describe in Postgres.
package native

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/media"
	"github.com/blacmusic/backend/internal/source"
)

// Name is the source identifier stored in track_sources.source.
const Name = "native"

// Presigner creates client-reachable download URLs (implemented by storage.Store).
type Presigner interface {
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
}

// Provider serves our own catalog.
type Provider struct {
	pool   *pgxpool.Pool
	signer Presigner
	ttl    time.Duration
}

// New creates the native provider.
func New(pool *pgxpool.Pool, signer Presigner, urlTTL time.Duration) *Provider {
	return &Provider{pool: pool, signer: signer, ttl: urlTTL}
}

func (p *Provider) Name() string { return Name }

func (p *Provider) Capabilities() []source.Capability {
	return []source.Capability{source.CapSearch, source.CapOffline}
}

const trackSelect = `
	SELECT t.id, t.title, COALESCE(t.artist_id, ''), COALESCE(a.name, ''), COALESCE(t.album_id, ''), COALESCE(al.title, ''),
	       t.duration_ms, COALESCE(t.artwork_url, al.artwork_url, '')
	FROM tracks t
	LEFT JOIN artists a  ON a.id  = t.artist_id
	LEFT JOIN albums  al ON al.id = t.album_id`

func (p *Provider) Search(ctx context.Context, query string, limit, offset int) ([]source.Track, error) {
	pattern := "%" + escapeLike(query) + "%"
	rows, err := p.pool.Query(ctx, trackSelect+`
		WHERE t.visibility = 'public'
		  AND EXISTS (SELECT 1 FROM track_sources ts WHERE ts.track_id = t.id AND ts.source = 'native')
		  AND (t.title ILIKE $1 OR a.name ILIKE $1)
		ORDER BY t.title
		LIMIT $2 OFFSET $3`, pattern, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tracks := []source.Track{}
	for rows.Next() {
		var t source.Track
		if err := rows.Scan(&t.ID, &t.Title, &t.ArtistID, &t.ArtistName, &t.AlbumID, &t.AlbumTitle, &t.DurationMs, &t.ArtworkURL); err != nil {
			return nil, err
		}
		t.Source, t.SourceID = Name, t.ID
		tracks = append(tracks, t)
	}
	return tracks, rows.Err()
}

func (p *Provider) GetTrack(ctx context.Context, sourceID string) (*source.Track, error) {
	var t source.Track
	err := p.pool.QueryRow(ctx, trackSelect+` WHERE t.id = $1`, sourceID).
		Scan(&t.ID, &t.Title, &t.ArtistID, &t.ArtistName, &t.AlbumID, &t.AlbumTitle, &t.DurationMs, &t.ArtworkURL)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, source.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.Source, t.SourceID = Name, t.ID
	return &t, nil
}

// ResolvePlayback serves the requested quality (or the closest one the track
// has). Tracks without renditions (e.g. a hand-seeded file) fall back to the
// single media_key on track_sources.
func (p *Provider) ResolvePlayback(ctx context.Context, sourceID string, opts source.PlaybackOptions) (*source.PlaybackDescriptor, error) {
	want, ok := media.ParseQuality(opts.Quality)
	if !ok {
		want = media.Default
	}

	rows, err := p.pool.Query(ctx, `
		SELECT quality, media_key, codec, container, bitrate_kbps, size_bytes
		FROM track_renditions WHERE track_id = $1`, sourceID)
	if err != nil {
		return nil, err
	}
	type rendition struct {
		key, codec, container string
		kbps                  int
		size                  int64
	}
	have := map[media.Quality]rendition{}
	for rows.Next() {
		var q string
		var r rendition
		if err := rows.Scan(&q, &r.key, &r.codec, &r.container, &r.kbps, &r.size); err != nil {
			rows.Close()
			return nil, err
		}
		have[media.Quality(q)] = r
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(have) > 0 {
		avail := make([]media.Quality, 0, len(have))
		for _, rung := range media.Ladder { // ladder order keeps the options list stable
			if _, ok := have[rung.Quality]; ok {
				avail = append(avail, rung.Quality)
			}
		}
		chosen, _ := media.Pick(want, avail)
		r := have[chosen]
		url, err := p.signer.PresignGet(ctx, r.key, p.ttl)
		if err != nil {
			return nil, err
		}
		exp := time.Now().Add(p.ttl).UTC()
		d := &source.PlaybackDescriptor{
			Mode:      source.ModeURL,
			URL:       url,
			ExpiresAt: &exp,
			Format:    &source.Format{Codec: r.codec, Container: r.container, BitrateKbps: r.kbps},
			Quality:   string(chosen),
		}
		for _, q := range avail {
			o := have[q]
			d.Qualities = append(d.Qualities, source.QualityOption{Quality: string(q), BitrateKbps: o.kbps, SizeBytes: o.size})
		}
		return d, nil
	}

	var key, codec string
	var kbps int
	err = p.pool.QueryRow(ctx, `
		SELECT COALESCE(media_key, ''), COALESCE(codec, ''), COALESCE(bitrate_kbps, 0)
		FROM track_sources WHERE source = $1 AND source_id = $2`, Name, sourceID).
		Scan(&key, &codec, &kbps)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && key == "") {
		return nil, source.ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	url, err := p.signer.PresignGet(ctx, key, p.ttl)
	if err != nil {
		return nil, err
	}
	exp := time.Now().Add(p.ttl).UTC()
	return &source.PlaybackDescriptor{
		Mode:      source.ModeURL,
		URL:       url,
		ExpiresAt: &exp,
		Format:    &source.Format{Codec: codec, BitrateKbps: kbps},
	}, nil
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
