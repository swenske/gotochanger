package library

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/swenske/gotochanger/internal/barcode"
	"github.com/swenske/gotochanger/internal/config"
	"github.com/swenske/gotochanger/internal/secrethash"
)

// barcodeRE restricts barcodes to safe filename characters, preventing path
// traversal or shell-metacharacter injection when the barcode is used to
// build a filesystem path (OWASP A03: Injection). This is a filesystem
// safety net on top of, not instead of, the per-tape-type format check
// (internal/barcode.Validate) - a real family's format is always a subset
// of this charset, but this also covers the "generic" family's more
// permissive shape.
var barcodeRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// Persister is implemented by the store package. Library persists its full
// dynamic state (volume placement) through this interface after every
// mutation so a restart of the daemon does not lose track of loaded media.
type Persister interface {
	Save(State) error
}

// State is the serializable dynamic state of the library (element -> volume
// assignments). Topology (counts, device paths, logical libraries) always
// comes from Config/the topology store, never from here.
type State struct {
	Slots          []*Slot      `json:"slots"`
	IOSlots        []*IOSlot    `json:"ioslots"`
	Drives         []*Drive     `json:"drives"`
	OutsideVolumes []*Volume    `json:"outside_volumes,omitempty"`
	OffsiteVolumes []*Volume    `json:"offsite_volumes,omitempty"`
	Doors          DoorStatus   `json:"doors,omitempty"`
	Events         []Event      `json:"events,omitempty"`
	RoboticFault   RoboticFault `json:"robotic_fault,omitempty"`
}

// LogicalLibrary represents a partition of the virtual tape library: a
// named subset of the drives/slots/ioslots above, for Bareos Autochanger
// resources that need to address only part of a shared physical library
// (see gotochanger-changer's --logical-library flag).
type LogicalLibrary struct {
	Name    string    `json:"name"`
	Drives  []*Drive  `json:"drives"`
	Slots   []*Slot   `json:"slots"`
	IOSlots []*IOSlot `json:"io_slots"`
	Color   string    `json:"color"`
}

// Library is the concurrency-safe in-memory model of the simulated
// autochanger.
type Library struct {
	mu                   sync.RWMutex
	cfg                  config.Config
	slots                []*Slot
	ioslots              []*IOSlot
	drives               []*Drive
	logicalLibs          []*LogicalLibrary
	outside              []*Volume
	offsite              []*Volume
	ioDoorOpen           map[string]bool // mailbox ID -> open
	stDoorOpen           map[string]bool // magazine ID -> open
	events               []Event
	notifier             Notifier
	persist              Persister
	latencyEnabled       bool
	driveLoadLatency     time.Duration
	driveUnloadLatency   time.Duration
	tapePositionLatency  time.Duration
	robotMoveTapeLatency time.Duration
	robotMoveScanLatency time.Duration
	magazineScanLatency  time.Duration
	doorActionLatency    time.Duration
	roboticFault         RoboticFault

	cleaningEnabled        bool
	cleaningMode           string
	cleaningMaxUses        int
	cleaningMountThreshold int
	cleaningDuration       time.Duration

	// phaseMu guards doorPhases/phaseNotifier and, as of the robotic-arm
	// live-state rework, armBusy/armLastPosition/armOpenDoors/armSteps too
	// - deliberately independent of mu: a phase/arm read or write must
	// never block behind the multi-second sleep a door/Move/Load/Unload
	// method holds mu for, or a live status read couldn't observe the
	// in-progress phase/arm state until the operation already finished.
	phaseMu       sync.Mutex
	doorPhases    map[string]string // "magazine:<id>" / "mailbox:<id>" -> "opening"|"closing"|"scanning"
	phaseNotifier PhaseNotifier

	// armBusy/armLastPosition/armOpenDoors/armSteps: the robotic arm's
	// live, non-audited state - see currentArmStateLocked,
	// setArmBusy/setArmPosition/setArmDoorsOpenDelta/recordArmStep,
	// ArmState/ArmSteps. Never persisted (not part of State/saveLocked/
	// restore) - like doorPhases, this is UI telemetry that starts fresh
	// at its Go zero value on every daemon start; armOpenDoors alone is
	// reconciled against the persisted stDoorOpen/ioDoorOpen after
	// restore/Reconfigure (see reconcileArmDoorsLocked) since those two
	// maps genuinely are persisted and could disagree with a fresh 0.
	armBusy         bool
	armLastPosition ArmPosition
	armOpenDoors    int
	armSteps        []ArmStep

	// magazinePINHash/mailboxPINHash implement presence-implies-protection
	// PIN gating (see checkMagazinePINLocked/checkMailboxPINLocked): an
	// empty magazinePINHash, or a mailbox ID absent from mailboxPINHash,
	// means that magazine/mailbox has no PIN configured and opens freely.
	magazinePINHash string
	mailboxPINHash  map[string]string // mailbox ID -> PIN hash, only present when actually configured

	// driveWatchers holds one live driveActivityWatcher per loaded drive,
	// keyed by drive index - see startDriveWatcherLocked/
	// stopDriveWatcherLocked/reconcileDriveWatchersLocked. Deliberately
	// keyed on Library, not stored on *Drive itself: Reconfigure rebuilds
	// l.drives from scratch on every topology change (buildTopologyLocked),
	// which would silently leak a running watcher goroutine/fd if it lived
	// on the old, discarded Drive struct instead.
	driveWatchers map[int]driveActivityWatcher
}

const maxEvents = 500

// maxArmSteps caps the live-only arm-narration ring buffer (see
// recordArmStep/ArmSteps) - deliberately much smaller than maxEvents,
// since this is only ever meant to seed a freshly (re)connected SSE
// client's activity panel, not serve as any kind of durable history.
const maxArmSteps = 50

// New builds a Library from configuration, optionally restoring dynamic
// state previously produced by State() (e.g. loaded from disk at startup).
func New(cfg config.Config, restored *State, notifier Notifier, persist Persister) (*Library, error) {
	l := &Library{cfg: cfg, notifier: notifier, persist: persist, stDoorOpen: map[string]bool{}, ioDoorOpen: map[string]bool{}, doorPhases: map[string]string{}, driveWatchers: map[int]driveActivityWatcher{}}
	l.buildTopologyLocked(cfg)

	if restored != nil {
		if err := l.restore(restored); err != nil {
			return nil, err
		}
	}
	// Resume activity monitoring for any drive that came back already
	// loaded (its device-path symlink is a real file left over from
	// before a daemon restart - see the Drive.DevicePath doc comment -
	// so a real backup job could already be writing to it again the
	// moment this process starts). Safe to call without l.mu: nothing
	// else can see l yet (see buildTopologyLocked's doc comment).
	l.reconcileDriveWatchersLocked()
	// Same reasoning for the arm's parked state: stDoorOpen/ioDoorOpen are
	// persisted, armOpenDoors is not, so a restart with a door already
	// open must not report the arm as un-parked just because armOpenDoors
	// defaults to 0.
	l.reconcileArmDoorsLocked()

	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "volumes"), 0o770); err != nil {
		return nil, fmt.Errorf("create volumes dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "exported"), 0o770); err != nil {
		return nil, fmt.Errorf("create exported dir: %w", err)
	}
	for _, d := range l.drives {
		if err := os.MkdirAll(filepath.Dir(d.DevicePath), 0o770); err != nil {
			return nil, fmt.Errorf("create drive dir for drive %d: %w", d.Index, err)
		}
	}
	return l, nil
}

// buildTopologyLocked (re)builds drives/slots/ioslots/logicalLibs from cfg.
// Shared by New() (fresh construction) and Reconfigure() (applying a
// topology change to an already-running Library without a restart).
// Callers must hold l.mu for Reconfigure; New() calls it before l is
// published to any other goroutine, so no lock is needed there.
func (l *Library) buildTopologyLocked(cfg config.Config) {
	l.cfg = cfg
	l.drives = nil
	l.slots = nil
	l.ioslots = nil
	l.logicalLibs = nil

	for i, dd := range cfg.Library.DriveDevices {
		l.drives = append(l.drives, &Drive{Index: i, DevicePath: dd.DevicePath, DriveType: dd.DriveType})
	}

	// Each magazine/mailbox's own persisted BaseAddress (assigned once at
	// creation by the topology store, never recomputed here) is used
	// directly - deliberately *not* a running counter across the whole
	// list, which is what used to make every magazine/mailbox's
	// addresses depend on everything listed before it. See the store's
	// migrateTopologyBaseAddresses doc comment for the full history.
	for _, mag := range cfg.Library.Magazines {
		for i := 0; i < mag.Slots; i++ {
			l.slots = append(l.slots, &Slot{Address: mag.BaseAddress + i, MagazineID: mag.ID})
		}
	}

	l.mailboxPINHash = map[string]string{}
	for _, mb := range cfg.Library.Mailboxes {
		for i := 0; i < mb.Slots; i++ {
			l.ioslots = append(l.ioslots, &IOSlot{Address: mb.BaseAddress + i, MailboxID: mb.ID})
		}
		if mb.PINHash != "" {
			l.mailboxPINHash[mb.ID] = mb.PINHash
		}
	}
	l.magazinePINHash = cfg.Library.MagazinePINHash

	for _, libCfg := range cfg.Library.LogicalLibraries {
		lib := l.resolveLogicalLibraryLocked(libCfg)
		l.logicalLibs = append(l.logicalLibs, lib)
	}

	l.resolveLatencyLocked(cfg.Library.Latency)
	l.resolveCleaningLocked(cfg.Library.Cleaning)
	// Note: l.roboticFault is deliberately left untouched here - it isn't
	// tied to any drive/slot/mailbox that could disappear in a topology
	// change, so Reconfigure (which calls buildTopologyLocked) carries an
	// active fault across a topology change for free.
}

// startDriveWatcherLocked starts (or restarts) activity monitoring for
// driveIndex against path. Callers must hold l.mu.
func (l *Library) startDriveWatcherLocked(driveIndex int, path string) {
	l.stopDriveWatcherLocked(driveIndex)
	l.driveWatchers[driveIndex] = startDriveActivityWatcher(path, func(kind string) {
		l.recordDriveActivity(driveIndex, path, kind)
	})
}

// stopDriveWatcherLocked stops and removes driveIndex's watcher, if any.
// Callers must hold l.mu.
func (l *Library) stopDriveWatcherLocked(driveIndex int) {
	if w, ok := l.driveWatchers[driveIndex]; ok {
		w.stop()
		delete(l.driveWatchers, driveIndex)
	}
}

// reconcileDriveWatchersLocked starts a watcher for every loaded drive
// that doesn't already have one, and stops any watcher whose drive is no
// longer loaded (or no longer exists) - called after New() restores
// persisted state and after every Reconfigure(), so activity monitoring
// survives both a daemon restart and a topology change without leaking a
// watcher goroutine/fd for a drive that's gone. Callers must hold l.mu
// (or, for New(), call before l is published to any other goroutine).
func (l *Library) reconcileDriveWatchersLocked() {
	live := make(map[int]bool, len(l.drives))
	for i, d := range l.drives {
		if d.Volume == nil {
			continue
		}
		live[i] = true
		if _, ok := l.driveWatchers[i]; !ok {
			l.startDriveWatcherLocked(i, d.Volume.Path)
		}
	}
	for idx := range l.driveWatchers {
		if !live[idx] {
			l.stopDriveWatcherLocked(idx)
		}
	}
}

// recordDriveActivity is called from a driveActivityWatcher's own
// goroutine (see startDriveActivityWatcher), never holding l.mu itself.
// path is the backing file path the watcher was started against,
// captured at start time - re-checked here against the drive's current
// volume so a stale callback from a watcher that should already have
// been stopped (a race against Unload/Reconfigure) is a safe no-op
// instead of corrupting a different, since-loaded volume's activity.
func (l *Library) recordDriveActivity(driveIndex int, path, kind string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if driveIndex < 0 || driveIndex >= len(l.drives) {
		return
	}
	d := l.drives[driveIndex]
	if d.Volume == nil || d.Volume.Path != path {
		return
	}
	d.Activity = kind
	if kind == driveActivityWrite {
		l.refreshVolumeSizeLocked(d.Volume)
	}
	detail := map[string]string{"drive": fmt.Sprint(driveIndex), "volume": d.Volume.Barcode}
	switch kind {
	case driveActivityRead:
		l.emit("drive-activity-read", fmt.Sprintf("drive %d reading volume %q", driveIndex, d.Volume.Barcode), detail)
	case driveActivityWrite:
		l.emit("drive-activity-write", fmt.Sprintf("drive %d writing volume %q", driveIndex, d.Volume.Barcode), detail)
	default:
		l.emit("drive-activity-idle", fmt.Sprintf("drive %d activity idle", driveIndex), detail)
	}
	l.saveLocked()
}

// resolveLatencyLocked resolves a LatencySettings into the individual
// time.Duration fields the sleep points below actually read, zeroing
// everything when disabled. Shared by buildTopologyLocked (fresh
// construction/Reconfigure) and UpdateLatencySettings (live-apply from
// the Admin API), which previously duplicated this logic. Callers must
// hold l.mu.
func (l *Library) resolveLatencyLocked(ls config.LatencySettings) {
	l.latencyEnabled = ls.Enabled
	if !ls.Enabled {
		l.driveLoadLatency, l.driveUnloadLatency, l.tapePositionLatency = 0, 0, 0
		l.robotMoveTapeLatency, l.robotMoveScanLatency, l.magazineScanLatency, l.doorActionLatency = 0, 0, 0, 0
		return
	}
	l.driveLoadLatency, _ = config.ParseDuration(ls.DriveLoad)
	l.driveUnloadLatency, _ = config.ParseDuration(ls.DriveUnload)
	l.tapePositionLatency, _ = config.ParseDuration(ls.TapePositioning)
	l.robotMoveTapeLatency, _ = config.ParseDuration(ls.RobotMoveTape)
	l.robotMoveScanLatency, _ = config.ParseDuration(ls.RobotMoveScan)
	l.magazineScanLatency, _ = config.ParseDuration(ls.MagazineScan)
	l.doorActionLatency, _ = config.ParseDuration(ls.DoorAction)
}

// resolveCleaningLocked resolves a CleaningSettings into the individual
// fields Load/Unload/TriggerCleaning/AutoCleanSweep actually read. Shared
// by buildTopologyLocked (fresh construction/Reconfigure) and
// UpdateCleaningSettings (live-apply from the Admin API). Callers must
// hold l.mu. Unlike resolveLatencyLocked, cleaningMaxUses/
// cleaningMountThreshold/cleaningDuration are meaningful even when
// disabled (Enabled only gates the automatic sweep - a manually
// triggered cleaning cycle still needs a real duration/max-uses to
// simulate against), so nothing is zeroed here.
func (l *Library) resolveCleaningLocked(cs config.CleaningSettings) {
	l.cleaningEnabled = cs.Enabled
	l.cleaningMode = cs.Mode
	l.cleaningMaxUses = cs.MaxUses
	l.cleaningMountThreshold = cs.MountThreshold
	l.cleaningDuration, _ = config.ParseDuration(cs.Duration)
}

