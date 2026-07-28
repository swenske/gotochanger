package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/swenske/gotochanger/internal/config"
	"github.com/swenske/gotochanger/internal/library"
)

// testTapeSet is the tape set name seeded by newTestServer; its tape type
// uses the "generic" barcode family with an 8-character volume identifier
// and no media-id suffix, so any 8-character uppercase-alphanumeric string
// is a valid manual barcode under it.
const testTapeSet = "TS1"

func newTestServer(t *testing.T) *Server {
	t.Helper()
	tmp := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = tmp
	cfg.Library.Magazines = []config.MagazineConfig{{ID: "Magazine1", Slots: 2}}
	cfg.Library.Mailboxes = []config.MailboxConfig{{ID: "Mailbox1", Slots: 1}}
	cfg.Library.DriveDevices = []config.DriveDeviceConfig{{DevicePath: filepath.Join(tmp, "drives", "drive0")}}
	cfg.Library.DefaultCapacity = "1MiB"
	cfg.Library.TapeTypes = []config.TapeType{{Name: "TESTTYPE", Capacity: "1MiB", BarcodeFamily: "generic", VolSerLength: 8}}
	cfg.Library.TapeSets = []config.TapeSetConfig{{Name: testTapeSet, TapeType: "TESTTYPE", StorageFolder: filepath.Join(tmp, "ts1")}}
	lib, err := library.New(cfg, nil, nil, nil)
	if err != nil {
		t.Fatalf("new library: %v", err)
	}
	return &Server{lib: lib, cfg: cfg}
}

