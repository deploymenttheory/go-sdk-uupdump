package wim

import (
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // WIM blobs are content-addressed by SHA-1
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"
)

// versionDefault is the WIM format version for non-solid images
// (WIM_VERSION_DEFAULT). Solid ESDs use 0x0E00; everything written here is
// standard, optionally XPRESS/LZX-compressed, non-solid WIM.
const versionDefault = 0x00010d00

// writeNode is a node in the directory tree being written.
type writeNode struct {
	name       string
	isDir      bool
	attrs      uint32
	createTime time.Time
	accessTime time.Time
	writeTime  time.Time
	hash       [20]byte // zero for directories and empty files
	children   []*writeNode
}

type blobRec struct {
	hash         [20]byte
	offset       int64
	size         int64 // on-disk size (compressed, when flags has resFlagCompressed)
	originalSize int64 // uncompressed size
	flags        byte
}

// imageRec records a written image's metadata resource and catalog info.
type imageRec struct {
	metaOffset  int64
	metaSize    int64
	metaHash    [20]byte
	name        string
	description string
	displayName string
	flags       string
	// windowsXML is the verbatim "<WINDOWS>…</WINDOWS>" fragment to re-emit for
	// this image (empty for images captured from a bare directory tree). Carried
	// from the source catalog by AddImageFromWIM so INSTALLATIONTYPE/EDITIONID/
	// ARCH/LANGUAGES survive a remaster.
	windowsXML string
	dirCount   int64
	fileCount  int64
	totalBytes int64
}

// Writer assembles a multi-image, uncompressed WIM: a header placeholder, then
// per-image file blobs (deduplicated by SHA-1) and metadata resources, and
// finally the blob table, XML catalog, and a header rewrite. Call AddImage once
// per image, then Close. out must be seekable.
type Writer struct {
	out       io.WriteSeeker
	pos       int64
	blobs     []blobRec
	seen      map[[20]byte]bool // blob dedup across all images
	images    []imageRec
	comp      Compression // resource compression; CompressionNone writes raw
	bootIndex int         // 1-based boot image index; 0 means none
}

// NewWriter starts an uncompressed WIM, reserving the header.
func NewWriter(out io.WriteSeeker) (*Writer, error) {
	return NewWriterCompressed(out, CompressionNone)
}

// NewWriterCompressed starts a WIM whose file resources are compressed with comp
// (CompressionNone writes them raw). boot.wim must be compressed for the
// firmware to RAM-disk-load it at boot.
func NewWriterCompressed(out io.WriteSeeker, comp Compression) (*Writer, error) {
	w := &Writer{out: out, seen: map[[20]byte]bool{}, comp: comp}
	if err := w.write(make([]byte, headerSize)); err != nil { // header placeholder
		return nil, err
	}
	return w, nil
}

// SetBootIndex marks the 1-based image number as the bootable image.
// The header's BootIndex and BootMetadata fields are written during Close.
// Call with 0 to produce no boot image (the default).
func (w *Writer) SetBootIndex(n int) {
	w.bootIndex = n
}

func (w *Writer) write(b []byte) error {
	n, err := w.out.Write(b)
	w.pos += int64(n)
	if err != nil {
		return fmt.Errorf("wim: write: %w", err)
	}
	return nil
}

// AddImage appends the directory tree at srcDir as a new image named name.
func (w *Writer) AddImage(srcDir, name string) error {
	root := &writeNode{name: "", isDir: true, attrs: attrDirectory}
	rec := imageRec{name: name}
	if err := w.addChildren(root, srcDir, &rec.dirCount, &rec.fileCount, &rec.totalBytes); err != nil {
		return err
	}
	meta := buildMetadata(root)
	rec.metaOffset = w.pos
	if err := w.write(meta); err != nil {
		return err
	}
	rec.metaSize = int64(len(meta))
	rec.metaHash = sha1.Sum(meta) //nolint:gosec
	w.images = append(w.images, rec)
	return nil
}

