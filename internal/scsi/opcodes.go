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
// WRITE(6) (Drive.throttle).
//
// Milestone 3 (see the mhvtl feature-gap plan) adds read-only
// compatibility commands with no domain-model impact: for the changer,
// INITIALIZE ELEMENT STATUS/INITIALIZE ELEMENT STATUS WITH RANGE, and
// REZERO UNIT; for a drive, READ POSITION, READ BLOCK LIMITS, and REPORT
// DENSITY SUPPORT.
//
// Milestone 4 adds LOG SENSE (page 0x00 Supported Log Pages, page 0x2E
// TapeAlert) and a LOG SELECT accept-and-ignore stub, for both device
// types - see logsense.go. This is a re-encoding of state gotochangerd
// already tracks (Drive.Fault/MountsSinceCleaning, RoboticFault, open
// doors), not new domain state.
//
// Milestone 5 adds no new opcodes: it makes the vendor/product identity
// INQUIRY already reports opt-in configurable (real IBM/STK strings
// instead of this project's own GOTOCHNG/Virtual identity) - see
// families.go's DriveFamily.RealisticIdentity/DefaultChangerIdentity/
// RealisticChangerIdentity and Changer.Identity's own doc comments,
// config.DriveType.SCSIIdentity/LogicalLibraryConfig.ChangerModel for the
// admin-facing setting, and cmd/gotochanger-tcmud's own drive/changer
// construction for where the two are actually resolved together.
//
// Milestone 6 deliberately adds no PERSISTENT RESERVE IN/OUT (0x5E/0x5F)
// code: confirmed empirically against a real kernel (see
// Changer.reserved's own doc comment for the full sg_persist evidence)
// that Linux's LIO target core already answers these generically before
// a CDB ever reaches this package's Handle - full userspace
// implementation would be redundant, not a gap.
//
// Milestone 7 adds, for the changer, EXCHANGE MEDIUM (the classic two-
// location swap case only, simulated via a borrowed free storage slot -
// see Changer.exchangeMedium) and OPEN/CLOSE IMPORT/EXPORT ELEMENT
// (mapped onto the existing Library.OpenIODoor/CloseIODoor mailbox-door
// concept); for a drive, ERASE(6), VERIFY(6) (BYTCMP=0 only), ALLOW
// OVERWRITE (accept-and-ignore stub), and SET CAPACITY (a session-scoped
// effective-capacity clamp, not a domain-level Volume.CapacityBytes
// mutation - see Drive.setCapacity's own doc comment for why).
//
// Milestone 8 adds real multi-partition support for a drive: FORMAT
// MEDIUM (applies a partition count staged via MODE SELECT's Medium
// Partition mode page - see Drive.formatMedium/modeSelect6/
// buildMediumPartitionPage), LOCATE(16) (the modern successor to
// LOCATE(10)'s own CP-bit partition switch, which is now honored rather
// than always rejected once a volume actually has more than one
// partition - see Drive.locate/locate16), and MODE SENSE reporting the
// Medium Partition Page back. Each partition beyond 0 is a fully
// independent backing file/filemarks sidecar (see filemarks.go's
// partitionPath) - LTFS's own 2-partition (index + data) convention is
// the concrete motivation, and this project's own declared ceiling
// (maxAdditionalPartitions) is exactly 1 additional partition (2 total).
//
// Milestone 9 adds MAM (Medium Auxiliary Memory) support: READ ATTRIBUTE
// (service action 0, "attribute values", only - every other service
// action is rejected) and WRITE ATTRIBUTE, covering a deliberately small
// subset of the full T10 MAM attribute table (see mam.go's own doc
// comment for which IDs and why) - most of that table is physical
// servo/head/environmental diagnostics with no genuine analogue in a
// simulator.
//
// Milestone 10 adds real tape encryption: SECURITY PROTOCOL IN/OUT
// (security protocol 0x20, "Tape Data Encryption" only), genuinely
// encrypting/decrypting WRITE(6)/READ(6) data with AES-256-GCM (stdlib
// crypto/aes/crypto/cipher, per this project's minimal-dependency
// philosophy) - see encryption.go's own doc comment for the byte layouts
// (verified against `stenc`, a real open-source implementation) and the
// chunk/tag-sidecar design that keeps this compatible with every
// existing position-addressed backing-file assumption elsewhere in this
// package.
//
// Not yet implemented: READ BUFFER/WRITE BUFFER and setmark tracking.
// Filemark positions are recorded in a sidecar file next to each
// volume's backing file (see
// Drive.writeFilemarks/filemarks.go), read by both READ(6) (stops a read
// at a filemark boundary instead of reading through it - see Drive.read6)
// and SPACE(6)'s own filemark code (see Drive.space6's own doc comment
// for the exact SCSI-2-verified forward/reverse landing semantics).
//
// Every CDB/response byte layout here was verified against an
// authoritative source (T10's ASC/ASCQ list, a mirrored SCSI-2 spec, and a
// real tape library/drive vendor SCSI reference), not reconstructed from
// memory alone, with flagged exceptions where that wasn't possible in this
// environment: LOCATE(10)'s exact bit layout, the volume tag sub-structure's
// internal byte split in READ ELEMENT STATUS's PVolTag=1 descriptor (see
// Drive.locate and elementDescriptorLenVolTag), READ POSITION's exact
// byte0 flag-bit assignment (see Drive.readPosition), and REPORT DENSITY
// SUPPORT's descriptor body, deliberately left minimal rather than
// guessed in full (see Drive.reportDensitySupport). Real-initiator
// verification (sg3-utils, mtx, a real Bareos job) is still needed once
// this runs against a real kernel - see internal/tcmu's own doc comments
// for the same caveat one layer down.
package scsi

