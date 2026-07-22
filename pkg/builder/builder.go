// Package builder orchestrates the full ESD→ISO pipeline: it reads a Windows
// ESD/WIM, extracts the Setup Media skeleton, rebuilds sources/boot.wim and
// sources/install.wim from the ESD's images, and masters a bootable UDF +
// El Torito ISO.
//
// Because the output media uses UDF (no ISO9660 4 GiB-per-file limit), the WIMs
// are written uncompressed; the resulting ISO is therefore larger than a
// Microsoft-produced one, but valid and bootable.
package builder

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/deploymenttheory/go-sdk-winmediafoundry/pkg/diskspace"
	"github.com/deploymenttheory/go-sdk-winmediafoundry/pkg/iso"
	"github.com/deploymenttheory/go-sdk-winmediafoundry/pkg/progress_counter"
	"github.com/deploymenttheory/go-sdk-winmediafoundry/pkg/unattend"
	"github.com/deploymenttheory/go-sdk-winmediafoundry/pkg/wim"
)

// Options configures an ISO build.
type Options struct {
	VolumeID string
	// WorkDir is the scratch directory for the media tree and temporary
	// extractions. Empty uses a fresh os.MkdirTemp directory (removed on success).
	WorkDir string
	// Progress, when non-nil, receives a terminal progress bar for the slow
	// phases (rebuilding boot.wim / install.wim). nil builds silently.
	Progress io.Writer
	// ExtraFiles maps ISO-relative, slash-separated paths to file content, staged
	// into the media tree just before mastering. A file mapped to "autounattend.xml"
	// lands at the ISO root, where Windows Setup auto-detects it for an unattended
	// install. Keys are anchored and cleaned so they cannot escape the media root;
	// intermediate directories are created as needed.
	ExtraFiles map[string][]byte
	// Bypass selects Windows 11 requirement-bypass mechanisms to apply while
	// building the media. The zero value applies none.
	Bypass Win11Bypass
	// SkipSpaceCheck disables the pre-flight free-disk-space check.
	SkipSpaceCheck bool
}

// Win11Bypass selects Windows 11 requirement-bypass mechanisms.
type Win11Bypass struct {
	// InstallationTypeServer sets WINDOWS/INSTALLATIONTYPE=Server on every image
	// in install.wim, making Windows Setup skip the entire hardware appraisal
	// (TPM/vTPM, Secure Boot, RAM, CPU) in one edit. This is the WinDiskWriter
	// mechanism and needs no autounattend.
	InstallationTypeServer bool
	// LabConfig writes an autounattend.xml at the media root carrying the
	// LabConfig registry keys that disable each Windows 11 check individually.
	// A user-supplied autounattend.xml in ExtraFiles is respected and not
	// overwritten (InstallationTypeServer still applies the bypass regardless).
	LabConfig bool
	// BypassNRO adds the OOBE\BypassNRO key to the generated autounattend.xml so
	// first-boot OOBE does not require a network / Microsoft account.
	BypassNRO bool
}

// Any reports whether any bypass mechanism is selected.
func (b Win11Bypass) Any() bool {
	return b.InstallationTypeServer || b.LabConfig || b.BypassNRO
}

// needsAutounattend reports whether a generated autounattend.xml is required.
func (b Win11Bypass) needsAutounattend() bool { return b.LabConfig || b.BypassNRO }

// BuildISO assembles a bootable Windows ISO at outISOPath from the ESD/WIM at
// esdPath.
func BuildISO(esdPath, outISOPath string, opts Options) error {
	if !opts.SkipSpaceCheck {
		if err := preflightSpace(esdPath, outISOPath, opts); err != nil {
			return err
		}
	}
	w, err := wim.Open(esdPath)
	if err != nil {
		return err
	}
	defer w.Close()
	return BuildISOFromWIM(w, outISOPath, opts)
}

// preflightSpace fails fast when the work directory or the output volume lacks
// room for the build. The rebuilt boot.wim + install.wim are, after LZX
// recompression, roughly the size of the source ESD, and the ISO wraps them; the
// source file size is therefore a sound estimate. Factors add headroom for the
// extracted setup-media skeleton and filesystem overhead.
func preflightSpace(esdPath, outISOPath string, opts Options) error {
	info, err := os.Stat(esdPath)
	if err != nil {
		return fmt.Errorf("builder: %w", err)
	}
	src := uint64(info.Size())

	workParent := opts.WorkDir
	if workParent == "" {
		workParent = os.TempDir()
	}
	outDir := filepath.Dir(outISOPath)

	// The work dir holds the rebuilt WIMs plus the extracted media; the output
	// volume holds the finished ISO. When both live on the same volume (the common
	// case — e.g. a temp workdir and an output file both on C:), the two
	// requirements are simultaneous, so they must be summed against that single
	// volume's free space. Checking them independently would wrongly pass when
	// each fits alone but their sum does not.
	workNeed := src * 3 / 2
	outNeed := src * 13 / 10
	if diskspace.SameVolume(workParent, outDir) {
		if err := diskspace.EnsureAvailable(workParent, workNeed+outNeed); err != nil {
			return fmt.Errorf("builder: work dir and output share a volume: %w; point --workdir at a volume with more free space", err)
		}
		return nil
	}
	if err := diskspace.EnsureAvailable(workParent, workNeed); err != nil {
		return fmt.Errorf("builder: work directory: %w", err)
	}
	if err := diskspace.EnsureAvailable(outDir, outNeed); err != nil {
		return fmt.Errorf("builder: output: %w", err)
	}
	return nil
}

