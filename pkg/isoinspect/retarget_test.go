package isoinspect

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// buildFixtureISO hand-rolls a minimal ISO9660 + El Torito image carrying two
// candidate EFI boot images — the "prompting" one wired into the catalog and a
// "noprompt" sibling that is just a file — mirroring Windows media's
// efisys.bin / efisys_noprompt.bin. Hand-rolled (no go-diskfs) because the
// diskfs iso9660 workspace mis-sizes nested files on Windows.
func buildFixtureISO(t *testing.T) (isoPath string, nopromptContent []byte) {
	t.Helper()
	const (
		pvdSector      = 16
		bootRecSector  = 17
		termSector     = 18
		catalogSector  = 19
		rootDirSector  = 20
		efiDirSector   = 21
		msDirSector    = 22
		bootDirSector  = 23
		promptSector   = 24
		nopromptSector = 25
		totalSectors   = 28
	)
	le := binary.LittleEndian
	img := make([]byte, totalSectors*sectorSize)
	sec := func(n int) []byte { return img[n*sectorSize : (n+1)*sectorSize] }

	// dirRecord writes one ISO9660 directory record.
	dirRecord := func(name string, extent, size uint32, isDir bool) []byte {
		n := []byte(name)
		recLen := 33 + len(n)
		if recLen%2 == 1 {
			recLen++ // records are padded to even length
		}
		r := make([]byte, recLen)
		r[0] = byte(recLen)
		le.PutUint32(r[2:], extent)
		le.PutUint32(r[10:], size)
		if isDir {
			r[25] = 0x02
		}
		r[32] = byte(len(n))
		copy(r[33:], n)
		return r
	}
	fillDir := func(sector int, records ...[]byte) {
		b := sec(sector)
		off := 0
		// "." and ".." entries.
		for _, special := range []byte{0x00, 0x01} {
			r := dirRecord(string([]byte{special}), uint32(sector), sectorSize, true)
			copy(b[off:], r)
			off += int(r[0])
		}
		for _, r := range records {
			copy(b[off:], r)
			off += int(r[0])
		}
	}

	// Primary volume descriptor with the root directory record at 156.
	pvd := sec(pvdSector)
	pvd[0] = 1
	copy(pvd[1:6], "CD001")
	le.PutUint32(pvd[80:], totalSectors)
	root := dirRecord("\x00", rootDirSector, sectorSize, true)
	copy(pvd[156:], root[:34])

	// El Torito boot record descriptor pointing at the catalog.
	br := sec(bootRecSector)
	br[0] = 0
	copy(br[1:6], "CD001")
	copy(br[7:], "EL TORITO SPECIFICATION")
	le.PutUint32(br[71:], catalogSector)

	// Set terminator.
	term := sec(termSector)
	term[0] = 255
	copy(term[1:6], "CD001")

	// Boot catalog: UEFI validation entry + default entry → the prompting image.
	prompting := bytes.Repeat([]byte{0xAA}, 2048)
	nopromptContent = bytes.Repeat([]byte{0xBB}, 3000) // deliberately not sector-aligned
	cat := sec(catalogSector)
	cat[0] = 0x01
	cat[1] = platformUEFI
	cat[30] = 0x55
	cat[31] = 0xAA
	var sum uint16
	for i := 0; i < 32; i += 2 {
		if i == 28 {
			continue
		}
		sum += le.Uint16(cat[i:])
	}
	le.PutUint16(cat[28:], -sum)
	cat[32] = 0x88
	le.PutUint16(cat[32+6:], uint16(len(prompting)/512))
	le.PutUint32(cat[32+8:], promptSector)

	// Directory tree: /EFI/MICROSOFT/BOOT/{EFISYS.BIN, EFISYS_NOPROMPT.BIN}.
	fillDir(rootDirSector, dirRecord("EFI", efiDirSector, sectorSize, true))
	fillDir(efiDirSector, dirRecord("MICROSOFT", msDirSector, sectorSize, true))
	fillDir(msDirSector, dirRecord("BOOT", bootDirSector, sectorSize, true))
	fillDir(bootDirSector,
		dirRecord("EFISYS.BIN;1", promptSector, uint32(len(prompting)), false),
		dirRecord("EFISYS_NOPROMPT.BIN;1", nopromptSector, uint32(len(nopromptContent)), false))
	copy(sec(promptSector), prompting)
	copy(sec(nopromptSector), nopromptContent)
	// (3000 bytes spill into the next sector; totalSectors leaves room.)
	copy(img[nopromptSector*sectorSize:], nopromptContent)

	isoPath = filepath.Join(t.TempDir(), "media.iso")
	if err := os.WriteFile(isoPath, img, 0o644); err != nil {
		t.Fatal(err)
	}
	return isoPath, nopromptContent
}

func TestRetargetElToritoUEFI(t *testing.T) {
	isoPath, noprompt := buildFixtureISO(t)

	before, err := Inspect(isoPath)
	if err != nil {
		t.Fatal(err)
	}
	if before.ElTorito == nil || before.ElTorito.UEFI == nil {
		t.Fatalf("fixture ISO has no UEFI boot entry: %+v", before.ElTorito)
	}
	origRBA := before.ElTorito.UEFI.LoadRBA

	// A missing boot image errors and leaves the catalog untouched.
	if err := RetargetElToritoUEFI(isoPath, "efi/microsoft/boot/absent.bin"); err == nil {
		t.Fatal("expected an error for a missing boot image")
	}

	if err := RetargetElToritoUEFI(isoPath, "efi/microsoft/boot/efisys_noprompt.bin"); err != nil {
		t.Fatal(err)
	}

	after, err := Inspect(isoPath)
	if err != nil {
		t.Fatal(err)
	}
	uefi := after.ElTorito.UEFI
	if uefi == nil {
		t.Fatal("retargeted ISO lost its UEFI entry")
	}
	if uefi.LoadRBA == origRBA {
		t.Fatalf("LoadRBA unchanged (%d) — catalog not retargeted", origRBA)
	}
	wantSectors := uint16((len(noprompt) + 511) / 512)
	if uefi.SectorCount != wantSectors {
		t.Fatalf("SectorCount = %d, want %d", uefi.SectorCount, wantSectors)
	}
	if !after.ElTorito.ValidationChecksumOK {
		t.Fatal("validation entry checksum broken by rewrite")
	}

	// The boot image the firmware would load is byte-for-byte the noprompt file.
	extracted := filepath.Join(t.TempDir(), "boot.img")
	if err := ExtractElToritoEFIImage(isoPath, extracted); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(extracted)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got[:len(noprompt)], noprompt) {
		t.Fatal("extracted boot image does not match the noprompt file content")
	}
}
