package wim

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

// Updater edits a WIM's XML image catalog in place. It is the pure-Go analogue
// of wimlib_get_image_property / wimlib_set_image_property + wimlib_overwrite:
// property edits touch only the catalog, never the file blobs or metadata
// resources, so committing appends a rewritten XML resource and repoints the
// header at it.
//
// The headline use is the Windows 11 requirement bypass — setting
// WINDOWS/INSTALLATIONTYPE to "Server" on every image makes Windows Setup skip
// the TPM/vTPM/Secure Boot/RAM/CPU appraisal. It requires that the catalog
// carry a <WINDOWS> element to edit, which the writer now preserves.
//
// Only uncompressed XML catalogs are supported (the standard for install.wim /
// boot.wim); OpenForUpdate returns an error for a compressed catalog.
type Updater struct {
	f    *os.File
	w    *WIM // read view over the same fd (for image trees and blobs)
	hdr  header
	recs []imageRec       // mutable catalog, image order
	ops  map[int][]fileOp // pending file add/remove ops, per 1-based image
}

// fileOp is a pending in-image file modification.
type fileOp struct {
	remove  bool
	wimPath string // slash-separated path within the image
	content []byte // for add/replace
}

// OpenForUpdate opens the WIM at path for read-write catalog and file editing.
func OpenForUpdate(path string) (*Updater, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0) //nolint:gosec // caller-provided path
	if err != nil {
		return nil, fmt.Errorf("wim: open for update %s: %w", path, err)
	}
	w, err := OpenReaderAt(f, mustSize(f))
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	recs := make([]imageRec, len(w.images))
	for i, im := range w.images {
		recs[i] = imageRec{
			name:        im.Name,
			description: im.Description,
			displayName: im.DisplayName,
			flags:       im.Flags,
			windowsXML:  im.WindowsXML,
			dirCount:    im.DirCount,
			fileCount:   im.FileCount,
			totalBytes:  im.TotalBytes,
		}
	}
	return &Updater{f: f, w: w, hdr: w.hdr, recs: recs, ops: map[int][]fileOp{}}, nil
}

// AddFile schedules creating or replacing the file at wimPath (slash-separated,
// relative to the image root) in the given 1-based image with content. Parent
// directories are created as needed. The change is applied by Commit.
func (u *Updater) AddFile(image int, wimPath string, content []byte) error {
	if image < 1 || image > len(u.recs) {
		return fmt.Errorf("wim: image %d out of range [1, %d]", image, len(u.recs))
	}
	clean := strings.Trim(strings.ReplaceAll(wimPath, "\\", "/"), "/")
	if strings.TrimSpace(clean) == "" {
		return fmt.Errorf("wim: empty file path")
	}
	u.ops[image] = append(u.ops[image], fileOp{wimPath: clean, content: content})
	return nil
}

// RemoveFile schedules deleting the file or directory at wimPath in the given
// 1-based image. Applied by Commit.
func (u *Updater) RemoveFile(image int, wimPath string) error {
	if image < 1 || image > len(u.recs) {
		return fmt.Errorf("wim: image %d out of range [1, %d]", image, len(u.recs))
	}
	clean := strings.Trim(strings.ReplaceAll(wimPath, "\\", "/"), "/")
	if clean == "" {
		return fmt.Errorf("wim: empty file path")
	}
	u.ops[image] = append(u.ops[image], fileOp{remove: true, wimPath: clean})
	return nil
}

// Close releases the underlying file. Call it whether or not Commit succeeded.
func (u *Updater) Close() error {
	if u.f == nil {
		return nil
	}
	err := u.f.Close()
	u.f = nil
	return err
}

// Images returns the current (possibly edited) catalog.
func (u *Updater) Images() []ImageInfo {
	var sb strings.Builder
	sb.WriteString("<WIM>")
	for i, im := range u.recs {
		sb.WriteString(imageXMLElement(i+1, im))
	}
	sb.WriteString("</WIM>")
	imgs, err := parseCatalog(sb.String())
	if err != nil {
		return nil
	}
	return imgs
}

