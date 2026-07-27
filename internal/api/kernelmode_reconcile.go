package api

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/swenske/gotochanger/internal/library"
)

// ReconcileKernelModeInstancesAsync brings gotochanger-tcmud@<name> systemd
// instances in line with the current topology - one instance per logical
// library (named after it), or a single "default" instance when there are
// none - whenever operational_mode is "kernel". Called after wizard
// completion and after any logical-library create/update/delete (see
// wizard.go's CompleteWizard and handlers.go's logical-library handlers) -
// see ReconcileKernelModeInstancesAsyncOnStartup for the gotochangerd-
// startup-specific variant.
//
// Always runs in its own goroutine, never on the request-handling
// goroutine (or, at startup, blocking the rest of main's setup):
// everything it does is best-effort (kernel mode may not be installed
// yet, the polkit rule may be missing, systemctl may be slow) and must
// never delay or fail the caller. Every external command carries its own
// timeout for the same reason - this must never be able to wedge
// gotochangerd itself, especially given gotochanger-tcmud's own history
// of kernel-level hangs (see CLAUDE.md's "Kernel mode (TCMU/LIO)"
// section) - though note that hang lived inside gotochanger-tcmud's own
// EnableBackstore call, not in `systemctl start`'s return (Type=simple
// services don't block start on readiness), so this code is not exposed
// to that specific class of hang - the timeouts here are pure defense in
// depth, not a response to a known failure mode in this path.
func (s *Server) ReconcileKernelModeInstancesAsync() {
	go s.reconcileKernelModeInstances(false)
}

// ReconcileKernelModeInstancesAsyncOnStartup is ReconcileKernelModeInstancesAsync's
// gotochangerd-own-startup variant (see cmd/gotochangerd/main.go) - the
// only difference is force=true, so every already-active, still-desired
// instance is unconditionally restarted rather than left alone when its
// drive set still matches.
//
// This is what stands in for systemd's own persistent "enabled" bit for
// these instances (see runSystemctl's doc comment for why this
// deliberately never enables/disables unit files, only starts/stops
// them) - a host reboot alone would already be covered by the plain
// (force=false) variant, since nothing would be "active" yet for a freshly
// booted host. force=true exists for the other, more common trigger of a
// gotochangerd startup: a redeploy, a crash-and-restart, or a manual
// `systemctl restart gotochanger` - cases where the already-running
// gotochanger-tcmud@<name> instances are left completely untouched by a
// plain reconcile, because their own drive set never changed, which was a
// real, reported rough edge: every such gotochangerd restart requires
// gotochanger-tcmud@<name> to be restarted by hand before it either (a)
// runs any newly-deployed code (gotochanger-kernel may have been upgraded
// in the same redeploy) or (b) re-reports its device paths at all
// (Server.kernelModeDevices is in-memory, wiped by gotochangerd's own
// restart - see "Kernel-mode devices now report..." in CLAUDE.md - and
// gotochanger-tcmud only ever reports once, right after its own startup,
// so an instance that itself never restarted stays permanently missing
// from GET /api/v1/kernel-mode/devices until something restarts it).
//
// Deliberately NOT solved with a systemd Requires=/BindsTo= dependency
// from gotochanger-tcmud@.service back onto gotochanger.service, even
// though that would also make a plain gotochanger.service restart bring
// gotochanger-tcmud@<name> back automatically - that was tried for a
// different reason earlier in this project ("Moving a drive between
// logical libraries..." in CLAUDE.md) and had to be reverted: systemd's
// unit dependency graph has no visibility into gotochangerd's own
// application-level "desired" instance set, so it would just as
// unconditionally resurrect an instance that a reset (or a switch out of
// kernel mode) deliberately wants stopped. Keeping this entirely inside
// reconcileKernelModeInstances means the exact same `desired` computation
// governs both starting an instance and force-restarting one - an
// instance that shouldn't exist is stopped either way, never resurrected.
func (s *Server) ReconcileKernelModeInstancesAsyncOnStartup() {
	go s.reconcileKernelModeInstances(true)
}

