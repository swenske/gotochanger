package offsite

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/swenske/gotochanger/internal/library"
	"github.com/swenske/gotochanger/internal/store"
)

// ErrRequestNotCancellable means a request id is known (a real
// offsite_requests row exists) but has nothing left to cancel - either
// already explicitly cancelled, or every member barcode already resolved
// naturally before the cancel arrived. Deliberately distinct from
// library.ErrNotFound ("no such request id at all"): only Engine can tell
// the two apart, since only it sees the offsite_requests row - see
// internal/api's statusFor() for the one place outside internal/library a
// domain sentinel needs its own explicit HTTP-status mapping.
var ErrRequestNotCancellable = errors.New("offsite request has nothing left to cancel")

// newRequestID generates a batch request's unique id - 16 hex characters
// (8 random bytes), plenty of entropy for this feature's actual scale
// (never more than a handful of open requests at once) without the
// security-credential-grade 32 bytes internal/api/tokens.go's
// generateToken uses for session/API tokens - this id is meant to be read
// off a table and typed into a CLI command, not kept secret.
func newRequestID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate offsite request id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// RequestPickupBatch is the shared batch primitive every mode with manual
// pickup support should call, instead of looping RequestPickupOne itself:
// mints one shared request id, loops RequestPickupOne per barcode (each
// still gets its own independent success/failure, matching this
// package's existing best-effort/partial-success contract), and - only if
// at least one barcode actually succeeded - persists the batch's identity
// via Store.CreateOffsiteRequest so it becomes listable/cancelable. A
// batch where every barcode failed mints no id and persists no request
// row: there would be nothing live for that id to ever refer to.
func (e *Engine) RequestPickupBatch(barcodes []string, mode string, dueAt time.Time, actor string) (requestID string, results []ItemResult) {
	id, err := newRequestID()
	if err != nil {
		return "", unsupportedResults(barcodes, err.Error())
	}
	results = make([]ItemResult, len(barcodes))
	var succeeded []string
	for i, bc := range barcodes {
		results[i] = e.RequestPickupOne(bc, mode, dueAt, id, actor)
		if results[i].OK {
			succeeded = append(succeeded, bc)
		}
	}
	if len(succeeded) == 0 {
		return "", results
	}
	if err := e.store.CreateOffsiteRequest(store.OffsiteRequest{
		ID: id, Kind: "pickup", Mode: mode, Barcodes: succeeded, CreatedAt: time.Now().UTC(), Actor: actor,
	}); err != nil {
		// The Library-side mutations already succeeded for `succeeded` -
		// undoing them because the request row itself failed to persist
		// would be worse than a request that's usable but not yet
		// listable/cancelable by id until this is investigated.
		e.lib.RecordEvent(library.Event{
			Code:    library.EventCodeSystemPersistFailure,
			Message: "failed to persist offsite request record",
			Detail:  map[string]string{"request_id": id, "mode": mode, "error": err.Error()},
		})
	}
	return id, results
}

// RequestReturnBatch is RequestPickupBatch's return-side counterpart.
func (e *Engine) RequestReturnBatch(barcodes []string, mode string, dueAt time.Time, urgent bool, actor string) (requestID string, results []ItemResult) {
	id, err := newRequestID()
	if err != nil {
		return "", unsupportedResults(barcodes, err.Error())
	}
	results = make([]ItemResult, len(barcodes))
	var succeeded []string
	for i, bc := range barcodes {
		results[i] = e.RequestReturnOne(bc, mode, dueAt, urgent, id, actor)
		if results[i].OK {
			succeeded = append(succeeded, bc)
		}
	}
	if len(succeeded) == 0 {
		return "", results
	}
	if err := e.store.CreateOffsiteRequest(store.OffsiteRequest{
		ID: id, Kind: "return", Mode: mode, Barcodes: succeeded, Urgent: urgent, CreatedAt: time.Now().UTC(), Actor: actor,
	}); err != nil {
		e.lib.RecordEvent(library.Event{
			Code:    library.EventCodeSystemPersistFailure,
			Message: "failed to persist offsite request record",
			Detail:  map[string]string{"request_id": id, "mode": mode, "error": err.Error()},
		})
	}
	return id, results
}

// CancelOffsiteRequest cancels a previously-created batch request by id:
// looks it up (unknown id -> library.ErrNotFound), rejects an
// already-resolved one (ErrRequestNotCancellable - either explicitly
// cancelled before, or every member barcode already completed on its
// own), reverts whichever members are still pending via
// Library.CancelOffsiteRequest, records one movement per barcode actually
// reverted, and marks the request row cancelled. Library's own
// CancelOffsiteRequest already emits one success event per reverted
// volume (see internal/library/event_taxonomy.go's "offsite-pickup-
// cancel"/"offsite-return-cancel" cases) - this method only ever records
// a *failure* event itself, matching every other offsite action's
// existing success-in-Library/failure-in-Engine split.
func (e *Engine) CancelOffsiteRequest(requestID, actor string) ([]library.CancelledOffsiteItem, error) {
	req, ok, err := e.store.GetOffsiteRequest(requestID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("offsite request %q: %w", requestID, library.ErrNotFound)
	}

	failureCode := library.EventCodeOffsitePickupCancelFailure
	if req.Kind == "return" {
		failureCode = library.EventCodeOffsiteReturnCancelFailure
	}

	if req.CancelledAt != nil {
		e.lib.RecordEvent(library.Event{
			Code: failureCode, Message: fmt.Sprintf("offsite request %q is already cancelled", requestID),
			Detail: map[string]string{"request_id": requestID, "mode": req.Mode},
		})
		return nil, fmt.Errorf("offsite request %q: %w", requestID, ErrRequestNotCancellable)
	}

	cancelled := e.lib.CancelOffsiteRequest(requestID)
	if len(cancelled) == 0 {
		e.lib.RecordEvent(library.Event{
			Code: failureCode, Message: fmt.Sprintf("offsite request %q has nothing left to cancel", requestID),
			Detail: map[string]string{"request_id": requestID, "mode": req.Mode},
		})
		return nil, fmt.Errorf("offsite request %q: %w", requestID, ErrRequestNotCancellable)
	}

	for _, item := range cancelled {
		e.recordMovement(store.OffsiteMovement{
			Barcode: item.Barcode, Direction: item.Kind + "_cancelled", Mode: req.Mode, Actor: actor, RequestID: requestID,
		})
	}
	if _, err := e.store.CancelOffsiteRequestRecord(requestID, time.Now().UTC()); err != nil {
		e.lib.RecordEvent(library.Event{
			Code:    library.EventCodeSystemPersistFailure,
			Message: "failed to mark offsite request cancelled",
			Detail:  map[string]string{"request_id": requestID, "error": err.Error()},
		})
	}
	return cancelled, nil
}