// resolveLogicalLibraryLocked builds a *LogicalLibrary by resolving a
// config-style DTO's drive indices/magazine IDs/mailbox IDs against the
// live l.drives/l.slots/l.ioslots (the same pointers, not copies — so
// mutating a drive/slot elsewhere is visible through the logical library
// too). Callers must hold l.mu.
func (l *Library) resolveLogicalLibraryLocked(libCfg config.LogicalLibraryConfig) *LogicalLibrary {
	lib := &LogicalLibrary{
		Name:    libCfg.Name,
		Color:   libCfg.Color,
		Drives:  make([]*Drive, 0),
		Slots:   make([]*Slot, 0),
		IOSlots: make([]*IOSlot, 0),
	}
	for _, driveIdx := range libCfg.Drives {
		if driveIdx >= 0 && driveIdx < len(l.drives) {
			lib.Drives = append(lib.Drives, l.drives[driveIdx])
		}
	}
	for _, magID := range libCfg.Magazines {
		for _, slot := range l.slots {
			if slot.MagazineID == magID {
				lib.Slots = append(lib.Slots, slot)
			}
		}
	}
	for _, mbID := range libCfg.Mailboxes {
		for _, io := range l.ioslots {
			if io.MailboxID == mbID {
				lib.IOSlots = append(lib.IOSlots, io)
			}
		}
	}
	return lib
}

// Reconfigure applies a new topology to an already-running Library without
// requiring a daemon restart: used when the setup wizard completes and when
// the Admin API adds/changes drives, magazines, I/O slots, or logical
// libraries. Existing volume placements are preserved wherever the same
// address/index still exists in the new topology; addresses that no longer
// exist are dropped from tracking (the caller is expected to have already
// refused a removal that would orphan a volume - see the Admin API's
// magazine-delete handler).
func (l *Library) Reconfigure(cfg config.Config) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	oldSlots := make(map[int]*Volume, len(l.slots))
	for _, s := range l.slots {
		if s.Volume != nil {
			oldSlots[s.Address] = s.Volume
		}
	}
	oldIOSlots := make(map[int]*Volume, len(l.ioslots))
	for _, io := range l.ioslots {
		if io.Volume != nil {
			oldIOSlots[io.Address] = io.Volume
		}
	}
	oldDrives := make(map[int]*Drive, len(l.drives))
	for _, d := range l.drives {
		oldDrives[d.Index] = d
	}

	l.buildTopologyLocked(cfg)

	for _, s := range l.slots {
		if v, ok := oldSlots[s.Address]; ok {
			s.Volume = v
		}
	}
	for _, io := range l.ioslots {
		if v, ok := oldIOSlots[io.Address]; ok {
			io.Volume = v
		}
	}
	for _, d := range l.drives {
		if old, ok := oldDrives[d.Index]; ok && old.DevicePath == d.DevicePath {
			d.Volume = old.Volume
			d.Fault = old.Fault
			d.Origin = old.Origin
			d.MountsSinceCleaning = old.MountsSinceCleaning
			d.Activity = old.Activity
		}
	}
	// Resume/stop activity watchers to match the drives that actually
	// carried a volume forward above - a watcher is keyed by index on
	// Library (see driveWatchers' doc comment), not on the now-discarded
	// old *Drive pointers, so it survives this rebuild on its own only if
	// reconciled explicitly here.
	l.reconcileDriveWatchersLocked()

	// Drop "open" flags for magazines that no longer exist in the new
	// topology, so a deleted magazine doesn't leak a stale entry forever
	// and a different magazine later reusing the same ID doesn't
	// spuriously inherit it.
	for magID := range l.stDoorOpen {
		if !l.magazineExistsLocked(magID) {
			delete(l.stDoorOpen, magID)
		}
	}
	// Same cleanup for mailbox I/O doors, for the same reason: a deleted
	// mailbox shouldn't leak a stale "open" entry, and a different mailbox
	// later reusing the same ID shouldn't inherit it.
	for mbID := range l.ioDoorOpen {
		if !l.mailboxExistsLocked(mbID) {
			delete(l.ioDoorOpen, mbID)
		}
	}
	// Recompute after the pruning above, so a deleted-and-thus-dropped
	// open magazine/mailbox doesn't keep the arm reported as parked.
	l.reconcileArmDoorsLocked()

	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "volumes"), 0o770); err != nil {
		return fmt.Errorf("create volumes dir: %w", err)
	}
	for _, d := range l.drives {
		if err := os.MkdirAll(filepath.Dir(d.DevicePath), 0o770); err != nil {
			return fmt.Errorf("create drive dir for drive %d: %w", d.Index, err)
		}
	}
	l.saveLocked()
	return nil
}

// restore re-applies previously persisted dynamic state (which volume sits
// where, drive fault/cleaning counters) onto the topology buildTopologyLocked
// has just constructed from config.
//
// Elements are matched by address (slots/ioslots) and physical index
// (drives), exactly like Reconfigure - deliberately NOT by array position,
// which is what this used to do behind an all-or-nothing "only if the
// lengths still match" guard. Position-based matching silently discarded
// every volume placement whenever the element count changed at all while
// the daemon was down, and quietly reassigned volumes to the wrong element
// whenever the counts happened to match but the ordering didn't. Matching
// by address is both safer and consistent with Reconfigure, now that each
// magazine/mailbox owns a permanent base_address (see the store's
// migrateTopologyBaseAddresses doc comment).
func (l *Library) restore(s *State) error {
	restoredSlots := make(map[int]*Volume, len(s.Slots))
	for _, sl := range s.Slots {
		if sl != nil && sl.Volume != nil {
			restoredSlots[sl.Address] = sl.Volume
		}
	}
	for _, sl := range l.slots {
		if v, ok := restoredSlots[sl.Address]; ok {
			sl.Volume = v
		}
	}
	restoredIOSlots := make(map[int]*Volume, len(s.IOSlots))
	for _, io := range s.IOSlots {
		if io != nil && io.Volume != nil {
			restoredIOSlots[io.Address] = io.Volume
		}
	}
	for _, io := range l.ioslots {
		if v, ok := restoredIOSlots[io.Address]; ok {
			io.Volume = v
		}
	}
	restoredDrives := make(map[int]*Drive, len(s.Drives))
	for _, d := range s.Drives {
		if d != nil {
			restoredDrives[d.Index] = d
		}
	}
	for _, d := range l.drives {
		old, ok := restoredDrives[d.Index]
		if !ok {
			continue
		}
		d.Volume = old.Volume
		d.Fault = old.Fault
		d.MountsSinceCleaning = old.MountsSinceCleaning
		// Origin is what "which slot did this loaded tape come from" queries
		// answer with (DriveOriginSlot, and through it gotochanger-changer's
		// "loaded"/"listall" output that Bareos relies on to unload a tape
		// back where it belongs). It is persisted by saveLocked, so failing
		// to restore it here silently reported slot 0 for every drive that
		// still had a tape loaded across a daemon restart.
		d.Origin = old.Origin
		// Activity is deliberately NOT restored: it's live telemetry from a
		// per-drive filesystem watcher, and reconcileDriveWatchersLocked
		// starts a fresh watcher below. A restored "writing" would stick
		// forever, since debounceActivity is edge-triggered and would never
		// see a transition away from a value it doesn't know it has.
	}
	l.outside = append([]*Volume(nil), s.OutsideVolumes...)
	l.offsite = append([]*Volume(nil), s.OffsiteVolumes...)
	for _, magID := range s.Doors.OpenMagazines {
		l.stDoorOpen[magID] = true
	}
	for _, mbID := range s.Doors.OpenMailboxes {
		l.ioDoorOpen[mbID] = true
	}
	l.events = append([]Event(nil), s.Events...)
	l.roboticFault = s.RoboticFault
	return nil
}

// State returns a snapshot suitable for persistence.
func (l *Library) State() State {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.stateLocked()
}

// stateLocked builds the serializable dynamic-state snapshot that both
// State() and saveLocked() use. Deliberately the single place that decides
// which fields are part of "the state": these two used to enumerate the
// struct's fields independently, and drifted - saveLocked simply omitted
// OffsiteVolumes, so every volume sent offsite was persisted as existing
// nowhere at all and was forgotten on the next restart. Callers must hold
// l.mu (read or write).
func (l *Library) stateLocked() State {
	return State{
		Slots:          l.slots,
		IOSlots:        l.ioslots,
		Drives:         l.drives,
		OutsideVolumes: l.outside,
		OffsiteVolumes: l.offsite,
		Doors: DoorStatus{
			OpenMagazines: l.openMagazinesLocked(),
			OpenMailboxes: l.openMailboxesLocked(),
		},
		Events:       append([]Event(nil), l.events...),
		RoboticFault: l.roboticFault,
	}
}

// LogicalLibraryStatus returns the status of a specific logical library.
// Like Status(), every element and volume returned is a copy - see
// snapshotElementsLocked.
func (l *Library) LogicalLibraryStatus(name string) Status {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, lib := range l.logicalLibs {
		if lib.Name != name {
			continue
		}
		_, _, _, byAddr, byIOAddr, byDriveIndex := l.snapshotElementsLocked()
		scoped := l.snapshotLogicalLibLocked(lib, byAddr, byIOAddr, byDriveIndex)
		return Status{
			Name:           l.cfg.Library.Name,
			Slots:          scoped.Slots,
			IOSlots:        scoped.IOSlots,
			Drives:         scoped.Drives,
			OutsideVolumes: snapshotVolumes(l.outside),
			OffsiteVolumes: snapshotVolumes(l.offsite),
			OffsiteEnabled: l.cfg.Library.OffsiteLocation,
			Doors: DoorStatus{
				OpenMagazines: l.openMagazinesLocked(),
				OpenMailboxes: l.openMailboxesLocked(),
				Phases:        l.DoorPhases(),
			},
			LogicalLibs:         []*LogicalLibrary{scoped},
			RoboticFault:        l.roboticFault,
			ArmState:            l.ArmState(),
			MagazinePINRequired: l.magazinePINHash != "",
			MailboxPINRequired:  l.mailboxPINRequiredLocked(),
		}
	}
	return Status{}
}

// UpdateLiveSettings applies the subset of library configuration that can
// safely change without restarting the daemon (topology such as slot/drive
// counts changes through Reconfigure instead).
func (l *Library) UpdateLiveSettings(defaultCapacity string, offsiteEnabled bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cfg.Library.DefaultCapacity = defaultCapacity
	l.cfg.Library.OffsiteLocation = offsiteEnabled
}

// UpdateLatencySettings applies a new set of simulated timing delays
// without a restart, the latency counterpart to UpdateLiveSettings.
// Separate from it (rather than one more parameter) since latency is a
// richer, dedicated Admin > Latency sub-resource (internal/api/latency.go)
// with its own GET/PUT endpoints, not a flat Settings field.
func (l *Library) UpdateLatencySettings(ls config.LatencySettings) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cfg.Library.Latency = ls
	l.resolveLatencyLocked(ls)
}

// UpdateCleaningSettings applies new cleaning-tape management settings
// without a restart, the cleaning counterpart to UpdateLatencySettings.
func (l *Library) UpdateCleaningSettings(cs config.CleaningSettings) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cfg.Library.Cleaning = cs
	l.resolveCleaningLocked(cs)
}

// UpdateMagazinePINHash live-applies a change to the single global
// magazine PIN hash without a restart, mirroring UpdateLatencySettings'
// persist-then-live-apply shape. An empty hash clears the PIN entirely
// (presence-implies-protection: no PIN configured means every magazine
// opens freely).
func (l *Library) UpdateMagazinePINHash(hash string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cfg.Library.MagazinePINHash = hash
	l.magazinePINHash = hash
}

// GetLogicalLibrary returns a logical library by name, deep-copied like
// ListLogicalLibraries.
func (l *Library) GetLogicalLibrary(name string) *LogicalLibrary {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, lib := range l.logicalLibs {
		if lib.Name == name {
			_, _, _, byAddr, byIOAddr, byDriveIndex := l.snapshotElementsLocked()
			return l.snapshotLogicalLibLocked(lib, byAddr, byIOAddr, byDriveIndex)
		}
	}
	return nil
}

// exclusivityConflictLocked reports the name of another logical library
// (other than exclude) that already claims one of libCfg's drives/
// magazines/io-slots, if any. Logical libraries partition the physical
// library, so an element may belong to at most one. Callers must hold l.mu.
func (l *Library) exclusivityConflictLocked(libCfg config.LogicalLibraryConfig, exclude string) (string, bool) {
	drives := make(map[int]bool, len(libCfg.Drives))
	for _, d := range libCfg.Drives {
		drives[d] = true
	}
	mags := make(map[string]bool, len(libCfg.Magazines))
	for _, m := range libCfg.Magazines {
		mags[m] = true
	}
	mbs := make(map[string]bool, len(libCfg.Mailboxes))
	for _, mb := range libCfg.Mailboxes {
		mbs[mb] = true
	}
	for _, existing := range l.logicalLibs {
		if existing.Name == exclude {
			continue
		}
		for _, d := range existing.Drives {
			if drives[d.Index] {
				return existing.Name, true
			}
		}
		for _, s := range existing.Slots {
			if s.MagazineID != "" && mags[s.MagazineID] {
				return existing.Name, true
			}
		}
		for _, io := range existing.IOSlots {
			if io.MailboxID != "" && mbs[io.MailboxID] {
				return existing.Name, true
			}
		}
	}
	return "", false
}

// AddLogicalLibrary resolves libCfg (drive indices, magazine IDs, io-slot
// indices) against the live elements and adds it, rejecting a duplicate
// name or any element already claimed by another logical library.
func (l *Library) AddLogicalLibrary(libCfg config.LogicalLibraryConfig) (*LogicalLibrary, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, existing := range l.logicalLibs {
		if existing.Name == libCfg.Name {
			return nil, fmt.Errorf("logical library %s: %w", libCfg.Name, ErrAlreadyExists)
		}
	}
	if owner, conflict := l.exclusivityConflictLocked(libCfg, ""); conflict {
		return nil, fmt.Errorf("logical library %s: one or more elements already belong to %q: %w", libCfg.Name, owner, ErrAlreadyExists)
	}
	lib := l.resolveLogicalLibraryLocked(libCfg)
	l.logicalLibs = append(l.logicalLibs, lib)
	l.emit("logical-library-create", fmt.Sprintf("created logical library %q", lib.Name), map[string]string{"logical_library": lib.Name})
	return lib, nil
}

