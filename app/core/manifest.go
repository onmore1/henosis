package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
)

// ManifestFormat is the only manifest format this code reads and writes.
const ManifestFormat = 1

// FileEntry is one file of a version.
type FileEntry struct {
	Path   string // relative to the project folder, '/' separated, UTF-8
	SHA256 string // 64 lowercase hex characters
	Size   uint64
}

// Manifest is the complete file list of one version of a project.
type Manifest struct {
	Files []FileEntry // kept sorted by path in byte order
}

// Sort orders the entries by path, as the canonical form requires.
func (m *Manifest) Sort() {
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
}

// Find returns the entry for a path, or nil. The manifest must be sorted.
func (m Manifest) Find(path string) *FileEntry {
	i := sort.Search(len(m.Files), func(i int) bool { return m.Files[i].Path >= path })
	if i < len(m.Files) && m.Files[i].Path == path {
		return &m.Files[i]
	}
	return nil
}

// Equal reports whether two sorted manifests list the same files.
func (m Manifest) Equal(other Manifest) bool {
	if len(m.Files) != len(other.Files) {
		return false
	}
	for i := range m.Files {
		if m.Files[i] != other.Files[i] {
			return false
		}
	}
	return true
}

// jsonString quotes text the canonical way: only quotes, backslashes and
// control characters are escaped; everything else stays as UTF-8.
func jsonString(buf *bytes.Buffer, text string) {
	const digits = "0123456789abcdef"
	buf.WriteByte('"')
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case c == '"':
			buf.WriteString(`\"`)
		case c == '\\':
			buf.WriteString(`\\`)
		case c < 0x20:
			buf.WriteString(`\u00`)
			buf.WriteByte(digits[c>>4])
			buf.WriteByte(digits[c&0x0F])
		default:
			buf.WriteByte(c)
		}
	}
	buf.WriteByte('"')
}

// JSON returns the canonical form: entries sorted by path, fixed key order,
// no whitespace. Two clients that scan the same folder produce the same bytes.
func (m Manifest) JSON() []byte {
	var buf bytes.Buffer
	buf.WriteString(`{"format":` + strconv.Itoa(ManifestFormat) + `,"files":[`)
	for i, entry := range m.Files {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(`{"path":`)
		jsonString(&buf, entry.Path)
		buf.WriteString(`,"sha256":`)
		jsonString(&buf, entry.SHA256)
		buf.WriteString(`,"size":` + strconv.FormatUint(entry.Size, 10) + `}`)
	}
	buf.WriteString("]}")
	return buf.Bytes()
}

// Hash is the SHA-256 of JSON(): the name a manifest is stored under.
func (m Manifest) Hash() string {
	sum := sha256.Sum256(m.JSON())
	return hex.EncodeToString(sum[:])
}

// IsSHA256Hex reports whether text is 64 lowercase hex characters.
func IsSHA256Hex(text string) bool {
	if len(text) != 64 {
		return false
	}
	for i := 0; i < len(text); i++ {
		c := text[i]
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Validate returns nil when the manifest obeys every rule: valid paths and
// hashes, nothing from the ignore list, sorted, no duplicates, and no two
// paths that differ only by case.
func (m Manifest) Validate() error {
	folded := make(map[string]bool, len(m.Files))
	for i, entry := range m.Files {
		if err := ValidatePath(entry.Path); err != nil {
			return fmt.Errorf("%v: %s", err, entry.Path)
		}
		if IsIgnored(entry.Path) {
			return fmt.Errorf("the path is on the ignore list: %s", entry.Path)
		}
		if !IsSHA256Hex(entry.SHA256) {
			return fmt.Errorf("bad hash for %s", entry.Path)
		}
		if i > 0 && m.Files[i-1].Path >= entry.Path {
			if m.Files[i-1].Path == entry.Path {
				return fmt.Errorf("duplicate path: %s", entry.Path)
			}
			return errors.New("entries are not sorted by path")
		}
		lower := asciiLower(entry.Path)
		if folded[lower] {
			return fmt.Errorf("two paths differ only by case: %s", entry.Path)
		}
		folded[lower] = true
	}
	return nil
}

type wireEntry struct {
	Path   *string `json:"path"`
	SHA256 *string `json:"sha256"`
	Size   *uint64 `json:"size"`
}

type wireManifest struct {
	Format *int        `json:"format"`
	Files  []wireEntry `json:"files"`
}

func decodeManifest(data []byte) (Manifest, error) {
	var wire wireManifest
	if err := json.Unmarshal(data, &wire); err != nil {
		return Manifest{}, errors.New("the manifest is not valid JSON")
	}
	if wire.Format == nil || *wire.Format != ManifestFormat {
		return Manifest{}, errors.New("unsupported manifest format")
	}
	if wire.Files == nil {
		return Manifest{}, errors.New("the manifest has no files array")
	}
	m := Manifest{Files: make([]FileEntry, 0, len(wire.Files))}
	for _, entry := range wire.Files {
		if entry.Path == nil || entry.SHA256 == nil || entry.Size == nil {
			return Manifest{}, errors.New("a file entry is missing path, sha256 or size")
		}
		m.Files = append(m.Files, FileEntry{Path: *entry.Path, SHA256: *entry.SHA256, Size: *entry.Size})
	}
	m.Sort()
	return m, nil
}

// ParseManifest reads manifest JSON, sorts the entries and validates them.
// Unknown keys are ignored so fields can be added later.
func ParseManifest(data []byte) (Manifest, error) {
	m, err := decodeManifest(data)
	if err != nil {
		return Manifest{}, err
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// MarshalJSON writes the canonical form, so a manifest can sit inside a
// larger JSON message.
func (m Manifest) MarshalJSON() ([]byte, error) { return m.JSON(), nil }

// UnmarshalJSON reads a manifest without validating it; whoever receives it
// decides what to do with an invalid one.
func (m *Manifest) UnmarshalJSON(data []byte) error {
	decoded, err := decodeManifest(data)
	if err != nil {
		return err
	}
	*m = decoded
	return nil
}
