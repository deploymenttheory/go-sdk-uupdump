package wim

import (
	"crypto/sha1" //nolint:gosec // WIM blobs are content-addressed by SHA-1
	"fmt"
	"io"
	"strings"
)

// commitWithFileOps applies pending AddFile/RemoveFile operations by appending to
// the WIM (wimlib_overwrite append mode): new blobs and the rebuilt metadata
// resources of modified images are written at end-of-file, then a fresh full
// blob table, the XML catalog, and a rewritten header. Existing resources stay
// in place; superseded metadata becomes unreferenced slack.
//
// Only non-solid WIMs are supported (as produced by this package's Writer);
// modifying a solid ESD is rejected.
func (u *Updater) commitWithFileOps() error {
	if err := u.w.loadOffsetTable(); err != nil {
		return err
	}
	blobEntries, metaEntries, err := u.parseFullTable()
	if err != nil {
		return err
	}
	if len(metaEntries) != len(u.recs) {
		return fmt.Errorf("wim: metadata/catalog image count mismatch (%d vs %d)", len(metaEntries), len(u.recs))
	}

	existing := make(map[[20]byte]bool, len(blobEntries))
	for _, b := range blobEntries {
		existing[b.hash] = true
	}

	pos, err := u.f.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("wim: seek end: %w", err)
	}

	// Rewrite the metadata for each modified image, appending new blobs and the
	// new metadata resource. Unmodified images keep their original entry.
	newMeta := make([]tableRec, len(metaEntries))
	copy(newMeta, metaEntries)
	for image, ops := range u.ops {
		root, err := u.w.OpenImage(image)
		if err != nil {
			return err
		}
		tree := fileToWriteNode(root)
		adds, err := applyFileOps(tree, ops)
		if err != nil {
			return err
		}
		// Append any genuinely new blob contents.
		for _, a := range adds {
			if existing[a.hash] {
				continue
			}
			existing[a.hash] = true
			if _, err := u.f.WriteAt(a.content, pos); err != nil {
				return fmt.Errorf("wim: write blob: %w", err)
			}
			blobEntries = append(blobEntries, tableRec{
				hash: a.hash, offset: pos,
				csize: int64(len(a.content)), osize: int64(len(a.content)),
			})
			pos += int64(len(a.content))
		}
		// Append the rebuilt metadata resource.
		meta := buildMetadata(tree)
		if _, err := u.f.WriteAt(meta, pos); err != nil {
			return fmt.Errorf("wim: write metadata: %w", err)
		}
		newMeta[image-1] = tableRec{
			hash:   sha1.Sum(meta), //nolint:gosec
			offset: pos, csize: int64(len(meta)), osize: int64(len(meta)),
			flags: resFlagMetadata,
		}
		pos += int64(len(meta))
	}

	// Append the full blob table: all file blobs, then all metadata entries.
	tableOff := pos
	writeEntry := func(r tableRec) error {
		if _, err := u.f.WriteAt(blobEntryPart(r.hash, r.offset, r.csize, r.osize, r.flags, 1), pos); err != nil {
			return fmt.Errorf("wim: write blob table: %w", err)
		}
		pos += blobTableEntrySize
		return nil
	}
	for _, b := range blobEntries {
		if err := writeEntry(b); err != nil {
			return err
		}
	}
	for _, m := range newMeta {
		if err := writeEntry(m); err != nil {
			return err
		}
	}
	tableSize := pos - tableOff

	// Append the XML catalog.
	xmlBytes := buildXML(u.recs)
	xmlOff := pos
	if _, err := u.f.WriteAt(xmlBytes, pos); err != nil {
		return fmt.Errorf("wim: write catalog: %w", err)
	}
	pos += int64(len(xmlBytes))

	u.hdr.OffsetTable = resourceDescriptor{Offset: tableOff, CompressedSize: tableSize, OriginalSize: tableSize}
	u.hdr.XMLData = resourceDescriptor{Offset: xmlOff, CompressedSize: int64(len(xmlBytes)), OriginalSize: int64(len(xmlBytes))}
	if u.hdr.BootIndex >= 1 && int(u.hdr.BootIndex) <= len(newMeta) {
		bm := newMeta[u.hdr.BootIndex-1]
		u.hdr.BootMetadata = resourceDescriptor{Offset: bm.offset, CompressedSize: bm.csize, OriginalSize: bm.osize, Flags: resFlagMetadata}
	}
	u.hdr.Integrity = resourceDescriptor{}
	if _, err := u.f.WriteAt(serializeHeader(u.hdr), 0); err != nil {
		return fmt.Errorf("wim: write header: %w", err)
	}
	return u.f.Sync()
}

