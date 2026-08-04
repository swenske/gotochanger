package scsi

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net/http"
	"testing"

	"github.com/swenske/gotochanger/internal/apiclient"
	"github.com/swenske/gotochanger/internal/library"
	"github.com/swenske/gotochanger/internal/tcmu"
)

func vol(barcode string) *library.Volume { return &library.Volume{Barcode: barcode} }

// testStatus builds a small, fixed topology: 2 storage slots (1 occupied),
// 1 I/O slot (empty), 1 drive (empty) - unified addresses (see
// buildUnifiedElements) come out as slot(1)->2, slot(2)->3, ioslot(21)->4,
// drive(0)->5.
func testStatus() library.Status {
	return library.Status{
		Slots:   []*library.Slot{{Address: 1, Volume: vol("VOL001")}, {Address: 2}},
		IOSlots: []*library.IOSlot{{Address: 21}},
		Drives:  []*library.Drive{{Index: 0}},
	}
}

func entryWithCDB(cdb []byte, buffers ...[]byte) tcmu.Entry {
	return tcmu.Entry{CDB: cdb, Buffers: buffers}
}

func TestChangerTestUnitReady(t *testing.T) {
	c := &Changer{Client: &fakeClient{st: testStatus()}}
	resp := c.Handle(entryWithCDB([]byte{OpTestUnitReady, 0, 0, 0, 0, 0}))
	if resp.Status != StatusGood {
		t.Fatalf("status = %#x, want GOOD", resp.Status)
	}

	c2 := &Changer{Client: &fakeClient{statusErr: fmt.Errorf("down")}}
	resp2 := c2.Handle(entryWithCDB([]byte{OpTestUnitReady, 0, 0, 0, 0, 0}))
	if resp2.Status != StatusCheckCondition {
		t.Fatalf("status = %#x, want CHECK CONDITION", resp2.Status)
	}
}

func TestChangerRequestSenseTracksLastFailure(t *testing.T) {
	c := &Changer{Client: &fakeClient{statusErr: fmt.Errorf("down")}}
	c.Handle(entryWithCDB([]byte{OpTestUnitReady, 0, 0, 0, 0, 0})) // primes lastSense

	buf := make([]byte, 18)
	resp := c.Handle(entryWithCDB([]byte{OpRequestSense, 0, 0, 0, 18, 0}, buf))
	if resp.Status != StatusGood {
		t.Fatalf("REQUEST SENSE status = %#x, want GOOD", resp.Status)
	}
	if key := buf[2] & 0x0F; key != SenseNotReady {
		t.Errorf("sense key = %#x, want %#x", key, SenseNotReady)
	}

	// A second REQUEST SENSE with nothing new pending reports NO SENSE.
	buf2 := make([]byte, 18)
	c.Handle(entryWithCDB([]byte{OpRequestSense, 0, 0, 0, 18, 0}, buf2))
	if key := buf2[2] & 0x0F; key != SenseNoSense {
		t.Errorf("second sense key = %#x, want NoSense", key)
	}
}

func TestChangerInquiry(t *testing.T) {
	naa := [8]byte{0x50, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77}
	c := &Changer{Client: &fakeClient{st: testStatus()}, NAA: naa}
	buf := make([]byte, StandardInquiryLength)
	resp := c.Handle(entryWithCDB([]byte{OpInquiry, 0, 0, 0, byte(StandardInquiryLength), 0}, buf))
	if resp.Status != StatusGood || resp.ReadLen != StandardInquiryLength {
		t.Fatalf("resp = %+v", resp)
	}
	if devType := buf[0] & 0x1F; devType != PeripheralDeviceTypeMediumChanger {
		t.Errorf("peripheral device type = %#x, want medium changer", devType)
	}

	// EVPD page 0x00 - Supported VPD Pages.
	vpdBuf := make([]byte, 32)
	resp0 := c.Handle(entryWithCDB([]byte{OpInquiry, 0x01, 0x00, 0, byte(len(vpdBuf)), 0}, vpdBuf))
	if resp0.Status != StatusGood {
		t.Fatalf("EVPD page 0x00 status = %#x, want GOOD", resp0.Status)
	}
	want0 := SupportedVPDPages(PeripheralDeviceTypeMediumChanger)
	if got := vpdBuf[:resp0.ReadLen]; !bytes.Equal(got, want0) {
		t.Errorf("EVPD page 0x00 = % x, want % x", got, want0)
	}

	// EVPD page 0x83 - Device Identification.
	resp83 := c.Handle(entryWithCDB([]byte{OpInquiry, 0x01, 0x83, 0, byte(len(vpdBuf)), 0}, vpdBuf))
	if resp83.Status != StatusGood {
		t.Fatalf("EVPD page 0x83 status = %#x, want GOOD", resp83.Status)
	}
	want83 := DeviceIdentificationVPD(PeripheralDeviceTypeMediumChanger, naa)
	if got := vpdBuf[:resp83.ReadLen]; !bytes.Equal(got, want83) {
		t.Errorf("EVPD page 0x83 = % x, want % x", got, want83)
	}

	// EVPD page not supported.
	respUnsupported := c.Handle(entryWithCDB([]byte{OpInquiry, 0x01, 0x89, 0, byte(len(vpdBuf)), 0}, vpdBuf))
	if respUnsupported.Status != StatusCheckCondition {
		t.Fatalf("EVPD unsupported page status = %#x, want CHECK CONDITION", respUnsupported.Status)
	}
}

