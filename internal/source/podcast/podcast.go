// Package podcast is the source provider for podcast episodes. The audio stays
// on the publisher's server: playback hands the app the enclosure URL.
package podcast

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/blacmusic/backend/internal/source"
)

// Name is the source identifier.
const Name = "podcast"

// Provider resolves episode ids (epi_...).
type Provider struct{ pool *pgxpool.Pool }

// New creates the provider.
func New(pool *pgxpool.Pool) *Provider { return &Provider{pool: pool} }

func (p *Provider) Name() string { return Name }

// Capabilities is empty on purpose: episodes are found through the
// /v1/shows endpoints, not mixed into track search.
func (p *Provider) Capabilities() []source.Capability { return nil }

func (p *Provider) Search(context.Context, string, int, int) ([]source.Track, error) {
	return []source.Track{}, nil
}

func (p *Provider) GetTrack(ctx context.Context, sourceID string) (*source.Track, error) {
	var t source.Track
	err := p.pool.QueryRow(ctx, `
		SELECT e.id, e.title, s.title, e.duration_ms, COALESCE(NULLIF(e.image_url, ''), s.image_url)
		FROM episodes e JOIN shows s ON s.id = e.show_id WHERE e.id = $1`, sourceID).
		Scan(&t.ID, &t.Title, &t.ArtistName, &t.DurationMs, &t.ArtworkURL)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, source.ErrNotFound
	}
	t.Source, t.SourceID = Name, t.ID
	return &t, err
}

// ResolvePlayback returns the episode's enclosure URL. Quality does not apply:
// a podcast has one stream.
func (p *Provider) ResolvePlayback(ctx context.Context, sourceID string, _ source.PlaybackOptions) (*source.PlaybackDescriptor, error) {
	var url, mime string
	err := p.pool.QueryRow(ctx, `SELECT audio_url, mime_type FROM episodes WHERE id = $1`, sourceID).Scan(&url, &mime)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, source.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &source.PlaybackDescriptor{
		Mode:   source.ModeURL,
		URL:    url,
		Format: formatOf(mime),
	}, nil
}

// formatOf maps an enclosure MIME type to what the app needs to know.
func formatOf(mime string) *source.Format {
	switch strings.TrimSpace(strings.SplitN(mime, ";", 2)[0]) {
	case "audio/mpeg", "audio/mp3":
		return &source.Format{Codec: "mp3", Container: "mp3"}
	case "audio/mp4", "audio/x-m4a", "audio/m4a", "audio/aac":
		return &source.Format{Codec: "aac", Container: "m4a"}
	case "audio/ogg", "audio/opus":
		return &source.Format{Codec: "opus", Container: "ogg"}
	case "audio/wav", "audio/x-wav":
		return &source.Format{Codec: "pcm", Container: "wav"}
	}
	return &source.Format{}
}