// Close writes the blob table, XML catalog, and final header.
func (w *Writer) Close() error {
	if len(w.images) == 0 {
		return fmt.Errorf("wim: no images added")
	}
	tableOffset := w.pos
	for _, b := range w.blobs {
		if err := w.write(blobEntry(b.hash, b.offset, b.size, b.originalSize, b.flags)); err != nil {
			return err
		}
	}
	// Metadata resource entries, in image order.
	for _, im := range w.images {
		if err := w.write(blobEntry(im.metaHash, im.metaOffset, im.metaSize, im.metaSize, resFlagMetadata)); err != nil {
			return err
		}
	}
	tableSize := w.pos - tableOffset

	xmlOffset := w.pos
	if err := w.write(buildXML(w.images)); err != nil {
		return err
	}
	xmlSize := w.pos - xmlOffset

	return w.finalizeHeader(tableOffset, tableSize, xmlOffset, xmlSize, len(w.images))
}

// CreateFromDir writes the directory tree at srcDir to out as a single-image,
// uncompressed WIM named imageName.
func CreateFromDir(out io.WriteSeeker, srcDir, imageName string) error {
	w, err := NewWriter(out)
	if err != nil {
		return err
	}
	if err := w.AddImage(srcDir, imageName); err != nil {
		return err
	}
	return w.Close()
}

// addChildren walks dir, writing each regular file's bytes as an (uncompressed)
// blob and recording the directory structure under node.
func (w *Writer) addChildren(node *writeNode, dir string, dirCount, fileCount, totalBytes *int64) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("wim: read dir %s: %w", dir, err)
	}
	for _, e := range entries {
		full := filepath.Join(dir, e.Name())
		info, err := e.Info()
		if err != nil {
			return fmt.Errorf("wim: stat %s: %w", full, err)
		}
		mod := info.ModTime()
		child := &writeNode{name: e.Name(), createTime: mod, accessTime: mod, writeTime: mod}
		switch {
		case e.IsDir():
			child.isDir = true
			child.attrs = attrDirectory
			*dirCount++
			if err := w.addChildren(child, full, dirCount, fileCount, totalBytes); err != nil {
				return err
			}
		default:
			child.attrs = 0x20 // FILE_ATTRIBUTE_ARCHIVE
			*fileCount++
			if err := w.addFileBlob(child, full); err != nil {
				return err
			}
			*totalBytes += sizeOf(info)
		}
		node.children = append(node.children, child)
	}
	return nil
}

func sizeOf(info os.FileInfo) int64 { return info.Size() }

func (w *Writer) addFileBlob(node *writeNode, path string) error {
	content, err := os.ReadFile(path) //nolint:gosec // caller-provided tree
	if err != nil {
		return fmt.Errorf("wim: read %s: %w", path, err)
	}
	if len(content) == 0 {
		return nil // empty file: zero hash, no blob
	}
	node.hash = sha1.Sum(content) //nolint:gosec
	if w.seen[node.hash] {
		return nil // deduplicated
	}
	w.seen[node.hash] = true
	offset := w.pos
	onDisk, flags, err := w.writeBlobData(content)
	if err != nil {
		return err
	}
	w.blobs = append(w.blobs, blobRec{hash: node.hash, offset: offset, size: onDisk, originalSize: int64(len(content)), flags: flags})
	return nil
}

// blobEntry encodes one 50-byte offset-table entry. compressedSize is the
// on-disk size (low 56 bits, with flags in the high byte); originalSize is the
// uncompressed size (equal to compressedSize for uncompressed resources).
func blobEntry(hash [20]byte, offset, compressedSize, originalSize int64, flags byte) []byte {
	e := make([]byte, blobTableEntrySize)
	le := binary.LittleEndian
	le.PutUint64(e[0:], uint64(compressedSize)|uint64(flags)<<56)
	le.PutUint64(e[8:], uint64(offset))
	le.PutUint64(e[16:], uint64(originalSize))
	le.PutUint16(e[24:], 1) // part number
	le.PutUint32(e[26:], 1) // refcount
	copy(e[30:], hash[:])
	return e
}

