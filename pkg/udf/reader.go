package udf

import (
	"encoding/binary"
	"fmt"
	"io"
	"strings"
)

// This is a self-contained, deliberately STRICT UDF reader — the test trusted source for the
// writer. It has no third-party dependency and validates every descriptor the way
// ECMA-167 / Windows UDFS do (tag checksum + CRC + tag location, contiguously packed
// File Identifier Descriptors with no zero-byte gaps, correct partition/File-Set
// linkage). A lenient third-party reader previously masked a zero-gap FID defect that
// Windows rejects with "the disk structure is corrupted and unreadable", so the
// writer's output is checked against this reader instead, giving confidence a
// dependency is not hiding a real defect.

// Entry is one directory entry.
type Entry struct {
	Name  string
	IsDir bool
	Size  int64
	block uint32 // File Entry, partition-relative logical block
}

// Volume is a parsed + validated UDF volume. Open it with Read.
type Volume struct {
	r         io.ReaderAt
	partStart uint32 // absolute sector of the partition's logical block 0
	root      Entry
}

// Read parses and validates the UDF filesystem in r: the Anchor Volume Descriptor
// Pointer at sector 256, the Main Volume Descriptor Sequence (Partition + Logical
// Volume descriptors), and the File Set Descriptor's root directory ICB. It returns an
// error if any descriptor's tag checksum/CRC/location is wrong, the logical block size
// is not 2048, or a required structure is missing or unreachable — i.e. the same
// reasons a strict OS rejects the volume.
func Read(r io.ReaderAt) (*Volume, error) {
	le := binary.LittleEndian

	// Anchor Volume Descriptor Pointer at logical sector 256.
	avdp, err := readDesc(r, 256, tagAnchorVolumePointer, 256)
	if err != nil {
		return nil, fmt.Errorf("anchor volume descriptor pointer: %w", err)
	}
	mvdsLen := le.Uint32(avdp[16:])
	mvdsLoc := le.Uint32(avdp[20:])
	if mvdsLen == 0 {
		return nil, fmt.Errorf("anchor: empty main volume descriptor sequence")
	}

	// Walk the Main Volume Descriptor Sequence for the Partition + Logical Volume descriptors.
	var partStart, fsdBlock uint32
	var haveP, haveL bool
	for i := uint32(0); i < mvdsLen/SectorSize; i++ {
		sec := mvdsLoc + i
		raw := make([]byte, SectorSize)
		if _, err := r.ReadAt(raw, int64(sec)*SectorSize); err != nil {
			return nil, fmt.Errorf("read MVDS sector %d: %w", sec, err)
		}
		ident := le.Uint16(raw[0:])
		if ident == 0 || ident == tagTerminating {
			break
		}
		if err := validateTag(raw, ident, sec); err != nil {
			return nil, fmt.Errorf("MVDS sector %d: %w", sec, err)
		}
		switch ident {
		case tagPartition:
			partStart = le.Uint32(raw[188:])
			haveP = true
		case tagLogicalVolume:
			if bs := le.Uint32(raw[212:]); bs != SectorSize {
				return nil, fmt.Errorf("logical block size %d, want %d", bs, SectorSize)
			}
			if n := le.Uint32(raw[268:]); n != 1 {
				return nil, fmt.Errorf("number of partition maps %d, want 1", n)
			}
			// LogicalVolumeContentsUse @248 is a long_ad to the File Set Descriptor;
			// its extent location (block within the partition) is at +4.
			fsdBlock = le.Uint32(raw[248+4:])
			haveL = true
		}
	}
	if !haveP || !haveL {
		return nil, fmt.Errorf("missing Partition (found=%v) or Logical Volume (found=%v) descriptor", haveP, haveL)
	}

	// File Set Descriptor at partition-relative block fsdBlock; RootDirICB @400 (long_ad).
	fsd, err := readDesc(r, int64(partStart+fsdBlock), tagFileSet, fsdBlock)
	if err != nil {
		return nil, fmt.Errorf("file set descriptor: %w", err)
	}
	rootBlock := le.Uint32(fsd[400+4:])

	return &Volume{r: r, partStart: partStart, root: Entry{IsDir: true, block: rootBlock}}, nil
}

