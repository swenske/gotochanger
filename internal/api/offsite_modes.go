package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/swenske/gotochanger/internal/library"
	"github.com/swenske/gotochanger/internal/offsite"
	"github.com/swenske/gotochanger/internal/store"
)

// requireOffsiteEngine writes a 503 and returns ok=false if no offsite
// engine has been wired in yet (see Server.SetOffsiteEngine) - matters at
// most briefly during daemon startup, since cmd/gotochangerd always
// constructs and sets one.
func (s *Server) requireOffsiteEngine(w http.ResponseWriter) (*offsite.Engine, bool) {
	s.mu.RLock()
	eng := s.offsiteEngine
	s.mu.RUnlock()
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("offsite engine is not available"))
		return nil, false
	}
	return eng, true
}

func (s *Server) activeOffsiteMode(eng *offsite.Engine) (offsite.OffsiteMode, error) {
	name, err := s.topology.GetOffsiteActiveMode()
	if err != nil {
		return nil, err
	}
	if name == "" {
		return nil, fmt.Errorf("no offsite mode is currently active")
	}
	mode, ok := eng.Registry().Get(name)
	if !ok {
		return nil, fmt.Errorf("active offsite mode %q is not registered", name)
	}
	return mode, nil
}

func (s *Server) knownMailboxIDs() (map[string]bool, error) {
	mbs, err := s.topology.ListMailboxes()
	if err != nil {
		return nil, err
	}
	known := make(map[string]bool, len(mbs))
	for _, m := range mbs {
		known[m.ID] = true
	}
	return known, nil
}

// validateOffsiteMailboxReferences checks every ParameterTypeMailboxes
// field in mode's schema against live topology - the one piece of
// existence validation the generic API layer does on a mode's behalf
// (mode.ValidateConfig only checks structure, e.g. "non-empty"), mirroring
// ValidateLogicalLibrary's own "structural check in config, existence
// check at the API layer" split for a list of IDs. Reads field.Key
// dynamically rather than assuming any particular JSON key name, so it
// stays correct for any future mode with a mailboxes field.
func (s *Server) validateOffsiteMailboxReferences(mode offsite.OffsiteMode, raw json.RawMessage) error {
	var fields map[string]json.RawMessage
	fieldsUnmarshaled := false
	var known map[string]bool

	for _, f := range mode.ParameterSchema() {
		if f.Type != offsite.ParameterTypeMailboxes {
			continue
		}
		if !fieldsUnmarshaled {
			if err := json.Unmarshal(raw, &fields); err != nil {
				return nil // malformed JSON is already caught by mode.ValidateConfig
			}
			fieldsUnmarshaled = true
		}
		var ids []string
		if err := json.Unmarshal(fields[f.Key], &ids); err != nil {
			continue
		}
		if known == nil {
			var err error
			if known, err = s.knownMailboxIDs(); err != nil {
				return err
			}
		}
		for _, id := range ids {
			if !known[id] {
				return fmt.Errorf("unknown mailbox %q in %s", id, f.Key)
			}
		}
	}
	return nil
}

type offsiteModeInfo struct {
	Name     string                   `json:"name"`
	Label    string                   `json:"label"`
	Describe string                   `json:"describe"`
	Schema   []offsite.ParameterField `json:"schema"`
	Default  json.RawMessage          `json:"default_config"`
	Config   json.RawMessage          `json:"config,omitempty"`
}

type offsiteModesResponse struct {
	Modes  []offsiteModeInfo `json:"modes"`
	Active string            `json:"active"`
}

func (s *Server) handleListOffsiteModes(w http.ResponseWriter, r *http.Request) {
	eng, ok := s.requireOffsiteEngine(w)
	if !ok {
		return
	}
	active, err := s.topology.GetOffsiteActiveMode()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	modes := make([]offsiteModeInfo, 0, len(eng.Registry().All()))
	for _, m := range eng.Registry().All() {
		info := offsiteModeInfo{
			Name:     m.Name(),
			Label:    m.Label(),
			Describe: m.Describe(),
			Schema:   m.ParameterSchema(),
			Default:  m.DefaultConfig(),
		}
		if cfg, ok, err := s.topology.GetOffsiteModeConfig(m.Name()); err == nil && ok {
			info.Config = cfg
		}
		modes = append(modes, info)
	}
	writeJSON(w, http.StatusOK, offsiteModesResponse{Modes: modes, Active: active})
}

