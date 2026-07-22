package wim

import (
	"fmt"
	"os"
	"sort"
)

// Join merges a spanned ".swm" set back into a single WIM at outPath. It is the
// inverse of Split (wimlib_join): it reads every part, unions their blob tables
// by content hash, and writes one standalone WIM with the image metadata
// resources (from the first part), all file blobs, the XML catalog, and a
// cleared spanned flag. Parts may be given in any order; they are ordered by
// header part number.
func Join(partPaths []string, outPath string) error {
	if len(partPaths) == 0 {
		return fmt.Errorf("wim: join needs at least one part")
	}

	type openPart struct {
		f   *os.File
		hdr header
	}
	var parts []openPart
	defer func() {
		for _, p := range parts {
			_ = p.f.Close()
		}
	}()

	for _, path := range partPaths {
		f, err := os.Open(path) //nolint:gosec // caller-provided path
		if err != nil {
			return fmt.Errorf("wim: open part %s: %w", path, err)
		}
		buf := make([]byte, headerSize)
		if _, err := f.ReadAt(buf, 0); err != nil {
			_ = f.Close()
			return fmt.Errorf("wim: read part header %s: %w", path, err)
		}
		hdr, err := parseHeader(buf)
		if err != nil {
			_ = f.Close()
			return err
		}
		parts = append(parts, openPart{f: f, hdr: hdr})
	}
	sort.SliceStable(parts, func(i, j int) bool { return parts[i].hdr.PartNumber < parts[j].hdr.PartNumber })

	first := parts[0]
	if first.hdr.PartNumber != 1 {
		return fmt.Errorf("wim: missing part 1 of the spanned set")
	}

	// Collect a resource copy plan: metadata (from part 1, in image order) then
	// unique blobs across all parts.
	type resItem struct {
		src  *os.File
		rd   resourceDescriptor
		hash [20]byte
	}
	var metaItems, blobItems []resItem
	seen := make(map[[20]byte]bool)
	for _, p := range parts {
		raw := make([]byte, p.hdr.OffsetTable.CompressedSize)
		if _, err := p.f.ReadAt(raw, p.hdr.OffsetTable.Offset); err != nil {
			return fmt.Errorf("wim: read part offset table: %w", err)
		}
		n := len(raw) / blobTableEntrySize
		for i := 0; i < n; i++ {
			rd, hash := parseBlobEntry(raw, i)
			switch {
			case rd.Flags&resFlagMetadata != 0:
				if p.hdr.PartNumber == 1 {
					metaItems = append(metaItems, resItem{p.f, rd, hash})
				}
			case rd.Flags&resFlagSolid != 0:
				return fmt.Errorf("wim: cannot join a set with solid resources")
			default:
				if !isZeroHash(hash) && !seen[hash] {
					seen[hash] = true
					blobItems = append(blobItems, resItem{p.f, rd, hash})
				}
			}
		}
	}

	out, err := os.Create(outPath) //nolint:gosec // caller-provided path
	if err != nil {
		return fmt.Errorf("wim: create %s: %w", outPath, err)
	}
	defer out.Close()

	if _, err := out.Write(make([]byte, headerSize)); err != nil {
		return fmt.Errorf("wim: write %s: %w", outPath, err)
	}
	pos := int64(headerSize)

	type tableEntry struct {
		hash                 [20]byte
		offset, csize, osize int64
		flags                byte
	}
	var entries []tableEntry
	var bootMeta resourceDescriptor

	copyRes := func(it resItem, isMeta bool) error {
		raw := make([]byte, it.rd.CompressedSize)
		if _, err := it.src.ReadAt(raw, it.rd.Offset); err != nil {
			return fmt.Errorf("wim: read resource: %w", err)
		}
		off := pos
		if _, err := out.WriteAt(raw, off); err != nil {
			return fmt.Errorf("wim: write %s: %w", outPath, err)
		}
		pos += int64(len(raw))
		entries = append(entries, tableEntry{it.hash, off, it.rd.CompressedSize, it.rd.OriginalSize, it.rd.Flags})
		if isMeta && first.hdr.BootMetadata.CompressedSize != 0 && it.rd.Offset == first.hdr.BootMetadata.Offset {
			bootMeta = resourceDescriptor{Offset: off, CompressedSize: it.rd.CompressedSize, OriginalSize: it.rd.OriginalSize, Flags: it.rd.Flags}
		}
		return nil
	}

	for _, it := range metaItems {
		if err := copyRes(it, true); err != nil {
			return err
		}
	}
	for _, it := range blobItems {
		if err := copyRes(it, false); err != nil {
			return err
		}
	}

	tableOff := pos
	for _, e := range entries {
		if _, err := out.WriteAt(blobEntryPart(e.hash, e.offset, e.csize, e.osize, e.flags, 1), pos); err != nil {
			return fmt.Errorf("wim: write %s: %w", outPath, err)
		}
		pos += blobTableEntrySize
	}
	tableSize := pos - tableOff

	xmlRaw := make([]byte, first.hdr.XMLData.CompressedSize)
	if _, err := first.f.ReadAt(xmlRaw, first.hdr.XMLData.Offset); err != nil {
		return fmt.Errorf("wim: read part 1 XML: %w", err)
	}
	xmlOff := pos
	if _, err := out.WriteAt(xmlRaw, pos); err != nil {
		return fmt.Errorf("wim: write %s: %w", outPath, err)
	}
	pos += int64(len(xmlRaw))

	h := header{
		Version:      first.hdr.Version,
		Flags:        first.hdr.Flags &^ flagSpanned, // no longer spanned
		ChunkSize:    first.hdr.ChunkSize,
		GUID:         first.hdr.GUID,
		PartNumber:   1,
		TotalParts:   1,
		ImageCount:   first.hdr.ImageCount,
		OffsetTable:  resourceDescriptor{Offset: tableOff, CompressedSize: tableSize, OriginalSize: tableSize},
		XMLData:      resourceDescriptor{Offset: xmlOff, CompressedSize: int64(len(xmlRaw)), OriginalSize: int64(len(xmlRaw))},
		BootMetadata: bootMeta,
		BootIndex:    first.hdr.BootIndex,
	}
	if _, err := out.WriteAt(serializeHeader(h), 0); err != nil {
		return fmt.Errorf("wim: write header %s: %w", outPath, err)
	}
	return nil
}
