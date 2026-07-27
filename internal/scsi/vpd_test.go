package scsi

import (
	"bytes"
	"testing"
)

func TestSupportedVPDPages(t *testing.T) {
	got := SupportedVPDPages(PeripheralDeviceTypeMediumChanger)
	want := []byte{
		PeripheralDeviceTypeMediumChanger, // byte0: qualifier(0)+device type
		0x00,                              // byte1: page code
		0x00, 0x02,                        // bytes2-3: page length = 2
		0x00, 0x83, // supported page codes
	}
	if !bytes.Equal(got, want) {
		t.Errorf("SupportedVPDPages(changer) = % x, want % x", got, want)
	}
}

func TestDeviceIdentificationVPD(t *testing.T) {
	naa := [8]byte{0x50, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07}
	got := DeviceIdentificationVPD(PeripheralDeviceTypeSequentialAccess, naa)
	want := []byte{
		PeripheralDeviceTypeSequentialAccess, // byte0: qualifier(0)+device type
		0x83,                                 // byte1: page code
		0x00, 0x0c,                           // bytes2-3: page length = 12
		0x01,                   // descriptor byte0: code set = binary
		0x03,                   // descriptor byte1: association=LU, type=NAA
		0x00,                   // descriptor byte2: reserved
		0x08,                   // descriptor byte3: identifier length = 8
		0x50, 0x01, 0x02, 0x03, // descriptor byte4-11: NAA value
		0x04, 0x05, 0x06, 0x07,
	}
	if !bytes.Equal(got, want) {
		t.Errorf("DeviceIdentificationVPD() = % x, want % x", got, want)
	}
}
