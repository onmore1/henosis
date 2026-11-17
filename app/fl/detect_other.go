//go:build !windows

package fl

// Window is one top-level window of FL Studio.
type Window struct {
	Caption string
	Visible bool
}

// Windows reports nothing here: FL Studio detection exists for Windows only.
func Windows() ([]Window, bool, error) { return nil, false, nil }

// System returns the detector for this computer. Outside Windows it never
// sees FL Studio, so projects sync as plain folders.
func System() Detector { return &Fake{} }