func buildReadElementStatusCDB(elementType uint8, startAddr, numElements uint16, allocLen int) []byte {
	cdb := make([]byte, 12)
	cdb[0] = OpReadElementStatus
	cdb[1] = elementType
	binary.BigEndian.PutUint16(cdb[2:4], startAddr)
	binary.BigEndian.PutUint16(cdb[4:6], numElements)
	cdb[7] = byte(allocLen >> 16)
	cdb[8] = byte(allocLen >> 8)
	cdb[9] = byte(allocLen)
	return cdb
}

func TestChangerReadElementStatusAllTypes(t *testing.T) {
	c := &Changer{Client: &fakeClient{st: testStatus()}}
	cdb := buildReadElementStatusCDB(0, 0, 10, 4096)
	buf := make([]byte, 4096)
	resp := c.Handle(entryWithCDB(cdb, buf))
	if resp.Status != StatusGood {
		t.Fatalf("status = %#x, want GOOD", resp.Status)
	}
	data := buf[:resp.ReadLen]
	if len(data) < 8 {
		t.Fatalf("response too short: %d bytes", len(data))
	}
	firstReported := binary.BigEndian.Uint16(data[0:2])
	numAvailable := binary.BigEndian.Uint16(data[2:4])
	if firstReported != 2 {
		t.Errorf("firstReported = %d, want 2", firstReported)
	}
	if numAvailable != 4 {
		t.Errorf("numAvailable = %d, want 4 (2 slots + 1 ioslot + 1 drive)", numAvailable)
	}

	// First page: storage elements (type 2), 2 descriptors.
	page := data[8:16]
	if page[0] != byte(elemStorage) {
		t.Fatalf("first page type = %d, want %d (storage)", page[0], elemStorage)
	}
	descLen := int(binary.BigEndian.Uint16(page[2:4]))
	if descLen != elementDescriptorLen {
		t.Errorf("descriptor length = %d, want %d", descLen, elementDescriptorLen)
	}
	desc0 := data[16 : 16+descLen]
	if addr := binary.BigEndian.Uint16(desc0[0:2]); addr != 2 {
		t.Errorf("first storage element address = %d, want 2", addr)
	}
	if full := desc0[2]&0x01 != 0; !full {
		t.Errorf("first storage element (occupied) reported as empty")
	}
	desc1 := data[16+descLen : 16+2*descLen]
	if full := desc1[2]&0x01 != 0; full {
		t.Errorf("second storage element (empty) reported as full")
	}

	// Second page: import/export (type 3), 1 descriptor at unified address 4.
	page2Off := 16 + 2*descLen
	page2 := data[page2Off : page2Off+8]
	if page2[0] != byte(elemImportExport) {
		t.Fatalf("second page type = %d, want %d (import/export)", page2[0], elemImportExport)
	}
	ioDesc := data[page2Off+8 : page2Off+8+descLen]
	if addr := binary.BigEndian.Uint16(ioDesc[0:2]); addr != 4 {
		t.Errorf("ioslot element address = %d, want 4", addr)
	}

	// Third page: data transfer (type 4), 1 descriptor at unified address 5.
	page3Off := page2Off + 8 + descLen
	page3 := data[page3Off : page3Off+8]
	if page3[0] != byte(elemDataTransfer) {
		t.Fatalf("third page type = %d, want %d (data transfer)", page3[0], elemDataTransfer)
	}
	driveDesc := data[page3Off+8 : page3Off+8+descLen]
	if addr := binary.BigEndian.Uint16(driveDesc[0:2]); addr != 5 {
		t.Errorf("drive element address = %d, want 5", addr)
	}
}

func TestChangerReadElementStatusVolTagReportsBarcode(t *testing.T) {
	c := &Changer{Client: &fakeClient{st: testStatus()}}
	cdb := buildReadElementStatusCDB(byte(elemStorage), 0, 10, 4096)
	cdb[1] |= 0x10 // VolTag bit
	buf := make([]byte, 4096)
	resp := c.Handle(entryWithCDB(cdb, buf))
	if resp.Status != StatusGood {
		t.Fatalf("status = %#x, want GOOD", resp.Status)
	}
	data := buf[:resp.ReadLen]
	page := data[8:16]
	if page[1]&0x80 == 0 {
		t.Errorf("page[1] PVolTag bit not set: %#x - a real initiator (mtx) ignores the volume tag field entirely when this is unset, even if it's actually present", page[1])
	}
	descLen := int(binary.BigEndian.Uint16(page[2:4]))
	if descLen != elementDescriptorLenVolTag {
		t.Fatalf("descriptor length = %d, want %d", descLen, elementDescriptorLenVolTag)
	}
	desc0 := data[16 : 16+descLen]
	barcode := string(desc0[12:44])
	if got := barcode[:6]; got != "VOL001" {
		t.Errorf("barcode = %q, want prefix %q", barcode, "VOL001")
	}
	// The empty second slot must report an all-zero (not garbage) tag field.
	desc1 := data[16+descLen : 16+2*descLen]
	for i, b := range desc1[12:44] {
		if b != 0 {
			t.Fatalf("empty slot's volume tag not zeroed at byte %d: %v", i, desc1[12:44])
		}
	}
}

