package api

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/swenske/gotochanger/internal/config"
	"github.com/swenske/gotochanger/internal/library"
	"github.com/swenske/gotochanger/internal/telemetry"
)

// WizardState represents the current state of the setup wizard.
type WizardState struct {
	Completed        bool                          `json:"completed"`
	CurrentStep      int                           `json:"current_step"`
	VTLName          string                        `json:"vtl_name,omitempty"`
	OperationalMode  string                        `json:"operational_mode,omitempty"`
	Drives           []config.DriveType            `json:"drives,omitempty"`
	Magazines        []config.MagazineConfig       `json:"magazines,omitempty"`
	Mailboxes        []config.MailboxConfig        `json:"mailboxes,omitempty"`
	OffsiteLocation  bool                          `json:"offsite_location,omitempty"`
	TapeSets         []WizardTapeSetRequest        `json:"tape_sets,omitempty"`
	LogicalLibraries []config.LogicalLibraryConfig `json:"logical_libraries,omitempty"`
	LatencyEnabled   bool                          `json:"latency_enabled,omitempty"`
	TelemetryEnabled bool                          `json:"telemetry_enabled,omitempty"`
}

// WizardTapeSetRequest is one step-6 tape set entry: the persisted
// TapeSetConfig fields plus a one-time TapeCount directive (how many
// cartridges to auto-generate on wizard completion). TapeCount isn't part
// of config.TapeSetConfig itself since it's an action, not a persistent
// property of the tape set.
type WizardTapeSetRequest struct {
	Name          string `json:"name"`
	TapeType      string `json:"tape_type"`
	StorageFolder string `json:"storage_folder"`
	TapeCount     int    `json:"tape_count,omitempty"`
}

// WizardRequest represents a request to update the wizard state.
type WizardRequest struct {
	Step             int                           `json:"step"`
	VTLName          string                        `json:"vtl_name,omitempty"`
	OperationalMode  string                        `json:"operational_mode,omitempty"`
	Drives           []config.DriveType            `json:"drives,omitempty"`
	Magazines        []config.MagazineConfig       `json:"magazines,omitempty"`
	Mailboxes        []config.MailboxConfig        `json:"mailboxes,omitempty"`
	OffsiteLocation  bool                          `json:"offsite_location,omitempty"`
	TapeSets         []WizardTapeSetRequest        `json:"tape_sets,omitempty"`
	LogicalLibraries []config.LogicalLibraryConfig `json:"logical_libraries,omitempty"`
	LatencyEnabled   bool                          `json:"latency_enabled,omitempty"`
	TelemetryEnabled bool                          `json:"telemetry_enabled,omitempty"`
}

// WizardResponse represents the response from the wizard API: the current
// WizardState plus the catalogs (drive types, tape types) the UI needs to
// render its pickers. Latency simulation no longer has a catalog to
// surface here - step 8 is just an enable/disable checkbox now; the
// actual delay values live in Admin > Latency (internal/api/latency.go).
type WizardResponse struct {
	WizardState
	DriveTypes []config.DriveType `json:"drive_types,omitempty"`
	TapeTypes  []config.TapeType  `json:"tape_types,omitempty"`
	// KernelMode backs step 1's kernel-mode radio button gating - see
	// currentKernelModeStatus (kernel_mode.go). Not omitempty: the wizard
	// UI always wants this field present, even when both underlying
	// checks are false (the zero value).
	KernelMode KernelModeStatus `json:"kernel_mode"`
	// TelemetryPreview backs step 9's "here's exactly what would be sent"
	// display - built by the same buildTelemetryPayload (telemetry.go)
	// the actual sender uses, so the preview can never drift from
	// reality. By step 9 almost all topology is already configured
	// (steps 2-7 run first), so this is a real, accurate snapshot, not a
	// mock.
	TelemetryPreview  telemetry.Payload `json:"telemetry_preview"`
	TelemetryEndpoint string            `json:"telemetry_endpoint"`
}

// loadWizardState reads wizard progress from the topology store. Every
// step's actual data already lives in its own relational table the moment
// it's submitted (see UpdateWizardState) - this only needs to recover which
// step we're on and whether the wizard has been completed, both of which
// are otherwise lost on every restart (the original bug this closes).
func loadWizardState(topology TopologyStore) WizardState {
	var ws WizardState
	if v, ok, _ := topology.GetSetting("wizard_current_step"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			ws.CurrentStep = n
		}
	}
	if v, ok, _ := topology.GetSetting("wizard_completed"); ok {
		ws.Completed = v == "true"
	}
	return ws
}

