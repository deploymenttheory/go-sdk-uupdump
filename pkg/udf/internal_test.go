package udf

import (
	"encoding/binary"
	"fmt"
	"testing"
)

// TestDirFIDsContiguousNoGap is the regression guard for directory FID packing.
// FIDs must be packed contiguously — a zero-byte gap before a block boundary makes
// strict readers (Windows Setup, macOS) read a zero tag as end-of-directory and hide
// every later entry (e.g. a >4 GiB install.wim). A FID MAY cross a logical-block
// boundary: real Microsoft install media does this, and the earlier "pad to the next
// block" behavior was itself rejected by Windows. This walks the raw FID stream for a
// directory spanning several blocks and asserts: (1) every descriptor is a valid
// FileIdentifier tag with a valid checksum+CRC (no zero-gap pad), (2) at least one FID
// crosses a 2048-byte boundary (contiguous packing), (3) exactly the entries written
// come back.
func TestDirFIDsContiguousNoGap(t *testing.T) {
	le := binary.LittleEndian
	ents := []fidEntry{{"", 1, true}}
	for i := range 300 { // varied-length names → FID list spans many blocks
		ents = append(ents, fidEntry{fmt.Sprintf("entry%03d_padding_name_%d.dat", i, i%7), uint32(100 + i), i%5 == 0})
	}
	ents = append(ents, fidEntry{"install.wim", 99999, false})

	stream := dirFIDStream(ents, 3)
	if len(stream) < 3*SectorSize {
		t.Fatalf("stream too small (%d bytes) to exercise multiple blocks", len(stream))
	}

	count, crossings := 0, 0
	for off := 0; off < len(stream); {
		if ident := le.Uint16(stream[off:]); ident != tagFileIdentifier {
			t.Fatalf("descriptor at offset %d has tag %#x, want FileIdentifier %#x "+
				"(zero-gap pad / misalignment — strict readers stop parsing here)", off, ident, tagFileIdentifier)
		}
		lFI := int(stream[off+19])
		lIU := int(le.Uint16(stream[off+36:]))
		flen := (38 + lIU + lFI + 3) / 4 * 4
		if err := checkTagIntegrity(stream[off : off+flen]); err != nil {
			t.Fatalf("FID #%d at offset %d: %v", count, off, err)
		}
		if off/SectorSize != (off+flen-1)/SectorSize {
			crossings++
		}
		count++
		off += flen
	}
	if count != len(ents) {
		t.Fatalf("recovered %d FIDs, wrote %d", count, len(ents))
	}
	if crossings == 0 {
		t.Fatal("no FID crosses a block boundary — stream is not packed contiguously")
	}
}

// TestTagCRCChecksum verifies that putTag writes a self-consistent ECMA-167 tag:
// the stored checksum and CRC match independent recomputation. The oracle reader
// does not validate these, so this guards Windows compatibility.
func TestTagCRCChecksum(t *testing.T) {
	desc := make([]byte, 64)
	for i := 16; i < 64; i++ {
		desc[i] = byte(i) // arbitrary body
	}
	putTag(desc, tagFileEntry, 42)

	le := binary.LittleEndian
	if got := le.Uint16(desc[0:]); got != tagFileEntry {
		t.Errorf("tag identifier = %d", got)
	}
	if got := le.Uint16(desc[2:]); got != descriptorVersion {
		t.Errorf("descriptor version = %d", got)
	}
	if got := le.Uint32(desc[12:]); got != 42 {
		t.Errorf("tag location = %d", got)
	}

	// CRC over the body must match the stored value and length.
	if gotLen := le.Uint16(desc[10:]); int(gotLen) != len(desc)-16 {
		t.Errorf("CRC length = %d, want %d", gotLen, len(desc)-16)
	}
	if want, got := crcCCITT(desc[16:]), le.Uint16(desc[8:]); want != got {
		t.Errorf("CRC = %#x, want %#x", got, want)
	}

	// Checksum is the sum of tag bytes 0..15 except byte 4.
	var sum uint8
	for i := range 16 {
		if i != 4 {
			sum += desc[i]
		}
	}
	if sum != desc[4] {
		t.Errorf("checksum = %#x, want %#x", desc[4], sum)
	}
}

// TestCRCKnownVector checks crcCCITT against a known CRC-CCITT(0x1021, init 0)
// value for the ASCII string "123456789".
func TestCRCKnownVector(t *testing.T) {
	if got := crcCCITT([]byte("123456789")); got != 0x31C3 {
		t.Errorf("crcCCITT(123456789) = %#x, want 0x31C3", got)
	}
}

// TestFileEntryLargeFileExtents verifies that a regular file larger than a
// single short_ad's reach (boot.wim ~1.5 GiB, install.wim >4 GiB) is described
// by multiple block-aligned extents, with an untruncated 64-bit information
// length and every extent-length's top two bits (the extent type) clear. A
// single overflowing short_ad is exactly what made the Windows boot manager
// unable to read boot.wim.
func TestFileEntryLargeFileExtents(t *testing.T) {
	le := binary.LittleEndian
	w := &imageWriter{}

	cases := []struct {
		name string
		size int64
	}{
		{"bootwim_1.5GiB", 1496060854},
		{"installwim_5GiB", 5 * 1024 * 1024 * 1024},
		{"exactly_maxExtent", maxExtentLen},
		{"one_over_maxExtent", maxExtentLen + 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const startBlock = 100
			n := &node{name: tc.name, size: tc.size, dataLB: startBlock}
			fe := w.fileEntry(n, fileTypeRegular)

			if got := le.Uint64(fe[56:]); got != uint64(tc.size) {
				t.Errorf("information length = %d, want %d (truncated?)", got, tc.size)
			}

			adBytes := le.Uint32(fe[172:])
			wantExtents := (uint64(tc.size) + maxExtentLen - 1) / maxExtentLen
			if uint64(adBytes) != wantExtents*8 {
				t.Fatalf("alloc-desc bytes = %d (%d extents), want %d extents", adBytes, adBytes/8, wantExtents)
			}

			// Allocation descriptors start after the extended-attributes area.
			adStart := 176 + int(le.Uint32(fe[168:]))
			var sumLen, block uint64 = 0, startBlock
			for off := adStart; off < adStart+int(adBytes); off += 8 {
				raw := le.Uint32(fe[off:])
				if raw&0xC0000000 != 0 {
					t.Errorf("extent at %d has non-zero type bits: %#x", off, raw)
				}
				extLen := uint64(raw & 0x3FFFFFFF)
				if loc := uint64(le.Uint32(fe[off+4:])); loc != block {
					t.Errorf("extent location = %d, want %d (non-contiguous)", loc, block)
				}
				block += uint64(blocks(extLen))
				sumLen += extLen
			}
			if sumLen != uint64(tc.size) {
				t.Errorf("sum of extent lengths = %d, want %d", sumLen, tc.size)
			}
		})
	}
}

func TestEncodeDStringUTF16(t *testing.T) {
	// A string with a rune > 0xFF must use 16-bit compression (leading byte 16).
	b := encodeDString("A误", 32)
	if b[0] != 16 {
		t.Errorf("compression id = %d, want 16", b[0])
	}
	if b[31] == 0 {
		t.Error("length byte should be non-zero")
	}
}
