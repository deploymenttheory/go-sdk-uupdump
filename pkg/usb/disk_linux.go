//go:build linux

package usb

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// linuxDriver enumerates disks with lsblk and prepares them with wipefs, sfdisk,
// and mkfs. It shells out to the standard util-linux / dosfstools / exfatprogs
// tools; those must be installed and the process must run as root.
type linuxDriver struct{}

func newDriver() (driver, error) { return linuxDriver{}, nil }

func (linuxDriver) list() ([]Disk, error) {
	// -d: whole disks only, -n: no header, -b: bytes, -o: columns.
	out, err := exec.Command("lsblk", "-dnbo", "NAME,MODEL,SIZE,RM,TYPE").Output() //nolint:gosec // fixed program
	if err != nil {
		return nil, fmt.Errorf("usb: lsblk: %w", err)
	}
	var disks []Disk
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[len(fields)-1] != "disk" {
			continue
		}
		// MODEL may contain spaces; reconstruct from the middle fields.
		name := fields[0]
		typ := fields[len(fields)-1]
		rm := fields[len(fields)-2]
		size := fields[len(fields)-3]
		model := strings.Join(fields[1:len(fields)-3], " ")
		if typ != "disk" {
			continue
		}
		sz, _ := strconv.ParseInt(size, 10, 64)
		disks = append(disks, Disk{
			ID:        "/dev/" + name,
			Model:     model,
			SizeBytes: sz,
			Removable: rm == "1",
		})
	}
	return disks, nil
}

func (linuxDriver) prepare(disk Disk, opts Options) (string, func() error, error) {
	if err := run("wipefs", "-a", disk.ID); err != nil {
		return "", nil, err
	}

	// Partition: a single primary spanning the disk. For MBR use partition type
	// 0x0c (FAT32 LBA) and mark it bootable; sfdisk's script format expresses
	// this as ",,c,*".
	label := "dos"
	line := ",,c,*"
	if opts.Scheme == SchemeGPT {
		label = "gpt"
		line = ",," // default GPT type (Linux filesystem); firmware boots FAT via ESP conventions
	}
	script := fmt.Sprintf("label: %s\n%s\n", label, line)
	if err := runInput(script, "sfdisk", disk.ID); err != nil {
		return "", nil, err
	}
	_ = run("partprobe", disk.ID)

	part := partitionNode(disk.ID, 1)
	if opts.FS == ExFAT {
		if err := run("mkfs.exfat", "-n", opts.VolumeLabel, part); err != nil {
			return "", nil, err
		}
	} else if err := run("mkfs.vfat", "-F", "32", "-n", fatLabel(opts.VolumeLabel), part); err != nil {
		return "", nil, err
	}

	mnt, err := os.MkdirTemp("", "wmf-usb-")
	if err != nil {
		return "", nil, fmt.Errorf("usb: mount dir: %w", err)
	}
	if err := run("mount", part, mnt); err != nil {
		_ = os.Remove(mnt)
		return "", nil, err
	}
	cleanup := func() error {
		_ = run("umount", mnt)
		return os.Remove(mnt)
	}
	return mnt, cleanup, nil
}

// partitionNode returns the device node for partition n of a disk, handling the
// "p" separator that nvme/mmc/loop devices use (e.g. /dev/nvme0n1p1).
func partitionNode(disk string, n int) string {
	base := disk
	last := base[len(base)-1]
	if last >= '0' && last <= '9' {
		return fmt.Sprintf("%sp%d", base, n)
	}
	return fmt.Sprintf("%s%d", base, n)
}

// fatLabel uppercases and truncates a FAT label to 11 characters.
func fatLabel(s string) string {
	s = strings.ToUpper(s)
	if len(s) > 11 {
		s = s[:11]
	}
	return s
}

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput() //nolint:gosec // fixed programs
	if err != nil {
		return fmt.Errorf("usb: %s failed: %v: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func runInput(stdin, name string, args ...string) error {
	cmd := exec.Command(name, args...) //nolint:gosec // fixed programs
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("usb: %s failed: %v: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}
