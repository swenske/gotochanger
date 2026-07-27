// Package scsi implements the SMC-3 (medium changer) and SSC-3
// (sequential-access/tape drive) command subset cmd/gotochanger-tcmud
// needs, translating parsed SCSI CDBs (internal/tcmu.Entry) into calls
// against gotochangerd's existing REST API (via LibraryClient, the same
// role apiclient.Client already plays for cmd/gotochanger-changer) and
// building the tcmu.Response Cursor.Complete writes back.
//
// This covers Milestone 1's minimal command set plus Milestone 2's
// additions (see the project's kernel-mode plan): for the changer,
// INQUIRY, TEST UNIT READY, REQUEST SENSE, READ ELEMENT STATUS (including
// volume-tag/barcode reporting), MOVE MEDIUM, POSITION TO ELEMENT,
// RESERVE/RELEASE ELEMENT, PREVENT/ALLOW MEDIUM REMOVAL, SEND VOLUME
// TAG/REQUEST VOLUME ELEMENT ADDRESS, and MODE SENSE(6) page 0x1D
// (Element Address Assignment); for a drive, INQUIRY, TEST UNIT READY,
// REQUEST SENSE, LOAD/UNLOAD, READ(6), WRITE(6), WRITE FILEMARKS(6),
// REWIND, SPACE(6), LOCATE(10), and a minimal MODE SENSE(6)/MODE
// SELECT(6) - plus per-drive-family bandwidth throttling on READ(6)/
// WRITE(6) (Drive.throttle). Not yet implemented: EXCHANGE MEDIUM, ERASE,
// READ BUFFER/WRITE BUFFER (all explicitly deferred in the plan), real
// multi-partition support, and setmark tracking. Filemark positions are
// recorded in a sidecar file next to each volume's backing file (see
// Drive.writeFilemarks/filemarks.go), read by both READ(6) (stops a read
// at a filemark boundary instead of reading through it - see Drive.read6)
// and SPACE(6)'s own filemark code (see Drive.space6's own doc comment
// for the exact SCSI-2-verified forward/reverse landing semantics).
//
// Every CDB/response byte layout here was verified against an
// authoritative source (T10's ASC/ASCQ list, a mirrored SCSI-2 spec, and a
// real tape library/drive vendor SCSI reference), not reconstructed from
// memory alone, with two flagged exceptions where that wasn't possible in
// this environment: LOCATE(10)'s exact bit layout, and the volume tag
// sub-structure's internal byte split in READ ELEMENT STATUS's PVolTag=1
// descriptor (see Drive.locate and elementDescriptorLenVolTag). Real-
// initiator verification (sg3-utils, mtx, a real Bareos job) is still
// needed once this runs against a real kernel - see internal/tcmu's own
// doc comments for the same caveat one layer down.
package scsi

// SCSI CDB operation codes this project implements, split by the two
// device types cmd/gotochanger-tcmud emulates. Several opcodes are reused
// across device types with entirely different meanings (e.g. 0x2B is
// LOCATE for a tape drive but POSITION TO ELEMENT for a changer) - this is
// normal SCSI, not a collision to worry about, since each simulated LUN
// only ever receives commands meant for its own device type.
const (
	// Common to every device type (SPC) - opcode values are extremely
	// stable, decades-old SCSI-2/SPC assignments.
	OpTestUnitReady             = 0x00
	OpRequestSense              = 0x03
	OpInquiry                   = 0x12
	OpReserve6                  = 0x16
	OpRelease6                  = 0x17
	OpPreventAllowMediumRemoval = 0x1E

	// SMC-3 (medium changer). Verified against the Oracle StorageTek
	// SL150 SCSI Reference Guide's CDB byte-layout figures.
	OpPositionToElement           = 0x2B
	OpMoveMedium                  = 0xA5
	OpSendVolumeTag               = 0xB6
	OpRequestVolumeElementAddress = 0xB5
	OpReadElementStatus           = 0xB8

	// SSC-3 (sequential-access / tape drive). Opcodes/CDB layout for
	// Rewind/Read6/Write6/Space6/LoadUnload verified against a mirrored
	// SCSI-2 spec and a real tape drive vendor's own SCSI command
	// reference (LOAD UNLOAD). ModeSense6/ModeSelect6's header+block-
	// descriptor shape is long-stable SCSI-2-era SPC, not independently
	// re-verified here. Locate's exact bit layout (byte1's IMMED/CP bits,
	// the 4-byte block address at bytes 3-6, byte8 Partition) is from
	// memory at moderate-not-high confidence, not fetched from a primary
	// source like the others - see Drive.locate's doc comment.
	OpRewind         = 0x01
	OpRead6          = 0x08
	OpWrite6         = 0x0A
	OpWriteFilemarks = 0x10
	OpSpace6         = 0x11
	OpModeSelect6    = 0x15
	OpModeSense6     = 0x1A
	OpLoadUnload     = 0x1B
	OpLocate         = 0x2B
)