// UpdateLogicalLibrary replaces an existing logical library's element
// assignments and color, with the same exclusivity check as AddLogicalLibrary
// (excluding the library being updated from the conflict check).
func (l *Library) UpdateLogicalLibrary(name string, libCfg config.LogicalLibraryConfig) (*LogicalLibrary, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	idx := -1
	for i, existing := range l.logicalLibs {
		if existing.Name == name {
			idx = i
			break
		}
	}
	if idx == -1 {
		return nil, fmt.Errorf("logical library %s: %w", name, ErrNotFound)
	}
	if owner, conflict := l.exclusivityConflictLocked(libCfg, name); conflict {
		return nil, fmt.Errorf("logical library %s: one or more elements already belong to %q: %w", name, owner, ErrAlreadyExists)
	}
	libCfg.Name = name
	lib := l.resolveLogicalLibraryLocked(libCfg)
	l.logicalLibs[idx] = lib
	l.emit("logical-library-update", fmt.Sprintf("updated logical library %q", name), map[string]string{"logical_library": name})
	return lib, nil
}

// DeleteLogicalLibrary deletes a logical library by name.
func (l *Library) DeleteLogicalLibrary(name string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, lib := range l.logicalLibs {
		if lib.Name == name {
			l.logicalLibs = append(l.logicalLibs[:i], l.logicalLibs[i+1:]...)
			l.emit("logical-library-delete", fmt.Sprintf("deleted logical library %q", name), map[string]string{"logical_library": name})
			return nil
		}
	}
	return fmt.Errorf("logical library %s: %w", name, ErrNotFound)
}

// ListLogicalLibraries returns all logical libraries, with their elements
// deep-copied (see snapshotElementsLocked) so a caller marshaling the result
// outside l.mu can't race a concurrent Load/Unload/Move.
func (l *Library) ListLogicalLibraries() []*LogicalLibrary {
	l.mu.RLock()
	defer l.mu.RUnlock()
	_, _, _, byAddr, byIOAddr, byDriveIndex := l.snapshotElementsLocked()
	return l.snapshotLogicalLibsLocked(byAddr, byIOAddr, byDriveIndex)
}

// UnassignedElements reports the drives, slots, and I/O slots that don't
// belong to any logical library, so the Admin UI can surface them for
// reassignment. Elements are deep-copied, like Status()'.
func (l *Library) UnassignedElements() (drives []*Drive, slots []*Slot, ioslots []*IOSlot) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	assignedDrive := map[int]bool{}
	assignedSlot := map[int]bool{}
	assignedIOSlot := map[int]bool{}
	for _, lib := range l.logicalLibs {
		for _, d := range lib.Drives {
			assignedDrive[d.Index] = true
		}
		for _, s := range lib.Slots {
			assignedSlot[s.Address] = true
		}
		for _, io := range lib.IOSlots {
			assignedIOSlot[io.Address] = true
		}
	}
	allSlots, allIOSlots, allDrives, _, _, _ := l.snapshotElementsLocked()
	for _, d := range allDrives {
		if !assignedDrive[d.Index] {
			drives = append(drives, d)
		}
	}
	for _, s := range allSlots {
		if !assignedSlot[s.Address] {
			slots = append(slots, s)
		}
	}
	for _, io := range allIOSlots {
		if !assignedIOSlot[io.Address] {
			ioslots = append(ioslots, io)
		}
	}
	return drives, slots, ioslots
}

// elementInLogicalLibraryLocked reports whether the given element (resolved
// via volumeSlot's identity, or a drive index) belongs to the named logical
// library. Callers must hold l.mu.
func (l *Library) elementInLogicalLibraryLocked(logicalLibrary string, ref ElementRef) bool {
	for _, lib := range l.logicalLibs {
		if lib.Name != logicalLibrary {
			continue
		}
		switch ref.Kind {
		case KindSlot:
			for _, s := range lib.Slots {
				if s.Address == ref.Address {
					return true
				}
			}
		case KindIOSlot:
			for _, io := range lib.IOSlots {
				if io.Address == ref.Address {
					return true
				}
			}
		case KindDrive:
			for _, d := range lib.Drives {
				if d.Index == ref.Address {
					return true
				}
			}
		}
		return false
	}
	return false
}

