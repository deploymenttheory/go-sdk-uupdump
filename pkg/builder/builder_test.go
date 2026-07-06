package builder_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/deploymenttheory/go-sdk-winmediafoundry/pkg/builder"
	"github.com/deploymenttheory/go-sdk-winmediafoundry/pkg/udf"
	"github.com/deploymenttheory/go-sdk-winmediafoundry/pkg/wim"
)

func writeFile(t *testing.T, root, rel string, content []byte) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

// makeSyntheticESD writes an uncompressed multi-image WIM that mimics an ESD's
// image set, and returns its path.
func makeSyntheticESD(t *testing.T) string {
	t.Helper()

	media := t.TempDir()
	writeFile(t, media, "boot/etfsboot.com", bytes.Repeat([]byte("BIOS"), 512))
	writeFile(t, media, "efi/microsoft/boot/efisys.bin", bytes.Repeat([]byte("UEFI"), 1024))
	writeFile(t, media, "setup.exe", []byte("setup launcher"))
	writeFile(t, media, "sources/lang.ini", []byte("[lang]"))

	pe := t.TempDir()
	writeFile(t, pe, "windows/system32/winpe.txt", []byte("WinPE files"))

	setup := t.TempDir()
	writeFile(t, setup, "windows/system32/setup.txt", []byte("Setup files"))

	edition := t.TempDir()
	writeFile(t, edition, "windows/explorer.exe", bytes.Repeat([]byte("OS"), 4000))
	writeFile(t, edition, "users/readme.txt", []byte("hello"))

	esdPath := filepath.Join(t.TempDir(), "install.esd")
	out, err := os.Create(esdPath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := wim.NewWriter(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, im := range []struct {
		dir, name string
	}{
		{media, "Windows Setup Media"},
		{pe, "Microsoft Windows PE (x64)"},
		{setup, "Microsoft Windows Setup (x64)"},
		{edition, "Windows 11 Pro"},
	} {
		if err := w.AddImage(im.dir, im.name); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out.Close()
	return esdPath
}

// writeWIM writes the given (dir,name) images as an uncompressed WIM.
func writeWIM(t *testing.T, specs [][2]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "x.wim")
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w, err := wim.NewWriter(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range specs {
		if err := w.AddImage(s[0], s[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out.Close()
	return path
}

func mediaDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	writeFile(t, d, "boot/etfsboot.com", []byte("bios"))
	writeFile(t, d, "efi/microsoft/boot/efisys.bin", []byte("uefi"))
	return d
}

func TestBuildISOErrors(t *testing.T) {
	out := filepath.Join(t.TempDir(), "x.iso")

	if err := builder.BuildISO(filepath.Join(t.TempDir(), "nope.esd"), out, builder.Options{}); err == nil {
		t.Error("expected error for missing ESD")
	}

	// No Setup Media image.
	d := t.TempDir()
	writeFile(t, d, "a.txt", []byte("x"))
	noMedia := writeWIM(t, [][2]string{{d, "Windows 11 Pro"}})
	if err := builder.BuildISO(noMedia, out, builder.Options{}); err == nil {
		t.Error("expected error: no Setup Media")
	}

	// Setup Media but no PE/Setup images.
	noBoot := writeWIM(t, [][2]string{{mediaDir(t), "Windows Setup Media"}})
	if err := builder.BuildISO(noBoot, out, builder.Options{}); err == nil {
		t.Error("expected error: no boot images")
	}

	// Unusable WorkDir makes extraction fail.
	pe := t.TempDir()
	writeFile(t, pe, "pe.txt", []byte("pe"))
	good := writeWIM(t, [][2]string{
		{mediaDir(t), "Windows Setup Media"},
		{pe, "Microsoft Windows PE"},
	})
	// A WorkDir path that lives under a regular file cannot be created.
	blocker := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := builder.BuildISO(good, out, builder.Options{WorkDir: filepath.Join(blocker, "work")}); err == nil {
		t.Error("expected error for unusable work dir")
	}

	// Setup Media without the boot images on it: the final master fails.
	bare := t.TempDir()
	writeFile(t, bare, "setup.exe", []byte("x")) // no boot/efi boot files
	noBootFiles := writeWIM(t, [][2]string{
		{bare, "Windows Setup Media"},
		{pe, "Microsoft Windows PE"},
	})
	if err := builder.BuildISO(noBootFiles, out, builder.Options{}); err == nil {
		t.Error("expected error: Setup Media lacks boot images")
	}
}

func TestBuildISOBootOnly(t *testing.T) {
	pe := t.TempDir()
	writeFile(t, pe, "pe.txt", []byte("pe"))
	setup := t.TempDir()
	writeFile(t, setup, "setup.txt", []byte("setup"))

	esd := writeWIM(t, [][2]string{
		{mediaDir(t), "Windows Setup Media"},
		{pe, "Microsoft Windows PE"},
		{setup, "Microsoft Windows Setup"},
	})

	work := t.TempDir() // explicit WorkDir branch
	out := filepath.Join(t.TempDir(), "boot.iso")
	if err := builder.BuildISO(esd, out, builder.Options{VolumeID: "BOOTONLY", WorkDir: work}); err != nil {
		t.Fatalf("BuildISO: %v", err)
	}

	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	vol, err := udf.Read(f)
	if err != nil {
		t.Fatalf("udf.Read: %v", err)
	}
	if readUDFFile(t, vol, []string{"sources", "boot.wim"}) == nil {
		t.Error("boot.wim missing")
	}
	if readUDFFile(t, vol, []string{"sources", "install.wim"}) != nil {
		t.Error("install.wim should be absent for boot-only media")
	}
}

func TestBuildISOEndToEnd(t *testing.T) {
	esd := makeSyntheticESD(t)
	outISO := filepath.Join(t.TempDir(), "win.iso")

	if err := builder.BuildISO(esd, outISO, builder.Options{VolumeID: "WIN_E2E"}); err != nil {
		t.Fatalf("BuildISO: %v", err)
	}

	raw, err := os.ReadFile(outISO)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("EL TORITO SPECIFICATION")) {
		t.Error("output ISO is not El Torito bootable")
	}

	// The UDF file system must contain sources/boot.wim and sources/install.wim.
	f, err := os.Open(outISO)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	vol, err := udf.Read(f)
	if err != nil {
		t.Fatalf("udf.Read: %v", err)
	}

	bootWIM := readUDFFile(t, vol, []string{"sources", "boot.wim"})
	installWIM := readUDFFile(t, vol, []string{"sources", "install.wim"})
	if bootWIM == nil || installWIM == nil {
		t.Fatal("boot.wim or install.wim missing from ISO")
	}

	// boot.wim read back out of the ISO must itself be a valid 2-image WIM.
	checkWIMImages(t, bootWIM, 2)
	checkWIMImages(t, installWIM, 1)
}

func checkWIMImages(t *testing.T, data []byte, want int) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "x.wim")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := wim.Open(p)
	if err != nil {
		t.Fatalf("reopen embedded WIM: %v", err)
	}
	defer w.Close()
	if w.ImageCount() != want {
		t.Errorf("embedded WIM image count = %d, want %d", w.ImageCount(), want)
	}
}

// readUDFFile returns the file's bytes via the strict reader, or nil if any path
// component is missing (some callers assert absence).
func readUDFFile(t *testing.T, vol *udf.Volume, parts []string) []byte {
	t.Helper()
	data, err := vol.ReadFile(parts)
	if err != nil {
		return nil
	}
	return data
}
