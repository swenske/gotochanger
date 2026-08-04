package scsi

// DriveFamily is the SCSI-specific identity/behavior a drive family
// reports in kernel mode - kept separate from config.DriveType (used by
// both changer-command-script and kernel mode) so changer-mode's existing
// config surface stays untouched, per the kernel-mode plan's rationale for
// this split.
type DriveFamily struct {
	Identity    Identity
	NativeSpeed int64 // bytes/sec - not yet enforced as throttling (Milestone 2), but already resolvable per family

	// DensityCode is the one-byte T10 density code REPORT DENSITY SUPPORT
	// (Milestone 3, Drive.reportDensitySupport) reports for this family.
	// The LTO values below follow the documented LTO Ultrium generation ->
	// density code progression at moderate, not high, confidence (not
	// independently re-verified against a primary T10 source in this
	// environment, same posture as Locate/ReadPosition - see
	// opcodes.go's doc comment) - real-initiator verification (sg_logs/
	// `mt`) should confirm these before relying on them. 0 (DefaultDriveFamily)
	// is reported as density code 0 ("default/unspecified"), a valid value
	// per SSC, not a placeholder bug.
	DensityCode uint8

	// RealisticIdentity (Milestone 5) is an opt-in real vendor/product
	// identity a kernel-mode drive of this family can report instead of
	// Identity, selected by config.DriveType.SCSIIdentity ==
	// config.SCSIIdentityRealistic (see cmd/gotochanger-tcmud's own
	// drive-construction loop, the only place this is consulted - nothing
	// in internal/scsi itself reads it). Zero-value (Identity{}) means "no
	// realistic profile defined for this family" - callers must check for
	// that before swapping it in, same convention DefaultChangerIdentity/
	// RealisticChangerIdentity below use.
	RealisticIdentity Identity
}

// DriveFamilies maps a config.DriveType.Generation string (e.g. "LTO-9",
// "DDS" - see config.DefaultDriveTypes) to its SCSI identity/behavior.
// Generations not listed here fall back to DefaultDriveFamily.
//
// RealisticIdentity is only populated for LTO-8/LTO-9: "IBM"/"ULT3580-TDn"
// is a long-stable, widely-published real INQUIRY string (confirmed
// against mhvtl's own etc/generate_device_conf.in device catalog, which
// lists exactly this vendor/product pattern through ULT3580-TD8; TD9 is a
// high-confidence extrapolation of that same, completely regular IBM
// Ultrium naming convention, not independently confirmed for a real
// LTO-9 drive). The Revision field on both is left as this package's own
// placeholder ("0100"), not a real IBM firmware revision string - vendor/
// product are what real compatibility checks actually key on, and this
// package has no genuine firmware revision to report anyway. DDS/DLT are
// left with a zero-value RealisticIdentity
// deliberately - unlike LTO, no single real vendor/model string for
// those families was confirmed at the same confidence during this
// addition (DDS/DAT drives shipped from several vendors; DLT from
// Quantum but under multiple distinct model lines) - inventing one would
// be a guess this package's own convention doesn't make.
var DriveFamilies = map[string]DriveFamily{
	"LTO-8": {Identity: Identity{Vendor: "GOTOCHNG", Product: "Virtual LTO-8", Revision: "0100"}, NativeSpeed: 300_000_000, DensityCode: 0x5E, RealisticIdentity: Identity{Vendor: "IBM", Product: "ULT3580-TD8", Revision: "0100"}},
	"LTO-9": {Identity: Identity{Vendor: "GOTOCHNG", Product: "Virtual LTO-9", Revision: "0100"}, NativeSpeed: 400_000_000, DensityCode: 0x60, RealisticIdentity: Identity{Vendor: "IBM", Product: "ULT3580-TD9", Revision: "0100"}},
	"DDS":   {Identity: Identity{Vendor: "GOTOCHNG", Product: "Virtual DDS", Revision: "0100"}, NativeSpeed: 12_000_000, DensityCode: 0x25},
	"DLT":   {Identity: Identity{Vendor: "GOTOCHNG", Product: "Virtual DLT", Revision: "0100"}, NativeSpeed: 10_000_000, DensityCode: 0x19},
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

// DefaultChangerIdentity/RealisticChangerIdentity (Milestone 5) are the
// changer's own counterpart to DriveFamily.Identity/RealisticIdentity
// above, consulted the same way (by cmd/gotochanger-tcmud, via
// config.LogicalLibraryConfig.ChangerModel ==
// config.ChangerModelRealistic) - see Changer.Identity's own doc comment
// for how a Changer falls back to DefaultChangerIdentity when its own
// Identity field is left unset. RealisticChangerIdentity reports as an
// Oracle StorageTek SL150 - the same real device this package's own SMC-3
// CDB/response byte layouts were already verified against (see
// opcodes.go/changer.go's doc comments), so opting into it is reporting
// as the exact device this project's changer emulation was built to
// match, not an arbitrary pick.
var (
	DefaultChangerIdentity   = Identity{Vendor: "GOTOCHNG", Product: "Virtual Changer", Revision: "0100"}
	RealisticChangerIdentity = Identity{Vendor: "STK", Product: "SL150", Revision: "0100"}
)
