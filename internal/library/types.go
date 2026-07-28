// Package library implements the in-memory model of a simulated tape
// autochanger: storage slots, import/export (I/O) slots, data transfer
// elements (drives) and the medium volumes (files) that move between them.
//
// The terminology follows SCSI SMC (Medium Changer) conventions:
//   - Storage Element   -> Slot
//   - Import/Export Element -> IOSlot ("mail slot")
//   - Data Transfer Element -> Drive
package library

import "time"

// Volume represents a virtual tape cartridge, backed by a regular file on
// disk. Its size grows as data is written to it (exactly like a real Bareos
// "Device Type = File" volume) up to CapacityBytes, at which point it is
// marked Full and made read-only to simulate reaching end-of-tape. Barcode
// is the sole identifier for a cartridge (merges what used to be two
// separate fields, "Label" and a cosmetic "Barcode") - it's the filesystem
// name, the uniqueness key, and every lookup key. TapeSet names the tape
// set this cartridge belongs to; every cartridge belongs to exactly one.
type Volume struct {
	Barcode       string    `json:"barcode"`
	TapeSet       string    `json:"tape_set,omitempty"`
	Path          string    `json:"path"`
	CapacityBytes int64     `json:"capacity_bytes"`
	WrittenBytes  int64     `json:"written_bytes"`
	Full          bool      `json:"full"`
	CreatedAt     time.Time `json:"created_at"`

	// WriteProtected simulates a physical write-protect tab: an
	// operator-togglable flag (see Library.SetVolumeWriteProtect),
	// independent of Full - unlike Full, it's reversible. It travels with
	// the cartridge regardless of where it currently sits (slot, ioslot,
	// drive, outside, offsite).
	WriteProtected bool `json:"write_protected"`

	// Cleaning marks this volume as a cleaning cartridge (see Library's
	// cleaning-tape management) rather than a data tape - the same "just a
	// Volume with a marker field set" pattern TapeSet already uses for
	// tape-set cartridges. CleaningState/CleaningUsageCount are only
	// meaningful when Cleaning is true.
	Cleaning           bool   `json:"cleaning,omitempty"`
	CleaningState      string `json:"cleaning_state,omitempty"`
	CleaningUsageCount int    `json:"cleaning_usage_count,omitempty"`
}

// Slot is a Storage Element: a home location for a Volume. Address is a
// flat, dense, library-wide integer recomputed live on every topology
// rebuild (see Library.buildTopologyLocked) - never persisted as a fixed
// value, so it may change whenever a magazine is added, deleted, or
// resized. Label is the human-facing, magazine-relative address
// ("<magazine ordinal>.<slot offset>", e.g. "2.3") admin tooling
// (gotochangerctl, the Admin API, the web UI) displays instead - see
// Library.buildTopologyLocked for how both are derived.
type Slot struct {
	Address    int     `json:"address"`
	Label      string  `json:"label,omitempty"`
	MagazineID string  `json:"magazine_id,omitempty"`
	Volume     *Volume `json:"volume,omitempty"`
}

// IOSlot is an Import/Export Element ("mail slot") used to introduce or
// remove media from the library without opening the whole enclosure.
// MailboxID identifies which mailbox (I/O door) it belongs to, mirroring
// Slot.MagazineID — mailboxes are real, independently addressable groups,
// not just a flat count. Address/Label mirror Slot's exactly, except
// Label's ordinal counts mailboxes independently from magazines (a
// mailbox's own "1st, 2nd..." sequence, not continuing magazines').
type IOSlot struct {
	Address   int     `json:"address"`
	Label     string  `json:"label,omitempty"`
	MailboxID string  `json:"mailbox_id,omitempty"`
	Volume    *Volume `json:"volume,omitempty"`
}

