package media

import (
	"context"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestPick(t *testing.T) {
	all := []Quality{Low, Medium, High}
	cases := []struct {
		want Quality
		have []Quality
		got  Quality
	}{
		{Low, all, Low},
		{Medium, all, Medium},
		{High, all, High},
		{High, []Quality{Low, Medium}, Medium},
		{Low, []Quality{Medium, High}, Medium},
		{Medium, []Quality{Low, High}, Low}, // tie -> cheaper
		{High, []Quality{Low}, Low},
	}
	for _, c := range cases {
		got, ok := Pick(c.want, c.have)
		if !ok || got != c.got {
			t.Errorf("Pick(%s, %v) = %s,%v want %s", c.want, c.have, got, ok, c.got)
		}
	}
	if _, ok := Pick(Medium, nil); ok {
		t.Error("Pick with nothing available must fail")
	}
}

func TestParseQuality(t *testing.T) {
	if q, ok := ParseQuality(""); !ok || q != Medium {
		t.Errorf("empty = %s,%v want medium", q, ok)
	}
	for _, s := range []string{"low", "medium", "high"} {
		if q, ok := ParseQuality(s); !ok || string(q) != s {
			t.Errorf("%s not accepted", s)
		}
	}
	for _, s := range []string{"HIGH", "lossless", "../x", "low "} {
		if _, ok := ParseQuality(s); ok {
			t.Errorf("%q must be rejected", s)
		}
	}
}

func TestLadderIsOrderedAndSane(t *testing.T) {
	prev := 0
	for _, r := range Ladder {
		if r.BitrateKbps <= prev {
			t.Fatalf("ladder must ascend: %v", Ladder)
		}
		prev = r.BitrateKbps
	}
	if Ladder[0].Quality != Low || Ladder[len(Ladder)-1].Quality != High {
		t.Fatalf("ladder must run low..high: %v", Ladder)
	}
}

func TestFFmpegArgs(t *testing.T) {
	args, outs := FFmpegArgs("/tmp/in", "/tmp/out", 6)
	if len(outs) != len(Ladder) {
		t.Fatalf("want %d outputs, got %d", len(Ladder), len(outs))
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"-protocol_whitelist file", "-map_metadata -1", "-vn", "-movflags +faststart", "-b:a 48k", "-b:a 128k", "-b:a 256k"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q:\n%s", want, joined)
		}
	}
	if !strings.Contains(joined, "-ac 2") || strings.Contains(joined, "-ac 6") {
		t.Errorf("surround must be downmixed to 2 channels:\n%s", joined)
	}
	// Input protocol restriction must come before -i so it applies to the input.
	if slices.Index(args, "-protocol_whitelist") > slices.Index(args, "-i") {
		t.Error("-protocol_whitelist must precede -i")
	}
	_, mono := FFmpegArgs("/in", "/out", 1)
	_ = mono
	a, _ := FFmpegArgs("/in", "/out", 1)
	if !strings.Contains(strings.Join(a, " "), "-ac 1") {
		t.Error("mono input must stay mono")
	}
}