// ReadDir returns the entries of the directory at the given slash-separated path
// (nil/empty = root), validating each File Entry and File Identifier Descriptor.
func (v *Volume) ReadDir(parts []string) ([]Entry, error) {
	cur := v.root
	for _, part := range parts {
		entries, err := v.readDirEntries(cur)
		if err != nil {
			return nil, err
		}
		var next *Entry
		for i := range entries {
			if entries[i].Name == part {
				next = &entries[i]
				break
			}
		}
		if next == nil {
			return nil, fmt.Errorf("path component %q not found", part)
		}
		cur = *next
	}
	return v.readDirEntries(cur)
}

// ReadFile returns the full content of the file at the slash-separated path.
func (v *Volume) ReadFile(parts []string) ([]byte, error) {
	if len(parts) == 0 {
		return nil, fmt.Errorf("empty path")
	}
	dir, err := v.ReadDir(parts[:len(parts)-1])
	if err != nil {
		return nil, err
	}
	name := parts[len(parts)-1]
	for i := range dir {
		if dir[i].Name == name {
			if dir[i].IsDir {
				return nil, fmt.Errorf("%q is a directory", name)
			}
			return v.readFileData(dir[i].block)
		}
	}
	return nil, fmt.Errorf("file %q not found", name)
}

// readDirEntries reads a directory's File Entry, its FID list, and returns children.
// Each child's File Entry is also read and validated, and its information length
// recorded as Entry.Size.
func (v *Volume) readDirEntries(dir Entry) ([]Entry, error) {
	data, _, err := v.readEntryData(dir.block, tagFileEntry)
	if err != nil {
		return nil, err
	}
	entries, err := parseFIDs(data)
	if err != nil {
		return nil, err
	}
	for i := range entries {
		fe, err := readDesc(v.r, int64(v.partStart+entries[i].block), tagFileEntry, entries[i].block)
		if err != nil {
			return nil, fmt.Errorf("%s: file entry: %w", entries[i].Name, err)
		}
		entries[i].Size = int64(binary.LittleEndian.Uint64(fe[56:]))
	}
	return entries, nil
}

// readFileData reads a regular file's content via its File Entry allocation descriptors.
func (v *Volume) readFileData(feBlock uint32) ([]byte, error) {
	data, infoLen, err := v.readEntryData(feBlock, tagFileEntry)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > infoLen {
		data = data[:infoLen]
	}
	return data, nil
}

// readEntryData reads + validates a File Entry at partition-relative block and returns
// the concatenated bytes of its allocation extents (or its embedded data) and the info
// length. Only short_ad (alloc type 0) and embedded (type 3) are produced by the writer.
func (v *Volume) readEntryData(feBlock uint32, wantTag uint16) ([]byte, int64, error) {
	le := binary.LittleEndian
	fe, err := readDesc(v.r, int64(v.partStart+feBlock), wantTag, feBlock)
	if err != nil {
		return nil, 0, err
	}
	infoLen := int64(le.Uint64(fe[56:]))
	allocType := le.Uint16(fe[16+18:]) & 0x7 // ICB tag flags @ (16+18)
	lEA := le.Uint32(fe[168:])
	lAD := le.Uint32(fe[172:])
	adStart := 176 + int(lEA)
	if adStart < 0 || adStart > len(fe) {
		return nil, 0, fmt.Errorf("file entry: bad extended-attribute length %d", lEA)
	}
	switch allocType {
	case 3: // embedded in the File Entry
		end := adStart + int(infoLen)
		if end > len(fe) {
			return nil, 0, fmt.Errorf("file entry: embedded length %d overruns descriptor", infoLen)
		}
		return fe[adStart:end], infoLen, nil
	case 0: // short_ad list
		var out []byte
		for off := adStart; off+8 <= adStart+int(lAD) && off+8 <= len(fe); off += 8 {
			raw := le.Uint32(fe[off:])
			length := raw & 0x3FFFFFFF
			etype := raw >> 30
			block := le.Uint32(fe[off+4:])
			if etype != 0 || length == 0 {
				continue
			}
			buf := make([]byte, length)
			if _, err := v.r.ReadAt(buf, int64(v.partStart+block)*SectorSize); err != nil && err != io.EOF {
				return nil, 0, fmt.Errorf("read extent block %d: %w", block, err)
			}
			out = append(out, buf...)
		}
		return out, infoLen, nil
	default:
		return nil, 0, fmt.Errorf("file entry: unsupported allocation type %d", allocType)
	}
}

