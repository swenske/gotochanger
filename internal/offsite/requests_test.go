package offsite

import (
	"errors"
	"testing"
	"time"

	"github.com/swenske/gotochanger/internal/library"
	"github.com/swenske/gotochanger/internal/mailer"
)

func TestRequestPickupBatchSharesOneIDAcrossSuccesses(t *testing.T) {
	lib := newTestLibrary(t)
	st := newTestStore(t)
	eng := NewEngine(lib, st, mailer.NoopMailer{}, NewRegistry())

	status := lib.Status()
	rackCartridge(t, lib, "VOLA0001", status.Slots[0].Address)
	rackCartridge(t, lib, "VOLA0002", status.Slots[1].Address)

	due := time.Now().Add(time.Hour)
	id, results := eng.RequestPickupBatch([]string{"VOLA0001", "VOLA0002"}, "scheduled_pickup", due, "Admin")
	if id == "" {
		t.Fatal("expected a non-empty request id")
	}
	if len(results) != 2 || !results[0].OK || !results[1].OK {
		t.Fatalf("expected both barcodes to succeed, got %+v", results)
	}
	if results[0].RequestID != id || results[1].RequestID != id {
		t.Fatalf("expected both results to carry the shared request id %q, got %+v", id, results)
	}

	req, ok, err := st.GetOffsiteRequest(id)
	if err != nil || !ok {
		t.Fatalf("get offsite request: ok=%v err=%v", ok, err)
	}
	if req.Kind != "pickup" || req.Mode != "scheduled_pickup" || req.Actor != "Admin" {
		t.Fatalf("unexpected request fields: %+v", req)
	}
	if len(req.Barcodes) != 2 || req.Barcodes[0] != "VOLA0001" || req.Barcodes[1] != "VOLA0002" {
		t.Fatalf("expected both barcodes persisted, got %+v", req.Barcodes)
	}
}

func TestRequestPickupBatchFailedBarcodesHaveNoRequestID(t *testing.T) {
	lib := newTestLibrary(t)
	st := newTestStore(t)
	eng := NewEngine(lib, st, mailer.NoopMailer{}, NewRegistry())

	status := lib.Status()
	rackCartridge(t, lib, "VOLA0001", status.Slots[0].Address)

	due := time.Now().Add(time.Hour)
	id, results := eng.RequestPickupBatch([]string{"VOLA0001", "NOSUCHVOL"}, "scheduled_pickup", due, "Admin")
	if id == "" {
		t.Fatal("expected a non-empty request id (at least one barcode succeeded)")
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %+v", results)
	}
	if !results[0].OK || results[0].RequestID != id {
		t.Fatalf("expected VOLA0001 to succeed and carry the request id, got %+v", results[0])
	}
	if results[1].OK || results[1].RequestID != "" {
		t.Fatalf("expected NOSUCHVOL to fail with no request id, got %+v", results[1])
	}

	req, ok, err := st.GetOffsiteRequest(id)
	if err != nil || !ok {
		t.Fatalf("get offsite request: ok=%v err=%v", ok, err)
	}
	if len(req.Barcodes) != 1 || req.Barcodes[0] != "VOLA0001" {
		t.Fatalf("expected only the successful barcode persisted, got %+v", req.Barcodes)
	}
}

