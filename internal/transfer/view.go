package transfer

import (
	"time"

	"github.com/blacmusic/backend/internal/upload"
)

// View is the JSON a client polls (and the SSE stream repeats).
type View struct {
	ID            string     `json:"id"`
	Status        string     `json:"status"`
	Queue         *QueueView `json:"queue"` // null unless queued or retrying
	Progress      Progress   `json:"progress"`
	Attempt       int        `json:"attempt"`
	MaxAttempts   int        `json:"maxAttempts"`
	NextAttemptAt *time.Time `json:"nextAttemptAt"`
	Error         *string    `json:"error"`
	Reason        *string    `json:"reason"`
	CreatedAt     time.Time  `json:"createdAt"`
	StartedAt     *time.Time `json:"startedAt"`
	FinishedAt    *time.Time `json:"finishedAt"`
	Items         []ItemView `json:"items"`
}

// QueueView says where a waiting transfer stands.
type QueueView struct {
	Position int `json:"position"`
	Length   int `json:"length"`
}

// Progress is the overall state.
type Progress struct {
	ItemsTotal   int `json:"itemsTotal"` // songs being transferred (duplicates and dropped ones not counted)
	ItemsDone    int `json:"itemsDone"`
	ItemsFailed  int `json:"itemsFailed"`
	ItemsSkipped int `json:"itemsSkipped"` // already in the catalog
	Percent      int `json:"percent"`
}

// UploadHint is what the app sends to POST /v1/uploads for this song.
type UploadHint struct {
	ReleaseID   string `json:"releaseId"`
	Title       string `json:"title"`
	Genre       string `json:"genre,omitempty"`
	TrackNumber *int   `json:"trackNumber,omitempty"`
	Explicit    bool   `json:"explicit"`
}

// ItemView is one song.
type ItemView struct {
	ID           string      `json:"id"`
	VideoID      string      `json:"videoId"`
	Title        string      `json:"title"`
	Genre        string      `json:"genre"`
	Explicit     bool        `json:"explicit"`
	Status       string      `json:"status"`
	Percent      int         `json:"percent"`
	ThumbnailURL string      `json:"thumbnailUrl,omitempty"`
	ReleaseType  string      `json:"releaseType"`
	AlbumTitle   string      `json:"albumTitle,omitempty"`
	ReleaseDate  string      `json:"releaseDate,omitempty"`
	ReleaseID    string      `json:"releaseId,omitempty"`
	UploadID     string      `json:"uploadId,omitempty"`
	TrackID      string      `json:"trackId,omitempty"`
	Error        *string     `json:"error"`
	DuplicateOf  string      `json:"duplicateOf,omitempty"`
	Upload       *UploadHint `json:"upload,omitempty"` // present while the song still needs audio
	// AudioSource is "youtube" while the server fetches the audio itself, otherwise "upload".
	AudioSource string `json:"audioSource"`
}

func itemStatus(t *Transfer, it Item) string {
	if terminal(it.Status) {
		return it.Status
	}
	switch it.UploadStatus {
	case "":
		if it.Status == ItemFetching {
			return ItemFetching
		}
		return ItemAwaitingAudio
	case string(upload.StatusPending):
		return ItemUploading
	case string(upload.StatusQueued):
		switch t.Status {
		case StatusCollecting, StatusQueued, StatusRetrying:
			return itemWaiting
		}
		return ItemProcessing
	case string(upload.StatusProcessing):
		return ItemProcessing
	case string(upload.StatusReady):
		if it.Status == ItemPublishing {
			return ItemPublishing
		}
		return ItemCover
	case string(upload.StatusFailed):
		return ItemFailed
	}
	return it.Status
}

var percentOf = map[string]int{
	ItemAwaitingAudio: 0, ItemFetching: 8, ItemUploading: 15, itemWaiting: 40, ItemProcessing: 60,
	ItemCover: 80, ItemPublishing: 90, ItemDone: 100, ItemFailed: 0,
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ToView builds the client's view of a transfer.
func ToView(t *Transfer) View {
	v := View{
		ID: t.ID, Status: t.Status, Attempt: t.Attempt, MaxAttempts: t.MaxAttempts, NextAttemptAt: nil,
		Error: optional(t.Error), Reason: optional(t.Reason), CreatedAt: t.CreatedAt, StartedAt: t.StartedAt, FinishedAt: t.FinishedAt,
		Items: make([]ItemView, 0, len(t.Items)),
	}
	if t.Status == StatusRetrying {
		v.NextAttemptAt = t.NextAttemptAt
	}
	if t.QueuePosition > 0 {
		v.Queue = &QueueView{Position: t.QueuePosition, Length: t.QueueLength}
	}
	sum := 0
	for _, it := range t.Items {
		st := itemStatus(t, it)
		iv := ItemView{
			ID: it.ID, VideoID: it.VideoID, Title: it.Title, Genre: it.Genre, Explicit: it.Explicit, Status: st, Percent: percentOf[st], ThumbnailURL: it.ThumbnailURL,
			ReleaseType: it.ReleaseType, AlbumTitle: it.AlbumTitle, ReleaseDate: it.ReleaseDate, ReleaseID: it.ReleaseID,
			UploadID: it.UploadID, TrackID: it.TrackID, DuplicateOf: it.DuplicateOf, AudioSource: it.AudioSource,
		}
		if st == ItemAwaitingAudio || st == ItemFetching {
			iv.Error = optional(it.FetchError) // why the automatic fetch gave up (or is trying again)
		}
		if st == ItemFailed {
			iv.Error = optional(orDefault(it.Error, it.UploadError))
		}
		switch st {
		case ItemSkipped:
			v.Progress.ItemsSkipped++
		case ItemDropped:
		default:
			v.Progress.ItemsTotal++
			sum += iv.Percent
			if st == ItemDone {
				v.Progress.ItemsDone++
			}
			if st == ItemFailed {
				v.Progress.ItemsFailed++
			}
		}
		if st == ItemAwaitingAudio || st == ItemFailed || st == ItemFetching { // fetching: the hint is for "use my own file instead"
			iv.Upload = &UploadHint{ReleaseID: it.ReleaseID, Title: it.Title, Genre: it.Genre, TrackNumber: it.TrackNumber, Explicit: it.Explicit}
		}
		v.Items = append(v.Items, iv)
	}
	if v.Progress.ItemsTotal > 0 {
		v.Progress.Percent = sum / v.Progress.ItemsTotal
	}
	if t.Status == StatusCompleted {
		v.Progress.Percent = 100
	}
	return v
}