func TestParseProbe(t *testing.T) {
	mp3 := `{"format":{"format_name":"mp3","duration":"215.5"},"streams":[{"codec_type":"audio","channels":2,"sample_rate":"44100"}]}`
	info, err := ParseProbe([]byte(mp3), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if info.Channels != 2 || info.SampleRate != 44100 || info.Duration != time.Duration(215.5*float64(time.Second)) {
		t.Errorf("bad info: %+v", info)
	}

	m4a := `{"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"10"},"streams":[{"codec_type":"audio","channels":1,"sample_rate":"48000"}]}`
	if _, err := ParseProbe([]byte(m4a), time.Hour); err != nil {
		t.Errorf("m4a rejected: %v", err)
	}

	// Raw AAC has no duration; that is accepted (the output is measured later).
	raw := `{"format":{"format_name":"aac","duration":"N/A"},"streams":[{"codec_type":"audio","channels":2,"sample_rate":"44100"}]}`
	if info, err := ParseProbe([]byte(raw), time.Hour); err != nil || info.Duration != 0 {
		t.Errorf("raw aac: %+v %v", info, err)
	}

	bad := map[string]string{
		"hls playlist":   `{"format":{"format_name":"hls,applehttp","duration":"10"},"streams":[{"codec_type":"audio","channels":2}]}`,
		"concat":         `{"format":{"format_name":"concat","duration":"10"},"streams":[{"codec_type":"audio","channels":2}]}`,
		"no audio":       `{"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"10"},"streams":[{"codec_type":"video"}]}`,
		"only cover art": `{"format":{"format_name":"mp3","duration":"10"},"streams":[{"codec_type":"audio","disposition":{"attached_pic":1}}]}`,
		"too long":       `{"format":{"format_name":"mp3","duration":"99999"},"streams":[{"codec_type":"audio","channels":2}]}`,
		"not json":       `garbage`,
	}
	for name, js := range bad {
		_, err := ParseProbe([]byte(js), time.Hour)
		var inv *InvalidMediaError
		if err == nil || !asInvalid(err, &inv) {
			t.Errorf("%s: want InvalidMediaError, got %v", name, err)
		}
	}
}

func asInvalid(err error, target **InvalidMediaError) bool {
	e, ok := err.(*InvalidMediaError)
	if ok {
		*target = e
	}
	return ok
}

const ebur128Sample = `
[Parsed_ebur128_0 @ 0x7f1] Summary:

  Integrated loudness:
    I:         -18.1 LUFS
    Threshold: -28.1 LUFS

  Loudness range:
    LRA:         3.2 LU
    Threshold: -38.1 LUFS
    LRA low:   -20.0 LUFS
    LRA high:  -16.8 LUFS

  True peak:
    Peak:       -1.2 dBFS
`

func TestParseEBUR128(t *testing.T) {
	l, err := ParseEBUR128("lots of earlier ffmpeg output\nI: -99.0 LUFS (per-frame noise)\n" + ebur128Sample)
	if err != nil || l == nil {
		t.Fatalf("parse: %v %v", l, err)
	}
	if l.IntegratedLUFS != -18.1 || l.RangeLU != 3.2 || l.TruePeakDB != -1.2 {
		t.Fatalf("parsed %+v, want -18.1 LUFS / 3.2 LU / -1.2 dBTP", l)
	}

	// Digital silence has no loudness: not an error, just "unknown".
	silent := strings.ReplaceAll(ebur128Sample, "-18.1 LUFS\n    Threshold: -28.1", "-70.0 LUFS\n    Threshold: -70.0")
	if l, err := ParseEBUR128(silent); err != nil || l != nil {
		t.Fatalf("silence = %v, %v; want nil, nil", l, err)
	}
	inf := strings.ReplaceAll(ebur128Sample, "-18.1 LUFS\n    Threshold", "-inf LUFS\n    Threshold")
	if l, err := ParseEBUR128(inf); err != nil || l != nil {
		t.Fatalf("-inf = %v, %v; want nil, nil", l, err)
	}

	for name, text := range map[string]string{"empty": "", "no summary": "I: -20.0 LUFS", "no integrated": "Summary:\n  nothing here"} {
		if l, err := ParseEBUR128(text); err == nil {
			t.Errorf("%s: got %v, want an error", name, l)
		}
	}
}

func TestMeasureLoudnessOnKnownSignals(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	gen := func(name, filter string) string {
		path := filepath.Join(dir, name)
		out, err := exec.Command("ffmpeg", "-nostdin", "-loglevel", "error", "-y", "-f", "lavfi", "-i", filter,
			"-ac", "2", "-c:a", "pcm_s16le", path).CombinedOutput()
		if err != nil {
			t.Fatalf("generate %s: %v\n%s", name, err, out)
		}
		return path
	}
	ctx := context.Background()

	loud, err := MeasureLoudness(ctx, "ffmpeg", gen("loud.wav", "sine=f=1000:d=5"))
	if err != nil || loud == nil {
		t.Fatalf("loud: %v %v", loud, err)
	}
	// A mono 1 kHz sine at the filter's default amplitude (1/8) is -21.1 LUFS; spread
	// over two channels (-3 dB each, power preserved) the total stays -21.1 and each
	// channel peaks at -21.1 dBFS.
	if loud.IntegratedLUFS < -21.7 || loud.IntegratedLUFS > -20.5 {
		t.Fatalf("1 kHz sine = %.1f LUFS, want about -21.1", loud.IntegratedLUFS)
	}
	if loud.TruePeakDB < -22.5 || loud.TruePeakDB > -19.5 {
		t.Fatalf("true peak = %.1f dBTP, want about -21", loud.TruePeakDB)
	}

	quiet, err := MeasureLoudness(ctx, "ffmpeg", gen("quiet.wav", "sine=f=1000:d=5,volume=-6dB"))
	if err != nil || quiet == nil {
		t.Fatalf("quiet: %v %v", quiet, err)
	}
	if d := loud.IntegratedLUFS - quiet.IntegratedLUFS; d < 5.7 || d > 6.3 {
		t.Fatalf("6 dB quieter measured %.1f dB quieter", d)
	}

	silent, err := MeasureLoudness(ctx, "ffmpeg", gen("silent.wav", "anullsrc=r=44100:cl=stereo:d=3"))
	if err != nil || silent != nil {
		t.Fatalf("silence = %v, %v; want nil, nil", silent, err)
	}

	if _, err := MeasureLoudness(ctx, "ffmpeg", filepath.Join(dir, "missing.wav")); err == nil {
		t.Fatal("a missing file must be an error")
	}
}
