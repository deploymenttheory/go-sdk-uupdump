package iso

import (
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/deploymenttheory/go-sdk-winmediafoundry/pkg/udf"
)

// Bridge image layout: ISO9660 descriptors and the El Torito boot catalog live
// in the reserved area before the UDF Volume Recognition Sequence.
const (
	isoPVDSector      = 16 // ISO9660 Primary Volume Descriptor
	isoBootRecSector  = 17 // El Torito Boot Record Volume Descriptor
	isoTermSector     = 18 // ISO9660 Volume Descriptor Set Terminator
	udfVRSSector      = 19 // UDF BEA01/NSR02/TEA01 at 19/20/21
	bootCatalogSector = 22
	pathTableLSector  = 23
	pathTableMSector  = 24
	rootDirSector     = 25
	pathTableSize     = 10 // one record for the root directory
)

// bootImage is one El Torito entry resolved to its on-disk location.
type bootImage struct {
	platform Platform
	sector   uint64
	length   int64
}

// Platform is the El Torito platform id of a boot entry (reused from iso.go's
// Firmware via the helpers below).

// BuildWindowsUDF masters a bootable UDF + El Torito image from an extracted
// media root. File content is stored in UDF (no ISO9660 4 GiB-per-file limit);
// a minimal ISO9660 descriptor set plus the El Torito boot catalog make it boot
// on BIOS and UEFI. The boot images are located within the UDF file system.
func BuildWindowsUDF(mediaRoot, outPath, volumeID string) error {
	index, err := indexTree(mediaRoot)
	if err != nil {
		return err
	}
	biosRel, hasBIOS := index[winBIOSBoot]
	uefiRel, hasUEFI := index[winUEFIBoot]
	if !hasBIOS && !hasUEFI {
		return fmt.Errorf("iso: no Windows boot images under %s", mediaRoot)
	}

	// ARM64 media is UEFI-only: its boot/etfsboot.com is an x86 BIOS boot sector
	// that cannot run on AARCH64, and Microsoft's ARM64 ISOs omit the BIOS El
	// Torito entry. Including it makes the UEFI image a section behind a BIOS
	// default entry (validation platform 0x00) and makes the firmware log
	// "Image type X64 can't be loaded on AARCH64 UEFI system". When the ARM64
	// UEFI loader is present, drop the BIOS entry so the UEFI image becomes the
	// validation/default entry (platform 0xEF), matching Microsoft's media.
	if _, isARM64 := index[winUEFIBootARM64]; isARM64 {
		hasBIOS = false
	}

	out, err := os.Create(outPath) //nolint:gosec // caller-provided path
	if err != nil {
		return fmt.Errorf("iso: create %s: %w", outPath, err)
	}
	defer out.Close()

	res, err := udf.WriteTree(out, mediaRoot, udf.Options{VolumeID: volumeID, VRSStart: udfVRSSector})
	if err != nil {
		return err
	}

	var images []bootImage
	if hasBIOS {
		loc := res.Files[biosRel]
		images = append(images, bootImage{platformBIOS, loc.Sector, loc.Length})
	}
	if hasUEFI {
		loc := res.Files[uefiRel]
		images = append(images, bootImage{platformUEFI, loc.Sector, loc.Length})
	}

	return writeISOBridge(out, volumeID, uint32(res.TotalSectors), images)
}

const (
	platformBIOS Platform = 0x00
	platformUEFI Platform = 0xEF
)

// Platform is the El Torito platform identifier.
type Platform = uint8

func writeISOBridge(out *os.File, volumeID string, totalSectors uint32, images []bootImage) error {
	writeSector := func(sector uint64, data []byte) error {
		if _, err := out.WriteAt(data, int64(sector*isoBlockSize)); err != nil {
			return fmt.Errorf("iso: write sector %d: %w", sector, err)
		}
		return nil
	}

	catalog, err := bootCatalog(images)
	if err != nil {
		return err
	}

	for _, s := range []struct {
		sector uint64
		data   []byte
	}{
		{isoPVDSector, primaryVolumeDescriptor(volumeID, totalSectors)},
		{isoBootRecSector, bootRecordDescriptor()},
		{isoTermSector, terminatorDescriptor()},
		{bootCatalogSector, catalog},
		{pathTableLSector, pathTable(binary.LittleEndian)},
		{pathTableMSector, pathTable(binary.BigEndian)},
		{rootDirSector, rootDirectoryExtent()},
	} {
		if err := writeSector(s.sector, s.data); err != nil {
			return err
		}
	}
	return nil
}

func both16(v uint16) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint16(b[0:], v)
	binary.BigEndian.PutUint16(b[2:], v)
	return b
}

func both32(v uint32) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint32(b[0:], v)
	binary.BigEndian.PutUint32(b[4:], v)
	return b
}

// isoDate7 encodes the 7-byte ISO9660 directory recording date.
func isoDate7(t time.Time) []byte {
	t = t.UTC()
	return []byte{byte(t.Year() - 1900), byte(t.Month()), byte(t.Day()),
		byte(t.Hour()), byte(t.Minute()), byte(t.Second()), 0}
}