func saveWizardProgress(topology TopologyStore, ws WizardState) {
	if topology == nil {
		return
	}
	_ = topology.SetSetting("wizard_current_step", strconv.Itoa(ws.CurrentStep))
	completed := "false"
	if ws.Completed {
		completed = "true"
	}
	_ = topology.SetSetting("wizard_completed", completed)
}

// GetWizardState returns the current state of the setup wizard, with every
// step's data freshly read from the topology store (not just the in-memory
// current-step/completed flags) so a page reload mid-wizard shows exactly
// what was last saved.
func (s *Server) GetWizardState() WizardState {
	s.mu.RLock()
	ws := s.wizardState
	s.mu.RUnlock()
	return s.fillWizardStateFromTopology(ws)
}

// fillWizardStateFromTopology populates every per-step field of ws by
// reading the topology store (the fields on Server.wizardState itself only
// ever track CurrentStep/Completed - see UpdateWizardState). Does not lock
// s.mu; callers that aren't already holding it should use GetWizardState
// instead. Shared by GetWizardState and UpdateWizardState (which must
// return the same fully-populated shape its own POST response as GET does,
// otherwise the wizard UI can't redisplay a step's own submitted data,
// e.g. rendering the Logical Libraries step's drive/magazine/mailbox
// checkboxes from a State response that only ever had current_step set).
func (s *Server) fillWizardStateFromTopology(ws WizardState) WizardState {
	if s.topology == nil {
		return ws
	}
	ws.VTLName, _, _ = s.topology.GetSetting("vtl_name")
	ws.OperationalMode, _, _ = s.topology.GetSetting("operational_mode")
	ws.Drives = selectedDriveTypes(s.topology)
	ws.Magazines, _ = s.topology.ListMagazines()
	ws.Mailboxes, _ = s.topology.ListMailboxes()
	ws.OffsiteLocation, _ = offsiteSetting(s.topology)
	ws.TapeSets = wizardTapeSetsWithCounts(s.topology)
	ws.LogicalLibraries, _ = s.topology.ListLogicalLibraries()
	ws.LatencyEnabled, _ = latencyEnabledSetting(s.topology)
	ws.TelemetryEnabled = s.telemetryEnabled()
	return ws
}

// wizardTapeSetsWithCounts reattaches each tape set's pending TapeCount
// directive (persisted separately by saveWizardTapeCounts, since it's not
// part of config.TapeSetConfig) so the wizard state response round-trips
// step 6 data losslessly. Without this, any later request that resends a
// previous WizardState response's tape_sets - e.g. clicking "Previous" from
// step 7 - has every TapeCount reset to zero, and case 6 above rejects it as
// "at least one tape is required" even though it was already valid.
func wizardTapeSetsWithCounts(t TopologyStore) []WizardTapeSetRequest {
	tapeSets, err := t.ListTapeSets()
	if err != nil {
		return nil
	}
	counts := loadWizardTapeCounts(t)
	out := make([]WizardTapeSetRequest, len(tapeSets))
	for i, ts := range tapeSets {
		out[i] = WizardTapeSetRequest{
			Name:          ts.Name,
			TapeType:      ts.TapeType,
			StorageFolder: ts.StorageFolder,
			TapeCount:     counts[ts.Name],
		}
	}
	return out
}

// selectedDriveTypes reconstructs the drive types picked in wizard step 2,
// in submission order and preserving duplicates (multiple drives of the
// same type), from the comma-separated name list persisted alongside the
// resulting drive_devices (the physical drive count/paths alone don't say
// which catalog entries they came from). Order/duplicates matter here,
// unlike a plain set, since each "Add Drive" click adds one physical drive
// - two LTO-8 drives must redisplay as two rows, not one.
func selectedDriveTypes(t TopologyStore) []config.DriveType {
	raw, ok, err := t.GetSetting("selected_drive_types")
	if err != nil || !ok || raw == "" {
		return nil
	}
	all, err := t.ListDriveTypes()
	if err != nil {
		return nil
	}
	byName := make(map[string]config.DriveType, len(all))
	for _, dt := range all {
		byName[dt.Name] = dt
	}
	var out []config.DriveType
	for _, n := range strings.Split(raw, ",") {
		if dt, ok := byName[n]; ok {
			out = append(out, dt)
		}
	}
	return out
}

func offsiteSetting(t TopologyStore) (bool, error) {
	v, ok, err := t.GetSetting("offsite_location")
	if err != nil {
		return false, err
	}
	return ok && v == "true", nil
}