// snapshotVolume deep-copies v (nil stays nil) - see snapshotElementsLocked.
func snapshotVolume(v *Volume) *Volume {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

func snapshotVolumes(vs []*Volume) []*Volume {
	out := make([]*Volume, 0, len(vs))
	for _, v := range vs {
		if v != nil {
			out = append(out, snapshotVolume(v))
		}
	}
	return out
}

// snapshotElementsLocked deep-copies the live slot/ioslot/drive structs and
// the volumes they hold, and returns lookup maps keyed by address/index so a
// logical library's own element lists can be rebuilt against the same copies
// (see snapshotLogicalLibsLocked).
//
// This is what makes Status()/LogicalLibraryStatus() genuinely the read-only
// snapshots their doc comments have always claimed to be. They used to hand
// back the live l.slots/l.ioslots/l.drives slices and their element
// pointers, which the HTTP handlers then JSON-marshal *after* releasing
// l.mu - so json.Marshal could be reading a Slot.Volume pointer or a
// Volume's WrittenBytes/Full fields at the exact moment Load/Unload/
// refreshVolumeSizeLocked was writing them. That's the same class of
// unsynchronized-read-of-mutable-state bug that already crashed this daemon
// once through the SNMP notifier's Event.Detail map (see
// cloneEventForNotify), just on the status path instead. Callers must hold
// l.mu.
func (l *Library) snapshotElementsLocked() (slots []*Slot, ioslots []*IOSlot, drives []*Drive, byAddr map[int]*Slot, byIOAddr map[int]*IOSlot, byDriveIndex map[int]*Drive) {
	slots = make([]*Slot, len(l.slots))
	byAddr = make(map[int]*Slot, len(l.slots))
	for i, s := range l.slots {
		c := *s
		c.Volume = snapshotVolume(s.Volume)
		slots[i] = &c
		byAddr[c.Address] = &c
	}
	ioslots = make([]*IOSlot, len(l.ioslots))
	byIOAddr = make(map[int]*IOSlot, len(l.ioslots))
	for i, io := range l.ioslots {
		c := *io
		c.Volume = snapshotVolume(io.Volume)
		ioslots[i] = &c
		byIOAddr[c.Address] = &c
	}
	drives = make([]*Drive, len(l.drives))
	byDriveIndex = make(map[int]*Drive, len(l.drives))
	for i, d := range l.drives {
		c := *d
		c.Volume = snapshotVolume(d.Volume)
		if d.Origin != nil {
			origin := *d.Origin
			c.Origin = &origin
		}
		drives[i] = &c
		byDriveIndex[c.Index] = &c
	}
	return slots, ioslots, drives, byAddr, byIOAddr, byDriveIndex
}

// snapshotLogicalLibsLocked rebuilds every logical library against the
// already-copied elements from snapshotElementsLocked, preserving the
// invariant that a logical library's element lists hold the *same* pointers
// as the top-level slices in the same Status (resolveLogicalLibraryLocked
// guarantees this for the live model; a snapshot has to keep it, since the
// web UI cross-references the two by identity). Callers must hold l.mu.
func (l *Library) snapshotLogicalLibsLocked(byAddr map[int]*Slot, byIOAddr map[int]*IOSlot, byDriveIndex map[int]*Drive) []*LogicalLibrary {
	out := make([]*LogicalLibrary, 0, len(l.logicalLibs))
	for _, lib := range l.logicalLibs {
		out = append(out, l.snapshotLogicalLibLocked(lib, byAddr, byIOAddr, byDriveIndex))
	}
	return out
}

func (l *Library) snapshotLogicalLibLocked(lib *LogicalLibrary, byAddr map[int]*Slot, byIOAddr map[int]*IOSlot, byDriveIndex map[int]*Drive) *LogicalLibrary {
	c := &LogicalLibrary{
		Name:    lib.Name,
		Color:   lib.Color,
		Drives:  make([]*Drive, 0, len(lib.Drives)),
		Slots:   make([]*Slot, 0, len(lib.Slots)),
		IOSlots: make([]*IOSlot, 0, len(lib.IOSlots)),
	}
	for _, d := range lib.Drives {
		if cd, ok := byDriveIndex[d.Index]; ok {
			c.Drives = append(c.Drives, cd)
		}
	}
	for _, s := range lib.Slots {
		if cs, ok := byAddr[s.Address]; ok {
			c.Slots = append(c.Slots, cs)
		}
	}
	for _, io := range lib.IOSlots {
		if cio, ok := byIOAddr[io.Address]; ok {
			c.IOSlots = append(c.IOSlots, cio)
		}
	}
	return c
}

// Status returns a read-only snapshot of the whole library for API/UI use.
// Every element and volume in it is a copy - see snapshotElementsLocked.
func (l *Library) Status() Status {
	l.mu.RLock()
	defer l.mu.RUnlock()
	slots, ioslots, drives, byAddr, byIOAddr, byDriveIndex := l.snapshotElementsLocked()
	return Status{
		Name:           l.cfg.Library.Name,
		Slots:          slots,
		IOSlots:        ioslots,
		Drives:         drives,
		OutsideVolumes: snapshotVolumes(l.outside),
		OffsiteVolumes: snapshotVolumes(l.offsite),
		OffsiteEnabled: l.cfg.Library.OffsiteLocation,
		Doors: DoorStatus{
			OpenMagazines: l.openMagazinesLocked(),
			OpenMailboxes: l.openMailboxesLocked(),
			Phases:        l.DoorPhases(),
		},
		LogicalLibs:            l.snapshotLogicalLibsLocked(byAddr, byIOAddr, byDriveIndex),
		RoboticFault:           l.roboticFault,
		ArmState:               l.ArmState(),
		CleaningEnabled:        l.cleaningEnabled,
		CleaningMountThreshold: l.cleaningMountThreshold,
		CleaningMaxUses:        l.cleaningMaxUses,
		MagazinePINRequired:    l.magazinePINHash != "",
		MailboxPINRequired:     l.mailboxPINRequiredLocked(),
	}
}

// mailboxPINRequiredLocked returns a sparse mailbox-ID -> true map for
// every mailbox that currently has its own PIN configured, or nil if none
// do. Callers must hold l.mu.
func (l *Library) mailboxPINRequiredLocked() map[string]bool {
	if len(l.mailboxPINHash) == 0 {
		return nil
	}
	out := make(map[string]bool, len(l.mailboxPINHash))
	for id := range l.mailboxPINHash {
		out[id] = true
	}
	return out
}

// Events returns the most recent events, newest first. Each event's Detail
// map is deep-copied, not shared: the caller (an HTTP handler) marshals the
// result after l.mu has been released, while AnnotateEventsSince keeps
// writing into the stored events' own Detail maps under the lock - handing
// out the live map is exactly the "concurrent map iteration and map write"
// crash that already took this daemon down once via the SNMP notifier (see
// cloneEventForNotify, which fixed the same bug on the notify path).
func (l *Library) Events() []Event {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]Event, len(l.events))
	for i, e := range l.events {
		out[i] = cloneEventForNotify(e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out
}

func (l *Library) emit(evtType, message string, detail map[string]string) {
	e := CanonicalizeEvent(Event{Type: evtType, Message: message, Detail: detail})
	l.events = append(l.events, e)
	if len(l.events) > maxEvents {
		l.events = l.events[len(l.events)-maxEvents:]
	}
	if l.notifier != nil {
		go l.notifier.Notify(cloneEventForNotify(e))
	}
}

// cloneEventForNotify deep-copies e.Detail before e escapes to anything
// that will read it without holding l.mu. Two such paths exist: a notifier
// goroutine (fire-and-forget, no lock at all) and Events()' return value
// (marshaled by an HTTP handler after the lock is released). Meanwhile
// AnnotateEventsSince mutates the stored event's Detail map in place under
// l.mu - without this clone, both sides can end up ranging over/writing the
// same underlying map concurrently ("concurrent map iteration and map
// write", a real crash this daemon hit in production).
func cloneEventForNotify(e Event) Event {
	if e.Detail != nil {
		detail := make(map[string]string, len(e.Detail))
		for k, v := range e.Detail {
			detail[k] = v
		}
		e.Detail = detail
	}
	return e
}

// RecordEvent appends an externally generated event (for example from API
// auth/config handlers), emits a trap, and persists the updated event log.
func (l *Library) RecordEvent(evt Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := CanonicalizeEvent(evt)
	l.events = append(l.events, e)
	if len(l.events) > maxEvents {
		l.events = l.events[len(l.events)-maxEvents:]
	}
	if l.notifier != nil {
		go l.notifier.Notify(cloneEventForNotify(e))
	}
	l.saveLocked()
}

// SetPhaseNotifier registers pn to receive live door-phase transitions (see
// setDoorPhase). Optional - a nil notifier simply means nobody is told.
func (l *Library) SetPhaseNotifier(pn PhaseNotifier) {
	l.phaseMu.Lock()
	defer l.phaseMu.Unlock()
	l.phaseNotifier = pn
}

// setDoorPhase records that kind ("magazine" or "mailbox") id is now in
// phase, or clears it when phase is "". Deliberately uses its own mutex
// instead of l.mu, which the four door methods hold for their entire
// sleep - a phase transition must be observable (via DoorPhases/Status)
// while that sleep is still in progress, not only after it returns.
func (l *Library) setDoorPhase(kind, id, phase string) {
	l.phaseMu.Lock()
	key := kind + ":" + id
	if phase == "" {
		delete(l.doorPhases, key)
	} else {
		l.doorPhases[key] = phase
	}
	pn := l.phaseNotifier
	l.phaseMu.Unlock()
	// Synchronous, unlike emit/RecordEvent's "go l.notifier.Notify(e)" -
	// see PhaseNotifier's doc comment for why ordering matters here.
	if pn != nil {
		pn.NotifyPhase(kind, id, phase)
	}
}

// DoorPhases returns a snapshot of every door/mailbox currently mid-
// open/close/scan, keyed "magazine:<id>"/"mailbox:<id>" -> phase. Safe to
// call concurrently with an in-progress Open/CloseStorageDoor/IODoor call
// (never blocks on l.mu).
func (l *Library) DoorPhases() map[string]string {
	l.phaseMu.Lock()
	defer l.phaseMu.Unlock()
	out := make(map[string]string, len(l.doorPhases))
	for k, v := range l.doorPhases {
		out[k] = v
	}
	return out
}

// armPositionFor converts an ElementRef into the equivalent ArmPosition -
// ElementRef.Kind's string values ("slot"/"ioslot"/"drive") are exactly
// the non-parked ArmPosition.Kind values.
func armPositionFor(ref ElementRef) ArmPosition {
	return ArmPosition{Kind: string(ref.Kind), Address: ref.Address}
}

// currentArmStateLocked computes the arm's current reported state:
// armLastPosition (the last real Move/Load/Unload destination), unless
// armOpenDoors > 0 - in which case the reported position is always
// ArmPositionParked regardless of armLastPosition, a derived, continuous
// value, not a one-time transition, so it stays correct even if an
// unrelated Move/Load/Unload runs on a different magazine/mailbox while
// this door is still open, and automatically reverts once every door is
// closed again - or armLastPosition.Kind is still "" (the arm has never
// moved since the daemon started, e.g. right after a fresh install):
// an unknown position is treated as parked too, since a real arm starts
// docked at a home/parked position, not floating at some arbitrary
// unaddressable "nowhere". Callers must hold phaseMu.
func (l *Library) currentArmStateLocked() ArmState {
	pos := l.armLastPosition
	if l.armOpenDoors > 0 || pos.Kind == "" {
		pos = ArmPosition{Kind: ArmPositionParked}
	}
	return ArmState{Busy: l.armBusy, Position: pos}
}

// notifyArmLocked computes the current derived ArmState and pushes it
// (plus an optional new narration step) to the live SSE channel, mirroring
// setDoorPhase's lock/mutate/capture/unlock/notify shape. Callers must
// hold phaseMu on entry; it is released before returning.
func (l *Library) notifyArmLocked(step ArmStep) {
	state := l.currentArmStateLocked()
	pn := l.phaseNotifier
	l.phaseMu.Unlock()
	if pn != nil {
		pn.NotifyArm(state, step)
	}
}

// setArmBusy records whether the arm is currently performing a mechanical
// operation (Move/Load/Unload, or a magazine's post-close scan) - see
// ArmState's doc comment. Works identically regardless of which caller
// (browser UI, gotochangerctl, or a real Bareos job via
// gotochanger-changer) triggered the operation, since it's set by the
// Library method itself, not by any specific transport.
func (l *Library) setArmBusy(busy bool) {
	l.phaseMu.Lock()
	l.armBusy = busy
	l.notifyArmLocked(ArmStep{})
}

// setArmPosition records the arm's last real destination. Never called
// with ArmPositionParked directly - see setArmDoorsOpenDelta, which is
// the only thing that can make the arm report as parked.
func (l *Library) setArmPosition(pos ArmPosition) {
	l.phaseMu.Lock()
	l.armLastPosition = pos
	l.notifyArmLocked(ArmStep{})
}

// setArmDoorsOpenDelta adjusts the count of currently-open magazine/
// mailbox doors, which is what currentArmStateLocked checks to decide
// whether to report the arm as parked - see OpenStorageDoor/
// CloseStorageDoor/OpenIODoor/CloseIODoor. Also records a live-only
// narration step exactly on the edge of the first door opening (nothing
// was open before, something is now) - not on every individual door
// open, so opening a second door while a first is already open doesn't
// spam a redundant "parking" step. Deliberately no narration on the
// closing edge ("resuming from parked...") - found not useful in
// practice, the next real Move/Load/Unload's own narration already
// shows where the arm goes next.
func (l *Library) setArmDoorsOpenDelta(delta int) {
	l.phaseMu.Lock()
	wasOpen := l.armOpenDoors > 0
	l.armOpenDoors += delta
	isOpen := l.armOpenDoors > 0
	var step ArmStep
	if !wasOpen && isOpen {
		step = l.appendArmStepLocked("moving to parked position")
	}
	l.notifyArmLocked(step)
}

// appendArmStepLocked appends one atomic, live-only narration entry to
// the capped ring buffer - never persisted, never SNMP'd, exactly like a
// door phase transition. Callers must hold phaseMu.
func (l *Library) appendArmStepLocked(msg string) ArmStep {
	step := ArmStep{Time: time.Now().UTC(), Message: msg}
	l.armSteps = append(l.armSteps, step)
	if len(l.armSteps) > maxArmSteps {
		l.armSteps = l.armSteps[len(l.armSteps)-maxArmSteps:]
	}
	return step
}

// recordArmStep is appendArmStepLocked plus the phaseMu lock/notify
// dance, for the common case of a narration step with no accompanying
// position change.
func (l *Library) recordArmStep(msg string) {
	l.phaseMu.Lock()
	step := l.appendArmStepLocked(msg)
	l.notifyArmLocked(step)
}

// recordArmStepAndPosition is recordArmStep plus an armLastPosition
// update, done atomically under one phaseMu section so only a single
// "arm" SSE push carries both - used where a narration step and a real
// position change happen together, e.g. each slot the arm passes while
// scanning a magazine (see CloseStorageDoor): without this, the position
// the arm settles on right before a door closes could otherwise be lost
// to a stale earlier value, since armLastPosition would never actually
// be updated to reflect where the arm's last real movement (the scan)
// left it.
func (l *Library) recordArmStepAndPosition(msg string, pos ArmPosition) {
	l.phaseMu.Lock()
	l.armLastPosition = pos
	step := l.appendArmStepLocked(msg)
	l.notifyArmLocked(step)
}

// ArmState returns the robotic arm's current live state. Safe to call
// concurrently with an in-progress Move/Load/Unload/door operation
// (never blocks on l.mu) - mirrors DoorPhases.
func (l *Library) ArmState() ArmState {
	l.phaseMu.Lock()
	defer l.phaseMu.Unlock()
	return l.currentArmStateLocked()
}

// ArmSteps returns a snapshot of the recent live-only arm-narration
// entries, newest-last (append order) - used to seed a freshly
// (re)connected SSE client's activity panel. Never blocks on l.mu.
func (l *Library) ArmSteps() []ArmStep {
	l.phaseMu.Lock()
	defer l.phaseMu.Unlock()
	return append([]ArmStep(nil), l.armSteps...)
}

// reconcileArmDoorsLocked recomputes armOpenDoors from the persisted
// stDoorOpen/ioDoorOpen maps - called after New()'s restore and after
// Reconfigure's buildTopologyLocked, since armOpenDoors itself is never
// persisted (see the Library struct's doc comment on these fields) but
// stDoorOpen/ioDoorOpen genuinely are, so a restart or topology reload
// with a door already open must not report the arm as un-parked just
// because armOpenDoors defaults to 0. Callers must hold l.mu; safe to
// call with or without phaseMu (it takes phaseMu itself).
func (l *Library) reconcileArmDoorsLocked() {
	open := 0
	for _, isOpen := range l.stDoorOpen {
		if isOpen {
			open++
		}
	}
	for _, isOpen := range l.ioDoorOpen {
		if isOpen {
			open++
		}
	}
	l.phaseMu.Lock()
	l.armOpenDoors = open
	l.notifyArmLocked(ArmStep{})
}

// AnnotateEventsSince backfills actor/source/detail on events emitted at or
// after since. Existing values are preserved.
func (l *Library) AnnotateEventsSince(since time.Time, actor, source string, detail map[string]string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	updated := false
	for i := range l.events {
		e := &l.events[i]
		if e.Time.Before(since) {
			continue
		}
		if actor != "" && e.Actor == "" {
			e.Actor = actor
			updated = true
		}
		if source != "" && e.Source == "" {
			e.Source = source
			updated = true
		}
		if len(detail) == 0 {
			continue
		}
		if e.Detail == nil {
			e.Detail = map[string]string{}
		}
		for k, v := range detail {
			if v == "" {
				continue
			}
			if _, ok := e.Detail[k]; ok {
				continue
			}
			e.Detail[k] = v
			updated = true
		}
	}
	if updated {
		l.saveLocked()
	}
}

func (l *Library) saveLocked() {
	if l.persist == nil {
		return
	}
	if err := l.persist.Save(l.stateLocked()); err != nil {
		l.emit("persist-error", "failed to persist library state", map[string]string{"error": err.Error()})
	}
}

// Errors returned by element lookups and validations.
var (
	ErrNotFound                = errors.New("element not found")
	ErrEmpty                   = errors.New("element is empty")
	ErrFull                    = errors.New("element is already full")
	ErrDriveFault              = errors.New("drive is in fault state")
	ErrRoboticFault            = errors.New("robotic arm is in fault state")
	ErrInvalidRoboticFaultKind = errors.New("invalid robotic fault kind")
	ErrInvalidBarcode          = errors.New("invalid tape barcode")
	ErrBarcodeExists           = errors.New("barcode already in use")
	ErrInvalidTarget           = errors.New("invalid move target")
	ErrDoorClosed              = errors.New("door is closed")
	ErrOutsideOnly             = errors.New("volume is not outside the library")
	ErrAlreadyExists           = errors.New("already exists")
	ErrOutsideLogicalLibrary   = errors.New("element does not belong to the requested logical library")
	ErrUnknownTapeSet          = errors.New("tape set references an unknown tape type")
	ErrCleaningTapeExpired     = errors.New("cleaning tape has expired")
	ErrCleaningPoolFull        = errors.New("cleaning tape pool is full")
	ErrCleaningTapeUnavailable = errors.New("no usable cleaning tape available")
	ErrPINRequired             = errors.New("a PIN is required to perform this action")
	ErrInvalidPIN              = errors.New("incorrect PIN")
	ErrOffsiteDisabled         = errors.New("offsite vaulting is not enabled")
	ErrVolumeNotAccessible     = errors.New("volume is not physically accessible (mounted in a drive, or behind a closed magazine/mailbox door)")
)

func (l *Library) findSlot(addr int) (*Slot, error) {
	for _, s := range l.slots {
		if s.Address == addr {
			return s, nil
		}
	}
	return nil, fmt.Errorf("slot %d: %w", addr, ErrNotFound)
}

func (l *Library) findIOSlot(addr int) (*IOSlot, error) {
	for _, s := range l.ioslots {
		if s.Address == addr {
			return s, nil
		}
	}
	return nil, fmt.Errorf("ioslot %d: %w", addr, ErrNotFound)
}

func (l *Library) findDrive(idx int) (*Drive, error) {
	for _, d := range l.drives {
		if d.Index == idx {
			return d, nil
		}
	}
	return nil, fmt.Errorf("drive %d: %w", idx, ErrNotFound)
}

func (l *Library) findOutside(label string) (*Volume, int) {
	for i, v := range l.outside {
		if v != nil && v.Barcode == label {
			return v, i
		}
	}
	return nil, -1
}

func (l *Library) containsBarcodeLocked(label string) bool {
	for _, s := range l.slots {
		if s.Volume != nil && s.Volume.Barcode == label {
			return true
		}
	}
	for _, io := range l.ioslots {
		if io.Volume != nil && io.Volume.Barcode == label {
			return true
		}
	}
	for _, d := range l.drives {
		if d.Volume != nil && d.Volume.Barcode == label {
			return true
		}
	}
	for _, v := range l.outside {
		if v != nil && v.Barcode == label {
			return true
		}
	}
	for _, v := range l.offsite {
		if v != nil && v.Barcode == label {
			return true
		}
	}
	return false
}

// findAccessibleVolumeForWriteProtectLocked finds bc across every location it
// might currently be in and reports whether an operator could physically
// reach it right now to flip a write-protect tab - mirroring a real tape:
// reachable while sitting outside the library or offsite, or in a
// storage/mailbox slot whose door is currently open, but never while
// mounted in a drive (sealed shut) or sitting behind a closed magazine/
// mailbox door. Used by SetVolumeWriteProtect only - other operations that
// need a plain barcode lookup regardless of location use
// containsBarcodeLocked (existence only) or a location-scoped finder like
// findOutside/findOffsite.
func (l *Library) findAccessibleVolumeForWriteProtectLocked(bc string) (*Volume, error) {
	for _, s := range l.slots {
		if s.Volume != nil && s.Volume.Barcode == bc {
			if !l.stDoorOpen[s.MagazineID] {
				return nil, fmt.Errorf("volume %q: %w", bc, ErrVolumeNotAccessible)
			}
			return s.Volume, nil
		}
	}
	for _, io := range l.ioslots {
		if io.Volume != nil && io.Volume.Barcode == bc {
			if !l.ioDoorOpen[io.MailboxID] {
				return nil, fmt.Errorf("volume %q: %w", bc, ErrVolumeNotAccessible)
			}
			return io.Volume, nil
		}
	}
	for _, d := range l.drives {
		if d.Volume != nil && d.Volume.Barcode == bc {
			return nil, fmt.Errorf("volume %q: %w", bc, ErrVolumeNotAccessible)
		}
	}
	for _, v := range l.outside {
		if v != nil && v.Barcode == bc {
			return v, nil
		}
	}
	for _, v := range l.offsite {
		if v != nil && v.Barcode == bc {
			return v, nil
		}
	}
	return nil, fmt.Errorf("volume %q: %w", bc, ErrNotFound)
}

// outsideVolumesSnapshotLocked deep-copies the outside inventory - like
// every other volume getter here, the result is marshaled by an HTTP handler
// after l.mu is released, so it must not alias volumes the library keeps
// mutating (see snapshotElementsLocked). Callers must hold l.mu.
func (l *Library) outsideVolumesSnapshotLocked() []*Volume {
	return snapshotVolumes(l.outside)
}

func (l *Library) findOffsite(label string) (*Volume, int) {
	for i, v := range l.offsite {
		if v != nil && v.Barcode == label {
			return v, i
		}
	}
	return nil, -1
}

// volumeAt returns the *Volume currently sitting at ref, and a setter to
// clear/replace it, unifying slot/ioslot/drive handling for Move/Load/Unload.
func (l *Library) volumeSlot(ref ElementRef) (get func() *Volume, set func(*Volume), label string, err error) {
	switch ref.Kind {
	case KindSlot:
		s, e := l.findSlot(ref.Address)
		if e != nil {
			return nil, nil, "", e
		}
		return func() *Volume { return s.Volume }, func(v *Volume) { s.Volume = v }, fmt.Sprintf("slot %d", ref.Address), nil
	case KindIOSlot:
		s, e := l.findIOSlot(ref.Address)
		if e != nil {
			return nil, nil, "", e
		}
		return func() *Volume { return s.Volume }, func(v *Volume) { s.Volume = v }, fmt.Sprintf("ioslot %d", ref.Address), nil
	case KindDrive:
		d, e := l.findDrive(ref.Address)
		if e != nil {
			return nil, nil, "", e
		}
		return func() *Volume { return d.Volume }, func(v *Volume) { d.Volume = v }, fmt.Sprintf("drive %d", ref.Address), nil
	default:
		return nil, nil, "", fmt.Errorf("%w: unknown kind %q", ErrInvalidTarget, ref.Kind)
	}
}

// Move relocates a volume from one element to another (slot<->slot,
// slot<->ioslot, ioslot<->ioslot). Use Load/Unload for drive interaction,
// which additionally manage the Bareos-facing device symlink. If
// logicalLibrary is non-empty, both from and to must belong to that logical
// library, or ErrOutsideLogicalLibrary is returned - this is how a Bareos
// Autochanger bound to one logical library (via gotochanger-changer's
// --logical-library flag) is kept from touching another library's media.
// An empty logicalLibrary is unscoped (the trusted local socket's default).
func (l *Library) Move(from, to ElementRef, logicalLibrary string) error {
	if from.Kind == KindDrive || to.Kind == KindDrive {
		return fmt.Errorf("%w: use Load/Unload for drive elements", ErrInvalidTarget)
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	if logicalLibrary != "" {
		if !l.elementInLogicalLibraryLocked(logicalLibrary, from) || !l.elementInLogicalLibraryLocked(logicalLibrary, to) {
			return fmt.Errorf("logical library %s: %w", logicalLibrary, ErrOutsideLogicalLibrary)
		}
	}

	if l.roboticFault.Active {
		return fmt.Errorf("robotic arm: %s: %w", l.roboticFault.Kind, ErrRoboticFault)
	}

	getFrom, setFrom, fromLabel, err := l.volumeSlot(from)
	if err != nil {
		return err
	}
	getTo, setTo, toLabel, err := l.volumeSlot(to)
	if err != nil {
		return err
	}
	vol := getFrom()
	if vol == nil {
		return fmt.Errorf("%s: %w", fromLabel, ErrEmpty)
	}
	if getTo() != nil {
		return fmt.Errorf("%s: %w", toLabel, ErrFull)
	}

	l.setArmBusy(true)
	defer l.setArmBusy(false)

	// Plain bracketing message, matching this codebase's convention for
	// every other coarse Move/Load/Unload started/success event - this
	// one stays audited (persisted + SNMP). The atomic "moving to"/
	// "grabbed"/"placed" narration below is deliberately NOT emitted
	// here: it's live-only (see recordArmStep), never logged or SNMP'd,
	// exactly like a door phase transition.
	l.emit("move-started", fmt.Sprintf("moving volume %q from %s to %s", vol.Barcode, fromLabel, toLabel),
		map[string]string{"volume": vol.Barcode, "from": fromLabel, "to": toLabel})

	half := l.robotMoveTapeLatency / 2
	l.recordArmStep(fmt.Sprintf("moving to %s", fromLabel))
	if half > 0 {
		time.Sleep(half)
	}
	l.recordArmStep(fmt.Sprintf("grabbed tape %s from %s", vol.Barcode, fromLabel))
	l.recordArmStep(fmt.Sprintf("moving to %s", toLabel))
	if rem := l.robotMoveTapeLatency - half; rem > 0 {
		time.Sleep(rem)
	}
	l.recordArmStep(fmt.Sprintf("placed tape %s into %s", vol.Barcode, toLabel))

	setFrom(nil)
	setTo(vol)
	l.setArmPosition(armPositionFor(to))
	l.emit("move", fmt.Sprintf("moved volume %q from %s to %s", vol.Barcode, fromLabel, toLabel),
		map[string]string{"volume": vol.Barcode, "from": fromLabel, "to": toLabel})
	l.saveLocked()
	return nil
}

// OpenIODoor marks the given mailbox's I/O door as open for physical
// load/pickup actions. Each mailbox's door is independent of every other
// mailbox's, mirroring OpenStorageDoor's per-magazine behavior. pin is
// checked against that mailbox's own configured PIN, if any (see
// checkMailboxPINLocked) - checked on every call, even one that's about to
// no-op because the door is already open, so a PIN can never be bypassed by
// calling Open twice.
func (l *Library) OpenIODoor(mailboxID, pin string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.mailboxExistsLocked(mailboxID) {
		return fmt.Errorf("mailbox %q: %w", mailboxID, ErrNotFound)
	}
	if err := l.checkMailboxPINLocked(mailboxID, pin); err != nil {
		return err
	}
	if l.ioDoorOpen[mailboxID] {
		return nil
	}
	l.setDoorPhase("mailbox", mailboxID, "opening")
	defer l.setDoorPhase("mailbox", mailboxID, "")
	if l.doorActionLatency > 0 {
		time.Sleep(l.doorActionLatency)
	}
	l.ioDoorOpen[mailboxID] = true
	l.setArmDoorsOpenDelta(+1)
	l.emit("io-door", fmt.Sprintf("opened IO mail slot door for mailbox %q", mailboxID), map[string]string{"mailbox": mailboxID})
	l.saveLocked()
	return nil
}

// CloseIODoor applies staged I/O actions scoped to mailboxID then closes
// that mailbox's door.
func (l *Library) CloseIODoor(mailboxID string, actions []DoorAction) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.ioDoorOpen[mailboxID] {
		return ErrDoorClosed
	}
	l.setDoorPhase("mailbox", mailboxID, "closing")
	defer l.setDoorPhase("mailbox", mailboxID, "")
	if l.doorActionLatency > 0 {
		time.Sleep(l.doorActionLatency)
	}
	if err := l.applyIOActionsLocked(mailboxID, actions); err != nil {
		return err
	}
	delete(l.ioDoorOpen, mailboxID)
	l.setArmDoorsOpenDelta(-1)
	l.emit("io-door", fmt.Sprintf("closed IO mail slot door for mailbox %q", mailboxID), map[string]string{"mailbox": mailboxID, "actions": fmt.Sprint(len(actions))})
	l.saveLocked()
	return nil
}

// OpenStorageDoor marks the given magazine's storage door as open for
// physical load/pickup actions. Each magazine's door is independent of
// every other magazine's. pin is checked against the single global
// magazine PIN, if one is configured (see checkMagazinePINLocked) -
// checked on every call, even one that's about to no-op because the door
// is already open, so a PIN can never be bypassed by calling Open twice.
func (l *Library) OpenStorageDoor(magazineID, pin string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.magazineExistsLocked(magazineID) {
		return fmt.Errorf("magazine %q: %w", magazineID, ErrNotFound)
	}
	if err := l.checkMagazinePINLocked(pin); err != nil {
		return err
	}
	if l.stDoorOpen[magazineID] {
		return nil
	}
	l.setDoorPhase("magazine", magazineID, "opening")
	defer l.setDoorPhase("magazine", magazineID, "")
	if l.doorActionLatency > 0 {
		time.Sleep(l.doorActionLatency)
	}
	l.stDoorOpen[magazineID] = true
	l.setArmDoorsOpenDelta(+1)
	l.emit("storage-door", fmt.Sprintf("opened storage door for magazine %q", magazineID), map[string]string{"magazine": magazineID})
	l.saveLocked()
	return nil
}

// CloseStorageDoor applies staged storage actions scoped to magazineID
// then closes that magazine's door.
func (l *Library) CloseStorageDoor(magazineID string, actions []DoorAction) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.stDoorOpen[magazineID] {
		return ErrDoorClosed
	}
	l.setDoorPhase("magazine", magazineID, "closing")
	defer l.setDoorPhase("magazine", magazineID, "")
	if l.doorActionLatency > 0 {
		time.Sleep(l.doorActionLatency)
	}
	if err := l.applyStorageActionsLocked(magazineID, actions); err != nil {
		return err
	}
	l.emit("storage-door", fmt.Sprintf("closed storage door for magazine %q", magazineID), map[string]string{"magazine": magazineID, "actions": fmt.Sprint(len(actions))})
	// The door is now physically shut, so the arm re-scans the magazine's
	// contents (moves along it, then reads barcodes) before it's usable
	// again - see the "Tape barcodes and tape sets"/latency rework notes.
	// This is genuine arm movement (unlike the door open/close mechanism
	// itself), so it's bracketed as busy and its per-slot narration is
	// live-only, exactly like Move/Load/Unload's granular steps - the
	// coarse "scanned magazine ... contents" event below stays audited.
	l.setDoorPhase("magazine", magazineID, "scanning")
	l.setArmBusy(true)
	if l.robotMoveScanLatency > 0 {
		time.Sleep(l.robotMoveScanLatency)
	}

	// Divide the magazine's total scan time evenly across its own slots
	// and record one live-only "scanning slot N: <status>" step per slot
	// as the arm reaches it, instead of one lump sleep followed by a
	// single summary line - this is what makes the activity log read as
	// the arm actually passing along the magazine, not just "some time
	// passed".
	var magSlots []*Slot
	for _, s := range l.slots {
		if s.MagazineID == magazineID {
			magSlots = append(magSlots, s)
		}
	}
	if len(magSlots) > 0 {
		// perSlot is only ever used to sleep - whether the arm's position
		// gets tracked/narrated must NOT depend on whether latency
		// simulation happens to be enabled (magazineScanLatency > 0).
		// Gating the loop itself on that (as an earlier version of this
		// code did) meant the scan - and so armLastPosition - silently
		// never updated at all with latency disabled, which is the
		// default on a fresh install.
		var perSlot time.Duration
		if l.magazineScanLatency > 0 {
			perSlot = l.magazineScanLatency / time.Duration(len(magSlots))
		}
		for _, s := range magSlots {
			if perSlot > 0 {
				time.Sleep(perSlot)
			}
			status := "empty"
			if s.Volume != nil {
				status = fmt.Sprintf("occupied (%s)", s.Volume.Barcode)
			}
			// Updates armLastPosition to this slot as the arm reaches it
			// (see recordArmStepAndPosition's doc comment) - by the time
			// the loop finishes, armLastPosition correctly holds the last
			// slot scanned, not whatever it was before the magazine was
			// opened. The displayed position still reports "parked" while
			// this magazine's door remains open (armOpenDoors > 0,
			// currentArmStateLocked), so this has no visible effect until
			// the door actually closes.
			l.recordArmStepAndPosition(fmt.Sprintf("scanning slot %d: %s", s.Address, status), ArmPosition{Kind: "slot", Address: s.Address})
		}
	} else if l.magazineScanLatency > 0 {
		time.Sleep(l.magazineScanLatency)
	}
	l.setArmBusy(false)
	delete(l.stDoorOpen, magazineID)
	l.setArmDoorsOpenDelta(-1)
	l.emit("storage-scan", fmt.Sprintf("scanned magazine %q contents after door close", magazineID), map[string]string{"magazine": magazineID})
	l.saveLocked()
	return nil
}