// tableRec is one blob-table entry's data (resource location + identity).
type tableRec struct {
	hash                 [20]byte
	offset, csize, osize int64
	flags                byte
}

// parseFullTable reads the offset table and returns the file-blob entries and the
// per-image metadata entries (in image order). It rejects solid resources.
func (u *Updater) parseFullTable() (blobs []tableRec, meta []tableRec, err error) {
	raw, err := u.w.readResourceRaw(u.hdr.OffsetTable)
	if err != nil {
		return nil, nil, fmt.Errorf("wim: read offset table: %w", err)
	}
	n := len(raw) / blobTableEntrySize
	for i := 0; i < n; i++ {
		rd, hash := parseBlobEntry(raw, i)
		switch {
		case rd.Flags&resFlagSolid != 0:
			return nil, nil, fmt.Errorf("wim: cannot modify files in a WIM with solid resources; export it non-solid first")
		case rd.Flags&resFlagMetadata != 0:
			meta = append(meta, tableRec{hash: hash, offset: rd.Offset, csize: rd.CompressedSize, osize: rd.OriginalSize, flags: resFlagMetadata})
		default:
			if !isZeroHash(hash) {
				blobs = append(blobs, tableRec{hash: hash, offset: rd.Offset, csize: rd.CompressedSize, osize: rd.OriginalSize, flags: rd.Flags})
			}
		}
	}
	return blobs, meta, nil
}

// blobContent pairs a new blob's hash with its bytes.
type blobContent struct {
	hash    [20]byte
	content []byte
}

// applyFileOps mutates the writeNode tree per ops and returns the new blob
// contents to append (empty files add no blob).
func applyFileOps(root *writeNode, ops []fileOp) ([]blobContent, error) {
	var adds []blobContent
	for _, op := range ops {
		parts := strings.Split(op.wimPath, "/")
		if op.remove {
			if !removeFileNode(root, parts) {
				return nil, fmt.Errorf("wim: remove: %q not found", op.wimPath)
			}
			continue
		}
		h, hasBlob := setFileNode(root, parts, op.content)
		if hasBlob {
			adds = append(adds, blobContent{hash: h, content: op.content})
		}
	}
	return adds, nil
}

// fileToWriteNode converts a read-side File tree into a writable writeNode tree.
func fileToWriteNode(f *File) *writeNode {
	n := &writeNode{
		name:       f.Name,
		isDir:      f.IsDir(),
		attrs:      f.Attributes,
		createTime: f.CreationTime,
		accessTime: f.LastAccessTime,
		writeTime:  f.LastWriteTime,
		hash:       f.Hash,
	}
	for _, c := range f.Children() {
		n.children = append(n.children, fileToWriteNode(c))
	}
	return n
}

// setFileNode creates or replaces the leaf named by parts under root, creating
// intermediate directories. It returns the content hash and whether a blob is
// needed (false for empty content, which the WIM format stores as a zero hash).
func setFileNode(root *writeNode, parts []string, content []byte) ([20]byte, bool) {
	cur := root
	for _, p := range parts[:len(parts)-1] {
		cur = childDir(cur, p)
	}
	name := parts[len(parts)-1]

	var h [20]byte
	hasBlob := len(content) > 0
	if hasBlob {
		h = sha1.Sum(content) //nolint:gosec
	}
	for _, c := range cur.children {
		if strings.EqualFold(c.name, name) && !c.isDir {
			c.hash = h
			c.attrs = 0x20 // FILE_ATTRIBUTE_ARCHIVE
			return h, hasBlob
		}
	}
	cur.children = append(cur.children, &writeNode{name: name, attrs: 0x20, hash: h})
	return h, hasBlob
}

// childDir returns the child directory named name under parent, creating it if
// absent.
func childDir(parent *writeNode, name string) *writeNode {
	for _, c := range parent.children {
		if strings.EqualFold(c.name, name) && c.isDir {
			return c
		}
	}
	d := &writeNode{name: name, isDir: true, attrs: attrDirectory}
	parent.children = append(parent.children, d)
	return d
}

// removeFileNode deletes the entry named by parts, returning whether it existed.
func removeFileNode(root *writeNode, parts []string) bool {
	cur := root
	for _, p := range parts[:len(parts)-1] {
		next := findChild(cur, p)
		if next == nil {
			return false
		}
		cur = next
	}
	name := parts[len(parts)-1]
	for i, c := range cur.children {
		if strings.EqualFold(c.name, name) {
			cur.children = append(cur.children[:i], cur.children[i+1:]...)
			return true
		}
	}
	return false
}

func findChild(parent *writeNode, name string) *writeNode {
	for _, c := range parent.children {
		if strings.EqualFold(c.name, name) {
			return c
		}
	}
	return nil
}