// latencyEnabledSetting reads wizard step 8's single enable/disable
// choice, mirroring offsiteSetting above. The actual delay values are
// never read/written by the wizard - they live in Admin > Latency
// (internal/api/latency.go), backed by the topology store's
// GetLatencySettings/SetLatencySettings.
func latencyEnabledSetting(t TopologyStore) (bool, error) {
	v, ok, err := t.GetSetting("latency_enabled")
	if err != nil {
		return false, err
	}
	return ok && v == "true", nil
}

// saveWizardTapeCounts/loadWizardTapeCounts persist the "create N tapes"
// directive from wizard step 6 as a small JSON singleton (tape-set name ->
// count), since it's a one-time creation action rather than a persistent
// property of the tape set (config.TapeSetConfig has no Count field, and
// the general Tape Sets CRUD API doesn't need one either). Consumed once by
// createPendingTapeSetVolumes at wizard completion, then cleared.
func saveWizardTapeCounts(t TopologyStore, counts map[string]int) error {
	if len(counts) == 0 {
		return t.SetSetting("wizard_tape_counts", "")
	}
	data, err := json.Marshal(counts)
	if err != nil {
		return err
	}
	return t.SetSetting("wizard_tape_counts", string(data))
}

func loadWizardTapeCounts(t TopologyStore) map[string]int {
	raw, ok, err := t.GetSetting("wizard_tape_counts")
	if err != nil || !ok || raw == "" {
		return nil
	}
	var counts map[string]int
	if err := json.Unmarshal([]byte(raw), &counts); err != nil {
		return nil
	}
	return counts
}

// createPendingTapeSetVolumes generates the cartridges requested for each
// tape set in wizard step 6 (map[name]count, persisted by
// saveWizardTapeCounts), barcoded per the tape set's tape type's format
// (internal/barcode), then clears the pending counts so a duplicate
// POST /api/v1/wizard/complete (e.g. a client retry) doesn't create a
// second batch.
func (s *Server) createPendingTapeSetVolumes() error {
	if s.topology == nil {
		return nil
	}
	counts := loadWizardTapeCounts(s.topology)
	if len(counts) == 0 {
		return nil
	}
	tapeSets, err := s.topology.ListTapeSets()
	if err != nil {
		return err
	}
	for _, ts := range tapeSets {
		count := counts[ts.Name]
		if count <= 0 {
			continue
		}
		if _, err := s.lib.CreateTapeSetCartridges(ts.Name, count); err != nil {
			return err
		}
	}
	return saveWizardTapeCounts(s.topology, nil)
}

