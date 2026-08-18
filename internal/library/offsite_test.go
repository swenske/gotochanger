package library

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/swenske/gotochanger/internal/config"
)

func TestRequestOffsitePickupKeepsVolumeUsable(t *testing.T) {
	lib := newTestLibrary(t)
	lib.cfg.Library.OffsiteLocation = true
	placeVolumeInFirstSlot(lib, "VOLA0001")
	slotAddr := lib.slots[0].Address

	due := time.Now().Add(time.Hour)
	vol, err := lib.RequestOffsitePickup("VOLA0001", "scheduled_pickup", due, "")
	if err != nil {
		t.Fatalf("request offsite pickup: %v", err)
	}
	if vol.Offsite == nil || vol.Offsite.Status != OffsiteStatusPendingPickup {
		t.Fatalf("expected PendingPickup status, got %+v", vol.Offsite)
	}

	// PendingPickup must not block ordinary operations: the tape hasn't
	// actually left yet, and stays fully usable until CompleteOffsitePickup
	// runs (see RequestOffsitePickup's doc comment).
	if err := lib.Load(ElementRef{Kind: KindSlot, Address: slotAddr}, 0, ""); err != nil {
		t.Fatalf("expected Load to still succeed on a PendingPickup volume, got %v", err)
	}
	if err := lib.Unload(0, ElementRef{Kind: KindSlot, Address: slotAddr}, ""); err != nil {
		t.Fatalf("expected Unload to still succeed, got %v", err)
	}
}

func TestRequestOffsitePickupRejectsUnknownOrAlreadyTracked(t *testing.T) {
	lib := newTestLibrary(t)
	lib.cfg.Library.OffsiteLocation = true
	placeVolumeInFirstSlot(lib, "VOLA0001")

	if _, err := lib.RequestOffsitePickup("NOSUCHVOL", "scheduled_pickup", time.Now(), ""); !errors.Is(err, ErrOffsiteNotOnsite) {
		t.Fatalf("expected ErrOffsiteNotOnsite for an unknown barcode, got %v", err)
	}
	if _, err := lib.RequestOffsitePickup("VOLA0001", "scheduled_pickup", time.Now(), ""); err != nil {
		t.Fatalf("first request: %v", err)
	}
	if _, err := lib.RequestOffsitePickup("VOLA0001", "scheduled_pickup", time.Now(), ""); !errors.Is(err, ErrOffsiteAlreadyTracked) {
		t.Fatalf("expected ErrOffsiteAlreadyTracked for a duplicate request, got %v", err)
	}
}

// TestCompleteOffsitePickupVaultsAndBlocksVolume is the direct regression
// test for the findAccessibleVolumeForWriteProtectLocked gap this feature
// fixed: before the fix, a vaulted offsite volume was treated as
// "accessible" and its write-protect tab could be toggled from a vendor's
// vault.
func TestCompleteOffsitePickupVaultsAndBlocksVolume(t *testing.T) {
	lib := newTestLibrary(t)
	lib.cfg.Library.OffsiteLocation = true
	placeVolumeInFirstSlot(lib, "VOLA0001")
	fromAddr := lib.slots[0].Address
	toAddr := lib.slots[1].Address

	if _, err := lib.RequestOffsitePickup("VOLA0001", "scheduled_pickup", time.Now(), ""); err != nil {
		t.Fatalf("request offsite pickup: %v", err)
	}
	vol, err := lib.CompleteOffsitePickup("VOLA0001")
	if err != nil {
		t.Fatalf("complete offsite pickup: %v", err)
	}
	if vol.Offsite == nil || vol.Offsite.Status != OffsiteStatusVaulted {
		t.Fatalf("expected Vaulted status after completion, got %+v", vol.Offsite)
	}
	if !vol.Offsite.DueAt.IsZero() {
		t.Fatalf("expected DueAt to be cleared once vaulted, got %v", vol.Offsite.DueAt)
	}
	if !containsBarcode(lib.OffsiteVolumes(), "VOLA0001") {
		t.Fatalf("expected volume in the offsite collection after completion")
	}
	if lib.slots[0].Volume != nil {
		t.Fatalf("expected the source slot to be empty after completion")
	}

	// Strict blocking: a vaulted volume is structurally unreachable via any
	// ElementRef (its old slot is simply empty now) and explicitly
	// rejected by the barcode-based write-protect lookup.
	if err := lib.Move(ElementRef{Kind: KindSlot, Address: fromAddr}, ElementRef{Kind: KindSlot, Address: toAddr}, ""); !errors.Is(err, ErrEmpty) {
		t.Fatalf("expected Move from the now-empty source slot to fail with ErrEmpty, got %v", err)
	}
	if err := lib.SetVolumeWriteProtect("VOLA0001", true); !errors.Is(err, ErrVolumeOffsite) {
		t.Fatalf("expected ErrVolumeOffsite from SetVolumeWriteProtect on an offsite volume, got %v", err)
	}
}

