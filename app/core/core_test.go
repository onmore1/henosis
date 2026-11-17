package core

import (
	"strings"
	"testing"
)

var (
	hashA = strings.Repeat("a", 64)
	hashB = strings.Repeat("b", 64)
	hashC = strings.Repeat("c", 64)
)

func TestValidatePath(t *testing.T) {
	valid := []string{"My Song.flp", "Audio/vocal take 03.wav", "Audio/Malibu \u2014 Camargue.mp3", "a/b/c/d.txt",
		".hidden", "console.txt", "com10.txt", strings.Repeat("a", 200), strings.Repeat("\u00e9", 200), "R&B <not>"[:3]}
	for _, p := range valid {
		if err := ValidatePath(p); err != nil {
			t.Errorf("ValidatePath(%q) = %v, want nil", p, err)
		}
	}
	invalid := map[string]error{
		"": ErrPathEmpty, "/abs.txt": ErrPathAbsolute, "a//b": ErrPathSegment, "a/": ErrPathSegment,
		"./a": ErrPathDot, "a/../b": ErrPathDot, `a\b`: ErrPathForbidden, "C:/x": ErrPathForbidden,
		"what?.wav": ErrPathForbidden, "a|b": ErrPathForbidden, "tab\there": ErrPathControl,
		"trailing /x": ErrPathTrailing, "x/trailing.": ErrPathTrailing, "CON": ErrPathReserved,
		"Audio/nul.wav": ErrPathReserved, "lpt1": ErrPathReserved, "Com9.txt": ErrPathReserved,
		"bad\xff.wav": ErrPathUTF8, strings.Repeat("a", 201): ErrPathTooLong,
	}
	for p, want := range invalid {
		if got := ValidatePath(p); got != want {
			t.Errorf("ValidatePath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestIgnoreRules(t *testing.T) {
	ignored := []string{".henosis/base.json", "Backup/eltan (overwritten at 19h26).flp", "backups/x.flp",
		"Audio/Backup/x.wav", "eltan.flp~", "Audio/render.TMP", "x.part", "Thumbs.db", "Audio/desktop.ini"}
	for _, p := range ignored {
		if !IsIgnored(p) {
			t.Errorf("IsIgnored(%q) = false", p)
		}
	}
	for _, p := range []string{"eltan.flp", "Audio/Backup plan.wav", "Backups.flp"} {
		if IsIgnored(p) {
			t.Errorf("IsIgnored(%q) = true", p)
		}
	}
	if !IsFLSaveInProgress("eltan.flp~") || !IsFLSaveInProgress("sub/New.FLP~") ||
		IsFLSaveInProgress("eltan.flp") || IsFLSaveInProgress("notes.txt~") {
		t.Error("IsFLSaveInProgress is wrong")
	}
}

func TestManifestCanonicalForm(t *testing.T) {
	m := Manifest{Files: []FileEntry{{"My Song.flp", hashB, 482113}, {"Audio/vocal.wav", hashA, 18422060}}}
	m.Sort()
	// The same literal the C++ implementation is tested against.
	want := `{"format":1,"files":[{"path":"Audio/vocal.wav","sha256":"` + hashA +
		`","size":18422060},{"path":"My Song.flp","sha256":"` + hashB + `","size":482113}]}`
	if got := string(m.JSON()); got != want {
		t.Fatalf("JSON() = %s", got)
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	if string(Manifest{}.JSON()) != `{"format":1,"files":[]}` {
		t.Error("empty manifest JSON is wrong")
	}

	// Characters JSON libraries often escape must stay as they are.
	odd := Manifest{Files: []FileEntry{{"R&B \"take\" \u2014 1.wav", hashA, 1}}}
	if got := string(odd.JSON()); !strings.Contains(got, `"R&B \"take\" `+"\u2014"+` 1.wav"`) {
		t.Errorf("escaping changed: %s", got)
	}

	// Round trip with entries out of order and unknown keys.
	shuffled := `{"extra":{"x":[1,2]},"files":[{"size":482113,"path":"My Song.flp","sha256":"` + hashB +
		`","future":true},{"path":"Audio/vocal.wav","sha256":"` + hashA + `","size":18422060}],"format":1}`
	parsed, err := ParseManifest([]byte(shuffled))
	if err != nil || !parsed.Equal(m) {
		t.Fatalf("round trip failed: %v", err)
	}
	if m.Find("My Song.flp") == nil || m.Find("My Song.flp").Size != 482113 || m.Find("nope") != nil {
		t.Error("Find is wrong")
	}
}

func TestManifestRejects(t *testing.T) {
	bad := []string{
		`[]`, `{"format":2,"files":[]}`, `{"format":1}`,
		`{"format":1,"files":[{"path":"a","sha256":"` + hashA + `"}]}`,
		`{"format":1,"files":[{"path":"a","sha256":"short","size":1}]}`,
		`{"format":1,"files":[{"path":"../a","sha256":"` + hashA + `","size":1}]}`,
		`{"format":1,"files":[{"path":"a","sha256":"` + hashA + `","size":1},{"path":"a","sha256":"` + hashB + `","size":1}]}`,
		`{"format":1,"files":[{"path":"Kick.wav","sha256":"` + hashA + `","size":1},{"path":"kick.WAV","sha256":"` + hashB + `","size":1}]}`,
		`{"format":1,"files":[{"path":".henosis/x","sha256":"` + hashA + `","size":1}]}`,
		`{"format":1,"files":[{"path":"a","sha256":"` + hashA + `","size":-1}]}`,
	}
	for _, text := range bad {
		if _, err := ParseManifest([]byte(text)); err == nil {
			t.Errorf("ParseManifest accepted %s", text)
		}
	}
	if _, err := ParseManifest([]byte(`{"format":1,"files":[]}`)); err != nil {
		t.Error(err)
	}
}

func one(hash string) Manifest {
	if hash == "" {
		return Manifest{}
	}
	return Manifest{Files: []FileEntry{{"f", hash, 1}}}
}

func TestMergeNeverLosesLocalWork(t *testing.T) {
	values := []string{"", hashA, hashB, hashC}
	for _, b := range values {
		for _, l := range values {
			for _, h := range values {
				plan := PlanMerge(one(b), one(l), one(h))
				if l == h {
					if len(plan.Steps) != 0 {
						t.Errorf("b=%.1s l=%.1s h=%.1s: unexpected step", b, l, h)
					}
					continue
				}
				if len(plan.Steps) != 1 {
					t.Fatalf("b=%.1s l=%.1s h=%.1s: %d steps", b, l, h, len(plan.Steps))
				}
				a := plan.Steps[0].Action
				// Local content that is not in the history and differs from the head must survive.
				if l != "" && l != b && a != KeepLocal && a != Conflict {
					t.Errorf("b=%.1s l=%.1s h=%.1s: local work lost (action %d)", b, l, h, a)
				}
				// A local file is only replaced or removed when it is unchanged since the base, or gone.
				if (a == TakeHead || a == DeleteLocal) && l != b && l != "" {
					t.Errorf("b=%.1s l=%.1s h=%.1s: changed local file overwritten", b, l, h)
				}
				if l == b && a != TakeHead && a != DeleteLocal {
					t.Errorf("b=%.1s l=%.1s h=%.1s: untouched file did not follow the head", b, l, h)
				}
				if a == DeleteLocal && (h != "" || l == "") || a == TakeHead && h == "" {
					t.Errorf("b=%.1s l=%.1s h=%.1s: impossible action %d", b, l, h, a)
				}
			}
		}
	}
}

func TestMergeManyPathsAndDiff(t *testing.T) {
	base := Manifest{Files: []FileEntry{{"a", hashA, 1}, {"b", hashA, 1}, {"c", hashA, 1}, {"d", hashA, 1}}}
	local := Manifest{Files: []FileEntry{{"a", hashB, 1}, {"c", hashA, 1}, {"d", hashA, 1}, {"e", hashC, 1}}}
	head := Manifest{Files: []FileEntry{{"a", hashA, 1}, {"b", hashA, 1}, {"c", hashB, 1}, {"f", hashC, 1}}}
	plan := PlanMerge(base, local, head)
	want := map[string]MergeAction{"a": KeepLocal, "b": KeepLocal, "e": KeepLocal, "c": TakeHead, "f": TakeHead, "d": DeleteLocal}
	if len(plan.Steps) != len(want) || plan.Conflicts != 0 || !plan.HasDraft {
		t.Fatalf("plan = %+v", plan)
	}
	for _, step := range plan.Steps {
		if want[step.Path] != step.Action {
			t.Errorf("%s: action %d", step.Path, step.Action)
		}
	}
	d := DiffManifests(base, head)
	if len(d.Removed) != 1 || d.Removed[0] != "d" || len(d.Changed) != 1 || d.Changed[0].Path != "c" ||
		len(d.Added) != 1 || d.Added[0].Path != "f" || !DiffManifests(base, base).Empty() {
		t.Errorf("diff = %+v", d)
	}
}

func TestConflictCopyName(t *testing.T) {
	used := map[string]bool{}
	taken := func(name string) bool { return used[name] }
	cases := [][2]string{
		{"My Song.flp", "My Song (conflict, vadym, 2026-11-15).flp"},
		{"My Song.flp", "My Song (conflict, vadym, 2026-11-15 2).flp"},
		{"My Song.flp", "My Song (conflict, vadym, 2026-11-15 3).flp"},
		{"Audio/take.01.wav", "Audio/take.01 (conflict, vadym, 2026-11-15).wav"},
		{"Audio/noext", "Audio/noext (conflict, vadym, 2026-11-15)"},
		{".config", ".config (conflict, vadym, 2026-11-15)"},
	}
	for _, c := range cases {
		got := ConflictCopyName(c[0], "vadym", "2026-11-15", taken)
		if got != c[1] {
			t.Errorf("ConflictCopyName(%q) = %q, want %q", c[0], got, c[1])
		}
		used[got] = true
	}
	odd := ConflictCopyName("a.txt", "we/ird:name?", "2026-01-02", taken)
	if odd != "a (conflict, we_ird_name_, 2026-01-02).txt" || ValidatePath(odd) != nil {
		t.Errorf("odd user name gave %q", odd)
	}
}

func TestDescribeDiff(t *testing.T) {
	f := func(paths ...string) []FileEntry {
		var out []FileEntry
		for _, p := range paths {
			out = append(out, FileEntry{Path: p})
		}
		return out
	}
	cases := []struct {
		diff Diff
		want string
	}{
		{Diff{}, "No changes"},
		{Diff{Added: f("Audio/kick.wav")}, "Added kick.wav"},
		{Diff{Changed: f("My Song.flp")}, "Changed My Song.flp"},
		{Diff{Removed: []string{"a/b/c.wav"}}, "Removed c.wav"},
		{Diff{Added: f("a", "b", "c"), Changed: f("d"), Removed: []string{"e", "f"}}, "Added 3 files, changed 1, removed 2"},
		{Diff{Changed: f("a"), Removed: []string{"b"}}, "Changed 1 file, removed 1"},
		{Diff{Removed: []string{"a", "b"}}, "Removed 2 files"},
	}
	for _, c := range cases {
		if got := c.diff.Describe(); got != c.want {
			t.Errorf("Describe() = %q, want %q", got, c.want)
		}
	}
}
