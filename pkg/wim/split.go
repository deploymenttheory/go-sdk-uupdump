package wim

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Split splits the WIM at srcPath into a spanned set of ".swm" parts, each no
// larger than maxPartBytes where possible, and returns the written part paths.
// The first part is "<baseName>.swm" and subsequent parts are
// "<baseName>2.swm", "<baseName>3.swm", … (the naming Windows Setup expects for
// an install.swm set).
//
// It is the pure-Go analogue of wimlib_split. Following that on-disk contract:
// every image's metadata resource lives in the first part; file blobs are
// bin-packed across parts in sequential order; each part is a standalone WIM
// with the spanned flag, a shared GUID, part number k of N, the full XML
// catalog, and a blob table describing only that part's resources (part_number
// == k). Readers (Join here, or Windows Setup) merge the per-part tables.
//
// A single blob larger than maxPartBytes occupies its own oversized part —
// individual resources cannot be divided. Splitting a WIM that contains solid
// resources is unsupported; export it non-solid first.
func Split(srcPath, outDir, baseName string, maxPartBytes int64) ([]string, error) {
	if maxPartBytes <= 0 {
		return nil, fmt.Errorf("wim: split part size must be positive")
	}
	w, err := Open(srcPath)
	if err != nil {
		return nil, err
	}
	defer w.Close()

	meta, blobs, err := w.splitItems()
	if err != nil {
		return nil, err
	}
	if len(meta) == 0 {
		return nil, fmt.Errorf("wim: cannot split a WIM with no images")
	}
	xmlBytes, err := w.readResourceRaw(w.hdr.XMLData)
	if err != nil {
		return nil, fmt.Errorf("wim: read XML: %w", err)
	}

	parts := packParts(meta, blobs, maxPartBytes)

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("wim: mkdir %s: %w", outDir, err)
	}
	names := make([]string, len(parts))
	for k := range parts {
		names[k] = filepath.Join(outDir, partFileName(baseName, k+1))
	}
	for k, items := range parts {
		if err := w.writePart(names[k], k+1, len(parts), items, xmlBytes); err != nil {
			return nil, err
		}
	}
	return names, nil
}

// partFileName returns the file name for 1-based part number n:
// "<base>.swm" for part 1, "<base>N.swm" otherwise.
func partFileName(base string, n int) string {
	if n == 1 {
		return base + ".swm"
	}
	return fmt.Sprintf("%s%d.swm", base, n)
}

// splitItem is one resource to place into a part: its on-disk descriptor, its
// content hash, and whether it is an image metadata resource.
type splitItem struct {
	rd     resourceDescriptor
	hash   [20]byte
	isMeta bool
}

// splitItems reads the offset table and returns the metadata resources (in image
// order) and the file blobs (sorted by on-disk offset — sequential order, so a
// part's copied bytes stay in ascending source order). It errors if the WIM
// contains solid resources.
func (w *WIM) splitItems() (meta, blobs []splitItem, err error) {
	raw, err := w.readResourceRaw(w.hdr.OffsetTable)
	if err != nil {
		return nil, nil, fmt.Errorf("wim: read offset table: %w", err)
	}
	n := len(raw) / blobTableEntrySize
	for i := 0; i < n; i++ {
		rd, hash := parseBlobEntry(raw, i)
		switch {
		case rd.Flags&resFlagSolid != 0:
			return nil, nil, fmt.Errorf("wim: cannot split a WIM with solid resources; export it non-solid first")
		case rd.Flags&resFlagMetadata != 0:
			meta = append(meta, splitItem{rd: rd, hash: hash, isMeta: true})
		default:
			if !isZeroHash(hash) {
				blobs = append(blobs, splitItem{rd: rd, hash: hash})
			}
		}
	}
	sort.SliceStable(blobs, func(i, j int) bool { return blobs[i].rd.Offset < blobs[j].rd.Offset })
	return meta, blobs, nil
}

// packParts bin-packs metadata (all into the first part) and blobs across parts,
// starting a new part when adding a blob would reach maxPartBytes and the
// current part already holds data. Mirrors wimlib's add_blob_to_swm.
func packParts(meta, blobs []splitItem, maxPartBytes int64) [][]splitItem {
	var parts [][]splitItem
	cur := append([]splitItem{}, meta...)
	var curSize int64
	for _, m := range meta {
		curSize += m.rd.CompressedSize
	}
	for _, b := range blobs {
		sz := b.rd.CompressedSize
		if curSize > 0 && curSize+sz >= maxPartBytes {
			parts = append(parts, cur)
			cur = nil
			curSize = 0
		}
		cur = append(cur, b)
		curSize += sz
	}
	parts = append(parts, cur)
	return parts
}