func TestRequestOffsiteReturnRequiresVaultedStatus(t *testing.T) {
	lib := newTestLibrary(t)
	lib.cfg.Library.OffsiteLocation = true
	placeVolumeInFirstSlot(lib, "VOLA0001")

	// Not vaulted at all yet (still onsite): rejected.
	if _, err := lib.RequestOffsiteReturn("VOLA0001", "scheduled_pickup", time.Now(), false, ""); !errors.Is(err, ErrOffsiteNotVaulted) {
		t.Fatalf("expected ErrOffsiteNotVaulted for an onsite volume, got %v", err)
	}

	if _, err := lib.RequestOffsitePickup("VOLA0001", "scheduled_pickup", time.Now(), ""); err != nil {
		t.Fatalf("request pickup: %v", err)
	}
	if _, err := lib.CompleteOffsitePickup("VOLA0001"); err != nil {
		t.Fatalf("complete pickup: %v", err)
	}

	due := time.Now().Add(time.Hour)
	vol, err := lib.RequestOffsiteReturn("VOLA0001", "scheduled_pickup", due, false, "")
	if err != nil {
		t.Fatalf("request return: %v", err)
	}
	if vol.Offsite.Status != OffsiteStatusPendingReturn {
		t.Fatalf("expected PendingReturn status, got %+v", vol.Offsite)
	}

	// Already PendingReturn: a second request is rejected outright, not
	// silently overwritten (e.g. quietly upgraded to urgent).
	if _, err := lib.RequestOffsiteReturn("VOLA0001", "scheduled_pickup", due, true, ""); !errors.Is(err, ErrOffsiteAlreadyTracked) {
		t.Fatalf("expected ErrOffsiteAlreadyTracked for a duplicate return request, got %v", err)
	}
}

func TestCompleteOffsiteReturnPlacesVolumeOutside(t *testing.T) {
	lib := newTestLibrary(t)
	lib.cfg.Library.OffsiteLocation = true
	placeVolumeInFirstSlot(lib, "VOLA0001")

	if _, err := lib.RequestOffsitePickup("VOLA0001", "scheduled_pickup", time.Now(), ""); err != nil {
		t.Fatalf("request pickup: %v", err)
	}
	if _, err := lib.CompleteOffsitePickup("VOLA0001"); err != nil {
		t.Fatalf("complete pickup: %v", err)
	}
	if _, err := lib.RequestOffsiteReturn("VOLA0001", "scheduled_pickup", time.Now(), false, ""); err != nil {
		t.Fatalf("request return: %v", err)
	}

	vol, err := lib.CompleteOffsiteReturn("VOLA0001")
	if err != nil {
		t.Fatalf("complete return: %v", err)
	}
	if vol.Offsite != nil {
		t.Fatalf("expected Offsite to be cleared after a completed return, got %+v", vol.Offsite)
	}
	if containsBarcode(lib.OffsiteVolumes(), "VOLA0001") {
		t.Fatalf("volume should no longer be in the offsite collection")
	}
	// CompleteOffsiteReturn deliberately doesn't guess/reserve a
	// destination slot (an automatic, unattended completion must never
	// fail with ErrFull) - it lands in outside-library inventory, exactly
	// like a fresh cartridge or a mailbox import, needing a manual
	// door-load to get racked.
	if !containsBarcode(lib.OutsideVolumes(), "VOLA0001") {
		t.Fatalf("expected a completed return to land in the outside-library inventory")
	}
}

