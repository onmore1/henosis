// Package fl finds out what FL Studio is doing on this computer: whether it
// runs, and which project its window names. See "Milestone 1" in the
// architecture plan for the observations this is built on.
package fl

import (
	"regexp"
	"strings"
)

// State is one observation of FL Studio.
type State struct {
	Running  bool
	Captions []string // captions of FL Studio's visible top-level windows
}

// Detector observes FL Studio.
type Detector interface {
	Detect() (State, error)
}

// CaptionKind says what a window caption tells us.
type CaptionKind int

const (
	CaptionOther     CaptionKind = iota // a dialog, a plugin window, ...
	CaptionNoProject                    // "FL Studio 2025": no project file is named
	CaptionFile                         // "My Song.flp - FL Studio 2025"
	CaptionTitle                        // "Some Title - FL Studio 2025": the project info has a title
)

// "<something> - FL Studio <edition>" or just "FL Studio <edition>". The
// greedy first group lets a title contain " - " itself.
var captionPattern = regexp.MustCompile(`^(?:(.+) - )?FL Studio(?: [^-]*)?$`)

// ParseCaption classifies a caption. For CaptionFile the text is the file
// name; for CaptionTitle it is the title.
func ParseCaption(caption string) (CaptionKind, string) {
	match := captionPattern.FindStringSubmatch(strings.TrimSpace(caption))
	if match == nil {
		return CaptionOther, ""
	}
	// Some builds mark unsaved changes with a star next to the name.
	text := strings.TrimSpace(strings.Trim(strings.TrimSpace(match[1]), "*"))
	switch {
	case text == "":
		return CaptionNoProject, ""
	case strings.HasSuffix(strings.ToLower(text), ".flp"):
		return CaptionFile, text
	default:
		return CaptionTitle, text
	}
}

var processPattern = regexp.MustCompile(`^fl(64)?( \(scaled\))?\.exe$`)

// IsFLProcessName reports whether an executable name is FL Studio's:
// FL64.exe, FL.exe and their "(scaled)" variants.
func IsFLProcessName(name string) bool {
	return processPattern.MatchString(strings.ToLower(strings.TrimSpace(name)))
}

// Tracker decides whether one project is open in FL Studio. It is sticky:
// a title typed into the project info replaces the file name in the caption,
// so once a project is known to be open it stays open until FL Studio shows
// something that rules it out.
type Tracker struct {
	open bool
}

// Update takes a new observation.
//
// flpNames are the base names of the project's .flp files; titles are the
// titles stored in those files; saved says a .flp in the project folder was
// written since the last call.
func (t *Tracker) Update(st State, flpNames, titles []string, saved bool) bool {
	if !st.Running {
		t.open = false
		return false
	}
	var matched, other, unknownTitle, none bool
	for _, caption := range st.Captions {
		kind, text := ParseCaption(caption)
		switch kind {
		case CaptionFile:
			if containsFold(flpNames, text) {
				matched = true
			} else {
				other = true
			}
		case CaptionTitle:
			if contains(titles, text) {
				matched = true
			} else {
				unknownTitle = true
			}
		case CaptionNoProject:
			none = true
		}
	}
	switch {
	case matched:
		t.open = true
	case saved && (unknownTitle || (!other && !none)):
		// FL Studio runs, a project file here was just written, and the
		// caption does not name a different file: the save came from it.
		t.open = true
	case t.open && !unknownTitle && (other || none):
		t.open = false
	}
	return t.open
}

// Open is the last answer of Update.
func (t *Tracker) Open() bool { return t.open }

func contains(list []string, text string) bool {
	for _, item := range list {
		if item == text {
			return true
		}
	}
	return false
}

func containsFold(list []string, text string) bool {
	for _, item := range list {
		if strings.EqualFold(item, text) {
			return true
		}
	}
	return false
}

// Fake is a Detector for tests and for platforms without FL Studio.
type Fake struct {
	State State
	Err   error
}

func (f *Fake) Detect() (State, error) { return f.State, f.Err }
