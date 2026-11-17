package fl

import "testing"

func TestParseCaption(t *testing.T) {
	cases := []struct {
		caption string
		kind    CaptionKind
		text    string
	}{
		{"eltan.flp - FL Studio 2025", CaptionFile, "eltan.flp"},
		{"My Song.FLP - FL Studio 21", CaptionFile, "My Song.FLP"},
		{"  cz.flp - FL Studio 2025  ", CaptionFile, "cz.flp"},
		{"*cz.flp - FL Studio 2025", CaptionFile, "cz.flp"},
		{"cz.flp * - FL Studio 2025", CaptionFile, "cz.flp"},
		{"a - b.flp - FL Studio 2025", CaptionFile, "a - b.flp"},
		{"Night Drive - FL Studio 2025", CaptionTitle, "Night Drive"},
		{"Side A - Intro - FL Studio 2025", CaptionTitle, "Side A - Intro"},
		{"FL Studio 2025", CaptionNoProject, ""},
		{"FL Studio 21", CaptionNoProject, ""},
		{"FL Studio", CaptionNoProject, ""},
		{"Save changes?", CaptionOther, ""},
		{"Serum (Master)", CaptionOther, ""},
		{"FL Studios are great", CaptionOther, ""},
		{"song.flp - Notepad", CaptionOther, ""},
		{"", CaptionOther, ""},
	}
	for _, c := range cases {
		kind, text := ParseCaption(c.caption)
		if kind != c.kind || text != c.text {
			t.Errorf("ParseCaption(%q) = %v %q, want %v %q", c.caption, kind, text, c.kind, c.text)
		}
	}
}

func TestIsFLProcessName(t *testing.T) {
	for _, name := range []string{"FL64.exe", "fl64.exe", "FL.exe", "FL64 (scaled).exe", "FL (scaled).exe"} {
		if !IsFLProcessName(name) {
			t.Errorf("%q should be FL Studio", name)
		}
	}
	for _, name := range []string{"flux.exe", "FL64", "notFL64.exe", "FL64.exe.bak", "ILBridge.exe", ""} {
		if IsFLProcessName(name) {
			t.Errorf("%q should not be FL Studio", name)
		}
	}
}

func TestTracker(t *testing.T) {
	names := []string{"My Song.flp", "alt take.flp"}
	titles := []string{"Night Drive"}
	run := func(caps ...string) State { return State{Running: true, Captions: caps} }
	type step struct {
		what  string
		state State
		saved bool
		want  bool
	}
	scenarios := map[string][]step{
		"open by file name, close by another file": {
			{"FL Studio is not running", State{}, false, false},
			{"starting up, no caption yet", run(), false, false},
			{"our file is named", run("my song.flp - FL Studio 2025"), false, true},
			{"a dialog appears next to it", run("my song.flp - FL Studio 2025", "Save changes?"), false, true},
			{"another project is opened", run("other.flp - FL Studio 2025"), false, false},
		},
		"a title typed into the project info": {
			{"our file is named", run("My Song.flp - FL Studio 2025"), false, true},
			{"an unknown title replaces the name", run("Brand New Title - FL Studio 2025"), false, true},
			{"still the unknown title", run("Brand New Title - FL Studio 2025"), false, true},
			{"FL Studio exits", State{}, false, false},
		},
		"recognised by its stored title": {
			{"the stored title is shown", run("Night Drive - FL Studio 2025"), false, true},
			{"a new empty project", run("FL Studio 2025"), false, false},
		},
		"recognised by a save under an unknown title": {
			{"an unknown title", run("Something - FL Studio 2025"), false, false},
			{"a project file here is written", run("Something - FL Studio 2025"), true, true},
			{"stays open afterwards", run("Something - FL Studio 2025"), false, true},
		},
		"a save while another file is named is not ours": {
			{"another project is open", run("other.flp - FL Studio 2025"), false, false},
			{"a file here changes (copied in by hand)", run("other.flp - FL Studio 2025"), true, false},
		},
		"a save with no project named is not ours": {
			{"no project", run("FL Studio 2025"), true, false},
		},
		"a save before any caption exists": {
			{"FL Studio runs without a caption", run(), true, true},
		},
		"two FL Studio windows": {
			{"ours and another", run("other.flp - FL Studio 2025", "alt take.flp - FL Studio 2025"), false, true},
			{"ours gets an unknown title", run("other.flp - FL Studio 2025", "Hmm - FL Studio 2025"), false, true},
			{"only the other remains", run("other.flp - FL Studio 2025"), false, false},
		},
		"a save does nothing when FL Studio is not running": {
			{"not running", State{}, true, false},
		},
	}
	for name, steps := range scenarios {
		var tracker Tracker
		for _, s := range steps {
			if got := tracker.Update(s.state, names, titles, s.saved); got != s.want {
				t.Errorf("%s / %s: open = %v, want %v", name, s.what, got, s.want)
			}
		}
	}
}
