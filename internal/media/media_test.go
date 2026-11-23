package media

import (
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
