package library

import (
	"fmt"
	"sort"
	"time"
)

// OffsiteVolumes returns a snapshot of tapes currently vaulted offsite.
func (l *Library) OffsiteVolumes() []*Volume {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return snapshotVolumes(l.offsite)
}

// OffsiteEnabled reports whether offsite vaulting is enabled at all - the
// one master switch every send/request path below is gated on.
func (l *Library) OffsiteEnabled() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.cfg.Library.OffsiteLocation
}

// OffsiteSend simulates sending a volume to offsite vault storage: it moves
// the volume from a storage slot to the (unlimited-capacity, like the
// outside-library inventory) offsite collection, freeing the slot.
func (l *Library) OffsiteSend(from ElementRef) (*Volume, error) {
	return l.offsiteSend(from, OffsiteMeta{Mode: "manual"})
}

// OffsiteSendMode is OffsiteSend's mode-attributed counterpart, called by
// internal/offsite's Engine so the resulting Volume.Offsite.Mode records
// which OffsiteMode initiated the send.
func (l *Library) OffsiteSendMode(from ElementRef, meta OffsiteMeta) (*Volume, error) {
	return l.offsiteSend(from, meta)
}

func (l *Library) offsiteSend(from ElementRef, meta OffsiteMeta) (*Volume, error) {
	if from.Kind == KindDrive {
		return nil, fmt.Errorf("%w: source cannot be a drive", ErrInvalidTarget)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.cfg.Library.OffsiteLocation {
		return nil, ErrOffsiteDisabled
	}

	getFrom, setFrom, fromLabel, err := l.volumeSlot(from)
	if err != nil {
		return nil, err
	}
	vol := getFrom()
	if vol == nil {
		return nil, fmt.Errorf("%s: %w", fromLabel, ErrEmpty)
	}
	setFrom(nil)
	origin := from
	vol.Offsite = &OffsiteInfo{Status: OffsiteStatusVaulted, Mode: meta.Mode, RequestedAt: time.Now().UTC(), Origin: &origin}
	l.offsite = append(l.offsite, vol)
	l.emit("offsite-send", fmt.Sprintf("sent volume %q offsite from %s", vol.Barcode, fromLabel),
		map[string]string{"volume": vol.Barcode, "from": fromLabel})
	l.saveLocked()
	return vol, nil
}

// OffsiteRecall simulates recalling a volume from offsite vault storage back
// into the library at the given slot or I/O slot.
func (l *Library) OffsiteRecall(label string, to ElementRef) error {
	return l.offsiteRecall(label, to, OffsiteMeta{Mode: "manual"})
}

// OffsiteRecallMode is OffsiteRecall's mode-attributed counterpart, called
// by internal/offsite's Engine.
func (l *Library) OffsiteRecallMode(label string, to ElementRef, meta OffsiteMeta) error {
	return l.offsiteRecall(label, to, meta)
}

func (l *Library) offsiteRecall(label string, to ElementRef, meta OffsiteMeta) error {
	if to.Kind == KindDrive {
		return fmt.Errorf("%w: destination cannot be a drive", ErrInvalidTarget)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.cfg.Library.OffsiteLocation {
		return ErrOffsiteDisabled
	}

	vol, idx := l.findOffsite(label)
	if vol == nil {
		return fmt.Errorf("offsite volume %q: %w", label, ErrNotFound)
	}
	getTo, setTo, toLabel, err := l.volumeSlot(to)
	if err != nil {
		return err
	}
	if getTo() != nil {
		return fmt.Errorf("%s: %w", toLabel, ErrFull)
	}
	l.offsite = append(l.offsite[:idx], l.offsite[idx+1:]...)
	vol.Offsite = nil
	setTo(vol)
	l.emit("offsite-recall", fmt.Sprintf("recalled volume %q from offsite into %s", vol.Barcode, toLabel),
		map[string]string{"volume": vol.Barcode, "to": toLabel})
	l.saveLocked()
	return nil
}

// findOffsite locates a volume in the offsite collection by barcode,
// alongside its index for splicing it out.
func (l *Library) findOffsite(label string) (*Volume, int) {
	for i, v := range l.offsite {
		if v != nil && v.Barcode == label {
			return v, i
		}
	}
	return nil, -1
}

// findOnsiteVolumeLocked locates barcode in a slot or ioslot (never a
// drive, l.outside, or l.offsite) without removing it - used to validate a
// pickup request/completion before mutating anything.
func (l *Library) findOnsiteVolumeLocked(barcode string) *Volume {
	for _, s := range l.slots {
		if s.Volume != nil && s.Volume.Barcode == barcode {
			return s.Volume
		}
	}
	for _, io := range l.ioslots {
		if io.Volume != nil && io.Volume.Barcode == barcode {
			return io.Volume
		}
	}
	return nil
}

// removeOnsiteByBarcodeLocked finds barcode in a slot or ioslot, clears
// that slot/ioslot, and returns the volume, the ElementRef and display
// label it was removed from. Used instead of volumeSlot wherever a
// barcode - not a caller-supplied ElementRef - is the only handle
// available (see CompleteOffsitePickup's own doc comment on why it must
// re-resolve by barcode rather than trust a possibly-stale ref).
func (l *Library) removeOnsiteByBarcodeLocked(barcode string) (vol *Volume, ref ElementRef, label string, ok bool) {
	for _, s := range l.slots {
		if s.Volume != nil && s.Volume.Barcode == barcode {
			vol = s.Volume
			ref = ElementRef{Kind: KindSlot, Address: s.Address}
			label = fmt.Sprintf("slot %s", displayLabel(s.Label, s.Address))
			s.Volume = nil
			return vol, ref, label, true
		}
	}
	for _, io := range l.ioslots {
		if io.Volume != nil && io.Volume.Barcode == barcode {
			vol = io.Volume
			ref = ElementRef{Kind: KindIOSlot, Address: io.Address}
			label = fmt.Sprintf("ioslot %s", displayLabel(io.Label, io.Address))
			io.Volume = nil
			return vol, ref, label, true
		}
	}
	return nil, ElementRef{}, "", false
}

// RequestOffsitePickup flags an onsite (slot/ioslot-resident) barcode as
// scheduled to leave at dueAt, without moving it - the volume stays fully
// usable and physically in place until CompleteOffsitePickup runs. Gated
// by the same OffsiteLocation flag OffsiteSend already uses. requestID
// (see OffsiteInfo.RequestID) is stamped directly here, not by a
// follow-up call, since the 30s offsite Tick sweep (CompleteDueTransfers)
// could otherwise complete this record in the window between "flagged
// Pending" and "ID stamped", silently dropping it from its batch.
func (l *Library) RequestOffsitePickup(barcode, mode string, dueAt time.Time, requestID string) (*Volume, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.cfg.Library.OffsiteLocation {
		return nil, ErrOffsiteDisabled
	}
	vol := l.findOnsiteVolumeLocked(barcode)
	if vol == nil {
		return nil, fmt.Errorf("volume %q: %w", barcode, ErrOffsiteNotOnsite)
	}
	if vol.Offsite != nil {
		return nil, fmt.Errorf("volume %q: %w", barcode, ErrOffsiteAlreadyTracked)
	}
	vol.Offsite = &OffsiteInfo{
		Status:      OffsiteStatusPendingPickup,
		Mode:        mode,
		RequestedAt: time.Now().UTC(),
		DueAt:       dueAt,
		RequestID:   requestID,
	}
	l.emit("offsite-pickup-request", fmt.Sprintf("flagged volume %q for offsite pickup", vol.Barcode),
		map[string]string{"volume": vol.Barcode, "mode": mode})
	l.saveLocked()
	return vol, nil
}

// RequestOffsiteReturn flags a currently-vaulted barcode as scheduled to
// return at dueAt. Only a volume in OffsiteStatusVaulted can transition to
// PendingReturn - a volume already PendingReturn is rejected with
// ErrOffsiteAlreadyTracked rather than silently overwriting an
// in-flight request. requestID is stamped directly here for the same
// race-window reason documented on RequestOffsitePickup.
func (l *Library) RequestOffsiteReturn(barcode, mode string, dueAt time.Time, urgent bool, requestID string) (*Volume, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.cfg.Library.OffsiteLocation {
		return nil, ErrOffsiteDisabled
	}
	vol, _ := l.findOffsite(barcode)
	if vol == nil {
		return nil, fmt.Errorf("volume %q: %w", barcode, ErrOffsiteNotVaulted)
	}
	if vol.Offsite == nil || vol.Offsite.Status != OffsiteStatusVaulted {
		return nil, fmt.Errorf("volume %q: %w", barcode, ErrOffsiteAlreadyTracked)
	}
	vol.Offsite.Status = OffsiteStatusPendingReturn
	vol.Offsite.Mode = mode
	vol.Offsite.RequestedAt = time.Now().UTC()
	vol.Offsite.DueAt = dueAt
	vol.Offsite.Urgent = urgent
	vol.Offsite.RequestID = requestID
	l.emit("offsite-return-request", fmt.Sprintf("flagged volume %q for offsite return", vol.Barcode),
		map[string]string{"volume": vol.Barcode, "mode": mode})
	l.saveLocked()
	return vol, nil
}

// CompleteOffsitePickup performs the actual physical send for a barcode
// previously flagged PendingPickup, once its DueAt has arrived - re-finds
// the volume by barcode (it may have been relocated by an operator since
// the request, e.g. moved to a different slot) rather than trusting the
// ElementRef captured when the pickup was requested. If it's no longer
// onsite in a slot/ioslot with a still-PendingPickup record, this fails;
// the caller (internal/offsite's Engine.CompleteDueTransfers) is expected
// to log and continue, mirroring RotateOffsite's existing best-effort
// contract for exactly this kind of race.
func (l *Library) CompleteOffsitePickup(barcode string) (*Volume, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	vol := l.findOnsiteVolumeLocked(barcode)
	if vol == nil || vol.Offsite == nil || vol.Offsite.Status != OffsiteStatusPendingPickup {
		return nil, fmt.Errorf("volume %q: %w", barcode, ErrOffsiteNotOnsite)
	}
	removed, ref, label, ok := l.removeOnsiteByBarcodeLocked(barcode)
	if !ok {
		return nil, fmt.Errorf("volume %q: %w", barcode, ErrOffsiteNotOnsite)
	}
	removed.Offsite.Status = OffsiteStatusVaulted
	removed.Offsite.RequestedAt = time.Now().UTC()
	removed.Offsite.DueAt = time.Time{}
	removed.Offsite.Origin = &ref
	removed.Offsite.RequestID = "" // pending state resolved naturally; nothing left to cancel
	l.offsite = append(l.offsite, removed)
	l.emit("offsite-pickup-complete", fmt.Sprintf("completed offsite pickup of volume %q from %s", removed.Barcode, label),
		map[string]string{"volume": removed.Barcode, "from": label})
	l.saveLocked()
	return removed, nil
}

// CompleteOffsiteReturn performs the actual return for a barcode
// previously flagged PendingReturn: clears its offsite record and places
// the volume into the outside-library inventory (not a specific slot),
// mirroring exactly how every other tape enters the library needing a
// manual door-load to get racked (a fresh cartridge, a mailbox import).
// Deliberately does not try to guess/reserve a destination slot: an
// automatic, unattended completion must never fail with ErrFull.
func (l *Library) CompleteOffsiteReturn(barcode string) (*Volume, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	vol, idx := l.findOffsite(barcode)
	if vol == nil || vol.Offsite == nil || vol.Offsite.Status != OffsiteStatusPendingReturn {
		return nil, fmt.Errorf("volume %q: %w", barcode, ErrOffsiteNotVaulted)
	}
	l.offsite = append(l.offsite[:idx], l.offsite[idx+1:]...)
	vol.Offsite = nil
	l.outside = append(l.outside, vol)
	l.emit("offsite-return-complete", fmt.Sprintf("completed offsite return of volume %q", vol.Barcode),
		map[string]string{"volume": vol.Barcode})
	l.saveLocked()
	return vol, nil
}

// CancelledOffsiteItem is one barcode CancelOffsiteRequest actually
// reverted, and which kind of pending state it was in - lets
// internal/offsite's Engine pick the right movement direction/event code
// per item without a second lookup.
type CancelledOffsiteItem struct {
	Barcode string `json:"barcode"`
	Kind    string `json:"kind"` // "pickup" | "return" - which Pending* state was reverted
}

// CancelOffsiteRequest reverts every volume still carrying requestID in a
// Pending* state: a still-onsite PendingPickup record is cleared entirely
// (Offsite = nil, as if never requested - the volume was never moved, so
// there's nothing else to undo); a PendingReturn record reverts to plain
// OffsiteStatusVaulted (clearing DueAt/Urgent/RequestID), since the
// volume is still physically away regardless.
//
// One l.mu.Lock() for the whole scan-and-revert (never opMu - this
// touches only Volume.Offsite, the same class of write
// RequestOffsitePickup/offsiteSend/offsiteRecall already make under mu
// alone, never topology or arm state). This is also what makes a
// concurrent cancel-vs-complete race safe by construction: whichever of
// this method or CompleteOffsitePickup/CompleteOffsiteReturn acquires
// the lock first simply wins - the loser's own pre-existing guard
// (Offsite == nil, or Offsite.Status no longer what it expects) already
// makes it a no-op, which CompleteDueTransfers already treats as
// log-and-continue, its existing contract for a candidate that moved out
// from under a sweep.
//
// Returns a plain (possibly empty) slice, never an error: a request that
// matches nothing here might be fully unknown, or might simply have
// already resolved - only the caller (which sees the offsite_requests
// row) can tell those apart. A non-empty result can legitimately be
// smaller than the request's original full membership, if some barcodes
// already completed independently before this ran - the same race as
// above, just observed from the "some already gone" side instead of the
// "cancel arrived first" side.
func (l *Library) CancelOffsiteRequest(requestID string) []CancelledOffsiteItem {
	if requestID == "" {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	var out []CancelledOffsiteItem
	for _, s := range l.slots {
		if s.Volume != nil && s.Volume.Offsite != nil && s.Volume.Offsite.Status == OffsiteStatusPendingPickup && s.Volume.Offsite.RequestID == requestID {
			out = append(out, CancelledOffsiteItem{Barcode: s.Volume.Barcode, Kind: "pickup"})
			l.emit("offsite-pickup-cancel", fmt.Sprintf("cancelled offsite pickup request for volume %q", s.Volume.Barcode),
				map[string]string{"volume": s.Volume.Barcode, "mode": s.Volume.Offsite.Mode})
			s.Volume.Offsite = nil
		}
	}
	for _, io := range l.ioslots {
		if io.Volume != nil && io.Volume.Offsite != nil && io.Volume.Offsite.Status == OffsiteStatusPendingPickup && io.Volume.Offsite.RequestID == requestID {
			out = append(out, CancelledOffsiteItem{Barcode: io.Volume.Barcode, Kind: "pickup"})
			l.emit("offsite-pickup-cancel", fmt.Sprintf("cancelled offsite pickup request for volume %q", io.Volume.Barcode),
				map[string]string{"volume": io.Volume.Barcode, "mode": io.Volume.Offsite.Mode})
			io.Volume.Offsite = nil
		}
	}
	for _, v := range l.offsite {
		if v != nil && v.Offsite != nil && v.Offsite.Status == OffsiteStatusPendingReturn && v.Offsite.RequestID == requestID {
			out = append(out, CancelledOffsiteItem{Barcode: v.Barcode, Kind: "return"})
			l.emit("offsite-return-cancel", fmt.Sprintf("cancelled offsite return request for volume %q", v.Barcode),
				map[string]string{"volume": v.Barcode, "mode": v.Offsite.Mode})
			v.Offsite.Status = OffsiteStatusVaulted
			v.Offsite.DueAt = time.Time{}
			v.Offsite.Urgent = false
			v.Offsite.RequestID = ""
		}
	}
	if len(out) > 0 {
		l.saveLocked()
	}
	return out
}

// BackfillOffsiteInfo sets a minimal Offsite record on a vaulted volume
// that doesn't have one yet - only ever true for a volume vaulted by a
// pre-mode-architecture binary (Volume.Offsite didn't exist then), never
// for one sent through offsiteSend/CompleteOffsitePickup, both of which
// always set it. Called once at startup by cmd/gotochangerd for every
// OffsiteVolumes() entry with a nil Offsite; a no-op if the volume already
// has a record or is no longer vaulted by the time this runs.
func (l *Library) BackfillOffsiteInfo(barcode string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	vol, _ := l.findOffsite(barcode)
	if vol == nil || vol.Offsite != nil {
		return
	}
	vol.Offsite = &OffsiteInfo{Status: OffsiteStatusVaulted, Mode: "unknown", RequestedAt: time.Now().UTC()}
	l.saveLocked()
}

// OffsitePendingByMode returns deep-copied snapshots of every volume
// currently PendingPickup (still onsite, in slots/ioslots) or
// PendingReturn (still vaulted, in l.offsite) whose Offsite.Mode matches
// mode - used by Engine.CompleteDueTransfers and the recap email to see
// what's outstanding for one specific mode.
func (l *Library) OffsitePendingByMode(mode string) (pickups, returns []*Volume) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, s := range l.slots {
		if s.Volume != nil && s.Volume.Offsite != nil && s.Volume.Offsite.Status == OffsiteStatusPendingPickup && s.Volume.Offsite.Mode == mode {
			pickups = append(pickups, snapshotVolume(s.Volume))
		}
	}
	for _, io := range l.ioslots {
		if io.Volume != nil && io.Volume.Offsite != nil && io.Volume.Offsite.Status == OffsiteStatusPendingPickup && io.Volume.Offsite.Mode == mode {
			pickups = append(pickups, snapshotVolume(io.Volume))
		}
	}
	for _, v := range l.offsite {
		if v != nil && v.Offsite != nil && v.Offsite.Status == OffsiteStatusPendingReturn && v.Offsite.Mode == mode {
			returns = append(returns, snapshotVolume(v))
		}
	}
	return pickups, returns
}

// OffsitePendingRequestIDs returns the set of RequestIDs currently
// attached to a Pending* volume, across every mode - used to compute a
// request's live status (pending vs. resolved) without a parallel status
// state machine in the offsite_requests table (see internal/api's
// handleListOffsiteRequests). Mirrors OffsitePendingByMode's exact scan
// shape, but collects RequestIDs instead of volumes and isn't scoped to
// one mode.
func (l *Library) OffsitePendingRequestIDs() map[string]bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	ids := map[string]bool{}
	for _, s := range l.slots {
		if s.Volume != nil && s.Volume.Offsite != nil && s.Volume.Offsite.Status == OffsiteStatusPendingPickup && s.Volume.Offsite.RequestID != "" {
			ids[s.Volume.Offsite.RequestID] = true
		}
	}
	for _, io := range l.ioslots {
		if io.Volume != nil && io.Volume.Offsite != nil && io.Volume.Offsite.Status == OffsiteStatusPendingPickup && io.Volume.Offsite.RequestID != "" {
			ids[io.Volume.Offsite.RequestID] = true
		}
	}
	for _, v := range l.offsite {
		if v != nil && v.Offsite != nil && v.Offsite.Status == OffsiteStatusPendingReturn && v.Offsite.RequestID != "" {
			ids[v.Offsite.RequestID] = true
		}
	}
	return ids
}

// TrackedOffsiteVolumes returns a deep-copied snapshot of every volume
// currently carrying a non-nil Offsite record, regardless of status -
// backs GET /api/v1/offsite/tracked.
func (l *Library) TrackedOffsiteVolumes() []*Volume {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var out []*Volume
	for _, s := range l.slots {
		if s.Volume != nil && s.Volume.Offsite != nil {
			out = append(out, snapshotVolume(s.Volume))
		}
	}
	for _, io := range l.ioslots {
		if io.Volume != nil && io.Volume.Offsite != nil {
			out = append(out, snapshotVolume(io.Volume))
		}
	}
	for _, v := range l.offsite {
		if v != nil && v.Offsite != nil {
			out = append(out, snapshotVolume(v))
		}
	}
	return out
}

// OffsiteRotationCandidates returns up to count ElementRefs for the
// least-recently-created Full volumes currently sitting in storage slots -
// the exact selection RotateOffsite has always used, extracted so
// internal/offsite's ContinuousRotationMode can share it instead of
// reimplementing the same selection logic. A pure read: callers decide
// whether/how to gate on OffsiteEnabled() before acting on the result.
func (l *Library) OffsiteRotationCandidates(count int) []ElementRef {
	l.mu.RLock()
	defer l.mu.RUnlock()
	type candidate struct {
		ref ElementRef
		vol *Volume
	}
	var candidates []candidate
	for _, s := range l.slots {
		if s.Volume != nil && s.Volume.Full {
			candidates = append(candidates, candidate{ElementRef{Kind: KindSlot, Address: s.Address}, s.Volume})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].vol.CreatedAt.Before(candidates[j].vol.CreatedAt)
	})
	if len(candidates) > count {
		candidates = candidates[:count]
	}
	refs := make([]ElementRef, len(candidates))
	for i, c := range candidates {
		refs[i] = c.ref
	}
	return refs
}

// RotateOffsite is run periodically (when offsite rotation is enabled) to
// simulate scheduled tape rotation: it sends up to count of the
// least-recently-created full volumes currently in storage slots offsite.
// Kept as a direct entry point (used by gotochangerctl and existing tests)
// alongside internal/offsite's ContinuousRotationMode, which shares the
// same OffsiteRotationCandidates selection but drives its own
// interval/last-run bookkeeping instead of taking count as a parameter.
func (l *Library) RotateOffsite(count int) {
	if count <= 0 || !l.OffsiteEnabled() {
		return
	}
	for _, ref := range l.OffsiteRotationCandidates(count) {
		if _, err := l.OffsiteSend(ref); err != nil {
			l.mu.Lock()
			l.emit("offsite-rotation-error", fmt.Sprintf("scheduled offsite rotation failed for slot %d", ref.Address),
				map[string]string{"error": err.Error()})
			l.mu.Unlock()
		}
	}
}
