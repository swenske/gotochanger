package scsi

import "encoding/binary"

// MAM (Medium Auxiliary Memory) attribute format codes and header layout
// - verified against sg3_utils' sg_read_attr.c/sg_write_attr.c (a real,
// working open-source implementation of both commands, the same
// citation tier vpd.go's SupportedVPDPages/DeviceIdentificationVPD
// already use for sdparm_vpd.c), not reconstructed from memory: each
// attribute entry is a 2-byte big-endian Attribute Identifier, a 1-byte
// format field (bit7 = read-only flag, bits0-1 = format code), a 2-byte
// big-endian Attribute Length (of the value that follows, not including
// this 5-byte header itself), then the value bytes - both READ
// ATTRIBUTE's response and WRITE ATTRIBUTE's own parameter list share
// this exact per-attribute shape, each preceded by its own 4-byte
// overall length header (Available Data Length / Attribute List Length
// respectively).
const (
	mamFormatBinary = 0x0
	mamFormatASCII  = 0x1
	mamFormatText   = 0x2

	mamReadOnlyBit = 0x80 // attribute header byte2, bit7
)

// MAM attribute identifiers this package implements - verified against
// sg3_utils' sg_read_attr.c's own attr_name_arr[] table (which itself
// cross-references the T10 SSC standard's numbering), not reconstructed
// from memory. The full T10 table defines several dozen IDs, most of
// them physical servo/head/environmental diagnostics a simulator has no
// genuine analogue for; this package implements only the subset below,
// each chosen because it maps onto data this project already tracks (or
// a straightforward new Volume field - see library.Volume's own doc
// comment) rather than inventing meaning for an attribute with nothing
// real behind it.
const (
	mamRemainingCapacity   = 0x0000 // binary, 8 bytes, MiB - read-only
	mamMaximumCapacity     = 0x0001 // binary, 8 bytes, MiB - read-only
	mamTapeAlertFlags      = 0x0002 // binary, 8 bytes, one bit per TapeAlert flag number (bit0 = flag 1) - read-only
	mamLoadCount           = 0x0003 // binary, 8 bytes - read-only
	mamVolumeIdentifier    = 0x0008 // ASCII, 32 bytes - read-only
	mamMediumSerialNumber  = 0x0401 // ASCII, 32 bytes - read-only
	mamApplicationVendor   = 0x0800 // ASCII, 8 bytes - mutable
	mamApplicationName     = 0x0801 // ASCII, 32 bytes - mutable
	mamApplicationVersion  = 0x0802 // ASCII, 8 bytes - mutable
	mamUserMediumTextLabel = 0x0803 // text, 160 bytes - mutable
)

// mamAttribute is one parsed/built MAM attribute entry.
type mamAttribute struct {
	id       uint16
	format   uint8
	readOnly bool
	value    []byte
}

// encodeMAMAttribute builds one attribute entry - see this file's own
// doc comment for the byte layout and its source.
func encodeMAMAttribute(a mamAttribute) []byte {
	b := make([]byte, 5+len(a.value))
	binary.BigEndian.PutUint16(b[0:2], a.id)
	b[2] = a.format & 0x03
	if a.readOnly {
		b[2] |= mamReadOnlyBit
	}
	binary.BigEndian.PutUint16(b[3:5], uint16(len(a.value)))
	copy(b[5:], a.value)
	return b
}

// mamBinary8 encodes a binary-format attribute value - always 8 bytes,
// big-endian, for every binary attribute this package implements (real
// SSC allows other binary attribute lengths elsewhere in the full T10
// table; every one of this package's own binary attributes happens to be
// 8 bytes).
func mamBinary8(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}
