package api

import (
	"testing"

	"github.com/swenske/gotochanger/internal/config"
	"github.com/swenske/gotochanger/internal/library"
)

func ioslotsInMailboxForTest(ioslots []*library.IOSlot, mailboxID string) []*library.IOSlot {
	var out []*library.IOSlot
	for _, io := range ioslots {
		if io.MailboxID == mailboxID {
			out = append(out, io)
		}
	}
	return out
}

// TestWizardMagazineStepRefusesShrinkingOccupiedMagazine is a regression
// test for the gap identified while removing fixed-block addressing:
// UpdateWizardState has no guard against being resubmitted after the
// wizard is already completed and the system is live with real volumes,
// so the wizard's magazine step (step 3) needs the same occupancy
// protection handleUpdateMagazine/handleDeleteMagazine give the Admin API
// - see checkMagazineResubmissionSafe.
func TestWizardMagazineStepRefusesShrinkingOccupiedMagazine(t *testing.T) {
	s := newTopologyTestServer(t, 1)

	if _, err := s.UpdateWizardState(WizardRequest{Step: 3, Magazines: []config.MagazineConfig{{ID: "Magazine1", Slots: 10}}}); err != nil {
		t.Fatalf("initial magazine step: %v", err)
	}
	// SaveMagazines only ever touches the store - "nothing hot-applies
	// until CompleteWizard" (see wizard.go). Hot-apply it here to simulate
	// the real-world danger this check exists for: an admin reopening the
	// wizard against a system whose wizard was already completed and is
	// now live with real volumes.
	if err := s.reconfigureFromStore(); err != nil {
		t.Fatalf("hot-apply initial magazine step: %v", err)
	}

	tail := slotsInMagazineForTest(s.lib.Status().Slots, "Magazine1")[9]
	loadVolumeIntoSlot(t, s.lib, "Magazine1", tail.Address, "VOLA0001")

	if _, err := s.UpdateWizardState(WizardRequest{Step: 3, Magazines: []config.MagazineConfig{{ID: "Magazine1", Slots: 5}}}); err == nil {
		t.Fatalf("expected an error resubmitting the magazine step with a shrink that would orphan the occupied tail slot")
	}
	if got := slotsInMagazineForTest(s.lib.Status().Slots, "Magazine1"); len(got) != 10 {
		t.Fatalf("expected the refused resubmission to leave Magazine1 at 10 slots, got %d", len(got))
	}
}

// TestWizardMagazineStepRefusesDroppingOccupiedMagazine covers removing an
// occupied magazine from the resubmitted list entirely (not just shrinking
// it), which SaveMagazines' delete-and-reinsert semantics would otherwise
// silently drop from tracking with its volume still inside.
func TestWizardMagazineStepRefusesDroppingOccupiedMagazine(t *testing.T) {
	s := newTopologyTestServer(t, 1)

	if _, err := s.UpdateWizardState(WizardRequest{Step: 3, Magazines: []config.MagazineConfig{
		{ID: "Magazine1", Slots: 5},
		{ID: "Magazine2", Slots: 5},
	}}); err != nil {
		t.Fatalf("initial magazine step: %v", err)
	}
	if err := s.reconfigureFromStore(); err != nil {
		t.Fatalf("hot-apply initial magazine step: %v", err)
	}

	first := slotsInMagazineForTest(s.lib.Status().Slots, "Magazine1")[0]
	loadVolumeIntoSlot(t, s.lib, "Magazine1", first.Address, "VOLA0001")

	if _, err := s.UpdateWizardState(WizardRequest{Step: 3, Magazines: []config.MagazineConfig{
		{ID: "Magazine2", Slots: 5},
	}}); err == nil {
		t.Fatalf("expected an error resubmitting the magazine step with Magazine1 dropped while it's still occupied")
	}
	if got := slotsInMagazineForTest(s.lib.Status().Slots, "Magazine1"); len(got) != 5 {
		t.Fatalf("expected the refused resubmission to leave Magazine1 in place, got %d slots", len(got))
	}
}

// TestWizardMailboxStepRefusesShrinkingOccupiedMailbox mirrors the
// magazine-step protection for the wizard's mailbox step (step 4).
func TestWizardMailboxStepRefusesShrinkingOccupiedMailbox(t *testing.T) {
	s := newTopologyTestServer(t, 1)

	if _, err := s.UpdateWizardState(WizardRequest{Step: 4, Mailboxes: []config.MailboxConfig{{ID: "Mailbox1", Slots: 4}}}); err != nil {
		t.Fatalf("initial mailbox step: %v", err)
	}
	if err := s.reconfigureFromStore(); err != nil {
		t.Fatalf("hot-apply initial mailbox step: %v", err)
	}

	tail := ioslotsInMailboxForTest(s.lib.Status().IOSlots, "Mailbox1")[3]
	if _, err := s.lib.CreateManualCartridge(testTapeSet, "VOLA0001"); err != nil {
		t.Fatalf("create manual cartridge: %v", err)
	}
	if err := s.lib.OpenIODoor("Mailbox1", ""); err != nil {
		t.Fatalf("open io door: %v", err)
	}
	if err := s.lib.CloseIODoor("Mailbox1", []library.DoorAction{{Action: "load", Address: tail.Address, Barcode: "VOLA0001"}}); err != nil {
		t.Fatalf("close io door: %v", err)
	}

	if _, err := s.UpdateWizardState(WizardRequest{Step: 4, Mailboxes: []config.MailboxConfig{{ID: "Mailbox1", Slots: 2}}}); err == nil {
		t.Fatalf("expected an error resubmitting the mailbox step with a shrink that would orphan the occupied tail slot")
	}
	if got := ioslotsInMailboxForTest(s.lib.Status().IOSlots, "Mailbox1"); len(got) != 4 {
		t.Fatalf("expected the refused resubmission to leave Mailbox1 at 4 slots, got %d", len(got))
	}
}
