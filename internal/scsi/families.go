package scsi

// DriveFamily is the SCSI-specific identity/behavior a drive family
// reports in kernel mode - kept separate from config.DriveType (used by
// both changer-command-script and kernel mode) so changer-mode's existing
// config surface stays untouched, per the kernel-mode plan's rationale for
// this split.
type DriveFamily struct {
	Identity    Identity
	NativeSpeed int64 // bytes/sec - not yet enforced as throttling (Milestone 2), but already resolvable per family
}

// DriveFamilies maps a config.DriveType.Generation string (e.g. "LTO-9",
// "DDS" - see config.DefaultDriveTypes) to its SCSI identity/behavior.
// Generations not listed here fall back to DefaultDriveFamily.
var DriveFamilies = map[string]DriveFamily{
	"LTO-8": {Identity: Identity{Vendor: "GOTOCHNG", Product: "Virtual LTO-8", Revision: "0100"}, NativeSpeed: 300_000_000},
	"LTO-9": {Identity: Identity{Vendor: "GOTOCHNG", Product: "Virtual LTO-9", Revision: "0100"}, NativeSpeed: 400_000_000},
	"DDS":   {Identity: Identity{Vendor: "GOTOCHNG", Product: "Virtual DDS", Revision: "0100"}, NativeSpeed: 12_000_000},
	"DLT":   {Identity: Identity{Vendor: "GOTOCHNG", Product: "Virtual DLT", Revision: "0100"}, NativeSpeed: 10_000_000},
}

// DefaultDriveFamily is used for any config.DriveType.Generation not
// listed in DriveFamilies - a fully generic, always-available fallback
// rather than an error, matching config.DriveType's own "Unlimited"
// catalog entry.
var DefaultDriveFamily = DriveFamily{
	Identity: Identity{Vendor: "GOTOCHNG", Product: "Virtual Tape Drive", Revision: "0100"},
}

// FamilyFor resolves generation's (a config.DriveType.Generation string)
// SCSI identity/behavior.
func FamilyFor(generation string) DriveFamily {
	if f, ok := DriveFamilies[generation]; ok {
		return f
	}
	return DefaultDriveFamily
}
