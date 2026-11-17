//go:build windows

package engine

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// hidden reports whether Windows marks the path as hidden.
func hidden(t *testing.T, path string) bool {
	t.Helper()
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	attrs, err := syscall.GetFileAttributes(p)
	if err != nil {
		t.Fatal(err)
	}
	return attrs&syscall.FILE_ATTRIBUTE_HIDDEN != 0
}

func TestStateFolderIsHidden(t *testing.T) {
	folder := t.TempDir()
	os.WriteFile(filepath.Join(folder, "song.flp"), []byte("v1"), 0o644)
	e := Open(folder, nil, "ann")
	// A scan alone creates the folder (for its file index), and it is hidden.
	if _, err := e.scan(); err != nil {
		t.Fatal(err)
	}
	meta := filepath.Join(folder, ".henosis")
	if _, err := os.Stat(meta); err != nil {
		t.Fatalf("the state folder was not created: %v", err)
	}
	if !hidden(t, meta) {
		t.Error("the state folder is not hidden after a scan")
	}
	// A folder left visible by an older version is hidden when it is opened.
	p, _ := syscall.UTF16PtrFromString(meta)
	attrs, _ := syscall.GetFileAttributes(p)
	if err := syscall.SetFileAttributes(p, attrs&^syscall.FILE_ATTRIBUTE_HIDDEN); err != nil {
		t.Fatal(err)
	}
	if hidden(t, meta) {
		t.Fatal("the test could not unhide the folder")
	}
	Open(folder, nil, "ann")
	if !hidden(t, meta) {
		t.Error("opening a project did not hide an unhidden state folder")
	}
}
