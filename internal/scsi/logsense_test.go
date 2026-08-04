package scsi

import (
	"encoding/binary"
	"testing"

	"github.com/swenske/gotochanger/internal/library"
)

func logSenseCDB(pageCode uint8, allocLen int) []byte {
	cdb := make([]byte, 10)
	cdb[0] = OpLogSense
	cdb[2] = pageCode & 0x3F
	binary.BigEndian.PutUint16(cdb[7:9], uint16(allocLen))
	return cdb
}

// findLogParam returns the active flag byte for parameter code within a
// buildLogSensePage-shaped response body (after the 4-byte header), and
// whether it was found at all.
func findLogParam(data []byte, code uint16) (active bool, found bool) {
	if len(data) < 4 {
		return false, false
	}
	body := data[4:]
	for i := 0; i+tapeAlertParamLen <= len(body); i += tapeAlertParamLen {
		if binary.BigEndian.Uint16(body[i:i+2]) == code {
			return body[i+4]&0x01 != 0, true
		}
	}
	return false, false
}

func TestBuildLogSensePage(t *testing.T) {
	page := buildLogSensePage(logPageTapeAlert, []logParameter{
		{code: 0x01, active: false},
		{code: 0x02, active: true},
	})
	if page[1] != logPageTapeAlert {
		t.Errorf("page code = %#x, want %#x", page[1], logPageTapeAlert)
	}
	wantLen := 2 * tapeAlertParamLen
	if gotLen := binary.BigEndian.Uint16(page[2:4]); int(gotLen) != wantLen {
		t.Errorf("page length = %d, want %d", gotLen, wantLen)
	}
	if active, found := findLogParam(page, 0x01); !found || active {
		t.Errorf("param 0x01: active=%v found=%v, want inactive/found", active, found)
	}
	if active, found := findLogParam(page, 0x02); !found || !active {
		t.Errorf("param 0x02: active=%v found=%v, want active/found", active, found)
	}
}

func TestBuildSupportedLogPages(t *testing.T) {
	pages := buildSupportedLogPages(logPageTapeAlert)
	body := pages[4:]
	if len(body) != 2 {
		t.Fatalf("body = % x, want 2 page codes", body)
	}
	if body[0] != logPageSupportedPages || body[1] != logPageTapeAlert {
		t.Errorf("body = % x, want [0x00 0x2E]", body)
	}
}

func TestDriveLogSenseTapeAlert(t *testing.T) {
	st, _ := driveStatusWithFile(t, nil, 0)
	st.Drives[0].Fault = true
	st.CleaningEnabled = true
	st.CleaningMountThreshold = 5
	st.Drives[0].MountsSinceCleaning = 10
	st.Drives[0].Volume.WriteProtected = true

	d := &Drive{Client: &fakeClient{st: st}}
	buf := make([]byte, 256)
	resp := d.Handle(entryWithCDB(logSenseCDB(logPageTapeAlert, len(buf)), buf))
	if resp.Status != StatusGood {
		t.Fatalf("resp = %+v", resp)
	}
	data := buf[:resp.ReadLen]

	for _, tc := range []struct {
		name string
		code uint16
		want bool
	}{
		{"hardware A (Fault)", driveTapeAlertHardwareA, true},
		{"drive cleaning (over threshold)", driveTapeAlertDriveCleaning, true},
		{"write protect (mounted, protected)", driveTapeAlertWriteProtect, true},
	} {
		active, found := findLogParam(data, tc.code)
		if !found {
			t.Errorf("%s: parameter code %#x not found in response", tc.name, tc.code)
			continue
		}
		if active != tc.want {
			t.Errorf("%s: active = %v, want %v", tc.name, active, tc.want)
		}
	}

	// A healthy drive with no write-protected volume reports every flag
	// inactive - confirms this isn't accidentally always-on.
	st2, _ := driveStatusWithFile(t, nil, 0)
	d2 := &Drive{Client: &fakeClient{st: st2}}
	buf2 := make([]byte, 256)
	resp2 := d2.Handle(entryWithCDB(logSenseCDB(logPageTapeAlert, len(buf2)), buf2))
	data2 := buf2[:resp2.ReadLen]
	if active, found := findLogParam(data2, driveTapeAlertHardwareA); !found || active {
		t.Errorf("healthy drive: hardware A active = %v found = %v, want inactive/found", active, found)
	}
}

