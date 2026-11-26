package importer

import (
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/blacmusic/backend/internal/genre"
	"github.com/blacmusic/backend/internal/image"
)

// Limits of one import.
const (
	MaxTracks    = 100
	maxFileName  = 255
	maxBasisRune = 500
)

// Request is the body of POST /v1/admin/imports: the manifest plus the list of files that will be sent.
type Request struct {
	Artist   ArtistSpec  `json:"artist"`
	Defaults Defaults    `json:"defaults"`
	Album    *AlbumSpec  `json:"album"`
	Tracks   []TrackSpec `json:"tracks"`
	Files    []FileSpec  `json:"files"`
	// RightsConfirmed and RightsBasis: the caller states that the right to publish these songs is held, and why
	// (for example "written permission from the artist, 20 Nov 2026"). Both are stored with the job.
	RightsConfirmed bool   `json:"rightsConfirmed"`
	RightsBasis     string `json:"rightsBasis"`
}

// ArtistSpec names the artist: an existing one by id, or a catalog artist found or created by name.
type ArtistSpec struct {
	ArtistID    string `json:"artistId"`
	Name        string `json:"name"`
	Bio         string `json:"bio"`
	PictureFile string `json:"pictureFile"`
}

// Defaults apply to every track.
type Defaults struct {
	Type        string  `json:"type"`       // single (default) | album
	Visibility  string  `json:"visibility"` // draft (default) | public
	ReleaseDate string  `json:"releaseDate"`
	Genre       *string `json:"genre"`
}

// AlbumSpec is the one release of an album import.
type AlbumSpec struct {
	Title     string  `json:"title"`
	CoverFile string  `json:"coverFile"`
	Genre     *string `json:"genre"`
}

// TrackSpec is one song.
type TrackSpec struct {
	Title       string  `json:"title"`
	Genre       *string `json:"genre"` // null or absent: left blank (or defaults.genre)
	Explicit    bool    `json:"explicit"`
	AudioFile   string  `json:"audioFile"`
	CoverFile   string  `json:"coverFile"`
	ReleaseDate string  `json:"releaseDate"`
	Note        string  `json:"note"`
}

// FileSpec is a file the caller will upload: name is how the manifest refers to it.
type FileSpec struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"sizeBytes"`
}

// problem is one reason a manifest is refused.
type problem struct {
	Title   string `json:"title,omitempty"`
	File    string `json:"file,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

var audioExt = map[string]bool{".mp3": true, ".m4a": true, ".aac": true, ".flac": true, ".wav": true, ".ogg": true, ".opus": true, ".aif": true, ".aiff": true, ".webm": true, ".mp4": true}

