package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// InvalidMediaError means the file itself is unusable (not a retryable
// system failure). Its message is safe to show to the uploader.
type InvalidMediaError struct{ Msg string }

func (e *InvalidMediaError) Error() string { return e.Msg }

// Info is what we need to know about an uploaded file.
type Info struct {
	FormatName string
	Duration   time.Duration // 0 when the container does not say
	Channels   int
	SampleRate int
}

// allowedFormats are the demuxers we accept. ffprobe reports the real demuxer
// regardless of the file name, so playlists, concat lists and network demuxers
// (hls, concat, ...) are rejected here before ffmpeg ever opens them.
var allowedFormats = map[string]bool{
	"mp3": true, "mov": true, "mp4": true, "m4a": true, "flac": true, "ogg": true,
	"wav": true, "matroska": true, "webm": true, "aac": true, "aiff": true,
}

type probeJSON struct {
	Format struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
	} `json:"format"`
	Streams []struct {
		CodecType   string `json:"codec_type"`
		Channels    int    `json:"channels"`
		SampleRate  string `json:"sample_rate"`
		Disposition struct {
			AttachedPic int `json:"attached_pic"`
		} `json:"disposition"`
	} `json:"streams"`
}

// ParseProbe turns ffprobe's JSON into Info and checks it is acceptable audio.
// maxDuration <= 0 disables the length check.
func ParseProbe(data []byte, maxDuration time.Duration) (*Info, error) {
	var p probeJSON
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, &InvalidMediaError{"could not read this file"}
	}

	known := false
	for _, f := range strings.Split(p.Format.FormatName, ",") {
		if allowedFormats[f] {
			known = true
			break
		}
	}
	if !known {
		return nil, &InvalidMediaError{"unsupported file type (use mp3, m4a/aac, flac, wav, ogg/opus or aiff)"}
	}

	info := &Info{FormatName: p.Format.FormatName}
	audio := false
	for _, s := range p.Streams {
		if s.CodecType != "audio" || s.Disposition.AttachedPic == 1 {
			continue
		}
		audio = true
		info.Channels = s.Channels
		info.SampleRate, _ = strconv.Atoi(s.SampleRate)
		break
	}
	if !audio {
		return nil, &InvalidMediaError{"the file has no audio"}
	}
	if info.Channels < 1 {
		info.Channels = 2
	}

	if f, err := strconv.ParseFloat(p.Format.Duration, 64); err == nil && f > 0 {
		info.Duration = time.Duration(f * float64(time.Second))
	}
	if maxDuration > 0 && info.Duration > maxDuration {
		return nil, &InvalidMediaError{fmt.Sprintf("too long (the limit is %s)", maxDuration.Round(time.Minute))}
	}
	return info, nil
}

// Probe runs ffprobe on a local file.
func Probe(ctx context.Context, ffprobe, path string, maxDuration time.Duration) (*Info, error) {
	cmd := exec.CommandContext(ctx, ffprobe,
		"-v", "error",
		"-protocol_whitelist", "file",
		"-print_format", "json",
		"-show_format", "-show_streams",
		"-i", path)
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() > 0 {
			return nil, &InvalidMediaError{"could not read this file as audio"}
		}
		return nil, fmt.Errorf("ffprobe: %w", err)
	}
	return ParseProbe(out, maxDuration)
}