func reqJSON(t *testing.T, method, target string, body any) *http.Request {
	t.Helper()
	var b []byte
	if body != nil {
		var err error
		b, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
	}
	r := httptest.NewRequest(method, target, bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestHandleAddTapeSetTapesBulk(t *testing.T) {
	s := newTestServer(t)
	rr := httptest.NewRecorder()
	req := reqJSON(t, http.MethodPost, "/api/v1/tape-sets/"+testTapeSet+"/tapes", map[string]any{"count": 1})
	req.SetPathValue("name", testTapeSet)

	s.handleAddTapeSetTapes(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected %d, got %d body=%s", http.StatusCreated, rr.Code, rr.Body.String())
	}

	status := s.lib.Status()
	if len(status.OutsideVolumes) != 1 {
		t.Fatalf("outside volume not created in library state")
	}
}

func TestHandleAddTapeSetTapesManual(t *testing.T) {
	s := newTestServer(t)
	rr := httptest.NewRecorder()
	req := reqJSON(t, http.MethodPost, "/api/v1/tape-sets/"+testTapeSet+"/tapes", map[string]any{"barcode": "TAPE0001"})
	req.SetPathValue("name", testTapeSet)

	s.handleAddTapeSetTapes(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected %d, got %d body=%s", http.StatusCreated, rr.Code, rr.Body.String())
	}

	status := s.lib.Status()
	if len(status.OutsideVolumes) != 1 || status.OutsideVolumes[0].Barcode != "TAPE0001" {
		t.Fatalf("outside volume not created with requested barcode")
	}
}

func TestHandleSetVolumeWriteProtect(t *testing.T) {
	s := newTestServer(t)
	vol, err := s.lib.CreateManualCartridge(testTapeSet, "VOLA0001")
	if err != nil {
		t.Fatalf("create cartridge: %v", err)
	}

	rr := httptest.NewRecorder()
	req := reqJSON(t, http.MethodPost, "/api/v1/volumes/VOLA0001/write-protect", map[string]any{"write_protected": true})
	req.SetPathValue("barcode", "VOLA0001")
	s.handleSetVolumeWriteProtect(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if !vol.WriteProtected {
		t.Fatalf("expected volume to be write-protected")
	}

	rr2 := httptest.NewRecorder()
	req2 := reqJSON(t, http.MethodPost, "/api/v1/volumes/NOPE0001/write-protect", map[string]any{"write_protected": true})
	req2.SetPathValue("barcode", "NOPE0001")
	s.handleSetVolumeWriteProtect(rr2, req2)
	if rr2.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rr2.Code, rr2.Body.String())
	}
}

func TestHandleSetVolumeWriteProtectRejectsInaccessibleLocation(t *testing.T) {
	s := newTestServer(t)
	if _, err := s.lib.CreateManualCartridge(testTapeSet, "VOLA0001"); err != nil {
		t.Fatalf("create cartridge: %v", err)
	}
	slotAddr := s.lib.Status().Slots[0].Address
	if err := s.lib.OpenStorageDoor("Magazine1", ""); err != nil {
		t.Fatalf("open storage door: %v", err)
	}
	if err := s.lib.CloseStorageDoor("Magazine1", []library.DoorAction{{Action: "load", Address: slotAddr, Barcode: "VOLA0001"}}); err != nil {
		t.Fatalf("close storage door with load action: %v", err)
	}

	rr := httptest.NewRecorder()
	req := reqJSON(t, http.MethodPost, "/api/v1/volumes/VOLA0001/write-protect", map[string]any{"write_protected": true})
	req.SetPathValue("barcode", "VOLA0001")
	s.handleSetVolumeWriteProtect(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body = %s", rr.Code, rr.Body.String())
	}
}

func TestHandleCloseIODoorConflictWhenClosed(t *testing.T) {
	s := newTestServer(t)
	rr := httptest.NewRecorder()
	req := reqJSON(t, http.MethodPost, "/api/v1/doors/io/Mailbox1/close", map[string]any{"actions": []any{}})
	req.SetPathValue("id", "Mailbox1")

	s.handleCloseIODoor(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected %d, got %d body=%s", http.StatusConflict, rr.Code, rr.Body.String())
	}

	evs := s.lib.Events()
	if len(evs) == 0 {
		t.Fatalf("expected failure event to be recorded")
	}
	if evs[0].Code != library.EventCodeRoboticsDoorIOCloseFailure {
		t.Fatalf("unexpected event code: %s", evs[0].Code)
	}
	if evs[0].Outcome != library.EventOutcomeFailure {
		t.Fatalf("expected failure outcome, got %s", evs[0].Outcome)
	}
}

func TestHandleAddTapeSetTapesBadJSON(t *testing.T) {
	s := newTestServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tape-sets/"+testTapeSet+"/tapes", bytes.NewBufferString("{"))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", testTapeSet)

	s.handleAddTapeSetTapes(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected %d, got %d body=%s", http.StatusBadRequest, rr.Code, rr.Body.String())
	}
}

func TestHandleOpenAndCloseIODoorWithActions(t *testing.T) {
	s := newTestServer(t)
	if _, err := s.lib.CreateManualCartridge(testTapeSet, "TAPE0001"); err != nil {
		t.Fatalf("seed outside volume: %v", err)
	}
	ioAddr := s.lib.Status().IOSlots[0].Address

	openReq := reqJSON(t, http.MethodPost, "/api/v1/doors/io/Mailbox1/open", map[string]any{})
	openReq.SetPathValue("id", "Mailbox1")
	rr := httptest.NewRecorder()
	s.handleOpenIODoor(rr, openReq)
	if rr.Code != http.StatusOK {
		t.Fatalf("open io door expected %d, got %d", http.StatusOK, rr.Code)
	}

	closeReq := reqJSON(t, http.MethodPost, "/api/v1/doors/io/Mailbox1/close", map[string]any{
		"actions": []map[string]any{{"action": "load", "address": ioAddr, "barcode": "TAPE0001"}},
	})
	closeReq.SetPathValue("id", "Mailbox1")
	rr = httptest.NewRecorder()
	s.handleCloseIODoor(rr, closeReq)
	if rr.Code != http.StatusOK {
		t.Fatalf("close io door expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}

	st := s.lib.Status()
	if st.IOSlots[0].Volume == nil || st.IOSlots[0].Volume.Barcode != "TAPE0001" {
		t.Fatalf("expected TAPE0001 loaded into ioslot")
	}
	for _, open := range st.Doors.OpenMailboxes {
		if open == "Mailbox1" {
			t.Fatalf("expected io door closed after close handler")
		}
	}
}

func TestHandleStorageDoorActions(t *testing.T) {
	s := newTestServer(t)
	if _, err := s.lib.CreateManualCartridge(testTapeSet, "TAPE0001"); err != nil {
		t.Fatalf("seed outside volume: %v", err)
	}
	slotAddr := s.lib.Status().Slots[0].Address
	magazineID := s.lib.Status().Slots[0].MagazineID

	rr := httptest.NewRecorder()
	openReq := reqJSON(t, http.MethodPost, "/api/v1/doors/storage/"+magazineID+"/open", map[string]any{})
	openReq.SetPathValue("id", magazineID)
	s.handleOpenStorageDoor(rr, openReq)
	if rr.Code != http.StatusOK {
		t.Fatalf("open storage door expected %d, got %d", http.StatusOK, rr.Code)
	}

	rr = httptest.NewRecorder()
	closeReq := reqJSON(t, http.MethodPost, "/api/v1/doors/storage/"+magazineID+"/close", map[string]any{
		"actions": []map[string]any{{"action": "load", "address": slotAddr, "barcode": "TAPE0001"}},
	})
	closeReq.SetPathValue("id", magazineID)
	s.handleCloseStorageDoor(rr, closeReq)
	if rr.Code != http.StatusOK {
		t.Fatalf("close storage door expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}

	st := s.lib.Status()
	if st.Slots[0].Volume == nil || st.Slots[0].Volume.Barcode != "TAPE0001" {
		t.Fatalf("expected TAPE0001 loaded into storage slot")
	}
	for _, open := range st.Doors.OpenMagazines {
		if open == magazineID {
			t.Fatalf("expected storage door closed after close handler")
		}
	}
}
