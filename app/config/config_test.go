package config

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestLoadSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.json")
	c, err := Load(path)
	if err != nil || c.SignedIn() || len(c.Links) != 0 || c.StatusPort() != DefaultPort {
		t.Fatalf("empty config: %v %+v", err, c)
	}
	c.Server, c.Token, c.User, c.Device = "http://example", "hn_secret", "ann", "pc-1"
	c.Port = 5000
	if err := c.AddLink(Link{ProjectID: "p1", Name: "Song", Folder: filepath.Join(t.TempDir(), "Song")}); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
			t.Errorf("the config is readable by others: %v", info.Mode().Perm())
		}
	}
	again, err := Load(path)
	if err != nil || !again.SignedIn() || again.User != "ann" || len(again.Links) != 1 || again.StatusPort() != 5000 {
		t.Fatalf("reloaded: %v %+v", err, again)
	}
	os.WriteFile(path, []byte("{not json"), 0o600)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Errorf("a damaged config: %v", err)
	}
}

func TestLinks(t *testing.T) {
	root := t.TempDir()
	c := &Config{}
	song := filepath.Join(root, "Music", "Song")
	if err := c.AddLink(Link{ProjectID: "p1", Name: "Song", Folder: song}); err != nil {
		t.Fatal(err)
	}
	for name, folder := range map[string]string{
		"the same folder": song,
		"a folder inside": filepath.Join(song, "Audio"),
		"a folder around": filepath.Join(root, "Music"),
		"the drive root":  filepath.VolumeName(root) + string(filepath.Separator),
		"around the home": filepath.Dir(mustHome(t)),
		"the home folder": mustHome(t),
	} {
		if err := c.AddLink(Link{ProjectID: "p2", Name: "Other", Folder: folder}); err == nil {
			t.Errorf("%s (%s) was accepted", name, folder)
		}
	}
	if err := c.AddLink(Link{ProjectID: "p1", Name: "Song", Folder: filepath.Join(root, "Elsewhere")}); err == nil {
		t.Error("the same project was linked twice")
	}
	// A sibling whose name merely starts the same way is fine.
	if err := c.AddLink(Link{ProjectID: "p2", Name: "Song 2", Folder: song + " 2"}); err != nil {
		t.Errorf("a sibling folder was refused: %v", err)
	}
	if link := c.LinkFor(filepath.Join(song, "Audio", "deep")); link == nil || link.ProjectID != "p1" {
		t.Errorf("LinkFor inside the folder: %+v", link)
	}
	if link := c.LinkFor(song + " 2"); link == nil || link.ProjectID != "p2" {
		t.Errorf("LinkFor the sibling: %+v", link)
	}
	if c.LinkFor(root) != nil {
		t.Error("LinkFor an unlinked folder found something")
	}
	if !c.RemoveLink("p1") || c.RemoveLink("p1") || c.LinkByProject("p1") != nil || len(c.Links) != 1 {
		t.Errorf("RemoveLink: %+v", c.Links)
	}
}

func mustHome(t *testing.T) string {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home folder")
	}
	return home
}

func TestCleanFolderAndDevice(t *testing.T) {
	if _, err := CleanFolder("   "); err == nil {
		t.Error("an empty folder was accepted")
	}
	got, err := CleanFolder(` "` + filepath.Join(t.TempDir(), "a", "..", "b") + `" `)
	if err != nil || !filepath.IsAbs(got) || filepath.Base(got) != "b" || strings.Contains(got, "..") {
		t.Errorf("CleanFolder: %q %v", got, err)
	}
	a, b := NewDevice(), NewDevice()
	if !regexp.MustCompile(`^[a-z0-9-]+-[0-9a-f]{4}$`).MatchString(a) || a == b {
		t.Errorf("device names: %q %q", a, b)
	}
}