func TestOffsiteSendModeAndManualBothSetOffsiteInfo(t *testing.T) {
	lib := newTestLibrary(t)
	lib.cfg.Library.OffsiteLocation = true
	placeVolumeInFirstSlot(lib, "VOLA0001")
	lib.slots[1].Volume = &Volume{Barcode: "VOLB0001", TapeSet: testTapeSet, Path: filepath.Join(lib.cfg.DataDir, "VOLB0001")}

	volManual, err := lib.OffsiteSend(ElementRef{Kind: KindSlot, Address: lib.slots[0].Address})
	if err != nil {
		t.Fatalf("manual offsite send: %v", err)
	}
	if volManual.Offsite == nil || volManual.Offsite.Mode != "manual" || volManual.Offsite.Status != OffsiteStatusVaulted {
		t.Fatalf("expected manual send to set Offsite{Mode: manual, Status: Vaulted}, got %+v", volManual.Offsite)
	}

	volMode, err := lib.OffsiteSendMode(ElementRef{Kind: KindSlot, Address: lib.slots[1].Address}, OffsiteMeta{Mode: "continuous_rotation"})
	if err != nil {
		t.Fatalf("mode-attributed offsite send: %v", err)
	}
	if volMode.Offsite == nil || volMode.Offsite.Mode != "continuous_rotation" || volMode.Offsite.Status != OffsiteStatusVaulted {
		t.Fatalf("expected mode-attributed send to set Offsite{Mode: continuous_rotation, Status: Vaulted}, got %+v", volMode.Offsite)
	}
}

// TestOffsiteInfoOriginShiftsAcrossRebuild mirrors
// TestRestoreMatchesElementsByAddressNotPosition, but for a vaulted
// volume's Offsite.Origin: it must be re-projected by (magazine, offset)
// identity across a topology rebuild, exactly like Drive.Origin, since raw
// Address values are recomputed (and can shift) on every rebuild - see
// shiftVolumeOffsiteOrigins.
func TestOffsiteInfoOriginShiftsAcrossRebuild(t *testing.T) {
	lib := newTestLibrary(t)
	lib.cfg.Library.OffsiteLocation = true
	placeVolumeInFirstSlot(lib, "VOLA0001")
	originalAddr := lib.slots[0].Address // Magazine1's 1st slot, address 1

	if _, err := lib.RequestOffsitePickup("VOLA0001", "scheduled_pickup", time.Now(), ""); err != nil {
		t.Fatalf("request pickup: %v", err)
	}
	if _, err := lib.CompleteOffsitePickup("VOLA0001"); err != nil {
		t.Fatalf("complete pickup: %v", err)
	}
	saved := lib.State()

	// "While the daemon is down", insert a new magazine before Magazine1,
	// so every one of Magazine1's slots - including the one this volume
	// left from - now computes to a different, higher address.
	cfg := lib.cfg
	cfg.Library.Magazines = []config.MagazineConfig{{ID: "Magazine0", Slots: 3}, {ID: "Magazine1", Slots: 2}}

	restored, err := New(cfg, &saved, nil, nil)
	if err != nil {
		t.Fatalf("restore library: %v", err)
	}
	var restoredMag1Addr int
	for _, s := range restored.slots {
		if s.MagazineID == "Magazine1" {
			restoredMag1Addr = s.Address
			break
		}
	}
	if restoredMag1Addr == originalAddr {
		t.Fatalf("test setup didn't actually shift addresses - Magazine1's 1st slot still has address %d", restoredMag1Addr)
	}

	var got *Volume
	for _, v := range restored.OffsiteVolumes() {
		if v.Barcode == "VOLA0001" {
			got = v
		}
	}
	if got == nil {
		t.Fatalf("volume lost across restart")
	}
	if got.Offsite == nil || got.Offsite.Origin == nil {
		t.Fatalf("expected Origin to survive restart, got %+v", got.Offsite)
	}
	wantOriginAddr := 4 // Magazine0 (3 slots, addresses 1-3) + Magazine1's 1st slot
	if got.Offsite.Origin.Address != wantOriginAddr {
		t.Fatalf("Origin.Address = %d, want %d (identity-based re-projection, not the stale raw address %d)",
			got.Offsite.Origin.Address, wantOriginAddr, originalAddr)
	}
}

