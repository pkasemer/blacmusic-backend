package titles

import "testing"

func TestNorm(t *testing.T) {
	same := [][2]string{
		{"Mukene (feat. Artin Pro)", "MUKENE"},
		{"Better (ft. Wana Benjamin)", "better"},
		{"One  Thang!", "one thang"},
		{"Song feat. Someone", "Song"},
	}
	for _, c := range same {
		if Norm(c[0]) != Norm(c[1]) {
			t.Errorf("Norm(%q)=%q != Norm(%q)=%q", c[0], Norm(c[0]), c[1], Norm(c[1]))
		}
	}
	if Norm("Alpha") == Norm("Alpha Two") {
		t.Error("different titles must differ")
	}
}

func TestClean(t *testing.T) {
	for in, want := range map[string]string{
		"BLESSED LYRICS VIDEO":                                       "Blessed",
		"MUKENE (Official Music Video)- AKEINE":                      "Mukene",
		"ONE THANG VISUALIZER":                                       "One Thang",
		"AKEINE- BOMBOCLAT BOI LYRIC VIDEO (ft Lamu, Navio)":         "Bomboclat Boi (ft Lamu, Navio)",
		"Saving My Love (SML) Lyrics Video - Akeine ft Kohen Jaycee": "Saving My Love (SML)",
		"WANTED full audio":                                          "Wanted",
		"UNTOLD - AKEINE X ANDY MUZIC":                               "Untold",
		"Plain Title":                                                "Plain Title",
	} {
		if got := Clean(in, "Akeine"); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKindAndRank(t *testing.T) {
	if Kind("MUKENE lyrics short", 30) != "short" || Kind("Song", 180) != "song" {
		t.Error("short/song")
	}
	if Kind("ONE THANG OUT NOW ON ALL STREAMING PLATFORMS!!!", 40) == "song" || Kind("Stream and Share 'One Thang'", 200) != "promo" {
		t.Error("promo")
	}
	if !(VariantRank("WANTED full audio") < VariantRank("X VISUALIZER") &&
		VariantRank("X VISUALIZER") < VariantRank("X lyric video") &&
		VariantRank("X lyric video") < VariantRank("X (Official Music Video)") &&
		VariantRank("X (Official Music Video)") < VariantRank("X remix")) {
		t.Error("variant order")
	}
}