// magazineExistsLocked reports whether any slot belongs to magazineID.
// Callers must hold l.mu.
func (l *Library) magazineExistsLocked(magazineID string) bool {
	for _, s := range l.slots {
		if s.MagazineID == magazineID {
			return true
		}
	}
	return false
}

// mailboxExistsLocked reports whether any I/O slot belongs to mailboxID.
// Callers must hold l.mu.
func (l *Library) mailboxExistsLocked(mailboxID string) bool {
	for _, io := range l.ioslots {
		if io.MailboxID == mailboxID {
			return true
		}
	}
	return false
}

// checkMagazinePINLocked validates pin against the single global magazine
// PIN, if one is configured - presence-implies-protection, so an empty
// magazinePINHash means every magazine opens freely, no PIN required at
// all. Callers must hold l.mu.
func (l *Library) checkMagazinePINLocked(pin string) error {
	if l.magazinePINHash == "" {
		return nil
	}
	if pin == "" {
		return ErrPINRequired
	}
	if !secrethash.Verify(pin, l.magazinePINHash) {
		return ErrInvalidPIN
	}
	return nil
}

// checkMailboxPINLocked is the per-mailbox equivalent of
// checkMagazinePINLocked: each mailbox has its own independent PIN, or
// none - a mailbox absent from mailboxPINHash opens freely. Callers must
// hold l.mu.
func (l *Library) checkMailboxPINLocked(mailboxID, pin string) error {
	hash, configured := l.mailboxPINHash[mailboxID]
	if !configured {
		return nil
	}
	if pin == "" {
		return ErrPINRequired
	}
	if !secrethash.Verify(pin, hash) {
		return ErrInvalidPIN
	}
	return nil
}

// openMagazinesLocked returns the currently-open magazine IDs, sorted for
// deterministic JSON output. Callers must hold l.mu.
func (l *Library) openMagazinesLocked() []string {
	open := make([]string, 0, len(l.stDoorOpen))
	for magID, isOpen := range l.stDoorOpen {
		if isOpen {
			open = append(open, magID)
		}
	}
	sort.Strings(open)
	return open
}

// openMailboxesLocked returns the currently-open mailbox IDs, sorted for
// deterministic JSON output. Callers must hold l.mu.
func (l *Library) openMailboxesLocked() []string {
	open := make([]string, 0, len(l.ioDoorOpen))
	for mbID, isOpen := range l.ioDoorOpen {
		if isOpen {
			open = append(open, mbID)
		}
	}
	sort.Strings(open)
	return open
}

func (l *Library) applyIOActionsLocked(mailboxID string, actions []DoorAction) error {
	ios := map[int]*Volume{}
	for _, io := range l.ioslots {
		ios[io.Address] = io.Volume
	}
	outside := map[string]*Volume{}
	for _, v := range l.outside {
		outside[v.Barcode] = v
	}

	for i, a := range actions {
		simErr := func(msg string) error {
			return fmt.Errorf("io action %d: %s", i+1, msg)
		}
		switch a.Action {
		case "load":
			if a.Barcode == "" {
				return simErr("label is required for load")
			}
			io, err := l.findIOSlot(a.Address)
			if err != nil {
				return simErr(err.Error())
			}
			if io.MailboxID != mailboxID {
				return simErr(fmt.Sprintf("ioslot %d does not belong to mailbox %q", a.Address, mailboxID))
			}
			vol, ok := outside[a.Barcode]
			if !ok {
				return simErr(fmt.Sprintf("outside volume %q not found", a.Barcode))
			}
			if ios[a.Address] != nil {
				return simErr(fmt.Sprintf("ioslot %d: %s", a.Address, ErrFull))
			}
			ios[a.Address] = vol
			delete(outside, a.Barcode)
		case "pickup":
			io, err := l.findIOSlot(a.Address)
			if err != nil {
				return simErr(err.Error())
			}
			if io.MailboxID != mailboxID {
				return simErr(fmt.Sprintf("ioslot %d does not belong to mailbox %q", a.Address, mailboxID))
			}
			vol := ios[a.Address]
			if vol == nil {
				return simErr(fmt.Sprintf("ioslot %d: %s", a.Address, ErrEmpty))
			}
			if _, exists := outside[vol.Barcode]; exists {
				return simErr(fmt.Sprintf("outside volume %q already exists", vol.Barcode))
			}
			outside[vol.Barcode] = vol
			ios[a.Address] = nil
		default:
			return simErr(fmt.Sprintf("unknown action %q", a.Action))
		}
	}

	// Rebuilt from a map, so the iteration order is random - sort by barcode
	// afterwards to keep the outside inventory (and the dashboard's "Outside
	// Tapes" card built from it) in a stable order instead of reshuffling on
	// every door close.
	l.outside = l.outside[:0]
	for _, v := range outside {
		l.outside = append(l.outside, v)
	}
	sort.Slice(l.outside, func(i, j int) bool { return l.outside[i].Barcode < l.outside[j].Barcode })
	for _, io := range l.ioslots {
		io.Volume = ios[io.Address]
	}
	for _, a := range actions {
		if a.Action == "load" {
			l.emit("io-load", fmt.Sprintf("loaded outside volume %q into ioslot %d", a.Barcode, a.Address), map[string]string{"volume": a.Barcode, "ioslot": fmt.Sprint(a.Address)})
		} else {
			l.emit("io-pickup", fmt.Sprintf("picked up volume from ioslot %d", a.Address), map[string]string{"ioslot": fmt.Sprint(a.Address)})
		}
	}
	return nil
}