// parseFIDs walks a directory's File Identifier Descriptor stream. FIDs are packed
// contiguously and MAY cross logical-block boundaries (real Windows media does this);
// the strict invariants are that each descriptor is a valid FileIdentifier tag (a
// zero-byte gap fails this — an invalid tag identifier — which is the defect strict
// readers stop at) with a correct checksum + CRC. It skips the parent ("") entry.
func parseFIDs(data []byte) ([]Entry, error) {
	le := binary.LittleEndian
	var out []Entry
	for off := 0; off+38 <= len(data); {
		if ident := le.Uint16(data[off:]); ident != tagFileIdentifier {
			return nil, fmt.Errorf("directory: descriptor at %d has tag %#x, want FileIdentifier %#x (zero gap / corruption)", off, ident, tagFileIdentifier)
		}
		lFI := int(data[off+19])
		lIU := int(le.Uint16(data[off+36:]))
		flen := (38 + lIU + lFI + 3) / 4 * 4
		if off+flen > len(data) {
			return nil, fmt.Errorf("directory: FID at %d overruns directory data (len %d)", off, flen)
		}
		if err := checkTagIntegrity(data[off : off+flen]); err != nil {
			return nil, fmt.Errorf("directory: FID at %d: %w", off, err)
		}
		chars := data[off+18]
		icbBlock := le.Uint32(data[off+20+4:]) // ICB long_ad @20, extent location at +4
		name := decodeDString(data[off+38+lIU : off+38+lIU+lFI])
		if chars&0x08 == 0 { // not the parent entry
			out = append(out, Entry{Name: name, IsDir: chars&0x02 != 0, block: icbBlock})
		}
		off += flen
	}
	return out, nil
}

// readDesc reads a 2048-byte descriptor at the given absolute sector and validates its
// tag against wantIdent + wantLoc.
func readDesc(r io.ReaderAt, sector int64, wantIdent uint16, wantLoc uint32) ([]byte, error) {
	raw := make([]byte, SectorSize)
	if _, err := r.ReadAt(raw, sector*SectorSize); err != nil {
		return nil, fmt.Errorf("read sector %d: %w", sector, err)
	}
	if err := validateTag(raw, wantIdent, wantLoc); err != nil {
		return nil, err
	}
	return raw, nil
}

// validateTag checks the ECMA-167 descriptor tag: identifier, checksum (sum of tag
// bytes 0..15 except byte 4, mod 256), CRC (CRC-CCITT over the CRC-length bytes after
// the 16-byte tag), and tag location.
func validateTag(raw []byte, wantIdent uint16, wantLoc uint32) error {
	le := binary.LittleEndian
	if id := le.Uint16(raw[0:]); id != wantIdent {
		return fmt.Errorf("tag identifier %#x, want %#x", id, wantIdent)
	}
	if err := checkTagIntegrity(raw); err != nil {
		return err
	}
	if loc := le.Uint32(raw[12:]); loc != wantLoc {
		return fmt.Errorf("tag location %d, want %d", loc, wantLoc)
	}
	return nil
}

// checkTagIntegrity validates the ECMA-167 tag checksum (sum of tag bytes 0..15 except
// byte 4, mod 256) and the descriptor CRC (CRC-CCITT over the CRC-length bytes after
// the 16-byte tag) of the descriptor at the start of raw.
func checkTagIntegrity(raw []byte) error {
	le := binary.LittleEndian
	var sum uint8
	for i := 0; i < 16; i++ {
		if i != 4 {
			sum += raw[i]
		}
	}
	if sum != raw[4] {
		return fmt.Errorf("tag checksum %#02x, want %#02x", raw[4], sum)
	}
	crcLen := le.Uint16(raw[10:])
	if 16+int(crcLen) > len(raw) {
		return fmt.Errorf("tag CRC length %d overruns descriptor", crcLen)
	}
	if got, want := le.Uint16(raw[8:]), crcCCITT(raw[16:16+crcLen]); got != want {
		return fmt.Errorf("tag CRC %#04x, want %#04x", got, want)
	}
	return nil
}

// decodeDString decodes an OSTA CS0 d-string: a leading compression-ID byte (8 =
// 8-bit, 16 = 16-bit big-endian) followed by the characters.
func decodeDString(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	switch b[0] {
	case 16:
		var sb strings.Builder
		for i := 1; i+1 < len(b); i += 2 {
			sb.WriteRune(rune(uint16(b[i])<<8 | uint16(b[i+1])))
		}
		return sb.String()
	default: // 8-bit (compression ID 8) or anything else
		return string(b[1:])
	}
}
