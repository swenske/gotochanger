package addressing

import (
	"testing"

	"github.com/swenske/gotochanger/internal/library"
)

// TestUnscopedIsIdentity covers the whole-physical-library case (no
// logical-library scoping): addresses are already dense from 1, so
// presented must equal physical.
func TestUnscopedIsIdentity(t *testing.T) {
	st := library.Status{
		Slots:   []*library.Slot{{Address: 1}, {Address: 2}, {Address: 3}},
		IOSlots: []*library.IOSlot{{Address: 4}, {Address: 5}},
	}
	a := Build(st)
	for _, addr := range []int{1, 2, 3, 4, 5} {
		if got := a.Present(addr); got != addr {
			t.Fatalf("Present(%d) = %d, want %d (identity)", addr, got, addr)
		}
		if got, err := a.Physical(addr); err != nil || got != addr {
			t.Fatalf("Physical(%d) = %d, %v, want %d, nil", addr, got, err, addr)
		}
	}
}

// TestScopedRenumbers reproduces the real-world bug: a second logical
// library whose elements sit at high, non-contiguous physical addresses
// (storage slots 41-45, I/O slots 106-110, matching the
// bareos-disk-sd-int-fr1 "Library2"/TAPES-EXPORT incident) must be
// renumbered to a dense 1-based range before being shown to an outside
// protocol (Bareos, or a real SCSI initiator).
func TestScopedRenumbers(t *testing.T) {
	st := library.Status{
		Slots: []*library.Slot{
			{Address: 41}, {Address: 42}, {Address: 43}, {Address: 44}, {Address: 45},
		},
		IOSlots: []*library.IOSlot{
			{Address: 106}, {Address: 107},
		},
	}
	a := Build(st)

	wantPresent := map[int]int{41: 1, 42: 2, 43: 3, 44: 4, 45: 5, 106: 6, 107: 7}
	for physical, want := range wantPresent {
		if got := a.Present(physical); got != want {
			t.Fatalf("Present(%d) = %d, want %d", physical, got, want)
		}
	}

	wantPhysical := map[int]int{1: 41, 2: 42, 3: 43, 4: 44, 5: 45, 6: 106, 7: 107}
	for presented, want := range wantPhysical {
		got, err := a.Physical(presented)
		if err != nil {
			t.Fatalf("Physical(%d): unexpected error: %v", presented, err)
		}
		if got != want {
			t.Fatalf("Physical(%d) = %d, want %d", presented, got, want)
		}
	}

	if _, err := a.Physical(8); err == nil {
		t.Fatal("expected error for out-of-range presented address")
	}
}

// TestDriveIndices reproduces the companion drive-index bug found on the
// same real-world Library2/TAPES-EXPORT incident: Bareos computes its
// "drive N" argument as the 0-based position within that Autochanger's own
// Device list ("Drive4, Drive5" -> local 0, 1), not gotochangerd's global
// physical drive index (4, 5) - and a real SCSI changer's data-transfer-
// element numbering works the same way.
func TestDriveIndices(t *testing.T) {
	st := library.Status{
		Drives: []*library.Drive{
			{Index: 4}, {Index: 5},
		},
	}
	a := Build(st)

	if got := a.PresentDrive(4); got != 0 {
		t.Fatalf("PresentDrive(4) = %d, want 0", got)
	}
	if got := a.PresentDrive(5); got != 1 {
		t.Fatalf("PresentDrive(5) = %d, want 1", got)
	}

	if got, err := a.PhysicalDrive(0); err != nil || got != 4 {
		t.Fatalf("PhysicalDrive(0) = %d, %v, want 4, nil", got, err)
	}
	if got, err := a.PhysicalDrive(1); err != nil || got != 5 {
		t.Fatalf("PhysicalDrive(1) = %d, %v, want 5, nil", got, err)
	}

	if _, err := a.PhysicalDrive(2); err == nil {
		t.Fatal("expected error for out-of-range presented drive index")
	}
}
