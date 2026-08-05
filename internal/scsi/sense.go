package scsi

// Sense keys (SPC), the subset used by this project's handlers.
const (
	SenseNoSense        = 0x0
	SenseNotReady       = 0x2
	SenseIllegalRequest = 0x5
	SenseBlankCheck     = 0x8
	SenseDataProtect    = 0x7
	SenseAbortedCommand = 0xB
	SenseVolumeOverflow = 0xD
)

// Additional sense code / additional sense code qualifier pairs, verified
// against T10's numeric ASC/ASCQ assignment list (t10.org/lists/asc-num.htm)
// rather than reconstructed from memory.
const (
	AscNoAdditionalSenseInfo  = 0x00
	AscqNoAdditionalSenseInfo = 0x00

	// Reported (sense key NoSense, EOM flag set - see senseFlags) when a
	// WRITE lands exactly at/past the point Volume.Full simulates "early
	// warning"/hard end-of-tape - see Drive.write6.
	AscEndOfPartitionMediumDetected  = 0x00
	AscqEndOfPartitionMediumDetected = 0x02

	AscLogicalUnitNotReady = 0x04
	AscqCauseNotReportable = 0x00

	// AscqManualInterventionRequired pairs with AscLogicalUnitNotReady for
	// a drive/robotic fault - real medium changers report this when an
	// operator has to physically intervene (matches this project's own
	// model: a raised drive/robotic fault is only ever cleared from the
	// dashboard, not automatically). See senseForLibraryError.
	AscqManualInterventionRequired = 0x03

	// AscCleaningFailure/AscqCleaningFailure report a cleaning-cartridge
	// operation that couldn't proceed (expired, pool full, or no usable
	// tape found) - distinct from a mechanical/robotic fault. See
	// senseForLibraryError.
	AscCleaningFailure  = 0x30
	AscqCleaningFailure = 0x07

	// AscIncompatibleMedium/AscqIncompatibleMedium (T10's "INCOMPATIBLE
	// MEDIUM INSTALLED") report a Load rejected by library.Library.Load's
	// family-compatibility check (library.ErrIncompatibleTapeFamily) - the
	// command is fundamentally invalid given the loaded medium's format,
	// the same category as AscWriteProtected below, not a "needs operator/
	// mechanical intervention" condition. See senseForLibraryError.
	AscIncompatibleMedium  = 0x30
	AscqIncompatibleMedium = 0x00

	AscInvalidCommandOperationCode  = 0x20
	AscqInvalidCommandOperationCode = 0x00

	AscInvalidFieldInCDB  = 0x24
	AscqInvalidFieldInCDB = 0x00

	AscMediumNotPresent  = 0x3A
	AscqMediumNotPresent = 0x00

	// AscWriteProtected/AscqWriteProtected report a WRITE(6) rejected
	// because the mounted volume's simulated write-protect tab is engaged
	// (Volume.WriteProtected) - real SCSI 0x27/0x00, paired with sense key
	// SenseDataProtect. See Drive.write6.
	AscWriteProtected  = 0x27
	AscqWriteProtected = 0x00

	AscMediumDestinationElementFull  = 0x3B
	AscqMediumDestinationElementFull = 0x0D

	AscMediumSourceElementEmpty  = 0x3B
	AscqMediumSourceElementEmpty = 0x0E

	AscMechanicalPositioningOrChangerError  = 0x3B
	AscqMechanicalPositioningOrChangerError = 0x16
)

// senseFlags are the FILEMARK/EOM/ILI bits packed into fixed-format sense
// byte 2 alongside the sense key - see FixedSense.
type senseFlags struct {
	Filemark bool
	EOM      bool
	ILI      bool
}

// fixedSenseLength is the size of the fixed-format sense buffer this
// project always generates (response code 0x70, "current errors") - SPC's
// minimum mandatory length, and the only format any of this project's
// handlers produce.
const fixedSenseLength = 18

// FixedSense builds a fixed-format sense buffer per SPC's stable,
// decades-old sense-data layout: byte0 response code 0x70, byte2 sense key
// (+ FILEMARK/EOM/ILI flags), byte7 additional sense length, byte12/13
// ASC/ASCQ.
func FixedSense(key uint8, asc, ascq uint8, flags senseFlags) []byte {
	b := make([]byte, fixedSenseLength)
	b[0] = 0x70
	b[2] = key & 0x0F
	if flags.Filemark {
		b[2] |= 0x80
	}
	if flags.EOM {
		b[2] |= 0x40
	}
	if flags.ILI {
		b[2] |= 0x20
	}
	b[7] = fixedSenseLength - 8
	b[12] = asc
	b[13] = ascq
	return b
}

// FixedSenseWithInfo is FixedSense plus the 4-byte INFORMATION field
// (byte0's VALID bit set) - used by Drive's READ(6) short-read/ILI
// reporting to carry the requested-minus-actual residual count.
func FixedSenseWithInfo(key uint8, asc, ascq uint8, flags senseFlags, info uint32) []byte {
	b := FixedSense(key, asc, ascq, flags)
	b[0] |= 0x80
	b[3] = byte(info >> 24)
	b[4] = byte(info >> 16)
	b[5] = byte(info >> 8)
	b[6] = byte(info)
	return b
}