func (w *Writer) finalizeHeader(tableOffset, tableSize, xmlOffset, xmlSize int64, imageCount int) error {
	if w.bootIndex < 0 || w.bootIndex > imageCount {
		return fmt.Errorf("wim: boot index %d out of range [0, %d]", w.bootIndex, imageCount)
	}

	hdr := make([]byte, headerSize)
	le := binary.LittleEndian
	copy(hdr, imageTag[:])
	le.PutUint32(hdr[8:], headerSize) // cbSize
	le.PutUint32(hdr[12:], versionDefault)
	hflags, chunkSize := w.headerCompression()
	le.PutUint32(hdr[16:], hflags)    // compression flags
	le.PutUint32(hdr[20:], chunkSize) // chunk size (0 when uncompressed)
	if _, err := rand.Read(hdr[24:40]); err != nil {
		return fmt.Errorf("wim: guid: %w", err)
	}
	le.PutUint16(hdr[40:], 1)                  // part number
	le.PutUint16(hdr[42:], 1)                  // total parts
	le.PutUint32(hdr[44:], uint32(imageCount)) // image count
	writeReshdr(hdr[48:], tableOffset, tableSize)
	writeReshdr(hdr[72:], xmlOffset, xmlSize)

	if w.bootIndex > 0 {
		boot := w.images[w.bootIndex-1]
		writeReshdrf(hdr[96:], boot.metaOffset, boot.metaSize, resFlagMetadata)
		le.PutUint32(hdr[120:], uint32(w.bootIndex))
	}

	if _, err := w.out.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("wim: seek header: %w", err)
	}
	if _, err := w.out.Write(hdr); err != nil {
		return fmt.Errorf("wim: write header: %w", err)
	}
	return nil
}

// serializeHeader reconstructs the 208-byte WIM header from a parsed header,
// preserving every resource descriptor faithfully (compressed size, flags,
// offset, and original size independently). It is the inverse of parseHeader and
// is used by the Updater to rewrite the header after editing the catalog.
func serializeHeader(h header) []byte {
	le := binary.LittleEndian
	b := make([]byte, headerSize)
	copy(b, imageTag[:])
	le.PutUint32(b[8:], headerSize) // cbSize
	le.PutUint32(b[12:], h.Version)
	le.PutUint32(b[16:], h.Flags)
	le.PutUint32(b[20:], h.ChunkSize)
	copy(b[24:40], h.GUID[:])
	le.PutUint16(b[40:], h.PartNumber)
	le.PutUint16(b[42:], h.TotalParts)
	le.PutUint32(b[44:], h.ImageCount)
	putResource(b[48:72], h.OffsetTable)
	putResource(b[72:96], h.XMLData)
	putResource(b[96:120], h.BootMetadata)
	le.PutUint32(b[120:], h.BootIndex)
	putResource(b[124:148], h.Integrity)
	return b
}

// putResource writes a 24-byte resource descriptor: the low 56 bits of the first
// word are the compressed size and the high 8 bits are flags, followed by the
// 8-byte offset and 8-byte original size. Inverse of parseResource.
func putResource(b []byte, rd resourceDescriptor) {
	le := binary.LittleEndian
	le.PutUint64(b[0:], uint64(rd.CompressedSize)&0x00FFFFFFFFFFFFFF|uint64(rd.Flags)<<56)
	le.PutUint64(b[8:], uint64(rd.Offset))
	le.PutUint64(b[16:], uint64(rd.OriginalSize))
}

// writeReshdr writes an uncompressed resource descriptor (size==originalSize,
// no flags) into b.
func writeReshdr(b []byte, offset, size int64) {
	writeReshdrf(b, offset, size, 0)
}

// writeReshdrf writes a resource descriptor with explicit flags into b.
// compressedSize and originalSize are both set to size (the resource is not
// chunk-compressed); the flags byte occupies the high 8 bits of the first word.
func writeReshdrf(b []byte, offset, size int64, flags byte) {
	le := binary.LittleEndian
	le.PutUint64(b[0:], uint64(size)|uint64(flags)<<56)
	le.PutUint64(b[8:], uint64(offset))
	le.PutUint64(b[16:], uint64(size))
}