func TestDriveLogSenseSupportedPages(t *testing.T) {
	d := &Drive{Client: &fakeClient{st: library.Status{Drives: []*library.Drive{{Index: 0}}}}}
	buf := make([]byte, 32)
	resp := d.Handle(entryWithCDB(logSenseCDB(logPageSupportedPages, len(buf)), buf))
	if resp.Status != StatusGood {
		t.Fatalf("resp = %+v", resp)
	}
	if buf[4] != logPageSupportedPages || buf[5] != logPageTapeAlert {
		t.Errorf("supported pages body = % x, want [0x00 0x2E]", buf[4:resp.ReadLen])
	}
}

func TestDriveLogSenseUnsupportedPage(t *testing.T) {
	d := &Drive{Client: &fakeClient{st: library.Status{Drives: []*library.Drive{{Index: 0}}}}}
	buf := make([]byte, 32)
	resp := d.Handle(entryWithCDB(logSenseCDB(0x3D, len(buf)), buf))
	if resp.Status != StatusCheckCondition || resp.Sense[12] != AscInvalidFieldInCDB {
		t.Fatalf("resp = %+v, want CHECK CONDITION/Invalid Field in CDB", resp)
	}
}

func TestDriveLogSelectStub(t *testing.T) {
	d := &Drive{}
	cdb := make([]byte, 10)
	cdb[0] = OpLogSelect
	if resp := d.Handle(entryWithCDB(cdb)); resp.Status != StatusGood {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestChangerLogSenseTapeAlertRoboticFaultKinds(t *testing.T) {
	for _, tc := range []struct {
		kind string
		code uint16
	}{
		{library.RoboticFaultBlockedArm, changerTapeAlertHardwareA},
		{library.RoboticFaultMovementJam, changerTapeAlertHardwareB},
		{library.RoboticFaultOther, changerTapeAlertHardwareC},
		{library.RoboticFaultPickupFailure, changerTapeAlertPickRetry},
		{library.RoboticFaultDropFailure, changerTapeAlertPlaceRetry},
		{library.RoboticFaultMispositionedCartridge, changerTapeAlertInventory},
	} {
		st := testStatus()
		st.RoboticFault = library.RoboticFault{Active: true, Kind: tc.kind}
		c := &Changer{Client: &fakeClient{st: st}}
		buf := make([]byte, 256)
		resp := c.Handle(entryWithCDB(logSenseCDB(logPageTapeAlert, len(buf)), buf))
		if resp.Status != StatusGood {
			t.Fatalf("kind %q: resp = %+v", tc.kind, resp)
		}
		data := buf[:resp.ReadLen]
		active, found := findLogParam(data, tc.code)
		if !found || !active {
			t.Errorf("kind %q: parameter %d active=%v found=%v, want active/found", tc.kind, tc.code, active, found)
		}
	}
}

func TestChangerLogSenseTapeAlertDoorOpen(t *testing.T) {
	st := testStatus()
	st.Doors.OpenMagazines = []string{"mag1"}
	c := &Changer{Client: &fakeClient{st: st}}
	buf := make([]byte, 256)
	resp := c.Handle(entryWithCDB(logSenseCDB(logPageTapeAlert, len(buf)), buf))
	if resp.Status != StatusGood {
		t.Fatalf("resp = %+v", resp)
	}
	active, found := findLogParam(buf[:resp.ReadLen], changerTapeAlertLibraryDoor)
	if !found || !active {
		t.Errorf("library door: active=%v found=%v, want active/found", active, found)
	}
}

func TestChangerLogSelectStub(t *testing.T) {
	c := &Changer{}
	cdb := make([]byte, 10)
	cdb[0] = OpLogSelect
	if resp := c.Handle(entryWithCDB(cdb)); resp.Status != StatusGood {
		t.Fatalf("resp = %+v", resp)
	}
}
