package isoinspect

import (
	"encoding/binary"
	"fmt"
)

// maxShortADExtent is the largest byte length a single short allocation
// descriptor may legally record: 30-bit length, block-aligned, below 1 GiB.
const maxShortADExtent = 0x3FFFF800

// UDF descriptor tag identifiers used here.
const (
	tagPrimaryVolume   = 0x0001
	tagAnchor          = 0x0002
	tagImplUseVol      = 0x0004
	tagPartition       = 0x0005
	tagLogicalVolume   = 0x0006
	tagFileSet         = 0x0100
	tagFileIdentifier  = 0x0101
	tagFileEntry       = 0x0105
	tagExtendedFileEnt = 0x010A
)

// UDFFile is a regular file discovered in the UDF tree.
type UDFFile struct {
	Path    string
	Size    int64
	Extents int // number of allocation descriptors describing the data
	// FirstExtentBlock is the partition-relative block of the first data
	// extent (meaningful when Extents > 0; embedded files have none).
	FirstExtentBlock uint32
}

// UDFInfo describes a parsed UDF file system.
type UDFInfo struct {
	Present         bool
	UDFRevision     uint16 // e.g. 0x0102 for UDF 1.02
	VolumeID        string
	PartitionStart  uint32 // absolute sector of partition logical block 0
	PartitionBlocks uint32
	RootFEBlock     uint32 // partition-relative block of the root File Entry
	Files           []UDFFile
	FileCount       int
	DirCount        int
}

const (
	udfMaxNodes = 20000
	udfMaxDepth = 24
)

func inspectUDF(v *volume, r *Report) *UDFInfo {
	info := &UDFInfo{}
	le := binary.LittleEndian

	// Anchor Volume Descriptor Pointer at sector 256.
	anchor, err := v.sector(256)
	if err != nil || le.Uint16(anchor[0:]) != tagAnchor {
		r.addError("udf", "no UDF Anchor Volume Descriptor Pointer at sector 256")
		return info
	}
	info.Present = true
	mainLen := le.Uint32(anchor[16:])
	mainLoc := le.Uint32(anchor[20:])

	// Walk the Main Volume Descriptor Sequence.
	var fsdBlock uint32
	var haveFSD, havePD, haveLVD bool
	for i := uint32(0); i < mainLen/sectorSize+1 && i < 32; i++ {
		d, err := v.sector(uint64(mainLoc + i))
		if err != nil {
			break
		}
		switch le.Uint16(d[0:]) {
		case tagPrimaryVolume:
			info.VolumeID = decodeDString(d[24:56])
		case tagPartition:
			info.PartitionStart = le.Uint32(d[188:])
			info.PartitionBlocks = le.Uint32(d[192:])
			havePD = true
		case tagLogicalVolume:
			info.UDFRevision = le.Uint16(d[216+24:]) // domain entity id suffix
			fsdBlock = le.Uint32(d[248+4:])          // LogicalVolumeContentsUse long_ad -> block
			haveFSD = true
			haveLVD = true
		case 8: // terminating descriptor
			i = 33
		}
	}
	if !havePD {
		r.addError("udf", "no UDF Partition Descriptor found")
		return info
	}
	if !haveLVD {
		r.addError("udf", "no UDF Logical Volume Descriptor found")
	}
	if info.UDFRevision != 0 && info.UDFRevision != 0x0102 {
		r.addWarning("udf", fmt.Sprintf("UDF revision is %#04x; Microsoft media uses 1.02 (0x0102)", info.UDFRevision))
	}

	if !haveFSD {
		return info
	}
	// File Set Descriptor -> root directory File Entry ICB.
	fsd, err := v.sector(uint64(info.PartitionStart) + uint64(fsdBlock))
	if err != nil || le.Uint16(fsd[0:]) != tagFileSet {
		r.addError("udf", "cannot read UDF File Set Descriptor")
		return info
	}
	info.RootFEBlock = le.Uint32(fsd[400+4:]) // root directory ICB long_ad -> block

	w := &udfWalker{v: v, r: r, info: info, partStart: info.PartitionStart}
	w.walk(info.RootFEBlock, "", 0)
	info.FileCount = len(info.Files)
	return info
}

type udfWalker struct {
	v         *volume
	r         *Report
	info      *UDFInfo
	partStart uint32
	nodes     int
}

// adExtent is one parsed allocation descriptor.
type adExtent struct {
	length uint32
	typ    uint8
	block  uint32
}

// readFE reads and minimally parses a File Entry at the given partition-relative
// block, returning its file type, information length, allocation descriptors,
// and whether its data is embedded in the FE.
func (w *udfWalker) readFE(feBlock uint32) (fileType uint8, infoLen uint64, exts []adExtent, embedded bool, ok bool) {
	le := binary.LittleEndian
	fe, err := w.v.sector(uint64(w.partStart) + uint64(feBlock))
	if err != nil {
		return 0, 0, nil, false, false
	}
	tag := le.Uint16(fe[0:])
	if tag != tagFileEntry && tag != tagExtendedFileEnt {
		return 0, 0, nil, false, false
	}
	fileType = fe[16+11]
	infoLen = le.Uint64(fe[56:])
	lEA := int(le.Uint32(fe[168:]))
	lAD := int(le.Uint32(fe[172:]))
	adType := le.Uint16(fe[16+18:]) & 0x07
	if adType == 3 {
		return fileType, infoLen, nil, true, true
	}
	start := 176 + lEA
	step := 8
	if adType == 1 {
		step = 16 // long_ad
	}
	for off := start; off+step <= start+lAD && off+step <= len(fe); off += step {
		raw := le.Uint32(fe[off:])
		exts = append(exts, adExtent{
			length: raw & 0x3FFFFFFF,
			typ:    uint8(raw >> 30),
			block:  le.Uint32(fe[off+4:]),
		})
	}
	return fileType, infoLen, exts, false, true
}

