package library

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/swenske/gotochanger/internal/config"
)

// newTestLibraryWithCleaning mirrors newTestLibraryWithLatency, but for
// CleaningSettings.
func newTestLibraryWithCleaning(t *testing.T, cs config.CleaningSettings) *Library {
	t.Helper()
	tmp := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = tmp
	cfg.Library.Magazines = []config.MagazineConfig{{ID: "Magazine1", Slots: 2, BaseAddress: 1}}
	cfg.Library.Mailboxes = []config.MailboxConfig{{ID: "Mailbox1", Slots: 1, BaseAddress: 21}}
	cfg.Library.DriveDevices = []config.DriveDeviceConfig{{DevicePath: filepath.Join(tmp, "drives", "drive0")}}
	cfg.Library.DefaultCapacity = "1MiB"
	cfg.Library.Cleaning = cs
	lib, err := New(cfg, nil, nil, nil)
	if err != nil {
		t.Fatalf("new library: %v", err)
	}
	return lib
}

// placeCleaningTapeInFirstSlot puts a cleaning-marked volume directly into
// the first storage slot, mirroring placeVolumeInFirstSlot's "bypass the
// door-open/load-action ceremony" convention.
func placeCleaningTapeInFirstSlot(lib *Library, barcode string) {
	lib.slots[0].Volume = &Volume{Barcode: barcode, Path: filepath.Join(lib.cfg.DataDir, barcode), Cleaning: true, CleaningState: CleaningTapeAvailable}
}

func TestCreateCleaningTapeGeneratesBarcodeAndEnforcesPoolLimit(t *testing.T) {
	lib := newTestLibrary(t)
	vol, err := lib.CreateCleaningTape()
	if err != nil {
		t.Fatalf("create cleaning tape: %v", err)
	}
	if vol.Barcode != "00001CLN" {
		t.Fatalf("expected barcode 00001CLN, got %q", vol.Barcode)
	}
	if !vol.Cleaning || vol.CleaningState != CleaningTapeAvailable {
		t.Fatalf("expected a fresh available cleaning tape, got %+v", vol)
	}
	if !containsBarcode(lib.OutsideVolumes(), "00001CLN") {
		t.Fatalf("expected new cleaning tape to be created outside the library")
	}

	for i := 2; i <= maxCleaningTapes; i++ {
		if _, err := lib.CreateCleaningTape(); err != nil {
			t.Fatalf("create cleaning tape %d: %v", i, err)
		}
	}
	if _, err := lib.CreateCleaningTape(); !errors.Is(err, ErrCleaningPoolFull) {
		t.Fatalf("expected ErrCleaningPoolFull for the 6th cleaning tape, got %v", err)
	}
	if len(lib.CleaningTapes()) != maxCleaningTapes {
		t.Fatalf("expected exactly %d cleaning tapes, got %d", maxCleaningTapes, len(lib.CleaningTapes()))
	}
}