// imageClasses groups an ESD's images by role.
type imageClasses struct {
	setupMedia int
	bootImages []int // Windows PE, then Windows Setup
	editions   []int
}

// classify assigns each image a role from its catalog name.
func classify(images []wim.ImageInfo) imageClasses {
	var c imageClasses
	for _, im := range images {
		name := strings.ToLower(im.Name)
		switch {
		case strings.Contains(name, "setup media"):
			c.setupMedia = im.Index
		case strings.Contains(name, "windows pe"):
			c.bootImages = append(c.bootImages, im.Index)
		case strings.Contains(name, "windows setup"):
			c.bootImages = append(c.bootImages, im.Index)
		default:
			c.editions = append(c.editions, im.Index)
		}
	}
	return c
}

// BuildISOFromWIM assembles the ISO from an already-opened ESD/WIM.
func BuildISOFromWIM(w *wim.WIM, outISOPath string, opts Options) error {
	classes := classify(w.Images())
	if classes.setupMedia == 0 {
		return fmt.Errorf("builder: no \"Windows Setup Media\" image found")
	}
	if len(classes.bootImages) == 0 {
		return fmt.Errorf("builder: no Windows PE/Setup images found for boot.wim")
	}

	work := opts.WorkDir
	if work == "" {
		var err error
		work, err = os.MkdirTemp("", "windowsuup-iso-")
		if err != nil {
			return fmt.Errorf("builder: workdir: %w", err)
		}
		defer os.RemoveAll(work)
	}

	progressf(opts.Progress, "Extracting setup media...\n")
	media := filepath.Join(work, "media")
	if err := w.ExtractImage(classes.setupMedia, media); err != nil {
		return fmt.Errorf("builder: extract setup media: %w", err)
	}

	sources := filepath.Join(media, "sources")
	if err := os.MkdirAll(sources, 0o755); err != nil {
		return fmt.Errorf("builder: %w", err)
	}

	// boot.wim and install.wim are compressed with LZX to match Microsoft media
	// and satisfy go-winio's WIM reader, which rejects XPRESS-flagged WIMs as
	// unsupported (supportedHdrFlags only includes hdrFlagCompressLzx).
	//
	// The classify step orders bootImages as [Windows PE, Windows Setup].
	// Windows Setup (the last image) is the bootable one, so bootIndex equals
	// len(bootImages). bootmgr reads BootIndex from the WIM header to find the
	// boot image and BootMetadata to locate its metadata resource.
	if err := buildWIM(w, classes.bootImages, filepath.Join(sources, "boot.wim"), wim.CompressionLZX, len(classes.bootImages), opts.Progress); err != nil {
		return fmt.Errorf("builder: boot.wim: %w", err)
	}
	installWIM := filepath.Join(sources, "install.wim")
	if len(classes.editions) > 0 {
		if err := buildWIM(w, classes.editions, installWIM, wim.CompressionLZX, 0, opts.Progress); err != nil {
			return fmt.Errorf("builder: install.wim: %w", err)
		}
		// Mechanism A: edit install.wim so Setup skips the Win11 appraisal. The
		// <WINDOWS> element the writer now preserves is what makes this possible.
		if opts.Bypass.InstallationTypeServer {
			progressf(opts.Progress, "Applying INSTALLATIONTYPE=Server bypass...\n")
			if err := applyServerInstallationType(installWIM); err != nil {
				return fmt.Errorf("builder: install.wim bypass: %w", err)
			}
		}
	}

	// Mechanism B: stage a generated autounattend.xml carrying the LabConfig /
	// BypassNRO registry keys (unless the caller already supplied one).
	extras := opts.ExtraFiles
	if opts.Bypass.needsAutounattend() {
		autoXML, err := unattend.Generate(unattend.Config{
			Architecture: unattendArch(w),
			LabConfig:    opts.Bypass.LabConfig,
			BypassNRO:    opts.Bypass.BypassNRO,
		})
		if err != nil {
			return fmt.Errorf("builder: generate autounattend: %w", err)
		}
		extras = mergeExtras(opts.ExtraFiles, "autounattend.xml", autoXML)
	}

	if err := injectExtraFiles(media, extras); err != nil {
		return fmt.Errorf("builder: inject extra files: %w", err)
	}

	progressf(opts.Progress, "Mastering ISO...\n")
	if err := iso.BuildWindowsUDF(media, outISOPath, opts.VolumeID); err != nil {
		return err
	}
	return nil
}

