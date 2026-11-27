package lyrics

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestTranslatedLyrics(t *testing.T) {
	src := "[00:12.50]Nkwagala nnyo\r\nI love you so much\n[00:17.20]Oh yeah\n[00:21.00]Tuula nange\nStay with me\n[00:25.40]\n[00:31.00]Nkwagala\nI love you"
	l, err := Build(Input{LRC: &src}, 60000)
	if err != nil {
		t.Fatal(err)
	}
	want := []Line{{12500, "Nkwagala nnyo", "I love you so much"}, {17200, "Oh yeah", ""}, {21000, "Tuula nange", "Stay with me"},
		{25400, "", ""}, {31000, "Nkwagala", "I love you"}}
	if !slices.Equal(l.Lines, want) {
		t.Fatalf("lines = %+v", l.Lines)
	}
	if l.Text != "Nkwagala nnyo\nOh yeah\nTuula nange\n\nNkwagala" || l.Untranslated() != 1 || l.LRC != strings.ReplaceAll(src, "\r\n", "\n") {
		t.Fatalf("text=%q untranslated=%d lrc=%q", l.Text, l.Untranslated(), l.LRC)
	}
	// what Format writes reads back the same
	if again, err := ParseLRC(Format(l.Lines), 60000); err != nil || !slices.Equal(again, want) {
		t.Fatalf("round trip = %+v %v", again, err)
	}
	// a repeated chorus shares its translation
	if got, err := ParseLRC("[00:10.00][00:40.00]Chorus\nIn English\n[00:20.00]Verse", 0); err != nil || len(got) != 3 || got[0].Translation != "In English" || got[2].Translation != "In English" {
		t.Fatalf("shared = %+v %v", got, err)
	}

	bad := strings.Join([]string{
		"At the top",                        // 1
		"[00:10.00]One",                     // 2
		"Translation",                       // 3
		"A second one",                      // 4
		"",                                  // 5
		"After a blank",                     // 6
		"[00:7]Short",                       // 7
		"[00:75.00]Seconds",                 // 8
		"[00:09.00]Backwards",               // 9
		"[00:10.00]Same time",               // 10
		"[00:20.00]",                        // 11
		"Words for a break",                 // 12
		"[02:00.00]Late",                    // 13
		"[00:30.00]" + "Fine",               // 14
		strings.Repeat("x", MaxLineRunes+1), // 15
	}, "\n")
	var ve *ValidationError
	if _, err := ParseLRC(bad, 90000); !errors.As(err, &ve) {
		t.Fatalf("accepted: %v", err)
	}
	var at []int
	said := map[int]string{}
	for _, e := range ve.Lines {
		at = append(at, e.Line)
		said[e.Line] = e.Message
	}
	if !slices.Equal(at, []int{1, 4, 6, 7, 8, 9, 10, 12, 13, 14, 15}) {
		t.Fatalf("wrong lines = %v %+v", at, ve.Lines)
	}
	for line, want := range map[int]string{
		1:  "Line 1 has no time stamp. A translation goes directly under its lyric line, one per line.",
		7:  "Line 7: write the time as [mm:ss.xx], for example [01:07.50].",
		9:  "Line 9: its time must be after [00:10.00] on line 2.",
		12: "Line 12: a music break has no translation.",
		13: "Line 13 is after the song ends (1:30).",
		14: "Line 14: its time must be after [02:00.00] on line 13.",
	} {
		if said[line] != want {
			t.Errorf("line %d says %q, want %q", line, said[line], want)
		}
	}
	if !strings.Contains(said[10], "A translation line has no time stamp.") || ve.Msg != said[1] {
		t.Fatalf("same time = %q, message = %q", said[10], ve.Msg)
	}
}