// SetProperty sets an image property by key. Supported keys are the top-level
// catalog fields NAME, DESCRIPTION, FLAGS, DISPLAYNAME, and single-level
// children of <WINDOWS> addressed as "WINDOWS/<TAG>" (e.g.
// "WINDOWS/INSTALLATIONTYPE"). image is 1-based.
func (u *Updater) SetProperty(image int, key, value string) error {
	if image < 1 || image > len(u.recs) {
		return fmt.Errorf("wim: image %d out of range [1, %d]", image, len(u.recs))
	}
	rec := &u.recs[image-1]
	switch {
	case strings.HasPrefix(key, "WINDOWS/"):
		tag := strings.TrimPrefix(key, "WINDOWS/")
		if tag == "" || strings.ContainsRune(tag, '/') {
			return fmt.Errorf("wim: unsupported nested property key %q", key)
		}
		rec.windowsXML = setWindowsChild(rec.windowsXML, tag, value)
	case key == "NAME":
		rec.name = value
	case key == "DESCRIPTION":
		rec.description = value
	case key == "FLAGS":
		rec.flags = value
	case key == "DISPLAYNAME":
		rec.displayName = value
	default:
		return fmt.Errorf("wim: unsupported property key %q", key)
	}
	return nil
}

// SetPropertyAll sets key=value on every image. This is how the Win11 bypass
// applies WINDOWS/INSTALLATIONTYPE=Server across all editions in one call.
func (u *Updater) SetPropertyAll(key, value string) error {
	for i := range u.recs {
		if err := u.SetProperty(i+1, key, value); err != nil {
			return err
		}
	}
	return nil
}

// Commit writes the edited catalog back to the file. The new XML resource is
// appended at end-of-file and the header's XMLData descriptor is repointed at
// it (append-overwrite, like wimlib); the old catalog bytes become unreferenced
// slack. Any integrity table is dropped, since appended data invalidates it.
func (u *Updater) Commit() error {
	if u.f == nil {
		return fmt.Errorf("wim: Commit on closed Updater")
	}
	if len(u.ops) > 0 {
		return u.commitWithFileOps()
	}
	xmlBytes := buildXML(u.recs) // UTF-16LE + BOM, uncompressed
	end, err := u.f.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("wim: seek end: %w", err)
	}
	if _, err := u.f.WriteAt(xmlBytes, end); err != nil {
		return fmt.Errorf("wim: write catalog: %w", err)
	}
	u.hdr.XMLData = resourceDescriptor{
		Offset:         end,
		CompressedSize: int64(len(xmlBytes)),
		OriginalSize:   int64(len(xmlBytes)),
		Flags:          0,
	}
	u.hdr.Integrity = resourceDescriptor{} // stale after append; drop it
	if _, err := u.f.WriteAt(serializeHeader(u.hdr), 0); err != nil {
		return fmt.Errorf("wim: write header: %w", err)
	}
	if err := u.f.Sync(); err != nil {
		return fmt.Errorf("wim: sync: %w", err)
	}
	return nil
}

// setWindowsChild sets a single-level text child element of the <WINDOWS>
// fragment: it replaces the existing <TAG>…</TAG> if present, inserts one right
// after <WINDOWS> otherwise, and creates the whole fragment when it is empty.
// value is XML-escaped.
func setWindowsChild(fragment, tag, value string) string {
	el := "<" + tag + ">" + xmlEscape(value) + "</" + tag + ">"
	if strings.TrimSpace(fragment) == "" {
		return "<WINDOWS>" + el + "</WINDOWS>"
	}
	re := regexp.MustCompile(`(?s)<` + regexp.QuoteMeta(tag) + `>.*?</` + regexp.QuoteMeta(tag) + `>`)
	if re.MatchString(fragment) {
		return re.ReplaceAllLiteralString(fragment, el)
	}
	return strings.Replace(fragment, "<WINDOWS>", "<WINDOWS>"+el, 1)
}