func TestChangerReadElementStatusNoVolTagLeavesPVolTagUnset(t *testing.T) {
	c := &Changer{Client: &fakeClient{st: testStatus()}}
	cdb := buildReadElementStatusCDB(byte(elemStorage), 0, 10, 4096) // VolTag bit not set
	buf := make([]byte, 4096)
	resp := c.Handle(entryWithCDB(cdb, buf))
	if resp.Status != StatusGood {
		t.Fatalf("status = %#x, want GOOD", resp.Status)
	}
	page := buf[8:16]
	if page[1]&0x80 != 0 {
		t.Errorf("page[1] PVolTag bit set = %#x, want unset when the request didn't ask for volume tags", page[1])
	}
}

func TestChangerPositionToElement(t *testing.T) {
	c := &Changer{Client: &fakeClient{st: testStatus()}}
	cdb := make([]byte, 10)
	cdb[0] = OpPositionToElement
	binary.BigEndian.PutUint16(cdb[4:6], 5) // unified address 5 = drive 0
	resp := c.Handle(entryWithCDB(cdb))
	if resp.Status != StatusGood {
		t.Fatalf("status = %#x, want GOOD", resp.Status)
	}
	if c.armPosition != 5 {
		t.Errorf("armPosition = %d, want 5", c.armPosition)
	}

	badCDB := make([]byte, 10)
	badCDB[0] = OpPositionToElement
	binary.BigEndian.PutUint16(badCDB[4:6], 999)
	resp2 := c.Handle(entryWithCDB(badCDB))
	if resp2.Status != StatusCheckCondition {
		t.Fatalf("status = %#x, want CHECK CONDITION for an unknown destination", resp2.Status)
	}
}

func TestChangerModeSense6ElementAddressAssignment(t *testing.T) {
	// testStatus(): 2 storage slots -> unified 2,3; 1 ioslot -> unified 4;
	// 1 drive -> unified 5.
	c := &Changer{Client: &fakeClient{st: testStatus()}}
	buf := make([]byte, 64)

	cdb := []byte{OpModeSense6, 0, 0x1D, 0, 64, 0}
	resp := c.Handle(entryWithCDB(cdb, buf))
	if resp.Status != StatusGood {
		t.Fatalf("resp = %+v", resp)
	}
	data := buf[:resp.ReadLen]
	if len(data) != 4+20 {
		t.Fatalf("response length = %d, want %d", len(data), 4+20)
	}
	if int(data[0]) != len(data)-1 {
		t.Errorf("mode data length = %d, want %d", data[0], len(data)-1)
	}
	page := data[4:]
	if page[0] != 0x1D {
		t.Errorf("page code = %#x, want 0x1D", page[0])
	}
	if page[1] != 0x12 {
		t.Errorf("page length = %#x, want 0x12", page[1])
	}
	get := func(off int) uint16 { return binary.BigEndian.Uint16(page[off : off+2]) }
	if first, count := get(2), get(4); first != 1 || count != 1 {
		t.Errorf("medium transport first/count = %d/%d, want 1/1", first, count)
	}
	if first, count := get(6), get(8); first != 2 || count != 2 {
		t.Errorf("storage first/count = %d/%d, want 2/2", first, count)
	}
	if first, count := get(10), get(12); first != 4 || count != 1 {
		t.Errorf("import/export first/count = %d/%d, want 4/1", first, count)
	}
	if first, count := get(14), get(16); first != 5 || count != 1 {
		t.Errorf("data transfer first/count = %d/%d, want 5/1", first, count)
	}

	// Page 0x00 - header only, no page data (matches Drive.modeSense6's
	// convention for "current page, none returned").
	cdbNoPage := []byte{OpModeSense6, 0, 0x00, 0, 64, 0}
	resp2 := c.Handle(entryWithCDB(cdbNoPage, buf))
	if resp2.Status != StatusGood || resp2.ReadLen != 4 {
		t.Fatalf("page 0x00 resp = %+v, want GOOD/4 bytes", resp2)
	}

	// Unsupported page code.
	cdbBadPage := []byte{OpModeSense6, 0, 0x02, 0, 64, 0}
	if resp3 := c.Handle(entryWithCDB(cdbBadPage, buf)); resp3.Status != StatusCheckCondition {
		t.Fatalf("resp = %+v, want CHECK CONDITION", resp3)
	}
}

