package scsi

// SCSI peripheral device type codes (SPC), byte0 bits 4-0 of a standard
// INQUIRY response.
const (
	PeripheralDeviceTypeSequentialAccess = 0x01 // tape drive
	PeripheralDeviceTypeMediumChanger    = 0x08 // changer/robot
)

// StandardInquiryLength is the size of the fixed-format standard INQUIRY
// response this project always returns - the minimum SPC mandates before
// vendor-specific bytes; nothing this project's handlers or a typical
// initiator's core INQUIRY parsing need lives past byte 35.
const StandardInquiryLength = 36

// Identity is the vendor/product/revision string triple a device reports
// in its standard INQUIRY response.
type Identity struct {
	Vendor   string // T10 vendor identification - conventionally 8 characters
	Product  string // conventionally up to 16 characters
	Revision string // conventionally up to 4 characters
}

// StandardInquiry builds a 36-byte standard INQUIRY response for the given
// peripheral device type and identity. RMB (removable media) is always
// set: every device this project emulates (a changer, and the cartridges
// its drives take) is removable-media by definition.
func StandardInquiry(deviceType uint8, id Identity) []byte {
	b := make([]byte, StandardInquiryLength)
	b[0] = deviceType & 0x1F // peripheral qualifier 0 (connected, supported)
	b[1] = 0x80              // RMB=1
	b[2] = 0x05              // VERSION: SPC-3
	b[3] = 0x02              // response data format = 2
	b[4] = StandardInquiryLength - 4 - 1
	copy(b[8:16], padASCII(id.Vendor, 8))
	copy(b[16:32], padASCII(id.Product, 16))
	copy(b[32:36], padASCII(id.Revision, 4))
	return b
}

// padASCII truncates or space-pads s to exactly n bytes, the convention
// every T10 vendor/product/revision identification field uses.
func padASCII(s string, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = ' '
	}
	copy(b, s)
	return b
}