// TestLoadOfCleaningTapeRunsCycleAndAutoEjects covers the core new
// behavior: in CleaningModeRobot, manual cleaning is just an ordinary
// Load of a cleaning cartridge (no separate trigger), and Load itself
// automatically ejects the cartridge back to its origin slot once the
// cycle completes - the drive is idle again and the tape is back in its
// slot by the time Load returns, with no separate Unload call needed.
// This auto-eject is robot-mode-specific - see
// TestLoadOfCleaningTapeInSoftwareModeStaysMountedUntilUnload for the
// backup_software-mode counterpart, where the backup software is
// expected to issue its own Unload instead.
func TestLoadOfCleaningTapeRunsCycleAndAutoEjects(t *testing.T) {
	lib := newTestLibraryWithCleaning(t, config.CleaningSettings{Enabled: true, Mode: config.CleaningModeRobot, MaxUses: 2, MountThreshold: 50, Duration: "0s"})
	placeCleaningTapeInFirstSlot(lib, "00001CLN")
	fromAddr := lib.slots[0].Address

	// First use: not yet expired, auto-ejected back to its slot.
	if err := lib.Load(ElementRef{Kind: KindSlot, Address: fromAddr}, 0, ""); err != nil {
		t.Fatalf("load cleaning tape: %v", err)
	}
	if lib.drives[0].Volume != nil {
		t.Fatalf("expected drive to be idle again after the cleaning cycle auto-ejects, got %+v", lib.drives[0].Volume)
	}
	if lib.slots[0].Volume == nil || lib.slots[0].Volume.CleaningUsageCount != 1 {
		t.Fatalf("expected the cleaning tape back in its slot with usage count 1, got %+v", lib.slots[0].Volume)
	}
	if lib.slots[0].Volume.CleaningState != CleaningTapeAvailable {
		t.Fatalf("expected cleaning tape back to available after first use, got %q", lib.slots[0].Volume.CleaningState)
	}
	if lib.drives[0].MountsSinceCleaning != 0 {
		t.Fatalf("expected MountsSinceCleaning reset to 0 after a cleaning cycle, got %d", lib.drives[0].MountsSinceCleaning)
	}

	// Second use hits MaxUses=2: must expire, but still auto-ejected.
	if err := lib.Load(ElementRef{Kind: KindSlot, Address: fromAddr}, 0, ""); err != nil {
		t.Fatalf("load cleaning tape (2nd use): %v", err)
	}
	if lib.drives[0].Volume != nil {
		t.Fatalf("expected drive to be idle again after the 2nd cleaning cycle, got %+v", lib.drives[0].Volume)
	}
	if lib.slots[0].Volume == nil || lib.slots[0].Volume.CleaningUsageCount != 2 {
		t.Fatalf("expected usage count 2, got %+v", lib.slots[0].Volume)
	}
	if lib.slots[0].Volume.CleaningState != CleaningTapeExpired {
		t.Fatalf("expected cleaning tape expired at max uses, got %q", lib.slots[0].Volume.CleaningState)
	}
}

// TestCleaningCycleEventSequence verifies the real-time, phase-by-phase
// event ordering in CleaningModeRobot: loading -> loaded -> cleaning-start
// -> cleaning done -> unloading -> unloaded, each emitted as that phase
// begins/completes rather than only in a summary at the very end. The
// trailing unloading/unload pair is the auto-eject, robot-mode-only - see
// TestLoadOfCleaningTapeInSoftwareModeStaysMountedUntilUnload for the
// shorter sequence backup_software mode produces.
func TestCleaningCycleEventSequence(t *testing.T) {
	lib := newTestLibraryWithCleaning(t, config.CleaningSettings{Enabled: true, Mode: config.CleaningModeRobot, MaxUses: 20, MountThreshold: 50, Duration: "0s"})
	placeCleaningTapeInFirstSlot(lib, "00001CLN")
	fromAddr := lib.slots[0].Address

	if err := lib.Load(ElementRef{Kind: KindSlot, Address: fromAddr}, 0, ""); err != nil {
		t.Fatalf("load: %v", err)
	}

	evs := lib.Events() // newest first
	var codes []string
	for i := len(evs) - 1; i >= 0; i-- {
		codes = append(codes, evs[i].Code)
	}
	want := []string{
		EventCodeRoboticsLoadStarted,
		EventCodeRoboticsLoadSuccess,
		EventCodeCleaningCycleStart,
		EventCodeCleaningCycleSuccess,
		EventCodeRoboticsUnloadStarted,
		EventCodeRoboticsUnloadSuccess,
	}
	if len(codes) < len(want) {
		t.Fatalf("expected at least %d events, got %d: %v", len(want), len(codes), codes)
	}
	got := codes[len(codes)-len(want):]
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("event %d: expected %s, got %s (full sequence: %v)", i, w, got[i], got)
		}
	}
}