// Drive is a Data Transfer Element. DevicePath is the path Bareos is
// configured with as "Archive Device"; the daemon symlinks it to the loaded
// Volume's backing file, exactly like disk-changer.in did, so existing
// Bareos "Device Type = File" configurations keep working unmodified.
type Drive struct {
	Index      int         `json:"index"`
	DevicePath string      `json:"device_path"`
	DriveType  string      `json:"drive_type,omitempty"` // links to the Drive Types catalog by name, for Admin > Drives' model/generation/capacity display
	Volume     *Volume     `json:"volume,omitempty"`
	Fault      bool        `json:"fault"`
	Origin     *ElementRef `json:"origin,omitempty"` // element the loaded volume came from, for legacy "loaded" queries

	// MountsSinceCleaning counts drive-mount operations (of a non-cleaning
	// volume) since this drive's last completed cleaning cycle, compared
	// against Library's configured mount threshold to decide when a
	// cleaning cycle is required. Reset to 0 whenever a cleaning cartridge
	// is unloaded from this drive.
	MountsSinceCleaning int `json:"mounts_since_cleaning,omitempty"`

	// Activity is "reading"/"writing"/"" (idle), set by a per-drive
	// filesystem watcher on the loaded volume's real backing file (see
	// startDriveActivityWatcher) rather than guessed client-side from
	// polling written_bytes - empty whenever the drive is empty or no
	// activity has been observed since it was loaded.
	Activity string `json:"activity,omitempty"`
}

// RoboticFault is the simulated fault state of the library's single
// robotic arm (see Library.SetRoboticFault). Unlike Drive.Fault (a plain
// bool - a drive just is or isn't faulted), the arm's fault carries a
// Kind, since the spec calls for distinct realistic failure modes
// (blocked arm, mispositioned cartridge, etc.), not just an on/off flag.
type RoboticFault struct {
	Active  bool      `json:"active"`
	Kind    string    `json:"kind,omitempty"`
	Message string    `json:"message,omitempty"`
	SetAt   time.Time `json:"set_at,omitempty"`
}

// Robotic fault kinds: realistic simulated mechanical failure modes for
// the library's single arm. Kept as plain strings (not a Go enum type)
// since they cross the JSON/REST boundary and are duplicated verbatim in
// the web UI (internal/api/static/app.js's roboticFaultKinds), matching
// this codebase's existing convention for small fixed action vocabularies
// (e.g. DoorAction.Action's "load"/"pickup").
const (
	RoboticFaultBlockedArm             = "blocked_arm"
	RoboticFaultMispositionedCartridge = "mispositioned_cartridge"
	RoboticFaultPickupFailure          = "pickup_failure"
	RoboticFaultDropFailure            = "drop_failure"
	RoboticFaultMovementJam            = "movement_jam"
	RoboticFaultOther                  = "other"
)

// RoboticFaultKinds enumerates every valid RoboticFault.Kind value, used
// by Library.SetRoboticFault to validate a raised fault's kind.
var RoboticFaultKinds = []string{
	RoboticFaultBlockedArm,
	RoboticFaultMispositionedCartridge,
	RoboticFaultPickupFailure,
	RoboticFaultDropFailure,
	RoboticFaultMovementJam,
	RoboticFaultOther,
}

// ArmPosition is the robotic arm's last known physical location. Kind is
// "slot"/"ioslot"/"drive" (matching ElementRef.Kind's string values -
// see armPositionFor) when the arm's last real movement placed a tape at
// that element, ArmPositionParked when it's tucked away for a
// magazine/mailbox door being open, or "" if the arm hasn't moved or
// parked since the daemon started. Kept as a plain string, not the Kind
// type, since "parked"/"" aren't valid ElementRef kinds - matching this
// codebase's existing convention for small fixed vocabularies crossing
// the JSON/REST boundary (see RoboticFaultKinds above).
type ArmPosition struct {
	Kind string `json:"kind,omitempty"`
	// Address deliberately has no omitempty: 0 is a real, common address
	// (e.g. the first drive, "drive 0") - omitting it whenever it happens
	// to be zero would make the JSON field vanish exactly for that
	// legitimate value, indistinguishable from "no address at all".
	Address int `json:"address"`
}

// ArmPositionParked is the one ArmPosition.Kind with no corresponding
// ElementRef: the arm is tucked away because at least one magazine or
// mailbox door is currently open (see Library.setArmDoorsOpenDelta).
const ArmPositionParked = "parked"

