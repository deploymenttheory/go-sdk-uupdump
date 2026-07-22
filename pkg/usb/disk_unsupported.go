//go:build !windows && !linux

package usb

// newDriver returns ErrUnsupported on platforms without a disk backend. The
// portable planning logic (planMedia) still works everywhere; only the
// privileged partition/format steps are gated.
func newDriver() (driver, error) { return nil, ErrUnsupported }
