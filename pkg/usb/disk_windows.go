//go:build windows

package usb

import (
	"encoding/csv"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// windowsDriver enumerates and prepares disks via PowerShell (Get-Disk) and
// diskpart. It shells out rather than calling IOCTLs directly — a pragmatic
// first cut that is honest about its dependency on the in-box tools; the
// interface allows a later raw-IOCTL implementation without changing callers.
type windowsDriver struct{}

func newDriver() (driver, error) { return windowsDriver{}, nil }

func (windowsDriver) list() ([]Disk, error) {
	// Number,FriendlyName,Size,BusType as CSV. BusType "USB" marks removable.
	out, err := powershell(`Get-Disk | Select-Object Number,FriendlyName,Size,BusType | ConvertTo-Csv -NoTypeInformation`)
	if err != nil {
		return nil, fmt.Errorf("usb: enumerate disks: %w", err)
	}
	rows, err := csv.NewReader(strings.NewReader(out)).ReadAll()
	if err != nil {
		return nil, fmt.Errorf("usb: parse disk list: %w", err)
	}
	var disks []Disk
	for i, r := range rows {
		if i == 0 || len(r) < 4 { // skip header
			continue
		}
		size, _ := strconv.ParseInt(strings.TrimSpace(r[2]), 10, 64)
		disks = append(disks, Disk{
			ID:        strings.TrimSpace(r[0]),
			Model:     strings.TrimSpace(r[1]),
			SizeBytes: size,
			Removable: strings.EqualFold(strings.TrimSpace(r[3]), "USB"),
		})
	}
	return disks, nil
}

func (windowsDriver) prepare(disk Disk, opts Options) (string, func() error, error) {
	convert := "mbr"
	if opts.Scheme == SchemeGPT {
		convert = "gpt"
	}
	fs := "fat32"
	if opts.FS == ExFAT {
		fs = "exfat"
	}
	// diskpart script: wipe, lay down the scheme, one primary partition, format,
	// mark active (MBR only, for BIOS boot), and assign a drive letter.
	var b strings.Builder
	fmt.Fprintf(&b, "select disk %s\n", disk.ID)
	b.WriteString("clean\n")
	fmt.Fprintf(&b, "convert %s\n", convert)
	b.WriteString("create partition primary\n")
	fmt.Fprintf(&b, "format fs=%s quick label=%s\n", fs, diskpartLabel(opts.VolumeLabel))
	if opts.Scheme == SchemeMBR {
		b.WriteString("active\n")
	}
	b.WriteString("assign\n")

	if err := runDiskpart(b.String()); err != nil {
		return "", nil, err
	}

	letter, err := driveLetterFor(disk.ID)
	if err != nil {
		return "", nil, err
	}
	return letter + `:\`, nil, nil
}

// driveLetterFor returns the drive letter assigned to the (single) partition of
// the given disk number, retrying briefly while Windows mounts the new volume.
func driveLetterFor(diskID string) (string, error) {
	cmd := fmt.Sprintf(`(Get-Partition -DiskNumber %s | Get-Volume).DriveLetter`, diskID)
	for attempt := 0; attempt < 10; attempt++ {
		out, err := powershell(cmd)
		if err == nil {
			if letter := strings.TrimSpace(out); letter != "" {
				return letter, nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return "", fmt.Errorf("usb: no drive letter assigned to disk %s", diskID)
}

// diskpartLabel sanitizes a FAT/exFAT volume label (uppercase, <=11 chars).
func diskpartLabel(s string) string {
	s = strings.ToUpper(s)
	if len(s) > 11 {
		s = s[:11]
	}
	if s == "" {
		s = "WINMEDIA"
	}
	return s
}

func runDiskpart(script string) error {
	f, err := os.CreateTemp("", "wmf-diskpart-*.txt")
	if err != nil {
		return fmt.Errorf("usb: diskpart script: %w", err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(script); err != nil {
		f.Close()
		return fmt.Errorf("usb: diskpart script: %w", err)
	}
	f.Close()

	out, err := exec.Command("diskpart", "/s", f.Name()).CombinedOutput() //nolint:gosec // fixed program
	if err != nil {
		return fmt.Errorf("usb: diskpart failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func powershell(command string) (string, error) {
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", command).Output() //nolint:gosec // fixed program
	if err != nil {
		return "", err
	}
	return string(out), nil
}