// normalizedTitle is how two titles are compared: trimmed, case folded, inner spaces collapsed.
func normalizedTitle(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

func validDate(s string) bool {
	if s == "" {
		return true
	}
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

// check validates a manifest and tidies it. maxAudio is the largest audio file accepted. All problems are
// returned, so a caller can fix a manifest in one go.
func check(req *Request, maxAudio int64) (files map[string]FileSpec, problems []problem) {
	add := func(title, file, code, msg string) {
		problems = append(problems, problem{Title: title, File: file, Code: code, Message: msg})
	}
	req.Artist.Name = strings.TrimSpace(req.Artist.Name) // the casing is kept exactly as sent
	req.Artist.ArtistID = strings.TrimSpace(req.Artist.ArtistID)
	req.Artist.Bio = strings.TrimSpace(req.Artist.Bio)
	req.RightsBasis = strings.TrimSpace(req.RightsBasis)
	if !req.RightsConfirmed {
		add("", "", "rights_not_accepted", "set rightsConfirmed to true to state that you hold the right to publish these songs")
	}
	if n := utf8.RuneCountInString(req.RightsBasis); n < 5 || n > maxBasisRune {
		add("", "", "rights_basis_required", "rightsBasis must say in 5-500 characters why you may publish these songs")
	}
	if req.Artist.ArtistID == "" {
		if n := utf8.RuneCountInString(req.Artist.Name); n < 1 || n > 100 {
			add("", "", "invalid_artist", "artist.name must be 1-100 characters")
		}
	}
	if utf8.RuneCountInString(req.Artist.Bio) > 1000 {
		add("", "", "invalid_artist", "artist.bio is at most 1000 characters")
	}
	switch req.Defaults.Type {
	case "":
		req.Defaults.Type = "single"
	case "single", "album":
	default:
		add("", "", "invalid_type", "defaults.type must be single or album (an EP is an album with up to 6 tracks)")
	}
	switch req.Defaults.Visibility {
	case "":
		req.Defaults.Visibility = "draft"
	case "draft", "public":
	default:
		add("", "", "invalid_visibility", "defaults.visibility must be public or draft")
	}
	if !validDate(req.Defaults.ReleaseDate) {
		add("", "", "invalid_date", "defaults.releaseDate must look like 2026-11-30")
	}
	if len(req.Tracks) < 1 || len(req.Tracks) > MaxTracks {
		add("", "", "invalid_tracks", "send 1 to 100 tracks")
	}

	files = map[string]FileSpec{}
	for _, f := range req.Files {
		if n := utf8.RuneCountInString(f.Name); n < 1 || n > maxFileName {
			add("", f.Name, "invalid_file", "a file name must be 1-255 characters")
			continue
		}
		if _, dup := files[f.Name]; dup {
			add("", f.Name, "invalid_file", "this file is listed twice")
			continue
		}
		files[f.Name] = f
	}

	genreOf := func(title string, own, fallback *string) string {
		g := own
		if g == nil {
			g = fallback
		}
		if g == nil {
			return ""
		}
		name, err := genre.Normalize(*g)
		if err != nil {
			add(title, "", "invalid_genre", "genre \""+*g+"\" is not in GET /v1/genres")
			return ""
		}
		return name
	}
	audio := func(title, name string) {
		f, ok := files[name]
		switch {
		case name == "":
			add(title, "", "missing_audio", "audioFile is required")
		case !ok:
			add(title, name, "missing_file", "audioFile is not in files")
		case f.SizeBytes < 1:
			add(title, name, "invalid_file", "the file is empty")
		case f.SizeBytes > maxAudio:
			add(title, name, "file_too_large", "an audio file is at most 2 GB")
		case !audioExt[strings.ToLower(path.Ext(name))]:
			// the content decides the real format; the name only has to look like audio
			add(title, name, "unsupported_audio", "use mp3, m4a/aac, flac, wav, ogg/opus or aiff")
		}
	}
	picture := func(title, name, what string) {
		if name == "" {
			return
		}
		f, ok := files[name]
		switch {
		case !ok:
			add(title, name, "missing_file", what+" is not in files")
		case f.SizeBytes < 1:
			add(title, name, "invalid_file", "the file is empty")
		case f.SizeBytes > image.MaxUploadBytes:
			add(title, name, "cover_too_large", "an image is at most 10 MB")
		}
	}
	picture("", req.Artist.PictureFile, "artist.pictureFile")

	usedAudio := map[string]bool{}
	for i := range req.Tracks {
		t := &req.Tracks[i]
		t.Title = strings.TrimSpace(t.Title)
		if n := utf8.RuneCountInString(t.Title); n < 1 || n > 200 {
			add(t.Title, t.AudioFile, "invalid_title", "title must be 1-200 characters")
		}
		if t.ReleaseDate == "" {
			t.ReleaseDate = req.Defaults.ReleaseDate
		}
		if !validDate(t.ReleaseDate) {
			add(t.Title, "", "invalid_date", "releaseDate must look like 2026-11-30")
		}
		g := genreOf(t.Title, t.Genre, req.Defaults.Genre)
		t.Genre = &g
		if utf8.RuneCountInString(t.Note) > 300 {
			t.Note = string([]rune(t.Note)[:300])
		}
		audio(t.Title, t.AudioFile)
		if t.AudioFile != "" && usedAudio[t.AudioFile] {
			add(t.Title, t.AudioFile, "invalid_file", "this audio file is used by two tracks")
		}
		usedAudio[t.AudioFile] = true
		if req.Defaults.Type == "single" {
			picture(t.Title, t.CoverFile, "coverFile")
		}
	}
	if req.Defaults.Type == "album" {
		if req.Album == nil {
			add("", "", "invalid_album", "an album import needs album.title")
		} else {
			req.Album.Title = strings.TrimSpace(req.Album.Title)
			if n := utf8.RuneCountInString(req.Album.Title); n < 1 || n > 200 {
				add("", "", "invalid_album", "album.title must be 1-200 characters")
			}
			g := genreOf(req.Album.Title, req.Album.Genre, req.Defaults.Genre)
			req.Album.Genre = &g
			picture(req.Album.Title, req.Album.CoverFile, "album.coverFile")
		}
	}
	return files, problems
}
