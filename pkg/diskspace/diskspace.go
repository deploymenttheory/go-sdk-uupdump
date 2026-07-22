// Package diskspace reports free disk space and guards large jobs (ISO builds,
// ESD downloads, USB writes) against starting when they would run out of room.
//
// It is pure Go and cross-platform: the per-OS query lives behind build tags in
// available_windows.go (GetDiskFreeSpaceEx) and available_unix.go (statfs).
package diskspace

import (
	"fmt"
	"os"
	"path/filepath"
)

// Available returns the number of bytes available to the current user on the
// file system that contains path. path need not exist — its nearest existing
// ancestor directory is queried, so callers can check space for an output file
// or work directory before creating it.
func Available(path string) (uint64, error) {
	return available(existingAncestor(path))
}

// EnsureAvailable returns an *InsufficientSpaceError if fewer than need bytes are
// free on the file system containing path.
func EnsureAvailable(path string, need uint64) error {
	avail, err := Available(path)
	if err != nil {
		return err
	}
	if avail < need {
		return &InsufficientSpaceError{Path: path, Need: need, Free: avail}
	}
	return nil
}

// SameVolume reports whether paths a and b resolve to the same file system, so a
// caller can decide whether their space requirements must be summed (same
// volume) or checked independently (different volumes). It resolves each path to
// its nearest existing ancestor first. On any resolution error it returns false,
// which is the safe default (independent checks).
func SameVolume(a, b string) bool {
	return sameVolume(existingAncestor(a), existingAncestor(b))
}

// InsufficientSpaceError reports that a location lacks the requested free space.
type InsufficientSpaceError struct {
	Path string
	Need uint64
	Free uint64
}

func (e *InsufficientSpaceError) Error() string {
	return fmt.Sprintf("diskspace: %s needs %s free but only %s available",
		e.Path, HumanBytes(e.Need), HumanBytes(e.Free))
}

// existingAncestor returns path if it exists, otherwise the nearest ancestor
// directory that does. It falls back to path unchanged if none is found (the
// platform query then surfaces the real error).
func existingAncestor(path string) string {
	p := path
	if !filepath.IsAbs(p) {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
	}
	for {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return path
		}
		p = parent
	}
}

// HumanBytes formats a byte count as a human-readable string (e.g. "4.68 GB").
func HumanBytes(n uint64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}