// TestLoadOfCleaningTapeInSoftwareModeStaysMountedUntilUnload covers the
// CleaningModeSoftware counterpart to TestLoadOfCleaningTapeRunsCycleAndAutoEjects:
// per config.CleaningModeSoftware's doc comment ("the backup software
// itself decides when to mount/unmount"), the cycle still runs (usage
// count incremented, MountsSinceCleaning reset), but Load must not
// auto-eject the cartridge - it stays loaded, still CleaningTapeInUse,
// until an explicit Unload (the same one the backup software would issue
// for any ordinary volume) returns it to its slot and flips it back to
// available.
func TestLoadOfCleaningTapeInSoftwareModeStaysMountedUntilUnload(t *testing.T) {
	lib := newTestLibraryWithCleaning(t, config.CleaningSettings{Enabled: true, Mode: config.CleaningModeSoftware, MaxUses: 20, MountThreshold: 50, Duration: "0s"})
	placeCleaningTapeInFirstSlot(lib, "00001CLN")
	fromAddr := lib.slots[0].Address

	if err := lib.Load(ElementRef{Kind: KindSlot, Address: fromAddr}, 0, ""); err != nil {
		t.Fatalf("load cleaning tape: %v", err)
	}
	if lib.drives[0].Volume == nil || lib.drives[0].Volume.Barcode != "00001CLN" {
		t.Fatalf("expected the cleaning tape to stay mounted after its cycle in backup_software mode, got %+v", lib.drives[0].Volume)
	}
	if lib.drives[0].Volume.CleaningUsageCount != 1 {
		t.Fatalf("expected usage count 1 after the cycle, got %d", lib.drives[0].Volume.CleaningUsageCount)
	}
	if lib.drives[0].Volume.CleaningState != CleaningTapeInUse {
		t.Fatalf("expected the still-mounted tape to remain in_use, got %q", lib.drives[0].Volume.CleaningState)
	}
	if lib.drives[0].MountsSinceCleaning != 0 {
		t.Fatalf("expected MountsSinceCleaning reset to 0 even though the tape wasn't auto-ejected, got %d", lib.drives[0].MountsSinceCleaning)
	}
	if lib.slots[0].Volume != nil {
		t.Fatalf("expected the origin slot to remain empty - nothing auto-ejected back to it")
	}

	// No unloading/unload events yet - the sequence stops at the cleaning
	// cycle completing, unlike CleaningModeRobot's auto-eject sequence
	// (see TestCleaningCycleEventSequence).
	for _, e := range lib.Events() {
		if e.Code == EventCodeRoboticsUnloadStarted || e.Code == EventCodeRoboticsUnloadSuccess {
			t.Fatalf("expected no unload events before an explicit Unload in backup_software mode, found %s", e.Code)
		}
	}

	// The backup software now issues its own Unload, exactly like any
	// ordinary volume - this already-existing cleaning-aware branch in
	// Unload must correctly restore availability.
	toAddr := lib.slots[0].Address
	if err := lib.Unload(0, ElementRef{Kind: KindSlot, Address: toAddr}, ""); err != nil {
		t.Fatalf("unload cleaning tape: %v", err)
	}
	if lib.drives[0].Volume != nil {
		t.Fatalf("expected drive to be empty after the explicit unload")
	}
	if lib.slots[0].Volume == nil || lib.slots[0].Volume.CleaningState != CleaningTapeAvailable {
		t.Fatalf("expected the cleaning tape back in its slot and available, got %+v", lib.slots[0].Volume)
	}
}

func TestLoadRejectsExpiredCleaningTape(t *testing.T) {
	lib := newTestLibraryWithCleaning(t, config.CleaningSettings{Enabled: true, Mode: config.CleaningModeSoftware, MaxUses: 20, MountThreshold: 50, Duration: "0s"})
	lib.slots[0].Volume = &Volume{Barcode: "00001CLN", Path: filepath.Join(lib.cfg.DataDir, "00001CLN"), Cleaning: true, CleaningState: CleaningTapeExpired}
	fromAddr := lib.slots[0].Address

	if err := lib.Load(ElementRef{Kind: KindSlot, Address: fromAddr}, 0, ""); !errors.Is(err, ErrCleaningTapeExpired) {
		t.Fatalf("expected ErrCleaningTapeExpired, got %v", err)
	}
	if lib.drives[0].Volume != nil {
		t.Fatalf("expected drive to remain empty after a rejected expired-tape load")
	}
}