func TestRequestPickupBatchAllFailedPersistsNoRequest(t *testing.T) {
	lib := newTestLibrary(t)
	st := newTestStore(t)
	eng := NewEngine(lib, st, mailer.NoopMailer{}, NewRegistry())

	id, results := eng.RequestPickupBatch([]string{"NOSUCHVOL"}, "scheduled_pickup", time.Now().Add(time.Hour), "Admin")
	if id != "" {
		t.Fatalf("expected no request id when every barcode fails, got %q", id)
	}
	if len(results) != 1 || results[0].OK {
		t.Fatalf("expected the one barcode to fail, got %+v", results)
	}

	all, err := st.ListOffsiteRequests(10)
	if err != nil {
		t.Fatalf("list offsite requests: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("expected no request row persisted when nothing succeeded, got %+v", all)
	}
}

func TestRequestReturnBatchSharesOneID(t *testing.T) {
	lib := newTestLibrary(t)
	st := newTestStore(t)
	eng := NewEngine(lib, st, mailer.NoopMailer{}, NewRegistry())

	status := lib.Status()
	rackCartridge(t, lib, "VOLA0001", status.Slots[0].Address)
	rackCartridge(t, lib, "VOLA0002", status.Slots[1].Address)
	for _, bc := range []string{"VOLA0001", "VOLA0002"} {
		if _, err := lib.RequestOffsitePickup(bc, "scheduled_pickup", time.Now(), ""); err != nil {
			t.Fatalf("request pickup %s: %v", bc, err)
		}
		if _, err := lib.CompleteOffsitePickup(bc); err != nil {
			t.Fatalf("complete pickup %s: %v", bc, err)
		}
	}

	due := time.Now().Add(time.Hour)
	id, results := eng.RequestReturnBatch([]string{"VOLA0001", "VOLA0002"}, "scheduled_pickup", due, false, "Admin")
	if id == "" {
		t.Fatal("expected a non-empty request id")
	}
	if len(results) != 2 || !results[0].OK || !results[1].OK {
		t.Fatalf("expected both barcodes to succeed, got %+v", results)
	}

	req, ok, err := st.GetOffsiteRequest(id)
	if err != nil || !ok {
		t.Fatalf("get offsite request: ok=%v err=%v", ok, err)
	}
	if req.Kind != "return" || req.Urgent {
		t.Fatalf("unexpected request fields: %+v", req)
	}
	if len(req.Barcodes) != 2 {
		t.Fatalf("expected both barcodes persisted, got %+v", req.Barcodes)
	}
}

func TestCancelOffsiteRequestUnknownIDReturnsNotFound(t *testing.T) {
	lib := newTestLibrary(t)
	st := newTestStore(t)
	eng := NewEngine(lib, st, mailer.NoopMailer{}, NewRegistry())

	if _, err := eng.CancelOffsiteRequest("does-not-exist", "Admin"); !errors.Is(err, library.ErrNotFound) {
		t.Fatalf("expected library.ErrNotFound for an unknown request id, got %v", err)
	}
}

func TestCancelOffsiteRequestAlreadyCancelledReturnsNotCancellable(t *testing.T) {
	lib := newTestLibrary(t)
	st := newTestStore(t)
	eng := NewEngine(lib, st, mailer.NoopMailer{}, NewRegistry())

	status := lib.Status()
	rackCartridge(t, lib, "VOLA0001", status.Slots[0].Address)
	id, results := eng.RequestPickupBatch([]string{"VOLA0001"}, "scheduled_pickup", time.Now().Add(time.Hour), "Admin")
	if !results[0].OK {
		t.Fatalf("request pickup: %+v", results)
	}

	if _, err := eng.CancelOffsiteRequest(id, "Admin"); err != nil {
		t.Fatalf("first cancel: %v", err)
	}
	if _, err := eng.CancelOffsiteRequest(id, "Admin"); !errors.Is(err, ErrRequestNotCancellable) {
		t.Fatalf("expected ErrRequestNotCancellable for a second cancel, got %v", err)
	}
}

func TestCancelOffsiteRequestNothingLeftReturnsNotCancellable(t *testing.T) {
	lib := newTestLibrary(t)
	st := newTestStore(t)
	eng := NewEngine(lib, st, mailer.NoopMailer{}, NewRegistry())

	status := lib.Status()
	rackCartridge(t, lib, "VOLA0001", status.Slots[0].Address)
	id, results := eng.RequestPickupBatch([]string{"VOLA0001"}, "scheduled_pickup", time.Now().Add(-time.Minute), "Admin")
	if !results[0].OK {
		t.Fatalf("request pickup: %+v", results)
	}
	// Completed independently (e.g. the Tick sweep) before the cancel
	// arrives - nothing left for CancelOffsiteRequest to revert, even
	// though the request row itself was never explicitly cancelled.
	if _, err := lib.CompleteOffsitePickup("VOLA0001"); err != nil {
		t.Fatalf("complete pickup: %v", err)
	}

	if _, err := eng.CancelOffsiteRequest(id, "Admin"); !errors.Is(err, ErrRequestNotCancellable) {
		t.Fatalf("expected ErrRequestNotCancellable when nothing is left to revert, got %v", err)
	}
}

func TestCancelOffsiteRequestRecordsMovementsAndMarksCancelled(t *testing.T) {
	lib := newTestLibrary(t)
	st := newTestStore(t)
	eng := NewEngine(lib, st, mailer.NoopMailer{}, NewRegistry())

	status := lib.Status()
	rackCartridge(t, lib, "VOLA0001", status.Slots[0].Address)
	rackCartridge(t, lib, "VOLA0002", status.Slots[1].Address)
	id, results := eng.RequestPickupBatch([]string{"VOLA0001", "VOLA0002"}, "scheduled_pickup", time.Now().Add(time.Hour), "Admin")
	if !results[0].OK || !results[1].OK {
		t.Fatalf("request pickup: %+v", results)
	}

	cancelled, err := eng.CancelOffsiteRequest(id, "Operator1")
	if err != nil {
		t.Fatalf("cancel offsite request: %v", err)
	}
	if len(cancelled) != 2 {
		t.Fatalf("expected both barcodes reverted, got %+v", cancelled)
	}

	movements, err := st.ListOffsiteMovements("", 10)
	if err != nil {
		t.Fatalf("list movements: %v", err)
	}
	var cancelMovements int
	for _, m := range movements {
		if m.Direction != "pickup_cancelled" {
			continue
		}
		cancelMovements++
		if m.Actor != "Operator1" || m.RequestID != id {
			t.Fatalf("expected pickup_cancelled movement to carry actor+request_id, got %+v", m)
		}
	}
	if cancelMovements != 2 {
		t.Fatalf("expected 2 pickup_cancelled movements (one per barcode), got %d: %+v", cancelMovements, movements)
	}

	req, ok, err := st.GetOffsiteRequest(id)
	if err != nil || !ok {
		t.Fatalf("get offsite request: ok=%v err=%v", ok, err)
	}
	if req.CancelledAt == nil {
		t.Fatal("expected cancelled_at to be set after a successful cancel")
	}
}

// TestCancelOffsiteRequestPartialCancelScenario is the batch analogue of
// internal/library's TestCancelOffsiteRequestSkipsAlreadyCompletedMember:
// a member barcode that already completed independently (e.g. the offsite
// Tick sweep beat the operator's cancel request) is simply skipped, not
// treated as a failure of the whole cancel.
func TestCancelOffsiteRequestPartialCancelScenario(t *testing.T) {
	lib := newTestLibrary(t)
	st := newTestStore(t)
	eng := NewEngine(lib, st, mailer.NoopMailer{}, NewRegistry())

	status := lib.Status()
	rackCartridge(t, lib, "VOLA0001", status.Slots[0].Address)
	rackCartridge(t, lib, "VOLA0002", status.Slots[1].Address)
	id, results := eng.RequestPickupBatch([]string{"VOLA0001", "VOLA0002"}, "scheduled_pickup", time.Now().Add(time.Hour), "Admin")
	if !results[0].OK || !results[1].OK {
		t.Fatalf("request pickup: %+v", results)
	}

	if _, err := lib.CompleteOffsitePickup("VOLA0001"); err != nil {
		t.Fatalf("complete pickup VOLA0001: %v", err)
	}

	cancelled, err := eng.CancelOffsiteRequest(id, "Admin")
	if err != nil {
		t.Fatalf("cancel offsite request: %v", err)
	}
	if len(cancelled) != 1 || cancelled[0].Barcode != "VOLA0002" {
		t.Fatalf("expected only the still-pending VOLA0002 reverted, got %+v", cancelled)
	}

	movements, err := st.ListOffsiteMovements("", 10)
	if err != nil {
		t.Fatalf("list movements: %v", err)
	}
	var cancelMovements int
	for _, m := range movements {
		if m.Direction == "pickup_cancelled" {
			cancelMovements++
			if m.Barcode != "VOLA0002" {
				t.Fatalf("expected the cancel movement to be for VOLA0002 only, got %+v", m)
			}
		}
	}
	if cancelMovements != 1 {
		t.Fatalf("expected exactly 1 pickup_cancelled movement, got %d: %+v", cancelMovements, movements)
	}
}