func (s *Server) handleUpdateOffsiteModeConfig(w http.ResponseWriter, r *http.Request) {
	eng, ok := s.requireOffsiteEngine(w)
	if !ok {
		return
	}
	name := r.PathValue("name")
	mode, ok := eng.Registry().Get(name)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("offsite mode %q not found", name))
		return
	}
	var raw json.RawMessage
	if err := decodeJSON(r, &raw); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := mode.ValidateConfig(raw); err != nil {
		s.emitFailure(r, library.EventCodeOffsiteModeConfigUpdateFailure, "failed to update offsite mode config", err, map[string]string{"mode": name})
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.validateOffsiteMailboxReferences(mode, raw); err != nil {
		s.emitFailure(r, library.EventCodeOffsiteModeConfigUpdateFailure, "failed to update offsite mode config", err, map[string]string{"mode": name})
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.topology.SetOffsiteModeConfig(name, raw); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.emitEvent(r, library.Event{
		Code:    library.EventCodeOffsiteModeConfigUpdateSuccess,
		Message: fmt.Sprintf("updated offsite mode %q config", name),
		Detail:  map[string]string{"mode": name},
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type offsiteActiveModeRequest struct {
	Mode string `json:"mode"`
}

func (s *Server) handleSetOffsiteActiveMode(w http.ResponseWriter, r *http.Request) {
	eng, ok := s.requireOffsiteEngine(w)
	if !ok {
		return
	}
	var req offsiteActiveModeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Mode != "" {
		if _, ok := eng.Registry().Get(req.Mode); !ok {
			writeError(w, http.StatusBadRequest, fmt.Errorf("offsite mode %q not found", req.Mode))
			return
		}
	}
	if err := s.topology.SetOffsiteActiveMode(req.Mode); err != nil {
		s.emitFailure(r, library.EventCodeOffsiteModeConfigUpdateFailure, "failed to set active offsite mode", err, map[string]string{"mode": req.Mode})
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.emitEvent(r, library.Event{
		Code:    library.EventCodeOffsiteModeConfigUpdateSuccess,
		Message: fmt.Sprintf("set active offsite mode to %q", req.Mode),
		Detail:  map[string]string{"mode": req.Mode},
	})
	writeJSON(w, http.StatusOK, map[string]string{"active": req.Mode})
}

type offsiteRequestPayload struct {
	Pickup       []string `json:"pickup,omitempty"`
	Return       []string `json:"return,omitempty"`
	ReturnUrgent []string `json:"return_urgent,omitempty"`
}

type offsiteRequestResult struct {
	Pickup       []offsite.ItemResult `json:"pickup"`
	Return       []offsite.ItemResult `json:"return"`
	ReturnUrgent []offsite.ItemResult `json:"return_urgent"`
	// The *RequestID fields surface the new shared batch id(s) this call
	// minted, so a caller learns them immediately without a follow-up GET
	// /api/v1/offsite/requests - each is the first non-empty
	// ItemResult.RequestID in its own group (every OK item within one
	// group shares the same id; empty if every barcode in that group
	// failed, since nothing was actually created to reference).
	PickupRequestID       string `json:"pickup_request_id,omitempty"`
	ReturnRequestID       string `json:"return_request_id,omitempty"`
	ReturnUrgentRequestID string `json:"return_urgent_request_id,omitempty"`
}

// firstRequestID returns the shared batch id carried by results, or "" if
// every item failed - see offsiteRequestResult's own doc comment.
func firstRequestID(results []offsite.ItemResult) string {
	for _, r := range results {
		if r.RequestID != "" {
			return r.RequestID
		}
	}
	return ""
}

// handleOffsiteRequest is best-effort/partial-success (HTTP 200 with
// per-barcode ItemResults even if some fail) rather than all-or-nothing:
// this endpoint only registers future intent for independent barcodes, so
// one bad barcode has no bearing on the others' validity - matching
// RotateOffsite's existing best-effort philosophy, applied to a
// synchronous batch instead of a background sweep.
func (s *Server) handleOffsiteRequest(w http.ResponseWriter, r *http.Request) {
	eng, ok := s.requireOffsiteEngine(w)
	if !ok {
		return
	}
	var req offsiteRequestPayload
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	mode, err := s.activeOffsiteMode(eng)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	actor := ""
	if p, ok := principalFrom(r); ok {
		actor = p.Subject
	}
	result := offsiteRequestResult{Pickup: []offsite.ItemResult{}, Return: []offsite.ItemResult{}, ReturnUrgent: []offsite.ItemResult{}}
	if len(req.Pickup) > 0 {
		result.Pickup = mode.RequestPickup(r.Context(), eng, req.Pickup, actor)
		result.PickupRequestID = firstRequestID(result.Pickup)
	}
	if len(req.Return) > 0 {
		result.Return = mode.RequestReturn(r.Context(), eng, req.Return, false, actor)
		result.ReturnRequestID = firstRequestID(result.Return)
	}
	if len(req.ReturnUrgent) > 0 {
		result.ReturnUrgent = mode.RequestReturn(r.Context(), eng, req.ReturnUrgent, true, actor)
		result.ReturnUrgentRequestID = firstRequestID(result.ReturnUrgent)
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleListTrackedOffsiteVolumes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.lib.TrackedOffsiteVolumes())
}

func (s *Server) handleOffsiteHistory(w http.ResponseWriter, r *http.Request) {
	barcode := r.URL.Query().Get("barcode")
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}
	movements, err := s.topology.ListOffsiteMovements(barcode, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, movements)
}

// offsiteRequestView is one batch request as exposed by GET
// /api/v1/offsite/requests - the stored config.OffsiteRequest snapshot
// plus a live-computed Status, so the UI/CLI never needs to separately
// cross-reference GET /api/v1/offsite/tracked to know whether a request
// is still cancelable.
type offsiteRequestView struct {
	ID          string     `json:"id"`
	Kind        string     `json:"kind"`
	Mode        string     `json:"mode"`
	Barcodes    []string   `json:"barcodes"`
	Urgent      bool       `json:"urgent,omitempty"`
	Status      string     `json:"status"` // "pending" | "completed" | "cancelled"
	CreatedAt   time.Time  `json:"created_at"`
	Actor       string     `json:"actor,omitempty"`
	CancelledAt *time.Time `json:"cancelled_at,omitempty"`
}

// handleListOffsiteRequests lists recent batch requests, newest first,
// each annotated with a Status derived live from Library state
// (OffsitePendingRequestIDs) rather than a parallel status column stored
// on the request row itself - see internal/offsite.Engine's own doc
// comments on why "still pending" can't safely be cached statically (a
// batch can complete or be cancelled member-by-member).
func (s *Server) handleListOffsiteRequests(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}
	reqs, err := s.topology.ListOffsiteRequests(limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	pending := s.lib.OffsitePendingRequestIDs()
	views := make([]offsiteRequestView, 0, len(reqs))
	for _, req := range reqs {
		status := "completed"
		if req.CancelledAt != nil {
			status = "cancelled"
		} else if pending[req.ID] {
			status = "pending"
		}
		views = append(views, offsiteRequestView{
			ID: req.ID, Kind: req.Kind, Mode: req.Mode, Barcodes: req.Barcodes, Urgent: req.Urgent,
			Status: status, CreatedAt: req.CreatedAt, Actor: req.Actor, CancelledAt: req.CancelledAt,
		})
	}
	writeJSON(w, http.StatusOK, views)
}

// handleCancelOffsiteRequest cancels a batch request by id, reverting
// whichever of its member barcodes are still pending (see
// internal/offsite.Engine.CancelOffsiteRequest for the partial-cancel
// contract this can hit).
func (s *Server) handleCancelOffsiteRequest(w http.ResponseWriter, r *http.Request) {
	eng, ok := s.requireOffsiteEngine(w)
	if !ok {
		return
	}
	id := r.PathValue("id")
	actor := ""
	if p, ok := principalFrom(r); ok {
		actor = p.Subject
	}
	cancelled, err := eng.CancelOffsiteRequest(id, actor)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cancelled": cancelled})
}

func (s *Server) handleOffsiteRecapPreview(w http.ResponseWriter, r *http.Request) {
	eng, ok := s.requireOffsiteEngine(w)
	if !ok {
		return
	}
	mode, err := s.activeOffsiteMode(eng)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	sp, ok := mode.(*offsite.ScheduledPickupMode)
	if !ok {
		writeError(w, http.StatusConflict, fmt.Errorf("recap preview is only available for the scheduled_pickup mode"))
		return
	}
	msg, err := sp.RecapPreview(eng, time.Now())
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, msg)
}

// recordManualOffsiteMovement writes one audit-trail row for a manual
// (mode="manual") offsite send/recall - used by handleOffsiteSend/
// handleOffsiteRecall in handlers.go, which predate the mode architecture
// and call *library.Library directly rather than through
// Engine.SendNow/RequestPickupOne.
func (s *Server) recordManualOffsiteMovement(r *http.Request, barcode, direction, status string) {
	if s.topology == nil {
		return
	}
	actor := ""
	if p, ok := principalFrom(r); ok {
		actor = p.Subject
	}
	if err := s.topology.RecordOffsiteMovement(store.OffsiteMovement{
		Barcode:   barcode,
		Direction: direction,
		Status:    status,
		Mode:      "manual",
		Actor:     actor,
	}); err != nil {
		s.log.Error("failed to record offsite movement history", "barcode", barcode, "error", err)
	}
}
