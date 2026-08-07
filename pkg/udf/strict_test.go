package udf

// Corruption-matrix tests for the strict reader and error-injection tests for the
// writer. Every mutation below models a defect class Windows UDFS rejects at mount
// time; the strict reader must reject them too, or it is no trusted source.

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// memImage is an in-memory io.WriterAt for mastering small test volumes.
type memImage struct{ b []byte }

func (m *memImage) WriteAt(p []byte, off int64) (int, error) {
	if end := int(off) + len(p); end > len(m.b) {
		m.b = append(m.b, make([]byte, end-len(m.b))...)
	}
	copy(m.b[off:], p)
	return len(p), nil
}

// failAtSector discards writes but fails any write touching the given sector.
type failAtSector struct{ sector int64 }

func (f failAtSector) WriteAt(p []byte, off int64) (int, error) {
	lo, hi := f.sector*SectorSize, (f.sector+1)*SectorSize
	if off < hi && off+int64(len(p)) > lo {
		return 0, os.ErrClosed
	}
	return len(p), nil
}

// buildStrictImage masters a small volume: /a.txt, /d/b.txt. Fixed layout
// (partition at 291): FSD@291, TERM@292, root FE@293, root FIDs@294, a.txt
// FE@295 data@296, d FE@297 FIDs@298, b.txt FE@299 data@300, backup anchor@301.
func buildStrictImage(t *testing.T) []byte {
	t.Helper()
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	for p, c := range map[string]string{"a.txt": "hello", "d/b.txt": "world"} {
		if err := os.WriteFile(filepath.Join(src, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var m memImage
	if err := Write(&m, src, "STRICT"); err != nil {
		t.Fatal(err)
	}
	return m.b
}

// refix recomputes a descriptor's tag (checksum + CRC over its recorded CRC
// length) after a test mutation, so the reader hits the targeted check instead
// of failing early on integrity.
func refix(sec []byte, ident uint16, loc uint32) {
	crcLen := int(binary.LittleEndian.Uint16(sec[10:]))
	putTag(sec[:16+crcLen], ident, loc)
}

func sector(img []byte, n int) []byte { return img[n*SectorSize : (n+1)*SectorSize] }

func TestStrictReaderRejectsCorruption(t *testing.T) {
	base := buildStrictImage(t)
	cases := []struct {
		name   string
		mutate func([]byte)
		atRead bool // error expected from Read itself (else from ReadDir(nil))
	}{
		{"anchor checksum", func(img []byte) { img[256*SectorSize+4] ^= 0x5A }, true},
		{"anchor wrong tag", func(img []byte) { refix(sector(img, 256), tagPartition, 256) }, true},
		{"MVDS descriptor CRC", func(img []byte) { img[257*SectorSize+100] ^= 0xFF }, true},
		{"missing partition descriptor", func(img []byte) { clear(sector(img, 259)) }, true},
		{"bad logical block size", func(img []byte) {
			s := sector(img, 260)
			binary.LittleEndian.PutUint32(s[212:], 1024)
			refix(s, tagLogicalVolume, 260)
		}, true},
		{"bad partition map count", func(img []byte) {
			s := sector(img, 260)
			binary.LittleEndian.PutUint32(s[268:], 2)
			refix(s, tagLogicalVolume, 260)
		}, true},
		{"FSD wrong tag location", func(img []byte) { refix(sector(img, 291), tagFileSet, 7) }, true},
		{"root FE corrupt", func(img []byte) { img[293*SectorSize+50] ^= 0xFF }, false},
		{"unsupported allocation type", func(img []byte) {
			s := sector(img, 293)
			binary.LittleEndian.PutUint16(s[34:], 1) // long_ad
			refix(s, tagFileEntry, 2)
		}, false},
		{"embedded data overrun", func(img []byte) {
			s := sector(img, 293)
			binary.LittleEndian.PutUint16(s[34:], 3) // embedded
			binary.LittleEndian.PutUint64(s[56:], 60000)
			refix(s, tagFileEntry, 2)
		}, false},
		{"bad extended-attribute length", func(img []byte) {
			s := sector(img, 293)
			binary.LittleEndian.PutUint32(s[168:], 0xFFFFF000)
			refix(s, tagFileEntry, 2)
		}, false},
		{"directory zero-gap", func(img []byte) { clear(sector(img, 294)) }, false},
		{"FID bad CRC", func(img []byte) { img[294*SectorSize+78] ^= 0xFF }, false},
		{"FID overruns directory", func(img []byte) {
			img[294*SectorSize+40+19] = 200 // second FID's L_FI
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			img := bytes.Clone(base)
			tc.mutate(img)
			vol, err := Read(bytes.NewReader(img))
			if tc.atRead {
				if err == nil {
					t.Fatal("Read accepted a corrupted volume")
				}
				return
			}
			if err != nil {
				t.Fatalf("Read failed early: %v", err)
			}
			if _, err := vol.ReadDir(nil); err == nil {
				t.Fatal("ReadDir accepted a corrupted volume")
			}
		})
	}
}

func TestReadPathErrors(t *testing.T) {
	vol, err := Read(bytes.NewReader(buildStrictImage(t)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vol.ReadDir([]string{"nope"}); err == nil {
		t.Error("ReadDir of missing path succeeded")
	}
	if _, err := vol.ReadFile(nil); err == nil {
		t.Error("ReadFile of empty path succeeded")
	}
	if _, err := vol.ReadFile([]string{"d"}); err == nil {
		t.Error("ReadFile of a directory succeeded")
	}
	if _, err := vol.ReadFile([]string{"zzz"}); err == nil {
		t.Error("ReadFile of missing file succeeded")
	}
	if _, err := vol.ReadFile([]string{"d", "zzz"}); err == nil {
		t.Error("ReadFile of missing nested file succeeded")
	}
	// Positive control: the volume itself is good.
	if got, err := vol.ReadFile([]string{"d", "b.txt"}); err != nil || string(got) != "world" {
		t.Errorf("ReadFile d/b.txt = %q, %v", got, err)
	}
}

func TestValidateTagBranches(t *testing.T) {
	desc := make([]byte, 64)
	for i := 16; i < 64; i++ {
		desc[i] = byte(i)
	}
	putTag(desc[:64], tagFileEntry, 42)
	if err := validateTag(desc, tagFileEntry, 42); err != nil {
		t.Fatalf("valid tag rejected: %v", err)
	}
	if err := validateTag(desc, tagPartition, 42); err == nil {
		t.Error("wrong identifier accepted")
	}
	if err := validateTag(desc, tagFileEntry, 43); err == nil {
		t.Error("wrong location accepted")
	}
	c := bytes.Clone(desc)
	c[4] ^= 1
	if err := validateTag(c, tagFileEntry, 42); err == nil {
		t.Error("bad checksum accepted")
	}
	c = bytes.Clone(desc)
	c[20] ^= 1
	if err := validateTag(c, tagFileEntry, 42); err == nil {
		t.Error("bad CRC accepted")
	}
	// CRC length pointing past the descriptor.
	c = bytes.Clone(desc)
	binary.LittleEndian.PutUint16(c[10:], 0xFFFF)
	var sum uint8
	for i := range 16 {
		if i != 4 {
			sum += c[i]
		}
	}
	c[4] = sum
	if err := checkTagIntegrity(c); err == nil {
		t.Error("overrunning CRC length accepted")
	}
}

func TestParseFIDErrors(t *testing.T) {
	stream := dirFIDStream([]fidEntry{{"", 1, true}, {"x.txt", 2, false}}, 0)
	if _, err := parseFIDs(stream); err != nil {
		t.Fatalf("valid stream rejected: %v", err)
	}
	if _, err := parseFIDs(append(bytes.Clone(stream), make([]byte, 40)...)); err == nil {
		t.Error("zero-gap tail accepted")
	}
}

func TestEncodeDStringTruncation(t *testing.T) {
	b := encodeDString(strings.Repeat("a", 100), 32)
	if b[0] != 8 || int(b[31]) != 31 {
		t.Errorf("8-bit truncation: comp=%d used=%d", b[0], b[31])
	}
	b = encodeDString(strings.Repeat("语", 100), 32)
	if b[0] != 16 || b[31] == 0 || int(b[31]) > 31 {
		t.Errorf("16-bit truncation: comp=%d used=%d", b[0], b[31])
	}
}

// TestWriteFailsAtEverySector drives every write-error return in the writer by
// failing exactly one sector per run — every metadata sector the small image
// writes, per the layout in buildStrictImage's comment.
func TestWriteFailsAtEverySector(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	for p, c := range map[string]string{"a.txt": "hello", "d/b.txt": "world"} {
		if err := os.WriteFile(filepath.Join(src, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []int64{16, 17, 18, 256, 257, 258, 262, 273, 289, 290,
		291, 292, 293, 294, 295, 296, 297, 298, 299, 300, 301} {
		if err := Write(failAtSector{s}, src, "X"); err == nil {
			t.Errorf("write with failing sector %d succeeded", s)
		}
	}
}

func TestWriteUnreadableSource(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits are not enforced")
	}
	// Unreadable file: buildTree stats it fine, streamFile's open fails.
	src := t.TempDir()
	p := filepath.Join(src, "locked.bin")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o644) })
	if err := Write(&memImage{}, src, "X"); err == nil {
		t.Error("unreadable file did not fail the write")
	}

	// Unreadable directory: addChildren's ReadDir fails.
	src2 := t.TempDir()
	d := filepath.Join(src2, "sealed")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(d, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(d, 0o755) })
	if err := Write(&memImage{}, src2, "X"); err == nil {
		t.Error("unreadable directory did not fail the write")
	}
}
