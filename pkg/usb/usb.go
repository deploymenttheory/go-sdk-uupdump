// Package usb writes bootable Windows installation media to a USB drive.
//
// It follows the file-copy model (as WinDiskWriter does, rather than a raw
// dd/image write): the target disk is partitioned and formatted, then the media
// tree is copied file by file, with two Windows-specific fix-ups —
//
//   - install.wim larger than FAT32's 4 GiB per-file limit is split into an
//     install.swm set (Windows Setup consumes install*.swm automatically); and
//   - if the media lacks an EFI boot loader, bootmgfw.efi is extracted from the
//     install image and placed at efi/boot/boot<arch>.efi.
//
// The portable orchestration (this file, media.go) has no OS calls and is unit
// tested against a temp directory. The privileged disk enumeration, partition,
// and format steps live behind build tags in disk_<os>.go; only Windows and
// Linux are implemented, and unsupported platforms return ErrUnsupported so the
// module still builds everywhere.
package usb

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/deploymenttheory/go-sdk-winmediafoundry/pkg/diskspace"
)

// ErrUnsupported is returned by disk operations on platforms without a backend.
var ErrUnsupported = errors.New("usb: disk operations are not supported on this platform")

// Scheme is a partition table type.
type Scheme string

const (
	// SchemeMBR is the most broadly bootable choice (BIOS and UEFI can both boot
	// a FAT32 MBR partition); it is the default, matching WinDiskWriter.
	SchemeMBR Scheme = "mbr"
	// SchemeGPT suits UEFI-only large media.
	SchemeGPT Scheme = "gpt"
)

// FS is a target file system.
type FS string

const (
	// FAT32 boots on the widest range of firmware but caps files at 4 GiB, so a
	// large install.wim is split into an install.swm set.
	FAT32 FS = "fat32"
	// ExFAT removes the 4 GiB limit but is not bootable by some older firmware.
	ExFAT FS = "exfat"
)

// fat32MaxFileSize is FAT32's hard per-file limit (UINT32_MAX bytes).
const fat32MaxFileSize = int64(1)<<32 - 1

// swmSliceSize is the target maximum for each install.swm part: comfortably
// under the FAT32 limit so a part never crosses it.
const swmSliceSize = int64(3900) * 1024 * 1024

// Disk identifies a candidate target disk.
type Disk struct {
	// ID is the platform device path used to address the disk: a diskpart disk
	// number on Windows (e.g. "1"), or a device node on Linux (e.g. "/dev/sdb").
	ID string
	// Model is a human-readable description.
	Model string
	// SizeBytes is the disk capacity.
	SizeBytes int64
	// Removable reports whether the OS marks the disk as removable. Fixed disks
	// are refused unless Options.Force is set.
	Removable bool
}

// Options configures a USB write.
type Options struct {
	Scheme      Scheme
	FS          FS
	VolumeLabel string
	// Force permits writing to a non-removable (fixed) disk. Off by default so a
	// mistyped disk id cannot wipe a system drive.
	Force bool
	// Progress, when non-nil, receives human-readable status lines.
	Progress io.Writer
}

func (o Options) withDefaults() Options {
	if o.Scheme == "" {
		o.Scheme = SchemeMBR
	}
	if o.FS == "" {
		o.FS = FAT32
	}
	if o.VolumeLabel == "" {
		o.VolumeLabel = "WINMEDIA"
	}
	return o
}

// driver is the platform-specific disk backend implemented in disk_<os>.go.
type driver interface {
	// list enumerates candidate disks.
	list() ([]Disk, error)
	// prepare wipes disk, writes the partition scheme + file system per opts, and
	// returns the path where the new volume is mounted (a drive root such as
	// "E:\\" on Windows or a mount directory on Linux) plus a cleanup func.
	prepare(disk Disk, opts Options) (mount string, cleanup func() error, err error)
}

// ListDisks enumerates candidate target disks on the current platform.
func ListDisks() ([]Disk, error) {
	d, err := newDriver()
	if err != nil {
		return nil, err
	}
	return d.list()
}

// WriteMedia partitions and formats disk, then writes the extracted Windows
// media tree at mediaRoot onto it. disk must be removable unless opts.Force.
//
// This is destructive: every existing partition on disk is erased.
func WriteMedia(ctx context.Context, disk Disk, mediaRoot string, opts Options) error {
	opts = opts.withDefaults()
	if !disk.Removable && !opts.Force {
		return fmt.Errorf("usb: refusing to write to non-removable disk %s (%s); set Force to override", disk.ID, disk.Model)
	}
	plan, err := planMedia(mediaRoot, opts)
	if err != nil {
		return err
	}
	// Fail fast if the media does not fit on the target disk (capacity, since the
	// disk is about to be wiped and reformatted). 5% headroom covers filesystem
	// metadata and cluster slack.
	if need := plan.totalBytes() + plan.totalBytes()/20; disk.SizeBytes > 0 && need > disk.SizeBytes {
		return fmt.Errorf("usb: media needs ~%s but disk %s holds only %s",
			diskspace.HumanBytes(uint64(need)), disk.ID, diskspace.HumanBytes(uint64(disk.SizeBytes)))
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	d, err := newDriver()
	if err != nil {
		return err
	}
	progressf(opts.Progress, "Partitioning %s (%s, %s)...\n", disk.ID, opts.Scheme, opts.FS)
	mount, cleanup, err := d.prepare(disk, opts)
	if err != nil {
		return err
	}
	defer func() {
		if cleanup != nil {
			_ = cleanup()
		}
	}()

	return plan.execute(ctx, mount, opts)
}
