package api

import (
	"fmt"
	"net/http"
	"os"

	"github.com/swenske/gotochanger/internal/library"
)

// resetFallbackConfirmName is what the operator must type to confirm a
// reset when the VTL has no name yet (e.g. the wizard was never
// completed) - there's no real VTL name to ask for in that case.
const resetFallbackConfirmName = "RESET"

// ResetRequest is the body of POST /api/v1/reset.
type ResetRequest struct {
	// ConfirmName must equal the VTL's current name (or
	// resetFallbackConfirmName if it has none yet) - the confirmation
	// gate the user types into, both from the Admin UI and gotochangerctl.
	ConfirmName string `json:"confirm_name"`
	// DeleteVolumes, if true, also permanently deletes every cartridge's
	// backing file on disk (slots, ioslots, drives, outside, offsite),
	// not just the database records referencing them.
	DeleteVolumes bool `json:"delete_volumes"`
}

// handleReset wipes the entire VTL - topology, settings, dynamic state,
// user accounts and API tokens - back to exactly what a brand new install
// looks like (see internal/store.Store.ResetToFactory), gated on the
// operator typing the VTL's current name to confirm. Like restore, it does
// not attempt to hot-reload the running Library/Server: a successful reset
// triggers a deliberate process restart so the daemon comes back up
// against the freshly emptied database, at which point SeedDefaults()
// repopulates every factory default exactly as it would for a genuinely
// new install.
func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	if s.backup == nil {
		s.emitFailure(r, library.EventCodeConfigResetFailure, "reset unavailable", errBackupUnavailable, nil)
		writeError(w, http.StatusServiceUnavailable, errBackupUnavailable)
		return
	}

	var req ResetRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	// Read the VTL name fresh from the topology store, not
	// s.settings.Current() - that cache is known to go stale after a
	// setting is written directly to the store (the wizard does this for
	// vtl_name, same as it does for latency_enabled - see
	// Settings.CurrentLatency's doc comment for the same class of bug).
	expected, _, _ := s.topology.GetSetting("vtl_name")
	if expected == "" {
		expected = resetFallbackConfirmName
	}
	if req.ConfirmName != expected {
		err := fmt.Errorf("confirmation name does not match")
		s.emitFailure(r, library.EventCodeConfigResetFailure, "reset rejected: confirmation name mismatch", err, nil)
		writeError(w, http.StatusBadRequest, err)
		return
	}

	// Safety net: snapshot the live database into the normal backups
	// directory before doing anything irreversible, the same primitive
	// handleBackupDownload uses - so a mistaken reset is recoverable via
	// Admin > Backup's restore flow. Fail closed: don't wipe anything if
	// this snapshot can't be taken.
	if _, err := s.backup.CreateBackupFile(s.backupsDir, expected); err != nil {
		s.emitFailure(r, library.EventCodeConfigResetFailure, "failed to snapshot before reset", err, nil)
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	if req.DeleteVolumes {
		for _, v := range s.lib.AllVolumes() {
			_ = os.Remove(v.Path)
		}
	}

	if err := s.backup.ResetToFactory(); err != nil {
		s.emitFailure(r, library.EventCodeConfigResetFailure, "reset failed", err, nil)
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// Persist one audit event directly into the freshly emptied database
	// via s.persist - deliberately not s.emitEvent/s.lib.RecordEvent,
	// whose saveLocked() would re-persist the *entire* still-in-memory
	// (pre-reset) Library state - slots, drives, outside volumes, the old
	// event log - right back into the database we just emptied, undoing
	// the reset. See enrichEvent's doc comment.
	evt := library.CanonicalizeEvent(s.enrichEvent(r, library.Event{
		Code:    library.EventCodeConfigResetSuccess,
		Message: "VTL reset to factory defaults; service restarting",
	}))
	_ = s.persist.Save(library.State{Events: []library.Event{evt}})

	writeJSON(w, http.StatusOK, map[string]string{"message": "reset successful; the service is restarting to apply it"})

	s.mu.RLock()
	restart := s.restartFunc
	s.mu.RUnlock()
	if restart != nil {
		restart()
	}
}
