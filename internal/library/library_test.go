package library

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swenske/gotochanger/internal/config"
	"github.com/swenske/gotochanger/internal/secrethash"
)

// testTapeSet is the tape set name seeded by newTestLibrary; its tape type
// uses the "generic" barcode family with an 8-character volume identifier
// and no media-id suffix, so any 8-character uppercase-alphanumeric string
// is a valid manual barcode under it.
const testTapeSet = "TS1"

func newTestLibrary(t *testing.T) *Library {
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
	lib, err := New(cfg, nil, nil, nil)
	if err != nil {
		t.Fatalf("new library: %v", err)
	}
	return lib
}

func containsBarcode(vols []*Volume, barcode string) bool {
	for _, v := range vols {
		if v != nil && v.Barcode == barcode {
			return true
		}
	}
	return false
}

func TestTapeSetCartridgeCreateDelete(t *testing.T) {
	lib := newTestLibrary(t)
	vol, err := lib.CreateManualCartridge(testTapeSet, "VOLA0001")
	if err != nil {
		t.Fatalf("create manual cartridge: %v", err)
	}
	if !containsBarcode(lib.OutsideVolumes(), "VOLA0001") {
		t.Fatalf("outside list missing created volume")
	}
	if _, err := os.Stat(vol.Path); err != nil {
		t.Fatalf("expected backing file to exist: %v", err)
	}

	if err := lib.DeleteOutsideVolume("VOLA0001"); err != nil {
		t.Fatalf("delete outside volume: %v", err)
	}
	if containsBarcode(lib.OutsideVolumes(), "VOLA0001") {
		t.Fatalf("outside list still contains deleted volume")
	}
	if _, err := os.Stat(vol.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected backing file removed, got: %v", err)
	}
}

