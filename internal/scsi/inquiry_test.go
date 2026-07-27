package scsi

import "testing"

func TestStandardInquiryLayout(t *testing.T) {
	b := StandardInquiry(PeripheralDeviceTypeMediumChanger, Identity{Vendor: "GOTOCHNG", Product: "Virtual Changer", Revision: "0100"})
	if len(b) != StandardInquiryLength {
		t.Fatalf("length = %d, want %d", len(b), StandardInquiryLength)
	}
	if devType := b[0] & 0x1F; devType != PeripheralDeviceTypeMediumChanger {
		t.Errorf("peripheral device type = %#x, want %#x", devType, PeripheralDeviceTypeMediumChanger)
	}
	if b[1]&0x80 == 0 {
		t.Error("RMB bit not set")
	}
	if b[4] != StandardInquiryLength-4-1 {
		t.Errorf("additional length = %d, want %d", b[4], StandardInquiryLength-4-1)
	}
	if vendor := string(b[8:16]); vendor != "GOTOCHNG" {
		t.Errorf("vendor = %q, want %q", vendor, "GOTOCHNG")
	}
	if product := string(b[16:32]); product != "Virtual Changer " {
		t.Errorf("product = %q, want %q", product, "Virtual Changer ")
	}
	if rev := string(b[32:36]); rev != "0100" {
		t.Errorf("revision = %q, want %q", rev, "0100")
	}
}

func TestPadASCIITruncatesLongStrings(t *testing.T) {
	b := padASCII("ThisIsWayTooLongForEightBytes", 8)
	if len(b) != 8 {
		t.Fatalf("length = %d, want 8", len(b))
	}
	if string(b) != "ThisIsWa" {
		t.Errorf("got %q, want %q", b, "ThisIsWa")
	}
}