func (l *Library) applyStorageActionsLocked(magazineID string, actions []DoorAction) error {
	slots := map[int]*Volume{}
	for _, s := range l.slots {
		slots[s.Address] = s.Volume
	}
	outside := map[string]*Volume{}
	for _, v := range l.outside {
		outside[v.Barcode] = v
	}

	for i, a := range actions {
		simErr := func(msg string) error {
			return fmt.Errorf("storage action %d: %s", i+1, msg)
		}
		switch a.Action {
		case "load":
			if a.Barcode == "" {
				return simErr("label is required for load")
			}
			slot, err := l.findSlot(a.Address)
			if err != nil {
				return simErr(err.Error())
			}
			if slot.MagazineID != magazineID {
				return simErr(fmt.Sprintf("slot %d does not belong to magazine %q", a.Address, magazineID))
			}
			vol, ok := outside[a.Barcode]
			if !ok {
				return simErr(fmt.Sprintf("outside volume %q not found", a.Barcode))
			}
			if slots[a.Address] != nil {
				return simErr(fmt.Sprintf("slot %d: %s", a.Address, ErrFull))
			}
			slots[a.Address] = vol
			delete(outside, a.Barcode)
		case "pickup":
			slot, err := l.findSlot(a.Address)
			if err != nil {
				return simErr(err.Error())
			}
			if slot.MagazineID != magazineID {
				return simErr(fmt.Sprintf("slot %d does not belong to magazine %q", a.Address, magazineID))
			}
			vol := slots[a.Address]
			if vol == nil {
				return simErr(fmt.Sprintf("slot %d: %s", a.Address, ErrEmpty))
			}
			if _, exists := outside[vol.Barcode]; exists {
				return simErr(fmt.Sprintf("outside volume %q already exists", vol.Barcode))
			}
			outside[vol.Barcode] = vol
			slots[a.Address] = nil
		default:
			return simErr(fmt.Sprintf("unknown action %q", a.Action))
		}
	}

	// Rebuilt from a map, so the iteration order is random - sort by barcode
	// afterwards to keep the outside inventory (and the dashboard's "Outside
	// Tapes" card built from it) in a stable order instead of reshuffling on
	// every door close.
	l.outside = l.outside[:0]
	for _, v := range outside {
		l.outside = append(l.outside, v)
	}
	sort.Slice(l.outside, func(i, j int) bool { return l.outside[i].Barcode < l.outside[j].Barcode })
	for _, s := range l.slots {
		s.Volume = slots[s.Address]
	}
	for _, a := range actions {
		if a.Action == "load" {
			l.emit("storage-load", fmt.Sprintf("loaded outside volume %q into slot %d", a.Barcode, a.Address), map[string]string{"volume": a.Barcode, "slot": fmt.Sprint(a.Address)})
		} else {
			l.emit("storage-pickup", fmt.Sprintf("picked up volume from slot %d", a.Address), map[string]string{"slot": fmt.Sprint(a.Address)})
		}
	}
	return nil
}

// resolveTapeSetLocked looks up tapeSet in the live config and returns its
// config, the resolved barcode.Spec derived from its tape type's format
// fields, and its tape type's capacity in bytes. Callers must hold l.mu.
func (l *Library) resolveTapeSetLocked(tapeSet string) (config.TapeSetConfig, barcode.Spec, int64, error) {
	var ts config.TapeSetConfig
	found := false
	for _, t := range l.cfg.Library.TapeSets {
		if t.Name == tapeSet {
			ts = t
			found = true
			break
		}
	}
	if !found {
		return config.TapeSetConfig{}, barcode.Spec{}, 0, fmt.Errorf("tape set %q: %w", tapeSet, ErrNotFound)
	}
	var tt config.TapeType
	ttFound := false
	for _, t := range l.cfg.Library.TapeTypes {
		if t.Name == ts.TapeType {
			tt = t
			ttFound = true
			break
		}
	}
	if !ttFound {
		return config.TapeSetConfig{}, barcode.Spec{}, 0, fmt.Errorf("tape set %q references tape type %q: %w", tapeSet, ts.TapeType, ErrUnknownTapeSet)
	}
	spec, err := barcode.SpecFor(tt.BarcodeFamily, tt.MediaID, tt.VolSerLength)
	if err != nil {
		return config.TapeSetConfig{}, barcode.Spec{}, 0, fmt.Errorf("tape set %q: %w", tapeSet, err)
	}
	capacityBytes, _ := config.ParseSize(tt.Capacity)
	if capacityBytes <= 0 {
		capacityBytes, _ = config.ParseSize(l.cfg.Library.DefaultCapacity)
	}
	return ts, spec, capacityBytes, nil
}

// createCartridgeLocked creates one cartridge file for ts with an explicit
// barcode already known to be free and well-formed. Callers must hold l.mu.
func (l *Library) createCartridgeLocked(ts config.TapeSetConfig, bc string, capacityBytes int64) (*Volume, error) {
	if err := os.MkdirAll(ts.StorageFolder, 0o770); err != nil {
		return nil, fmt.Errorf("create tape set folder: %w", err)
	}
	path := filepath.Join(ts.StorageFolder, bc)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o660)
	if err != nil {
		return nil, fmt.Errorf("create cartridge file: %w", err)
	}
	f.Close()
	vol := &Volume{Barcode: bc, TapeSet: ts.Name, Path: path, CapacityBytes: capacityBytes, CreatedAt: time.Now().UTC()}
	l.outside = append(l.outside, vol)
	l.emit("outside-create", fmt.Sprintf("created cartridge %q in tape set %q", bc, ts.Name), map[string]string{"volume": bc, "tape_set": ts.Name})
	return vol, nil
}

// CreateTapeSetCartridges bulk-generates count new cartridges for tapeSet,
// auto-assigning barcodes per its tape type's barcode format (see
// internal/barcode). Safe to call repeatedly to top up a tape set: barcode
// sequence numbers are derived by scanning for the next free one each call
// (via containsBarcodeLocked), not from a persisted counter, so topping up
// after a daemon restart continues the sequence correctly.
func (l *Library) CreateTapeSetCartridges(tapeSet string, count int) ([]*Volume, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ts, spec, capacityBytes, err := l.resolveTapeSetLocked(tapeSet)
	if err != nil {
		return nil, err
	}
	barcodes, err := barcode.NextAvailable(spec, l.containsBarcodeLocked, count)
	if err != nil {
		return nil, err
	}
	out := make([]*Volume, 0, len(barcodes))
	for _, bc := range barcodes {
		vol, err := l.createCartridgeLocked(ts, bc, capacityBytes)
		if err != nil {
			l.saveLocked()
			return out, err
		}
		out = append(out, vol)
	}
	l.saveLocked()
	return out, nil
}

// CreateManualCartridge creates exactly one cartridge in tapeSet with an
// operator-supplied barcode, validated against the tape set's tape type's
// barcode format and checked for uniqueness across the entire system (every
// slot, ioslot, drive, outside, and offsite volume - see
// containsBarcodeLocked).
func (l *Library) CreateManualCartridge(tapeSet, bc string) (*Volume, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ts, spec, capacityBytes, err := l.resolveTapeSetLocked(tapeSet)
	if err != nil {
		return nil, err
	}
	if !barcodeRE.MatchString(bc) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidBarcode, bc)
	}
	if err := barcode.Validate(spec, bc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidBarcode, err)
	}
	if l.containsBarcodeLocked(bc) {
		return nil, fmt.Errorf("barcode %q: %w", bc, ErrBarcodeExists)
	}
	vol, err := l.createCartridgeLocked(ts, bc, capacityBytes)
	if err != nil {
		return nil, err
	}
	l.saveLocked()
	return vol, nil
}

// DeleteOutsideVolume permanently removes a tape from outside inventory.
func (l *Library) DeleteOutsideVolume(bc string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	vol, idx := l.findOutside(bc)
	if vol == nil {
		return fmt.Errorf("volume %q: %w", bc, ErrOutsideOnly)
	}
	_ = os.Remove(vol.Path)
	l.outside = append(l.outside[:idx], l.outside[idx+1:]...)
	l.emit("outside-delete", fmt.Sprintf("deleted outside volume %q", bc), map[string]string{"volume": bc})
	l.saveLocked()
	return nil
}

// OutsideVolumes returns a snapshot of tapes physically outside the library.
func (l *Library) OutsideVolumes() []*Volume {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.outsideVolumesSnapshotLocked()
}

// cleaningBarcodeSpec is the fixed, non-admin-configurable barcode format
// for cleaning cartridges: a 5-digit sequence plus a "CLN" suffix, e.g.
// "00001CLN". Unlike tape-set cartridges, cleaning tapes don't belong to
// any tape type/tape set catalog entry, so this is a package-level
// constant rather than something resolved per tape set.
// barcode.Generate/Validate/NextAvailable never consult
// barcode.shapeFor/SpecFor/ValidateSpec (those only apply to the
// admin-defined TapeType catalog), so this family needs no changes to
// the internal/barcode package at all.
var cleaningBarcodeSpec = barcode.Spec{Family: "cleaning", MediaID: "CLN", VolSerLength: 5}

const maxCleaningTapes = 5

// cleaningTapesLocked scans every location a volume can be (slots,
// ioslots, drives, outside) for cleaning cartridges. Mirrors AllVolumes'
// scan shape, filtered to Cleaning volumes only. Callers must hold l.mu.
func (l *Library) cleaningTapesLocked() []*Volume {
	var out []*Volume
	for _, s := range l.slots {
		if s.Volume != nil && s.Volume.Cleaning {
			out = append(out, s.Volume)
		}
	}
	for _, io := range l.ioslots {
		if io.Volume != nil && io.Volume.Cleaning {
			out = append(out, io.Volume)
		}
	}
	for _, d := range l.drives {
		if d.Volume != nil && d.Volume.Cleaning {
			out = append(out, d.Volume)
		}
	}
	for _, v := range l.outside {
		if v != nil && v.Cleaning {
			out = append(out, v)
		}
	}
	return out
}

// CleaningTapes returns a snapshot of every cleaning cartridge in the
// pool, regardless of where it currently sits (racked in a slot, loaded
// in a drive, or outside pending placement/removal).
func (l *Library) CleaningTapes() []*Volume {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return snapshotVolumes(l.cleaningTapesLocked())
}

// CreateCleaningTape generates and creates one new cleaning cartridge
// (up to maxCleaningTapes total across the whole pool), auto-assigning
// the next free barcode per cleaningBarcodeSpec. The new cartridge is
// created "outside" the library, exactly like a fresh tape-set
// cartridge - an admin then racks it into a slot using the existing
// storage-door "load" staged action, no new placement mechanism needed.
func (l *Library) CreateCleaningTape() (*Volume, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.cleaningTapesLocked()) >= maxCleaningTapes {
		return nil, ErrCleaningPoolFull
	}
	barcodes, err := barcode.NextAvailable(cleaningBarcodeSpec, l.containsBarcodeLocked, 1)
	if err != nil {
		return nil, err
	}
	bc := barcodes[0]
	dir := filepath.Join(l.cfg.DataDir, "cleaning")
	if err := os.MkdirAll(dir, 0o770); err != nil {
		return nil, fmt.Errorf("create cleaning tape folder: %w", err)
	}
	path := filepath.Join(dir, bc)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o660)
	if err != nil {
		return nil, fmt.Errorf("create cleaning tape file: %w", err)
	}
	f.Close()
	vol := &Volume{
		Barcode:       bc,
		Path:          path,
		CreatedAt:     time.Now().UTC(),
		Cleaning:      true,
		CleaningState: CleaningTapeAvailable,
	}
	l.outside = append(l.outside, vol)
	l.emit("cleaning-tape-create", fmt.Sprintf("created cleaning tape %q", bc), map[string]string{"volume": bc})
	l.saveLocked()
	return vol, nil
}

// findAvailableCleaningTapeLocked returns the first racked (slot or
// ioslot), available, non-expired cleaning cartridge found anywhere in
// the physical library, along with an ElementRef locating it. A cleaning
// tape sitting "outside" isn't eligible - it must already be racked to
// be loadable. Deliberately unscoped by logical library: the backup
// robot (mode CleaningModeRobot) must be able to reach a cleaning tape
// regardless of which logical library the target drive belongs to,
// mirroring how Load/Move already treat an empty logicalLibrary as
// unscoped/trusted. Callers must hold l.mu.
func (l *Library) findAvailableCleaningTapeLocked() (ElementRef, *Volume, error) {
	for _, s := range l.slots {
		if s.Volume != nil && s.Volume.Cleaning && s.Volume.CleaningState == CleaningTapeAvailable {
			return ElementRef{Kind: KindSlot, Address: s.Address}, s.Volume, nil
		}
	}
	for _, io := range l.ioslots {
		if io.Volume != nil && io.Volume.Cleaning && io.Volume.CleaningState == CleaningTapeAvailable {
			return ElementRef{Kind: KindIOSlot, Address: io.Address}, io.Volume, nil
		}
	}
	return ElementRef{}, nil, ErrCleaningTapeUnavailable
}

// OffsiteVolumes returns a snapshot of tapes currently vaulted offsite.
func (l *Library) OffsiteVolumes() []*Volume {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return snapshotVolumes(l.offsite)
}

