// Package transfer turns songs discovered on an artist's YouTube channel into
// releases, in a persistent queue with live progress. The artist supplies the
// audio files themselves; nothing here fetches media from YouTube.
package transfer

import (
	"errors"
	"time"
)

// Transfer statuses.
const (
	StatusCollecting  = "collecting" // the artist is still attaching audio
	StatusQueued      = "queued"
	StatusInProgress  = "in_progress"
	StatusRetrying    = "retrying"
	StatusCompleted   = "completed"
	StatusFailed      = "failed"
	StatusCancelled   = "cancelled"
	openStatusesSQL   = `('collecting', 'queued', 'in_progress', 'retrying')`
	maxItemsPerTransf = 50
)

// Item statuses (stored). The view also reports "waiting" for an uploaded file
// whose transfer has not started yet.
const (
	ItemAwaitingAudio = "awaiting_audio"
	ItemFetching      = "fetching" // the server is fetching the audio from the artist's YouTube video
	ItemUploading     = "uploading"
	ItemProcessing    = "processing"
	ItemCover         = "cover"
	ItemPublishing    = "publishing"
	ItemDone          = "done"
	ItemFailed        = "failed"
	ItemSkipped       = "skipped_duplicate"
	ItemDropped       = "dropped"
	itemWaiting       = "waiting"
)

var (
	ErrNotFound     = errors.New("transfer not found")
	ErrItemNotFound = errors.New("transfer item not found")
	ErrNoArtist     = errors.New("create your artist profile first")
	ErrActive       = errors.New("this artist already has an open transfer")
	ErrWrongState   = errors.New("not allowed in this transfer state")
	ErrNoAudio      = errors.New("no song has audio yet")
	// ErrAudioBusy: the file is being processed right now and cannot be removed.
	ErrAudioBusy    = errors.New("this file is being processed right now; try again in a moment")
	ErrUnknownVideo = errors.New("unknown video")
	// ErrRightsNotAccepted: importing audio from YouTube needs the artist's confirmation that they own the rights.
	ErrRightsNotAccepted = errors.New("confirm that you own the rights to the songs you import")
)

// ValidationError is a bad request; Msg is safe to show.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// AudioMissingError lists the items that still need a finished upload.
type AudioMissingError struct{ ItemIDs []string }

func (e *AudioMissingError) Error() string { return "some songs have no finished audio upload yet" }

// Item is one song of a transfer.
type Item struct {
	ID           string
	Position     int
	VideoID      string
	Title        string
	ReleaseType  string
	AlbumTitle   string
	Genre        string
	ReleaseDate  string
	Explicit     bool
	TrackNumber  *int
	ThumbnailURL string
	ReleaseID    string
	UploadID     string
	TrackID      string
	CoverImageID string
	CoverDone    bool
	Status       string
	Error        string
	DuplicateOf  string
	// UploadStatus and UploadError come from the linked upload (empty when none).
	UploadStatus  string
	UploadError   string
	UploadTrackID string

	// AudioSource is "youtube" (the server fetches the audio) or "upload" (the artist supplies a file).
	AudioSource   string
	FetchAttempts int
	NextFetchAt   *time.Time
	FetchError    string // why the automatic fetch gave up, shown while the song waits for a file
}

// Transfer is a queued unit of work for one artist.
type Transfer struct {
	ID            string
	UserID        string
	ArtistID      string
	Status        string
	Attempt       int
	MaxAttempts   int
	NextAttemptAt *time.Time
	Error         string
	Reason        string
	CreatedAt     time.Time
	SubmittedAt   *time.Time
	StartedAt     *time.Time
	FinishedAt    *time.Time
	Items         []Item

	TermsVersion  string
	QueuePosition int // 1-based among waiting transfers; 0 when not waiting
	QueueLength   int
}

// ItemRequest is what the artist chose for one video.
type ItemRequest struct {
	VideoID     string `json:"videoId"`
	Title       string `json:"title"`
	ReleaseType string `json:"releaseType"` // single (default) | album
	AlbumTitle  string `json:"albumTitle"`  // items with the same album title form one album
	Genre       string `json:"genre"`
	ReleaseDate string `json:"releaseDate"` // YYYY-MM-DD; default: earliest upload of the song
	Explicit    bool   `json:"explicit"`
	// Audio says where the audio comes from: "youtube" (fetched by the server from the video,
	// the default when that is available) or "upload" (a file the artist sends). Leave VideoID
	// empty, and give a Title, for a song that is not on YouTube.
	Audio string `json:"audio"`
}

// Rights is the artist's confirmation that they own what they import.
type Rights struct {
	Accepted     bool
	TermsVersion string
}

// NeedsAudio is reported when songs could not be fetched and wait for the artist's own files.
type NeedsAudio struct {
	TransferID string
	UserID     string
	Songs      int
}

// ImportDisabledError means the way of importing the request uses is switched off (see GET /v1/import-options).
type ImportDisabledError struct{ Option string }

func (e *ImportDisabledError) Error() string {
	return "importing with \"" + e.Option + "\" is not available right now"
}
