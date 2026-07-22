package usb

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/deploymenttheory/go-sdk-winmediafoundry/pkg/wim"
)

func writeTreeFile(t *testing.T, root, rel string, content []byte) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

// makeMediaTree builds a minimal media tree whose install.wim contains
// /Windows/Boot/EFI/bootmgfw.efi but which ships no efi/boot/boot*.efi loader,
// so the EFI fallback path is exercised.
func makeMediaTree(t *testing.T) (string, []byte) {
	t.Helper()
	media := t.TempDir()
	writeTreeFile(t, media, "boot/etfsboot.com", []byte("bios boot"))
	writeTreeFile(t, media, "efi/microsoft/boot/efisys.bin", []byte("uefi boot"))
	writeTreeFile(t, media, "setup.exe", []byte("setup launcher"))
	writeTreeFile(t, media, "sources/boot.wim", []byte("boot wim stub"))

	// Build install.wim from a tree that includes bootmgfw.efi.
	bootmgfw := bytes.Repeat([]byte("MZ-EFI-LOADER"), 64)
	osTree := t.TempDir()
	writeTreeFile(t, osTree, "Windows/Boot/EFI/bootmgfw.efi", bootmgfw)
	writeTreeFile(t, osTree, "Windows/explorer.exe", []byte("os"))

	installWIM := filepath.Join(media, "sources", "install.wim")
	out, err := os.Create(installWIM)
	if err != nil {
		t.Fatal(err)
	}
	if err := wim.CreateFromDir(out, osTree, "Windows 11 Pro"); err != nil {
		t.Fatal(err)
	}
	out.Close()
	return media, bootmgfw
}

func TestPlanAndExecuteEFIFallback(t *testing.T) {
	media, bootmgfw := makeMediaTree(t)

	plan, err := planMedia(media, Options{FS: FAT32})
	if err != nil {
		t.Fatalf("planMedia: %v", err)
	}
	if plan.splitWIM != "" {
		t.Errorf("small install.wim should not be split, got %q", plan.splitWIM)
	}
	if !plan.efiFallback {
		t.Fatal("expected EFI fallback (no efi/boot/boot*.efi present)")
	}
	if plan.efiArch != "x64" {
		t.Errorf("efiArch = %q, want x64 (default)", plan.efiArch)
	}

	mount := t.TempDir()
	if err := plan.execute(context.Background(), mount, Options{FS: FAT32}); err != nil {
		t.Fatalf("execute: %v", err)
	}

	// Copied file is present.
	if b, err := os.ReadFile(filepath.Join(mount, "setup.exe")); err != nil || string(b) != "setup launcher" {
		t.Errorf("setup.exe not copied: %q err=%v", b, err)
	}
	// EFI fallback loader synthesized with the extracted bytes.
	got, err := os.ReadFile(filepath.Join(mount, "efi", "boot", "bootx64.efi"))
	if err != nil {
		t.Fatalf("bootx64.efi not synthesized: %v", err)
	}
	if !bytes.Equal(got, bootmgfw) {
		t.Error("synthesized bootx64.efi content mismatch")
	}
}

func TestPlanNoFallbackWhenLoaderPresent(t *testing.T) {
	media, _ := makeMediaTree(t)
	// Ship a UEFI loader; fallback should then be unnecessary.
	writeTreeFile(t, media, "efi/boot/bootx64.efi", []byte("existing loader"))

	plan, err := planMedia(media, Options{FS: FAT32})
	if err != nil {
		t.Fatal(err)
	}
	if plan.efiFallback {
		t.Error("EFI fallback should be off when a loader is present")
	}
}

func TestPlanMediaSplitDecision(t *testing.T) {
	if testing.Short() {
		t.Skip("creates a >4 GiB sparse file")
	}
	media := t.TempDir()
	writeTreeFile(t, media, "efi/boot/bootx64.efi", []byte("loader")) // avoid fallback WIM read
	writeTreeFile(t, media, "setup.exe", []byte("x"))

	// Sparse >4 GiB install.wim: planMedia only stats size, so it need not be a
	// valid WIM for the split *decision*.
	big := filepath.Join(media, "sources", "install.wim")
	if err := os.MkdirAll(filepath.Dir(big), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(fat32MaxFileSize + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()

	fat, err := planMedia(media, Options{FS: FAT32})
	if err != nil {
		t.Fatal(err)
	}
	if fat.splitWIM == "" {
		t.Error("FAT32 should split a >4 GiB install.wim")
	}

	ex, err := planMedia(media, Options{FS: ExFAT})
	if err != nil {
		t.Fatal(err)
	}
	if ex.splitWIM != "" {
		t.Error("exFAT should not split install.wim")
	}
}

func TestPlanMediaOversizeNonSplittable(t *testing.T) {
	if testing.Short() {
		t.Skip("creates a >4 GiB sparse file")
	}
	media := t.TempDir()
	writeTreeFile(t, media, "efi/boot/bootx64.efi", []byte("loader"))
	big := filepath.Join(media, "sources", "install.esd") // .esd cannot be split
	if err := os.MkdirAll(filepath.Dir(big), 0o755); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Create(big)
	if err := f.Truncate(fat32MaxFileSize + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if _, err := planMedia(media, Options{FS: FAT32}); err == nil {
		t.Error("expected error for an un-splittable >4 GiB file on FAT32")
	}
}