// TestOffsitePendingPickupSurvivesRestart mirrors
// TestOffsiteVolumesArePersisted/TestDriveOriginSurvivesRestart for the new
// pending-pickup state - this codebase has hit exactly this class of bug
// (offsite-adjacent state silently dropped across a restart) twice before,
// so this is the single most important new test this feature adds.
func TestOffsitePendingPickupSurvivesRestart(t *testing.T) {
	lib := newTestLibrary(t)
	lib.cfg.Library.OffsiteLocation = true
	placeVolumeInFirstSlot(lib, "VOLA0001")
	due := time.Now().Add(48 * time.Hour).Truncate(time.Second)

	if _, err := lib.RequestOffsitePickup("VOLA0001", "scheduled_pickup", due, "req-restart-1"); err != nil {
		t.Fatalf("request offsite pickup: %v", err)
	}

	saved := lib.State()
	restored, err := New(lib.cfg, &saved, nil, nil)
	if err != nil {
		t.Fatalf("restore library: %v", err)
	}
	var got *Volume
	for _, s := range restored.Status().Slots {
		if s.Volume != nil && s.Volume.Barcode == "VOLA0001" {
			got = s.Volume
		}
	}
	if got == nil {
		t.Fatalf("volume lost across restart")
	}
	if got.Offsite == nil || got.Offsite.Status != OffsiteStatusPendingPickup {
		t.Fatalf("expected PendingPickup to survive restart, got %+v", got.Offsite)
	}
	if !got.Offsite.DueAt.Equal(due) {
		t.Fatalf("DueAt not preserved across restart: got %v, want %v", got.Offsite.DueAt, due)
	}
	if got.Offsite.RequestID != "req-restart-1" {
		t.Fatalf("RequestID not preserved across restart: got %q, want %q", got.Offsite.RequestID, "req-restart-1")
	}

	// The restored library must still be able to cancel it by that same
	// id - RequestID surviving the JSON round-trip is only useful if it
	// still functions as a live lookup key afterward.
	cancelled := restored.CancelOffsiteRequest("req-restart-1")
	if len(cancelled) != 1 || cancelled[0].Barcode != "VOLA0001" {
		t.Fatalf("expected the restored request to still be cancelable by id, got %+v", cancelled)
	}
}

func TestOffsiteRotationCandidatesSelectsLeastRecentlyCreatedFull(t *testing.T) {
	lib := newTestLibrary(t)
	cfg := lib.cfg
	cfg.Library.Magazines = []config.MagazineConfig{{ID: "Magazine1", Slots: 3}}
	if err := lib.Reconfigure(cfg); err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	base := time.Now()
	lib.slots[0].Volume = &Volume{Barcode: "VOLA0001", Full: true, CreatedAt: base.Add(2 * time.Hour)}
	lib.slots[1].Volume = &Volume{Barcode: "VOLA0002", Full: true, CreatedAt: base}
	lib.slots[2].Volume = &Volume{Barcode: "VOLA0003", Full: true, CreatedAt: base.Add(time.Hour)}

	refs := lib.OffsiteRotationCandidates(2)
	if len(refs) != 2 {
		t.Fatalf("expected 2 candidates, got %d: %+v", len(refs), refs)
	}
	if refs[0].Address != lib.slots[1].Address || refs[1].Address != lib.slots[2].Address {
		t.Fatalf("expected the two least-recently-created volumes first, got %+v", refs)
	}
}

