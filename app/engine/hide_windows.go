//go:build windows

package engine

import "syscall"

// hideFolder marks the .henosis folder as hidden so it does not clutter the
// project folder in Explorer. Failing to do so is harmless.
func hideFolder(path string) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return
	}
	attrs, err := syscall.GetFileAttributes(p)
	if err != nil {
		return
	}
	_ = syscall.SetFileAttributes(p, attrs|syscall.FILE_ATTRIBUTE_HIDDEN)
}
