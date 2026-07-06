// Package udf writes UDF 1.02 (ECMA-167) file systems, the format Windows
// installation ISOs use so that files larger than the ISO9660 4 GiB limit fit.
// It is paired with package iso to master a bootable UDF + El Torito image.
//
// The writer targets the subset of UDF that Windows accepts (validated by the
// strict reader in reader.go): a single non-partitioned logical volume, short
// allocation descriptors, and OSTA CS0 identifiers.
package udf

import "encoding/binary"

// SectorSize is the UDF logical sector / block size used here (and by CD/DVD
// media): 2048 bytes.
const SectorSize = 2048

// Descriptor tag identifiers (ECMA-167 / UDF).
const (
	tagPrimaryVolume        = 0x0001
	tagAnchorVolumePointer  = 0x0002
	tagVolumePointer        = 0x0003
	tagImplementationUseVol = 0x0004
	tagPartition            = 0x0005
	tagLogicalVolume        = 0x0006
	tagUnallocatedSpace     = 0x0007
	tagTerminating          = 0x0008
	tagLogicalVolumeInteg   = 0x0009
	tagFileSet              = 0x0100
	tagFileIdentifier       = 0x0101
	tagAllocationExtent     = 0x0102
	tagFileEntry            = 0x0105
	tagExtendedAttrHeader   = 0x0106
)

// descriptorVersion is 2 for UDF 1.02 (the tag DescriptorVersion field).
const descriptorVersion = 2

// crcCCITT computes the 16-bit CRC used by ECMA-167 descriptor tags:
// polynomial 0x1021, initial value 0, no reflection, no final XOR.
func crcCCITT(b []byte) uint16 {
	var crc uint16
	for _, v := range b {
		crc ^= uint16(v) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// putTag writes the 16-byte descriptor tag at the start of desc, computing the
// CRC over the descriptor body (everything after the tag) and the tag checksum.
// tagLocation is the logical sector/block number the descriptor is stored at.
func putTag(desc []byte, ident uint16, tagLocation uint32) {
	le := binary.LittleEndian
	le.PutUint16(desc[0:], ident)
	le.PutUint16(desc[2:], descriptorVersion)
	desc[4] = 0               // checksum, filled below
	desc[5] = 0               // reserved
	le.PutUint16(desc[6:], 0) // tag serial number (0, like Microsoft and hdiutil media)

	crcLen := uint16(len(desc) - 16)
	le.PutUint16(desc[8:], crcCCITT(desc[16:]))
	le.PutUint16(desc[10:], crcLen)
	le.PutUint32(desc[12:], tagLocation)

	var sum uint8
	for i := range 16 {
		if i == 4 {
			continue
		}
		sum += desc[i]
	}
	desc[4] = sum
}
