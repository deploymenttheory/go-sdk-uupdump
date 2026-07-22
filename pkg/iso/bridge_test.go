package iso_test

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/deploymenttheory/go-sdk-winmediafoundry/pkg/iso"
	"github.com/deploymenttheory/go-sdk-winmediafoundry/pkg/udf"
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

func TestBuildWindowsUDFBridge(t *testing.T) {
	root := t.TempDir()
	biosContent := bytes.Repeat([]byte("ETFSBOOT"), 256) // 2 KiB
	uefiContent := bytes.Repeat([]byte("EFISYSXX"), 512) // 4 KiB
	install := bytes.Repeat([]byte{0x77}, 9000)          // multi-sector payload
	writeFile(t, root, "boot/etfsboot.com", biosContent)
	writeFile(t, root, "efi/microsoft/boot/efisys.bin", uefiContent)
	writeFile(t, root, "sources/install.wim", install)
	writeFile(t, root, "setup.exe", []byte("setup"))

	out := filepath.Join(t.TempDir(), "win.iso")
	if err := iso.BuildWindowsUDF(root, out, "WIN_TEST"); err != nil {
		t.Fatalf("BuildWindowsUDF: %v", err)
	}

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}

	// El Torito boot record descriptor.
	if !bytes.Contains(raw, []byte("EL TORITO SPECIFICATION")) {
		t.Error("missing El Torito boot record")
	}

	// UDF content reads back through the strict reader (anchor at sector 256).
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	vol, err := udf.Read(f)
	if err != nil {
		t.Fatalf("udf.Read: %v", err)
	}
	got, err := vol.ReadFile([]string{"sources", "install.wim"})
	if err != nil {
		t.Fatalf("read install.wim via UDF: %v", err)
	}
	if !bytes.Equal(got, install) {
		t.Errorf("install.wim mismatch via UDF (%d bytes)", len(got))
	}

	// Boot catalog: validation entry signature, and load addresses that actually
	// contain the boot images.
	cat := raw[bootCatalogOffset : bootCatalogOffset+2048]
	if cat[0] != 0x01 || cat[30] != 0x55 || cat[31] != 0xAA {
		t.Fatalf("bad validation entry: %x %x %x", cat[0], cat[30], cat[31])
	}
	var sum uint16
	for i := 0; i < 32; i += 2 {
		sum += binary.LittleEndian.Uint16(cat[i:])
	}
	if sum != 0 {
		t.Errorf("validation checksum nonzero: %d", sum)
	}

	// Default entry (BIOS) at offset 32, section entry (UEFI) at 96.
	checkEntry(t, raw, cat[32:64], biosContent, "BIOS")
	checkEntry(t, raw, cat[96:128], uefiContent, "UEFI")

	// The BIOS entry records the fixed oscdimg load size of 8 virtual sectors,
	// independent of etfsboot.com's actual 2 KiB length (which would otherwise
	// derive 4). The UEFI entry must cover the whole efisys.bin.
	if got := binary.LittleEndian.Uint16(cat[32+6:]); got != 8 {
		t.Errorf("BIOS sector count = %d, want 8 (biosBootLoadSize)", got)
	}
	if got, want := binary.LittleEndian.Uint16(cat[96+6:]), uint16((len(uefiContent)+511)/512); got != want {
		t.Errorf("UEFI sector count = %d, want %d", got, want)
	}
}

const bootCatalogOffset = 22 * 2048

func checkEntry(t *testing.T, raw, entry, want []byte, label string) {
	t.Helper()
	if entry[0] != 0x88 {
		t.Errorf("%s entry not bootable: %x", label, entry[0])
	}
	rba := binary.LittleEndian.Uint32(entry[8:])
	at := int(rba) * 2048
	if at+len(want) > len(raw) {
		t.Fatalf("%s load RBA %d out of range", label, rba)
	}
	if !bytes.Equal(raw[at:at+len(want)], want) {
		t.Errorf("%s boot image not found at load RBA %d", label, rba)
	}
}