// ArmState is the robotic arm's live, non-audited transport state -
// mirrors DoorPhases/PhaseNotifier's "live UI signal only, never
// persisted or SNMP'd" contract (see PhaseNotifier's doc comment below).
// Busy is true for the whole mechanical duration of a Move/Load/Unload
// (and a magazine's post-close scan), set by the Library method itself
// so it reflects a real Bareos-driven operation over the trusted socket
// exactly like a browser-driven one - see setArmBusy. Position is a
// derived value (see Library.currentArmStateLocked): the arm's last real
// destination, overridden to ArmPositionParked whenever any door is
// currently open, and automatically reverting once every door is closed
// again - not a one-time transition on door-open.
type ArmState struct {
	Busy     bool        `json:"busy"`
	Position ArmPosition `json:"position"`
}

// ArmStep is one atomic, live-only narration entry (e.g. "moving to slot
// 3", "grabbed tape ABC123 from slot 3") - see Library.recordArmStep.
// Never persisted or SNMP'd, exactly like a door phase transition.
type ArmStep struct {
	Time    time.Time `json:"time"`
	Message string    `json:"message"`
}

// Cleaning tape lifecycle states (Volume.CleaningState, only meaningful
// when Volume.Cleaning is true). Kept as plain strings (not a Go enum
// type), duplicated verbatim in the web UI (internal/api/static/app.js's
// cleaningTapeStates), matching this codebase's existing convention for
// small fixed vocabularies crossing the JSON/REST boundary (see
// RoboticFaultKinds above). A fourth conceptual state, "removed", is
// deliberately not a stored value here - a removed cleaning tape is
// simply deleted (via the existing DeleteOutsideVolume, after being
// physically picked up out of its slot via the existing storage-door
// mechanism), exactly like a deleted tape-set cartridge leaves no
// lingering record today.
const (
	CleaningTapeAvailable = "available"
	CleaningTapeInUse     = "in_use"
	CleaningTapeExpired   = "expired"
)

// CleaningTapeStates enumerates every valid Volume.CleaningState value.
var CleaningTapeStates = []string{
	CleaningTapeAvailable,
	CleaningTapeInUse,
	CleaningTapeExpired,
}

// ElementRef addresses any element in the library, e.g. "slot:3",
// "ioslot:1", "drive:0".
type ElementRef struct {
	Kind    Kind `json:"kind"`
	Address int  `json:"address"`
}

// Kind enumerates the three element categories.
type Kind string

const (
	KindSlot   Kind = "slot"
	KindIOSlot Kind = "ioslot"
	KindDrive  Kind = "drive"
)

// Status is a full point-in-time snapshot of the library, as returned by the
// API and rendered by the web UI.
type Status struct {
	Name           string            `json:"name,omitempty"`
	Slots          []*Slot           `json:"slots"`
	IOSlots        []*IOSlot         `json:"ioslots"`
	Drives         []*Drive          `json:"drives"`
	OutsideVolumes []*Volume         `json:"outside_volumes"`
	OffsiteVolumes []*Volume         `json:"offsite_volumes,omitempty"`
	OffsiteEnabled bool              `json:"offsite_enabled"`
	Doors          DoorStatus        `json:"doors"`
	LogicalLibs    []*LogicalLibrary `json:"logical_libs,omitempty"`
	RoboticFault   RoboticFault      `json:"robotic_fault"`
	ArmState       ArmState          `json:"arm_state"`

	// CleaningEnabled/CleaningMountThreshold/CleaningMaxUses let any role
	// (not just Admin, which is all GET /api/v1/settings/cleaning
	// allows) compute a live "mounts until cleaning" countdown per drive
	// and a "cleaning cycles left" countdown per cleaning cartridge from
	// the already-Viewer-readable Status - see
	// internal/api/static/app.js's drive/slot card tooltips.
	CleaningEnabled        bool `json:"cleaning_enabled"`
	CleaningMountThreshold int  `json:"cleaning_mount_threshold"`
	CleaningMaxUses        int  `json:"cleaning_max_uses"`

	// MagazinePINRequired/MailboxPINRequired let the dashboard show a PIN
	// keypad before opening a door, without a second round trip: derived
	// from whether a hash is configured (see Library.checkMagazinePINLocked/
	// checkMailboxPINLocked), never the hash itself. MailboxPINRequired is
	// sparse - only mailboxes with a PIN configured appear, always true.
	MagazinePINRequired bool            `json:"magazine_pin_required,omitempty"`
	MailboxPINRequired  map[string]bool `json:"mailbox_pin_required,omitempty"`
}