// UpdateWizardState validates and immediately persists one wizard step's
// data to the topology store (not just an in-memory scratch state), so a
// daemon restart mid-wizard resumes exactly where it left off instead of
// losing everything entered so far.
func (s *Server) UpdateWizardState(req WizardRequest) (WizardState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if req.Step < 1 || req.Step > 9 {
		return s.wizardState, fmt.Errorf("invalid step: %d", req.Step)
	}
	if s.topology == nil {
		return s.wizardState, fmt.Errorf("wizard requires a topology store")
	}

	prevStep := s.wizardState.CurrentStep

	switch req.Step {
	case 1:
		if strings.TrimSpace(req.VTLName) == "" {
			return s.wizardState, fmt.Errorf("a virtual tape library name is required")
		}
		if req.OperationalMode != "changer" && req.OperationalMode != "kernel" {
			return s.wizardState, fmt.Errorf("invalid operational mode: %s", req.OperationalMode)
		}
		if err := s.topology.SetSetting("vtl_name", req.VTLName); err != nil {
			return s.wizardState, err
		}
		if err := s.topology.SetSetting("operational_mode", req.OperationalMode); err != nil {
			return s.wizardState, err
		}
	case 2:
		if len(req.Drives) < 1 {
			return s.wizardState, fmt.Errorf("at least one drive is required")
		}
		devices := make([]config.DriveDeviceConfig, len(req.Drives))
		names := make([]string, len(req.Drives))
		for i, dt := range req.Drives {
			// Linking DriveType here (rather than leaving it unset) means a
			// drive created via the wizard is already associated with its
			// selected catalog entry - Admin > Drives can show its
			// model/generation/capacity immediately, with no separate admin
			// action needed.
			devices[i] = config.DriveDeviceConfig{DevicePath: fmt.Sprintf("%s/drives/drive%d", s.cfg.DataDir, i), DriveType: dt.Name}
			names[i] = dt.Name
		}
		if err := s.topology.SaveDriveDevices(devices); err != nil {
			return s.wizardState, err
		}
		if err := s.topology.SetSetting("selected_drive_types", strings.Join(names, ",")); err != nil {
			return s.wizardState, err
		}
	case 3:
		if len(req.Magazines) < 1 {
			return s.wizardState, fmt.Errorf("at least one magazine is required")
		}
		for _, mag := range req.Magazines {
			if err := config.ValidateMagazine(mag); err != nil {
				return s.wizardState, err
			}
		}
		if err := s.checkMagazineResubmissionSafe(req.Magazines); err != nil {
			return s.wizardState, err
		}
		if err := s.topology.SaveMagazines(req.Magazines); err != nil {
			return s.wizardState, err
		}
	case 4:
		// Mailboxes are optional - a logical library doesn't need any I/O
		// slots unless the deployment actually uses import/export, so
		// zero mailboxes is a valid submission here (see
		// config.ValidateLogicalLibrary, which no longer requires one).
		for _, mb := range req.Mailboxes {
			if err := config.ValidateMailbox(mb); err != nil {
				return s.wizardState, err
			}
		}
		if err := s.checkMailboxResubmissionSafe(req.Mailboxes); err != nil {
			return s.wizardState, err
		}
		if err := s.topology.SaveMailboxes(req.Mailboxes); err != nil {
			return s.wizardState, err
		}
	case 5:
		v := "false"
		if req.OffsiteLocation {
			v = "true"
		}
		if err := s.topology.SetSetting("offsite_location", v); err != nil {
			return s.wizardState, err
		}
	case 6:
		if len(req.TapeSets) < 1 {
			return s.wizardState, fmt.Errorf("at least one tape set is required")
		}
		known, err := s.knownTapeTypeNames()
		if err != nil {
			return s.wizardState, err
		}
		tapeSets := make([]config.TapeSetConfig, len(req.TapeSets))
		tapeCounts := make(map[string]int, len(req.TapeSets))
		for i, ts := range req.TapeSets {
			cfg := config.TapeSetConfig{Name: ts.Name, TapeType: ts.TapeType, StorageFolder: ts.StorageFolder}
			if err := config.ValidateTapeSet(cfg, known); err != nil {
				return s.wizardState, err
			}
			if err := validateTapeSetFolder(cfg.StorageFolder); err != nil {
				return s.wizardState, err
			}
			if ts.TapeCount < 1 {
				return s.wizardState, fmt.Errorf("tape set %s: at least one tape is required", ts.Name)
			}
			tapeSets[i] = cfg
			tapeCounts[ts.Name] = ts.TapeCount
		}
		if err := s.topology.SaveTapeSets(tapeSets); err != nil {
			return s.wizardState, err
		}
		if err := saveWizardTapeCounts(s.topology, tapeCounts); err != nil {
			return s.wizardState, err
		}
	case 7:
		if len(req.LogicalLibraries) < 1 {
			return s.wizardState, fmt.Errorf("at least one logical library is required")
		}
		for _, lib := range req.LogicalLibraries {
			if err := config.ValidateLogicalLibrary(lib); err != nil {
				return s.wizardState, err
			}
		}
		if err := s.topology.SaveLogicalLibraries(req.LogicalLibraries); err != nil {
			return s.wizardState, err
		}
	case 8:
		// Only the enable/disable choice is set here - the actual delay
		// values are never wizard-editable, they always come from
		// whatever SeedDefaults/migrateLegacyLatencySetting already wrote
		// (config.DefaultLatencySettings() on a fresh install) until an
		// admin tunes them via Admin > Latency.
		v := "false"
		if req.LatencyEnabled {
			v = "true"
		}
		if err := s.topology.SetSetting("latency_enabled", v); err != nil {
			return s.wizardState, err
		}
	case 9:
		// Only the enable/disable choice is set here, mirroring step 8
		// (latency) above - see internal/api/telemetry.go for the exact
		// payload/endpoint this opt-in controls.
		v := "false"
		if req.TelemetryEnabled {
			v = "true"
		}
		if err := s.topology.SetSetting("telemetry_enabled", v); err != nil {
			return s.wizardState, err
		}
		if req.TelemetryEnabled {
			// Opt-in takes effect immediately rather than silently
			// waiting for the next daemon restart - see
			// sendTelemetryAsync's doc comment.
			s.sendTelemetryAsync()
		}
		s.wizardState.Completed = true
	}

	// A forward submission (the Next button, req.Step == the step whose
	// data was just validated above) advances to the following step.
	// Backward navigation (Previous) resends a lower step's already-valid
	// data so it re-validates cleanly here, but must land exactly on that
	// step rather than being advanced past it again.
	if req.Step >= prevStep && req.Step < 9 {
		s.wizardState.CurrentStep = req.Step + 1
	} else {
		s.wizardState.CurrentStep = req.Step
	}
	saveWizardProgress(s.topology, s.wizardState)

	return s.fillWizardStateFromTopology(s.wizardState), nil
}

