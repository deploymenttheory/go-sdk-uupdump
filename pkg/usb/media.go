package usb

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/deploymenttheory/go-sdk-winmediafoundry/pkg/wim"
)

// mediaPlan is the portable, side-effect-free decision of how a media tree will
// be written: which files to copy, whether install.wim needs splitting, and
// whether an EFI boot loader must be synthesized. It is computed by planMedia
// and can be reported for a dry run or carried out by execute.
type mediaPlan struct {
	mediaRoot    string
	files        []plannedFile // regular files to copy verbatim
	splitWIM     string        // absolute path of install.wim to split, or ""
	splitWIMSize int64         // size of the install.wim being split (bytes)
	efiFallback  bool          // synthesize efi/boot/boot<arch>.efi
	efiArch      string        // "x64" | "arm64" | "ia32" — boot file suffix
	fs           FS
}

// totalBytes is the approximate space the written media occupies: every copied
// file plus the install.wim (whether copied whole or emitted as an .swm set of
// roughly the same total size).
func (p *mediaPlan) totalBytes() int64 {
	var t int64
	for _, f := range p.files {
		t += f.size
	}
	return t + p.splitWIMSize
}

type plannedFile struct {
	rel  string // slash-relative path under the media root
	size int64
}

// Plan summarizes what WriteMedia would do, for a dry run.
type Plan struct {
	Scheme          Scheme
	FS              FS
	FileCount       int
	TotalBytes      int64
	SplitInstallWIM bool
	EFIFallback     bool
	EFIArch         string
}

// DryRun computes the write plan for mediaRoot under opts without touching any
// disk, so a caller can preview the partition scheme, file count, and whether
// install.wim will be split or an EFI loader synthesized.
func DryRun(mediaRoot string, opts Options) (*Plan, error) {
	opts = opts.withDefaults()
	mp, err := planMedia(mediaRoot, opts)
	if err != nil {
		return nil, err
	}
	return &Plan{
		Scheme:          opts.Scheme,
		FS:              opts.FS,
		FileCount:       len(mp.files),
		TotalBytes:      mp.totalBytes(),
		SplitInstallWIM: mp.splitWIM != "",
		EFIFallback:     mp.efiFallback,
		EFIArch:         mp.efiArch,
	}, nil
}

// planMedia walks mediaRoot and decides how it will be written under opts,
// returning an error for a layout that cannot be written (e.g. an un-splittable
// file exceeding the FAT32 limit).
func planMedia(mediaRoot string, opts Options) (*mediaPlan, error) {
	opts = opts.withDefaults()
	index, err := indexTree(mediaRoot)
	if err != nil {
		return nil, err
	}

	p := &mediaPlan{mediaRoot: mediaRoot, fs: opts.FS}
	installWIMRel := index.paths["sources/install.wim"]

	for rel, size := range index.sizes {
		// install.wim over the FAT32 limit is written as a split .swm set instead
		// of being copied whole.
		if rel == installWIMRel && opts.FS == FAT32 && size > fat32MaxFileSize {
			p.splitWIM = filepath.Join(mediaRoot, filepath.FromSlash(rel))
			p.splitWIMSize = size
			continue
		}
		if opts.FS == FAT32 && size > fat32MaxFileSize {
			return nil, fmt.Errorf("usb: %s is %d bytes, over the FAT32 4 GiB limit and cannot be split; use exFAT", rel, size)
		}
		p.files = append(p.files, plannedFile{rel: rel, size: size})
	}

	// EFI boot loader fallback: if the media ships none, one must be extracted.
	hasEFILoader := index.paths["efi/boot/bootx64.efi"] != "" ||
		index.paths["efi/boot/bootaa64.efi"] != "" ||
		index.paths["efi/boot/bootia32.efi"] != ""
	if !hasEFILoader {
		arch, err := installArch(mediaRoot, index)
		if err != nil {
			return nil, fmt.Errorf("usb: media has no EFI boot loader and none could be synthesized: %w", err)
		}
		p.efiFallback = true
		p.efiArch = arch
	}
	return p, nil
}

// execute carries out the plan against the mounted destination volume.
func (p *mediaPlan) execute(ctx context.Context, mount string, opts Options) error {
	for _, f := range p.files {
		if err := ctx.Err(); err != nil {
			return err
		}
		src := filepath.Join(p.mediaRoot, filepath.FromSlash(f.rel))
		dst := filepath.Join(mount, filepath.FromSlash(f.rel))
		if err := copyFile(src, dst); err != nil {
			return err
		}
	}

	if p.splitWIM != "" {
		progressf(opts.Progress, "Splitting install.wim into install.swm set (FAT32)...\n")
		outDir := filepath.Join(mount, "sources")
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			return fmt.Errorf("usb: %w", err)
		}
		if _, err := wim.Split(p.splitWIM, outDir, "install", swmSliceSize); err != nil {
			return err
		}
	}

	if p.efiFallback {
		progressf(opts.Progress, "Synthesizing efi/boot/boot%s.efi from install image...\n", p.efiArch)
		if err := writeEFIFallback(p.mediaRoot, mount, p.efiArch); err != nil {
			return err
		}
	}
	return nil
}