// TestRequestOffsitePickupDuringConcurrentMoveBothSucceed exercises
// PendingPickup's core design property under -race: a tape merely
// scheduled for pickup is still ordinary, movable inventory (only a
// vaulted tape is structurally unreachable), so a Move racing a
// RequestOffsitePickup on the same volume must not block or corrupt
// either - the volume ends up at its new address, still carrying the
// PendingPickup record that CompleteOffsitePickup will later re-resolve
// by barcode rather than by a possibly-stale ElementRef (see
// CompleteOffsitePickup's own doc comment).
func TestRequestOffsitePickupDuringConcurrentMoveBothSucceed(t *testing.T) {
	const delay = 200 * time.Millisecond
	tmp := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = tmp
	cfg.Library.Magazines = []config.MagazineConfig{{ID: "Magazine1", Slots: 2}}
	cfg.Library.DefaultCapacity = "1MiB"
	cfg.Library.OffsiteLocation = true
	cfg.Library.Latency = config.LatencySettings{
		Enabled: true, DriveLoad: "0s", DriveUnload: "0s", TapePositioning: "0s",
		RobotMoveTape: delay.String(), RobotMoveScan: "0s", MagazineScan: "0s", DoorAction: "0s",
	}
	lib, err := New(cfg, nil, nil, nil)
	if err != nil {
		t.Fatalf("new library: %v", err)
	}
	lib.slots[0].Volume = &Volume{Barcode: "VOLA0001", Path: filepath.Join(tmp, "VOLA0001")}
	fromAddr := lib.slots[0].Address
	toAddr := lib.slots[1].Address

	done := make(chan error, 1)
	go func() {
		done <- lib.Move(ElementRef{Kind: KindSlot, Address: fromAddr}, ElementRef{Kind: KindSlot, Address: toAddr}, "")
	}()
	time.Sleep(delay / 4) // let Move grab the arm and release l.mu for its sleep

	if _, err := lib.RequestOffsitePickup("VOLA0001", "test-mode", time.Now().Add(time.Hour), ""); err != nil {
		t.Fatalf("request offsite pickup raced in during the move's sleep: %v", err)
	}

	if err := <-done; err != nil {
		t.Fatalf("expected Move to succeed alongside a PendingPickup request, got %v", err)
	}

	lib.mu.RLock()
	toVol := lib.slots[1].Volume
	lib.mu.RUnlock()
	if toVol == nil || toVol.Barcode != "VOLA0001" {
		t.Fatalf("expected the volume to have moved to its destination slot, got %+v", toVol)
	}
	if toVol.Offsite == nil || toVol.Offsite.Status != OffsiteStatusPendingPickup {
		t.Fatalf("expected the PendingPickup record to survive the move, got %+v", toVol.Offsite)
	}
}

func TestCancelOffsiteRequestRevertsPendingPickup(t *testing.T) {
	lib := newTestLibrary(t)
	lib.cfg.Library.OffsiteLocation = true
	placeVolumeInFirstSlot(lib, "VOLA0001")

	if _, err := lib.RequestOffsitePickup("VOLA0001", "scheduled_pickup", time.Now().Add(time.Hour), "req-1"); err != nil {
		t.Fatalf("request pickup: %v", err)
	}

	cancelled := lib.CancelOffsiteRequest("req-1")
	if len(cancelled) != 1 || cancelled[0].Barcode != "VOLA0001" || cancelled[0].Kind != "pickup" {
		t.Fatalf("expected one cancelled pickup item for VOLA0001, got %+v", cancelled)
	}

	vol := lib.slots[0].Volume
	if vol == nil || vol.Barcode != "VOLA0001" {
		t.Fatalf("expected the volume to still be in its slot, got %+v", vol)
	}
	if vol.Offsite != nil {
		t.Fatalf("expected Offsite to be cleared entirely after cancelling a pending pickup, got %+v", vol.Offsite)
	}
}

