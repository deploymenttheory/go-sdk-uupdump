package udf

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// node is a file or directory in the tree being written.
type node struct {
	name     string
	isDir    bool
	srcPath  string // regular files only
	size     int64
	modTime  time.Time
	children []*node

	feBlock  uint32 // File Entry logical block (within partition)
	dataLB   uint32 // first logical block of file data / directory FID list
	dataLen  uint32 // byte length of that extent
	uniqueID uint64
}

// fixed image layout (logical sector numbers; sector size 2048).
const (
	lbnAnchor      = 256 // Anchor Volume Descriptor Pointer
	lbnMainVDS     = 257 // Main Volume Descriptor Sequence (16 sectors)
	lbnReserveVDS  = 273 // Reserve Volume Descriptor Sequence (16 sectors)
	lbnIntegrity   = 289 // Logical Volume Integrity Descriptor (+ terminator)
	lbnPartitionLB = 291 // partition starting location (absolute sector)
	vdsSectors     = 16
	defaultVRS     = 16 // default first Volume Recognition Sequence sector
)

// Location is the absolute sector and byte length of a written file's data.
type Location struct {
	Sector uint64
	Length int64
}

// Result reports the assembled image's total sector count and the location of
// every regular file, keyed by slash-separated path relative to the root.
type Result struct {
	TotalSectors uint64
	Files        map[string]Location
}

// Options configures a UDF write.
type Options struct {
	VolumeID string
	// VRSStart is the first Volume Recognition Sequence sector. Zero means 16.
	// A bridge image that places ISO9660 descriptors at 16-18 sets this to 19.
	VRSStart uint32
}

// Write masters a UDF 1.02 file system holding the directory tree at srcDir into
// out, labelled volumeID.
func Write(out io.WriterAt, srcDir, volumeID string) error {
	_, err := WriteTree(out, srcDir, Options{VolumeID: volumeID})
	return err
}

// WriteTree masters a UDF 1.02 file system and reports the layout. out is
// written via WriteAt; file data is streamed so large files are not buffered.
func WriteTree(out io.WriterAt, srcDir string, opts Options) (*Result, error) {
	root, err := buildTree(srcDir)
	if err != nil {
		return nil, err
	}

	// Phase 1: assign partition-relative logical blocks. Block 0 is the File Set
	// Descriptor, block 1 its Terminating Descriptor (Windows-good media always
	// closes the FSD sequence with one); everything else follows.
	next := uint32(2)
	var uid uint64
	assignBlocks(root, &next, &uid)
	partitionBlocks := next

	vrs := opts.VRSStart
	if vrs == 0 {
		vrs = defaultVRS
	}
	w := &imageWriter{
		out:       out,
		partStart: lbnPartitionLB,
		volumeID:  opts.VolumeID,
		now:       time.Now().UTC(),
		vrsStart:  vrs,
		files:     map[string]Location{},
	}
	numFiles, numDirs := countTree(root)
	if err := w.writeVolumeStructures(partitionBlocks, numFiles, numDirs); err != nil {
		return nil, err
	}
	if err := w.writeFileSet(root); err != nil {
		return nil, err
	}
	if err := w.writeTree(root, root.feBlock, ""); err != nil { // root's parent is itself
		return nil, err
	}

	// Backup anchor in the last sector.
	totalSectors := lbnPartitionLB + uint64(partitionBlocks)
	backup := make([]byte, SectorSize)
	putAnchor(backup, uint32(totalSectors))
	if err := w.writeSector(totalSectors, backup); err != nil {
		return nil, err
	}
	return &Result{TotalSectors: totalSectors + 1, Files: w.files}, nil
}

// assignBlocks lays out FE and data/FID extents depth-first.
func assignBlocks(n *node, next *uint32, uid *uint64) {
	n.feBlock = *next
	*next++
	n.uniqueID = *uid
	*uid++

	if n.isDir {
		n.dataLen = dirFIDBytes(n)
		n.dataLB = *next
		*next += blocks(uint64(n.dataLen))
		for _, c := range n.children {
			assignBlocks(c, next, uid)
		}
		return
	}
	n.dataLen = uint32(n.size)
	if n.size > 0 {
		n.dataLB = *next
		*next += blocks(uint64(n.size))
	}
}

func blocks(n uint64) uint32 { return uint32((n + SectorSize - 1) / SectorSize) }

