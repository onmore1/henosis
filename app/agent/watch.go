package agent

import (
	"hash/fnv"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"henosis/core"
)

// stamp identifies one state of a file cheaply.
type stamp struct {
	Size  int64
	MTime int64
}

// folderView is a cheap look at a project folder: no file content is read.
type folderView struct {
	Missing   bool
	Unstable  bool             // the folder could not be read completely just now
	Saving    bool             // a "name.flp~" exists: FL Studio is in the middle of a save
	Sum       uint64           // changes whenever a synced file is added, removed or written
	FLPs      map[string]stamp // project files, by relative path
	Conflicts []string         // conflict copies waiting to be looked at
	Files     int
}

var conflictName = regexp.MustCompile(`\(conflict, [^()]*\)`)

func viewFolder(root string) folderView {
	view := folderView{FLPs: map[string]stamp{}}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		view.Missing = true
		return view
	}
	sum := fnv.New64a()
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			view.Unstable = true
			return nil
		}
		if path == root {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			if core.IsIgnored(rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if core.IsFLSaveInProgress(rel) {
			view.Saving = true
			sum.Write([]byte("saving:" + rel + "\n"))
			return nil
		}
		if core.IsIgnored(rel) {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			view.Unstable = true
			return nil
		}
		st := stamp{Size: info.Size(), MTime: info.ModTime().UnixNano()}
		sum.Write([]byte(rel + "\x00" + strconv.FormatInt(st.Size, 10) + "\x00" + strconv.FormatInt(st.MTime, 10) + "\n"))
		view.Files++
		if strings.HasSuffix(strings.ToLower(rel), ".flp") {
			view.FLPs[rel] = st
		}
		if conflictName.MatchString(rel) {
			view.Conflicts = append(view.Conflicts, rel)
		}
		return nil
	})
	view.Sum = sum.Sum64()
	return view
}

// flpsDiffer reports whether any project file was added, removed or written.
func flpsDiffer(a, b map[string]stamp) bool {
	if len(a) != len(b) {
		return true
	}
	for path, st := range a {
		if other, ok := b[path]; !ok || other != st {
			return true
		}
	}
	return false
}
