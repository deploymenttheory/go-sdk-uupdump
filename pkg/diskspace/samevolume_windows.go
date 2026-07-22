//go:build windows

package diskspace

import (
	"path/filepath"
	"strings"
)

// sameVolume compares the volume names of two existing paths (e.g. "C:" or a UNC
// share root). This is sufficient for the drive-letter paths the tooling uses.
func sameVolume(a, b string) bool {
	va, vb := filepath.VolumeName(a), filepath.VolumeName(b)
	if va == "" || vb == "" {
		return false
	}
	return strings.EqualFold(va, vb)
}