// imageWriter writes sectors and streams file data into out.
type imageWriter struct {
	out       io.WriterAt
	partStart uint64
	volumeID  string
	now       time.Time
	vrsStart  uint32
	files     map[string]Location
}

func (w *imageWriter) writeSector(sector uint64, data []byte) error {
	if _, err := w.out.WriteAt(data, int64(sector*SectorSize)); err != nil {
		return fmt.Errorf("udf: write sector %d: %w", sector, err)
	}
	return nil
}

// writeVolumeStructures writes the VRS, anchor, main+reserve VDS, and integrity
// descriptors.
// countTree counts the files and directories in the tree rooted at n (the root
// directory itself counts as a directory), for the LVID's LVInformation.
func countTree(n *node) (files, dirs uint32) {
	if n.isDir {
		dirs = 1
	} else {
		files = 1
	}
	for _, c := range n.children {
		f, d := countTree(c)
		files += f
		dirs += d
	}
	return files, dirs
}

func (w *imageWriter) writeVolumeStructures(partitionBlocks, numFiles, numDirs uint32) error {
	// Volume Recognition Sequence.
	if err := w.writeSector(uint64(w.vrsStart), volStructDesc("BEA01")); err != nil {
		return err
	}
	if err := w.writeSector(uint64(w.vrsStart)+1, volStructDesc("NSR02")); err != nil {
		return err
	}
	if err := w.writeSector(uint64(w.vrsStart)+2, volStructDesc("TEA01")); err != nil {
		return err
	}

	anchor := make([]byte, SectorSize)
	putAnchor(anchor, lbnAnchor)
	if err := w.writeSector(lbnAnchor, anchor); err != nil {
		return err
	}

	for _, base := range []uint32{lbnMainVDS, lbnReserveVDS} {
		if err := w.writeVDS(base, partitionBlocks); err != nil {
			return err
		}
	}
	return w.writeIntegrity(partitionBlocks, numFiles, numDirs)
}

func (w *imageWriter) writeVDS(base, partitionBlocks uint32) error {
	descs := [][]byte{
		w.primaryVolumeDescriptor(base + 0),
		w.implUseVolumeDescriptor(base + 1),
		w.partitionDescriptor(base+2, partitionBlocks),
		w.logicalVolumeDescriptor(base + 3),
		w.unallocatedSpaceDescriptor(base + 4),
		terminatingDescriptor(base + 5),
	}
	for i, d := range descs {
		if err := w.writeSector(uint64(base)+uint64(i), d); err != nil {
			return err
		}
	}
	return nil
}

func (w *imageWriter) writeIntegrity(partitionBlocks, numFiles, numDirs uint32) error {
	if err := w.writeSector(lbnIntegrity, w.integrityDescriptor(partitionBlocks, numFiles, numDirs)); err != nil {
		return err
	}
	return w.writeSector(lbnIntegrity+1, terminatingDescriptor(lbnIntegrity+1))
}

func (w *imageWriter) writeFileSet(root *node) error {
	fsd := make([]byte, SectorSize)
	le := binary.LittleEndian
	copy(fsd[16:], encodeTimestamp(w.now))
	le.PutUint16(fsd[28:], 3) // interchange level
	le.PutUint16(fsd[30:], 3)
	le.PutUint32(fsd[32:], 1) // char set list
	le.PutUint32(fsd[36:], 1)
	copy(fsd[48:], charSpec())
	copy(fsd[112:], encodeDString(w.volumeID, 128))
	copy(fsd[240:], charSpec())
	copy(fsd[304:], encodeDString(w.volumeID, 32))
	copy(fsd[400:], longAD(SectorSize, root.feBlock, 0)) // root directory ICB
	copy(fsd[416:], domainEntityID())
	putTag(fsd[:512], tagFileSet, 0) // partition LB 0
	if err := w.writeSector(w.partStart+0, fsd); err != nil {
		return err
	}
	// Terminate the File Set Descriptor sequence at partition LB 1. Both
	// Windows-good references (Microsoft, hdiutil) record one; without it the
	// next sector holds a File Entry tag where UDFS expects the sequence to end.
	return w.writeSector(w.partStart+1, terminatingDescriptor(1))
}

// writeTree writes each node's File Entry plus its data (file bytes or directory
// FID list), recording each regular file's absolute location under prefix.
func (w *imageWriter) writeTree(n *node, parentFE uint32, prefix string) error {
	if n.isDir {
		if err := w.writeDirEntry(n, parentFE); err != nil {
			return err
		}
		for _, c := range n.children {
			child := c.name
			if prefix != "" {
				child = prefix + "/" + c.name
			}
			if err := w.writeTree(c, n.feBlock, child); err != nil {
				return err
			}
		}
		return nil
	}
	if n.size > 0 {
		w.files[prefix] = Location{Sector: w.partStart + uint64(n.dataLB), Length: n.size}
	}
	return w.writeFileEntry(n)
}