// writePart writes one spanned WIM part.
func (w *WIM) writePart(path string, partNum, totalParts int, items []splitItem, xmlBytes []byte) error {
	out, err := os.Create(path) //nolint:gosec // caller-provided path
	if err != nil {
		return fmt.Errorf("wim: create %s: %w", path, err)
	}
	defer out.Close()

	// Header placeholder.
	if _, err := out.Write(make([]byte, headerSize)); err != nil {
		return fmt.Errorf("wim: write %s: %w", path, err)
	}
	pos := int64(headerSize)

	// Copy each resource's raw (already-compressed) bytes verbatim, recording its
	// new offset for the blob table. Track the boot metadata resource's new
	// location so the first part's header can point at it.
	type tableEntry struct {
		hash                 [20]byte
		offset, csize, osize int64
		flags                byte
	}
	var entries []tableEntry
	var bootMeta resourceDescriptor
	for _, it := range items {
		raw, err := w.readResourceRaw(it.rd)
		if err != nil {
			return err
		}
		off := pos
		if _, err := out.WriteAt(raw, off); err != nil {
			return fmt.Errorf("wim: write %s: %w", path, err)
		}
		pos += int64(len(raw))
		entries = append(entries, tableEntry{it.hash, off, it.rd.CompressedSize, it.rd.OriginalSize, it.rd.Flags})
		if it.isMeta && w.hdr.BootMetadata.CompressedSize != 0 && it.rd.Offset == w.hdr.BootMetadata.Offset {
			bootMeta = resourceDescriptor{Offset: off, CompressedSize: it.rd.CompressedSize, OriginalSize: it.rd.OriginalSize, Flags: it.rd.Flags}
		}
	}

	// Blob table for this part: one entry per copied resource, part_number == k.
	tableOff := pos
	for _, e := range entries {
		if _, err := out.WriteAt(blobEntryPart(e.hash, e.offset, e.csize, e.osize, e.flags, uint16(partNum)), pos); err != nil {
			return fmt.Errorf("wim: write %s: %w", path, err)
		}
		pos += blobTableEntrySize
	}
	tableSize := pos - tableOff

	// XML catalog (identical in every part).
	xmlOff := pos
	if _, err := out.WriteAt(xmlBytes, pos); err != nil {
		return fmt.Errorf("wim: write %s: %w", path, err)
	}
	pos += int64(len(xmlBytes))

	h := header{
		Version:     w.hdr.Version,
		Flags:       w.hdr.Flags | flagSpanned,
		ChunkSize:   w.hdr.ChunkSize,
		GUID:        w.hdr.GUID,
		PartNumber:  uint16(partNum),
		TotalParts:  uint16(totalParts),
		ImageCount:  w.hdr.ImageCount,
		OffsetTable: resourceDescriptor{Offset: tableOff, CompressedSize: tableSize, OriginalSize: tableSize},
		XMLData:     resourceDescriptor{Offset: xmlOff, CompressedSize: int64(len(xmlBytes)), OriginalSize: int64(len(xmlBytes))},
	}
	// Boot metadata/index belong only to the first part.
	if partNum == 1 {
		h.BootMetadata = bootMeta
		h.BootIndex = w.hdr.BootIndex
	}
	if _, err := out.WriteAt(serializeHeader(h), 0); err != nil {
		return fmt.Errorf("wim: write header %s: %w", path, err)
	}
	return nil
}

// blobEntryPart encodes one 50-byte blob-table entry with an explicit part
// number (the streaming Writer's blobEntry hardcodes part 1).
func blobEntryPart(hash [20]byte, offset, csize, osize int64, flags byte, part uint16) []byte {
	e := make([]byte, blobTableEntrySize)
	le := binary.LittleEndian
	le.PutUint64(e[0:], uint64(csize)|uint64(flags)<<56)
	le.PutUint64(e[8:], uint64(offset))
	le.PutUint64(e[16:], uint64(osize))
	le.PutUint16(e[24:], part)
	le.PutUint32(e[26:], 1) // refcount
	copy(e[30:], hash[:])
	return e
}