func primaryVolumeDescriptor(volumeID string, totalSectors uint32) []byte {
	b := make([]byte, isoBlockSize)
	b[0] = 1 // primary volume descriptor
	copy(b[1:6], "CD001")
	b[6] = 1
	for i := 8; i < 72; i++ {
		b[i] = ' ' // system + volume identifier areas default to spaces
	}
	putStrPad(b[40:72], strings.ToUpper(volumeID))
	copy(b[80:88], both32(totalSectors))
	copy(b[120:124], both16(1))            // volume set size
	copy(b[124:128], both16(1))            // volume sequence number
	copy(b[128:132], both16(isoBlockSize)) // logical block size
	copy(b[132:140], both32(pathTableSize))
	binary.LittleEndian.PutUint32(b[140:], pathTableLSector)
	binary.BigEndian.PutUint32(b[148:], pathTableMSector)
	copy(b[156:190], rootDirRecord(0))
	b[881] = 1 // file structure version
	return b
}

// rootDirRecord builds a 34-byte directory record for the root, with the given
// file identifier byte (0 = ".", 1 = "..").
func rootDirRecord(ident byte) []byte {
	r := make([]byte, 34)
	r[0] = 34
	copy(r[2:10], both32(rootDirSector))
	copy(r[10:18], both32(isoBlockSize))
	copy(r[18:25], isoDate7(time.Now()))
	r[25] = 0x02 // directory
	copy(r[28:32], both16(1))
	r[32] = 1 // length of file identifier
	r[33] = ident
	return r
}

func rootDirectoryExtent() []byte {
	b := make([]byte, isoBlockSize)
	self := rootDirRecord(0)
	parent := rootDirRecord(1)
	copy(b[0:], self)
	copy(b[len(self):], parent)
	return b
}

func pathTable(order binary.ByteOrder) []byte {
	b := make([]byte, isoBlockSize)
	b[0] = 1 // length of directory identifier (root)
	b[1] = 0 // extended attribute length
	order.PutUint32(b[2:], rootDirSector)
	order.PutUint16(b[6:], 1) // parent directory number
	b[8] = 0                  // directory identifier (root)
	return b
}

func bootRecordDescriptor() []byte {
	b := make([]byte, isoBlockSize)
	b[0] = 0 // boot record
	copy(b[1:6], "CD001")
	b[6] = 1
	copy(b[7:39], "EL TORITO SPECIFICATION")
	binary.LittleEndian.PutUint32(b[71:], bootCatalogSector)
	return b
}

func terminatorDescriptor() []byte {
	b := make([]byte, isoBlockSize)
	b[0] = 255
	copy(b[1:6], "CD001")
	b[6] = 1
	return b
}

// bootCatalog builds the El Torito boot catalog: a validation entry, a default
// (initial) entry for the first image, and a section header+entry per remaining
// image.
func bootCatalog(images []bootImage) ([]byte, error) {
	b := make([]byte, isoBlockSize)

	// Validation entry.
	b[0] = 0x01
	b[1] = images[0].platform
	b[30] = 0x55
	b[31] = 0xAA
	// Checksum word so all 16 words of the entry sum to zero.
	var sum uint16
	for i := 0; i < 32; i += 2 {
		sum += binary.LittleEndian.Uint16(b[i:])
	}
	binary.LittleEndian.PutUint16(b[28:], -sum)

	// Default entry (first image).
	if err := putBootEntry(b[32:64], images[0]); err != nil {
		return nil, err
	}

	// Additional images as section headers + entries.
	off := 64
	for i := 1; i < len(images); i++ {
		final := byte(0x90)
		if i == len(images)-1 {
			final = 0x91
		}
		b[off] = final
		b[off+1] = images[i].platform
		binary.LittleEndian.PutUint16(b[off+2:], 1) // one entry follows
		if err := putBootEntry(b[off+32:off+64], images[i]); err != nil {
			return nil, err
		}
		off += 64
	}
	return b, nil
}

// putBootEntry writes one 32-byte El Torito section entry for img.
//
// The "sector count" is the number of 512-byte virtual sectors the firmware
// loads. For a BIOS no-emulation entry, Microsoft/oscdimg record a fixed load
// size (biosBootLoadSize = 8) for etfsboot.com regardless of the file's true
// length; deriving it from the file length instead (the previous behaviour)
// under-loads the boot sector. For UEFI the firmware reads the whole efisys.bin
// FAT image, so the count must cover it. The field is 16-bit, so guard against
// an image whose sector count would overflow it.
func putBootEntry(e []byte, img bootImage) error {
	e[0] = 0x88 // bootable
	e[1] = 0x00 // no emulation
	var sectors uint32
	if img.platform == platformBIOS {
		sectors = biosBootLoadSize
	} else {
		sectors = uint32((img.length + 511) / 512)
	}
	if sectors > 0xFFFF {
		return fmt.Errorf("iso: boot image sector count %d exceeds the El Torito 16-bit limit (image too large)", sectors)
	}
	binary.LittleEndian.PutUint16(e[6:], uint16(sectors))
	binary.LittleEndian.PutUint32(e[8:], uint32(img.sector))
	return nil
}

// putStrPad copies s into dst, space-padding the remainder.
func putStrPad(dst []byte, s string) {
	n := copy(dst, s)
	for i := n; i < len(dst); i++ {
		dst[i] = ' '
	}
}