func (s *Server) reconcileKernelModeInstances(force bool) {
	if s.lib == nil || s.topology == nil {
		return
	}

	// desired defaults to "nothing should be running" - deliberately not
	// an early return when operational_mode isn't "kernel" (or kernel
	// mode isn't installed/available yet). A deployment that switches
	// away from kernel mode, or gets reset back to factory defaults
	// (which resets operational_mode along with everything else), still
	// needs any already-running gotochanger-tcmud instances stopped -
	// found for real: a VTL reset left a stale, orphaned
	// gotochanger-tcmud@Library1 instance running indefinitely, exposing
	// real SCSI devices for a logical library that no longer existed,
	// because the old code returned here before ever computing what
	// should be stopped.
	desired := map[string]bool{}
	var expectedDrives map[string]map[int]bool
	mode, ok, err := s.topology.GetSetting("operational_mode")
	if err == nil && ok && mode == "kernel" && currentKernelModeStatus().Available {
		libs := s.lib.ListLogicalLibraries()
		desired = desiredKernelModeInstances(libs)
		expectedDrives = expectedKernelModeDriveSets(libs, s.lib.Status().Drives)
	}

	active, err := activeKernelModeInstances()
	if err != nil {
		s.log.Warn("kernel-mode reconcile: list active instances failed", "error", err)
		return
	}

	for name := range desired {
		if active[name] {
			// Already running and still desired doesn't mean its content
			// is still correct - gotochanger-tcmud resolves its own drive
			// set once, at its own startup (Status() called once - see
			// "Kernel mode (TCMU/LIO)" in CLAUDE.md), so an instance that
			// keeps running across a drive reassignment (e.g. moving a
			// drive from one logical library to another via Admin > Logical
			// Libraries) never notices - both the losing and gaining
			// instance stay "desired" throughout, so the plain existence
			// diff above never touches either of them. Found for real: a
			// drive moved from Library2 to Library1 kept reporting
			// "(kernel mode, Library2)" in the Admin UI indefinitely.
			// Fixed by comparing each instance's last self-reported drive
			// set (internal/api/kernel_mode_devices.go) against what the
			// current topology actually assigns it, and restarting only
			// the instances where those disagree.
			s.restartKernelModeInstanceIfDriveSetChanged(name, expectedDrives[name], force)
			continue
		}
		if err := runSystemctl("start", kernelModeUnitName(name)); err != nil {
			s.log.Warn("kernel-mode reconcile: start failed", "instance", name, "error", err)
		} else {
			s.log.Info("kernel-mode reconcile: started instance", "instance", name)
		}
	}
	for name := range active {
		if desired[name] {
			continue
		}
		if err := runSystemctl("stop", kernelModeUnitName(name)); err != nil {
			s.log.Warn("kernel-mode reconcile: stop failed", "instance", name, "error", err)
		} else {
			s.log.Info("kernel-mode reconcile: stopped instance", "instance", name)
		}
	}
}

// shouldRestartKernelModeInstance is the pure decision half of
// restartKernelModeInstanceIfDriveSetChanged, split out so it's testable
// without ever invoking a real systemctl (see that function's doc comment,
// and kernelmode_reconcile_test.go's own note on why this suite otherwise
// never exercises runSystemctl for real): force always restarts; otherwise
// only when a report exists and it disagrees with expected.
func shouldRestartKernelModeInstance(reported map[int]bool, reportedOK bool, expected map[int]bool, force bool) bool {
	if force {
		return true
	}
	return reportedOK && !driveIndexSetsEqual(reported, expected)
}