// CompleteWizard applies the wizard's already-persisted configuration to the
// running library (hot-reload, no restart needed), generates any tape sets'
// initial cartridges, and marks the wizard permanently completed.
func (s *Server) CompleteWizard() error {
	s.mu.Lock()
	if !s.wizardState.Completed {
		s.mu.Unlock()
		return fmt.Errorf("wizard not completed")
	}
	s.mu.Unlock()

	if err := s.reconfigureFromStore(); err != nil {
		return err
	}
	if err := s.createPendingTapeSetVolumes(); err != nil {
		return err
	}
	s.ReconcileKernelModeInstancesAsync()
	return nil
}

// ResetWizard resets the wizard state, both in memory and in the topology
// store, so it will be presented again on next login. It does not delete
// already-saved topology (magazines/logical libraries/etc.) - use the Admin
// API/CLI for that.
func (s *Server) ResetWizard() {
	s.mu.Lock()
	s.wizardState = WizardState{}
	s.mu.Unlock()
	saveWizardProgress(s.topology, WizardState{})
}

// GetWizardOptions returns the catalogs the wizard UI needs (drive types,
// tape types), plus the current step's already-saved data (equivalent to
// GetWizardState) so the wizard can pre-fill its form when re-displaying
// a step.
func (s *Server) GetWizardOptions() WizardResponse {
	ws := s.GetWizardState()
	resp := WizardResponse{WizardState: ws, KernelMode: currentKernelModeStatus(), TelemetryPreview: s.buildTelemetryPayload(), TelemetryEndpoint: telemetryEndpoint}
	if s.topology != nil {
		resp.DriveTypes, _ = s.topology.ListDriveTypes()
		resp.TapeTypes, _ = s.topology.ListTapeTypes()
	}
	return resp
}

// checkMagazineResubmissionSafe refuses a wizard magazine-step (step 3)
// submission that would remove or shrink an already-existing,
// currently-occupied magazine - the same protection
// handleDeleteMagazine/handleUpdateMagazine give the Admin API. Needed
// here too: UpdateWizardState has no guard against being resubmitted
// after the wizard is already completed and the system is live with real
// volumes (SaveMagazines itself has no such guard either, by design - see
// its doc comment - since it also serves the pre-completion, nothing-live
// case where this check is always a no-op).
func (s *Server) checkMagazineResubmissionSafe(mags []config.MagazineConfig) error {
	existing, err := s.topology.ListMagazines()
	if err != nil {
		return err
	}
	newSlots := make(map[string]int, len(mags))
	for _, m := range mags {
		newSlots[m.ID] = m.Slots
	}
	st := s.lib.Status()
	for _, old := range existing {
		newCount, stillPresent := newSlots[old.ID]
		current := slotsInMagazine(st.Slots, old.ID)
		var removed []*library.Slot
		switch {
		case !stillPresent:
			removed = current
		case newCount < len(current):
			removed = current[newCount:]
		default:
			continue
		}
		for _, slot := range removed {
			if slot.Volume != nil {
				return fmt.Errorf("magazine %s: %w", old.ID, errMagazineNotEmpty)
			}
		}
		if driveOriginInSlots(st.Drives, removed) {
			return fmt.Errorf("magazine %s: %w", old.ID, errMagazineVolumeOnDrive)
		}
	}
	return nil
}

// checkMailboxResubmissionSafe mirrors checkMagazineResubmissionSafe for
// the wizard's mailbox step (step 4).
func (s *Server) checkMailboxResubmissionSafe(mbs []config.MailboxConfig) error {
	existing, err := s.topology.ListMailboxes()
	if err != nil {
		return err
	}
	newSlots := make(map[string]int, len(mbs))
	for _, m := range mbs {
		newSlots[m.ID] = m.Slots
	}
	st := s.lib.Status()
	for _, old := range existing {
		newCount, stillPresent := newSlots[old.ID]
		current := ioslotsInMailbox(st.IOSlots, old.ID)
		var removed []*library.IOSlot
		switch {
		case !stillPresent:
			removed = current
		case newCount < len(current):
			removed = current[newCount:]
		default:
			continue
		}
		for _, io := range removed {
			if io.Volume != nil {
				return fmt.Errorf("mailbox %s: %w", old.ID, errMailboxNotEmpty)
			}
		}
		if driveOriginInIOSlots(st.Drives, removed) {
			return fmt.Errorf("mailbox %s: %w", old.ID, errMailboxVolumeOnDrive)
		}
	}
	return nil
}
