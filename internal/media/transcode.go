package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// Output is one rendition written to disk.
type Output struct {
	Quality     Quality
	BitrateKbps int
	Path        string
	SizeBytes   int64
}

// FFmpegArgs builds one ffmpeg invocation that decodes the input once and
// writes every rung of the ladder. Metadata, chapters, cover art and
// everything but the first audio stream are dropped. Only the "file" protocol
// is allowed so a crafted input cannot make ffmpeg open other resources.
func FFmpegArgs(in, outDir string, channels int) (args []string, outs []Output) {
	if channels < 1 {
		channels = 1
	}
	if channels > 2 {
		channels = 2 // downmix surround; never spend bits on more
	}
	args = []string{
		"-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-protocol_whitelist", "file",
		"-i", in,
	}
	for _, r := range Ladder {
		path := filepath.Join(outDir, string(r.Quality)+"."+Container)
		args = append(args,
			"-map", "0:a:0",
			"-map_metadata", "-1", "-map_chapters", "-1",
			"-vn", "-sn", "-dn",
			"-c:a", "aac",
			"-b:a", strconv.Itoa(r.BitrateKbps)+"k",
			"-ar", strconv.Itoa(SampleRate),
			"-ac", strconv.Itoa(channels),
			"-movflags", "+faststart",
			"-f", "mp4",
			path,
		)
		outs = append(outs, Output{Quality: r.Quality, BitrateKbps: r.BitrateKbps, Path: path})
	}
	return args, outs
}

// Transcode produces every rendition of in inside outDir.
func Transcode(ctx context.Context, ffmpeg, in, outDir string, info *Info) ([]Output, error) {
	args, outs := FFmpegArgs(in, outDir, info.Channels)

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, ffmpeg, args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() > 0 {
			// The decoder rejected the stream: a problem with the file, not with us.
			slog.Warn("ffmpeg rejected input", "stderr", truncate(stderr.String(), 2000))
			return nil, &InvalidMediaError{"could not convert this audio (the file may be damaged)"}
		}
		return nil, fmt.Errorf("ffmpeg: %w", err) // timeout, killed, missing binary: retryable
	}

	for i := range outs {
		st, err := os.Stat(outs[i].Path)
		if err != nil || st.Size() == 0 {
			return nil, &InvalidMediaError{"could not convert this audio (no output)"}
		}
		outs[i].SizeBytes = st.Size()
	}
	return outs, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