// applyServerInstallationType sets WINDOWS/INSTALLATIONTYPE=Server on every
// image of the WIM at path (the Win11 hardware-appraisal bypass) and commits.
func applyServerInstallationType(path string) error {
	u, err := wim.OpenForUpdate(path)
	if err != nil {
		return err
	}
	defer u.Close()
	if err := u.SetPropertyAll("WINDOWS/INSTALLATIONTYPE", "Server"); err != nil {
		return err
	}
	return u.Commit()
}

// unattendArch maps the source images' WIM architecture to an autounattend
// processorArchitecture, defaulting to amd64.
func unattendArch(w *wim.WIM) string {
	for _, im := range w.Images() {
		switch im.Architecture {
		case "x64":
			return "amd64"
		case "arm64":
			return "arm64"
		case "x86":
			return "x86"
		}
	}
	return "amd64"
}

// mergeExtras returns a copy of base with key=content added only when key is
// absent, so a caller-supplied file (e.g. their own autounattend.xml) wins.
func mergeExtras(base map[string][]byte, key string, content []byte) map[string][]byte {
	out := make(map[string][]byte, len(base)+1)
	for k, v := range base {
		out[k] = v
	}
	if _, ok := out[key]; !ok {
		out[key] = content
	}
	return out
}

// injectExtraFiles writes opts.ExtraFiles into the staged media tree (rooted at
// mediaRoot) before the ISO is mastered, so the UDF writer that walks the tree
// picks them up. Keys are ISO-relative, slash-separated paths; each is anchored
// at "/" and cleaned, so ".." cannot escape mediaRoot. Intermediate directories
// are created. A nil/empty map is a no-op.
func injectExtraFiles(mediaRoot string, extras map[string][]byte) error {
	rootPrefix := filepath.Clean(mediaRoot) + string(os.PathSeparator)
	for rel, content := range extras {
		clean := filepath.Clean("/" + filepath.FromSlash(rel))
		dst := filepath.Join(mediaRoot, clean)
		if !strings.HasPrefix(dst, rootPrefix) {
			return fmt.Errorf("extra file %q escapes media root", rel)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, content, 0o644); err != nil { //nolint:gosec // staged media files
			return err
		}
	}
	return nil
}

// buildWIM writes the given source images as the images of a new WIM at
// outPath, preserving order. Images are copied directly from the source WIM (no
// extraction to disk), which preserves file attributes/timestamps. bootIndex is
// the 1-based output image number to mark as bootable (0 for none). When
// progress is non-nil, the (slow) write is reported via a progress bar sized to
// the images' uncompressed bytes.
func buildWIM(w *wim.WIM, indices []int, outPath string, comp wim.Compression, bootIndex int, progress io.Writer) error {
	out, err := os.Create(outPath) //nolint:gosec // caller-controlled path
	if err != nil {
		return err
	}
	defer out.Close()

	var dst io.WriteSeeker = out
	if progress != nil {
		bar := progress_counter.NewWithLabel(progress, "Building")
		dst = bar.WriteSeeker(filepath.Base(outPath), out, sumImageBytes(w, indices))
	}

	ww, err := wim.NewWriterCompressed(dst, comp)
	if err != nil {
		return err
	}
	ww.SetBootIndex(bootIndex)
	for _, idx := range indices {
		if err := ww.AddImageFromWIM(w, idx, imageName(w, idx)); err != nil {
			return fmt.Errorf("copy image %d: %w", idx, err)
		}
	}
	return ww.Close()
}

// sumImageBytes totals the uncompressed size of the given images, used as the
// progress-bar denominator. Returns 0 when unknown (the bar then shows bytes +
// speed only).
func sumImageBytes(w *wim.WIM, indices []int) int64 {
	want := make(map[int]bool, len(indices))
	for _, idx := range indices {
		want[idx] = true
	}
	var total int64
	for _, im := range w.Images() {
		if want[im.Index] {
			total += im.TotalBytes
		}
	}
	return total
}

// progressf writes a status line to w when it is non-nil.
func progressf(w io.Writer, format string, args ...any) {
	if w != nil {
		fmt.Fprintf(w, format, args...)
	}
}

func imageName(w *wim.WIM, index int) string {
	for _, im := range w.Images() {
		if im.Index == index {
			return im.Name
		}
	}
	return fmt.Sprintf("Image %d", index)
}
