package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"henosis/config"
)

// DroppedFile is one file the window saw in a folder that was dragged in or
// picked. A browser never tells a page where a folder is on disk, so the app
// finds it: the folder's name and the names and sizes of the files inside it
// identify it well enough.
type DroppedFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// searchRoots are the places a project folder is looked for.
func (a *Agent) searchRoots() []string {
	if a.SearchRoots != nil {
		return a.SearchRoots
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	// The likely places first, then the whole home folder, so a project kept
	// somewhere of your own is still found.
	roots := []string{
		filepath.Join(home, "Documents", "Image-Line", "FL Studio", "Projects"),
		filepath.Join(home, "OneDrive", "Documents", "Image-Line", "FL Studio", "Projects"),
		filepath.Join(home, "Desktop"), filepath.Join(home, "Documents"), filepath.Join(home, "Downloads"),
		filepath.Join(home, "Music"), filepath.Join(home, "Videos"), filepath.Join(home, "OneDrive"),
		home,
	}
	if a.BaseFolder != "" {
		roots = append([]string{a.BaseFolder}, roots...)
	}
	return roots
}

// skipped names are folders a project is never in, and which can be huge.
var skipped = map[string]bool{"appdata": true, "application data": true, "node_modules": true,
	"backup": true, "backups": true, "windows": true, "program files": true, "program files (x86)": true,
	"programdata": true, "$recycle.bin": true, "onedrivetemp": true, "packages": true}

// walkRoots visits the folders under every search root, newest-looking places
// first, and stops after a budget so a huge home folder cannot hang the app.
func (a *Agent) walkRoots(visit func(path string, d fs.DirEntry) error) {
	budget := 150000
	seen := map[string]bool{}
	for _, root := range a.searchRoots() {
		if root == "" || seen[strings.ToLower(filepath.Clean(root))] {
			continue
		}
		seen[strings.ToLower(filepath.Clean(root))] = true
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if budget--; budget < 0 {
				return filepath.SkipAll
			}
			if d.IsDir() && path != root {
				name := strings.ToLower(d.Name())
				if strings.HasPrefix(name, ".") || skipped[name] {
					return filepath.SkipDir
				}
				rel, _ := filepath.Rel(root, path)
				if strings.Count(filepath.ToSlash(rel), "/") > 4 {
					return filepath.SkipDir
				}
			}
			return visit(path, d)
		})
	}
}

// LocateFolder finds the folder that was dragged in. It returns the paths
// that match, best first; an empty list means it is not under the places the
// app looks.
func (a *Agent) LocateFolder(name string, files []DroppedFile) []string {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, `\/:*?"<>|`) {
		return nil
	}
	type hit struct {
		path  string
		score int
		depth int
	}
	var hits []hit
	seen := map[string]bool{}
	a.walkRoots(func(path string, d fs.DirEntry) error {
		if !d.IsDir() || !strings.EqualFold(d.Name(), name) || seen[strings.ToLower(path)] {
			return nil
		}
		seen[strings.ToLower(path)] = true
		// Count how many of the dropped files are really in there.
		score := 0
		for _, f := range files {
			info, err := os.Stat(filepath.Join(path, f.Name))
			if err == nil && !info.IsDir() && (f.Size < 0 || info.Size() == f.Size) {
				score++
			}
		}
		hits = append(hits, hit{path: path, score: score, depth: strings.Count(path, string(filepath.Separator))})
		return nil
	})
	// The best match is the one holding most of the files; nearer the top wins ties.
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].depth < hits[j].depth
	})
	var out []string
	for _, h := range hits {
		if len(files) > 0 && h.score == 0 {
			continue // a folder of that name holding none of those files is not it
		}
		out = append(out, h.path)
	}
	return out
}

// LocateFile finds a single file that was dragged in, by name and size.
func (a *Agent) LocateFile(name string, size int64) []string {
	if strings.TrimSpace(name) == "" || strings.ContainsAny(name, `\/:*?"<>|`) {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	a.walkRoots(func(path string, d fs.DirEntry) error {
		if d.IsDir() || !strings.EqualFold(d.Name(), name) || seen[strings.ToLower(path)] {
			return nil
		}
		if info, err := d.Info(); err == nil && (size < 0 || info.Size() == size) {
			seen[strings.ToLower(path)] = true
			out = append(out, path)
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		return strings.Count(out[i], string(filepath.Separator)) < strings.Count(out[j], string(filepath.Separator))
	})
	return out
}

// looksLikeAProjectFolder reports whether a folder can be a project of its
// own. The big folders a project folder merely sits in cannot: linking one
// would sync everything in it.
func (a *Agent) looksLikeAProjectFolder(dir string) bool {
	dir = filepath.Clean(dir)
	tooBig := append(append([]string{}, a.searchRoots()...), a.ProjectRoots...)
	if home, err := os.UserHomeDir(); err == nil {
		tooBig = append(tooBig, home)
		for _, name := range []string{"Desktop", "Documents", "Downloads", "Music", "Videos", "OneDrive",
			filepath.Join("OneDrive", "Documents"), filepath.Join("Documents", "Image-Line", "FL Studio", "Projects"),
			filepath.Join("OneDrive", "Documents", "Image-Line", "FL Studio", "Projects")} {
			tooBig = append(tooBig, filepath.Join(home, name))
		}
	}
	for _, big := range tooBig {
		if strings.EqualFold(dir, filepath.Clean(big)) {
			return false
		}
	}
	return filepath.Dir(dir) != dir
}

// AddDropped adds the project that a folder or a file dragged into the
// window belongs to. folder is the dropped folder's name, empty when a single
// file was dropped; files are what the window saw inside it, or the file.
func (a *Agent) AddDropped(folder string, files []DroppedFile) (config.Link, error) {
	if folder == "" && len(files) == 1 {
		// A single file: its own folder becomes the project, unless it sits
		// loose in the FL Studio projects folder, which is not a project.
		paths := a.LocateFile(files[0].Name, files[0].Size)
		if len(paths) == 0 {
			return config.Link{}, fmt.Errorf("%s is not in your Documents, Desktop or Downloads, so the app cannot find it. Paste its folder below instead", files[0].Name)
		}
		if !strings.EqualFold(filepath.Ext(paths[0]), ".flp") {
			return config.Link{}, errors.New("drag the project's folder, or its .flp file")
		}
		dir := filepath.Dir(paths[0])
		if !a.looksLikeAProjectFolder(dir) {
			// It lies loose in a big folder, so that folder is not a project:
			// the file is copied into one of its own.
			return a.CreateFromFile(paths[0])
		}
		return a.CreateProject("", dir)
	}
	if folder == "" {
		return config.Link{}, errors.New("drag a project folder, or one .flp file")
	}
	paths := a.LocateFolder(folder, files)
	switch len(paths) {
	case 0:
		return config.Link{}, fmt.Errorf("the folder %q is not in your Documents, Desktop or Downloads, so the app cannot find it on disk. Paste its path below instead", folder)
	case 1:
		return a.CreateProject("", paths[0])
	default:
		if link, err := a.CreateProject("", paths[0]); err == nil {
			return link, nil
		}
		return config.Link{}, fmt.Errorf("there is more than one folder called %q on this computer (%s). Paste the right path below", folder, strings.Join(paths, ", "))
	}
}