func (w *udfWalker) walk(feBlock uint32, path string, depth int) {
	if depth > udfMaxDepth || w.nodes > udfMaxNodes {
		return
	}
	w.nodes++

	fileType, infoLen, exts, embedded, ok := w.readFE(feBlock)
	if !ok {
		w.r.add(SeverityError, "udf", path, "unreadable or non-File-Entry ICB")
		return
	}

	if fileType == 4 { // directory
		w.info.DirCount++
		w.walkDir(exts, embedded, path, depth)
		return
	}

	// Regular file: validate its allocation descriptors.
	w.validateFileExtents(path, infoLen, exts, embedded)
	f := UDFFile{Path: path, Size: int64(infoLen), Extents: len(exts)}
	if len(exts) > 0 {
		f.FirstExtentBlock = exts[0].block
	}
	w.info.Files = append(w.info.Files, f)
}

// validateFileExtents is the headline check: the allocation descriptors of a
// regular file must cover its exact length with type-0 extents, none exceeding
// the short_ad limit. A single oversized descriptor is the defect that makes a
// >1 GiB boot.wim/install.wim unreadable by the Windows boot manager.
func (w *udfWalker) validateFileExtents(path string, infoLen uint64, exts []adExtent, embedded bool) {
	if embedded || infoLen == 0 {
		return
	}
	if len(exts) == 0 {
		w.r.add(SeverityError, "udf", path, "file has no allocation descriptors")
		return
	}

	var sum uint64
	for i, e := range exts {
		if e.typ != 0 {
			w.r.add(SeverityError, "udf", path,
				fmt.Sprintf("allocation descriptor %d has non-zero extent type %d (length overflowed into the type bits?)", i, e.typ))
		}
		if uint64(e.length) > maxShortADExtent {
			w.r.add(SeverityError, "udf", path,
				fmt.Sprintf("allocation descriptor %d length %d exceeds the %d-byte short_ad limit", i, e.length, maxShortADExtent))
		}
		sum += uint64(e.length)
	}

	if sum != infoLen {
		w.r.add(SeverityError, "udf", path,
			fmt.Sprintf("information length %d does not match the sum of extent lengths %d (truncated or overflowed)", infoLen, sum))
	}

	wantExtents := (infoLen + maxShortADExtent - 1) / maxShortADExtent
	if uint64(len(exts)) < wantExtents {
		w.r.add(SeverityError, "udf", path,
			fmt.Sprintf("file is %d bytes but has only %d allocation descriptor(s); needs at least %d (a single short_ad cannot exceed ~1 GiB)",
				infoLen, len(exts), wantExtents))
	}
}

// walkDir reads a directory's File Identifier Descriptors and recurses.
func (w *udfWalker) walkDir(exts []adExtent, embedded bool, path string, depth int) {
	if embedded {
		// Embedded directory data is uncommon from this writer; skip traversal.
		return
	}
	var data []byte
	for _, e := range exts {
		if e.typ != 0 || e.length == 0 {
			continue
		}
		chunk, err := w.v.read(int64(w.partStart+e.block)*sectorSize, int(e.length))
		if err != nil {
			break
		}
		data = append(data, chunk...)
	}

	le := binary.LittleEndian
	for off := 0; off+38 <= len(data); {
		if le.Uint16(data[off:]) != tagFileIdentifier {
			// FIDs are block-aligned; jump to the next block.
			next := (off/sectorSize + 1) * sectorSize
			if next <= off {
				break
			}
			off = next
			continue
		}
		chars := data[off+18]
		lFI := int(data[off+19])
		childBlock := le.Uint32(data[off+24:]) // ICB long_ad -> block
		lIU := int(le.Uint16(data[off+36:]))
		total := 38 + lIU + lFI
		total = (total + 3) / 4 * 4 // pad to 4 bytes

		isParent := chars&0x08 != 0
		if !isParent && lFI > 0 && off+38+lIU+lFI <= len(data) {
			name := decodeUDFName(data[off+38+lIU : off+38+lIU+lFI])
			childPath := name
			if path != "" {
				childPath = path + "/" + name
			}
			w.walk(childBlock, childPath, depth+1)
		}

		if total <= 0 {
			break
		}
		off += total
	}
}

// decodeDString decodes an OSTA CS0 d-string occupying the whole field (the
// final byte is the used length).
func decodeDString(b []byte) string {
	if len(b) < 2 {
		return ""
	}
	used := int(b[len(b)-1])
	if used == 0 || used > len(b) {
		return ""
	}
	return decodeUDFName(b[:used])
}

// decodeUDFName decodes an OSTA CS0 compressed-unicode identifier (leading byte
// 8 = 8-bit, 16 = 16-bit big-endian).
func decodeUDFName(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	switch b[0] {
	case 8:
		return string(b[1:])
	case 16:
		var out []rune
		for i := 1; i+1 < len(b); i += 2 {
			out = append(out, rune(binary.BigEndian.Uint16(b[i:])))
		}
		return string(out)
	default:
		return string(b)
	}
}