// restartKernelModeInstanceIfDriveSetChanged restarts an already-active,
// still-desired instance when its last self-reported set of physical drive
// indices no longer matches what the current topology assigns it, or
// unconditionally when force is true (see
// ReconcileKernelModeInstancesAsyncOnStartup's doc comment for why that
// exists). Without force, if the instance hasn't reported anything yet
// (freshly started and hasn't gotten there, or crashed before its first
// report), there's nothing to compare against - deliberately not
// restarting in that case, both because there's no evidence of an actual
// mismatch and to avoid a restart loop racing the instance's own startup;
// a genuine mismatch will be caught on the next reconcile once it does
// report.
//
// Restart is done as stop-then-start, not a single "systemctl restart" -
// found the hard way: systemd's polkit check for RestartUnit passes a
// different "verb" value than StartUnit/StopUnit do, so
// configs/gotochanger-kernel.rules' existing start/stop-only rule rejects
// it outright ("Interactive authentication required"), exactly the
// scenario this whole mechanism exists to avoid. Widening the polkit grant
// to also cover "restart" was rejected as unnecessary - stop-then-start
// achieves the identical end state using only the two verbs already
// deliberately, narrowly authorized.
func (s *Server) restartKernelModeInstanceIfDriveSetChanged(name string, expected map[int]bool, force bool) {
	reported, ok := s.reportedKernelModeDriveIndices(name)
	if !shouldRestartKernelModeInstance(reported, ok, expected, force) {
		return
	}
	unit := kernelModeUnitName(name)
	if err := runSystemctl("stop", unit); err != nil {
		s.log.Warn("kernel-mode reconcile: restart (stop phase) failed", "instance", name, "error", err)
		return
	}
	reason := "drive assignment changed"
	if force {
		reason = "gotochangerd startup"
	}
	if err := runSystemctl("start", unit); err != nil {
		s.log.Warn("kernel-mode reconcile: restart (start phase) failed", "instance", name, "reason", reason, "error", err)
	} else {
		s.log.Info("kernel-mode reconcile: restarted instance", "instance", name, "reason", reason)
	}
}

// reportedKernelModeDriveIndices reads back the set of physical drive
// indices instance last reported via POST /api/v1/kernel-mode/devices/
// {instance} (see kernel_mode_devices.go). ok is false when no report
// exists at all (never reported, or cleared by a clean shutdown and not
// yet replaced).
func (s *Server) reportedKernelModeDriveIndices(name string) (map[int]bool, bool) {
	s.mu.RLock()
	report, ok := s.kernelModeDevices[name]
	s.mu.RUnlock()
	if !ok {
		return nil, false
	}
	set := make(map[int]bool, len(report.Drives))
	for idx := range report.Drives {
		set[idx] = true
	}
	return set, true
}

func kernelModeUnitName(instance string) string {
	return fmt.Sprintf("gotochanger-tcmud@%s.service", instance)
}

// desiredKernelModeInstances computes which gotochanger-tcmud instances
// should exist for the current topology: one per logical library (named
// after it), or a single "default" instance when there are none - the
// same "@default means the whole physical library, unscoped" convention
// systemd/gotochanger-tcmud@.service's own ExecStart uses.
func desiredKernelModeInstances(libs []*library.LogicalLibrary) map[string]bool {
	desired := map[string]bool{}
	if len(libs) == 0 {
		desired["default"] = true
		return desired
	}
	for _, l := range libs {
		desired[l.Name] = true
	}
	return desired
}

// expectedKernelModeDriveSets computes, for every instance
// desiredKernelModeInstances would produce, the set of physical drive
// indices (library.Drive.Index - never a locally-scoped loop position, see
// cmd/gotochanger-tcmud's own comment on that distinction) it should
// currently expose: all physical drives for the "default" (no logical
// libraries) case, or just that logical library's own Drives otherwise.
// Mirrors desiredKernelModeInstances' own branching so the two can never
// disagree on which instances exist.
func expectedKernelModeDriveSets(libs []*library.LogicalLibrary, allDrives []*library.Drive) map[string]map[int]bool {
	sets := map[string]map[int]bool{}
	if len(libs) == 0 {
		set := make(map[int]bool, len(allDrives))
		for _, d := range allDrives {
			set[d.Index] = true
		}
		sets["default"] = set
		return sets
	}
	for _, l := range libs {
		set := make(map[int]bool, len(l.Drives))
		for _, d := range l.Drives {
			set[d.Index] = true
		}
		sets[l.Name] = set
	}
	return sets
}