// installArch returns the boot-file architecture suffix ("x64" or "arm64") of
// the install image, used both to name the EFI fallback loader and to pick the
// matching image to extract it from. Windows 11 media is x64 or ARM64 only, so
// an absent or x86 ARCH (as a metadata-less WIM reports) defaults to x64 rather
// than the effectively-extinct 32-bit ia32 loader.
func installArch(mediaRoot string, index treeIndex) (string, error) {
	wimRel := index.paths["sources/install.wim"]
	if wimRel == "" {
		wimRel = index.paths["sources/boot.wim"]
	}
	if wimRel == "" {
		return "", fmt.Errorf("no install.wim or boot.wim to read architecture from")
	}
	w, err := wim.Open(filepath.Join(mediaRoot, filepath.FromSlash(wimRel)))
	if err != nil {
		return "", err
	}
	defer w.Close()
	for _, im := range w.Images() {
		if im.Architecture == "arm64" {
			return "arm64", nil
		}
	}
	return "x64", nil
}

// writeEFIFallback extracts /Windows/Boot/EFI/bootmgfw.efi from the first
// matching-architecture image of install.wim and writes it as
// efi/boot/boot<arch>.efi on the destination.
func writeEFIFallback(mediaRoot, mount, arch string) error {
	wimPath := filepath.Join(mediaRoot, "sources", "install.wim")
	data, err := extractWIMFile(wimPath, "windows/boot/efi/bootmgfw.efi")
	if err != nil {
		return fmt.Errorf("usb: extract bootmgfw.efi: %w", err)
	}
	dst := filepath.Join(mount, "efi", "boot", "boot"+arch+".efi")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("usb: %w", err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil { //nolint:gosec // boot loader
		return fmt.Errorf("usb: write %s: %w", dst, err)
	}
	return nil
}

// extractWIMFile returns the bytes of wimRelPath (slash-separated,
// case-insensitive) from the first image of the WIM at wimPath that contains it.
func extractWIMFile(wimPath, wimRelPath string) ([]byte, error) {
	w, err := wim.Open(wimPath)
	if err != nil {
		return nil, err
	}
	defer w.Close()
	want := strings.ToLower(wimRelPath)
	for i := 1; i <= w.ImageCount(); i++ {
		root, err := w.OpenImage(i)
		if err != nil {
			return nil, err
		}
		var found *wim.File
		root.Walk(func(path string, f *wim.File) {
			if !f.IsDir() && strings.ToLower(path) == want {
				found = f
			}
		})
		if found != nil {
			return w.ReadFile(found)
		}
	}
	return nil, fmt.Errorf("%s not found in any image of %s", wimRelPath, wimPath)
}

// copyFile streams src to dst, creating parent directories.
func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("usb: %w", err)
	}
	in, err := os.Open(src) //nolint:gosec // media tree file
	if err != nil {
		return fmt.Errorf("usb: open %s: %w", src, err)
	}
	defer in.Close()
	out, err := os.Create(dst) //nolint:gosec // destination volume
	if err != nil {
		return fmt.Errorf("usb: create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("usb: copy %s: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("usb: close %s: %w", dst, err)
	}
	return nil
}

// treeIndex maps lowercased slash-relative paths to their on-disk relative path
// (for case-insensitive lookup) and records each file's size.
type treeIndex struct {
	paths map[string]string
	sizes map[string]int64 // keyed by the actual (case-preserved) relative path
}

// indexTree walks root, recording every file's relative path (case-preserved)
// and size, plus a lowercased lookup map so boot files can be found regardless
// of the casing Microsoft used.
func indexTree(root string) (treeIndex, error) {
	ti := treeIndex{paths: map[string]string{}, sizes: map[string]int64{}}
	err := filepath.WalkDir(root, func(p string, de os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if de.IsDir() {
			return nil
		}
		rel, e := filepath.Rel(root, p)
		if e != nil {
			return e
		}
		rel = filepath.ToSlash(rel)
		ti.paths[strings.ToLower(rel)] = rel
		info, e := de.Info()
		if e != nil {
			return e
		}
		ti.sizes[rel] = info.Size()
		return nil
	})
	if err != nil {
		return treeIndex{}, fmt.Errorf("usb: index %s: %w", root, err)
	}
	return ti, nil
}

func progressf(w io.Writer, format string, args ...any) {
	if w != nil {
		fmt.Fprintf(w, format, args...)
	}
}
