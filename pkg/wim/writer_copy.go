package wim

import (
	"cmp"
	"crypto/sha1" //nolint:gosec // WIM blobs are content-addressed by SHA-1
	"slices"
)

// AddImageFromWIM copies the 1-based image index from src into the WIM being
// written, as a new image named name. Blob data is streamed directly from src
// (no extraction to disk), deduplicated by SHA-1, and the directory tree is
// rebuilt preserving file attributes and the three timestamps.
//
// Blobs are written in two passes: first the directory tree is planned (and the
// set of unique blobs collected), then the blobs are copied in source-storage
// order. The ordering is essential for solid resources: their chunks are large
// and the reader caches only the most-recently-decompressed chunk, so reading
// blobs in tree order (which does not match the solid layout) would
// re-decompress the same chunks repeatedly. Reading in ascending solid offset
// decompresses each chunk exactly once.
func (w *Writer) AddImageFromWIM(src *WIM, index int, name string) error {
	root, err := src.OpenImage(index)
	if err != nil {
		return err
	}
	if err := src.loadOffsetTable(); err != nil {
		return err
	}

	rec := imageRec{name: name}
	// Preserve the source image's catalog metadata so a remaster keeps edition/
	// arch/installation-type. The <WINDOWS> fragment is carried verbatim; without
	// it Windows Setup loses edition selection and the INSTALLATIONTYPE=Server
	// bypass has nothing to edit.
	for _, im := range src.Images() {
		if im.Index == index {
			rec.description = im.Description
			rec.displayName = im.DisplayName
			rec.flags = im.Flags
			rec.windowsXML = im.WindowsXML
			break
		}
	}
	var pending []*File // unique, not-yet-written blobs to copy, in tree order
	inBatch := make(map[[20]byte]bool)
	node := w.planNode(src, root, &rec, &pending, inBatch)

	for _, f := range sortBlobsBySource(src, pending) {
		if w.seen[f.Hash] {
			continue
		}
		data, err := src.ReadFile(f)
		if err != nil {
			return err
		}
		w.seen[f.Hash] = true
		offset := w.pos
		onDisk, flags, werr := w.writeBlobData(data)
		if werr != nil {
			return werr
		}
		w.blobs = append(w.blobs, blobRec{hash: f.Hash, offset: offset, size: onDisk, originalSize: int64(len(data)), flags: flags})
		rec.totalBytes += int64(len(data))
	}

	meta := buildMetadata(node)
	rec.metaOffset = w.pos
	if err := w.write(meta); err != nil {
		return err
	}
	rec.metaSize = int64(len(meta))
	rec.metaHash = sha1.Sum(meta) //nolint:gosec
	w.images = append(w.images, rec)
	return nil
}

// planNode converts a source File into a writeNode (preserving attributes and
// timestamps) and collects the unique regular-file blobs that still need to be
// written. It performs no blob reads; the actual copy happens afterwards in
// source order.
func (w *Writer) planNode(src *WIM, f *File, rec *imageRec, pending *[]*File, inBatch map[[20]byte]bool) *writeNode {
	node := &writeNode{
		name:       f.Name,
		isDir:      f.IsDir(),
		attrs:      f.Attributes,
		createTime: f.CreationTime,
		accessTime: f.LastAccessTime,
		writeTime:  f.LastWriteTime,
		hash:       f.Hash,
	}

	if f.IsDir() {
		rec.dirCount++
		for _, c := range f.Children() {
			node.children = append(node.children, w.planNode(src, c, rec, pending, inBatch))
		}
		return node
	}

	rec.fileCount++
	if isZeroHash(f.Hash) || w.seen[f.Hash] || inBatch[f.Hash] {
		return node // empty file, or a blob already written / already queued
	}
	inBatch[f.Hash] = true
	*pending = append(*pending, f)
	return node
}

// sortBlobsBySource orders files by where their content is stored: blobs in the
// same solid resource are sorted by ascending uncompressed offset (so the
// solid's one-chunk cache is hit sequentially), with each distinct solid kept
// contiguous; standalone-resource blobs sort after the solids in their original
// order. The sort is stable, so blobs with equal keys keep tree order.
func sortBlobsBySource(src *WIM, files []*File) []*File {
	// Assign each distinct solid a contiguous group index in first-seen order.
	solidIdx := make(map[*solidResource]int)
	for _, f := range files {
		if loc, ok := src.blobs[f.Hash]; ok && loc.solid != nil {
			if _, seen := solidIdx[loc.solid]; !seen {
				solidIdx[loc.solid] = len(solidIdx)
			}
		}
	}
	standaloneGroup := len(solidIdx)

	key := func(f *File) (int, int64) {
		loc, ok := src.blobs[f.Hash]
		if !ok {
			return standaloneGroup, 0
		}
		if loc.solid != nil {
			return solidIdx[loc.solid], loc.offset
		}
		return standaloneGroup, 0
	}

	slices.SortStableFunc(files, func(a, b *File) int {
		ga, oa := key(a)
		gb, ob := key(b)
		if ga != gb {
			return cmp.Compare(ga, gb)
		}
		return cmp.Compare(oa, ob)
	})
	return files
}