// OffsiteSend simulates sending a volume to offsite vault storage: it moves
// the volume from a storage slot to the (unlimited-capacity, like the
// outside-library inventory) offsite collection, freeing the slot.
func (l *Library) OffsiteSend(from ElementRef) (*Volume, error) {
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
	l.offsite = append(l.offsite, vol)
	l.emit("offsite-send", fmt.Sprintf("sent volume %q offsite from %s", vol.Barcode, fromLabel),
		map[string]string{"volume": vol.Barcode, "from": fromLabel})
	l.saveLocked()
	return vol, nil
}

// OffsiteRecall simulates recalling a volume from offsite vault storage back
// into the library at the given slot or I/O slot.
func (l *Library) OffsiteRecall(label string, to ElementRef) error {
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
	setTo(vol)
	l.emit("offsite-recall", fmt.Sprintf("recalled volume %q from offsite into %s", vol.Barcode, toLabel),
		map[string]string{"volume": vol.Barcode, "to": toLabel})
	l.saveLocked()
	return nil
}

// RotateOffsite is run periodically (when offsite rotation is enabled) to
// simulate scheduled tape rotation: it sends up to count of the
// least-recently-created full volumes currently in storage slots offsite.
//
// Candidates are selected under the lock, which is then released before
// each OffsiteSend re-acquires it - deliberately, since OffsiteSend is the
// single place that owns the send's own validation/eventing and calling it
// under an already-held lock would deadlock. The gap means a candidate that
// something else moves in the meantime is simply skipped with a logged
// error rather than moved from under that operation, which is the right
// outcome for a best-effort background sweep (contrast with Move/Load/
// Unload, which hold the lock throughout precisely because they must not
// interleave).
func (l *Library) RotateOffsite(count int) {
	if count <= 0 {
		return
	}
	l.mu.Lock()
	if !l.cfg.Library.OffsiteLocation {
		l.mu.Unlock()
		return
	}
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
	l.mu.Unlock()

	for _, c := range candidates {
		if _, err := l.OffsiteSend(c.ref); err != nil {
			l.mu.Lock()
			l.emit("offsite-rotation-error", fmt.Sprintf("scheduled offsite rotation failed for %s", c.vol.Barcode),
				map[string]string{"volume": c.vol.Barcode, "error": err.Error()})
			l.mu.Unlock()
		}
	}
}