func TestCancelOffsiteRequestRevertsPendingReturn(t *testing.T) {
	lib := newTestLibrary(t)
	lib.cfg.Library.OffsiteLocation = true
	placeVolumeInFirstSlot(lib, "VOLA0001")

	if _, err := lib.RequestOffsitePickup("VOLA0001", "scheduled_pickup", time.Now(), ""); err != nil {
		t.Fatalf("request pickup: %v", err)
	}
	if _, err := lib.CompleteOffsitePickup("VOLA0001"); err != nil {
		t.Fatalf("complete pickup: %v", err)
	}
	if _, err := lib.RequestOffsiteReturn("VOLA0001", "scheduled_pickup", time.Now().Add(time.Hour), false, "req-2"); err != nil {
		t.Fatalf("request return: %v", err)
	}

	cancelled := lib.CancelOffsiteRequest("req-2")
	if len(cancelled) != 1 || cancelled[0].Barcode != "VOLA0001" || cancelled[0].Kind != "return" {
		t.Fatalf("expected one cancelled return item for VOLA0001, got %+v", cancelled)
	}

	vol, _ := lib.findOffsite("VOLA0001")
	if vol == nil {
		t.Fatal("expected the volume to still be in the offsite collection (vaulted, not returned)")
	}
	if vol.Offsite == nil || vol.Offsite.Status != OffsiteStatusVaulted {
		t.Fatalf("expected Status to revert to Vaulted, got %+v", vol.Offsite)
	}
	if !vol.Offsite.DueAt.IsZero() || vol.Offsite.Urgent || vol.Offsite.RequestID != "" {
		t.Fatalf("expected DueAt/Urgent/RequestID cleared after cancelling a pending return, got %+v", vol.Offsite)
	}
}

func TestCancelOffsiteRequestOnlyTouchesMatchingRequestID(t *testing.T) {
	lib := newTestLibrary(t)
	lib.cfg.Library.OffsiteLocation = true
	placeVolumeInFirstSlot(lib, "VOLA0001")
	lib.slots[1].Volume = &Volume{Barcode: "VOLB0001", TapeSet: testTapeSet, Path: filepath.Join(lib.cfg.DataDir, "VOLB0001")}

	if _, err := lib.RequestOffsitePickup("VOLA0001", "scheduled_pickup", time.Now().Add(time.Hour), "req-1"); err != nil {
		t.Fatalf("request pickup VOLA0001: %v", err)
	}
	if _, err := lib.RequestOffsitePickup("VOLB0001", "scheduled_pickup", time.Now().Add(time.Hour), "req-2"); err != nil {
		t.Fatalf("request pickup VOLB0001: %v", err)
	}

	cancelled := lib.CancelOffsiteRequest("req-1")
	if len(cancelled) != 1 || cancelled[0].Barcode != "VOLA0001" {
		t.Fatalf("expected only VOLA0001 cancelled, got %+v", cancelled)
	}
	if lib.slots[0].Volume.Offsite != nil {
		t.Fatalf("expected VOLA0001's offsite record cleared")
	}
	if lib.slots[1].Volume.Offsite == nil || lib.slots[1].Volume.Offsite.RequestID != "req-2" {
		t.Fatalf("expected VOLB0001 (a different request) to be untouched, got %+v", lib.slots[1].Volume.Offsite)
	}
}

