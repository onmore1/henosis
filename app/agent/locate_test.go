package agent_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"henosis/agent"
)

// A folder dragged into the window is found on disk by its name and the
// files it holds, because a browser never gives a page a real path.
func TestAddingADraggedFolder(t *testing.T) {
	d := newDesk(t, "ann")
	d.ok("/api/host/start", map[string]any{"name": "ann"}, nil)
	projects := filepath.Join(d.home, "Documents", "Image-Line", "FL Studio", "Projects")
	make := func(dir string, files map[string]string) string {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	song := make(filepath.Join(projects, "Night Drive"), map[string]string{"Night Drive.flp": "v1", "idea.wav": "wav"})
	make(filepath.Join(d.home, "Downloads", "Night Drive"), map[string]string{"unrelated.txt": "x"})
	make(filepath.Join(d.home, "Documents", "Elsewhere"), map[string]string{"Elsewhere.flp": "e"})
	os.WriteFile(filepath.Join(projects, "loose idea.flp"), []byte("idea"), 0o644)

	drop := func(name string, files ...map[string]any) (int, string) {
		return d.call("POST", "/api/drop", map[string]any{"name": name, "files": files}, nil)
	}
	file := func(name string, size int) map[string]any { return map[string]any{"name": name, "size": size} }

	// The folder with the matching files wins over the same name elsewhere.
	if code, text := drop("Night Drive", file("Night Drive.flp", 2), file("idea.wav", 3)); code != 200 || !strings.Contains(text, "Night Drive") {
		t.Fatalf("dropping the project folder: %d %s", code, text)
	}
	if p := projectByID(d.state(), d.state().Projects[0].ProjectID); p.Folder != song {
		t.Fatalf("the project was linked to %q, not %q", p.Folder, song)
	}
	// Dropping it a second time is refused, with a reason.
	if code, text := drop("Night Drive", file("Night Drive.flp", 2)); code != 400 || !strings.Contains(text, "already") {
		t.Fatalf("dropping the same folder twice: %d %s", code, text)
	}
	// A folder that is nowhere near this computer is explained, not guessed at.
	if code, text := drop("Some Other Song", file("Some Other Song.flp", 10)); code != 400 || !strings.Contains(text, "cannot find it on disk") {
		t.Fatalf("dropping an unknown folder: %d %s", code, text)
	}
	// A folder whose name matches but whose files do not is not taken either.
	if code, text := drop("Elsewhere", file("Night Drive.flp", 2)); code != 400 || !strings.Contains(text, "cannot find it on disk") {
		t.Fatalf("dropping a folder whose files do not match: %d %s", code, text)
	}
	// One .flp dropped from a project folder links that folder.
	make(filepath.Join(projects, "Second Song"), map[string]string{"Second Song.flp": "two"})
	if code, text := d.call("POST", "/api/drop", map[string]any{"files": []map[string]any{file("Second Song.flp", 3)}}, nil); code != 200 {
		t.Fatalf("dropping a .flp: %d %s", code, text)
	}
	// One .flp lying loose in the projects folder is copied into a folder of its own.
	if code, text := d.call("POST", "/api/drop", map[string]any{"files": []map[string]any{file("loose idea.flp", 4)}}, nil); code != 200 {
		t.Fatalf("dropping a loose .flp: %d %s", code, text)
	}
	copied := filepath.Join(d.home, "Documents", "Henosis", "loose idea", "loose idea.flp")
	if data, err := os.ReadFile(copied); err != nil || string(data) != "idea" {
		t.Fatalf("the loose file was not copied into its own folder: %v", err)
	}
	if _, err := os.Stat(filepath.Join(projects, "loose idea.flp")); err != nil {
		t.Error("the original loose file was moved instead of copied")
	}
	// Something that is not a project at all says so.
	if code, text := d.call("POST", "/api/drop", map[string]any{"files": []map[string]any{file("unrelated.txt", 1)}}, nil); code != 400 || !strings.Contains(text, ".flp") {
		t.Fatalf("dropping a random file: %d %s", code, text)
	}
	names := map[string]bool{}
	for _, p := range d.state().Projects {
		names[p.Name] = true
	}
	if len(names) != 3 || !names["Night Drive"] || !names["Second Song"] || !names["loose idea"] {
		t.Fatalf("projects: %v", names)
	}
	_ = agent.DroppedFile{}
}