// Load moves a volume from a slot or IO slot into a drive, and creates the
// Bareos-facing symlink at the drive's configured device path. If
// logicalLibrary is non-empty, both the source and the drive must belong to
// that logical library (see Move's doc comment).
func (l *Library) Load(from ElementRef, driveIndex int, logicalLibrary string) error {
	if from.Kind == KindDrive {
		return fmt.Errorf("%w: source cannot be a drive", ErrInvalidTarget)
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	if logicalLibrary != "" {
		driveRef := ElementRef{Kind: KindDrive, Address: driveIndex}
		if !l.elementInLogicalLibraryLocked(logicalLibrary, from) || !l.elementInLogicalLibraryLocked(logicalLibrary, driveRef) {
			return fmt.Errorf("logical library %s: %w", logicalLibrary, ErrOutsideLogicalLibrary)
		}
	}

	drive, err := l.findDrive(driveIndex)
	if err != nil {
		return err
	}
	if drive.Fault {
		return fmt.Errorf("drive %d: %w", driveIndex, ErrDriveFault)
	}
	if l.roboticFault.Active {
		return fmt.Errorf("robotic arm: %s: %w", l.roboticFault.Kind, ErrRoboticFault)
	}
	if drive.Volume != nil {
		return fmt.Errorf("drive %d: %w", driveIndex, ErrFull)
	}
	getFrom, setFrom, fromLabel, err := l.volumeSlot(from)
	if err != nil {
		return err
	}
	vol := getFrom()
	if vol == nil {
		return fmt.Errorf("%s: %w", fromLabel, ErrEmpty)
	}
	// Cleaning-specific behavior (duration sleep, usage tracking, expiry
	// enforcement, auto-eject below) only applies while the feature is
	// globally enabled - mirrors Latency.Enabled zeroing every simulated
	// delay when disabled, so a cleaning cartridge with management
	// switched off just behaves like an ordinary volume.
	cleaning := l.cleaningEnabled && vol.Cleaning
	if cleaning && vol.CleaningState == CleaningTapeExpired {
		return fmt.Errorf("cleaning tape %q: %w", vol.Barcode, ErrCleaningTapeExpired)
	}

	l.setArmBusy(true)
	defer l.setArmBusy(false)

	// Emitted now, not before the checks above: this announces a load
	// that is actually about to happen, not one that might still be
	// rejected - real-time progression means "starting" events only
	// fire once the action is committed to proceed. Plain bracketing
	// message - the atomic "moving to"/"grabbed"/"loaded" narration below
	// is live-only (see recordArmStep), not logged or SNMP'd.
	l.emit("loading", fmt.Sprintf("loading volume %q from %s into drive %d", vol.Barcode, fromLabel, driveIndex),
		map[string]string{"volume": vol.Barcode, "from": fromLabel, "drive": fmt.Sprint(driveIndex)})

	half := l.driveLoadLatency / 2
	l.recordArmStep(fmt.Sprintf("moving to %s", fromLabel))
	if half > 0 {
		time.Sleep(half)
	}
	l.recordArmStep(fmt.Sprintf("grabbed tape %s from %s", vol.Barcode, fromLabel))
	l.recordArmStep(fmt.Sprintf("moving to drive %d", driveIndex))
	if rem := l.driveLoadLatency - half; rem > 0 {
		time.Sleep(rem)
	}
	l.recordArmStep(fmt.Sprintf("loaded tape %s into drive %d", vol.Barcode, driveIndex))
	l.setArmPosition(ArmPosition{Kind: "drive", Address: driveIndex})

	// Positioning/seek inside the drive after the mechanical load, before
	// it's ready to read/write - drive-internal, not arm movement (the
	// arm has already stepped away), so this gets no step/event of its
	// own; the "load" success event below already brackets the whole
	// load+position sequence.
	if l.tapePositionLatency > 0 {
		time.Sleep(l.tapePositionLatency)
	}

	// The Bareos-facing device-path symlink is only created now, once the
	// drive is genuinely ready - mechanical load AND tape positioning both
	// complete. Creating it any earlier (as this code used to, right at
	// the very start of Load) would let Bareos open/read/write through a
	// device gotochangerd itself doesn't yet consider loaded, racing
	// ahead of the whole simulated mechanical sequence - real read/write
	// activity could start (and finish, and be missed) before the "moving
	// to drive N"/"loaded tape" narration above has even been shown.
	_ = os.Remove(drive.DevicePath)
	if err := os.Symlink(vol.Path, drive.DevicePath); err != nil {
		return fmt.Errorf("link drive device: %w", err)
	}

	setFrom(nil)
	drive.Volume = vol
	origin := from
	drive.Origin = &origin
	l.startDriveWatcherLocked(driveIndex, vol.Path)

	if cleaning {
		vol.CleaningState = CleaningTapeInUse
	} else {
		drive.MountsSinceCleaning++
	}

	l.emit("load", fmt.Sprintf("loaded volume %q from %s into drive %d", vol.Barcode, fromLabel, driveIndex),
		map[string]string{"volume": vol.Barcode, "from": fromLabel, "drive": fmt.Sprint(driveIndex)})
	// Explicit, not just the deferred clear at the top: this must happen
	// before the cleaning branch's l.mu.Unlock()/cleaningDuration sleep
	// below, since cleaning is drive-internal, not robotic transport (see
	// withCleaningOp's doc comment in app.js) - the arm is genuinely free
	// during that sleep. The defer remains as a safety net for early
	// returns above this point; calling both is harmless (idempotent).
	l.setArmBusy(false)

	// A cleaning cartridge runs its full cycle - and is automatically
	// ejected back to where it came from - within this same Load call,
	// rather than waiting for a separate Unload call. This is what
	// "manual cleaning" means in practice: an operator loading a
	// cleaning tape into a drive (the same generic Load action used for
	// any cartridge) *is* the trigger - there's no separate "start
	// cleaning" action or endpoint. The drive is committed above, so
	// Status() already reports it busy with the cleaning volume.
	if cleaning {
		l.emit("cleaning-start", fmt.Sprintf("start cleaning cycle on drive %d with tape %q", driveIndex, vol.Barcode),
			map[string]string{"volume": vol.Barcode, "drive": fmt.Sprint(driveIndex)})
		l.saveLocked()

		// Unlike every other simulated delay in this codebase (held
		// under l.mu for its whole duration - see CLAUDE.md's "single
		// robotic arm" tradeoff), the cleaning-duration sleep
		// specifically releases the lock: a real drive performs its own
		// internal cleaning cycle once a cartridge is inserted, largely
		// independent of the robotic arm, so the arm (modeled by l.mu)
		// is free to service other drives/slots/status reads while this
		// one cleans. This is also what makes "the drive must be
		// considered busy while cleaning" genuinely observable by a
		// concurrent Status() call - without releasing the lock, a
		// concurrent caller would simply block for the cycle's entire
		// duration and only ever see the *post*-cycle (already ejected)
		// state once unblocked.
		l.mu.Unlock()
		if l.cleaningDuration > 0 {
			time.Sleep(l.cleaningDuration)
		}
		l.mu.Lock()

		// Re-resolve the drive (rather than reuse the pre-sleep pointer,
		// which a topology change could have orphaned) and confirm this
		// cartridge is still the one loaded - another operation could
		// have unloaded/replaced it while the lock was released above.
		// If not, there's nothing left to finish here; whatever
		// displaced it already recorded its own event.
		d, err := l.findDrive(driveIndex)
		if err != nil || d.Volume != vol {
			return nil
		}

		vol.CleaningUsageCount++
		detail := map[string]string{"volume": vol.Barcode, "drive": fmt.Sprint(driveIndex)}
		if l.cleaningMaxUses > 0 && vol.CleaningUsageCount >= l.cleaningMaxUses {
			vol.CleaningState = CleaningTapeExpired
			l.emit("cleaning-expired", fmt.Sprintf("cleaning tape %q reached its maximum use count (%d)", vol.Barcode, vol.CleaningUsageCount), detail)
		}
		d.MountsSinceCleaning = 0

		// Auto-eject only in CleaningModeRobot, where cleaning cartridges
		// live in a magazine structurally invisible to Bareos and
		// gotochangerd is the only actor that will ever move them (see
		// AutoCleanSweep/autoCleanDrive - the same "who manages the
		// cartridge" split applies here for a manually-triggered cycle,
		// not just the automatic sweep). In CleaningModeSoftware, the
		// cartridge is config.CleaningSettings.Mode's documented "backup
		// software itself decides when to mount/unmount" case - the
		// backup software issued this Load and is expected to issue its
		// own matching Unload once done, exactly like any other volume,
		// so the cartridge is left mounted (still CleaningTapeInUse)
		// rather than snatched back by the robot mid-workflow.
		if l.cleaningMode == config.CleaningModeRobot {
			l.emit("cleaning-cycle", fmt.Sprintf("cleaning cycle done on drive %d with tape %q", driveIndex, vol.Barcode), detail)
			l.ejectCleaningTapeAfterCycleLocked(d, driveIndex, origin)
		} else {
			l.emit("cleaning-cycle", fmt.Sprintf("cleaning cycle done on drive %d with tape %q; still mounted, awaiting unload", driveIndex, vol.Barcode), detail)
		}
	}

	l.saveLocked()
	return nil
}

// ejectCleaningTapeAfterCycleLocked automatically returns a cleaning
// cartridge to the slot/ioslot it was loaded from once its cleaning
// cycle completes, for both an operator-initiated Load and an
// AutoCleanSweep-initiated one alike - callers never issue a separate
// Unload for a cleaning tape. Because Load releases l.mu for the
// cleaning-duration sleep (see Load's doc comment), another operation
// genuinely can move something else into the origin slot/ioslot - or
// remove it entirely via Reconfigure - in that window, so this case is
// handled for real, not just defensively: the cartridge is moved
// "outside" instead of being dropped, and a warning event is logged.
// Callers must hold l.mu and have already confirmed drive.Volume is
// still the cleaning volume being ejected.
func (l *Library) ejectCleaningTapeAfterCycleLocked(drive *Drive, driveIndex int, origin ElementRef) {
	l.setArmBusy(true)
	defer l.setArmBusy(false)

	vol := drive.Volume
	// Resolved once, up front, and reused below for both the "unloading"
	// pre-event's label and the post-sleep occupancy check - safe since
	// l.mu is held for this whole function, so nothing else can act on
	// origin in between.
	getTo, setTo, toLabel, err := l.volumeSlot(origin)
	unloadingMsg := fmt.Sprintf("unloading volume %q from drive %d", vol.Barcode, driveIndex)
	unloadingDetail := map[string]string{"volume": vol.Barcode, "drive": fmt.Sprint(driveIndex)}
	if err == nil {
		unloadingMsg += " to " + toLabel
		unloadingDetail["to"] = toLabel
	}
	l.emit("unloading", unloadingMsg, unloadingDetail)

	// See driveActivityUnloadSettleDelay's doc comment: gives the drive's
	// activity watcher a chance to report any already-in-flight, genuinely
	// detected read/write before stopDriveWatcherLocked below tears it
	// down and the drive's volume is cleared.
	if driveActivityUnloadSettleDelay > 0 {
		time.Sleep(driveActivityUnloadSettleDelay)
	}
	l.stopDriveWatcherLocked(driveIndex)
	_ = os.Remove(drive.DevicePath)

	half := l.driveUnloadLatency / 2
	l.recordArmStep(fmt.Sprintf("moving to drive %d", driveIndex))
	if half > 0 {
		time.Sleep(half)
	}
	l.recordArmStep(fmt.Sprintf("grabbed tape %s from drive %d", vol.Barcode, driveIndex))
	moveTarget := toLabel
	if err != nil {
		moveTarget = "storage"
	}
	l.recordArmStep(fmt.Sprintf("moving to %s", moveTarget))
	if rem := l.driveUnloadLatency - half; rem > 0 {
		time.Sleep(rem)
	}

	drive.Volume = nil
	drive.Origin = nil
	drive.Activity = ""
	if vol.CleaningState != CleaningTapeExpired {
		vol.CleaningState = CleaningTapeAvailable
	}

	if err != nil || getTo() != nil {
		reason := "its original location no longer exists"
		if err == nil {
			reason = fmt.Sprintf("%s is occupied", toLabel)
		}
		l.outside = append(l.outside, vol)
		l.recordArmStep(fmt.Sprintf("placed tape %s outside the library", vol.Barcode))
		l.setArmPosition(ArmPosition{Kind: ArmPositionParked})
		l.emit("cleaning-eject-fallback", fmt.Sprintf("could not return cleaning tape %q to its original location after drive %d's cleaning cycle (%s); moved it outside the library instead", vol.Barcode, driveIndex, reason),
			map[string]string{"volume": vol.Barcode, "drive": fmt.Sprint(driveIndex), "reason": reason})
		return
	}
	setTo(vol)
	l.recordArmStep(fmt.Sprintf("placed tape %s into %s", vol.Barcode, toLabel))
	l.setArmPosition(armPositionFor(origin))
	l.emit("unload", fmt.Sprintf("unloaded volume %q from drive %d into %s", vol.Barcode, driveIndex, toLabel),
		map[string]string{"volume": vol.Barcode, "drive": fmt.Sprint(driveIndex), "to": toLabel})
}

// Unload moves the volume currently in a drive back out to a slot or IO
// slot, and removes the Bareos-facing device symlink. If logicalLibrary is
// non-empty, both the drive and the destination must belong to that logical
// library (see Move's doc comment).
func (l *Library) Unload(driveIndex int, to ElementRef, logicalLibrary string) error {
	if to.Kind == KindDrive {
		return fmt.Errorf("%w: destination cannot be a drive", ErrInvalidTarget)
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	if logicalLibrary != "" {
		driveRef := ElementRef{Kind: KindDrive, Address: driveIndex}
		if !l.elementInLogicalLibraryLocked(logicalLibrary, driveRef) || !l.elementInLogicalLibraryLocked(logicalLibrary, to) {
			return fmt.Errorf("logical library %s: %w", logicalLibrary, ErrOutsideLogicalLibrary)
		}
	}

	drive, err := l.findDrive(driveIndex)
	if err != nil {
		return err
	}
	if l.roboticFault.Active {
		return fmt.Errorf("robotic arm: %s: %w", l.roboticFault.Kind, ErrRoboticFault)
	}
	if drive.Volume == nil {
		return fmt.Errorf("drive %d: %w", driveIndex, ErrEmpty)
	}
	getTo, setTo, toLabel, err := l.volumeSlot(to)
	if err != nil {
		return err
	}
	if getTo() != nil {
		return fmt.Errorf("%s: %w", toLabel, ErrFull)
	}

	vol := drive.Volume
	l.refreshVolumeSizeLocked(vol)

	l.setArmBusy(true)
	defer l.setArmBusy(false)

	l.emit("unloading", fmt.Sprintf("unloading volume %q from drive %d to %s", vol.Barcode, driveIndex, toLabel),
		map[string]string{"volume": vol.Barcode, "drive": fmt.Sprint(driveIndex), "to": toLabel})

	// See driveActivityUnloadSettleDelay's doc comment: gives the drive's
	// activity watcher a chance to report any already-in-flight, genuinely
	// detected read/write before stopDriveWatcherLocked below tears it
	// down and the drive's volume is cleared. This matters most for very
	// fast operations (e.g. Bareos labeling a tape - a tiny, near-instant
	// write) immediately followed by an unload.
	if driveActivityUnloadSettleDelay > 0 {
		time.Sleep(driveActivityUnloadSettleDelay)
	}
	l.stopDriveWatcherLocked(driveIndex)
	_ = os.Remove(drive.DevicePath)

	half := l.driveUnloadLatency / 2
	l.recordArmStep(fmt.Sprintf("moving to drive %d", driveIndex))
	if half > 0 {
		time.Sleep(half)
	}
	l.recordArmStep(fmt.Sprintf("grabbed tape %s from drive %d", vol.Barcode, driveIndex))
	l.recordArmStep(fmt.Sprintf("moving to %s", toLabel))
	if rem := l.driveUnloadLatency - half; rem > 0 {
		time.Sleep(rem)
	}
	l.recordArmStep(fmt.Sprintf("placed tape %s into %s", vol.Barcode, toLabel))

	drive.Volume = nil
	drive.Origin = nil
	drive.Activity = ""
	setTo(vol)
	l.setArmPosition(armPositionFor(to))
	if vol.Cleaning {
		drive.MountsSinceCleaning = 0
		if vol.CleaningState != CleaningTapeExpired {
			vol.CleaningState = CleaningTapeAvailable
		}
	}
	l.emit("unload", fmt.Sprintf("unloaded volume %q from drive %d into %s", vol.Barcode, driveIndex, toLabel),
		map[string]string{"volume": vol.Barcode, "drive": fmt.Sprint(driveIndex), "to": toLabel})
	l.saveLocked()
	return nil
}

// autoCleanDrive runs one automatic cleaning cycle on driveIndex, used
// only by AutoCleanSweep. Unlike manual cleaning (just an operator's
// ordinary Load call targeting a cleaning cartridge they picked
// themselves - see Load's doc comment), the automatic path has no human
// choosing a tape, so it must find a usable one itself first before
// calling the same Load, which then handles the cleaning-duration
// sleep/usage-tracking/expiry-check/auto-eject on its own.
func (l *Library) autoCleanDrive(driveIndex int) {
	l.mu.Lock()
	drive, err := l.findDrive(driveIndex)
	if err != nil || drive.Volume != nil {
		// Already busy, or the drive disappeared between the sweep's
		// scan and this goroutine actually running - nothing to do.
		l.mu.Unlock()
		return
	}
	ref, _, err := l.findAvailableCleaningTapeLocked()
	if err != nil {
		l.emit("cleaning-unavailable", fmt.Sprintf("no usable cleaning tape available for drive %d", driveIndex), map[string]string{"drive": fmt.Sprint(driveIndex)})
		l.saveLocked()
		l.mu.Unlock()
		return
	}
	l.mu.Unlock()

	_ = l.Load(ref, driveIndex, "")
}

// AutoCleanSweep is called periodically (see cmd/gotochangerd/main.go's
// cleaning ticker) to find idle drives at or over the configured mount
// threshold and automatically run a cleaning cycle for each. A no-op
// unless cleaning is enabled AND in CleaningModeRobot - in
// CleaningModeSoftware, MountsSinceCleaning is still tracked (for Admin
// UI visibility) but nothing is auto-triggered, since the backup
// software alone decides when to mount a cleaning tape in that mode.
// Each attempt is fired via a separate goroutine so a multi-minute
// cleaning cycle on one drive doesn't delay noticing another due drive -
// a second concurrent attempt would just block on l.mu.Lock() anyway
// given the single-robotic-arm model, so this only keeps the sweep
// itself responsive, mirroring the existing fire-and-forget
// "go l.notifier.Notify(e)" convention already used inside emit.
func (l *Library) AutoCleanSweep() {
	l.mu.RLock()
	if !l.cleaningEnabled || l.cleaningMode != config.CleaningModeRobot || l.cleaningMountThreshold <= 0 {
		l.mu.RUnlock()
		return
	}
	var due []int
	for _, d := range l.drives {
		if d.Volume == nil && d.MountsSinceCleaning >= l.cleaningMountThreshold {
			due = append(due, d.Index)
		}
	}
	l.mu.RUnlock()

	for _, idx := range due {
		go l.autoCleanDrive(idx)
	}
}

// SetDriveFault toggles the simulated fault state of a drive, useful for
// testing failure handling in the backup software and for the SNMP demo.
func (l *Library) SetDriveFault(driveIndex int, fault bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	d, err := l.findDrive(driveIndex)
	if err != nil {
		return err
	}
	d.Fault = fault
	msg := "cleared"
	if fault {
		msg = "raised"
	}
	l.emit("drive-fault", fmt.Sprintf("%s fault on drive %d", msg, driveIndex), map[string]string{"drive": fmt.Sprint(driveIndex)})
	l.saveLocked()
	return nil
}

// SetVolumeWriteProtect toggles the simulated write-protect tab on the
// cartridge identified by bc - mirrors SetDriveFault's toggle shape, but the
// flag lives on the Volume itself rather than a fixed element, since a
// physical write-protect tab travels with the cartridge.
//
// Only reachable while the cartridge is physically accessible - outside the
// library, offsite, or in a storage/mailbox slot whose door is currently
// open - matching a real write-protect tab, which can't be flipped while
// the tape is sealed inside a closed magazine/mailbox or mounted in a
// drive. See findAccessibleVolumeForWriteProtectLocked.
//
// This deliberately never blocks Move/Load/Unload - the changer must not
// prevent loading a write-protected tape, only the drive enforces it, on an
// actual write attempt. Enforcement is two independent mechanisms for this
// project's two operational modes: applyVolumeFileModeLocked's chmod (the
// only interception point available in command-script mode, since
// gotochanger-changer never sees Bareos's write byte stream) and
// Drive.write6's explicit field check in kernel mode (chmod alone doesn't
// help there - gotochanger-tcmud runs unsandboxed as root and bypasses
// permission bits).
func (l *Library) SetVolumeWriteProtect(bc string, protected bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	vol, err := l.findAccessibleVolumeForWriteProtectLocked(bc)
	if err != nil {
		return err
	}
	vol.WriteProtected = protected
	l.applyVolumeFileModeLocked(vol)
	msg := "cleared"
	if protected {
		msg = "set"
	}
	l.emit("write-protect", fmt.Sprintf("write-protect %s on volume %q", msg, bc), map[string]string{"volume": bc})
	l.saveLocked()
	return nil
}

// SetRoboticFault raises or clears a simulated fault on the library's
// single robotic arm. While active, every operation that requires arm
// movement (Move, Load, Unload) is rejected with ErrRoboticFault until an
// admin clears it - mirrors SetDriveFault, but scoped to the whole
// physical library rather than one drive, since there is only one arm
// (door open/close is deliberately unaffected - see Move/Load/Unload's
// own fault checks). kind/message are only stored when active; clearing
// always resets both.
func (l *Library) SetRoboticFault(active bool, kind, message string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if active {
		if !validRoboticFaultKind(kind) {
			return fmt.Errorf("robotic fault kind %q: %w", kind, ErrInvalidRoboticFaultKind)
		}
		l.roboticFault = RoboticFault{Active: true, Kind: kind, Message: message, SetAt: time.Now().UTC()}
	} else {
		l.roboticFault = RoboticFault{}
	}
	msg := "cleared"
	if active {
		msg = "raised"
	}
	l.emit("robotic-fault", fmt.Sprintf("%s robotic fault (kind=%s)", msg, kind), map[string]string{"kind": kind})
	l.saveLocked()
	return nil
}

func validRoboticFaultKind(kind string) bool {
	for _, k := range RoboticFaultKinds {
		if k == kind {
			return true
		}
	}
	return false
}

const (
	volumeWritableMode = 0o660 // matches createCartridgeLocked's file mode
	volumeReadOnlyMode = 0o440 // matches the mode a Full volume has always been chmod'd to
)

// applyVolumeFileModeLocked reconciles v's real backing-file permission bits
// with its current Full/WriteProtected state: read-only if either is true,
// writable otherwise. This is the ONLY write-enforcement mechanism available
// in command-script mode - gotochangerd never sees Bareos's write byte
// stream there. Called immediately whenever either flag changes, regardless
// of the volume's current location, since a physical write-protect tab
// (like Full/end-of-tape) is a property of the cartridge, not something
// tied to Load/Unload timing.
//
// Full is one-way in practice (see refreshVolumeSizeLocked) but
// WriteProtected is reversible - toggling WriteProtected off must NOT make
// a Full volume's file writable again, which is why this always recomputes
// from both flags together instead of each call site chmod'ing on its own.
func (l *Library) applyVolumeFileModeLocked(v *Volume) {
	mode := os.FileMode(volumeWritableMode)
	if v.Full || v.WriteProtected {
		mode = volumeReadOnlyMode
	}
	_ = os.Chmod(v.Path, mode)
}

// refreshVolumeSizeLocked stats a volume's backing file and updates
// WrittenBytes/Full. Callers must hold l.mu.
func (l *Library) refreshVolumeSizeLocked(v *Volume) {
	fi, err := os.Stat(v.Path)
	if err != nil {
		return
	}
	v.WrittenBytes = fi.Size()
	wasFull := v.Full
	v.Full = v.CapacityBytes > 0 && fi.Size() >= v.CapacityBytes
	if v.Full && !wasFull {
		l.applyVolumeFileModeLocked(v)
		l.emit("volume-full", fmt.Sprintf("volume %q reached capacity (simulated end of tape)", v.Barcode),
			map[string]string{"volume": v.Barcode})
	}
}

// PollCapacity is run periodically by the daemon to detect volumes loaded in
// drives that have grown to their configured capacity, simulating a real
// tape drive reporting end-of-medium back to the application.
func (l *Library) PollCapacity() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, d := range l.drives {
		if d.Volume != nil {
			l.refreshVolumeSizeLocked(d.Volume)
		}
	}
}

// AllVolumes returns every known volume across slots, IO slots and drives,
// deep-copied like every other volume getter here.
func (l *Library) AllVolumes() []*Volume {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var out []*Volume
	for _, s := range l.slots {
		if s.Volume != nil {
			out = append(out, snapshotVolume(s.Volume))
		}
	}
	for _, io := range l.ioslots {
		if io.Volume != nil {
			out = append(out, snapshotVolume(io.Volume))
		}
	}
	for _, d := range l.drives {
		if d.Volume != nil {
			out = append(out, snapshotVolume(d.Volume))
		}
	}
	out = append(out, snapshotVolumes(l.outside)...)
	out = append(out, snapshotVolumes(l.offsite)...)
	return out
}

// FindDriveByVolume returns the index of the drive currently holding label,
// mirroring the "loaded" command from disk-changer.in.
func (l *Library) FindDriveByVolume(label string) (int, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, d := range l.drives {
		if d.Volume != nil && d.Volume.Barcode == label {
			return d.Index, true
		}
	}
	return 0, false
}

// DriveLoadedSlot returns the slot/ioslot address currently loaded in a
// drive if the caller tracks placement by originating slot (used by the
// legacy CLI shim's "loaded" command, which historically returns a slot
// number rather than a volume label).
func (l *Library) DriveVolumeLabel(driveIndex int) (string, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	d, err := l.findDrive(driveIndex)
	if err != nil {
		return "", err
	}
	if d.Volume == nil {
		return "", nil
	}
	return d.Volume.Barcode, nil
}

// DriveOriginSlot returns the storage slot address a drive's current volume
// was loaded from, or 0 if empty or it came from an IO slot. This mirrors
// the legacy disk-changer.in "loaded" command, which always answers with a
// storage slot number.
func (l *Library) DriveOriginSlot(driveIndex int) (int, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	d, err := l.findDrive(driveIndex)
	if err != nil {
		return 0, err
	}
	if d.Volume == nil || d.Origin == nil || d.Origin.Kind != KindSlot {
		return 0, nil
	}
	return d.Origin.Address, nil
}
