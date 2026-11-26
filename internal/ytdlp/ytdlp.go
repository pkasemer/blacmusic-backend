// Package ytdlp fetches the audio stream of one YouTube video with the yt-dlp
// tool. It exists for artists who connected their own channel and asked us to
// import songs from it; callers must only pass ids of videos from that channel.
//
// YouTube delivers audio and video as separate DASH streams. yt-dlp picks the
// audio-only stream and downloads just that: no video is fetched.
package ytdlp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var videoIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

// ValidVideoID reports whether s looks like a YouTube video id.
func ValidVideoID(s string) bool { return videoIDRe.MatchString(s) }

// Error is a failed fetch. Permanent means asking again will not help (the video is private,
// removed, too long...): the artist should upload a file instead. Reason is safe to show.
type Error struct {
	Permanent bool
	Reason    string
	Detail    string // the tool's own words, for logs
}

func (e *Error) Error() string { return e.Reason + " (" + e.Detail + ")" }

// File is the downloaded audio.
type File struct {
	Path        string
	Name        string // <videoId>.<ext>
	ContentType string
	Size        int64
}

// Fetcher downloads the audio of a video into dir.
type Fetcher interface {
	Fetch(ctx context.Context, videoID, dir string) (*File, error)
}

// YtDlp runs the yt-dlp program.
type YtDlp struct {
	Path        string        // the yt-dlp executable
	JSRuntime   string        // "node" (or empty): YouTube's player needs a JavaScript runtime
	MaxBytes    int64         // the largest audio file accepted
	MaxDuration time.Duration // the longest video accepted (songs, not mixes or streams)
	Timeout     time.Duration // for the whole download
}

// New returns a fetcher with safe defaults for any zero value.
func New(path, jsRuntime string, maxBytes int64, maxDuration, timeout time.Duration) *YtDlp {
	if path == "" {
		path = "yt-dlp"
	}
	if maxBytes <= 0 {
		maxBytes = 150 << 20
	}
	if maxDuration <= 0 {
		maxDuration = 20 * time.Minute
	}
	if timeout <= 0 {
		timeout = 8 * time.Minute
	}
	return &YtDlp{Path: path, JSRuntime: jsRuntime, MaxBytes: maxBytes, MaxDuration: maxDuration, Timeout: timeout}
}

// args builds the command line. The URL is built here from a validated id and comes after
// "--", so nothing the artist sends can become an option.
func (y *YtDlp) args(videoID, dir string) []string {
	a := []string{
		"--ignore-config", "--no-playlist", "--no-warnings", "--no-progress", "--no-colors", "--no-cache-dir", "--no-mtime",
		"-f", "bestaudio[ext=m4a]/bestaudio",
		"--max-filesize", strconv.FormatInt(y.MaxBytes, 10),
		"--match-filter", fmt.Sprintf("duration <= %d & !is_live", int(y.MaxDuration.Seconds())),
		"--socket-timeout", "30", "--retries", "3", "--fragment-retries", "3",
		"-o", filepath.Join(dir, videoID+".%(ext)s"),
	}
	if y.JSRuntime != "" {
		a = append(a, "--js-runtimes", y.JSRuntime)
	}
	return append(a, "--", "https://www.youtube.com/watch?v="+videoID)
}

// Fetch downloads the audio-only stream of videoID into dir.
func (y *YtDlp) Fetch(ctx context.Context, videoID, dir string) (*File, error) {
	if !ValidVideoID(videoID) {
		return nil, &Error{Permanent: true, Reason: "that is not a YouTube video id", Detail: "invalid id"}
	}
	ctx, cancel := context.WithTimeout(ctx, y.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, y.Path, y.args(videoID, dir)...)
	cmd.Dir = dir
	// a clean environment: nothing of ours (database passwords, keys) is visible to the tool
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "LANG=C.UTF-8"}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = nil
	cmd.WaitDelay = 5 * time.Second // do not wait for stray child processes after a kill
	err := cmd.Run()

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, &Error{Reason: "the download took too long", Detail: "timeout after " + y.Timeout.String()}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		var nf *exec.Error
		if errors.As(err, &nf) || errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
			return nil, &Error{Permanent: true, Reason: "importing from YouTube is not available on this server", Detail: err.Error()}
		}
		return nil, classify(stderr.String(), err)
	}
	f, ferr := findFile(dir, videoID)
	if ferr != nil {
		// a clean exit without a file: the tool skipped the video because of a limit
		return nil, classifySkipped(stderr.String())
	}
	if f.Size > y.MaxBytes {
		_ = os.Remove(f.Path)
		return nil, &Error{Permanent: true, Reason: "the audio is too large to import", Detail: fmt.Sprintf("%d bytes", f.Size)}
	}
	return f, nil
}