// TestCancelOffsiteRequestSkipsAlreadyCompletedMember is the partial-cancel
// scenario: a batch's members can complete independently (e.g. the offsite
// Tick sweep) before an operator's cancel request lands. Cancelling must
// revert whichever members are still pending and simply skip the rest,
// mirroring CompleteDueTransfers' own "an operator moved a candidate out
// from under this sweep - skip it, not fatal" contract.
func TestCancelOffsiteRequestSkipsAlreadyCompletedMember(t *testing.T) {
	lib := newTestLibrary(t)
	lib.cfg.Library.OffsiteLocation = true
	placeVolumeInFirstSlot(lib, "VOLA0001")
	lib.slots[1].Volume = &Volume{Barcode: "VOLB0001", TapeSet: testTapeSet, Path: filepath.Join(lib.cfg.DataDir, "VOLB0001")}

	due := time.Now().Add(time.Hour)
	if _, err := lib.RequestOffsitePickup("VOLA0001", "scheduled_pickup", due, "req-1"); err != nil {
		t.Fatalf("request pickup VOLA0001: %v", err)
	}
	if _, err := lib.RequestOffsitePickup("VOLB0001", "scheduled_pickup", due, "req-1"); err != nil {
		t.Fatalf("request pickup VOLB0001: %v", err)
	}

	// VOLA0001 completes independently (e.g. the Tick sweep ran) before
	// the batch is cancelled.
	if _, err := lib.CompleteOffsitePickup("VOLA0001"); err != nil {
		t.Fatalf("complete pickup VOLA0001: %v", err)
	}

	cancelled := lib.CancelOffsiteRequest("req-1")
	if len(cancelled) != 1 || cancelled[0].Barcode != "VOLB0001" {
		t.Fatalf("expected only the still-pending VOLB0001 to be reverted, got %+v", cancelled)
	}
	if lib.slots[1].Volume.Offsite != nil {
		t.Fatalf("expected VOLB0001's pending pickup reverted")
	}
	// VOLA0001 stays vaulted - already completed, untouched by the cancel.
	if !containsBarcode(lib.OffsiteVolumes(), "VOLA0001") {
		t.Fatalf("expected VOLA0001 to remain vaulted (already completed before the cancel ran)")
	}
}

func TestCancelOffsiteRequestUnknownIDReturnsEmpty(t *testing.T) {
	lib := newTestLibrary(t)
	lib.cfg.Library.OffsiteLocation = true
	if cancelled := lib.CancelOffsiteRequest("does-not-exist"); len(cancelled) != 0 {
		t.Fatalf("expected no cancelled items for an unknown request id, got %+v", cancelled)
	}
}

func TestOffsitePendingRequestIDsReflectsLiveState(t *testing.T) {
	lib := newTestLibrary(t)
	lib.cfg.Library.OffsiteLocation = true
	placeVolumeInFirstSlot(lib, "VOLA0001")

	if ids := lib.OffsitePendingRequestIDs(); len(ids) != 0 {
		t.Fatalf("expected no pending request ids initially, got %+v", ids)
	}

	if _, err := lib.RequestOffsitePickup("VOLA0001", "scheduled_pickup", time.Now().Add(time.Hour), "req-1"); err != nil {
		t.Fatalf("request pickup: %v", err)
	}
	ids := lib.OffsitePendingRequestIDs()
	if !ids["req-1"] {
		t.Fatalf("expected req-1 to be pending, got %+v", ids)
	}

	if _, err := lib.CompleteOffsitePickup("VOLA0001"); err != nil {
		t.Fatalf("complete pickup: %v", err)
	}
	if ids := lib.OffsitePendingRequestIDs(); ids["req-1"] {
		t.Fatalf("expected req-1 to no longer be pending after completion, got %+v", ids)
	}
}