func TestChangerReadElementStatusFiltersByTypeAndStart(t *testing.T) {
	c := &Changer{Client: &fakeClient{st: testStatus()}}
	// Only storage elements (type 2), starting at unified address 3.
	cdb := buildReadElementStatusCDB(byte(elemStorage), 3, 10, 4096)
	buf := make([]byte, 4096)
	resp := c.Handle(entryWithCDB(cdb, buf))
	data := buf[:resp.ReadLen]
	numAvailable := binary.BigEndian.Uint16(data[2:4])
	if numAvailable != 1 {
		t.Fatalf("numAvailable = %d, want 1 (only slot at unified address 3)", numAvailable)
	}
}

func buildMoveMediumCDB(src, dst uint16) []byte {
	cdb := make([]byte, 12)
	cdb[0] = OpMoveMedium
	binary.BigEndian.PutUint16(cdb[4:6], src)
	binary.BigEndian.PutUint16(cdb[6:8], dst)
	return cdb
}

func TestChangerMoveMediumSlotToSlot(t *testing.T) {
	fc := &fakeClient{st: testStatus()}
	c := &Changer{Client: fc}
	// unified 2 (slot 1, occupied) -> unified 3 (slot 2, empty)
	resp := c.Handle(entryWithCDB(buildMoveMediumCDB(2, 3)))
	if resp.Status != StatusGood {
		t.Fatalf("status = %#x, want GOOD", resp.Status)
	}
	if fc.st.Slots[0].Volume != nil || fc.st.Slots[1].Volume == nil {
		t.Fatalf("volume did not move: slot0=%v slot1=%v", fc.st.Slots[0].Volume, fc.st.Slots[1].Volume)
	}
}

func TestChangerMoveMediumIgnoresWriteProtected(t *testing.T) {
	st := testStatus()
	st.Slots[0].Volume.WriteProtected = true
	fc := &fakeClient{st: st}
	c := &Changer{Client: fc}
	// unified 2 (slot 1, occupied, write-protected) -> unified 3 (slot 2, empty)
	resp := c.Handle(entryWithCDB(buildMoveMediumCDB(2, 3)))
	if resp.Status != StatusGood {
		t.Fatalf("status = %#x, want GOOD (write-protect must not block Move)", resp.Status)
	}
	if fc.st.Slots[0].Volume != nil || fc.st.Slots[1].Volume == nil {
		t.Fatalf("volume did not move: slot0=%v slot1=%v", fc.st.Slots[0].Volume, fc.st.Slots[1].Volume)
	}
}

func TestChangerMoveMediumSlotToDrive(t *testing.T) {
	fc := &fakeClient{st: testStatus()}
	c := &Changer{Client: fc}
	// unified 2 (slot 1, occupied) -> unified 5 (drive 0, empty)
	resp := c.Handle(entryWithCDB(buildMoveMediumCDB(2, 5)))
	if resp.Status != StatusGood {
		t.Fatalf("status = %#x, want GOOD", resp.Status)
	}
	if fc.st.Drives[0].Volume == nil {
		t.Fatal("drive was not loaded")
	}
}

func TestChangerMoveMediumDriveToSlot(t *testing.T) {
	fc := &fakeClient{st: testStatus()}
	fc.st.Drives[0].Volume = vol("INDRIVE")
	c := &Changer{Client: fc}
	// unified 5 (drive 0, occupied) -> unified 3 (slot 2, empty)
	resp := c.Handle(entryWithCDB(buildMoveMediumCDB(5, 3)))
	if resp.Status != StatusGood {
		t.Fatalf("status = %#x, want GOOD", resp.Status)
	}
	if fc.st.Drives[0].Volume != nil || fc.st.Slots[1].Volume == nil {
		t.Fatal("volume was not unloaded to slot")
	}
}

