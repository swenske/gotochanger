package scsi

import "encoding/binary"

// SCSI Vital Product Data page codes this package implements - requested
// via INQUIRY with EVPD=1 (CDB byte1 bit0) and a page code in CDB byte2.
// See Changer.inquiry/Drive.inquiry for the dispatch, and vpdIdentifier
// in cmd/gotochanger-tcmud/main.go for where the NAA value itself comes
// from.
const (
	vpdPageSupportedPages       = 0x00
	vpdPageDeviceIdentification = 0x83
)

// SupportedVPDPages builds the page 0x00 (Supported VPD Pages) response:
// byte0 = peripheral qualifier+device type (same convention as
// StandardInquiry's own byte0), byte1 = page code (0x00), bytes2-3 =
// big-endian PAGE LENGTH (count of page-code bytes that follow), then
// the list of supported page codes itself. Byte layout verified against
// the same class of source this package already cites elsewhere (a real,
// working VPD parser - sg3_utils' sdparm_vpd.c - not reconstructed from
// memory).
func SupportedVPDPages(deviceType uint8) []byte {
	pages := []byte{vpdPageSupportedPages, vpdPageDeviceIdentification}
	b := make([]byte, 4+len(pages))
	b[0] = deviceType & 0x1F
	b[1] = vpdPageSupportedPages
	binary.BigEndian.PutUint16(b[2:4], uint16(len(pages)))
	copy(b[4:], pages)
	return b
}

// DeviceIdentificationVPD builds the page 0x83 (Device Identification)
// response with exactly one NAA identification descriptor - this is the
// page real udev tooling (scsi_id, via the stock 60-persistent-storage-
// tape.rules every Debian install already ships) queries to build a
// stable /dev/tape/by-id/... symlink, which is the entire point of this
// file existing (see vpdIdentifier's own doc comment for the full
// story). Byte layout verified against sg3_utils' sdparm_vpd.c (a real,
// working parser, not reconstructed from memory):
//
//	byte0-3:  page header (peripheral qualifier+type, page code 0x83,
//	          big-endian PAGE LENGTH = 12, the one descriptor's own size)
//	byte4:    PROTOCOL IDENTIFIER (bits7-4, unused since PIV=0 below) |
//	          CODE SET (bits3-0 = 1, binary)
//	byte5:    PIV (bit7 = 0) | reserved (bit6) | ASSOCIATION (bits5-4 = 0,
//	          logical unit - the simplest, universally-supported case for
//	          a single-LUN-per-target device like this project's) |
//	          IDENTIFIER TYPE (bits3-0 = 3, NAA)
//	byte6:    reserved
//	byte7:    IDENTIFIER LENGTH (8)
//	byte8-15: the 8-byte NAA identifier value itself (naa, verbatim)
func DeviceIdentificationVPD(deviceType uint8, naa [8]byte) []byte {
	b := make([]byte, 4+4+8)
	b[0] = deviceType & 0x1F
	b[1] = vpdPageDeviceIdentification
	binary.BigEndian.PutUint16(b[2:4], 12) // page length: one 12-byte descriptor
	desc := b[4:]
	desc[0] = 0x01 // PROTOCOL IDENTIFIER=0, CODE SET=1 (binary)
	desc[1] = 0x03 // PIV=0, ASSOCIATION=0 (logical unit), IDENTIFIER TYPE=3 (NAA)
	desc[3] = 0x08 // IDENTIFIER LENGTH = 8
	copy(desc[4:12], naa[:])
	return b
}