// TestCleaningDisabledActsAsOrdinaryVolume verifies that when the
// cleaning-tape management feature is globally disabled, a cleaning-
// marked volume behaves like any ordinary volume: no duration sleep, no
// usage tracking, no auto-eject, and even an "expired" one loads
// normally (the whole feature is inert, mirroring how Latency.Enabled
// zeroes every simulated delay when off).
func TestCleaningDisabledActsAsOrdinaryVolume(t *testing.T) {
	lib := newTestLibraryWithCleaning(t, config.CleaningSettings{Enabled: false, Mode: config.CleaningModeSoftware, MaxUses: 1, MountThreshold: 50, Duration: "0s"})
	lib.slots[0].Volume = &Volume{Barcode: "00001CLN", Path: filepath.Join(lib.cfg.DataDir, "00001CLN"), Cleaning: true, CleaningState: CleaningTapeExpired}
	fromAddr := lib.slots[0].Address

	if err := lib.Load(ElementRef{Kind: KindSlot, Address: fromAddr}, 0, ""); err != nil {
		t.Fatalf("expected an expired cleaning tape to load normally while cleaning is disabled, got %v", err)
	}
	if lib.drives[0].Volume == nil || lib.drives[0].Volume.Barcode != "00001CLN" {
		t.Fatalf("expected the tape to simply stay loaded like any ordinary volume, got %+v", lib.drives[0].Volume)
	}
	if lib.drives[0].MountsSinceCleaning != 1 {
		t.Fatalf("expected MountsSinceCleaning to increment like an ordinary mount, got %d", lib.drives[0].MountsSinceCleaning)
	}
}

// TestCleaningCommitsBeforeDurationSleep verifies the key reordering this
// feature depends on: Load must mark the drive occupied *before* the
// cleaning-duration sleep, not after, so Status() reports the drive busy
// for the whole cleaning cycle - not just once it completes.
func TestCleaningCommitsBeforeDurationSleep(t *testing.T) {
	const delay = 40 * time.Millisecond
	lib := newTestLibraryWithCleaning(t, config.CleaningSettings{Enabled: true, Mode: config.CleaningModeSoftware, MaxUses: 20, MountThreshold: 50, Duration: delay.String()})
	placeCleaningTapeInFirstSlot(lib, "00001CLN")
	fromAddr := lib.slots[0].Address

	done := make(chan error, 1)
	go func() {
		done <- lib.Load(ElementRef{Kind: KindSlot, Address: fromAddr}, 0, "")
	}()

	time.Sleep(delay / 2)
	// Read through l.mu directly rather than holding onto a Status()-
	// returned pointer (see mountsSinceCleaningLocked's doc comment) -
	// Status only holds its RLock while building the returned struct,
	// not while the caller inspects it afterward, so a bare
	// Status().Drives[0].Volume access here would race against Load's
	// own re-acquired Lock() once the cleaning-duration sleep ends.
	lib.mu.RLock()
	vol := lib.drives[0].Volume
	lib.mu.RUnlock()
	if vol == nil || !vol.Cleaning {
		t.Fatalf("expected drive to already report the cleaning volume mid-cycle, got %+v", vol)
	}

	if err := <-done; err != nil {
		t.Fatalf("load: %v", err)
	}
}

