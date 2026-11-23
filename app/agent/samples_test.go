package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSearchByNameFindsARenamedPack(t *testing.T) {
	root := t.TempDir()
	pack := filepath.Join(root, "Downloads", "Apoc Krysis Vol 1-4")
	real := filepath.Join(pack, "Apoc Krysis Drum Kit Vol.IV", "Perc & Hi-Hats", "Mystic Stylez Open Hat.wav")
	os.MkdirAll(filepath.Dir(real), 0o755)
	os.WriteFile(real, []byte("hat"), 0o644)
	// What the project file still says, from before the folders were renamed.
	stored := filepath.Join(pack, "Apoc Krysis Drum Kit Vol . IV", "Perc Hi Hats", "Mystic Stylez Open Hat.wav")

	var f sampleFinder
	if got := f.find(stored); got != real {
		t.Fatalf("found %q, want %q", got, real)
	}
	if got := f.find(real); got != real {
		t.Fatalf("an existing path: %q", got)
	}

	// The same name twice: it cannot tell which one, so it finds neither.
	twin := filepath.Join(pack, "Other Kit", "Mystic Stylez Open Hat.wav")
	os.MkdirAll(filepath.Dir(twin), 0o755)
	os.WriteFile(twin, []byte("another hat"), 0o644)
	if got := searchByName(stored); got != "" {
		t.Fatalf("a name found twice gave %q", got)
	}
}

func TestSearchByNameStaysOutOfDrivesAndUserFolders(t *testing.T) {
	home, _ := os.UserHomeDir()
	for _, dir := range []string{string(filepath.Separator), filepath.Join(string(filepath.Separator), "tmp"), home, filepath.Dir(home)} {
		if dir != "" && deepEnough(dir, home) {
			t.Errorf("%q would be searched", dir)
		}
	}
	if !deepEnough(filepath.Join(home, "Downloads"), home) || !deepEnough(filepath.Join(home, "Downloads", "Pack"), home) {
		t.Error("a folder inside the user folder is not searched")
	}
	// A path from another person's computer leads nowhere here.
	if got := searchByName(filepath.Join(string(filepath.Separator), "nonexistent-user-xyz", "Downloads", "hat.wav")); got != "" {
		t.Fatalf("found %q", got)
	}
}
