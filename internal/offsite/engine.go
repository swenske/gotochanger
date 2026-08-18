package offsite

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/swenske/gotochanger/internal/library"
	"github.com/swenske/gotochanger/internal/mailer"
	"github.com/swenske/gotochanger/internal/store"
)

// LibraryOffsite is the narrow slice of *library.Library every mode needs,
// declared here rather than imported from internal/api so this package has
// no dependency on internal/api - mirrors internal/api's own
// TopologyStore/BackupStore pattern of a consumer-declared interface
// satisfied by a concrete type it doesn't otherwise need the whole
// surface of.
type LibraryOffsite interface {
	OffsiteEnabled() bool
	RequestOffsitePickup(barcode, mode string, dueAt time.Time, requestID string) (*library.Volume, error)
	RequestOffsiteReturn(barcode, mode string, dueAt time.Time, urgent bool, requestID string) (*library.Volume, error)
	CompleteOffsitePickup(barcode string) (*library.Volume, error)
	CompleteOffsiteReturn(barcode string) (*library.Volume, error)
	OffsiteSendMode(from library.ElementRef, meta library.OffsiteMeta) (*library.Volume, error)
	OffsiteRecallMode(barcode string, to library.ElementRef, meta library.OffsiteMeta) error
	OffsitePendingByMode(mode string) (pickups, returns []*library.Volume)
	OffsiteRotationCandidates(count int) []library.ElementRef
	CancelOffsiteRequest(requestID string) []library.CancelledOffsiteItem
	OffsitePendingRequestIDs() map[string]bool
	RecordEvent(evt library.Event)
}

// Store is this package's own narrow cut of *store.Store, declared
// separately from internal/api.TopologyStore since this package must not
// import internal/api (that would cycle back through it).
type Store interface {
	GetOffsiteModeConfig(mode string) (json.RawMessage, bool, error)
	RecordOffsiteMovement(store.OffsiteMovement) error
	GetSetting(key string) (string, bool, error)
	SetSetting(key, value string) error
	CreateOffsiteRequest(store.OffsiteRequest) error
	GetOffsiteRequest(id string) (store.OffsiteRequest, bool, error)
	CancelOffsiteRequestRecord(id string, cancelledAt time.Time) (bool, error)
}

// Engine is what every OffsiteMode method receives: the shared primitives
// (send-and-record, due-transfer completion) so modes don't each
// duplicate movement-history/event recording.
type Engine struct {
	lib      LibraryOffsite
	store    Store
	mailer   mailer.Mailer
	registry *Registry
}

func NewEngine(lib LibraryOffsite, st Store, m mailer.Mailer, reg *Registry) *Engine {
	return &Engine{lib: lib, store: st, mailer: m, registry: reg}
}

func (e *Engine) Registry() *Registry { return e.registry }

// SendNow performs one mode-attributed offsite send and records it to
// movement history and the event log - the one place any mode's automatic
// send gets recorded, so ContinuousRotationMode (and any future mode that
// sends outside the request/complete flow) doesn't duplicate this.
func (e *Engine) SendNow(ref library.ElementRef, mode string) (*library.Volume, error) {
	vol, err := e.lib.OffsiteSendMode(ref, library.OffsiteMeta{Mode: mode})
	if err != nil {
		e.lib.RecordEvent(library.Event{
			Code:    library.EventCodeOffsiteSendFailure,
			Message: fmt.Sprintf("mode %q failed to send a volume offsite", mode),
			Detail:  map[string]string{"mode": mode, "error": err.Error()},
		})
		return nil, err
	}
	e.recordMovement(store.OffsiteMovement{Barcode: vol.Barcode, Direction: "sent_offsite", Status: string(library.OffsiteStatusVaulted), Mode: mode})
	return vol, nil
}

// RequestPickupOne requests one barcode's pickup via mode, recording
// movement history and an event on both outcomes - the shared
// implementation RequestPickupBatch loops over per barcode. requestID may
// be empty for a caller with no batch concept of its own (none exist
// today, but nothing here requires one).
func (e *Engine) RequestPickupOne(barcode, mode string, dueAt time.Time, requestID, actor string) ItemResult {
	vol, err := e.lib.RequestOffsitePickup(barcode, mode, dueAt, requestID)
	if err != nil {
		e.lib.RecordEvent(library.Event{
			Code:    library.EventCodeOffsitePickupRequestFailure,
			Message: fmt.Sprintf("offsite pickup request for %q failed", barcode),
			Detail:  map[string]string{"mode": mode, "volume": barcode, "error": err.Error()},
		})
		return ItemResult{Barcode: barcode, OK: false, Error: err.Error()}
	}
	e.recordMovement(store.OffsiteMovement{
		Barcode: vol.Barcode, Direction: "pickup_requested", Status: string(library.OffsiteStatusPendingPickup),
		Mode: mode, Actor: actor, RequestID: requestID,
	})
	due := dueAt
	return ItemResult{Barcode: barcode, OK: true, DueAt: &due, RequestID: requestID}
}