// fidEntry is one directory entry to emit as a File Identifier Descriptor.
type fidEntry struct {
	name string
	fe   uint32 // child File Entry block
	dir  bool
}

// dirFIDStream builds a directory's File Identifier Descriptor byte stream: the parent
// entry ("") first, then one per child, packed contiguously. FIDs may cross logical-
// block boundaries (as real Windows media does); baseLB is the directory's first data
// block, used to stamp each FID's tag location (the block in which it begins).
func dirFIDStream(ents []fidEntry, baseLB uint32) []byte {
	var fid []byte
	for _, e := range ents {
		fid = appendFID(fid, baseLB, e.name, e.fe, e.dir)
	}
	return fid
}

func (w *imageWriter) writeDirEntry(n *node, parentFE uint32) error {
	// The parent ("") FID references the PARENT directory's File Entry, not this
	// directory's own — Windows UDFS validates this back-reference and rejects the
	// volume ("Mismatch between parent FID and in-reference") if it points to self.
	ents := []fidEntry{{"", parentFE, true}}
	for _, c := range n.children {
		ents = append(ents, fidEntry{c.name, c.feBlock, c.isDir})
	}
	fid := dirFIDStream(ents, n.dataLB)
	for off := 0; off < len(fid); off += SectorSize {
		end := min(off+SectorSize, len(fid))
		sector := make([]byte, SectorSize)
		copy(sector, fid[off:end])
		if err := w.writeSector(w.partStart+uint64(n.dataLB)+uint64(off/SectorSize), sector); err != nil {
			return err
		}
	}
	fe := w.fileEntry(n, fileTypeDirectory)
	return w.writeSector(w.partStart+uint64(n.feBlock), fe)
}

func (w *imageWriter) writeFileEntry(n *node) error {
	if n.size > 0 {
		if err := w.streamFile(n); err != nil {
			return err
		}
	}
	fe := w.fileEntry(n, fileTypeRegular)
	return w.writeSector(w.partStart+uint64(n.feBlock), fe)
}

func (w *imageWriter) streamFile(n *node) error {
	src, err := os.Open(n.srcPath) //nolint:gosec // caller-provided tree
	if err != nil {
		return fmt.Errorf("udf: open %s: %w", n.srcPath, err)
	}
	defer src.Close()
	base := int64((w.partStart + uint64(n.dataLB)) * SectorSize)
	buf := make([]byte, SectorSize)
	var off int64
	for {
		nr, rerr := src.Read(buf)
		if nr > 0 {
			// Zero the tail of the final partial sector.
			if nr < len(buf) {
				for i := nr; i < len(buf); i++ {
					buf[i] = 0
				}
			}
			if _, err := w.out.WriteAt(buf[:sectorRound(nr)], base+off); err != nil {
				return fmt.Errorf("udf: write file data: %w", err)
			}
			off += int64(sectorRound(nr))
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return fmt.Errorf("udf: read %s: %w", n.srcPath, rerr)
		}
	}
}

func sectorRound(n int) int { return (n + SectorSize - 1) / SectorSize * SectorSize }

// buildTree walks srcDir into a node tree.
func buildTree(srcDir string) (*node, error) {
	info, err := os.Stat(srcDir)
	if err != nil {
		return nil, fmt.Errorf("udf: stat %s: %w", srcDir, err)
	}
	root := &node{name: "", isDir: true, modTime: info.ModTime()}
	if err := addChildren(root, srcDir); err != nil {
		return nil, err
	}
	return root, nil
}

func addChildren(n *node, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("udf: read dir %s: %w", dir, err)
	}
	for _, e := range entries {
		full := filepath.Join(dir, e.Name())
		info, err := e.Info()
		if err != nil {
			return fmt.Errorf("udf: stat %s: %w", full, err)
		}
		c := &node{name: e.Name(), isDir: e.IsDir(), modTime: info.ModTime()}
		if e.IsDir() {
			if err := addChildren(c, full); err != nil {
				return err
			}
		} else {
			c.srcPath = full
			c.size = info.Size()
		}
		n.children = append(n.children, c)
	}
	return nil
}
