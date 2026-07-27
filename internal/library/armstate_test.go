package library

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swenske/gotochanger/internal/config"
)

// TestArmBusyReflectsRealOperationRegardlessOfCaller is the direct
// regression test for the bug this rework fixes: the dashboard's old
// busy/idle LED was driven by a same-browser-tab-only client counter, so
// it never moved for a real Bareos job (which reaches gotochangerd via
// gotochanger-changer over the trusted socket, with no relationship to
// any browser tab). ArmState().Busy is set by Library.Move itself, so it
// must be observably true mid-operation regardless of who's calling it -
// this test calls Move directly, with no HTTP/browser layer at all.
func TestArmBusyReflectsRealOperationRegardlessOfCaller(t *testing.T) {
	const delay = 100 * time.Millisecond
	ls := config.LatencySettings{
		Enabled: true, DriveLoad: "0s", DriveUnload: "0s", TapePositioning: "0s",
		RobotMoveTape: delay.String(), RobotMoveScan: "0s", MagazineScan: "0s", DoorAction: "0s",
	}
	lib := newTestLibraryWithLatency(t, ls)
	placeVolumeInFirstSlot(lib, "VOLA0001")
	fromAddr := lib.slots[0].Address
	toAddr := lib.slots[1].Address

	if lib.ArmState().Busy {
		t.Fatalf("expected arm to be idle before any operation")
	}

	done := make(chan error, 1)
	go func() {
		done <- lib.Move(ElementRef{Kind: KindSlot, Address: fromAddr}, ElementRef{Kind: KindSlot, Address: toAddr}, "")
	}()

	deadline := time.Now().Add(2 * time.Second)
	sawBusy := false
	for time.Now().Before(deadline) {
		if lib.ArmState().Busy {
			sawBusy = true
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !sawBusy {
		t.Fatalf("expected to observe ArmState().Busy while Move was in flight")
	}

	if err := <-done; err != nil {
		t.Fatalf("move: %v", err)
	}
	if lib.ArmState().Busy {
		t.Fatalf("expected arm to be idle again after Move returned")
	}
}

// TestArmPositionUpdatesOnMoveLoadUnload verifies the arm's reported
// position tracks each operation's real destination.
func TestArmPositionUpdatesOnMoveLoadUnload(t *testing.T) {
	lib := newTestLibrary(t)
	placeVolumeInFirstSlot(lib, "VOLA0001")
	fromAddr := lib.slots[0].Address
	toAddr := lib.slots[1].Address

	if err := lib.Move(ElementRef{Kind: KindSlot, Address: fromAddr}, ElementRef{Kind: KindSlot, Address: toAddr}, ""); err != nil {
		t.Fatalf("move: %v", err)
	}
	want := ArmPosition{Kind: "slot", Address: toAddr}
	if got := lib.ArmState().Position; got != want {
		t.Fatalf("after Move: expected position %+v, got %+v", want, got)
	}

	if err := lib.Load(ElementRef{Kind: KindSlot, Address: toAddr}, 0, ""); err != nil {
		t.Fatalf("load: %v", err)
	}
	want = ArmPosition{Kind: "drive", Address: 0}
	if got := lib.ArmState().Position; got != want {
		t.Fatalf("after Load: expected position %+v, got %+v", want, got)
	}

	if err := lib.Unload(0, ElementRef{Kind: KindSlot, Address: fromAddr}, ""); err != nil {
		t.Fatalf("unload: %v", err)
	}
	want = ArmPosition{Kind: "slot", Address: fromAddr}
	if got := lib.ArmState().Position; got != want {
		t.Fatalf("after Unload: expected position %+v, got %+v", want, got)
	}
}

// TestArmParkedWhileAnyDoorOpen proves "parked" is a derived, continuous
// state (see currentArmStateLocked), not a one-time transition on
// door-open: the arm must still report as parked even after an unrelated
// Move runs on a completely different magazine while the first
// magazine's door remains open, and must automatically revert to the
// real last position once every door is closed again.
func TestArmParkedWhileAnyDoorOpen(t *testing.T) {
	tmp := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = tmp
	cfg.Library.Magazines = []config.MagazineConfig{
		{ID: "Magazine1", Slots: 2, BaseAddress: 1},
		{ID: "Magazine2", Slots: 2, BaseAddress: 3},
	}
	cfg.Library.DriveDevices = []config.DriveDeviceConfig{{DevicePath: filepath.Join(tmp, "drives", "drive0")}}
	cfg.Library.DefaultCapacity = "1MiB"
	cfg.Library.TapeTypes = []config.TapeType{{Name: "TESTTYPE", Capacity: "1MiB", BarcodeFamily: "generic", VolSerLength: 8}}
	cfg.Library.TapeSets = []config.TapeSetConfig{{Name: testTapeSet, TapeType: "TESTTYPE", StorageFolder: filepath.Join(tmp, "ts1")}}
	lib, err := New(cfg, nil, nil, nil)
	if err != nil {
		t.Fatalf("new library: %v", err)
	}

	var mag1Slots, mag2Slots []*Slot
	for _, s := range lib.slots {
		switch s.MagazineID {
		case "Magazine1":
			mag1Slots = append(mag1Slots, s)
		case "Magazine2":
			mag2Slots = append(mag2Slots, s)
		}
	}
	if len(mag1Slots) != 2 || len(mag2Slots) != 2 {
		t.Fatalf("expected 2 slots each in Magazine1/Magazine2, got %d/%d", len(mag1Slots), len(mag2Slots))
	}
	mag2Slots[0].Volume = &Volume{Barcode: "VOLB0001", TapeSet: testTapeSet, Path: filepath.Join(tmp, "VOLB0001")}

	if err := lib.OpenStorageDoor("Magazine1", ""); err != nil {
		t.Fatalf("open Magazine1 door: %v", err)
	}
	if got := lib.ArmState().Position.Kind; got != ArmPositionParked {
		t.Fatalf("expected parked position while Magazine1's door is open, got %q", got)
	}

	if err := lib.Move(ElementRef{Kind: KindSlot, Address: mag2Slots[0].Address}, ElementRef{Kind: KindSlot, Address: mag2Slots[1].Address}, ""); err != nil {
		t.Fatalf("unrelated move on Magazine2: %v", err)
	}
	if got := lib.ArmState().Position.Kind; got != ArmPositionParked {
		t.Fatalf("expected position to still report parked while Magazine1's door remains open, even after an unrelated Move on Magazine2, got %q", got)
	}

	if err := lib.CloseStorageDoor("Magazine1", nil); err != nil {
		t.Fatalf("close Magazine1 door: %v", err)
	}
	// Not mag2Slots[1] (the earlier unrelated Move's destination):
	// closing Magazine1's door runs Magazine1's own post-close scan (see
	// TestArmPositionReflectsLastScannedSlotAfterDoorCloses), which
	// updates armLastPosition to Magazine1's own last slot as the arm
	// passes it - that scan happens chronologically after the Move, so
	// it correctly wins as the arm's most recent real position, exactly
	// like a second Move would.
	want := ArmPosition{Kind: "slot", Address: mag1Slots[len(mag1Slots)-1].Address}
	if got := lib.ArmState().Position; got != want {
		t.Fatalf("expected position to reflect Magazine1's own post-close scan once its door closes, got %+v want %+v", got, want)
	}
}

// TestArmStepsCapturedAndNeverAudited proves the live-only arm-narration
// channel is genuinely separate from the persisted/SNMP'd event log: the
// atomic step text must appear in ArmSteps() but never in Events().
func TestArmStepsCapturedAndNeverAudited(t *testing.T) {
	lib := newTestLibrary(t)
	placeVolumeInFirstSlot(lib, "VOLA0001")
	fromAddr := lib.slots[0].Address
	toAddr := lib.slots[1].Address

	if err := lib.Move(ElementRef{Kind: KindSlot, Address: fromAddr}, ElementRef{Kind: KindSlot, Address: toAddr}, ""); err != nil {
		t.Fatalf("move: %v", err)
	}

	steps := lib.ArmSteps()
	wantFragments := []string{"moving to", "grabbed tape", "placed tape"}
	for _, frag := range wantFragments {
		found := false
		for _, s := range steps {
			if strings.Contains(s.Message, frag) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected an arm step containing %q, got %+v", frag, steps)
		}
	}

	for _, e := range lib.Events() {
		for _, frag := range wantFragments {
			if strings.Contains(e.Message, frag) {
				t.Fatalf("expected granular arm narration to never appear in the persisted event log, found %q in event %+v", frag, e)
			}
		}
	}
}

// TestArmStepsRingBufferCap verifies the live-only step buffer is capped
// at maxArmSteps, not an unbounded slice.
func TestArmStepsRingBufferCap(t *testing.T) {
	lib := newTestLibrary(t)
	placeVolumeInFirstSlot(lib, "VOLA0001")
	addrA := lib.slots[0].Address
	addrB := lib.slots[1].Address

	// Each Move records 4 steps; enough round trips to comfortably exceed
	// maxArmSteps (50).
	for i := 0; i < 20; i++ {
		if err := lib.Move(ElementRef{Kind: KindSlot, Address: addrA}, ElementRef{Kind: KindSlot, Address: addrB}, ""); err != nil {
			t.Fatalf("move %d (A->B): %v", i, err)
		}
		if err := lib.Move(ElementRef{Kind: KindSlot, Address: addrB}, ElementRef{Kind: KindSlot, Address: addrA}, ""); err != nil {
			t.Fatalf("move %d (B->A): %v", i, err)
		}
	}
	if got := len(lib.ArmSteps()); got != maxArmSteps {
		t.Fatalf("expected ArmSteps() capped at exactly %d entries, got %d", maxArmSteps, got)
	}
}

// TestArmOpenDoorsReconciledAfterRestore verifies armOpenDoors (never
// itself persisted) is correctly rebuilt from the genuinely-persisted
// stDoorOpen/ioDoorOpen maps after a restore - a restart with a door
// already open must not report the arm as un-parked just because
// armOpenDoors defaults to its Go zero value.
func TestArmOpenDoorsReconciledAfterRestore(t *testing.T) {
	lib := newTestLibrary(t)
	if err := lib.OpenStorageDoor("Magazine1", ""); err != nil {
		t.Fatalf("open storage door: %v", err)
	}

	st := lib.State()
	restored, err := New(lib.cfg, &st, nil, nil)
	if err != nil {
		t.Fatalf("restore library: %v", err)
	}
	if got := restored.ArmState().Position.Kind; got != ArmPositionParked {
		t.Fatalf("expected restored library to immediately report the arm as parked (door was open at save time), got %q", got)
	}
}

// TestArmPositionReflectsLastScannedSlotAfterDoorCloses is the direct
// regression test for the reported bug: "after a scan ending at slot 30,
// the last position was shown as slot 1". armLastPosition must track
// where the magazine scan actually left the arm, not some stale value
// from before the magazine was even opened.
func TestArmPositionReflectsLastScannedSlotAfterDoorCloses(t *testing.T) {
	lib := newTestLibrary(t)
	// Plant a deliberately wrong prior position - e.g. drive 0 - so the
	// test would fail if the scan doesn't genuinely overwrite it.
	lib.setArmPosition(ArmPosition{Kind: "drive", Address: 0})

	if err := lib.OpenStorageDoor("Magazine1", ""); err != nil {
		t.Fatalf("open storage door: %v", err)
	}
	if err := lib.CloseStorageDoor("Magazine1", nil); err != nil {
		t.Fatalf("close storage door: %v", err)
	}

	lastSlot := lib.slots[len(lib.slots)-1] // newTestLibrary has only Magazine1, so this is its last slot
	want := ArmPosition{Kind: "slot", Address: lastSlot.Address}
	if got := lib.ArmState().Position; got != want {
		t.Fatalf("expected position to reflect the last-scanned slot %+v after the magazine door closes, got %+v", want, got)
	}
}

// TestArmStepsAnnounceParkedTransitionOnceAcrossMultipleDoors verifies
// the live activity panel explicitly narrates the transition to parked
// (previously only the silent state change was pushed, with no visible
// step), and that it's edge-triggered on the aggregate open-door count -
// opening a second door while a first is already open must not
// re-announce "moving to parked position". A closing-side "resuming from
// parked..." narration was deliberately tried and then removed as not
// useful (see setArmDoorsOpenDelta's doc comment) - this test also
// confirms it never appears, on either a partial or full door close.
func TestArmStepsAnnounceParkedTransitionOnceAcrossMultipleDoors(t *testing.T) {
	tmp := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = tmp
	cfg.Library.Magazines = []config.MagazineConfig{
		{ID: "Magazine1", Slots: 2, BaseAddress: 1},
		{ID: "Magazine2", Slots: 2, BaseAddress: 3},
	}
	cfg.Library.DriveDevices = []config.DriveDeviceConfig{{DevicePath: filepath.Join(tmp, "drives", "drive0")}}
	cfg.Library.DefaultCapacity = "1MiB"
	cfg.Library.TapeTypes = []config.TapeType{{Name: "TESTTYPE", Capacity: "1MiB", BarcodeFamily: "generic", VolSerLength: 8}}
	cfg.Library.TapeSets = []config.TapeSetConfig{{Name: testTapeSet, TapeType: "TESTTYPE", StorageFolder: filepath.Join(tmp, "ts1")}}
	lib, err := New(cfg, nil, nil, nil)
	if err != nil {
		t.Fatalf("new library: %v", err)
	}

	if err := lib.OpenStorageDoor("Magazine1", ""); err != nil {
		t.Fatalf("open Magazine1: %v", err)
	}
	countParkedSteps := func() int {
		n := 0
		for _, s := range lib.ArmSteps() {
			if s.Message == "moving to parked position" {
				n++
			}
		}
		return n
	}
	if n := countParkedSteps(); n != 1 {
		t.Fatalf("expected exactly 1 'moving to parked position' step after opening Magazine1's door, got %d: %+v", n, lib.ArmSteps())
	}

	if err := lib.OpenStorageDoor("Magazine2", ""); err != nil {
		t.Fatalf("open Magazine2: %v", err)
	}
	if n := countParkedSteps(); n != 1 {
		t.Fatalf("expected still exactly 1 'moving to parked position' step after opening a second door, got %d: %+v", n, lib.ArmSteps())
	}

	hasResumingStep := func() bool {
		for _, s := range lib.ArmSteps() {
			if strings.HasPrefix(s.Message, "resuming from parked") {
				return true
			}
		}
		return false
	}

	if err := lib.CloseStorageDoor("Magazine1", nil); err != nil {
		t.Fatalf("close Magazine1: %v", err)
	}
	if hasResumingStep() {
		t.Fatalf("expected no 'resuming from parked' step (removed as not useful) after closing one of two open doors, got %+v", lib.ArmSteps())
	}

	if err := lib.CloseStorageDoor("Magazine2", nil); err != nil {
		t.Fatalf("close Magazine2: %v", err)
	}
	if hasResumingStep() {
		t.Fatalf("expected no 'resuming from parked' step (removed as not useful) even after the last open door closes, got %+v", lib.ArmSteps())
	}
}

// TestArmStateUnknownPositionReportsAsParked verifies a fresh Library
// (the arm has never moved since the daemon started, so armLastPosition
// is still its Go zero value, Kind "") reports as parked rather than an
// "unknown" position - a real arm starts docked at a home/parked
// position, not floating at some arbitrary unaddressable "nowhere".
func TestArmStateUnknownPositionReportsAsParked(t *testing.T) {
	lib := newTestLibrary(t)
	if got := lib.ArmState().Position.Kind; got != ArmPositionParked {
		t.Fatalf("expected an unknown/never-moved position to be reported as parked, got %q", got)
	}
}
