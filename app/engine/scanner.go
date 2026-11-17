// Package engine syncs one project folder with a remote: it scans the folder,
// pushes new versions, pulls newer ones with the three-way rule, and restores
// old versions. It knows nothing about HTTP or locks.
package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"henosis/core"
)

var (
	// ErrFolderMissing means the project folder is gone. It is never read as
	// "every file was deleted".
	ErrFolderMissing = errors.New("the project folder is missing")
	// ErrSaveInProgress means FL Studio's temporary "name.flp~" is present.
	ErrSaveInProgress = errors.New("FL Studio is saving the project")
	// ErrUnstable means a file changed or vanished while it was being read.
	ErrUnstable = errors.New("files changed while they were being read")
)

// IndexEntry is what the scanner remembers about a file so it does not hash
// it again.
type IndexEntry struct {
	Size   uint64 `json:"size"`
	MTime  int64  `json:"mtime"` // nanoseconds, only compared for equality
	SHA256 string `json:"sha256"`
}

// Index is the scanner's cache, keyed by relative path. It is never a source
// of truth: losing it only costs a full re-hash.
type Index map[string]IndexEntry

// LoadIndex reads a saved index; a missing or damaged file gives an empty one.
func LoadIndex(path string) Index {
	index := Index{}
	if data, err := os.ReadFile(path); err == nil {
		if json.Unmarshal(data, &index) != nil {
			return Index{}
		}
	}
	return index
}

// Save writes the index next to the project's other metadata.
func (index Index) Save(path string) error {
	data, err := json.Marshal(index)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}

// Scan is the result of reading a project folder.
type Scan struct {
	Manifest core.Manifest
	Hashed   int      // files whose content was read this time
	Skipped  []string // files left out because their path breaks a rule
}

// freshWindow: files modified this recently are not cached, so a second write
// inside the same timestamp tick can never be missed.
const freshWindow = 2 * time.Second

// HashFile returns the SHA-256 and size of a file.
func HashFile(path string) (string, uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), uint64(n), nil
}

// ScanFolder walks root, skips ignored paths and links, and hashes only files
// whose size or modified time differ from index. It returns the manifest and
// the index to use next time.
func ScanFolder(root string, index Index) (Scan, Index, error) {
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return Scan{}, nil, ErrFolderMissing
	}
	var scan Scan
	fresh := Index{}
	started := time.Now()

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return ErrUnstable
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.Type()&fs.ModeSymlink != 0 {
			return nil // links are skipped, never followed
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
			return ErrSaveInProgress
		}
		if core.IsIgnored(rel) {
			return nil
		}
		if core.ValidatePath(rel) != nil {
			scan.Skipped = append(scan.Skipped, rel)
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return ErrUnstable
		}
		size, mtime := uint64(info.Size()), info.ModTime().UnixNano()

		var sha string
		if cached, ok := index[rel]; ok && cached.Size == size && cached.MTime == mtime {
			sha = cached.SHA256
		} else {
			hashed, hashedSize, err := HashFile(path)
			if err != nil {
				return ErrUnstable
			}
			// The file must not have moved under us while it was read.
			after, err := os.Stat(path)
			if err != nil || hashedSize != size || uint64(after.Size()) != size || after.ModTime().UnixNano() != mtime {
				return ErrUnstable
			}
			sha = hashed
			scan.Hashed++
		}
		scan.Manifest.Files = append(scan.Manifest.Files, core.FileEntry{Path: rel, SHA256: sha, Size: size})
		if started.Sub(info.ModTime()) >= freshWindow {
			fresh[rel] = IndexEntry{Size: size, MTime: mtime, SHA256: sha}
		}
		return nil
	})
	if err != nil {
		return Scan{}, nil, err
	}
	scan.Manifest.Sort()
	if err := scan.Manifest.Validate(); err != nil {
		return Scan{}, nil, err
	}
	return scan, fresh, nil
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
