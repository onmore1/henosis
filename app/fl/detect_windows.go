//go:build windows

package fl

import (
	"sync"
	"syscall"
	"unsafe"
)

var (
	user32                       = syscall.NewLazyDLL("user32.dll")
	procEnumWindows              = user32.NewProc("EnumWindows")
	procGetWindowTextW           = user32.NewProc("GetWindowTextW")
	procGetWindowTextLengthW     = user32.NewProc("GetWindowTextLengthW")
	procIsWindowVisible          = user32.NewProc("IsWindowVisible")
	procGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
)

// Window is one top-level window of FL Studio.
type Window struct {
	Caption string
	Visible bool
}

// The EnumWindows callback is created once: Windows callbacks made by
// syscall.NewCallback are never freed. enumMu guards the state it fills.
var (
	enumMu       sync.Mutex
	enumPIDs     map[uint32]bool
	enumWindows  []Window
	enumCallback uintptr
	enumOnce     sync.Once
)

func enumProc(hwnd syscall.Handle, _ uintptr) uintptr {
	var pid uint32
	procGetWindowThreadProcessId.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&pid)))
	if !enumPIDs[pid] {
		return 1 // keep enumerating
	}
	length, _, _ := procGetWindowTextLengthW.Call(uintptr(hwnd))
	if length == 0 || length > 4096 {
		return 1
	}
	buf := make([]uint16, length+1)
	copied, _, _ := procGetWindowTextW.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if copied == 0 || copied >= uintptr(len(buf)) {
		return 1
	}
	visible, _, _ := procIsWindowVisible.Call(uintptr(hwnd))
	enumWindows = append(enumWindows, Window{Caption: syscall.UTF16ToString(buf[:copied]), Visible: visible != 0})
	return 1
}

// flProcessIDs lists the process ids of running FL Studio instances.
func flProcessIDs() (map[uint32]bool, error) {
	snapshot, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer syscall.CloseHandle(snapshot)
	var entry syscall.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	pids := map[uint32]bool{}
	for err = syscall.Process32First(snapshot, &entry); err == nil; err = syscall.Process32Next(snapshot, &entry) {
		if IsFLProcessName(syscall.UTF16ToString(entry.ExeFile[:])) {
			pids[entry.ProcessID] = true
		}
	}
	return pids, nil // the loop ends with ERROR_NO_MORE_FILES
}

// Windows lists every captioned top-level window of FL Studio, visible or
// not. running is false when no FL Studio process exists.
func Windows() (windows []Window, running bool, err error) {
	pids, err := flProcessIDs()
	if err != nil {
		return nil, false, err
	}
	if len(pids) == 0 {
		return nil, false, nil
	}
	enumMu.Lock()
	defer enumMu.Unlock()
	enumOnce.Do(func() { enumCallback = syscall.NewCallback(enumProc) })
	enumPIDs, enumWindows = pids, nil
	ok, _, callErr := procEnumWindows.Call(enumCallback, 0)
	windows, enumPIDs, enumWindows = enumWindows, nil, nil
	if ok == 0 {
		return nil, true, callErr
	}
	return windows, true, nil
}

type systemDetector struct{}

// System returns the detector for this computer.
func System() Detector { return systemDetector{} }

func (systemDetector) Detect() (State, error) {
	windows, running, err := Windows()
	state := State{Running: running}
	for _, window := range windows {
		if window.Visible {
			state.Captions = append(state.Captions, window.Caption)
		}
	}
	return state, err
}