// driveIndexSetsEqual compares two physical-drive-index sets for exact
// equality (same members, regardless of iteration order - these are
// always built from a map already, so order was never meaningful).
func driveIndexSetsEqual(a, b map[int]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// kernelModeUnitPrefix/kernelModeUnitSuffix bound the instance-name
// extraction in activeKernelModeInstances - kept as named constants
// rather than inlined so kernelModeUnitName and this parsing can never
// drift apart.
const (
	kernelModeUnitPrefix = "gotochanger-tcmud@"
	kernelModeUnitSuffix = ".service"
)

// activeKernelModeInstances lists every gotochanger-tcmud@<name> instance
// currently active (running) - a plain read-only systemd query, needs no
// elevated privilege at all (listing units is always allowed for any
// local user), unlike runSystemctl's start/stop calls.
func activeKernelModeInstances() (map[string]bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "list-units",
		"--type=service", "--no-legend", "--plain", "--state=active", kernelModeUnitPrefix+"*"+kernelModeUnitSuffix).Output()
	if err != nil {
		return nil, fmt.Errorf("systemctl list-units: %w", err)
	}
	return parseActiveKernelModeInstances(string(out)), nil
}

// parseActiveKernelModeInstances is the pure-logic half of
// activeKernelModeInstances, split out so it's testable without a real
// systemctl - see kernelmode_reconcile_test.go for the exact
// "list-units --no-legend --plain --state=active" output shape this
// parses (UNIT LOAD ACTIVE SUB DESCRIPTION - every returned line is
// already active thanks to --state=active, so unlike a list-unit-files
// parse there's no separate state column to re-check here).
func parseActiveKernelModeInstances(output string) map[string]bool {
	active := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		name := fields[0]
		if !strings.HasPrefix(name, kernelModeUnitPrefix) || !strings.HasSuffix(name, kernelModeUnitSuffix) {
			continue
		}
		inst := strings.TrimSuffix(strings.TrimPrefix(name, kernelModeUnitPrefix), kernelModeUnitSuffix)
		active[inst] = true
	}
	return active
}

// runSystemctl runs exactly the two command shapes
// /usr/share/polkit-1/rules.d/gotochanger-kernel.rules authorizes the
// gotochangerd service account for: "systemctl start
// gotochanger-tcmud@*.service" and the stop equivalent - nothing else,
// and deliberately never enable/disable.
//
// No sudo, no setuid, no privilege escalation of gotochangerd's own
// process at all: systemctl talks to systemd (already running as root)
// over D-Bus, and systemd checks the *caller's* polkit authorization for
// the specific action/unit/verb before acting on its own (already
// privileged) side - gotochangerd's own process never gains anything.
// This is what makes this compatible with gotochanger.service's
// NoNewPrivileges=true - found the hard way: an earlier version of this
// used "sudo systemctl enable --now ...", authorized via a sudoers.d
// NOPASSWD rule, which failed outright ("sudo: the 'no new privileges'
// flag is set, which prevents sudo from running as root") the first time
// it actually ran against the real installed service, since sudo's
// privilege elevation is exactly the class of thing NoNewPrivileges
// blocks (a setuid-root exec), unlike an authenticated D-Bus call to an
// already-running privileged daemon. Also why this only ever uses
// start/stop, never enable/disable: systemd's polkit integration passes
// per-call "unit"/"verb" details for org.freedesktop.systemd1.manage-units
// (what start/stop need), letting the rules file authorize narrowly by
// unit-name pattern, but does not pass equivalent details for
// org.freedesktop.systemd1.manage-unit-files (what enable/disable need) -
// granting that action at all would authorize enabling/disabling *any*
// unit on the host, not just gotochanger-tcmud@*, defeating the point of
// a narrow grant. Persistence across a reboot instead comes from
// reconciling once at gotochangerd startup (see ReconcileKernelModeInstancesAsync's
// doc comment) - gotochangerd itself is the source of truth for what
// should be running, not systemd's enabled-unit-file bit.
func runSystemctl(verb, unit string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemctl", verb, unit)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}