// --- metadata (security data + dentry tree) ---

func buildMetadata(root *writeNode) []byte {
	le := binary.LittleEndian
	buf := make([]byte, 8) // empty security data: total_length=8, num_entries=0
	le.PutUint32(buf[0:], 8)

	rootSubdirPos := appendDentry(&buf, root)
	appendEndMarker(&buf) // root has no siblings
	childOff := appendChildList(&buf, root.children)
	le.PutUint64(buf[rootSubdirPos:], uint64(childOff))
	return buf
}

// appendChildList appends the dentries for children (plus the terminating
// zero-length marker), then recursively appends each subdirectory's child list
// and patches its subdir offset. Returns the offset of this child list.
func appendChildList(buf *[]byte, children []*writeNode) int64 {
	off := int64(len(*buf))
	type patch struct {
		node *writeNode
		pos  int
	}
	var patches []patch
	for _, c := range children {
		p := appendDentry(buf, c)
		if c.isDir {
			patches = append(patches, patch{c, p})
		}
	}
	appendEndMarker(buf)
	for _, pt := range patches {
		childOff := appendChildList(buf, pt.node.children)
		binary.LittleEndian.PutUint64((*buf)[pt.pos:], uint64(childOff))
	}
	return off
}

// appendDentry appends a directory entry for node and returns the byte offset of
// its subdir-offset field (for later patching of directories).
func appendDentry(buf *[]byte, node *writeNode) int {
	le := binary.LittleEndian
	nameUTF16 := encodeUTF16LE(node.name)
	namesLen := len(nameUTF16) + 2 // + null terminator; short name length 0
	total := align8Int(direntrySize + namesLen)

	start := len(*buf)
	d := make([]byte, total)
	le.PutUint64(d[0:], uint64(total)) // length
	le.PutUint32(d[8:], node.attrs)
	le.PutUint32(d[12:], 0xffffffff)                      // security ID: none
	le.PutUint64(d[40:], timeToFiletime(node.createTime)) // creation
	le.PutUint64(d[48:], timeToFiletime(node.accessTime)) // last access
	le.PutUint64(d[56:], timeToFiletime(node.writeTime))  // last write
	if !node.isDir {
		copy(d[64:84], node.hash[:])
	}
	le.PutUint16(d[96:], 0)                       // stream count
	le.PutUint16(d[98:], 0)                       // short name length
	le.PutUint16(d[100:], uint16(len(nameUTF16))) // file name length (bytes)
	copy(d[direntrySize:], nameUTF16)

	*buf = append(*buf, d...)
	return start + 16 // subdir-offset field
}

func appendEndMarker(buf *[]byte) { *buf = append(*buf, make([]byte, 8)...) }

func align8Int(n int) int { return (n + 7) &^ 7 }

// timeToFiletime converts a time.Time to a Windows FILETIME (100-ns intervals
// since 1601). The zero time maps to 0.
func timeToFiletime(t time.Time) uint64 {
	if t.IsZero() {
		return 0
	}
	return uint64(t.UnixNano()/100 + 116444736000000000)
}

func encodeUTF16LE(s string) []byte {
	u16 := utf16.Encode([]rune(s))
	b := make([]byte, len(u16)*2)
	for i, c := range u16 {
		binary.LittleEndian.PutUint16(b[i*2:], c)
	}
	return b
}

// buildXML produces the UTF-16LE (BOM-prefixed) WIM XML catalog. The per-image
// element (including the preserved <WINDOWS> fragment) is built by
// imageXMLElement in catalog.go so the same serialization is reused by the
// in-place Updater.
func buildXML(images []imageRec) []byte {
	var total int64
	for _, im := range images {
		total += im.totalBytes
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "<WIM><TOTALBYTES>%d</TOTALBYTES>", total)
	for i, im := range images {
		sb.WriteString(imageXMLElement(i+1, im))
	}
	sb.WriteString("</WIM>")

	out := []byte{0xFF, 0xFE} // UTF-16LE BOM
	return append(out, encodeUTF16LE(sb.String())...)
}
