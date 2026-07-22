//go:build !windows

package diskspace

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// available returns the bytes available to an unprivileged user on the file
// system containing dir (statvfs f_bavail * f_bsize).
func available(dir string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, fmt.Errorf("diskspace: query %s: %w", dir, err)
	}
	//nolint:unconvert // Bavail/Bsize types vary by platform; conversion keeps it portable
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
