// Package core holds the rules every part of Henosis agrees on: which paths
// may be synced, what a manifest looks like, and how three versions of a
// project are merged. It does no network I/O.
package core

import (
	"errors"
	"strings"
	"unicode/utf8"
)

const maxPathCharacters = 200

var (
	ErrPathEmpty      = errors.New("the path is empty")
	ErrPathUTF8       = errors.New("the path is not valid UTF-8")
	ErrPathTooLong    = errors.New("the path is longer than 200 characters")
	ErrPathAbsolute   = errors.New("the path is not relative")
	ErrPathSegment    = errors.New("the path has an empty segment")
	ErrPathDot        = errors.New("the path has a '.' or '..' segment")
	ErrPathControl    = errors.New("the path has a control character")
	ErrPathForbidden  = errors.New("the path has a character Windows does not allow")
	ErrPathTrailing   = errors.New("a path segment ends in a space or a dot")
	ErrPathReserved   = errors.New("the path uses a reserved Windows device name")
	windowsFixedNames = map[string]bool{"con": true, "prn": true, "aux": true, "nul": true}
)

// asciiLower lower-cases ASCII letters only, so comparisons behave the same on
// every machine.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// AsciiLower is asciiLower for other packages.
func AsciiLower(s string) string { return asciiLower(s) }

func isReservedName(segment string) bool {
	stem := segment
	if i := strings.IndexByte(segment, '.'); i >= 0 {
		stem = segment[:i]
	}
	stem = asciiLower(stem)
	if windowsFixedNames[stem] {
		return true
	}
	if len(stem) == 4 && (strings.HasPrefix(stem, "com") || strings.HasPrefix(stem, "lpt")) {
		return stem[3] >= '1' && stem[3] <= '9'
	}
	return false
}

// ValidatePath checks one manifest path: relative, '/' separated, UTF-8, and
// storable on Windows. It returns nil when the path is acceptable.
func ValidatePath(path string) error {
	if path == "" {
		return ErrPathEmpty
	}
	if !utf8.ValidString(path) {
		return ErrPathUTF8
	}
	if utf8.RuneCountInString(path) > maxPathCharacters {
		return ErrPathTooLong
	}
	if path[0] == '/' {
		return ErrPathAbsolute
	}
	for _, segment := range strings.Split(path, "/") {
		switch {
		case segment == "":
			return ErrPathSegment
		case segment == "." || segment == "..":
			return ErrPathDot
		}
		for i := 0; i < len(segment); i++ {
			c := segment[i]
			if c < 0x20 {
				return ErrPathControl
			}
			if strings.IndexByte(`<>:"|?*\`, c) >= 0 {
				return ErrPathForbidden
			}
		}
		if last := segment[len(segment)-1]; last == ' ' || last == '.' {
			return ErrPathTrailing
		}
		if isReservedName(segment) {
			return ErrPathReserved
		}
	}
	return nil
}

// IsIgnored reports paths Henosis never uploads and never touches in a pull:
// its own metadata, FL Studio's backups, temp files and OS clutter.
func IsIgnored(relativePath string) bool {
	segments := strings.Split(relativePath, "/")
	for i, segment := range segments {
		lower := asciiLower(segment)
		if lower == ".henosis" || lower == "backups" || lower == "backup" {
			return true
		}
		if i == len(segments)-1 {
			return strings.HasSuffix(lower, ".tmp") || strings.HasSuffix(lower, ".part") ||
				strings.HasSuffix(lower, "~") || lower == "thumbs.db" || lower == "desktop.ini" ||
				lower == ".ds_store"
		}
	}
	return false
}

// IsFLSaveInProgress reports the temporary file FL Studio writes while it
// saves ("name.flp~"). A scan that sees one treats a save as in progress.
func IsFLSaveInProgress(relativePath string) bool {
	leaf := relativePath
	if i := strings.LastIndexByte(relativePath, '/'); i >= 0 {
		leaf = relativePath[i+1:]
	}
	return strings.HasSuffix(asciiLower(leaf), ".flp~")
}