// RequestReturnOne is RequestPickupOne's counterpart.
func (e *Engine) RequestReturnOne(barcode, mode string, dueAt time.Time, urgent bool, requestID, actor string) ItemResult {
	vol, err := e.lib.RequestOffsiteReturn(barcode, mode, dueAt, urgent, requestID)
	if err != nil {
		e.lib.RecordEvent(library.Event{
			Code:    library.EventCodeOffsiteReturnRequestFailure,
			Message: fmt.Sprintf("offsite return request for %q failed", barcode),
			Detail:  map[string]string{"mode": mode, "volume": barcode, "error": err.Error()},
		})
		return ItemResult{Barcode: barcode, OK: false, Error: err.Error()}
	}
	e.recordMovement(store.OffsiteMovement{
		Barcode: vol.Barcode, Direction: "return_requested", Status: string(library.OffsiteStatusPendingReturn),
		Mode: mode, Actor: actor, RequestID: requestID,
	})
	due := dueAt
	return ItemResult{Barcode: barcode, OK: true, DueAt: &due, RequestID: requestID}
}

// CompleteDueTransfers sweeps every PendingPickup/PendingReturn record
// owned by mode and completes whichever is due (DueAt <= now) -
// best-effort, log-and-continue on error, mirroring RotateOffsite's
// existing documented tradeoff (internal/library/offsite.go): a candidate
// an operator moved out from under this in the meantime is skipped, not
// fatal to the sweep. Any mode with due-at semantics calls this same
// helper from its own Tick; it isn't Scheduled-Pickup-specific.
func (e *Engine) CompleteDueTransfers(mode string, now time.Time) (completedPickups, completedReturns []*library.Volume) {
	pickups, returns := e.lib.OffsitePendingByMode(mode)
	for _, p := range pickups {
		if p.Offsite == nil || p.Offsite.DueAt.IsZero() || p.Offsite.DueAt.After(now) {
			continue
		}
		vol, err := e.lib.CompleteOffsitePickup(p.Barcode)
		if err != nil {
			e.lib.RecordEvent(library.Event{
				Code:    library.EventCodeOffsitePickupCompleteFailure,
				Message: fmt.Sprintf("completing offsite pickup of %q failed", p.Barcode),
				Detail:  map[string]string{"mode": mode, "volume": p.Barcode, "error": err.Error()},
			})
			continue
		}
		// RequestID comes from the pre-completion snapshot (p, from
		// OffsitePendingByMode) rather than vol (CompleteOffsitePickup's
		// return value) - CompleteOffsitePickup already clears it on the
		// live volume once the pending state resolves naturally.
		e.recordMovement(store.OffsiteMovement{Barcode: vol.Barcode, Direction: "sent_offsite", Status: string(library.OffsiteStatusVaulted), Mode: mode, RequestID: p.Offsite.RequestID})
		completedPickups = append(completedPickups, vol)
	}
	for _, r := range returns {
		if r.Offsite == nil || r.Offsite.DueAt.IsZero() || r.Offsite.DueAt.After(now) {
			continue
		}
		vol, err := e.lib.CompleteOffsiteReturn(r.Barcode)
		if err != nil {
			e.lib.RecordEvent(library.Event{
				Code:    library.EventCodeOffsiteReturnCompleteFailure,
				Message: fmt.Sprintf("completing offsite return of %q failed", r.Barcode),
				Detail:  map[string]string{"mode": mode, "volume": r.Barcode, "error": err.Error()},
			})
			continue
		}
		// Same reasoning as the pickup branch above: r is the
		// pre-completion snapshot, since CompleteOffsiteReturn nils the
		// live volume's whole Offsite record (RequestID included) once it
		// leaves the offsite collection.
		e.recordMovement(store.OffsiteMovement{Barcode: vol.Barcode, Direction: "returned_onsite", Mode: mode, RequestID: r.Offsite.RequestID})
		completedReturns = append(completedReturns, vol)
	}
	return completedPickups, completedReturns
}

// recordMovement takes the full store.OffsiteMovement struct directly
// (Time is always left zero - RecordOffsiteMovement defaults it to now)
// rather than a growing list of positional strings, now that it carries
// Actor and RequestID too on some call sites.
func (e *Engine) recordMovement(m store.OffsiteMovement) {
	if err := e.store.RecordOffsiteMovement(m); err != nil {
		e.lib.RecordEvent(library.Event{
			Code:    library.EventCodeSystemPersistFailure,
			Message: "failed to record offsite movement history",
			Detail:  map[string]string{"barcode": m.Barcode, "direction": m.Direction, "error": err.Error()},
		})
	}
}