func TestChangerMoveMediumSourceEmpty(t *testing.T) {
	c := &Changer{Client: &fakeClient{st: testStatus()}}
	// unified 3 (slot 2) is empty.
	resp := c.Handle(entryWithCDB(buildMoveMediumCDB(3, 4)))
	if resp.Status != StatusCheckCondition || resp.Sense[12] != AscMediumSourceElementEmpty {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestChangerMoveMediumDestFull(t *testing.T) {
	// unified 2 (slot 1, occupied) -> unified 2 itself is occupied; use a
	// second occupied element as destination instead.
	fc := &fakeClient{st: testStatus()}
	fc.st.Drives[0].Volume = vol("INDRIVE")
	c := &Changer{Client: fc}
	resp := c.Handle(entryWithCDB(buildMoveMediumCDB(2, 5)))
	if resp.Status != StatusCheckCondition || resp.Sense[12] != AscMediumDestinationElementFull {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestChangerMoveMediumUnknownAddress(t *testing.T) {
	c := &Changer{Client: &fakeClient{st: testStatus()}}
	resp := c.Handle(entryWithCDB(buildMoveMediumCDB(2, 999)))
	if resp.Status != StatusCheckCondition || resp.Sense[12] != AscInvalidFieldInCDB {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestChangerMoveMediumUnderlyingFailure(t *testing.T) {
	c := &Changer{Client: &fakeClient{st: testStatus(), failMove: true}}
	resp := c.Handle(entryWithCDB(buildMoveMediumCDB(2, 3)))
	if resp.Status != StatusCheckCondition || resp.Sense[12] != AscMechanicalPositioningOrChangerError {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestChangerMoveMediumRoboticFaultSense(t *testing.T) {
	fc := &fakeClient{st: testStatus(), moveErr: &apiclient.APIError{
		StatusCode: http.StatusConflict,
		Message:    "POST /api/v1/move: robotic arm: blocked_arm: robotic arm is in fault state",
	}}
	c := &Changer{Client: fc}
	resp := c.Handle(entryWithCDB(buildMoveMediumCDB(2, 3)))
	if resp.Status != StatusCheckCondition {
		t.Fatalf("resp.Status = %d, want CHECK CONDITION", resp.Status)
	}
	if key := resp.Sense[2] & 0x0F; key != SenseNotReady {
		t.Errorf("sense key = %#x, want SenseNotReady (%#x)", key, SenseNotReady)
	}
	if resp.Sense[12] != AscLogicalUnitNotReady || resp.Sense[13] != AscqManualInterventionRequired {
		t.Errorf("ASC/ASCQ = %#x/%#x, want %#x/%#x", resp.Sense[12], resp.Sense[13], AscLogicalUnitNotReady, AscqManualInterventionRequired)
	}
}

func TestChangerMoveMediumCleaningFailureSense(t *testing.T) {
	fc := &fakeClient{st: testStatus(), moveErr: &apiclient.APIError{
		StatusCode: http.StatusConflict,
		Message:    `POST /api/v1/move: cleaning tape "00001CLN": cleaning tape has expired`,
	}}
	c := &Changer{Client: fc}
	resp := c.Handle(entryWithCDB(buildMoveMediumCDB(2, 3)))
	if resp.Status != StatusCheckCondition {
		t.Fatalf("resp.Status = %d, want CHECK CONDITION", resp.Status)
	}
	if key := resp.Sense[2] & 0x0F; key != SenseNotReady {
		t.Errorf("sense key = %#x, want SenseNotReady (%#x)", key, SenseNotReady)
	}
	if resp.Sense[12] != AscCleaningFailure || resp.Sense[13] != AscqCleaningFailure {
		t.Errorf("ASC/ASCQ = %#x/%#x, want %#x/%#x", resp.Sense[12], resp.Sense[13], AscCleaningFailure, AscqCleaningFailure)
	}
}

func TestChangerMoveMediumUnrelatedConflictKeepsGenericSense(t *testing.T) {
	fc := &fakeClient{st: testStatus(), moveErr: &apiclient.APIError{
		StatusCode: http.StatusConflict,
		Message:    "POST /api/v1/move: element is already full",
	}}
	c := &Changer{Client: fc}
	resp := c.Handle(entryWithCDB(buildMoveMediumCDB(2, 3)))
	if resp.Status != StatusCheckCondition || resp.Sense[12] != AscMechanicalPositioningOrChangerError {
		t.Fatalf("resp = %+v, want the generic fallback sense", resp)
	}
}

func TestChangerReserveRelease(t *testing.T) {
	c := &Changer{Client: &fakeClient{st: testStatus()}}
	if resp := c.Handle(entryWithCDB([]byte{OpReserve6, 0, 0, 0, 0, 0})); resp.Status != StatusGood {
		t.Fatalf("RESERVE resp = %+v", resp)
	}
	if !c.reserved {
		t.Error("reserved flag not set after RESERVE")
	}
	// A second RESERVE (simulating the same or another initiator, which
	// this project doesn't distinguish) still succeeds - see the
	// Changer.reserved field's doc comment.
	if resp := c.Handle(entryWithCDB([]byte{OpReserve6, 0, 0, 0, 0, 0})); resp.Status != StatusGood {
		t.Fatalf("second RESERVE resp = %+v", resp)
	}
	if resp := c.Handle(entryWithCDB([]byte{OpRelease6, 0, 0, 0, 0, 0})); resp.Status != StatusGood {
		t.Fatalf("RELEASE resp = %+v", resp)
	}
	if c.reserved {
		t.Error("reserved flag still set after RELEASE")
	}
	// Releasing when nothing is reserved is not an error.
	if resp := c.Handle(entryWithCDB([]byte{OpRelease6, 0, 0, 0, 0, 0})); resp.Status != StatusGood {
		t.Fatalf("RELEASE-when-not-reserved resp = %+v", resp)
	}
}

func TestChangerPreventAllowMediumRemoval(t *testing.T) {
	c := &Changer{Client: &fakeClient{st: testStatus()}}
	preventCDB := []byte{OpPreventAllowMediumRemoval, 0, 0, 0, 0x01, 0}
	if resp := c.Handle(entryWithCDB(preventCDB)); resp.Status != StatusGood || !c.preventRemoval {
		t.Fatalf("PREVENT resp = %+v, preventRemoval = %v", resp, c.preventRemoval)
	}
	allowCDB := []byte{OpPreventAllowMediumRemoval, 0, 0, 0, 0x00, 0}
	if resp := c.Handle(entryWithCDB(allowCDB)); resp.Status != StatusGood || c.preventRemoval {
		t.Fatalf("ALLOW resp = %+v, preventRemoval = %v", resp, c.preventRemoval)
	}
}

func buildSendVolumeTagCDB(elementType uint8, paramListLen int) []byte {
	cdb := make([]byte, 12)
	cdb[0] = OpSendVolumeTag
	cdb[1] = elementType & 0x0F
	cdb[8] = byte(paramListLen >> 8)
	cdb[9] = byte(paramListLen)
	return cdb
}

// buildVolumeTagParamList builds a 40-byte SEND VOLUME TAG parameter list:
// a NUL-terminated (not space-padded) 32-byte search template, per the
// Oracle StorageTek SL150 SCSI Reference Guide - make([]byte, 40) is
// already zeroed, so copy alone leaves the rest correctly NUL-padded.
func buildVolumeTagParamList(template string) []byte {
	data := make([]byte, 40)
	copy(data, template)
	return data
}

func buildRequestVolumeElementAddressCDB(startAddr, numElements uint16, allocLen int) []byte {
	cdb := make([]byte, 12)
	cdb[0] = OpRequestVolumeElementAddress
	binary.BigEndian.PutUint16(cdb[2:4], startAddr)
	binary.BigEndian.PutUint16(cdb[4:6], numElements)
	cdb[7] = byte(allocLen >> 16)
	cdb[8] = byte(allocLen >> 8)
	cdb[9] = byte(allocLen)
	return cdb
}

func TestChangerSendVolumeTagAndRequestVolumeElementAddress(t *testing.T) {
	c := &Changer{Client: &fakeClient{st: testStatus()}}
	params := buildVolumeTagParamList("VOL*")
	resp := c.Handle(entryWithCDB(buildSendVolumeTagCDB(0, len(params)), params))
	if resp.Status != StatusGood {
		t.Fatalf("SEND VOLUME TAG resp = %+v", resp)
	}
	if len(c.searchResults) != 1 || c.searchResults[0].barcode != "VOL001" {
		t.Fatalf("searchResults = %+v, want one match on VOL001", c.searchResults)
	}

	buf := make([]byte, 4096)
	resp2 := c.Handle(entryWithCDB(buildRequestVolumeElementAddressCDB(0, 10, 4096), buf))
	if resp2.Status != StatusGood {
		t.Fatalf("REQUEST VOLUME ELEMENT ADDRESS resp = %+v", resp2)
	}
	data := buf[:resp2.ReadLen]
	if got := binary.BigEndian.Uint16(data[2:4]); got != 1 {
		t.Fatalf("numAvailable = %d, want 1", got)
	}
	if data[4]&0x1F != sendActionCode {
		t.Errorf("send action code = %#x, want %#x", data[4]&0x1F, sendActionCode)
	}
	if addr := binary.BigEndian.Uint16(data[0:2]); addr != 2 {
		t.Errorf("first element address = %d, want 2 (the occupied slot)", addr)
	}
}

func TestChangerSendVolumeTagNoMatch(t *testing.T) {
	c := &Changer{Client: &fakeClient{st: testStatus()}}
	params := buildVolumeTagParamList("NOPE*")
	c.Handle(entryWithCDB(buildSendVolumeTagCDB(0, len(params)), params))
	if len(c.searchResults) != 0 {
		t.Fatalf("searchResults = %+v, want none", c.searchResults)
	}

	buf := make([]byte, 4096)
	resp := c.Handle(entryWithCDB(buildRequestVolumeElementAddressCDB(0, 10, 4096), buf))
	data := buf[:resp.ReadLen]
	if got := binary.BigEndian.Uint16(data[2:4]); got != 0 {
		t.Fatalf("numAvailable = %d, want 0", got)
	}
}

func TestChangerRequestVolumeElementAddressBeforeAnySearch(t *testing.T) {
	c := &Changer{Client: &fakeClient{st: testStatus()}}
	buf := make([]byte, 4096)
	resp := c.Handle(entryWithCDB(buildRequestVolumeElementAddressCDB(0, 10, 4096), buf))
	if resp.Status != StatusGood || resp.ReadLen != 8 {
		t.Fatalf("resp = %+v, want an empty 8-byte header", resp)
	}
}

func TestChangerSendVolumeTagZeroLengthClearsSearch(t *testing.T) {
	c := &Changer{Client: &fakeClient{st: testStatus()}}
	params := buildVolumeTagParamList("VOL*")
	c.Handle(entryWithCDB(buildSendVolumeTagCDB(0, len(params)), params))
	if len(c.searchResults) == 0 {
		t.Fatal("test setup: expected a match before clearing")
	}
	c.Handle(entryWithCDB(buildSendVolumeTagCDB(0, 0)))
	if c.searchResults != nil {
		t.Fatalf("searchResults = %+v, want nil after a zero-length SEND VOLUME TAG", c.searchResults)
	}
}

func TestChangerUnsupportedOpcode(t *testing.T) {
	c := &Changer{Client: &fakeClient{st: testStatus()}}
	resp := c.Handle(entryWithCDB([]byte{0xFF, 0, 0, 0, 0, 0}))
	if resp.Status != StatusCheckCondition || resp.Sense[12] != AscInvalidCommandOperationCode {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestChangerInquiryIdentityDefaultAndOptIn(t *testing.T) {
	// Zero-value Identity (every existing caller/test that doesn't set it)
	// falls back to DefaultChangerIdentity, unchanged from before this
	// field existed.
	c := &Changer{Client: &fakeClient{st: testStatus()}}
	buf := make([]byte, StandardInquiryLength)
	c.Handle(entryWithCDB([]byte{OpInquiry, 0, 0, 0, byte(StandardInquiryLength), 0}, buf))
	if vendor := string(buf[8:16]); vendor[:8] != "GOTOCHNG" {
		t.Errorf("default vendor = %q, want GOTOCHNG", vendor)
	}

	// An explicitly set Identity (Milestone 5's opt-in realistic profile)
	// is reported verbatim.
	c2 := &Changer{Client: &fakeClient{st: testStatus()}, Identity: RealisticChangerIdentity}
	buf2 := make([]byte, StandardInquiryLength)
	c2.Handle(entryWithCDB([]byte{OpInquiry, 0, 0, 0, byte(StandardInquiryLength), 0}, buf2))
	if vendor := string(buf2[8:16]); vendor[:3] != "STK" {
		t.Errorf("opted-in vendor = %q, want STK", vendor)
	}
	if product := string(buf2[16:32]); product[:5] != "SL150" {
		t.Errorf("opted-in product = %q, want SL150", product)
	}
}

func TestChangerInitializeElementStatus(t *testing.T) {
	c := &Changer{Client: &fakeClient{st: testStatus()}}
	if resp := c.Handle(entryWithCDB([]byte{OpInitializeElementStatus, 0, 0, 0, 0, 0})); resp.Status != StatusGood {
		t.Fatalf("INITIALIZE ELEMENT STATUS resp = %+v", resp)
	}
	if resp := c.Handle(entryWithCDB([]byte{OpInitializeElementStatusWithRange, 0, 0, 0, 0, 0, 0, 0, 0, 0})); resp.Status != StatusGood {
		t.Fatalf("INITIALIZE ELEMENT STATUS WITH RANGE resp = %+v", resp)
	}
}

func TestChangerRezeroUnit(t *testing.T) {
	c := &Changer{Client: &fakeClient{st: testStatus()}}
	c.armPosition = 99
	resp := c.Handle(entryWithCDB([]byte{OpRezeroUnit, 0, 0, 0, 0, 0}))
	if resp.Status != StatusGood {
		t.Fatalf("resp = %+v", resp)
	}
	if c.armPosition != 1 {
		t.Errorf("armPosition = %d, want 1 (home)", c.armPosition)
	}
}

// exchangeTestStatus: 3 storage slots (unified addresses 2, 3, 4) - two
// occupied (VOLA/VOLB), one free, used as EXCHANGE MEDIUM's own scratch
// space for the classic-swap case.
func exchangeTestStatus() library.Status {
	return library.Status{
		Slots: []*library.Slot{
			{Address: 1, Volume: vol("VOLA")},
			{Address: 2, Volume: vol("VOLB")},
			{Address: 3},
		},
	}
}

func exchangeMediumCDB(src, dst1, dst2 uint16) []byte {
	cdb := make([]byte, 11)
	cdb[0] = OpExchangeMedium
	binary.BigEndian.PutUint16(cdb[2:4], src)
	binary.BigEndian.PutUint16(cdb[4:6], dst1)
	binary.BigEndian.PutUint16(cdb[6:8], dst2)
	return cdb
}

func TestChangerExchangeMediumClassicSwapViaBorrowedScratch(t *testing.T) {
	fc := &fakeClient{st: exchangeTestStatus()}
	c := &Changer{Client: fc}
	// src=2 (VOLA), dst1=3 (VOLB), dst2=0 -> classic swap.
	resp := c.Handle(entryWithCDB(exchangeMediumCDB(2, 3, 0)))
	if resp.Status != StatusGood {
		t.Fatalf("resp = %+v", resp)
	}
	if got := fc.st.Slots[0].Volume; got == nil || got.Barcode != "VOLB" {
		t.Errorf("slot 1 (was VOLA) = %+v, want VOLB", got)
	}
	if got := fc.st.Slots[1].Volume; got == nil || got.Barcode != "VOLA" {
		t.Errorf("slot 2 (was VOLB) = %+v, want VOLA", got)
	}
	if fc.st.Slots[2].Volume != nil {
		t.Errorf("scratch slot 3 = %+v, want empty again after the swap completes", fc.st.Slots[2].Volume)
	}
}

func TestChangerExchangeMediumClassicSwapNoFreeSlot(t *testing.T) {
	st := exchangeTestStatus()
	st.Slots[2].Volume = vol("VOLC") // no free slot anywhere to borrow
	c := &Changer{Client: &fakeClient{st: st}}
	resp := c.Handle(entryWithCDB(exchangeMediumCDB(2, 3, 0)))
	if resp.Status != StatusCheckCondition || resp.Sense[12] != AscMediumDestinationElementFull {
		t.Fatalf("resp = %+v, want MediumDestinationElementFull", resp)
	}
}

func TestChangerExchangeMediumChainVariant(t *testing.T) {
	fc := &fakeClient{st: exchangeTestStatus()}
	c := &Changer{Client: fc}
	// src=2 (VOLA), dst1=3 (VOLB), dst2=4 (distinct, empty third location).
	resp := c.Handle(entryWithCDB(exchangeMediumCDB(2, 3, 4)))
	if resp.Status != StatusGood {
		t.Fatalf("resp = %+v", resp)
	}
	if fc.st.Slots[0].Volume != nil {
		t.Errorf("slot 1 (source) = %+v, want empty", fc.st.Slots[0].Volume)
	}
	if got := fc.st.Slots[1].Volume; got == nil || got.Barcode != "VOLA" {
		t.Errorf("slot 2 (dest1) = %+v, want VOLA", got)
	}
	if got := fc.st.Slots[2].Volume; got == nil || got.Barcode != "VOLB" {
		t.Errorf("slot 3 (dest2) = %+v, want VOLB (dest1's previous occupant)", got)
	}
}

func TestChangerExchangeMediumSourceEmpty(t *testing.T) {
	c := &Changer{Client: &fakeClient{st: exchangeTestStatus()}}
	resp := c.Handle(entryWithCDB(exchangeMediumCDB(4, 3, 0))) // addr 4 (slot 3) is empty
	if resp.Status != StatusCheckCondition || resp.Sense[12] != AscMediumSourceElementEmpty {
		t.Fatalf("resp = %+v, want MediumSourceElementEmpty", resp)
	}
}

func openCloseImportExportCDB(addr uint16, action uint8) []byte {
	cdb := make([]byte, 5)
	cdb[0] = OpOpenCloseImportExportElement
	binary.BigEndian.PutUint16(cdb[2:4], addr)
	cdb[4] = action
	return cdb
}

func TestChangerOpenCloseImportExportElement(t *testing.T) {
	st := library.Status{IOSlots: []*library.IOSlot{{Address: 21, MailboxID: "mb1"}}}
	fc := &fakeClient{st: st}
	c := &Changer{Client: fc}
	// The one ioslot is unified address 2 (no storage slots ahead of it
	// in this topology).
	if resp := c.Handle(entryWithCDB(openCloseImportExportCDB(2, 1))); resp.Status != StatusGood {
		t.Fatalf("open resp = %+v", resp)
	}
	if !fc.ioDoorOpen["mb1"] {
		t.Fatal("expected mailbox mb1's door to be open")
	}
	if resp := c.Handle(entryWithCDB(openCloseImportExportCDB(2, 0))); resp.Status != StatusGood {
		t.Fatalf("close resp = %+v", resp)
	}
	if fc.ioDoorOpen["mb1"] {
		t.Error("expected mailbox mb1's door to be closed")
	}
}

func TestChangerOpenCloseImportExportElementUnknownAddress(t *testing.T) {
	c := &Changer{Client: &fakeClient{st: testStatus()}}
	resp := c.Handle(entryWithCDB(openCloseImportExportCDB(999, 1)))
	if resp.Status != StatusCheckCondition || resp.Sense[12] != AscInvalidFieldInCDB {
		t.Fatalf("resp = %+v, want ILLEGAL REQUEST/INVALID FIELD IN CDB", resp)
	}
}

func TestChangerOpenCloseImportExportElementInvalidAction(t *testing.T) {
	st := library.Status{IOSlots: []*library.IOSlot{{Address: 21, MailboxID: "mb1"}}}
	c := &Changer{Client: &fakeClient{st: st}}
	resp := c.Handle(entryWithCDB(openCloseImportExportCDB(2, 2)))
	if resp.Status != StatusCheckCondition || resp.Sense[12] != AscInvalidFieldInCDB {
		t.Fatalf("resp = %+v, want ILLEGAL REQUEST/INVALID FIELD IN CDB", resp)
	}
}