// DoorStatus reflects whether physical access doors are currently open.
// Both doors are per-group, not a single global flag: the storage door is
// per-magazine (see Library.OpenStorageDoor, OpenMagazines lists the IDs of
// magazines currently open) and the I/O door is per-mailbox (see
// Library.OpenIODoor, OpenMailboxes lists the IDs of mailboxes currently
// open).
type DoorStatus struct {
	OpenMagazines []string `json:"open_magazines"`
	OpenMailboxes []string `json:"open_mailboxes"`

	// Phases reports every magazine/mailbox currently mid-open/close/scan
	// (see Library.DoorPhases), keyed "magazine:<id>"/"mailbox:<id>" ->
	// "opening"|"closing"|"scanning". Live-status only - never persisted,
	// since a phase is by definition transient and always clears itself
	// once the underlying door operation returns.
	Phases map[string]string `json:"phases,omitempty"`
}

// DoorAction is one operator-selected physical action staged while a door is
// open and applied when closing that door.
type DoorAction struct {
	Action  string `json:"action"` // "load" or "pickup"
	Address int    `json:"address"`
	Barcode string `json:"barcode,omitempty"` // required for "load"
}

// Event is a single audit/notification record, surfaced through the API
// (/api/v1/events), the web UI activity log, and SNMP traps.
type Event struct {
	Time      time.Time         `json:"time"`
	Code      string            `json:"code"`
	Type      string            `json:"type"` // deprecated alias for code
	Category  string            `json:"category,omitempty"`
	Severity  string            `json:"severity,omitempty"`
	Outcome   string            `json:"outcome,omitempty"`
	Actor     string            `json:"actor,omitempty"`
	Source    string            `json:"source,omitempty"`
	Operation string            `json:"operation,omitempty"`
	Message   string            `json:"message"`
	Detail    map[string]string `json:"detail,omitempty"`
}

// Notifier is implemented by the SNMP trap sender. The library package only
// depends on this small interface so it never needs to import the snmp
// package (avoiding an import cycle and keeping SNMP optional/pluggable).
type Notifier interface {
	Notify(Event)
}

// MultiNotifier fans a single Notify call out to every non-nil child,
// letting more than one subscriber (e.g. the SNMP sender and a live-update
// broadcaster for the web UI) receive the same event without Library
// knowing about either concretely.
type MultiNotifier []Notifier

func (m MultiNotifier) Notify(e Event) {
	for _, n := range m {
		if n != nil {
			n.Notify(e)
		}
	}
}

// PhaseNotifier receives live door-phase transitions and robotic-arm
// state (see Library.SetPhaseNotifier). This is deliberately separate
// from Notifier above: a phase/arm change is a live UI signal only, not
// an auditable action - it never goes through emit/RecordEvent, so
// intermediate transitions (e.g. "closing" -> "scanning", or "moving to
// slot 3", "grabbed tape ABC123") never get persisted into the event log
// or fire an SNMP trap on every micro-step. Only the real open/close/scan
// and Move/Load/Unload started/success/failure events, already emitted
// today via Notifier, do that.
type PhaseNotifier interface {
	// NotifyPhase reports that kind ("magazine" or "mailbox") id is now in
	// phase ("opening"/"closing"/"scanning"), or phase == "" if the
	// operation just finished/cleared. Called synchronously, in order,
	// while the calling door method still holds Library's main lock -
	// implementations must return quickly and must not call back into
	// Library. This is unlike Notifier.Notify (fired via a goroutine,
	// since SNMP sending does real network I/O); a phase transition's
	// ordering matters (closing must be observed before scanning, before
	// the final clear), so delivery here is deliberately synchronous.
	NotifyPhase(kind, id, phase string)

	// NotifyArm reports the arm's current busy/position state, and
	// optionally a newly recorded live-only narration step (step's zero
	// value when this call is purely a busy/position/parked change with
	// no new step attached - see Library's setArmBusy/setArmPosition/
	// setArmDoorsOpenDelta/recordArmStep). Called synchronously under the
	// same constraints as NotifyPhase.
	NotifyArm(state ArmState, step ArmStep)
}
