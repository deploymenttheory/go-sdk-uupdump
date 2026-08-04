package isoinspect

import (
	"encoding/binary"
	"fmt"
	"os"
	"strings"
)

// RetargetElToritoUEFI rewrites the El Torito boot catalog of the ISO at
// isoPath in place so its UEFI/default entry boots the named file inside the
// image instead of the original boot image. imagePath is slash-separated and
// case-insensitive (e.g. "efi/microsoft/boot/efisys_noprompt.bin" — the
// no-prompt EFI System Partition image Microsoft ships on Windows media next
// to the prompting efisys.bin). The resulting catalog is UEFI-only, matching
// SetElToritoUEFIOnly's layout; the image's file data is untouched.
func RetargetElToritoUEFI(isoPath, imagePath string) error {
	rep, err := Inspect(isoPath)
	if err != nil {
		return err
	}
	et := rep.ElTorito
	if et == nil || !et.Present {
		return fmt.Errorf("isoinspect: %s has no El Torito boot catalog", isoPath)
	}

	f, err := os.OpenFile(isoPath, os.O_RDWR, 0) //nolint:gosec // caller-provided path
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	v := &volume{ra: f, size: st.Size()}

	// Resolve the image file: the ISO9660 tree when it carries the payload,
	// else the UDF tree (modern Windows media keeps everything in UDF and
	// leaves a near-empty ISO9660 root).
	extent, length, err := findISOFile(v, imagePath)
	if err != nil {
		var udfErr error
		extent, length, udfErr = findUDFFile(rep, imagePath)
		if udfErr != nil {
			return fmt.Errorf("%w; %w", err, udfErr)
		}
	}
	// The catalog's sector count is in 512-byte virtual sectors.
	sectors512 := (int64(length) + 511) / 512
	if sectors512 > 0xFFFF {
		return fmt.Errorf("isoinspect: %s is %d bytes — too large for an El Torito entry", imagePath, length)
	}

	cat := make([]byte, sectorSize)
	le := binary.LittleEndian

	// Validation entry: header 0x01, platform UEFI, 55AA key, zero-sum checksum.
	cat[0] = 0x01
	cat[1] = platformUEFI
	cat[30] = 0x55
	cat[31] = 0xAA
	var sum uint16
	for i := 0; i < 32; i += 2 {
		if i == 28 {
			continue
		}
		sum += le.Uint16(cat[i:])
	}
	le.PutUint16(cat[28:], -sum)

	// Default/initial entry: the requested image, no emulation.
	cat[32] = 0x88
	cat[33] = 0x00
	le.PutUint16(cat[32+6:], uint16(sectors512))
	le.PutUint32(cat[32+8:], extent)

	if _, err := f.WriteAt(cat, int64(et.CatalogSector)*sectorSize); err != nil {
		return fmt.Errorf("isoinspect: rewrite boot catalog: %w", err)
	}
	return nil
}

// findUDFFile resolves the file through an already-inspected UDF tree to its
// absolute sector and byte length.
func findUDFFile(rep *Report, path string) (extent, length uint32, err error) {
	if rep.UDF == nil || !rep.UDF.Present {
		return 0, 0, fmt.Errorf("isoinspect: no UDF file system")
	}
	for _, f := range rep.UDF.Files {
		if !strings.EqualFold(f.Path, path) {
			continue
		}
		if f.Extents != 1 {
			return 0, 0, fmt.Errorf("isoinspect: %s has %d extents — an El Torito image needs exactly one", path, f.Extents)
		}
		return rep.UDF.PartitionStart + f.FirstExtentBlock, uint32(f.Size), nil
	}
	return 0, 0, fmt.Errorf("isoinspect: %s not found in the UDF tree", path)
}

// findISOFile resolves a slash-separated, case-insensitive path through the
// ISO9660 directory tree to the file's extent sector and byte length.
func findISOFile(v *volume, path string) (extent, length uint32, err error) {
	pvd, err := v.sector(16)
	if err != nil || pvd[0] != 1 || string(pvd[1:6]) != "CD001" {
		return 0, 0, fmt.Errorf("isoinspect: no ISO9660 primary volume descriptor")
	}
	rootRec := pvd[156 : 156+34]
	extent = binary.LittleEndian.Uint32(rootRec[2:])
	length = binary.LittleEndian.Uint32(rootRec[10:])

	components := strings.Split(strings.Trim(path, "/"), "/")
	for i, comp := range components {
		wantDir := i < len(components)-1
		e, l, isDir, found := findDirEntry(v, extent, length, comp)
		if !found {
			return 0, 0, fmt.Errorf("isoinspect: %s not found in image (missing %q)", path, comp)
		}
		if isDir != wantDir {
			return 0, 0, fmt.Errorf("isoinspect: %s: %q is a %s", path, comp, map[bool]string{true: "directory", false: "file"}[isDir])
		}
		extent, length = e, l
	}
	return extent, length, nil
}

// findDirEntry scans one directory extent for a name (case-insensitive,
// version suffix ignored), returning its extent, length, and directory flag.
func findDirEntry(v *volume, dirExtent, dirLength uint32, name string) (extent, length uint32, isDir, found bool) {
	if dirLength == 0 {
		dirLength = sectorSize
	}
	data, err := v.read(int64(dirExtent)*sectorSize, int(dirLength))
	if err != nil {
		return 0, 0, false, false
	}
	for off := 0; off < len(data); {
		recLen := int(data[off])
		if recLen == 0 {
			next := (off/sectorSize + 1) * sectorSize
			if next <= off || next >= len(data) {
				break
			}
			off = next
			continue
		}
		if off+33 > len(data) || off+recLen > len(data) {
			break
		}
		nlen := int(data[off+32])
		if off+33+nlen > len(data) {
			break
		}
		n := string(data[off+33 : off+33+nlen])
		if i := strings.IndexByte(n, ';'); i >= 0 {
			n = n[:i]
		}
		if nlen > 1 || (nlen == 1 && n != "\x00" && n != "\x01") {
			if strings.EqualFold(n, name) {
				le := binary.LittleEndian
				return le.Uint32(data[off+2:]), le.Uint32(data[off+10:]), data[off+25]&0x02 != 0, true
			}
		}
		off += recLen
	}
	return 0, 0, false, false
}