// SCSI CDB operation codes this project implements, split by the two
// device types cmd/gotochanger-tcmud emulates. Several opcodes are reused
// across device types with entirely different meanings (e.g. 0x2B is
// LOCATE for a tape drive but POSITION TO ELEMENT for a changer) - this is
// normal SCSI, not a collision to worry about, since each simulated LUN
// only ever receives commands meant for its own device type.
const (
	// Common to every device type (SPC) - opcode values are extremely
	// stable, decades-old SCSI-2/SPC assignments. OpLogSense/OpLogSelect
	// (Milestone 4) added for TapeAlert reporting - see logsense.go.
	OpTestUnitReady             = 0x00
	OpRequestSense              = 0x03
	OpInquiry                   = 0x12
	OpReserve6                  = 0x16
	OpRelease6                  = 0x17
	OpPreventAllowMediumRemoval = 0x1E
	OpLogSelect                 = 0x4C
	OpLogSense                  = 0x4D

	// SMC-3 (medium changer). Verified against the Oracle StorageTek
	// SL150 SCSI Reference Guide's CDB byte-layout figures - including
	// OpInitializeElementStatus/OpInitializeElementStatusWithRange, whose
	// opcodes and no-parameter-data CDB shape the SL150 guide documents
	// directly (checked specifically for this Milestone 3 addition).
	OpPositionToElement                = 0x2B
	OpMoveMedium                       = 0xA5
	OpSendVolumeTag                    = 0xB6
	OpRequestVolumeElementAddress      = 0xB5
	OpReadElementStatus                = 0xB8
	OpInitializeElementStatus          = 0x07
	OpInitializeElementStatusWithRange = 0x37

	// OpRezeroUnit (changer only) numerically collides with OpRewind
	// (drive only) - both 0x01, an SMC-2-era legacy opcode reuse. Harmless
	// per this package's own established convention (see this const
	// block's own doc comment): each simulated LUN only ever receives
	// commands meant for its own device type, so Changer.Handle and
	// Drive.Handle never see each other's dispatch table.
	OpRezeroUnit = 0x01

	// OpExchangeMedium/OpOpenCloseImportExportElement (Milestone 7):
	// opcodes/overall CDB field layout are from established SMC knowledge
	// at moderate, not high, confidence - the SL150 guide (this package's
	// usual changer-side source) documents neither opcode at all, and a
	// real SL150 doesn't implement EXCHANGE MEDIUM either (consistent
	// with it being genuinely rarely-used/optional). See Changer.
	// exchangeMedium/openCloseImportExportElement's own doc comments for
	// the exact scope limits each command's implementation accepts.
	// OpOpenCloseImportExportElement numerically collides with
	// OpLoadUnload (drive-only) - harmless, same established convention
	// as OpRezeroUnit/OpRewind above.
	OpExchangeMedium               = 0xA6
	OpOpenCloseImportExportElement = 0x1B

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

	// OpReadBlockLimits/OpReadPosition/OpReportDensitySupport (Milestone
	// 3): ReadBlockLimits' opcode/response shape is long-stable SCSI-2 SPC,
	// high confidence. ReadPosition's opcode and overall CDB/response
	// shape (short-form, 20-byte response) are high confidence; its exact
	// byte0 status-flag bit assignment is not independently re-verified
	// here - see Drive.readPosition's own doc comment, same posture as
	// Locate's already-flagged exception above. ReportDensitySupport's
	// opcode/CDB is high confidence; this package deliberately implements
	// only a minimal single-descriptor response rather than guessing the
	// full descriptor body from memory - see Drive.reportDensitySupport.
	OpReadBlockLimits      = 0x05
	OpReadPosition         = 0x34
	OpReportDensitySupport = 0x44

	// OpErase/OpVerify6/OpAllowOverwrite/OpSetCapacity (Milestone 7):
	// opcodes are long-stable, high-confidence SCSI-2/SSC-2 assignments.
	// Verify6's BYTCMP-driven data-out phase and SetCapacity's PROPORTION
	// field are implemented with documented scope limits - see
	// Drive.verify6/setCapacity's own doc comments.
	OpErase          = 0x19
	OpVerify6        = 0x13
	OpAllowOverwrite = 0x82
	OpSetCapacity    = 0x0B

	// OpFormatMedium/OpLocate16 (Milestone 8, multi-partition support).
	// FormatMedium's opcode is long-stable, high-confidence SCSI-2/SSC-2;
	// its FORMAT field's exact encoding is only partially implemented
	// (00b only - see Drive.formatMedium). Locate16's opcode (92h) was
	// confirmed against T10's own numeric opcode listing
	// (t10.org/lists/op-num.htm); its exact CDB byte/bit layout is from
	// established SSC knowledge at moderate, not high, confidence - two
	// attempts at a primary source (a T10 SSC draft, an IBM LTO SCSI
	// Reference) were both unreachable in this environment (403/PDF-
	// extraction failures) - see Drive.locate16's own doc comment.
	OpFormatMedium = 0x04
	OpLocate16     = 0x92

	// OpReadAttribute/OpWriteAttribute (Milestone 9, MAM support) -
	// opcodes and CDB byte layout verified against sg3_utils'
	// sg_read_attr.c/sg_write_attr.c (a real, working open-source
	// implementation of both commands - see mam.go's own doc comment),
	// high confidence.
	OpReadAttribute  = 0x8C
	OpWriteAttribute = 0x8D

	// OpSecurityProtocolIn/OpSecurityProtocolOut (Milestone 10, real tape
	// encryption) - opcodes and CDB byte layout verified against `stenc`
	// (github.com/scsitape/stenc, a real, working open-source SCSI Tape
	// Encryption Manager - see encryption.go's own doc comment), high
	// confidence.
	OpSecurityProtocolIn  = 0xA2
	OpSecurityProtocolOut = 0xB5
)
