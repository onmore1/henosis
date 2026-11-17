//go:build windows

package agent

import (
	"os/exec"
	"syscall"
)

// hideWindow keeps a console program started by the app from flashing a
// black window on screen (CREATE_NO_WINDOW).
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}
