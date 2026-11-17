package core

import (
	"sort"
	"strconv"
	"strings"
)

// Diff lists what changed between two sorted manifests.
type Diff struct {
	Added   []FileEntry // in `to` only
	Changed []FileEntry // in both with different content; the `to` entry
	Removed []string    // in `from` only
}

// Empty reports whether nothing changed.
func (d Diff) Empty() bool { return len(d.Added) == 0 && len(d.Changed) == 0 && len(d.Removed) == 0 }

// Describe summarises a diff in one line, for example "Changed My Song.flp"
// or "Added 3 files, changed 1, removed 2".
func (d Diff) Describe() string {
	base := func(path string) string { return path[strings.LastIndexByte(path, '/')+1:] }
	total := len(d.Added) + len(d.Changed) + len(d.Removed)
	switch {
	case total == 0:
		return "No changes"
	case total == 1 && len(d.Added) == 1:
		return "Added " + base(d.Added[0].Path)
	case total == 1 && len(d.Changed) == 1:
		return "Changed " + base(d.Changed[0].Path)
	case total == 1:
		return "Removed " + base(d.Removed[0])
	}
	var parts []string
	add := func(verb string, n int) {
		if n == 0 {
			return
		}
		text := verb + " " + strconv.Itoa(n)
		if len(parts) == 0 {
			if n == 1 {
				text += " file"
			} else {
				text += " files"
			}
		}
		parts = append(parts, text)
	}
	add("added", len(d.Added))
	add("changed", len(d.Changed))
	add("removed", len(d.Removed))
	text := strings.Join(parts, ", ")
	return strings.ToUpper(text[:1]) + text[1:]
}

// DiffManifests compares two sorted manifests.
func DiffManifests(from, to Manifest) Diff {
	var d Diff
	i, j := 0, 0
	for i < len(from.Files) || j < len(to.Files) {
		switch {
		case j == len(to.Files) || (i < len(from.Files) && from.Files[i].Path < to.Files[j].Path):
			d.Removed = append(d.Removed, from.Files[i].Path)
			i++
		case i == len(from.Files) || to.Files[j].Path < from.Files[i].Path:
			d.Added = append(d.Added, to.Files[j])
			j++
		default:
			if from.Files[i].SHA256 != to.Files[j].SHA256 {
				d.Changed = append(d.Changed, to.Files[j])
			}
			i++
			j++
		}
	}
	return d
}

// MergeAction says what a pull does with one path.
type MergeAction int

const (
	// TakeHead writes the head's file at this path (create or replace).
	TakeHead MergeAction = iota
	// DeleteLocal removes a file the head deleted and the local side left alone.
	DeleteLocal
	// KeepLocal leaves a local-only change in place as part of the draft.
	KeepLocal
	// Conflict keeps the local file under a conflict name, then gives the
	// path to the head's version, if it has one.
	Conflict
)

// MergeStep is the decision for one path.
type MergeStep struct {
	Path        string
	Action      MergeAction
	Head        *FileEntry // the head's entry for this path, if it has one
	LocalExists bool
}

// MergePlan is the outcome of the three-way rule for a whole project.
type MergePlan struct {
	Steps     []MergeStep // only paths that need attention, sorted by path
	Conflicts int
	HasDraft  bool // local changes remain to be pushed
}

func contentAt(m Manifest, path string) string {
	if entry := m.Find(path); entry != nil {
		return entry.SHA256
	}
	return ""
}

// PlanMerge applies the three-way rule. base is the last synced version,
// local is what is on disk, head is the server. Paths where local and head
// already agree produce no step.
func PlanMerge(base, local, head Manifest) MergePlan {
	seen := map[string]bool{}
	var paths []string
	for _, m := range []Manifest{base, local, head} {
		for _, entry := range m.Files {
			if !seen[entry.Path] {
				seen[entry.Path] = true
				paths = append(paths, entry.Path)
			}
		}
	}
	sort.Strings(paths)

	var plan MergePlan
	for _, path := range paths {
		b, l, h := contentAt(base, path), contentAt(local, path), contentAt(head, path)
		if l == h {
			continue // both sides agree, whatever happened
		}
		step := MergeStep{Path: path, LocalExists: l != ""}
		if entry := head.Find(path); entry != nil {
			copied := *entry
			step.Head = &copied
		}
		switch {
		case l == b: // only the head changed
			if h == "" {
				step.Action = DeleteLocal
			} else {
				step.Action = TakeHead
			}
		case h == b: // only the local side changed (edited, created or deleted)
			step.Action = KeepLocal
			plan.HasDraft = true
		case l == "": // deleted locally, changed on the head: nothing local to keep
			step.Action = TakeHead
		default:
			step.Action = Conflict
			plan.Conflicts++
			plan.HasDraft = true // the conflict copy still has to be pushed
		}
		plan.Steps = append(plan.Steps, step)
	}
	return plan
}

func safeUser(user string) string {
	var b strings.Builder
	for _, c := range []byte(user) {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			b.WriteByte(c)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

// ConflictCopyName turns "Audio/take.wav" into
// "Audio/take (conflict, vadym, 2026-11-15).wav". taken says whether a
// candidate is already in use; a counter is added until a free name is found.
func ConflictCopyName(path, user, date string, taken func(string) bool) string {
	folder, leaf := "", path
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		folder, leaf = path[:i+1], path[i+1:]
	}
	stem, extension := leaf, ""
	if dot := strings.LastIndexByte(leaf, '.'); dot > 0 { // a leading dot is not an extension
		stem, extension = leaf[:dot], leaf[dot:]
	}
	label := "conflict, " + safeUser(user) + ", " + date
	for counter := 1; ; counter++ {
		name := folder + stem + " (" + label
		if counter > 1 {
			name += " " + strconv.Itoa(counter)
		}
		name += ")" + extension
		if !taken(name) {
			return name
		}
	}
}
