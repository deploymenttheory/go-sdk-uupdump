//go:build windows

package diskspace

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// available returns the bytes free to the caller on the volume containing dir.
func available(dir string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, fmt.Errorf("diskspace: %s: %w", dir, err)
	}
	var freeToCaller, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &freeToCaller, &total, &totalFree); err != nil {
		return 0, fmt.Errorf("diskspace: query %s: %w", dir, err)
	}
	return freeToCaller, nil
}
