//go:build !windows

package diskspace

import (
	"os"
	"syscall"
)

// sameVolume compares the underlying device of two existing paths via stat's
// st_dev, so bind mounts and separate file systems are distinguished correctly.
func sameVolume(a, b string) bool {
	sa, err := os.Stat(a)
	if err != nil {
		return false
	}
	sb, err := os.Stat(b)
	if err != nil {
		return false
	}
	da, oka := sa.Sys().(*syscall.Stat_t)
	db, okb := sb.Sys().(*syscall.Stat_t)
	if !oka || !okb {
		return false
	}
	return da.Dev == db.Dev
}
