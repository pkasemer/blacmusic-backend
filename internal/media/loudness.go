package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"regexp"
	"strconv"
)

// Loudness is an EBU R128 measurement of a whole track.
type Loudness struct {
	IntegratedLUFS float64 // programme loudness
	TruePeakDB     float64 // true peak, dBTP
	RangeLU        float64 // loudness range
}

var (
	reIntegrated = regexp.MustCompile(`I:\s+(-?inf|-?[0-9]+(?:\.[0-9]+)?)\s+LUFS`)
	reRange      = regexp.MustCompile(`LRA:\s+(-?inf|-?[0-9]+(?:\.[0-9]+)?)\s+LU`)
	rePeak       = regexp.MustCompile(`Peak:\s+(-?inf|-?[0-9]+(?:\.[0-9]+)?)\s+dBFS`)
)

// ParseEBUR128 reads the summary ffmpeg's ebur128 filter prints when it
// finishes. It returns (nil, nil) for material with no measurable loudness
// (digital silence reports -inf), and an error when no summary is found.
func ParseEBUR128(stderr string) (*Loudness, error) {
	i := bytes.LastIndex([]byte(stderr), []byte("Summary:"))
	if i < 0 {
		return nil, errors.New("loudness: no ebur128 summary in ffmpeg output")
	}
	s := stderr[i:]
	num := func(re *regexp.Regexp) (float64, bool) {
		m := re.FindStringSubmatch(s)
		if m == nil {
			return 0, false
		}
		v, err := strconv.ParseFloat(m[1], 64)
		return v, err == nil
	}
	integrated, ok1 := num(reIntegrated)
	if !ok1 {
		return nil, errors.New("loudness: integrated loudness missing from the summary")
	}
	if math.IsInf(integrated, 0) || math.IsNaN(integrated) || integrated <= -69.9 {
		return nil, nil // silence (ffmpeg floors at -70 LUFS)
	}
	l := &Loudness{IntegratedLUFS: round1(integrated)}
	if v, ok := num(reRange); ok && !math.IsInf(v, 0) {
		l.RangeLU = round1(v)
	}
	if v, ok := num(rePeak); ok && !math.IsInf(v, 0) {
		l.TruePeakDB = round1(v)
	}
	return l, nil
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

// MeasureLoudness decodes a file and measures its integrated loudness, loudness
// range and true peak with ffmpeg's ebur128 filter. It returns (nil, nil) for
// silence. Only the "file" protocol is allowed, as everywhere else.
func MeasureLoudness(ctx context.Context, ffmpeg, path string) (*Loudness, error) {
	cmd := exec.CommandContext(ctx, ffmpeg,
		"-nostdin", "-hide_banner", "-nostats",
		"-protocol_whitelist", "file",
		"-i", path,
		"-map", "0:a:0", "-vn", "-sn", "-dn",
		"-af", "ebur128=peak=true:framelog=quiet",
		"-f", "null", "-")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg ebur128: %w", err)
	}
	return ParseEBUR128(stderr.String())
}
