// Package isoinspect inspects, validates, and manipulates Windows installation
// ISO images. It parses the three on-disc structures that matter for a bootable
// Windows install medium — the ISO9660 primary volume descriptor, the El Torito
// boot catalog, and the UDF file system — and reports correctness problems that
// keep media from booting even though lenient readers (e.g. macOS) accept it.
//
// Its headline check is the UDF allocation-descriptor validation: a file larger
// than ~1 GiB (boot.wim, install.wim) must be split across several block-aligned
// short allocation descriptors, because a single short_ad's length field is only
// 30 bits. Media that records one oversized descriptor mounts on macOS but the
// Windows boot manager cannot read boot.wim from it — the medium starts then
// hangs. This package detects exactly that class of defect.
package isoinspect

import (
	"fmt"
	"io"
	"os"
)

// sectorSize is the logical sector size of CD/DVD/UDF media.
const sectorSize = 2048

// Severity classifies a validation Issue.
type Severity string

const (
	// SeverityError marks a defect that prevents the medium from booting.
	SeverityError Severity = "error"
	// SeverityWarning marks a deviation from Microsoft's media that is unusual
	// or wasteful but not necessarily fatal.
	SeverityWarning Severity = "warning"
)

// Issue is a single validation finding.
type Issue struct {
	Severity Severity
	// Area is the structure the issue concerns: "iso9660", "eltorito", or "udf".
	Area string
	// Path is the file path within the UDF tree the issue concerns, when
	// applicable (otherwise empty).
	Path    string
	Message string
}

func (i Issue) String() string {
	if i.Path != "" {
		return fmt.Sprintf("[%s] %s: %s (%s)", i.Severity, i.Area, i.Message, i.Path)
	}
	return fmt.Sprintf("[%s] %s: %s", i.Severity, i.Area, i.Message)
}

// Report is the result of inspecting an ISO.
type Report struct {
	Path     string
	Size     int64
	ISO9660  *ISO9660Info
	ElTorito *ElToritoInfo
	UDF      *UDFInfo
	Issues   []Issue
}

// OK reports whether the ISO has no error-severity issues.
func (r *Report) OK() bool {
	for _, i := range r.Issues {
		if i.Severity == SeverityError {
			return false
		}
	}
	return true
}

// Errors returns only the error-severity issues.
func (r *Report) Errors() []Issue { return r.filter(SeverityError) }

// Warnings returns only the warning-severity issues.
func (r *Report) Warnings() []Issue { return r.filter(SeverityWarning) }

func (r *Report) filter(s Severity) []Issue {
	var out []Issue
	for _, i := range r.Issues {
		if i.Severity == s {
			out = append(out, i)
		}
	}
	return out
}

func (r *Report) addError(area, msg string)   { r.add(SeverityError, area, "", msg) }
func (r *Report) addWarning(area, msg string) { r.add(SeverityWarning, area, "", msg) }
func (r *Report) add(sev Severity, area, path, msg string) {
	r.Issues = append(r.Issues, Issue{Severity: sev, Area: area, Path: path, Message: msg})
}

// volume reads logical sectors from an ISO image.
type volume struct {
	ra   io.ReaderAt
	size int64
}

// sector returns the 2048-byte logical sector n.
func (v *volume) sector(n uint64) ([]byte, error) {
	b := make([]byte, sectorSize)
	if _, err := v.ra.ReadAt(b, int64(n*sectorSize)); err != nil {
		return nil, fmt.Errorf("read sector %d: %w", n, err)
	}
	return b, nil
}

// read returns length bytes starting at absolute byte offset.
func (v *volume) read(offset int64, length int) ([]byte, error) {
	b := make([]byte, length)
	if _, err := v.ra.ReadAt(b, offset); err != nil {
		return nil, fmt.Errorf("read %d bytes @%d: %w", length, offset, err)
	}
	return b, nil
}

// Inspect opens the ISO at path and returns a full Report. A non-nil error is
// returned only for I/O failures; structural defects are reported as Issues so a
// caller can inspect a malformed image without the call failing.
func Inspect(path string) (*Report, error) {
	f, err := os.Open(path) //nolint:gosec // caller-provided path
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return InspectReaderAt(f, info.Size(), path), nil
}

// InspectReaderAt inspects an ISO available through ra (size bytes). path is used
// only for reporting.
func InspectReaderAt(ra io.ReaderAt, size int64, path string) *Report {
	v := &volume{ra: ra, size: size}
	r := &Report{Path: path, Size: size}

	r.ISO9660 = inspectISO9660(v, r)
	r.ElTorito = inspectElTorito(v, r)
	r.UDF = inspectUDF(v, r)
	return r
}
