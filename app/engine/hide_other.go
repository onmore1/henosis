//go:build !windows

package engine

// hideFolder does nothing here: a leading dot already hides the folder.
func hideFolder(string) {}