// TestCancelOffsiteRequestRacesCompleteOffsitePickupSafely exercises the
// documented cancel-vs-complete race under -race: whichever of
// CancelOffsiteRequest/CompleteOffsitePickup acquires l.mu first simply
// wins outright, leaving a coherent final state - never a data race, never
// a torn/inconsistent OffsiteInfo. Mirrors
// TestRequestOffsitePickupDuringConcurrentMoveBothSucceed's style.
func TestCancelOffsiteRequestRacesCompleteOffsitePickupSafely(t *testing.T) {
	lib := newTestLibrary(t)
	lib.cfg.Library.OffsiteLocation = true
	placeVolumeInFirstSlot(lib, "VOLA0001")

	if _, err := lib.RequestOffsitePickup("VOLA0001", "scheduled_pickup", time.Now(), "req-race"); err != nil {
		t.Fatalf("request pickup: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	var cancelled []CancelledOffsiteItem
	var completeErr error
	go func() {
		defer wg.Done()
		cancelled = lib.CancelOffsiteRequest("req-race")
	}()
	go func() {
		defer wg.Done()
		_, completeErr = lib.CompleteOffsitePickup("VOLA0001")
	}()
	wg.Wait()

	// Exactly one of the two must have "won": either Cancel reverted it
	// (Complete then failed, finding it no longer PendingPickup) or
	// Complete vaulted it first (Cancel then found nothing left to
	// revert) - never both, never neither.
	cancelWon := len(cancelled) == 1
	completeWon := completeErr == nil
	if cancelWon == completeWon {
		t.Fatalf("expected exactly one of cancel/complete to win, got cancelled=%+v completeErr=%v", cancelled, completeErr)
	}

	if cancelWon {
		if lib.slots[0].Volume == nil || lib.slots[0].Volume.Offsite != nil {
			t.Fatalf("cancel won: expected the volume back onsite with no Offsite record, got %+v", lib.slots[0].Volume)
		}
		if containsBarcode(lib.OffsiteVolumes(), "VOLA0001") {
			t.Fatalf("cancel won: volume should not be in the offsite collection")
		}
	} else {
		if lib.slots[0].Volume != nil {
			t.Fatalf("complete won: expected the source slot to be empty, got %+v", lib.slots[0].Volume)
		}
		if !containsBarcode(lib.OffsiteVolumes(), "VOLA0001") {
			t.Fatalf("complete won: expected the volume to be vaulted")
		}
	}
}

// TestCancelOffsiteRequestRacesCompleteOffsiteReturnSafely is
// TestCancelOffsiteRequestRacesCompleteOffsitePickupSafely's PendingReturn
// counterpart - CancelOffsiteRequest's return-side revert is a separate
// code path (l.offsite, not slots/ioslots) and deserves its own race
// coverage rather than assuming the pickup case proves both.
func TestCancelOffsiteRequestRacesCompleteOffsiteReturnSafely(t *testing.T) {
	lib := newTestLibrary(t)
	lib.cfg.Library.OffsiteLocation = true
	placeVolumeInFirstSlot(lib, "VOLA0001")

	if _, err := lib.RequestOffsitePickup("VOLA0001", "scheduled_pickup", time.Now(), ""); err != nil {
		t.Fatalf("request pickup: %v", err)
	}
	if _, err := lib.CompleteOffsitePickup("VOLA0001"); err != nil {
		t.Fatalf("complete pickup: %v", err)
	}
	if _, err := lib.RequestOffsiteReturn("VOLA0001", "scheduled_pickup", time.Now(), false, "req-race"); err != nil {
		t.Fatalf("request return: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	var cancelled []CancelledOffsiteItem
	var completeErr error
	go func() {
		defer wg.Done()
		cancelled = lib.CancelOffsiteRequest("req-race")
	}()
	go func() {
		defer wg.Done()
		_, completeErr = lib.CompleteOffsiteReturn("VOLA0001")
	}()
	wg.Wait()

	cancelWon := len(cancelled) == 1
	completeWon := completeErr == nil
	if cancelWon == completeWon {
		t.Fatalf("expected exactly one of cancel/complete to win, got cancelled=%+v completeErr=%v", cancelled, completeErr)
	}

	if cancelWon {
		vol, _ := lib.findOffsite("VOLA0001")
		if vol == nil || vol.Offsite == nil || vol.Offsite.Status != OffsiteStatusVaulted {
			t.Fatalf("cancel won: expected the volume reverted to Vaulted, got %+v", vol)
		}
	} else {
		if containsBarcode(lib.OffsiteVolumes(), "VOLA0001") {
			t.Fatalf("complete won: volume should have left the offsite collection")
		}
		if !containsBarcode(lib.OutsideVolumes(), "VOLA0001") {
			t.Fatalf("complete won: expected the volume in outside-library inventory")
		}
	}
}
