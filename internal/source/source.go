// Package source defines the pluggable audio-source contract.
//
// Every place audio can come from (our own catalog, podcasts, open catalogs, an
// optional client-resolved YouTube plugin) implements Provider. The API never
// cares which one it is talking to; the mobile app only sees a PlaybackDescriptor.
package source

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned when a provider has no such item.
var ErrNotFound = errors.New("source: not found")

// Capability advertises what a provider can do.
type Capability string

const (
	CapSearch         Capability = "search"
	CapLossless       Capability = "lossless"
	CapOffline        Capability = "offline"
	CapClientResolved Capability = "clientResolved"
)

// PlaybackMode tells the app how to play a PlaybackDescriptor.
type PlaybackMode string

const (
	ModeURL           PlaybackMode = "url"            // direct (possibly presigned) file URL
	ModeHLS           PlaybackMode = "hls"            // HLS manifest URL
	ModeDASH          PlaybackMode = "dash"           // DASH manifest URL
	ModeClientResolve PlaybackMode = "client_resolve" // app resolves the stream on-device
)

// Format describes the audio encoding.
type Format struct {
	Codec       string `json:"codec,omitempty"`
	Container   string `json:"container,omitempty"`
	BitrateKbps int    `json:"bitrateKbps,omitempty"`
}

// QualityOption is one quality a track can be played at.
type QualityOption struct {
	Quality     string `json:"quality"` // low | medium | high
	BitrateKbps int    `json:"bitrateKbps"`
	SizeBytes   int64  `json:"sizeBytes"` // for "about 4 MB" labels and download estimates
}

// PlaybackOptions are the client's preferences for one playback request.
type PlaybackOptions struct {
	Quality string // low | medium | high; empty means the server default
}

// ClientResolve names the on-device plugin and the id it should resolve.
type ClientResolve struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
}

// PlaybackDescriptor is what the mobile player consumes.
type PlaybackDescriptor struct {
	Mode          PlaybackMode      `json:"mode"`
	URL           string            `json:"url,omitempty"`
	ExpiresAt     *time.Time        `json:"expiresAt,omitempty"`
	Format        *Format           `json:"format,omitempty"`
	Quality       string            `json:"quality,omitempty"`   // the quality actually served
	Qualities     []QualityOption   `json:"qualities,omitempty"` // every quality available for this track
	Headers       map[string]string `json:"headers,omitempty"`
	ClientResolve *ClientResolve    `json:"clientResolve,omitempty"`
}

// Track is the normalised metadata every provider returns.
type Track struct {
	ID         string `json:"id,omitempty"` // Blac Music id (trk_...), when known
	Title      string `json:"title"`
	ArtistID   string `json:"artistId,omitempty"`
	ArtistName string `json:"artistName,omitempty"`
	AlbumID    string `json:"albumId,omitempty"`
	AlbumTitle string `json:"albumTitle,omitempty"`
	DurationMs int    `json:"durationMs"`
	ArtworkURL string `json:"artworkUrl,omitempty"`
	Source     string `json:"source,omitempty"`
	SourceID   string `json:"sourceId,omitempty"`
}

// Provider is one pluggable audio source.
type Provider interface {
	Name() string
	Capabilities() []Capability
	Search(ctx context.Context, query string, limit, offset int) ([]Track, error)
	GetTrack(ctx context.Context, sourceID string) (*Track, error)
	ResolvePlayback(ctx context.Context, sourceID string, opts PlaybackOptions) (*PlaybackDescriptor, error)
}

// Has reports whether p advertises capability c.
func Has(p Provider, c Capability) bool {
	for _, have := range p.Capabilities() {
		if have == c {
			return true
		}
	}
	return false
}