func TestSetVolumeWriteProtectTogglesChmod(t *testing.T) {
	lib := newTestLibrary(t)
	vol, err := lib.CreateManualCartridge(testTapeSet, "VOLA0001")
	if err != nil {
		t.Fatalf("create cartridge: %v", err)
	}

	if err := lib.SetVolumeWriteProtect("VOLA0001", true); err != nil {
		t.Fatalf("set write-protect: %v", err)
	}
	if !vol.WriteProtected {
		t.Fatalf("expected WriteProtected=true")
	}
	fi, err := os.Stat(vol.Path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o440 {
		t.Errorf("mode = %o, want 0440", fi.Mode().Perm())
	}

	if err := lib.SetVolumeWriteProtect("VOLA0001", false); err != nil {
		t.Fatalf("clear write-protect: %v", err)
	}
	if vol.WriteProtected {
		t.Fatalf("expected WriteProtected=false")
	}
	fi2, err := os.Stat(vol.Path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi2.Mode().Perm() != 0o660 {
		t.Errorf("mode after clearing = %o, want 0660", fi2.Mode().Perm())
	}
}

func TestSetVolumeWriteProtectClearDoesNotUnlockFullVolume(t *testing.T) {
	lib := newTestLibrary(t)
	vol, err := lib.CreateManualCartridge(testTapeSet, "VOLA0001")
	if err != nil {
		t.Fatalf("create cartridge: %v", err)
	}
	vol.Full = true
	lib.applyVolumeFileModeLocked(vol) // simulate the state a real capacity-triggered chmod would leave

	if err := lib.SetVolumeWriteProtect("VOLA0001", true); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := lib.SetVolumeWriteProtect("VOLA0001", false); err != nil {
		t.Fatalf("clear: %v", err)
	}
	fi, err := os.Stat(vol.Path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o440 {
		t.Errorf("mode = %o, want 0440 (Full must keep it read-only even after WriteProtected is cleared)", fi.Mode().Perm())
	}
}

func TestSetVolumeWriteProtectNotFound(t *testing.T) {
	lib := newTestLibrary(t)
	if err := lib.SetVolumeWriteProtect("NOPE0001", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestSetVolumeWriteProtectRequiresAccessibleLocation(t *testing.T) {
	lib := newTestLibrary(t)
	placeVolumeInFirstSlot(lib, "VOLA0001") // Magazine1, storage door starts closed

	if err := lib.SetVolumeWriteProtect("VOLA0001", true); !errors.Is(err, ErrVolumeNotAccessible) {
		t.Fatalf("expected ErrVolumeNotAccessible behind a closed magazine door, got %v", err)
	}

	if err := lib.OpenStorageDoor("Magazine1", ""); err != nil {
		t.Fatalf("open storage door: %v", err)
	}
	if err := lib.SetVolumeWriteProtect("VOLA0001", true); err != nil {
		t.Fatalf("set write-protect with magazine door open: %v", err)
	}
	if err := lib.CloseStorageDoor("Magazine1", nil); err != nil {
		t.Fatalf("close storage door: %v", err)
	}

	// Mounted in a drive: rejected regardless of any door state.
	if err := lib.OpenStorageDoor("Magazine1", ""); err != nil {
		t.Fatalf("reopen storage door: %v", err)
	}
	if err := lib.Load(ElementRef{Kind: KindSlot, Address: lib.slots[0].Address}, 0, ""); err != nil {
		t.Fatalf("load into drive: %v", err)
	}
	if err := lib.SetVolumeWriteProtect("VOLA0001", false); !errors.Is(err, ErrVolumeNotAccessible) {
		t.Fatalf("expected ErrVolumeNotAccessible while mounted in a drive, got %v", err)
	}
}

func TestSetVolumeWriteProtectAccessibleInOpenMailbox(t *testing.T) {
	lib := newTestLibrary(t)
	lib.ioslots[0].Volume = &Volume{Barcode: "VOLA0001", TapeSet: testTapeSet, Path: filepath.Join(lib.cfg.DataDir, "VOLA0001")}

	if err := lib.SetVolumeWriteProtect("VOLA0001", true); !errors.Is(err, ErrVolumeNotAccessible) {
		t.Fatalf("expected ErrVolumeNotAccessible behind a closed mailbox door, got %v", err)
	}

	if err := lib.OpenIODoor("Mailbox1", ""); err != nil {
		t.Fatalf("open io door: %v", err)
	}
	if err := lib.SetVolumeWriteProtect("VOLA0001", true); err != nil {
		t.Fatalf("set write-protect with mailbox door open: %v", err)
	}
}

func TestMoveAndLoadIgnoreWriteProtected(t *testing.T) {
	lib := newTestLibrary(t)
	placeVolumeInFirstSlot(lib, "VOLA0001")
	lib.slots[0].Volume.WriteProtected = true

	if err := lib.Move(ElementRef{Kind: KindSlot, Address: lib.slots[0].Address}, ElementRef{Kind: KindSlot, Address: lib.slots[1].Address}, ""); err != nil {
		t.Fatalf("move write-protected volume: %v", err)
	}
	if err := lib.Load(ElementRef{Kind: KindSlot, Address: lib.slots[1].Address}, 0, ""); err != nil {
		t.Fatalf("load write-protected volume: %v", err)
	}
}

func TestCreateTapeSetCartridgesBulk(t *testing.T) {
	lib := newTestLibrary(t)
	vols, err := lib.CreateTapeSetCartridges(testTapeSet, 3)
	if err != nil {
		t.Fatalf("create tape set cartridges: %v", err)
	}
	if len(vols) != 3 {
		t.Fatalf("expected 3 cartridges, got %d", len(vols))
	}
	seen := map[string]bool{}
	for _, v := range vols {
		if seen[v.Barcode] {
			t.Fatalf("duplicate barcode generated: %s", v.Barcode)
		}
		seen[v.Barcode] = true
		if len(v.Barcode) != 8 {
			t.Fatalf("expected 8-character barcode, got %q", v.Barcode)
		}
	}
}

func TestCreateTapeSetCartridgesTopsUpAfterExistingBarcodes(t *testing.T) {
	lib := newTestLibrary(t)
	first, err := lib.CreateTapeSetCartridges(testTapeSet, 2)
	if err != nil {
		t.Fatalf("create first batch: %v", err)
	}
	second, err := lib.CreateTapeSetCartridges(testTapeSet, 2)
	if err != nil {
		t.Fatalf("create second batch: %v", err)
	}
	for _, v := range second {
		if containsBarcode(first, v.Barcode) {
			t.Fatalf("second batch barcode %s collides with first batch", v.Barcode)
		}
	}
}

func TestCreateManualCartridgeRejectsWrongFormat(t *testing.T) {
	lib := newTestLibrary(t)
	if _, err := lib.CreateManualCartridge(testTapeSet, "short"); !errors.Is(err, ErrInvalidBarcode) {
		t.Fatalf("expected ErrInvalidBarcode for wrong-length lowercase barcode, got %v", err)
	}
}

func TestCreateManualCartridgeRejectsDuplicateBarcode(t *testing.T) {
	lib := newTestLibrary(t)
	if _, err := lib.CreateManualCartridge(testTapeSet, "VOLA0001"); err != nil {
		t.Fatalf("create first cartridge: %v", err)
	}
	if _, err := lib.CreateManualCartridge(testTapeSet, "VOLA0001"); !errors.Is(err, ErrBarcodeExists) {
		t.Fatalf("expected ErrBarcodeExists for duplicate barcode, got %v", err)
	}
}

func TestCreateTapeSetCartridgesUnknownTapeSet(t *testing.T) {
	lib := newTestLibrary(t)
	if _, err := lib.CreateTapeSetCartridges("NoSuchTapeSet", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for unknown tape set, got %v", err)
	}
}

func TestCloseIODoorWhenClosed(t *testing.T) {
	lib := newTestLibrary(t)
	if err := lib.CloseIODoor("Mailbox1", nil); !errors.Is(err, ErrDoorClosed) {
		t.Fatalf("expected ErrDoorClosed, got %v", err)
	}
}

func TestIODoorCloseAppliesBatchActions(t *testing.T) {
	lib := newTestLibrary(t)
	if _, err := lib.CreateManualCartridge(testTapeSet, "VOLA0001"); err != nil {
		t.Fatalf("create outside A: %v", err)
	}
	if _, err := lib.CreateManualCartridge(testTapeSet, "VOLB0001"); err != nil {
		t.Fatalf("create outside B: %v", err)
	}

	if err := lib.OpenIODoor("Mailbox1", ""); err != nil {
		t.Fatalf("open io door: %v", err)
	}
	ioAddr := lib.Status().IOSlots[0].Address
	if err := lib.CloseIODoor("Mailbox1", []DoorAction{{Action: "load", Address: ioAddr, Barcode: "VOLA0001"}}); err != nil {
		t.Fatalf("close io door load: %v", err)
	}

	st := lib.Status()
	for _, open := range st.Doors.OpenMailboxes {
		if open == "Mailbox1" {
			t.Fatalf("io door should be closed after processing")
		}
	}
	if st.IOSlots[0].Volume == nil || st.IOSlots[0].Volume.Barcode != "VOLA0001" {
		t.Fatalf("expected VOLA0001 in ioslot, got %+v", st.IOSlots[0].Volume)
	}
	if !containsBarcode(st.OutsideVolumes, "VOLB0001") || containsBarcode(st.OutsideVolumes, "VOLA0001") {
		t.Fatalf("outside volumes mismatch after io load")
	}

	if err := lib.OpenIODoor("Mailbox1", ""); err != nil {
		t.Fatalf("re-open io door: %v", err)
	}
	if err := lib.CloseIODoor("Mailbox1", []DoorAction{{Action: "pickup", Address: ioAddr}}); err != nil {
		t.Fatalf("close io door pickup: %v", err)
	}
	st = lib.Status()
	if st.IOSlots[0].Volume != nil {
		t.Fatalf("expected ioslot empty after pickup")
	}
	if !containsBarcode(st.OutsideVolumes, "VOLA0001") || !containsBarcode(st.OutsideVolumes, "VOLB0001") {
		t.Fatalf("expected both volumes outside after pickup")
	}
}

func TestIODoorCloseIsAtomicOnValidationError(t *testing.T) {
	lib := newTestLibrary(t)
	if _, err := lib.CreateManualCartridge(testTapeSet, "VOLA0001"); err != nil {
		t.Fatalf("create outside A: %v", err)
	}
	if _, err := lib.CreateManualCartridge(testTapeSet, "VOLB0001"); err != nil {
		t.Fatalf("create outside B: %v", err)
	}

	if err := lib.OpenIODoor("Mailbox1", ""); err != nil {
		t.Fatalf("open io door: %v", err)
	}
	ioAddr := lib.Status().IOSlots[0].Address
	err := lib.CloseIODoor("Mailbox1", []DoorAction{
		{Action: "load", Address: ioAddr, Barcode: "VOLA0001"},
		{Action: "load", Address: ioAddr, Barcode: "VOLB0001"},
	})
	if err == nil {
		t.Fatalf("expected validation error for conflicting io actions")
	}

	st := lib.Status()
	stillOpen := false
	for _, open := range st.Doors.OpenMailboxes {
		if open == "Mailbox1" {
			stillOpen = true
		}
	}
	if !stillOpen {
		t.Fatalf("io door should stay open when batch fails")
	}
	if st.IOSlots[0].Volume != nil {
		t.Fatalf("batch should be atomic; ioslot should remain unchanged")
	}
	if !containsBarcode(st.OutsideVolumes, "VOLA0001") || !containsBarcode(st.OutsideVolumes, "VOLB0001") {
		t.Fatalf("batch should be atomic; outside volumes changed")
	}
}

func TestStorageDoorBatchLoadAndPickup(t *testing.T) {
	lib := newTestLibrary(t)
	if _, err := lib.CreateManualCartridge(testTapeSet, "VOLA0001"); err != nil {
		t.Fatalf("create outside A: %v", err)
	}
	if _, err := lib.CreateManualCartridge(testTapeSet, "VOLB0001"); err != nil {
		t.Fatalf("create outside B: %v", err)
	}

	if err := lib.OpenStorageDoor("Magazine1", ""); err != nil {
		t.Fatalf("open storage door: %v", err)
	}
	st := lib.Status()
	if err := lib.CloseStorageDoor("Magazine1", []DoorAction{
		{Action: "load", Address: st.Slots[0].Address, Barcode: "VOLA0001"},
		{Action: "load", Address: st.Slots[1].Address, Barcode: "VOLB0001"},
	}); err != nil {
		t.Fatalf("close storage door load: %v", err)
	}
	st = lib.Status()
	if st.Slots[0].Volume == nil || st.Slots[1].Volume == nil {
		t.Fatalf("expected both slots loaded after storage close")
	}
	if len(st.OutsideVolumes) != 0 {
		t.Fatalf("expected no outside volumes after loading both slots")
	}

	if err := lib.OpenStorageDoor("Magazine1", ""); err != nil {
		t.Fatalf("open storage door for pickup: %v", err)
	}
	if err := lib.CloseStorageDoor("Magazine1", []DoorAction{{Action: "pickup", Address: st.Slots[0].Address}}); err != nil {
		t.Fatalf("close storage door pickup: %v", err)
	}
	st = lib.Status()
	if st.Slots[0].Volume != nil {
		t.Fatalf("expected picked-up slot to be empty")
	}
	if !containsBarcode(st.OutsideVolumes, "VOLA0001") {
		t.Fatalf("expected VOLA0001 outside after pickup")
	}
}

func TestStorageDoorIsIndependentPerMagazine(t *testing.T) {
	tmp := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = tmp
	cfg.Library.Magazines = []config.MagazineConfig{{ID: "Magazine1", Slots: 2}, {ID: "Magazine2", Slots: 2}}
	cfg.Library.DefaultCapacity = "1MiB"
	lib, err := New(cfg, nil, nil, nil)
	if err != nil {
		t.Fatalf("new library: %v", err)
	}

	if err := lib.OpenStorageDoor("Magazine1", ""); err != nil {
		t.Fatalf("open Magazine1 door: %v", err)
	}
	st := lib.Status()
	if !containsString(st.Doors.OpenMagazines, "Magazine1") {
		t.Fatalf("expected Magazine1 open, got %v", st.Doors.OpenMagazines)
	}
	if containsString(st.Doors.OpenMagazines, "Magazine2") {
		t.Fatalf("expected Magazine2 to remain closed, got %v", st.Doors.OpenMagazines)
	}

	if err := lib.CloseStorageDoor("Magazine2", nil); !errors.Is(err, ErrDoorClosed) {
		t.Fatalf("expected ErrDoorClosed closing an unopened magazine, got %v", err)
	}

	mag2Slot := st.Slots[2].Address // first slot of Magazine2
	if err := lib.CloseStorageDoor("Magazine1", []DoorAction{{Action: "pickup", Address: mag2Slot}}); err == nil {
		t.Fatalf("expected error scoping an action to a slot outside the open magazine")
	}
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestEventsUseStructuredCodeFields(t *testing.T) {
	lib := newTestLibrary(t)
	if _, err := lib.CreateManualCartridge(testTapeSet, "VOLEVENT"); err != nil {
		t.Fatalf("create outside volume: %v", err)
	}

	events := lib.Events()
	if len(events) == 0 {
		t.Fatalf("expected at least one event")
	}
	last := events[0]
	if last.Code == "" {
		t.Fatalf("expected structured event code")
	}
	if last.Type != last.Code {
		t.Fatalf("expected type alias to match code, got type=%q code=%q", last.Type, last.Code)
	}
	if last.Outcome == "" || last.Severity == "" || last.Category == "" || last.Operation == "" {
		t.Fatalf("expected derived event fields populated: %+v", last)
	}
}

func TestEventsPersistInStateRestore(t *testing.T) {
	lib := newTestLibrary(t)
	lib.RecordEvent(Event{Code: EventCodeAuthLoginFailure, Message: "failed login", Detail: map[string]string{"username": "Admin"}})

	st := lib.State()
	if len(st.Events) == 0 {
		t.Fatalf("expected events in persisted state")
	}

	restored, err := New(lib.cfg, &st, nil, nil)
	if err != nil {
		t.Fatalf("restore library: %v", err)
	}
	restoredEvents := restored.Events()
	if len(restoredEvents) == 0 {
		t.Fatalf("expected restored events")
	}
	if restoredEvents[0].Code != EventCodeAuthLoginFailure {
		t.Fatalf("unexpected restored event code: %s", restoredEvents[0].Code)
	}
}

// ---- latency simulation / robotic fault (0.5.0 rework) ----

func newTestLibraryWithLatency(t *testing.T, ls config.LatencySettings) *Library {
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
	cfg.Library.Latency = ls
	lib, err := New(cfg, nil, nil, nil)
	if err != nil {
		t.Fatalf("new library: %v", err)
	}
	return lib
}

// placeVolumeInFirstSlot puts a bare (no real backing file needed for
// Move/Load, which only symlink/read vol.Path - a dangling symlink is
// fine for these tests) volume directly into the first storage slot,
// bypassing the door-open/load-action ceremony other tests use, since
// these tests only care about latency/fault behavior on Move/Load/Unload.
func placeVolumeInFirstSlot(lib *Library, barcode string) {
	lib.slots[0].Volume = &Volume{Barcode: barcode, TapeSet: testTapeSet, Path: filepath.Join(lib.cfg.DataDir, barcode)}
}

func TestRoboticFaultBlocksMoveLoadUnload(t *testing.T) {
	lib := newTestLibrary(t)
	placeVolumeInFirstSlot(lib, "VOLA0001")
	fromAddr := lib.slots[0].Address
	toAddr := lib.slots[1].Address

	if err := lib.SetRoboticFault(true, RoboticFaultBlockedArm, "arm stuck"); err != nil {
		t.Fatalf("raise robotic fault: %v", err)
	}
	if !lib.Status().RoboticFault.Active {
		t.Fatalf("expected Status to report an active robotic fault")
	}

	if err := lib.Move(ElementRef{Kind: KindSlot, Address: fromAddr}, ElementRef{Kind: KindSlot, Address: toAddr}, ""); !errors.Is(err, ErrRoboticFault) {
		t.Fatalf("expected ErrRoboticFault from Move, got %v", err)
	}
	if err := lib.Load(ElementRef{Kind: KindSlot, Address: fromAddr}, 0, ""); !errors.Is(err, ErrRoboticFault) {
		t.Fatalf("expected ErrRoboticFault from Load, got %v", err)
	}

	// Door open/close is deliberately unaffected by a robotic fault
	// (confirmed scope: only Move/Load/Unload are gated).
	if err := lib.OpenStorageDoor("Magazine1", ""); err != nil {
		t.Fatalf("expected OpenStorageDoor to succeed despite robotic fault, got %v", err)
	}
	if err := lib.CloseStorageDoor("Magazine1", nil); err != nil {
		t.Fatalf("expected CloseStorageDoor to succeed despite robotic fault, got %v", err)
	}

	if err := lib.SetRoboticFault(false, "", ""); err != nil {
		t.Fatalf("clear robotic fault: %v", err)
	}
	if lib.Status().RoboticFault.Active {
		t.Fatalf("expected robotic fault cleared")
	}
	if err := lib.Move(ElementRef{Kind: KindSlot, Address: fromAddr}, ElementRef{Kind: KindSlot, Address: toAddr}, ""); err != nil {
		t.Fatalf("expected Move to succeed once fault is cleared, got %v", err)
	}
}

func TestRoboticFaultUnloadBlocked(t *testing.T) {
	lib := newTestLibrary(t)
	placeVolumeInFirstSlot(lib, "VOLA0001")
	fromAddr := lib.slots[0].Address
	if err := lib.Load(ElementRef{Kind: KindSlot, Address: fromAddr}, 0, ""); err != nil {
		t.Fatalf("load: %v", err)
	}

	if err := lib.SetRoboticFault(true, RoboticFaultMovementJam, ""); err != nil {
		t.Fatalf("raise robotic fault: %v", err)
	}
	if err := lib.Unload(0, ElementRef{Kind: KindSlot, Address: fromAddr}, ""); !errors.Is(err, ErrRoboticFault) {
		t.Fatalf("expected ErrRoboticFault from Unload, got %v", err)
	}
}

func TestSetRoboticFaultRejectsUnknownKind(t *testing.T) {
	lib := newTestLibrary(t)
	if err := lib.SetRoboticFault(true, "not-a-real-kind", ""); !errors.Is(err, ErrInvalidRoboticFaultKind) {
		t.Fatalf("expected ErrInvalidRoboticFaultKind, got %v", err)
	}
	if lib.Status().RoboticFault.Active {
		t.Fatalf("an invalid kind must not leave a fault active")
	}
}

func TestMagazinePINRequiredToOpen(t *testing.T) {
	lib := newTestLibrary(t)
	hash, err := secrethash.Hash("1234")
	if err != nil {
		t.Fatalf("hash pin: %v", err)
	}
	lib.UpdateMagazinePINHash(hash)

	if err := lib.OpenStorageDoor("Magazine1", ""); !errors.Is(err, ErrPINRequired) {
		t.Fatalf("expected ErrPINRequired with no PIN, got %v", err)
	}
	if err := lib.OpenStorageDoor("Magazine1", "0000"); !errors.Is(err, ErrInvalidPIN) {
		t.Fatalf("expected ErrInvalidPIN with a wrong PIN, got %v", err)
	}
	if containsString(lib.Status().Doors.OpenMagazines, "Magazine1") {
		t.Fatalf("magazine must not be open after a failed PIN check")
	}
	if err := lib.OpenStorageDoor("Magazine1", "1234"); err != nil {
		t.Fatalf("expected the correct PIN to open the door, got %v", err)
	}
	if !containsString(lib.Status().Doors.OpenMagazines, "Magazine1") {
		t.Fatalf("expected the magazine to be open after a correct PIN")
	}

	// The PIN is checked on every call, even one that's about to no-op
	// because the door is already open - a wrong PIN can't be used to
	// "probe" an already-open door for free.
	if err := lib.OpenStorageDoor("Magazine1", "wrong"); !errors.Is(err, ErrInvalidPIN) {
		t.Fatalf("expected ErrInvalidPIN to still be enforced while already open, got %v", err)
	}

	// Clearing the PIN (presence-implies-protection) makes every magazine
	// open freely again, no PIN required at all.
	lib.UpdateMagazinePINHash("")
	if err := lib.CloseStorageDoor("Magazine1", nil); err != nil {
		t.Fatalf("close storage door: %v", err)
	}
	if err := lib.OpenStorageDoor("Magazine1", ""); err != nil {
		t.Fatalf("expected no PIN required once cleared, got %v", err)
	}
}

func TestMailboxPINIsIndependentPerMailbox(t *testing.T) {
	lib := newTestLibrary(t)
	hash, err := secrethash.Hash("4321")
	if err != nil {
		t.Fatalf("hash pin: %v", err)
	}
	// Set directly on the in-memory map (mirrors how buildTopologyLocked
	// populates it from config) - there's no public per-mailbox setter,
	// only the store-backed Admin API path exercised at the api-package
	// level.
	lib.mailboxPINHash["Mailbox1"] = hash

	if err := lib.OpenIODoor("Mailbox1", ""); !errors.Is(err, ErrPINRequired) {
		t.Fatalf("expected ErrPINRequired with no PIN, got %v", err)
	}
	if err := lib.OpenIODoor("Mailbox1", "0000"); !errors.Is(err, ErrInvalidPIN) {
		t.Fatalf("expected ErrInvalidPIN with a wrong PIN, got %v", err)
	}
	if err := lib.OpenIODoor("Mailbox1", "4321"); err != nil {
		t.Fatalf("expected the correct PIN to open the door, got %v", err)
	}

	// A magazine PIN (or another mailbox's PIN) must never satisfy a
	// different mailbox's own PIN - each is independent.
	if err := lib.CloseIODoor("Mailbox1", nil); err != nil {
		t.Fatalf("close io door: %v", err)
	}
	magHash, err := secrethash.Hash("9999")
	if err != nil {
		t.Fatalf("hash magazine pin: %v", err)
	}
	lib.UpdateMagazinePINHash(magHash)
	if err := lib.OpenIODoor("Mailbox1", "9999"); !errors.Is(err, ErrInvalidPIN) {
		t.Fatalf("expected the magazine PIN to not satisfy this mailbox's own PIN, got %v", err)
	}
}

func TestRoboticFaultPersistsAcrossRestore(t *testing.T) {
	lib := newTestLibrary(t)
	if err := lib.SetRoboticFault(true, RoboticFaultDropFailure, "dropped a cartridge"); err != nil {
		t.Fatalf("raise robotic fault: %v", err)
	}

	st := lib.State()
	restored, err := New(lib.cfg, &st, nil, nil)
	if err != nil {
		t.Fatalf("restore library: %v", err)
	}
	rf := restored.Status().RoboticFault
	if !rf.Active || rf.Kind != RoboticFaultDropFailure {
		t.Fatalf("expected robotic fault to survive restore, got %+v", rf)
	}
}

// TestReconfigureAddingMagazineDoesNotMoveExistingVolumes is an
// end-to-end regression test for the critical bug reported against
// magazine creation: adding a new magazine must never shift an existing,
// earlier magazine's slot addresses or reassign its volumes to the new
// magazine. buildTopologyLocked computes every Slot.Address live, in
// magazine list order (see its doc comment) - appending a new magazine at
// the end of the list never changes anything about the magazines already
// listed before it, so an existing magazine's addresses (and, thanks to
// Reconfigure's identity-based preservation, its volumes) are unaffected
// either way.
func TestReconfigureAddingMagazineDoesNotMoveExistingVolumes(t *testing.T) {
	lib := newTestLibrary(t)
	placeVolumeInFirstSlot(lib, "VOLA0001")
	firstAddr := lib.slots[0].Address
	firstMagID := lib.slots[0].MagazineID

	cfg := lib.cfg
	cfg.Library.Magazines = append(append([]config.MagazineConfig(nil), cfg.Library.Magazines...),
		config.MagazineConfig{ID: "Cleaning Tapes", Slots: 5})
	if err := lib.Reconfigure(cfg); err != nil {
		t.Fatalf("reconfigure: %v", err)
	}

	found := false
	for _, s := range lib.slots {
		if s.Address == firstAddr {
			found = true
			if s.MagazineID != firstMagID {
				t.Fatalf("slot %d's magazine changed from %q to %q after adding a new magazine", firstAddr, firstMagID, s.MagazineID)
			}
			if s.Volume == nil || s.Volume.Barcode != "VOLA0001" {
				t.Fatalf("expected volume VOLA0001 to remain at slot %d, got %+v", firstAddr, s.Volume)
			}
		}
	}
	if !found {
		t.Fatalf("slot %d no longer exists after adding a new magazine", firstAddr)
	}
}

// TestReconfigureDeletingMagazineRenumbersButKeepsOtherMagazinesVolumes
// reproduces the exact real-world report that followed the creation-only
// fix above: a 20-slot "Magazine5" and a 5-slot "Cleaning Tapes" magazine
// both exist, a tape sits in Cleaning Tapes, Magazine5 (which was created
// *first*, so it occupies the lower address range) gets deleted. Cleaning
// Tapes' flat Address is now *expected* to shift down (from 21 to 1 -
// addressing is recomputed live from magazine list order on every
// rebuild, see buildTopologyLocked's doc comment, so a gapless library
// stays gapless after a deletion instead of leaving Magazine5's old range
// stranded). What must never happen is losing or misattributing the tape
// in the process: Reconfigure now preserves volume placement by
// (MagazineID, offset) identity, not by raw address (see its doc
// comment), so the tape must be found - correctly - at Cleaning Tapes'
// new address, not left behind at the old one or silently handed to a
// different entity that happens to land there.
func TestReconfigureDeletingMagazineRenumbersButKeepsOtherMagazinesVolumes(t *testing.T) {
	tmp := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = tmp
	cfg.Library.Magazines = []config.MagazineConfig{
		{ID: "Magazine5", Slots: 20},
		{ID: "Cleaning Tapes", Slots: 5},
	}
	cfg.Library.DefaultCapacity = "1MiB"
	lib, err := New(cfg, nil, nil, nil)
	if err != nil {
		t.Fatalf("new library: %v", err)
	}

	var cleaningSlot *Slot
	for _, s := range lib.slots {
		if s.MagazineID == "Cleaning Tapes" {
			cleaningSlot = s
			break
		}
	}
	if cleaningSlot == nil {
		t.Fatalf("Cleaning Tapes has no slots")
	}
	if cleaningSlot.Address != 21 {
		t.Fatalf("expected Cleaning Tapes' first slot at address 21 (right after Magazine5's 20), got %d", cleaningSlot.Address)
	}
	cleaningSlot.Volume = &Volume{Barcode: "00001CLN", Path: filepath.Join(tmp, "00001CLN"), Cleaning: true, CleaningState: CleaningTapeAvailable}

	// Delete Magazine5 (simulating the Admin API's handleDeleteMagazine,
	// which just resubmits the topology minus that one magazine).
	newCfg := lib.cfg
	newCfg.Library.Magazines = []config.MagazineConfig{
		{ID: "Cleaning Tapes", Slots: 5},
	}
	if err := lib.Reconfigure(newCfg); err != nil {
		t.Fatalf("reconfigure (delete Magazine5): %v", err)
	}

	if len(lib.slots) != 5 {
		t.Fatalf("expected exactly 5 remaining slots (Cleaning Tapes only), got %d", len(lib.slots))
	}
	var found *Slot
	for _, s := range lib.slots {
		if s.MagazineID == "Cleaning Tapes" && s.Volume != nil {
			found = s
		}
	}
	if found == nil {
		t.Fatalf("expected Cleaning Tapes' tape to still be tracked after deleting Magazine5, got slots %+v", lib.slots)
	}
	if found.Address != 1 {
		t.Fatalf("expected Cleaning Tapes' occupied slot to renumber to address 1 after deleting Magazine5, got %d", found.Address)
	}
	if found.Label != "1.1" {
		t.Fatalf("expected Cleaning Tapes' occupied slot to renumber to label \"1.1\" after deleting Magazine5, got %q", found.Label)
	}
	if found.Volume.Barcode != "00001CLN" {
		t.Fatalf("expected tape 00001CLN to still be in Cleaning Tapes' first slot after deleting Magazine5, got %+v", found.Volume)
	}
}

// TestReconfigureShiftsDriveOriginWithItsSlot is a regression test for the
// Drive.Origin staleness risk identified while designing gapless
// addressing: a drive that still has a tape checked out remembers where it
// came from as a flat Address (see ElementRef), not an identity - if a
// magazine ahead of the tape's home magazine is added/deleted/resized and
// shifts that magazine's addresses, Origin must shift by the same amount
// (see shiftOriginAcrossRebuild), or it would end up pointing at whatever
// unrelated slot now happens to sit at the old address once that range is
// gapless and thus actually reused.
func TestReconfigureShiftsDriveOriginWithItsSlot(t *testing.T) {
	lib := newTestLibrary(t)
	placeVolumeInFirstSlot(lib, "VOLA0001")
	homeMagID := lib.slots[0].MagazineID
	homeAddr := lib.slots[0].Address

	if err := lib.Load(ElementRef{Kind: KindSlot, Address: homeAddr}, 0, ""); err != nil {
		t.Fatalf("load: %v", err)
	}
	origin, err := lib.DriveOriginSlot(0)
	if err != nil {
		t.Fatalf("drive origin slot: %v", err)
	}
	if origin != homeAddr {
		t.Fatalf("expected origin address %d right after load, got %d", homeAddr, origin)
	}

	// Insert a new 3-slot magazine *ahead* of the existing one, shifting
	// the existing magazine's (and thus the checked-out tape's origin
	// slot's) flat Address by 3.
	cfg := lib.cfg
	cfg.Library.Magazines = append([]config.MagazineConfig{{ID: "NewFirst", Slots: 3}}, cfg.Library.Magazines...)
	if err := lib.Reconfigure(cfg); err != nil {
		t.Fatalf("reconfigure: %v", err)
	}

	newOrigin, err := lib.DriveOriginSlot(0)
	if err != nil {
		t.Fatalf("drive origin slot after reconfigure: %v", err)
	}
	if newOrigin != homeAddr+3 {
		t.Fatalf("expected origin address to shift from %d to %d after inserting a 3-slot magazine ahead of it, got %d", homeAddr, homeAddr+3, newOrigin)
	}
	var atNewOrigin *Slot
	for _, s := range lib.slots {
		if s.Address == newOrigin {
			atNewOrigin = s
		}
	}
	if atNewOrigin == nil || atNewOrigin.MagazineID != homeMagID {
		t.Fatalf("expected address %d to belong to magazine %q, got %+v", newOrigin, homeMagID, atNewOrigin)
	}
}

// TestReconfigureAcrossTwoLogicalLibrariesPreservesVolumesAndAddressing is
// a regression test for the exact scenario CLAUDE.md flags as untested by
// anything with fewer than two logical libraries: two logical libraries
// each own a different magazine - Library1 owns the physically-first one,
// Library2 owns a later one whose addresses aren't the lowest in the
// library. Deleting Library1's (empty) magazine must correctly renumber
// Library2's magazine's flat addresses (it's no longer second in the
// list) while keeping Library2's volume tracked and reachable through the
// logical-library-scoped view (LogicalLibraryStatus), not just the
// unscoped one.
func TestReconfigureAcrossTwoLogicalLibrariesPreservesVolumesAndAddressing(t *testing.T) {
	tmp := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = tmp
	cfg.Library.Magazines = []config.MagazineConfig{
		{ID: "Mag1", Slots: 5},
		{ID: "Mag2", Slots: 5},
	}
	cfg.Library.DriveDevices = []config.DriveDeviceConfig{
		{DevicePath: filepath.Join(tmp, "drives", "drive0")},
		{DevicePath: filepath.Join(tmp, "drives", "drive1")},
	}
	cfg.Library.DefaultCapacity = "1MiB"
	cfg.Library.LogicalLibraries = []config.LogicalLibraryConfig{
		{Name: "Library1", Drives: []int{0}, Magazines: []string{"Mag1"}},
		{Name: "Library2", Drives: []int{1}, Magazines: []string{"Mag2"}},
	}
	lib, err := New(cfg, nil, nil, nil)
	if err != nil {
		t.Fatalf("new library: %v", err)
	}

	var mag2Slot *Slot
	for _, s := range lib.slots {
		if s.MagazineID == "Mag2" {
			mag2Slot = s
			break
		}
	}
	if mag2Slot == nil {
		t.Fatalf("Mag2 has no slots")
	}
	if mag2Slot.Address != 6 {
		t.Fatalf("expected Mag2's first slot at address 6 (right after Mag1's 5), got %d", mag2Slot.Address)
	}
	mag2Slot.Volume = &Volume{Barcode: "LIB20001", Path: filepath.Join(tmp, "LIB20001")}

	before := lib.LogicalLibraryStatus("Library2")
	if len(before.Slots) != 5 || before.Slots[0].Volume == nil || before.Slots[0].Volume.Barcode != "LIB20001" {
		t.Fatalf("expected Library2's scoped status to show the volume before reconfigure, got %+v", before.Slots)
	}

	// Delete Mag1 (Library1's magazine - empty, a legitimate delete) -
	// Mag2's flat addresses must renumber down to 1-5.
	newCfg := lib.cfg
	newCfg.Library.Magazines = []config.MagazineConfig{{ID: "Mag2", Slots: 5}}
	newCfg.Library.LogicalLibraries = []config.LogicalLibraryConfig{
		{Name: "Library2", Drives: []int{1}, Magazines: []string{"Mag2"}},
	}
	if err := lib.Reconfigure(newCfg); err != nil {
		t.Fatalf("reconfigure (delete Mag1): %v", err)
	}

	after := lib.LogicalLibraryStatus("Library2")
	if len(after.Slots) != 5 {
		t.Fatalf("expected Library2 to still have 5 slots, got %d", len(after.Slots))
	}
	var found *Slot
	for _, s := range after.Slots {
		if s.Volume != nil {
			found = s
		}
	}
	if found == nil {
		t.Fatalf("expected Library2's volume to survive Mag1's deletion, got slots %+v", after.Slots)
	}
	if found.Address != 1 {
		t.Fatalf("expected Mag2's occupied slot to renumber to address 1 after Mag1's deletion, got %d", found.Address)
	}
	if found.Label != "1.1" {
		t.Fatalf("expected Mag2's occupied slot to renumber to label \"1.1\" after Mag1's deletion, got %q", found.Label)
	}
	if found.Volume.Barcode != "LIB20001" {
		t.Fatalf("expected volume LIB20001 to survive, got %+v", found.Volume)
	}
}

func TestReconfigureCarriesRoboticFault(t *testing.T) {
	lib := newTestLibrary(t)
	if err := lib.SetRoboticFault(true, RoboticFaultPickupFailure, ""); err != nil {
		t.Fatalf("raise robotic fault: %v", err)
	}
	if err := lib.Reconfigure(lib.cfg); err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	if !lib.Status().RoboticFault.Active {
		t.Fatalf("expected robotic fault to survive Reconfigure (it's not tied to any drive/slot/mailbox)")
	}
}

func TestLatencySleepsAreApplied(t *testing.T) {
	const delay = 30 * time.Millisecond
	ls := config.LatencySettings{
		Enabled: true, DriveLoad: delay.String(), DriveUnload: delay.String(),
		TapePositioning: "0s", RobotMoveTape: delay.String(), RobotMoveScan: "0s",
		MagazineScan: "0s", DoorAction: "0s",
	}
	lib := newTestLibraryWithLatency(t, ls)
	placeVolumeInFirstSlot(lib, "VOLA0001")
	fromAddr := lib.slots[0].Address
	toAddr := lib.slots[1].Address

	start := time.Now()
	if err := lib.Move(ElementRef{Kind: KindSlot, Address: fromAddr}, ElementRef{Kind: KindSlot, Address: toAddr}, ""); err != nil {
		t.Fatalf("move: %v", err)
	}
	if elapsed := time.Since(start); elapsed < delay {
		t.Errorf("expected Move to take at least %s (RobotMoveTape), took %s", delay, elapsed)
	}

	start = time.Now()
	if err := lib.Load(ElementRef{Kind: KindSlot, Address: toAddr}, 0, ""); err != nil {
		t.Fatalf("load: %v", err)
	}
	if elapsed := time.Since(start); elapsed < delay {
		t.Errorf("expected Load to take at least %s (DriveLoad), took %s", delay, elapsed)
	}

	start = time.Now()
	if err := lib.Unload(0, ElementRef{Kind: KindSlot, Address: toAddr}, ""); err != nil {
		t.Fatalf("unload: %v", err)
	}
	if elapsed := time.Since(start); elapsed < delay {
		t.Errorf("expected Unload to take at least %s (DriveUnload), took %s", delay, elapsed)
	}
}

// TestLoadCreatesDeviceSymlinkOnlyOnceDriveIsReady is the direct
// regression test for the reported bug: the Bareos-facing device-path
// symlink must not appear until the drive is genuinely ready (mechanical
// load AND tape positioning both complete), not immediately when Load is
// called - otherwise Bareos could open/read/write through the device
// before gotochangerd itself considers the tape loaded, racing ahead of
// the simulated mechanical sequence and the live activity narration.
func TestLoadCreatesDeviceSymlinkOnlyOnceDriveIsReady(t *testing.T) {
	const delay = 60 * time.Millisecond
	ls := config.LatencySettings{
		Enabled: true, DriveLoad: delay.String(), DriveUnload: "0s",
		TapePositioning: delay.String(), RobotMoveTape: "0s", RobotMoveScan: "0s",
		MagazineScan: "0s", DoorAction: "0s",
	}
	lib := newTestLibraryWithLatency(t, ls)
	placeVolumeInFirstSlot(lib, "VOLA0001")
	fromAddr := lib.slots[0].Address
	devicePath := lib.drives[0].DevicePath

	done := make(chan error, 1)
	go func() {
		done <- lib.Load(ElementRef{Kind: KindSlot, Address: fromAddr}, 0, "")
	}()

	// Well before DriveLoad+TapePositioning (2*delay) have elapsed.
	time.Sleep(delay / 2)
	if _, err := os.Lstat(devicePath); !os.IsNotExist(err) {
		t.Fatalf("expected no device symlink to exist yet mid-load, got err=%v", err)
	}

	if err := <-done; err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := os.Lstat(devicePath); err != nil {
		t.Fatalf("expected the device symlink to exist once Load returns (drive ready), got: %v", err)
	}
}

// TestUnloadWaitsForActivitySettleDelayBeforeStoppingWatcher confirms
// Unload always waits at least driveActivityUnloadSettleDelay before
// tearing down the drive's activity watcher - see that var's doc comment
// for the real race this closes (a fast write immediately followed by
// Unload could otherwise have its activity report silently dropped).
// Uses a library with latency disabled, so without this mechanism Unload
// would otherwise be near-instant.
func TestUnloadWaitsForActivitySettleDelayBeforeStoppingWatcher(t *testing.T) {
	orig := driveActivityUnloadSettleDelay
	driveActivityUnloadSettleDelay = 50 * time.Millisecond
	t.Cleanup(func() { driveActivityUnloadSettleDelay = orig })

	lib := newTestLibrary(t)
	placeVolumeInFirstSlot(lib, "VOLA0001")
	fromAddr := lib.slots[0].Address
	if err := lib.Load(ElementRef{Kind: KindSlot, Address: fromAddr}, 0, ""); err != nil {
		t.Fatalf("load: %v", err)
	}

	start := time.Now()
	if err := lib.Unload(0, ElementRef{Kind: KindSlot, Address: fromAddr}, ""); err != nil {
		t.Fatalf("unload: %v", err)
	}
	if elapsed := time.Since(start); elapsed < driveActivityUnloadSettleDelay {
		t.Fatalf("expected Unload to wait at least %s for the activity watcher to settle before tearing down, took %s", driveActivityUnloadSettleDelay, elapsed)
	}
}

func TestLatencyDisabledMeansNoSleep(t *testing.T) {
	ls := config.LatencySettings{Enabled: false, DriveLoad: "5s", DriveUnload: "5s", RobotMoveTape: "5s"}
	lib := newTestLibraryWithLatency(t, ls)
	placeVolumeInFirstSlot(lib, "VOLA0001")
	fromAddr := lib.slots[0].Address
	toAddr := lib.slots[1].Address

	start := time.Now()
	if err := lib.Move(ElementRef{Kind: KindSlot, Address: fromAddr}, ElementRef{Kind: KindSlot, Address: toAddr}, ""); err != nil {
		t.Fatalf("move: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("expected Move to be near-instant with latency disabled despite a 5s configured value, took %s", elapsed)
	}
}

func TestMagazineScanLatencyAppliedOnStorageDoorClose(t *testing.T) {
	const delay = 30 * time.Millisecond
	ls := config.LatencySettings{
		Enabled: true, DriveLoad: "0s", DriveUnload: "0s", TapePositioning: "0s",
		RobotMoveTape: "0s", RobotMoveScan: delay.String(), MagazineScan: delay.String(), DoorAction: "0s",
	}
	lib := newTestLibraryWithLatency(t, ls)
	if err := lib.OpenStorageDoor("Magazine1", ""); err != nil {
		t.Fatalf("open storage door: %v", err)
	}
	start := time.Now()
	if err := lib.CloseStorageDoor("Magazine1", nil); err != nil {
		t.Fatalf("close storage door: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 2*delay {
		t.Errorf("expected CloseStorageDoor to take at least %s (RobotMoveScan+MagazineScan), took %s", 2*delay, elapsed)
	}
}

// TestMoveEmitsStartedBeforeSuccess verifies Move emits the coarse,
// audited ROBOTICS.MOVE.STARTED bracket before the transit sleep and
// ROBOTICS.MOVE.SUCCESS after - the atomic, physical-step narration
// ("moving to slot 3", "grabbed tape ...") this pair used to carry in its
// message text now lives entirely in the live-only arm-step channel (see
// TestArmStepsCapturedAndNeverAudited), not in these persisted/SNMP'd
// events, which only ever bracket the whole operation.
func TestMoveEmitsStartedBeforeSuccess(t *testing.T) {
	lib := newTestLibrary(t)
	placeVolumeInFirstSlot(lib, "VOLA0001")
	fromAddr := lib.slots[0].Address
	toAddr := lib.slots[1].Address

	if err := lib.Move(ElementRef{Kind: KindSlot, Address: fromAddr}, ElementRef{Kind: KindSlot, Address: toAddr}, ""); err != nil {
		t.Fatalf("move: %v", err)
	}

	evs := lib.Events() // newest first
	if len(evs) < 2 {
		t.Fatalf("expected at least 2 events, got %d", len(evs))
	}
	if evs[0].Code != EventCodeRoboticsMoveSuccess || evs[1].Code != EventCodeRoboticsMoveStarted {
		t.Fatalf("expected [MOVE.STARTED, MOVE.SUCCESS] as the last two events (newest first), got [%s, %s]", evs[0].Code, evs[1].Code)
	}
}

// TestStorageDoorCloseEmitsPerSlotScanEvents verifies the magazine scan
// phase logs one ROBOTICS.SCAN.STORAGE.SLOT event per slot in the
// magazine (in address order, as the arm passes each one) before the
// final scan-complete summary event, rather than a single lump sleep.
// TestStorageDoorCloseEmitsPerSlotScanEvents verifies the per-slot
// magazine-scan narration is live-only (see Library.recordArmStep) - it
// must appear in ArmSteps(), in address order, with occupied/empty
// status, and must NOT appear in the persisted/audited Events() log or
// fire an SNMP trap on every slot - only the coarse "closed storage
// door"/"scanned magazine ... contents" events do that (see Context in
// the plan this implements: the per-slot narration was added in the same
// prior rework as Move/Load's granular steps, so it gets the same
// live-only treatment for consistency).
func TestStorageDoorCloseEmitsPerSlotScanEvents(t *testing.T) {
	const delay = 30 * time.Millisecond
	ls := config.LatencySettings{
		Enabled: true, DriveLoad: "0s", DriveUnload: "0s", TapePositioning: "0s",
		RobotMoveTape: "0s", RobotMoveScan: "0s", MagazineScan: delay.String(), DoorAction: "0s",
	}
	lib := newTestLibraryWithLatency(t, ls)
	placeVolumeInFirstSlot(lib, "VOLA0001") // Magazine1 has 2 slots; slot 0 occupied, slot 1 empty
	if err := lib.OpenStorageDoor("Magazine1", ""); err != nil {
		t.Fatalf("open storage door: %v", err)
	}
	if err := lib.CloseStorageDoor("Magazine1", nil); err != nil {
		t.Fatalf("close storage door: %v", err)
	}

	steps := lib.ArmSteps() // append order, oldest first
	var slotSteps []ArmStep
	for _, s := range steps {
		if strings.HasPrefix(s.Message, "scanning slot ") {
			slotSteps = append(slotSteps, s)
		}
	}
	if len(slotSteps) != 2 {
		t.Fatalf("expected 2 per-slot scan steps (one per Magazine1 slot), got %d: %+v", len(slotSteps), slotSteps)
	}
	wantSlot0 := fmt.Sprintf("scanning slot %d:", lib.slots[0].Address)
	wantSlot1 := fmt.Sprintf("scanning slot %d:", lib.slots[1].Address)
	if !strings.HasPrefix(slotSteps[0].Message, wantSlot0) || !strings.HasPrefix(slotSteps[1].Message, wantSlot1) {
		t.Fatalf("expected slot scan steps in address order, got %+v", slotSteps)
	}
	if !strings.Contains(slotSteps[0].Message, "occupied") || !strings.Contains(slotSteps[1].Message, "empty") {
		t.Fatalf("expected occupied/empty status in scan step messages, got %+v", slotSteps)
	}

	for _, e := range lib.Events() {
		if strings.Contains(e.Message, "scanning slot") {
			t.Fatalf("expected per-slot scan narration to never appear in the persisted event log, found %+v", e)
		}
	}
	foundDoorClosed, foundScanned := false, false
	for _, e := range lib.Events() {
		if e.Code == EventCodeRoboticsDoorStorageCloseSuccess {
			foundDoorClosed = true
		}
		if e.Code == EventCodeRoboticsScanStorageSuccess {
			foundScanned = true
		}
	}
	if !foundDoorClosed || !foundScanned {
		t.Fatalf("expected the coarse door-closed/scanned-magazine events to still be audited, found door=%v scanned=%v", foundDoorClosed, foundScanned)
	}
}

// fakePhaseNotifier records every NotifyPhase call it receives, in order.
type fakePhaseNotifier struct {
	mu  sync.Mutex
	got []string // "kind:id:phase"
}

func (f *fakePhaseNotifier) NotifyPhase(kind, id, phase string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, kind+":"+id+":"+phase)
}

// NotifyArm satisfies PhaseNotifier; fakePhaseNotifier only records door
// phases (see snapshot), arm-state notifications are ignored here.
func (f *fakePhaseNotifier) NotifyArm(ArmState, ArmStep) {}

func (f *fakePhaseNotifier) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

func TestDoorPhaseVisibleDuringOpenAndClearedAfter(t *testing.T) {
	const delay = 100 * time.Millisecond
	ls := config.LatencySettings{
		Enabled: true, DriveLoad: "0s", DriveUnload: "0s", TapePositioning: "0s",
		RobotMoveTape: "0s", RobotMoveScan: "0s", MagazineScan: "0s", DoorAction: delay.String(),
	}
	lib := newTestLibraryWithLatency(t, ls)
	pn := &fakePhaseNotifier{}
	lib.SetPhaseNotifier(pn)

	done := make(chan error, 1)
	go func() { done <- lib.OpenStorageDoor("Magazine1", "") }()

	// Poll DoorPhases() (its own, independent mutex - must never block
	// behind the open call's l.mu, which is held for the whole delay).
	deadline := time.Now().Add(2 * time.Second)
	sawOpening := false
	for time.Now().Before(deadline) {
		if lib.DoorPhases()["magazine:Magazine1"] == "opening" {
			sawOpening = true
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !sawOpening {
		t.Fatalf("expected to observe phase %q for magazine:Magazine1 while OpenStorageDoor was in flight", "opening")
	}

	if err := <-done; err != nil {
		t.Fatalf("open storage door: %v", err)
	}
	if phases := lib.DoorPhases(); len(phases) != 0 {
		t.Errorf("expected no in-progress phases after OpenStorageDoor returned, got %v", phases)
	}

	got := pn.snapshot()
	if len(got) < 2 || got[0] != "magazine:Magazine1:opening" || got[len(got)-1] != "magazine:Magazine1:" {
		t.Errorf("expected phase notifications to start with opening and end with a clear, got %v", got)
	}
}

func TestStorageDoorClosePhaseTransitionsToScanning(t *testing.T) {
	const delay = 30 * time.Millisecond
	ls := config.LatencySettings{
		Enabled: true, DriveLoad: "0s", DriveUnload: "0s", TapePositioning: "0s",
		RobotMoveTape: "0s", RobotMoveScan: delay.String(), MagazineScan: delay.String(), DoorAction: "0s",
	}
	lib := newTestLibraryWithLatency(t, ls)
	if err := lib.OpenStorageDoor("Magazine1", ""); err != nil {
		t.Fatalf("open storage door: %v", err)
	}
	pn := &fakePhaseNotifier{}
	lib.SetPhaseNotifier(pn)

	if err := lib.CloseStorageDoor("Magazine1", nil); err != nil {
		t.Fatalf("close storage door: %v", err)
	}
	if phases := lib.DoorPhases(); len(phases) != 0 {
		t.Errorf("expected no in-progress phases after CloseStorageDoor returned, got %v", phases)
	}
	got := pn.snapshot()
	want := []string{"magazine:Magazine1:closing", "magazine:Magazine1:scanning", "magazine:Magazine1:"}
	if len(got) != len(want) {
		t.Fatalf("expected phase sequence %v, got %v", want, got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("phase transition %d: expected %q, got %q (full sequence: %v)", i, w, got[i], got)
		}
	}
}

func TestIODoorCloseHasNoScanningPhase(t *testing.T) {
	const delay = 30 * time.Millisecond
	ls := config.LatencySettings{
		Enabled: true, DriveLoad: "0s", DriveUnload: "0s", TapePositioning: "0s",
		RobotMoveTape: "0s", RobotMoveScan: delay.String(), MagazineScan: delay.String(), DoorAction: delay.String(),
	}
	lib := newTestLibraryWithLatency(t, ls)
	if err := lib.OpenIODoor("Mailbox1", ""); err != nil {
		t.Fatalf("open io door: %v", err)
	}
	pn := &fakePhaseNotifier{}
	lib.SetPhaseNotifier(pn)

	if err := lib.CloseIODoor("Mailbox1", nil); err != nil {
		t.Fatalf("close io door: %v", err)
	}
	got := pn.snapshot()
	want := []string{"mailbox:Mailbox1:closing", "mailbox:Mailbox1:"}
	if len(got) != len(want) {
		t.Fatalf("expected phase sequence %v (no scanning phase for a mailbox), got %v", want, got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("phase transition %d: expected %q, got %q (full sequence: %v)", i, w, got[i], got)
		}
	}
}

func TestOffsiteSendRejectedWhenDisabled(t *testing.T) {
	lib := newTestLibrary(t)
	placeVolumeInFirstSlot(lib, "VOLA0001")
	if _, err := lib.OffsiteSend(ElementRef{Kind: KindSlot, Address: lib.slots[0].Address}); !errors.Is(err, ErrOffsiteDisabled) {
		t.Fatalf("expected ErrOffsiteDisabled, got %v", err)
	}
}

func TestOffsiteRecallRejectedWhenDisabled(t *testing.T) {
	lib := newTestLibrary(t)
	if err := lib.OffsiteRecall("VOLA0001", ElementRef{Kind: KindSlot, Address: lib.slots[0].Address}); !errors.Is(err, ErrOffsiteDisabled) {
		t.Fatalf("expected ErrOffsiteDisabled, got %v", err)
	}
}

func TestOffsiteSendAndRecallSucceedWhenEnabled(t *testing.T) {
	lib := newTestLibrary(t)
	placeVolumeInFirstSlot(lib, "VOLA0001")
	addr := lib.slots[0].Address

	newCfg := lib.cfg
	newCfg.Library.OffsiteLocation = true
	if err := lib.Reconfigure(newCfg); err != nil {
		t.Fatalf("reconfigure: %v", err)
	}

	vol, err := lib.OffsiteSend(ElementRef{Kind: KindSlot, Address: addr})
	if err != nil {
		t.Fatalf("offsite send: %v", err)
	}
	if vol.Barcode != "VOLA0001" {
		t.Fatalf("unexpected volume sent offsite: %+v", vol)
	}
	if !lib.Status().OffsiteEnabled {
		t.Fatalf("expected Status().OffsiteEnabled to be true once offsite is enabled")
	}

	if err := lib.OffsiteRecall("VOLA0001", ElementRef{Kind: KindSlot, Address: addr}); err != nil {
		t.Fatalf("offsite recall: %v", err)
	}
	if lib.slots[0].Volume == nil || lib.slots[0].Volume.Barcode != "VOLA0001" {
		t.Fatalf("expected volume back in its slot after recall")
	}
}

func TestRotateOffsiteNoOpWhenDisabled(t *testing.T) {
	lib := newTestLibrary(t)
	placeVolumeInFirstSlot(lib, "VOLA0001")
	lib.slots[0].Volume.Full = true

	lib.RotateOffsite(1)

	if lib.slots[0].Volume == nil {
		t.Fatalf("expected volume to remain in its slot when offsite is disabled")
	}
	if len(lib.OffsiteVolumes()) != 0 {
		t.Fatalf("expected no volumes sent offsite when offsite is disabled")
	}
}

// slowDetailRangeNotifier simulates internal/snmp.Sender.Notify: it ranges
// over the event's Detail map (as trapVarbindsForEvent does) with a small
// sleep per key, holding the iteration open long enough for a concurrent
// mutation of the same map to be observed by the race detector if the two
// aren't properly isolated.
type slowDetailRangeNotifier struct {
	started chan struct{}
}

func (n *slowDetailRangeNotifier) Notify(e Event) {
	close(n.started)
	for k := range e.Detail {
		_ = k
		time.Sleep(time.Millisecond)
	}
}

// TestEmitNotifyDoesNotRaceWithAnnotateEventsSince reproduces the crash seen
// in production ("fatal error: concurrent map iteration and map write" in
// internal/snmp.trapVarbindsForEvent): emit's async "go l.notifier.Notify(e)"
// used to hand the notifier the *same* Detail map stored in l.events, which
// AnnotateEventsSince can mutate in place (under l.mu) while the unsynchronized
// notifier goroutine is still ranging over it. Run with -race.
func TestEmitNotifyDoesNotRaceWithAnnotateEventsSince(t *testing.T) {
	tmp := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = tmp
	notifier := &slowDetailRangeNotifier{started: make(chan struct{})}
	lib, err := New(cfg, nil, notifier, nil)
	if err != nil {
		t.Fatalf("new library: %v", err)
	}

	lib.mu.Lock()
	lib.emit("test-event", "test message", map[string]string{"a": "1", "b": "2", "c": "3"})
	lib.mu.Unlock()

	<-notifier.started
	lib.AnnotateEventsSince(time.Time{}, "actor", "source", map[string]string{"d": "4", "e": "5"})
}
