package agent

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// sampleFinder finds a sample a project file refers to on this computer.
//
// A project file stores each sample's full path, and folders get renamed
// after a sample is used: a re-extracted pack becomes "Drum Kit Vol.IV\Perc
// & Hi-Hats" where the project still says "Drum Kit Vol . IV\Perc Hi Hats".
// FL Studio then finds the file by its name, so it plays, and Henosis must
// find it too, or it never travels with the project. When the stored path
// is gone, the finder goes up that path to the nearest folder that still
// exists and looks under it for one file of the same name.
type sampleFinder struct {
	mu    sync.Mutex
	cache map[string]finding
}

type finding struct {
	path string
	at   time.Time
}

// How hard the finder looks. A sample pack holds a few thousand files; the
// limits keep a search under an ancestor like Downloads short.
var (
	findMaxEntries = 20000
	findMaxDepth   = 8
	findAgainAfter = time.Minute // a sample not found is looked for again after this
)

// find returns where the sample is on this computer, or "".
func (f *sampleFinder) find(stored string) string {
	if info, err := os.Stat(stored); err == nil && !info.IsDir() {
		return stored
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cache == nil {
		f.cache = map[string]finding{}
	}
	if hit, ok := f.cache[stored]; ok {
		if hit.path != "" {
			if info, err := os.Stat(hit.path); err == nil && !info.IsDir() {
				return hit.path
			}
		} else if time.Since(hit.at) < findAgainAfter {
			return ""
		}
	}
	found := searchByName(stored)
	f.cache[stored] = finding{path: found, at: time.Now()}
	return found
}

// searchByName looks for the stored path's file name under the nearest
// existing folder of that path. It gives up on a name found twice, since it
// cannot tell which one the project meant, and never searches a whole drive
// or a whole user folder.
func searchByName(stored string) string {
	name := baseName(stored)
	if name == "" {
		return ""
	}
	dir := filepath.Dir(filepath.FromSlash(strings.ReplaceAll(stored, `\`, string(filepath.Separator))))
	home, _ := os.UserHomeDir()
	for {
		parent := filepath.Dir(dir)
		if parent == dir || !deepEnough(dir, home) {
			return ""
		}
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			break
		}
		dir = parent
	}
	base := strings.Count(filepath.Clean(dir), string(filepath.Separator))
	var found []string
	seen := 0
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if seen++; seen > findMaxEntries || len(found) > 1 {
			return filepath.SkipAll
		}
		if d.IsDir() {
			if strings.Count(filepath.Clean(path), string(filepath.Separator))-base >= findMaxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.EqualFold(d.Name(), name) {
			found = append(found, path)
		}
		return nil
	})
	if len(found) == 1 {
		return found[0]
	}
	return ""
}

// deepEnough says whether a folder is below the user folder's children (like
// Downloads\Some Pack), or elsewhere at least two levels below a drive, so a
// search never covers a drive or a whole user folder.
func deepEnough(dir, home string) bool {
	clean := filepath.Clean(dir)
	if home != "" {
		h := filepath.Clean(home)
		if strings.EqualFold(clean, h) || strings.HasPrefix(strings.ToLower(h), strings.ToLower(clean)+string(filepath.Separator)) {
			return false // the user folder itself, or above it
		}
		if strings.HasPrefix(strings.ToLower(clean), strings.ToLower(h)+string(filepath.Separator)) {
			return true
		}
	}
	vol := filepath.VolumeName(clean)
	rest := strings.Trim(strings.TrimPrefix(clean, vol), string(filepath.Separator))
	return strings.Count(rest, string(filepath.Separator)) >= 1
}