// TestEjectCleaningTapeFallbackWhenOriginOccupied covers the defensive
// fallback path: if a cleaning tape's origin slot is no longer free by
// the time its cycle completes - unreachable in real operation under
// this library's single-exclusive-lock model, but exercised directly
// here - the tape is moved outside instead of being dropped, and a
// warning event is logged.
func TestEjectCleaningTapeFallbackWhenOriginOccupied(t *testing.T) {
	lib := newTestLibraryWithCleaning(t, config.CleaningSettings{Enabled: true, Mode: config.CleaningModeSoftware, MaxUses: 20, MountThreshold: 50, Duration: "0s"})
	origin := ElementRef{Kind: KindSlot, Address: lib.slots[0].Address}
	vol := &Volume{Barcode: "00001CLN", Path: filepath.Join(lib.cfg.DataDir, "00001CLN"), Cleaning: true, CleaningState: CleaningTapeInUse}
	lib.drives[0].Volume = vol
	lib.slots[0].Volume = &Volume{Barcode: "OTHERVOL"} // origin slot now occupied by something else

	lib.mu.Lock()
	lib.ejectCleaningTapeAfterCycleLocked(lib.drives[0], 0, origin)
	lib.mu.Unlock()

	if lib.drives[0].Volume != nil {
		t.Fatalf("expected the drive to be cleared regardless of the fallback")
	}
	if !containsBarcode(lib.OutsideVolumes(), "00001CLN") {
		t.Fatalf("expected the cleaning tape to fall back to outside when its origin slot is occupied")
	}
	found := false
	for _, e := range lib.Events() {
		if e.Code == EventCodeCleaningTapeEjectFallback {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a CLEANING.TAPE.EJECT_FALLBACK.WARNING event to be logged")
	}
}

// TestCleaningSleepDoesNotBlockOtherDrives confirms the lock-release
// fix: a cleaning cycle on one drive must not stall Status() or an
// unrelated Move/Load on another drive - unlike every other simulated
// delay in this codebase, which does hold l.mu (and so does block
// concurrent callers) for its whole duration by design.
func TestCleaningSleepDoesNotBlockOtherDrives(t *testing.T) {
	const delay = 300 * time.Millisecond
	tmp := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = tmp
	cfg.Library.Magazines = []config.MagazineConfig{{ID: "Magazine1", Slots: 2, BaseAddress: 1}}
	cfg.Library.Mailboxes = []config.MailboxConfig{{ID: "Mailbox1", Slots: 1, BaseAddress: 21}}
	cfg.Library.DriveDevices = []config.DriveDeviceConfig{{DevicePath: filepath.Join(tmp, "drives", "drive0")}, {DevicePath: filepath.Join(tmp, "drives", "drive1")}}
	cfg.Library.DefaultCapacity = "1MiB"
	cfg.Library.Cleaning = config.CleaningSettings{Enabled: true, Mode: config.CleaningModeSoftware, MaxUses: 20, MountThreshold: 50, Duration: delay.String()}
	lib, err := New(cfg, nil, nil, nil)
	if err != nil {
		t.Fatalf("new library: %v", err)
	}
	placeCleaningTapeInFirstSlot(lib, "00001CLN")
	lib.slots[1].Volume = &Volume{Barcode: "DATA0001", Path: filepath.Join(tmp, "DATA0001")}

	done := make(chan error, 1)
	go func() {
		done <- lib.Load(ElementRef{Kind: KindSlot, Address: lib.slots[0].Address}, 0, "")
	}()
	time.Sleep(50 * time.Millisecond) // let the cleaning cycle actually start

	start := time.Now()
	if err := lib.Load(ElementRef{Kind: KindSlot, Address: lib.slots[1].Address}, 1, ""); err != nil {
		t.Fatalf("load on the other drive: %v", err)
	}
	if elapsed := time.Since(start); elapsed > delay/2 {
		t.Fatalf("expected loading drive 1 to complete quickly while drive 0 is mid cleaning-cycle, took %v", elapsed)
	}

	if err := <-done; err != nil {
		t.Fatalf("cleaning load: %v", err)
	}
}

// mountsSinceCleaningLocked reads Drive.MountsSinceCleaning through l.mu,
// unlike a bare Status()-returned pointer field access - Status shares
// live Drive pointers with the running Library (deliberately, so a
// LogicalLibrary's own Drives slice stays in sync - see
// resolveLogicalLibraryLocked), so reading a field off that pointer
// after Status() has already released its RLock races against the
// Library's own Lock()-protected writes once Load releases l.mu for the
// cleaning-duration sleep and later re-acquires it (see Load's doc
// comment) - exactly the kind of concurrent access these tests
// deliberately exercise.
func mountsSinceCleaningLocked(lib *Library, driveIndex int) int {
	lib.mu.RLock()
	defer lib.mu.RUnlock()
	return lib.drives[driveIndex].MountsSinceCleaning
}

func TestAutoCleanSweepOnlyActsInRobotMode(t *testing.T) {
	lib := newTestLibraryWithCleaning(t, config.CleaningSettings{Enabled: true, Mode: config.CleaningModeSoftware, MaxUses: 20, MountThreshold: 1, Duration: "0s"})
	placeCleaningTapeInFirstSlot(lib, "00001CLN")
	lib.mu.Lock()
	lib.drives[0].MountsSinceCleaning = 5 // well over the threshold
	lib.mu.Unlock()

	lib.AutoCleanSweep()
	time.Sleep(50 * time.Millisecond) // let any (unwanted) goroutine finish
	if got := mountsSinceCleaningLocked(lib, 0); got != 5 {
		t.Fatalf("expected AutoCleanSweep to be a no-op in backup_software mode, MountsSinceCleaning changed to %d", got)
	}

	lib.mu.Lock()
	lib.cleaningMode = config.CleaningModeRobot
	lib.mu.Unlock()
	lib.AutoCleanSweep()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if mountsSinceCleaningLocked(lib, 0) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected AutoCleanSweep to trigger a cleaning cycle in backup_robot mode and reset MountsSinceCleaning")
}

func TestAutoCleanSweepEmitsUnavailableEventWhenNoTapeExists(t *testing.T) {
	lib := newTestLibraryWithCleaning(t, config.CleaningSettings{Enabled: true, Mode: config.CleaningModeRobot, MaxUses: 20, MountThreshold: 1, Duration: "0s"})
	lib.drives[0].MountsSinceCleaning = 5

	lib.AutoCleanSweep()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range lib.Events() {
			if e.Code == EventCodeCleaningTapeUnavailable {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected a CLEANING.TAPE.UNAVAILABLE.WARNING event when no cleaning tape exists")
}

// TestEjectCleaningTapeWaitsForActivitySettleDelay is
// ejectCleaningTapeAfterCycleLocked's counterpart to
// TestUnloadWaitsForActivitySettleDelayBeforeStoppingWatcher (in
// library_test.go) - it also stops a drive's activity watcher and must
// give it the same settle margin before doing so, since a robot-mode
// auto-eject tears down drive state exactly the same way a manual
// Unload does.
func TestEjectCleaningTapeWaitsForActivitySettleDelay(t *testing.T) {
	orig := driveActivityUnloadSettleDelay
	driveActivityUnloadSettleDelay = 50 * time.Millisecond
	t.Cleanup(func() { driveActivityUnloadSettleDelay = orig })

	lib := newTestLibraryWithCleaning(t, config.CleaningSettings{Enabled: true, Mode: config.CleaningModeRobot, MaxUses: 20, MountThreshold: 50, Duration: "0s"})
	placeCleaningTapeInFirstSlot(lib, "00001CLN")
	fromAddr := lib.slots[0].Address

	start := time.Now()
	if err := lib.Load(ElementRef{Kind: KindSlot, Address: fromAddr}, 0, ""); err != nil {
		t.Fatalf("load cleaning tape: %v", err)
	}
	if elapsed := time.Since(start); elapsed < driveActivityUnloadSettleDelay {
		t.Fatalf("expected the robot-mode auto-eject to wait at least %s for the activity watcher to settle before tearing down, took %s", driveActivityUnloadSettleDelay, elapsed)
	}
	if lib.drives[0].Volume != nil {
		t.Fatalf("expected the drive to be idle again after the cleaning cycle auto-ejects, got %+v", lib.drives[0].Volume)
	}
}
