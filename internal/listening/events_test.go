package listening

import (
	"encoding/json"
	"testing"
)

func TestNewerEventFieldsAreTidied(t *testing.T) {
	for name, c := range map[string]struct {
		in                            string
		outcome, source, reason, seed string
	}{
		"an older app":                 {`{"outcome":"skipped","completion":20,"source":"album"}`, "skipped", "album", "", ""},
		"autoplay has its own source":  {`{"outcome":"completed","completion":100,"source":"search","autoplay":true,"autoplaySeedId":"trk_a","endReason":"ended"}`, "completed", "autoplay", "ended", "trk_a"},
		"a seed only with autoplay":    {`{"outcome":"stopped","source":"album","autoplaySeedId":"trk_a"}`, "stopped", "album", "", ""},
		"heard through is not skipped": {`{"outcome":"skipped","completion":95,"endReason":"next"}`, "completed", "", "next", ""},
		"ran out is not skipped":       {`{"outcome":"skipped","completion":20,"endReason":"ended"}`, "stopped", "", "ended", ""},
		"unseen is not skipped":        {`{"outcome":"skipped","completion":20,"endReason":"unobserved"}`, "stopped", "", "unobserved", ""},
		"a real skip stays":            {`{"outcome":"skipped","completion":20,"endReason":"next"}`, "skipped", "", "next", ""},
		"an unknown reason is dropped": {`{"outcome":"stopped","endReason":"moon"}`, "stopped", "", "", ""},
	} {
		var e event
		if err := json.Unmarshal([]byte(c.in), &e); err != nil {
			t.Fatal(err)
		}
		e.tidy()
		if e.Outcome != c.outcome || e.Source != c.source || e.EndReason != c.reason || e.SeedTrackID != c.seed {
			t.Errorf("%s: got %q %q %q %q", name, e.Outcome, e.Source, e.EndReason, e.SeedTrackID)
		}
	}
}
