package scsi

import (
	"encoding/binary"

	"github.com/swenske/gotochanger/internal/library"
)

// LOG SENSE page codes this package implements.
const (
	logPageSupportedPages = 0x00
	logPageTapeAlert      = 0x2E
)

// logParameter is one LOG SENSE parameter: a parameter code (SPC calls
// this the "log parameter code") plus, for a TapeAlert-style flag page,
// whether that flag is currently active.
type logParameter struct {
	code   uint16
	active bool
}

// tapeAlertParamLen is the size of one TapeAlert log parameter: a 2-byte
// parameter code, a 1-byte control byte, a 1-byte parameter length (always
// 1 for a single flag byte), and the 1-byte flag value itself.
const tapeAlertParamLen = 5

// buildLogSensePage builds a generic LOG SENSE page response: the 4-byte
// page header (page code, reserved, big-endian page length) SPC mandates,
// followed by one log parameter per entry in params, in order. This
// project always reports every entry in params (even when inactive),
// rather than omitting inactive flags - matching how a real device's
// TapeAlert page reports a fixed, stable set of supported parameter codes
// across every poll, not a variable-length "only what's currently wrong"
// list (a scanning initiator/monitoring tool - e.g. sg_logs - expects a
// consistent shape to diff against).
//
// Each parameter's control byte (byte2: DU|DS|TSD|ETC|TMC[2 bits]|
// reserved|LP) is left entirely zero - "binary format, no threshold
// comparison" - this package has no log parameter thresholds/reset
// semantics to honor (see logSelect's own doc comment). The single flag
// value byte's bit assignment (bit0 vs. bit7 for "flag active") is not
// independently re-verified against a primary source in this environment:
// two sources consulted while building this gave conflicting answers (a
// scraped read of T10/02-142r0 suggested bit7; this project's own
// moderate-confidence recollection of how sg3_utils' sg_logs decodes a
// TapeAlert page - checking the parameter value's low bit - points to
// bit0, which is what's implemented here). Needs a real `sg_logs -a`
// check against a real gotochanger-tcmud device before being trusted -
// see opcodes.go's doc comment.
func buildLogSensePage(pageCode uint8, params []logParameter) []byte {
	body := make([]byte, 0, len(params)*tapeAlertParamLen)
	for _, p := range params {
		entry := make([]byte, tapeAlertParamLen)
		binary.BigEndian.PutUint16(entry[0:2], p.code)
		entry[3] = 1 // parameter length: one flag byte follows
		if p.active {
			entry[4] = 0x01
		}
		body = append(body, entry...)
	}
	header := make([]byte, 4)
	header[1] = pageCode
	binary.BigEndian.PutUint16(header[2:4], uint16(len(body)))
	return append(header, body...)
}

// buildSupportedLogPages builds the page 0x00 (Supported Log Pages)
// response: the same 4-byte header (page code 0x00, page length = count
// of page-code bytes that follow) shape every other page in this file
// uses, then the list of supported page codes - same convention as
// vpd.go's SupportedVPDPages.
func buildSupportedLogPages(pages ...uint8) []byte {
	all := append([]uint8{logPageSupportedPages}, pages...)
	header := make([]byte, 4)
	binary.BigEndian.PutUint16(header[2:4], uint16(len(all)))
	return append(header, all...)
}

// TapeAlert flag parameter codes this package reports, verified against a
// real vendor's own published TapeAlert flag table (an IBM TS3100 Setup,
// Operator, and Service Manual's "Appendix C. TapeAlert Flags" -
// consulted directly for this addition, the same "real tape library/drive
// vendor SCSI reference" role the Oracle SL150 guide already plays
// elsewhere in this package) rather than reconstructed from memory alone.
// Drive and changer TapeAlert pages use entirely independent parameter
// code spaces (confirmed against that same source: a medium changer's own
// TapeAlert page numbers its flags starting from 1 with library-specific
// names like "Library Hardware A"/"Library Door", completely unrelated to
// a drive's 0x01-range "Read Warning"/"Hard Error" flags) - do not treat
// driveTapeAlert* and changerTapeAlert* constants as sharing one space.
const (
	driveTapeAlertWriteProtect  = 0x06 // "Write Protect": the mounted volume's write-protect tab is engaged.
	driveTapeAlertDriveCleaning = 0x0C // "Drive Cleaning": drive requires cleaning.
	driveTapeAlertHardwareA     = 0x16 // "Hardware A": drive hardware fault, matches this project's Drive.Fault/ErrDriveFault semantics.

	changerTapeAlertHardwareA   = 1  // "Library Hardware A"
	changerTapeAlertHardwareB   = 2  // "Library Hardware B"
	changerTapeAlertHardwareC   = 3  // "Library Hardware C"
	changerTapeAlertPickRetry   = 13 // "Library Pick Retry"
	changerTapeAlertPlaceRetry  = 14 // "Library Place Retry"
	changerTapeAlertLibraryDoor = 16 // "Library Door"
	changerTapeAlertInventory   = 24 // "Library Inventory"
)

// driveTapeAlertParams builds this Drive's TapeAlert flag set from
// gotochangerd's existing fault/cleaning-threshold state - no new domain
// state, just a new encoding of state this project already tracks for the
// web UI/SNMP/Prometheus (see the mhvtl feature-gap plan's own framing of
// this milestone). vol is the drive's currently mounted Volume, nil if
// empty - Write Protect is reported only while a write-protected volume
// is actually mounted, a reasonable simplification of the real flag's
// "fired on an attempted write" semantics (this project has no discrete
// write-attempt event to key off instead).
func driveTapeAlertParams(st library.Status, drv *library.Drive, vol *library.Volume) []logParameter {
	return []logParameter{
		{code: driveTapeAlertHardwareA, active: drv.Fault},
		{code: driveTapeAlertDriveCleaning, active: st.CleaningEnabled && drv.MountsSinceCleaning >= st.CleaningMountThreshold},
		{code: driveTapeAlertWriteProtect, active: vol != nil && vol.WriteProtected},
	}
}

// changerTapeAlertParams builds the Changer's TapeAlert flag set from
// gotochangerd's existing robotic-fault/door state. Deliberately does NOT
// fold in any individual Drive's own Fault - a real medium changer's own
// TapeAlert page reports changer-scoped conditions; a drive fault belongs
// on that drive's own TapeAlert page (driveTapeAlertHardwareA above), not
// here, matching the real per-device-type flag numbering this file's own
// doc comment above already established.
func changerTapeAlertParams(st library.Status) []logParameter {
	fault := st.RoboticFault
	return []logParameter{
		{code: changerTapeAlertHardwareA, active: fault.Active && fault.Kind == library.RoboticFaultBlockedArm},
		{code: changerTapeAlertHardwareB, active: fault.Active && fault.Kind == library.RoboticFaultMovementJam},
		{code: changerTapeAlertHardwareC, active: fault.Active && (fault.Kind == library.RoboticFaultOther || fault.Kind == "")},
		{code: changerTapeAlertPickRetry, active: fault.Active && fault.Kind == library.RoboticFaultPickupFailure},
		{code: changerTapeAlertPlaceRetry, active: fault.Active && fault.Kind == library.RoboticFaultDropFailure},
		{code: changerTapeAlertInventory, active: fault.Active && fault.Kind == library.RoboticFaultMispositionedCartridge},
		{code: changerTapeAlertLibraryDoor, active: len(st.Doors.OpenMagazines) > 0 || len(st.Doors.OpenMailboxes) > 0},
	}
}