var contentTypes = map[string]string{
	".m4a": "audio/mp4", ".mp4": "audio/mp4", ".webm": "audio/webm", ".opus": "audio/ogg", ".ogg": "audio/ogg",
	".mp3": "audio/mpeg", ".aac": "audio/aac", ".wav": "audio/wav", ".flac": "audio/flac",
}

func findFile(dir, videoID string) (*File, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, videoID+".") {
			continue
		}
		ext := strings.ToLower(filepath.Ext(name))
		ct, ok := contentTypes[ext]
		if !ok { // .part, .ytdl, .json and other leftovers are not the audio
			continue
		}
		info, err := e.Info()
		if err != nil || info.Size() == 0 {
			continue
		}
		return &File{Path: filepath.Join(dir, name), Name: name, ContentType: ct, Size: info.Size()}, nil
	}
	return nil, os.ErrNotExist
}

func classifySkipped(stderr string) error {
	l := strings.ToLower(stderr)
	switch {
	case strings.Contains(l, "does not pass filter"), strings.Contains(l, "is_live"):
		return &Error{Permanent: true, Reason: "the video is a live stream or longer than a song", Detail: tail(stderr)}
	case strings.Contains(l, "max-filesize"), strings.Contains(l, "larger than"):
		return &Error{Permanent: true, Reason: "the audio is too large to import", Detail: tail(stderr)}
	}
	d := tail(stderr)
	if d == "" {
		d = "the tool finished without producing an audio file"
	}
	return &Error{Reason: "YouTube returned no audio", Detail: d}
}

// classify turns yt-dlp's error text into a permanent or temporary failure.
func classify(stderr string, runErr error) error {
	l := strings.ToLower(stderr)
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(l, s) {
				return true
			}
		}
		return false
	}
	detail := tail(stderr)
	if detail == "" {
		detail = runErr.Error()
	}
	switch {
	case has("confirm your age", "age-restricted", "age restricted"):
		return &Error{Permanent: true, Reason: "the video is age-restricted", Detail: detail}
	case has("private video", "this video is private"):
		return &Error{Permanent: true, Reason: "the video is private", Detail: detail}
	case has("members-only", "members only", "join this channel"):
		return &Error{Permanent: true, Reason: "the video is for channel members only", Detail: detail}
	case has("not available in your country", "blocked it in your country", "blocked in your country"):
		return &Error{Permanent: true, Reason: "the video is not available from our server's country", Detail: detail}
	case has("video unavailable", "video is unavailable", "has been removed", "no longer available", "this video is not available", "account associated with this video has been terminated", "copyright"):
		return &Error{Permanent: true, Reason: "the video is unavailable or was removed", Detail: detail}
	case has("not a bot", "http error 429", "too many requests", "http error 403", "forbidden"):
		return &Error{Reason: "YouTube is blocking automatic downloads from our server right now", Detail: detail}
	case has("requested format is not available", "no video formats", "unable to extract", "nsig"):
		return &Error{Reason: "YouTube changed and the import tool needs an update", Detail: detail}
	}
	return &Error{Reason: "the download from YouTube failed", Detail: detail}
}

// tail keeps the last lines of the tool's error text (enough for a log line).
func tail(s string) string {
	s = strings.TrimSpace(s)
	lines := strings.Split(s, "\n")
	if len(lines) > 4 {
		lines = lines[len(lines)-4:]
	}
	out := strings.Join(lines, " | ")
	if len(out) > 400 {
		out = out[len(out)-400:]
	}
	return out
}
