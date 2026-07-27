package api

import (
	"encoding/json"
	"net/http"
)

// KernelModeDrivePaths is one drive's real, kernel-assigned device
// node(s), as reported by gotochanger-tcmud (see internal/tcmu.DevicePaths,
// which this mirrors) - only ever set from a running kernel-mode
// instance's own self-report, never derived or guessed by gotochangerd
// itself, which has no visibility into kernel-assigned device numbers.
type KernelModeDrivePaths struct {
	Generic string `json:"generic"`
	Tape    string `json:"tape,omitempty"`

	// StableGeneric/StableTape are the /dev/tape/by-id/... paths udev
	// derives from this device's VPD page 0x83 identity (see
	// internal/scsi/vpd.go, internal/tcmu.DiscoverStablePaths) - unlike
	// Generic/Tape above (raw kernel-assigned /dev/sgN/dev/nstN numbers,
	// reassigned non-deterministically across a gotochanger-tcmud
	// restart), these stay the same across a restart, matching how a
	// real tape library's own WWN-derived /dev/tape/by-id path works.
	// Empty until udev has processed the device.
	StableGeneric string `json:"stable_generic,omitempty"`
	StableTape    string `json:"stable_tape,omitempty"`
}

// KernelModeDeviceReport is what one gotochanger-tcmud instance reports
// about itself: its changer's real device path, plus one entry per drive
// keyed by that drive's real physical index (gotochangerd's own
// Drive.Index, not any locally-scoped position - see
// cmd/gotochanger-tcmud's own comment on this distinction).
type KernelModeDeviceReport struct {
	Changer string `json:"changer"`
	// ChangerStable is the changer's own stable by-id path - kept as a
	// separate additive field rather than widening Changer to the full
	// KernelModeDrivePaths shape, which would carry a permanently
	// meaningless Tape/StableTape pair for a changer and force every
	// existing "Changer: \"/dev/sg4\"" test literal to be rewritten for
	// no real benefit.
	ChangerStable string                       `json:"changer_stable,omitempty"`
	Drives        map[int]KernelModeDrivePaths `json:"drives"`
}

// handleReportKernelModeDevices lets a running gotochanger-tcmud instance
// tell gotochangerd what real device paths the kernel assigned it, for
// the Admin UI (Drives, Logical Libraries) and the Bareos Config
// generator to display - see CLAUDE.md's "Kernel mode (TCMU/LIO)" for why
// gotochangerd itself has no way to discover these on its own (it isn't
// the process that talks to the kernel's TCMU/configfs/sysfs interfaces
// at all). Operator+, matching every other kernel-mode write action.
// {instance} is whatever name the reporting gotochanger-tcmud invocation
// uses for itself (its logical library's name, or "default" for the
// whole-physical-library case - see cmd/gotochanger-tcmud's
// kernelModeInstanceName) - not validated or looked up against real
// logical libraries here, just an opaque key both sides agree on.
func (s *Server) handleReportKernelModeDevices(w http.ResponseWriter, r *http.Request) {
	instance := r.PathValue("instance")
	var report KernelModeDeviceReport
	if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.mu.Lock()
	if s.kernelModeDevices == nil {
		// Defensive, not just belt-and-suspenders: some tests construct
		// a Server{} literal directly rather than through New (see
		// handlers_test.go's newTestServer), which would otherwise leave
		// this nil and panic on write.
		s.kernelModeDevices = map[string]KernelModeDeviceReport{}
	}
	s.kernelModeDevices[instance] = report
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// handleClearKernelModeDevices removes a previously-reported device set -
// called by gotochanger-tcmud on a clean shutdown, so the Admin UI/Bareos
// Config generator don't keep showing device paths that no longer exist.
// An unclean shutdown (crash, kill -9) can't call this, so a stale report
// can persist until the instance's next clean start/stop cycle - a known,
// accepted limitation (see CLAUDE.md), not different in kind from any
// other "best effort, refresh as needed" reference display this project
// already has.
func (s *Server) handleClearKernelModeDevices(w http.ResponseWriter, r *http.Request) {
	instance := r.PathValue("instance")
	s.mu.Lock()
	delete(s.kernelModeDevices, instance)
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// handleListKernelModeDevices returns every currently-reported device set,
// keyed by instance name - Viewer+, purely informational (Admin > Drives/
// Logical Libraries and the Bareos Config generator all read from this).
func (s *Server) handleListKernelModeDevices(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	out := make(map[string]KernelModeDeviceReport, len(s.kernelModeDevices))
	for k, v := range s.kernelModeDevices {
		out[k] = v
	}
	s.mu.RUnlock()
	writeJSON(w, http.StatusOK, out)
}
