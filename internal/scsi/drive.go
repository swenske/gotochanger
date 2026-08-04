package scsi

import (
	"encoding/binary"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/swenske/gotochanger/internal/library"
	"github.com/swenske/gotochanger/internal/tcmu"
)

// Drive implements every SSC-3 command in scope through Milestone 2 (see
// opcodes.go's package doc comment for the full list) against a
// LibraryClient - one Drive per emulated tape drive. Every CDB byte layout
// here was verified against a mirrored SCSI-2 spec text and a real tape
// drive vendor's own SCSI command reference, not reconstructed from memory
// alone (see opcodes.go's doc comment) - except LOCATE, whose exact bit
// layout is from memory at moderate confidence, flagged on Drive.locate
// itself.
//
// READ/WRITE(6) perform real file I/O directly against the mounted
// volume's backing file (found via volume(), see its own doc comment)
// rather than proxying every byte through gotochangerd, throttled to
// this Drive's
// Family.NativeSpeed (see throttle) - this is what finally makes bandwidth
// throttling and genuine read/write interception possible (see the
// kernel-mode plan's rationale), and it reuses gotochangerd's own inotify
// activity-watcher/capacity-poll mechanisms unmodified, since they're
// watching the same backing file regardless of which process writes to
// it.
//
// Milestone 1 deliberately treats every READ/WRITE(6) TRANSFER LENGTH
// field as a literal byte count, regardless of the CDB's own Fixed bit -
// this is exactly correct for variable-block mode (Fixed=0, what real
// backup software like Bareos actually uses against a tape drive), and is
// a reasonable simplification for fixed-block mode (Fixed=1) until MODE
// SELECT/block-descriptor support exists in a later milestone.
type Drive struct {
	Client LibraryClient
	Index  int // this drive's physical index, as gotochangerd's REST API addresses it
	Family DriveFamily

	// NAA is this drive's 8-byte Device Identification VPD (page 0x83)
	// identifier - see inquiry's own doc comment and vpd.go. Set once at
	// construction (cmd/gotochanger-tcmud derives it from this device's
	// own unique backstore name, the same input deviceWWN already uses
	// for its own, unrelated internal fabric identity).
	NAA [8]byte

	position  int64 // current byte offset into the mounted volume's *current partition* (see partition below); reset to 0 by REWIND or a successful LOAD
	lastSense []byte

	// sessionCapacityLimitBytes is SET CAPACITY's (Milestone 7) own
	// session-scoped state - see Drive.setCapacity's own doc comment for
	// why this is a session clamp, not a Volume.CapacityBytes mutation.
	// 0 means "no session limit set" (use the mounted Volume's own
	// CapacityBytes as-is) - reset by loadUnload's Load case, matching
	// real SET CAPACITY's own "lasts until the volume is unloaded"
	// semantics.
	sessionCapacityLimitBytes int64

	// partition (Milestone 8) is which SSC partition d.position is
	// currently offset within - 0 (the default, and every volume's only
	// partition before this milestone existed) always uses the mounted
	// Volume's own Path unchanged; a real switch only ever happens via
	// LOCATE(10)'s CP bit or LOCATE(16) (see Drive.locate/locate16), both
	// gated on the mounted volume actually having more than one
	// partition. Reset to 0 by loadUnload's Load case (a fresh mount
	// always starts at partition 0, BOT) - REWIND deliberately does NOT
	// reset it, matching real SSC semantics (REWIND never changes
	// partition).
	partition int

	// pendingPartitionCount (Milestone 8) is FORMAT MEDIUM's own staged
	// state: the partition count most recently written via MODE SELECT's
	// Medium Partition mode page (see Drive.modeSelect6), applied for
	// real (persisted, via Client.SetDriveVolumeNumberOfPartitions) the
	// next time FORMAT MEDIUM actually runs (see Drive.formatMedium) - a
	// real drive stages a MODE SELECT the same way, only taking effect
	// once FORMAT MEDIUM is issued. 0 means "nothing staged yet" (FORMAT
	// MEDIUM then defaults to a single ordinary partition). Session-
	// scoped like sessionCapacityLimitBytes, reset by loadUnload's Load
	// case for the same reason: an un-applied MODE SELECT from a
	// previous mount shouldn't silently apply to a different cartridge.
	pendingPartitionCount int

	// encryptMode/decryptMode/algorithmIndex/encryptionKey/
	// keyInstanceCounter (Milestone 10) are SECURITY PROTOCOL OUT's own
	// session-scoped state - see encryption.go's own doc comment for the
	// byte layouts and Drive.encryptionActive for how they're consulted
	// together. Reset by loadUnload's Load case, matching real tape
	// encryption's own "the initiator's key manager must re-supply the
	// key every session" semantics - nothing about a real drive persists
	// a key either.
	encryptMode        uint8
	decryptMode        uint8
	algorithmIndex     uint8
	encryptionKey      []byte
	keyInstanceCounter uint32
}

func (d *Drive) Handle(entry tcmu.Entry) tcmu.Response {
	if len(entry.CDB) == 0 {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	switch entry.CDB[0] {
	case OpTestUnitReady:
		return d.testUnitReady()
	case OpRequestSense:
		return d.requestSense(entry.Buffers)
	case OpInquiry:
		return d.inquiry(entry.CDB, entry.Buffers)
	case OpRewind:
		return d.rewind()
	case OpLoadUnload:
		return d.loadUnload(entry.CDB)
	case OpRead6:
		return d.read6(entry.CDB, entry.Buffers)
	case OpWrite6:
		return d.write6(entry.CDB, entry.Buffers)
	case OpWriteFilemarks:
		return d.writeFilemarks(entry.CDB)
	case OpSpace6:
		return d.space6(entry.CDB)
	case OpLocate:
		return d.locate(entry.CDB)
	case OpLocate16:
		return d.locate16(entry.CDB)
	case OpModeSense6:
		return d.modeSense6(entry.CDB, entry.Buffers)
	case OpModeSelect6:
		return d.modeSelect6(entry.CDB, entry.Buffers)
	case OpFormatMedium:
		return d.formatMedium(entry.CDB)
	case OpReadPosition:
		return d.readPosition(entry.CDB, entry.Buffers)
	case OpReadBlockLimits:
		return d.readBlockLimits(entry.Buffers)
	case OpReportDensitySupport:
		return d.reportDensitySupport(entry.CDB, entry.Buffers)
	case OpLogSense:
		return d.logSense(entry.CDB, entry.Buffers)
	case OpLogSelect:
		return d.logSelect(entry.CDB)
	case OpErase:
		return d.erase(entry.CDB)
	case OpVerify6:
		return d.verify6(entry.CDB)
	case OpAllowOverwrite:
		return d.allowOverwrite(entry.CDB)
	case OpSetCapacity:
		return d.setCapacity(entry.CDB)
	case OpReadAttribute:
		return d.readAttribute(entry.CDB, entry.Buffers)
	case OpWriteAttribute:
		return d.writeAttribute(entry.CDB, entry.Buffers)
	case OpSecurityProtocolIn:
		return d.securityProtocolIn(entry.CDB, entry.Buffers)
	case OpSecurityProtocolOut:
		return d.securityProtocolOut(entry.CDB, entry.Buffers)
	default:
		return d.fail(SenseIllegalRequest, AscInvalidCommandOperationCode, AscqInvalidCommandOperationCode, senseFlags{})
	}
}

func (d *Drive) ok() tcmu.Response { return tcmu.Response{Status: StatusGood} }

func (d *Drive) fail(key, asc, ascq uint8, flags senseFlags) tcmu.Response {
	d.lastSense = FixedSense(key, asc, ascq, flags)
	return tcmu.Response{Status: StatusCheckCondition, Sense: d.lastSense}
}

// volume fetches this drive's currently mounted Volume (nil if empty).
// volume looks up this drive's currently-loaded volume by matching
// st.Drives entries against d.Index (the drive's real physical index),
// never by direct array position. found the hard way, against a real
// kernel: st.Drives comes from d.Client.Status(), which - once
// gotochanger-tcmud is scoped to one logical library via
// --logical-library - only contains that library's own drives,
// positioned by array order starting at 0, not by physical index. A
// logical library whose drives don't happen to start at physical index 0
// (e.g. Library2's drives 4/5, in a real multi-library deployment) used
// to hit `d.Index >= len(st.Drives)` on literally every call - "drive 4
// not found" against a 2-element slice - which testUnitReady turned into
// CHECK CONDITION/NOT READY/LOGICAL UNIT NOT READY/CAUSE NOT REPORTABLE
// (ASC 0x04/ASCQ 0x00), a sense combination the Linux `st` driver's own
// check_tape() treats as "still initializing, keep retrying" rather than
// a definitive failure - so a real `mt status`/open() against that
// drive blocked in msleep_interruptible inside check_tape() forever,
// with a real cartridge sitting loaded in the drive the whole time. This
// was invisible in every earlier real-hardware pass because those only
// ever exercised changer-level commands (MOVE MEDIUM, READ ELEMENT
// STATUS - both go through internal/scsi.Changer, calling gotochangerd's
// HTTP API directly, never through this array) against a scoped
// multi-library topology; drive-level I/O (TEST UNIT READY, READ,
// WRITE, ...) was only ever tested unscoped or against Library1, whose
// drives happen to be physically contiguous from index 0, masking this
// by coincidence - the same class of "physical index vs. scoped array
// position" bug this project has hit and fixed several times elsewhere
// (see CLAUDE.md's "Known gotchas" #10).
func (d *Drive) volume() (*library.Volume, error) {
	st, err := d.Client.Status()
	if err != nil {
		return nil, err
	}
	drv, err := driveRecord(st, d.Index)
	if err != nil {
		return nil, err
	}
	return drv.Volume, nil
}

// driveRecord finds this Drive's own library.Drive record within st by
// physical index - the exact same physical-index-not-array-position
// lookup volume() has always needed (see its own doc comment above for
// why), factored out so logSense can reuse it for fields beyond just the
// mounted Volume (Fault, MountsSinceCleaning).
func driveRecord(st library.Status, index int) (*library.Drive, error) {
	for _, drv := range st.Drives {
		if drv.Index == index {
			return drv, nil
		}
	}
	return nil, fmt.Errorf("drive %d not found", index)
}

// currentVolPath is vol's backing file path for d's currently selected
// partition (Milestone 8, see the Drive.partition field's own doc
// comment) - partition 0 always returns vol.Path unchanged (zero
// behavior change for any pre-Milestone-8 volume).
//
// Lazily creates an empty backing file the first time any operation
// touches a partition beyond 0 that hasn't been written to yet: real
// tape has no comparable "doesn't exist yet" state once a volume is
// formatted with N partitions - every partition the Medium Partition
// Page reports already physically exists, just possibly blank - and
// every existing read6/write6/space6/erase/verify6/writeFilemarks call
// site already correctly treats a real, empty file as "blank"/BLANK
// CHECK. Doing the lazy-create here, once, keeps every one of those call
// sites' own error-handling logic completely unchanged rather than
// teaching each of them a second, not-yet-created error case.
func (d *Drive) currentVolPath(vol *library.Volume) string {
	p := partitionPath(vol.Path, d.partition)
	if d.partition > 0 {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			_ = os.WriteFile(p, nil, 0o644)
		}
	}
	return p
}

func (d *Drive) testUnitReady() tcmu.Response {
	vol, err := d.volume()
	if err != nil {
		return d.fail(SenseNotReady, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
	}
	if vol == nil {
		return d.fail(SenseNotReady, AscMediumNotPresent, AscqMediumNotPresent, senseFlags{})
	}
	return d.ok()
}

func (d *Drive) requestSense(buffers [][]byte) tcmu.Response {
	sense := d.lastSense
	if sense == nil {
		sense = FixedSense(SenseNoSense, AscNoAdditionalSenseInfo, AscqNoAdditionalSenseInfo, senseFlags{})
	}
	d.lastSense = nil
	n := writeToBuffers(buffers, sense)
	return tcmu.Response{Status: StatusGood, ReadLen: uint32(n)}
}

func (d *Drive) inquiry(cdb []byte, buffers [][]byte) tcmu.Response {
	if len(cdb) < 5 {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	if cdb[1]&0x01 != 0 { // EVPD - vital product data page requested
		switch cdb[2] {
		case vpdPageSupportedPages:
			n := writeToBuffers(buffers, SupportedVPDPages(PeripheralDeviceTypeSequentialAccess))
			return tcmu.Response{Status: StatusGood, ReadLen: uint32(n)}
		case vpdPageDeviceIdentification:
			n := writeToBuffers(buffers, DeviceIdentificationVPD(PeripheralDeviceTypeSequentialAccess, d.NAA))
			return tcmu.Response{Status: StatusGood, ReadLen: uint32(n)}
		default:
			return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
		}
	}
	resp := StandardInquiry(PeripheralDeviceTypeSequentialAccess, d.Family.Identity)
	n := writeToBuffers(buffers, resp)
	return tcmu.Response{Status: StatusGood, ReadLen: uint32(n)}
}

func (d *Drive) rewind() tcmu.Response {
	if _, err := d.volume(); err != nil {
		return d.fail(SenseNotReady, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
	}
	d.position = 0
	return d.ok()
}

// loadUnload implements SSC's own LOAD UNLOAD command - distinct from the
// changer's MOVE MEDIUM, which is what actually, physically mounts/
// dismounts a cartridge into this drive's data-transfer element (see
// Changer.moveMedium). Load=1 just confirms a cartridge is already
// resident and repositions to BOT; Load=0 (unload) is treated as a
// compatibility no-op here, since the real physical eject is MOVE MEDIUM's
// job, not this command's - flagged in the kernel-mode plan as a detail
// worth re-checking against a real initiator, not a design gap.
func (d *Drive) loadUnload(cdb []byte) tcmu.Response {
	if len(cdb) < 5 {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	load := cdb[4]&0x01 != 0
	if !load {
		return d.ok()
	}
	vol, err := d.volume()
	if err != nil {
		return d.fail(SenseNotReady, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
	}
	if vol == nil {
		return d.fail(SenseNotReady, AscMediumNotPresent, AscqMediumNotPresent, senseFlags{})
	}
	d.position = 0
	d.sessionCapacityLimitBytes = 0
	d.partition = 0
	d.pendingPartitionCount = 0
	d.encryptMode = 0
	d.decryptMode = 0
	d.algorithmIndex = 0
	d.encryptionKey = nil
	d.keyInstanceCounter = 0
	return d.ok()
}

// throttleDuration is the pure calculation behind throttle, split out so
// tests can check the math without actually sleeping: how long a real
// drive at this Drive's family native speed would take to transfer n
// bytes. Zero (no delay) when the family has no known native speed - see
// DriveFamily.NativeSpeed's doc comment (DefaultDriveFamily leaves it
// unset, so an unrecognized drive generation is never artificially slow).
func (d *Drive) throttleDuration(n int) time.Duration {
	if d.Family.NativeSpeed <= 0 || n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second / time.Duration(d.Family.NativeSpeed)
}

// throttle sleeps for throttleDuration(n) - called with the actual number
// of bytes transferred (not the number requested) right after a READ(6)/
// WRITE(6) completes its real, near-instant file I/O, so the command as a
// whole takes roughly as long as it would against real media at this
// drive family's native speed.
func (d *Drive) throttle(n int) {
	if dur := d.throttleDuration(n); dur > 0 {
		time.Sleep(dur)
	}
}

// read6 implements SSC's READ(6): CDB byte1 bit1=SILI, bytes2-4 = transfer
// length (a byte count here - see the type's doc comment). A short read
// (less data available than requested) is reported via ILI unless SILI
// suppresses it; reading with nothing left at all is BLANK CHECK, the
// correct SSC sense key for "attempted to read past the last recorded
// data on tape".
//
// A recorded filemark strictly ahead of the current position, within the
// requested range, truncates the read to stop exactly at it and reports
// FILEMARK (senseFlags.Filemark) instead of quietly reading straight
// through into whatever comes after it - added 2026-07-26 alongside
// SPACE(6)'s own filemark code, found necessary the same way: a real
// Bareos copy job needs to read exactly one job's recorded data, not
// spill into an unrelated job written later on the same volume. A
// position already sitting exactly on a recorded filemark (rather than
// approaching one) is not "hit" again here - see the `m > d.position`
// comparison below - matching how writeFilemarks/space6 land exactly at
// a marker's position, ready to read the file that starts there.
func (d *Drive) read6(cdb []byte, buffers [][]byte) tcmu.Response {
	if len(cdb) < 5 {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	sili := cdb[1]&0x02 != 0
	n := int(cdb[2])<<16 | int(cdb[3])<<8 | int(cdb[4])

	vol, err := d.volume()
	if err != nil {
		return d.fail(SenseNotReady, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
	}
	if vol == nil {
		return d.fail(SenseNotReady, AscMediumNotPresent, AscqMediumNotPresent, senseFlags{})
	}

	volPath := d.currentVolPath(vol)
	readLen := n
	hitFilemark := false
	if marks, mErr := readFilemarks(volPath); mErr == nil {
		sort.Slice(marks, func(i, j int) bool { return marks[i] < marks[j] })
		for _, m := range marks {
			if m > d.position {
				if m <= d.position+int64(n) {
					readLen = int(m - d.position)
					hitFilemark = true
				}
				break
			}
		}
	}

	// Milestone 10: an encrypted volume's recorded bytes are AES-256-GCM
	// ciphertext, not raw plaintext - decryptRange finds and authenticates
	// every chunk (see encryption.go's own doc comment) rather than a
	// plain ReadAt. cryptErr is a categorically different failure from an
	// ordinary short read/BLANK CHECK: an existing chunk failed to
	// authenticate against d's current key (wrong key, or no key set at
	// all - see Drive.openChunk) - reported as a distinct security-class
	// CHECK CONDITION instead of silently returning garbage or the wrong
	// sense entirely.
	var buf []byte
	var read int
	if vol.Encrypted {
		pt, cryptErr := d.decryptRange(volPath, d.position, int64(readLen))
		if cryptErr {
			return d.failSecurity()
		}
		buf = pt
		read = len(pt)
	} else {
		f, err := os.Open(volPath)
		if err != nil {
			return d.fail(SenseAbortedCommand, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
		}
		defer f.Close()
		buf = make([]byte, readLen)
		read, _ = f.ReadAt(buf, d.position)
	}
	if read == 0 && n > 0 && !hitFilemark {
		return d.fail(SenseBlankCheck, AscNoAdditionalSenseInfo, AscqNoAdditionalSenseInfo, senseFlags{})
	}
	d.position += int64(read)
	written := writeToBuffers(buffers, buf[:read])
	d.throttle(read)

	if hitFilemark && read == readLen {
		d.lastSense = FixedSenseWithInfo(SenseNoSense, AscNoAdditionalSenseInfo, AscqNoAdditionalSenseInfo, senseFlags{Filemark: true}, uint32(n-read))
		return tcmu.Response{Status: StatusCheckCondition, Sense: d.lastSense, ReadLen: uint32(written)}
	}
	if read < n && !sili {
		d.lastSense = FixedSenseWithInfo(SenseNoSense, AscNoAdditionalSenseInfo, AscqNoAdditionalSenseInfo, senseFlags{ILI: true}, uint32(n-read))
		return tcmu.Response{Status: StatusCheckCondition, Sense: d.lastSense, ReadLen: uint32(written)}
	}
	return tcmu.Response{Status: StatusGood, ReadLen: uint32(written)}
}

// write6 implements SSC's WRITE(6): CDB bytes2-4 = transfer length (a byte
// count - see the type's doc comment). Capacity/end-of-tape is enforced
// here, directly, before any byte is written - VOLUME OVERFLOW is the
// correct SSC sense key for "write couldn't complete because the medium
// is full", paired with END-OF-PARTITION/MEDIUM DETECTED and the EOM flag.
func (d *Drive) write6(cdb []byte, buffers [][]byte) tcmu.Response {
	if len(cdb) < 5 {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	n := int(cdb[2])<<16 | int(cdb[3])<<8 | int(cdb[4])

	vol, err := d.volume()
	if err != nil {
		return d.fail(SenseNotReady, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
	}
	if vol == nil {
		return d.fail(SenseNotReady, AscMediumNotPresent, AscqMediumNotPresent, senseFlags{})
	}
	if vol.WriteProtected {
		return d.fail(SenseDataProtect, AscWriteProtected, AscqWriteProtected, senseFlags{})
	}
	if cap := d.effectiveCapacity(vol); cap > 0 && d.position+int64(n) > cap {
		return d.fail(SenseVolumeOverflow, AscEndOfPartitionMediumDetected, AscqEndOfPartitionMediumDetected, senseFlags{EOM: true})
	}

	data := make([]byte, 0, n)
	for _, buf := range buffers {
		if len(data) >= n {
			break
		}
		take := n - len(data)
		if take > len(buf) {
			take = len(buf)
		}
		data = append(data, buf[:take]...)
	}

	volPath := d.currentVolPath(vol)

	// Milestone 10: encryption is decided once, at BOT, for the whole
	// recording pass (see library.Volume.Encrypted's own doc comment) -
	// re-checked (and persisted for real, via SetDriveVolumeEncrypted)
	// only at d.position==0 rather than on every single WRITE(6), so an
	// ordinary multi-call sequential write doesn't pay a REST round trip
	// per call. Switching encryption on/off mid-recording-pass (without a
	// rewind) is a real, accepted simplification this project doesn't
	// track correctly - the volume's own Encrypted flag reflects only
	// whatever was true at BOT, and read6 will attempt to decrypt (or
	// not) the *entire* volume uniformly based on that one flag.
	if d.position == 0 {
		wantEncrypted := d.encryptionActive()
		if vol.Encrypted != wantEncrypted {
			if err := d.Client.SetDriveVolumeEncrypted(d.Index, wantEncrypted); err != nil {
				return d.fail(SenseAbortedCommand, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
			}
			vol.Encrypted = wantEncrypted
		}
	}

	writeData := data
	var tag []byte
	if vol.Encrypted {
		if !d.encryptionActive() {
			// The volume is (or is about to become, at this same BOT
			// write above) encrypted, but this session has no usable key
			// - a real drive can't write encrypted data it has no key
			// for either.
			return d.failSecurity()
		}
		ciphertext, sealTag, err := d.sealChunk(d.position, data)
		if err != nil {
			return d.fail(SenseAbortedCommand, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
		}
		writeData = ciphertext
		tag = sealTag
	}

	f, err := os.OpenFile(volPath, os.O_WRONLY, 0)
	if err != nil {
		return d.fail(SenseAbortedCommand, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
	}
	defer f.Close()
	if _, err := f.WriteAt(writeData, d.position); err != nil {
		return d.fail(SenseAbortedCommand, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
	}
	if tag != nil {
		if err := appendEncTag(volPath, encTag{start: d.position, length: int64(len(writeData)), tag: tag}); err != nil {
			return d.fail(SenseAbortedCommand, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
		}
	}
	// Real data written at d.position makes any filemark strictly beyond
	// this point stale - see invalidateFilemarksFrom's own doc comment,
	// including why this must NOT also drop a mark exactly at d.position
	// (that's the ordinary, expected case of writing the next file right
	// after the previous one's own filemark - dropping it broke every
	// real multi-file sequential write, found against a real kernel).
	_ = invalidateFilemarksFrom(volPath, d.position)
	// It makes every *byte* beyond this write stale too - end-of-data now
	// sits immediately after it, exactly as on real tape. See
	// truncateToEOD; a no-op for an ordinary sequential append, where the
	// file already ends where this write did.
	_ = truncateToEOD(volPath, d.position+int64(len(writeData)))
	d.position += int64(len(data))
	d.throttle(len(data))
	return d.ok()
}

// writeFilemarks implements SSC's WRITE FILEMARKS(6) (0x10): CDB byte1
// bit0=IMMED (ignored - every command here already completes
// synchronously), bit1=WSMK (write setmarks instead of filemarks - not
// distinguished, see below), bytes2-4=TRANSFER LENGTH, a genuine count of
// filemarks to write here, unlike READ/WRITE(6)'s byte-count TRANSFER
// LENGTH (see the type's doc comment) - CDB byte layout verified against
// the same mirrored SCSI-2 spec text this package's other SSC commands
// already cite.
//
// Found missing for real, not defensively: this opcode had no handler at
// all before 2026-07-26 (fell through to the default "unsupported
// opcode" response) - a real Bareos backup job against bareos-disk-sd-
// int-fr1 failed outright because of it: Bareos always writes a filemark
// to close out each job (and a second one immediately after, to mark
// logical end-of-tape), surfacing as "ioctl MTWEOF error ... ERR=Input/
// output error" and the whole job aborting. Every other write path in
// this package already worked; this single missing opcode blocked real
// backup software from completing any job at all.
//
// Unlike WRITE(6), this leaves d.position unchanged: this project's
// backing file represents only real recorded data, so a filemark never
// consumes byte-stream space the way it physically consumes tape length
// on a real drive - it's purely a marker recorded at the position it was
// written (see recordFilemark/filemarks.go), and the next WRITE(6)
// continues from that same position, which is exactly why a filemark's
// recorded position always lands at the boundary between the file that
// preceded it and whatever gets written after it. WSMK is intentionally
// not distinguished from a plain filemark - this project has no separate
// setmark concept, and no real backup software this project targets
// (Bareos) ever writes one.
func (d *Drive) writeFilemarks(cdb []byte) tcmu.Response {
	if len(cdb) < 5 {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	count := int(cdb[2])<<16 | int(cdb[3])<<8 | int(cdb[4])

	vol, err := d.volume()
	if err != nil {
		return d.fail(SenseNotReady, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
	}
	if vol == nil {
		return d.fail(SenseNotReady, AscMediumNotPresent, AscqMediumNotPresent, senseFlags{})
	}
	if vol.WriteProtected {
		return d.fail(SenseDataProtect, AscWriteProtected, AscqWriteProtected, senseFlags{})
	}

	// A filemark actually written to the medium puts end-of-data right
	// after itself, so everything the previous pass recorded beyond
	// d.position is gone - see truncateToEOD. This is what makes the
	// standard `mt rewind; mt weof` erase idiom genuinely erase a volume,
	// and what makes Bareos's own "write the label at BOT" reclaim a
	// recycled volume's full capacity. Guarded on count > 0: WRITE
	// FILEMARKS with a zero transfer length writes nothing at all (it's
	// the conventional flush-the-drive's-buffer no-op), so it must not
	// move end-of-data either.
	volPath := d.currentVolPath(vol)
	if count > 0 {
		if err := truncateToEOD(volPath, d.position); err != nil {
			return d.fail(SenseAbortedCommand, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
		}
	}

	for i := 0; i < count; i++ {
		if err := recordFilemark(volPath, d.position); err != nil {
			return d.fail(SenseAbortedCommand, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
		}
	}
	return d.ok()
}

// truncateToEOD moves a volume's end-of-data to eod, discarding anything
// recorded beyond it - the byte-stream counterpart of what
// invalidateFilemarksFrom already does for filemark structure.
//
// Real magnetic tape offers no way to record at one position while keeping
// data further along the medium: end-of-data always ends up immediately
// after whatever was just written, and the previous pass's data past that
// point is gone. This project's backing file *is* the recorded data, and
// end-of-data is read straight off its size - see space6's own
// end-of-data and ran-out-of-filemarks cases, and the library's
// refreshVolumeSizeLocked, which derives a volume's WrittenBytes/Full the
// same way - so without this, rewinding and overwriting a volume left
// every stale byte of the previous pass logically on the tape, inside the
// volume's own end-of-data and past the newly written data.
//
// Found on the real deployment (2026-07-26, bareos-disk-sd-int-fr1), not
// preemptively: a real `mt rewind; mt weof` followed by a real Bareos
// `label barcodes` against a 3.1GB volume left all 3.1GB in place behind
// the fresh 64512-byte label. Bareos's own next space-to-end-of-data
// would land 3.1GB in while the label's trailing filemark sat at 64512,
// so the job written there could not be positioned to by file number;
// gotochangerd also still reported the freshly-labeled tape as 3.1GB of
// its 12GB used, and the operator's second attempt to erase it by hand
// failed outright ("Cannot label Volume because it is already labeled"),
// because writing a filemark never touched the label block either.
//
// Only ever shrinks, never extends: d.position can legitimately sit past
// end-of-data (LOCATE accepts any absolute block address), and growing
// the backing file to meet it would invent zero-filled recorded data that
// was never written.
func truncateToEOD(volPath string, eod int64) error {
	fi, err := os.Stat(volPath)
	if err != nil {
		return err
	}
	if fi.Size() <= eod {
		return nil
	}
	return os.Truncate(volPath, eod)
}

// space6 implements SSC's SPACE(6): CDB byte1 bits 0-2 = Code (0=blocks,
// 1=filemarks, 3=end-of-data - setmarks are still not implemented, this
// project has no separate setmark concept), bytes2-4 = Count, a signed
// 24-bit two's-complement value (negative spaces backward). "Blocks" is
// byte count here, consistent with this type's byte-count model - see
// the type's doc comment.
//
// Code=1 (filemarks) byte layout and forward/reverse landing semantics
// verified against the same mirrored SCSI-2 spec text this package's
// other SSC commands already cite: "a positive value N ... shall cause
// forward positioning ... over N ... filemarks ... ending on the
// end-of-partition side of the last filemark" and "a negative value -N
// ... reverse positioning ... ending on the beginning-of-partition side
// of the last filemark" - both land at that filemark's own recorded
// position (see filemarkForward/filemarkReverse), just approached from
// opposite directions, which for a plain byte-offset position model
// (this project's, throughout) is the same numeric value either way.
// Running out of real filemarks going *forward* before satisfying the
// requested count is BLANK CHECK with the spec's own residual-count
// convention (VALID bit + INFO field = requested minus actual, the same
// FixedSenseWithInfo primitive read6's own short-read path already
// uses) - verified against the same spec text ("If end-of-data is
// encountered while spacing ... CHECK CONDITION ... BLANK CHECK ...
// information field ... requested count minus the actual number ...
// spaced over"). Running out going *reverse* (past beginning-of-medium)
// is instead silently clamped to position 0 with no error at all,
// deliberately mirroring code=0's own existing asymmetric convention two
// cases below (backward block-spacing past BOT is also a silent clamp,
// never an error) - kept consistent with its own sibling case in this
// same function rather than independently re-deriving a "more correct"
// reverse-BOM error convention the rest of this function doesn't use
// either.
//
// Found necessary for real, not preemptively: a real Bareos copy job
// (2026-07-26, bareos-disk-sd-int-fr1) needs this to seek to the
// specific file it wants to copy - Bareos's own device backend also
// turns out to implement "space to end of data" (used after loading a
// volume for an append) as SPACE(6) code=1 with an enormous count
// (rather than code=3, which this project already had) when the device
// isn't reporting hardware end-of-medium support, confirmed against a
// real job log showing a literal count of 8388607 (2^23-1, the max
// positive 24-bit signed value) - so this same code path also fixes the
// "number of files mismatch" warning real backup jobs were hitting
// before this, not just copy/restore.
func (d *Drive) space6(cdb []byte) tcmu.Response {
	if len(cdb) < 5 {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	code := cdb[1] & 0x07

	vol, err := d.volume()
	if err != nil {
		return d.fail(SenseNotReady, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
	}
	if vol == nil {
		return d.fail(SenseNotReady, AscMediumNotPresent, AscqMediumNotPresent, senseFlags{})
	}
	volPath := d.currentVolPath(vol)

	switch code {
	case 0: // blocks
		raw := uint32(cdb[2])<<16 | uint32(cdb[3])<<8 | uint32(cdb[4])
		count := int64(raw)
		if raw&0x800000 != 0 { // sign-extend the 24-bit two's-complement value
			count -= 1 << 24
		}
		newPos := d.position + count
		if newPos < 0 {
			newPos = 0
		}
		fi, statErr := os.Stat(volPath)
		if statErr != nil {
			return d.fail(SenseAbortedCommand, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
		}
		if newPos > fi.Size() {
			d.position = fi.Size()
			return d.fail(SenseBlankCheck, AscNoAdditionalSenseInfo, AscqNoAdditionalSenseInfo, senseFlags{})
		}
		d.position = newPos
		return d.ok()
	case 1: // filemarks
		raw := uint32(cdb[2])<<16 | uint32(cdb[3])<<8 | uint32(cdb[4])
		count := int64(raw)
		if raw&0x800000 != 0 { // sign-extend the 24-bit two's-complement value
			count -= 1 << 24
		}
		if count == 0 {
			return d.ok()
		}
		marks, mErr := readFilemarks(volPath)
		if mErr != nil {
			return d.fail(SenseAbortedCommand, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
		}
		sort.Slice(marks, func(i, j int) bool { return marks[i] < marks[j] })

		if count > 0 {
			newPos, found := filemarkForward(marks, d.position, count)
			d.position = newPos
			if found < count {
				fi, statErr := os.Stat(volPath)
				if statErr == nil {
					d.position = fi.Size()
				}
				d.lastSense = FixedSenseWithInfo(SenseBlankCheck, AscNoAdditionalSenseInfo, AscqNoAdditionalSenseInfo, senseFlags{}, uint32(count-found))
				return tcmu.Response{Status: StatusCheckCondition, Sense: d.lastSense}
			}
			return d.ok()
		}
		newPos, found := filemarkReverse(marks, d.position, -count)
		if found < -count {
			newPos = 0
		}
		d.position = newPos
		return d.ok()
	case 3: // end-of-data
		fi, statErr := os.Stat(volPath)
		if statErr != nil {
			return d.fail(SenseAbortedCommand, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
		}
		d.position = fi.Size()
		return d.ok()
	default:
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
}

// locate implements SSC's LOCATE(10): positions directly to an absolute
// block address (byte offset, in this project's byte-count model).
//
// Unlike every other CDB layout in this package, LOCATE's exact bit
// positions (byte1's IMMED/CP bits, the 4-byte block address at bytes
// 3-6, byte8's Partition field) were not independently re-verified against
// a primary source the way MOVE MEDIUM/READ ELEMENT STATUS/READ/WRITE(6)
// were (see opcodes.go) - this is from established SCSI knowledge at
// moderate, not high, confidence. CP (change partition, Milestone 8) is
// honored only once the mounted volume actually has more than one
// partition (library.Volume.NumberOfPartitions > 1) - rejected outright
// for an ordinary single-partition volume, same as it always was before
// this milestone existed.
func (d *Drive) locate(cdb []byte) tcmu.Response {
	if len(cdb) < 10 {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	cp := cdb[1]&0x02 != 0
	blockAddr := uint32(cdb[3])<<24 | uint32(cdb[4])<<16 | uint32(cdb[5])<<8 | uint32(cdb[6])
	partition := int(cdb[8])

	vol, err := d.volume()
	if err != nil {
		return d.fail(SenseNotReady, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
	}
	if cp {
		if vol == nil || vol.NumberOfPartitions <= 1 {
			return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
		}
		if partition >= vol.NumberOfPartitions {
			return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
		}
		d.partition = partition
	}
	d.position = int64(blockAddr)
	return d.ok()
}

// locate16 implements SSC's LOCATE(16) (0x92, Milestone 8), the modern
// successor to LOCATE(10)'s own CP-bit partition switch (see Drive.
// locate) - added for real multi-partition support, since a real
// initiator that actually cares about partitions (e.g. an LTFS
// implementation) is more likely to issue this than LOCATE(10). CDB byte
// layout is from established SSC knowledge at moderate, not high,
// confidence - a primary source (a T10 SSC draft, or an IBM LTO SCSI
// Reference) was not reachable in this environment (both attempts hit a
// 403/unreadable-PDF wall) - real-initiator verification (a real LTFS
// mount attempt, or `sg_raw` against a real kernel) is needed before this
// is fully trusted:
//
//	byte0:     opcode 92h
//	byte1:     bit0=IMMED (ignored, every command here already completes
//	           synchronously), bit1=CP (change partition), bits4-2=DEST
//	           TYPE (000b=Logical Block - the only value this project
//	           understands, given its own byte-offset-as-block-address
//	           model; any other value is rejected rather than guessed at)
//	byte2:     reserved
//	byte3:     Partition (the destination partition when CP=1)
//	bytes4-11: Logical Object Identifier, an 8-byte big-endian block
//	           address - only the low 32 bits are used (this project's
//	           own backing files never need more); a non-zero high half
//	           is rejected rather than silently truncated
//	bytes12-14: reserved
//	byte15:    control
func (d *Drive) locate16(cdb []byte) tcmu.Response {
	if len(cdb) < 16 {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	cp := cdb[1]&0x02 != 0
	destType := (cdb[1] >> 2) & 0x07
	if destType != 0 {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	partition := int(cdb[3])
	high := binary.BigEndian.Uint32(cdb[4:8])
	low := binary.BigEndian.Uint32(cdb[8:12])
	if high != 0 {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}

	vol, err := d.volume()
	if err != nil {
		return d.fail(SenseNotReady, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
	}
	if cp {
		if vol == nil || vol.NumberOfPartitions <= 1 {
			return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
		}
		if partition >= vol.NumberOfPartitions {
			return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
		}
		d.partition = partition
	}
	d.position = int64(low)
	return d.ok()
}

// modeBlockDescriptorLen is sizeof one SCSI-2-era mode parameter block
// descriptor - long-stable SPC/SSC structure, not independently
// re-verified here the way the other CDB layouts in this file were.
const modeBlockDescriptorLen = 8

func buildModeBlockDescriptor(blockLength uint32) []byte {
	d := make([]byte, modeBlockDescriptorLen)
	// byte0 density code (0, unspecified), bytes1-3 number of blocks (0,
	// unspecified/all of them) - left zero.
	d[5] = byte(blockLength >> 16)
	d[6] = byte(blockLength >> 8)
	d[7] = byte(blockLength)
	return d
}

// modeSense6 implements MODE SENSE(6): the 4-byte mode parameter header
// plus (unless DBD suppresses it) one block descriptor reporting a block
// length of 1 - consistent with this type's byte-count READ/WRITE(6)
// model. Page code 0x00 (current page only, none returned), 0x3F (all
// implemented pages), or 0x11 (Milestone 8's Medium Partition Page, see
// buildMediumPartitionPage) are accepted; any other page code is
// rejected.
func (d *Drive) modeSense6(cdb []byte, buffers [][]byte) tcmu.Response {
	if len(cdb) < 5 {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	dbd := cdb[1]&0x08 != 0
	pageCode := cdb[2] & 0x3F
	if pageCode != 0x00 && pageCode != mediumPartitionPageCode && pageCode != 0x3F {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}

	full := make([]byte, 4)
	if !dbd {
		full = append(full, buildModeBlockDescriptor(1)...)
		full[3] = modeBlockDescriptorLen
	}
	if pageCode == mediumPartitionPageCode || pageCode == 0x3F {
		numberOfPartitions := 1
		if vol, err := d.volume(); err == nil && vol != nil && vol.NumberOfPartitions > 1 {
			numberOfPartitions = vol.NumberOfPartitions
		}
		full = append(full, buildMediumPartitionPage(numberOfPartitions)...)
	}
	full[0] = byte(len(full) - 1) // mode data length: n-1

	n := writeToBuffers(buffers, full)
	return tcmu.Response{Status: StatusGood, ReadLen: uint32(n)}
}

// mediumPartitionPageCode/mediumPartitionPageLen: see
// buildMediumPartitionPage's own doc comment.
const (
	mediumPartitionPageCode = 0x11
	mediumPartitionPageLen  = 8

	// maxAdditionalPartitions is this project's own declared ceiling on
	// how many partitions beyond partition 0 it supports - 1 (two
	// partitions total), exactly enough for LTFS's own index-partition +
	// data-partition convention, the whole real-world motivation for this
	// milestone. Not a T10-specified value; this project's own choice.
	maxAdditionalPartitions = 1
)

// buildMediumPartitionPage builds the Medium Partition mode page (0x11):
// byte0=page code, byte1=page length (6), byte2=Maximum Additional
// Partitions (maxAdditionalPartitions), byte3=Additional Partitions
// Defined (numberOfPartitions-1; 0 for an unformatted/single-partition
// volume). This 4-byte header is the part of this page's byte layout
// this package is most confident about; everything past it (real SSC
// defines FDP/SDP/IDP/PSUM flag bits and per-partition Partition Size
// fields here) is NOT independently verified against a primary source in
// this environment - two attempts (a T10 SSC draft, an IBM LTO SCSI
// Reference) were both unreachable (403/PDF-extraction failures) - and is
// deliberately left as self-consistent zero-filled padding rather than
// guessed at in detail, same posture as reportDensitySupport's own
// descriptor body past byte3. This project doesn't track or enforce
// per-partition sizes at all (see library.Volume.NumberOfPartitions' own
// doc comment for why that's a deliberate simplification, not an
// oversight) - a host that sets explicit Partition Size values via MODE
// SELECT has that part of its request silently ignored. Needs a real
// LTFS mount attempt or T10 draft cross-check before being fully trusted
// - see opcodes.go's doc comment.
func buildMediumPartitionPage(numberOfPartitions int) []byte {
	p := make([]byte, mediumPartitionPageLen)
	p[0] = mediumPartitionPageCode
	p[1] = mediumPartitionPageLen - 2
	p[2] = maxAdditionalPartitions
	if numberOfPartitions > 1 {
		p[3] = byte(numberOfPartitions - 1)
	}
	return p
}

// modeSelect6 implements MODE SELECT(6): CDB byte4=Parameter List Length.
// Through Milestone 7 this was a pure accept-but-ignore stub (this
// project's READ/WRITE(6) always treat transfer length as a literal byte
// count - see the type's doc comment - so there was no block-size
// setting a real MODE SELECT would change that this project acted on).
// Milestone 8 adds real parsing of exactly one page out of the data-out
// parameter list: the Medium Partition Page (see
// buildMediumPartitionPage's own doc comment for its byte layout and
// this package's confidence level in it), staging its Additional
// Partitions Defined field in d.pendingPartitionCount for a subsequent
// FORMAT MEDIUM to actually apply (see Drive.formatMedium) - matching
// real SSC semantics, where MODE SELECT alone never reformats anything.
// Every other page in the parameter list (and every byte if the Medium
// Partition Page isn't present at all) is silently ignored, preserving
// this command's pre-Milestone-8 stub behavior for everything else.
//
// Parameter list layout: 4-byte mode parameter header (byte3=Block
// Descriptor Length, the only header field this parser reads), that many
// bytes of block descriptor (skipped, not parsed), then zero or more
// pages (byte0=PS+page code, byte1=page length, byte2..=payload) - long-
// stable, high-confidence SCSI-2-era SPC/SSC structure, same posture as
// modeSense6's own header/block-descriptor shape.
func (d *Drive) modeSelect6(cdb []byte, buffers [][]byte) tcmu.Response {
	if len(cdb) < 5 {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	paramLen := int(cdb[4])
	if paramLen == 0 {
		return d.ok()
	}
	data := make([]byte, 0, paramLen)
	for _, buf := range buffers {
		if len(data) >= paramLen {
			break
		}
		take := paramLen - len(data)
		if take > len(buf) {
			take = len(buf)
		}
		data = append(data, buf[:take]...)
	}
	if len(data) < 4 {
		return d.ok()
	}
	blockDescLen := int(data[3])
	pages := data[4:]
	if len(pages) >= blockDescLen {
		pages = pages[blockDescLen:]
	}
	for len(pages) >= 2 {
		pageCode := pages[0] & 0x3F
		pageLen := int(pages[1])
		if len(pages) < 2+pageLen {
			break
		}
		if pageCode == mediumPartitionPageCode && pageLen >= 2 {
			d.pendingPartitionCount = int(pages[3]) + 1 // Additional Partitions Defined + partition 0 itself
		}
		pages = pages[2+pageLen:]
	}
	return d.ok()
}

// readPositionLength is the size of the short-form (BT=0) READ POSITION
// response this project always returns - the long form (32 bytes, byte1
// bit2 LONG/service action) is not implemented; see readPosition's own
// doc comment.
const readPositionLength = 20

// readPosition implements SSC's READ POSITION (0x34): CDB byte1 bit0 = BT
// (Block address Type) - only the short-form, BT=0 response this project
// always builds is supported; BT=1 (vendor-specific block address type)
// is rejected outright rather than guessed at.
//
// The overall 20-byte short-form response shape (byte0 status flags,
// byte1 partition, bytes4-7/8-11 first/last block location, byte12
// reserved, bytes13-15/16-19 buffered block/byte counts) is high
// confidence, long-stable SCSI-2-era SSC structure. byte0's exact bit
// assignment is not independently re-verified against a primary source in
// this environment - flagged the same way Drive.locate's own CDB layout
// already is (see opcodes.go's doc comment): BOP=bit7, EOP=bit6,
// PERR=bit0 here, at moderate not high confidence, due for real-initiator
// (sg_raw/`mt tell`) verification.
//
// First/last block location both report d.position directly, truncated to
// 32 bits - consistent with this project's existing byte-offset-as-
// block-address model (see Drive.locate, which already writes a raw CDB
// block address straight into d.position with no unit conversion).
// First==last (no buffered/queued blocks pending) is always correct here:
// every READ(6)/WRITE(6) in this package completes synchronously before
// returning, so there is never a buffered block in flight to report
// separately.
func (d *Drive) readPosition(cdb []byte, buffers [][]byte) tcmu.Response {
	if len(cdb) < 10 {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	if cdb[1]&0x01 != 0 { // BT=1, vendor-specific block address type - not supported
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	vol, err := d.volume()
	if err != nil {
		return d.fail(SenseNotReady, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
	}
	if vol == nil {
		return d.fail(SenseNotReady, AscMediumNotPresent, AscqMediumNotPresent, senseFlags{})
	}

	resp := make([]byte, readPositionLength)
	if d.position == 0 {
		resp[0] |= 1 << 7 // BOP
	}
	if cap := d.effectiveCapacity(vol); cap > 0 && d.position >= cap {
		resp[0] |= 1 << 6 // EOP
	}
	resp[1] = byte(d.partition)
	blockAddr := uint32(d.position)
	binary.BigEndian.PutUint32(resp[4:8], blockAddr)
	binary.BigEndian.PutUint32(resp[8:12], blockAddr)

	n := writeToBuffers(buffers, resp)
	return tcmu.Response{Status: StatusGood, ReadLen: uint32(n)}
}

// readBlockLimits implements SSC's READ BLOCK LIMITS (0x05): a fixed
// 6-byte response, long-stable SCSI-2-era SPC/SSC structure (high
// confidence, unlike readPosition/reportDensitySupport above/below).
// Granularity (byte0 bits 4-0) is reported as 0 ("not applicable"),
// consistent with this project's own byte-count transfer-length model
// (see the Drive type's own doc comment) rather than a real fixed block
// granularity. Maximum block length is reported as the largest value
// READ(6)/WRITE(6)'s own 3-byte CDB transfer-length field can represent
// (0xFFFFFF), since this package already treats that field as a literal
// byte count with no smaller enforced limit; minimum block length is 1.
func (d *Drive) readBlockLimits(buffers [][]byte) tcmu.Response {
	resp := make([]byte, 6)
	resp[1] = 0xFF
	resp[2] = 0xFF
	resp[3] = 0xFF
	binary.BigEndian.PutUint16(resp[4:6], 1)
	n := writeToBuffers(buffers, resp)
	return tcmu.Response{Status: StatusGood, ReadLen: uint32(n)}
}

// densityDescriptorLen is this project's own chosen length for the one
// density descriptor reportDensitySupport returns - see that function's
// doc comment for why the body past byte3 is best-effort rather than
// verified against a primary source.
const densityDescriptorLen = 44

// reportDensitySupport implements SSC's REPORT DENSITY SUPPORT (0x44):
// opcode and overall CDB shape (byte1 bits 0-1 select "by current medium"
// vs. "by drive", bytes7-8 allocation length) are high confidence: this
// package deliberately ignores that distinction and always reports the
// one density this Drive's Family supports, since it never models a
// drive capable of more than one.
//
// The response's 4-byte header (2-byte available data length + 2 reserved
// bytes) followed by density descriptors is the correct, stable shape;
// each descriptor's own internal byte layout past byte3 (bits-per-mm,
// media width, track count, capacity, assigning organization/density
// name/description text fields) is this package's own best-effort
// approximation, NOT independently verified against a primary source in
// this environment - unlike every other response builder in this file.
// Only byte0 (primary density code, from DriveFamily.DensityCode) and
// byte2's WRTOK/DFLT flags are meaningful; the rest is present so the
// descriptor's declared length is self-consistent, not because its exact
// field offsets are trusted. Needs real-initiator (sg_logs/mt) or a T10
// SSC draft check before being relied on.
func (d *Drive) reportDensitySupport(cdb []byte, buffers [][]byte) tcmu.Response {
	if len(cdb) < 10 {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	allocLen := int(binary.BigEndian.Uint16(cdb[7:9]))

	desc := make([]byte, densityDescriptorLen)
	desc[0] = d.Family.DensityCode
	desc[1] = d.Family.DensityCode
	desc[2] = 1<<7 | 1<<5 // WRTOK=1, DFLT=1
	copy(desc[14:22], padASCII("GOTOCHNG", 8))
	copy(desc[22:30], padASCII(d.Family.Identity.Product, 8))
	copy(desc[30:44], padASCII(d.Family.Identity.Product, 14))

	full := make([]byte, 4+len(desc))
	binary.BigEndian.PutUint16(full[0:2], uint16(2+len(desc)))
	copy(full[4:], desc)
	if len(full) > allocLen {
		full = full[:allocLen]
	}
	n := writeToBuffers(buffers, full)
	return tcmu.Response{Status: StatusGood, ReadLen: uint32(n)}
}

// logSense implements SSC's LOG SENSE (0x4D): CDB byte2 bits 5-0 = PAGE
// CODE (bits 7-6, page control, ignored - this package has only one set
// of live values per page, not separate current/changeable/default/saved
// copies), bytes7-8 = allocation length. Only page 0x00 (Supported Log
// Pages) and 0x2E (TapeAlert) are implemented - see logsense.go.
func (d *Drive) logSense(cdb []byte, buffers [][]byte) tcmu.Response {
	if len(cdb) < 9 {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	pageCode := cdb[2] & 0x3F
	allocLen := int(binary.BigEndian.Uint16(cdb[7:9]))

	var full []byte
	switch pageCode {
	case logPageSupportedPages:
		full = buildSupportedLogPages(logPageTapeAlert)
	case logPageTapeAlert:
		st, err := d.Client.Status()
		if err != nil {
			return d.fail(SenseNotReady, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
		}
		drv, err := driveRecord(st, d.Index)
		if err != nil {
			return d.fail(SenseNotReady, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
		}
		full = buildLogSensePage(logPageTapeAlert, driveTapeAlertParams(st, drv, drv.Volume))
	default:
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	if len(full) > allocLen {
		full = full[:allocLen]
	}
	n := writeToBuffers(buffers, full)
	return tcmu.Response{Status: StatusGood, ReadLen: uint32(n)}
}

// logSelect implements LOG SELECT(6) as an accept-but-ignore stub, same
// posture as modeSelect6: this package has no log parameter thresholds
// or resettable counters to honor a LOG SELECT write against.
func (d *Drive) logSelect(cdb []byte) tcmu.Response {
	if len(cdb) < 9 {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	return d.ok()
}

// erase implements SSC's ERASE(6) (0x19): CDB byte1 bit0=Long (0=erase
// from the current position to end-of-data only, 1=erase the entire
// medium), bit1=IMMED (ignored, every command here already completes
// synchronously) - opcode/CDB layout is long-stable, high-confidence
// SCSI-2/SSC-2. Reuses truncateToEOD/invalidateFilemarksFrom exactly as
// writeFilemarks already does for Long=0 (see that function's own doc
// comment for why real magnetic tape can never retain structure beyond a
// freshly-written point); Long=1 additionally rewinds to BOT and clears
// the filemarks sidecar entirely via writeFilemarksFile(vol.Path, nil)
// rather than invalidateFilemarksFrom(vol.Path, 0), since the latter
// would keep a mark recorded exactly at position 0 - correct for an
// ordinary write starting at BOT, but wrong for "erase the whole medium",
// which must leave zero recorded structure at all.
func (d *Drive) erase(cdb []byte) tcmu.Response {
	if len(cdb) < 6 {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	long := cdb[1]&0x01 != 0

	vol, err := d.volume()
	if err != nil {
		return d.fail(SenseNotReady, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
	}
	if vol == nil {
		return d.fail(SenseNotReady, AscMediumNotPresent, AscqMediumNotPresent, senseFlags{})
	}
	if vol.WriteProtected {
		return d.fail(SenseDataProtect, AscWriteProtected, AscqWriteProtected, senseFlags{})
	}

	if long {
		d.position = 0
	}
	volPath := d.currentVolPath(vol)
	if err := truncateToEOD(volPath, d.position); err != nil {
		return d.fail(SenseAbortedCommand, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
	}
	if long {
		if err := writeFilemarksFile(volPath, nil); err != nil {
			return d.fail(SenseAbortedCommand, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
		}
		if err := writeEncTagsFile(volPath, nil); err != nil {
			return d.fail(SenseAbortedCommand, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
		}
	} else {
		if err := invalidateFilemarksFrom(volPath, d.position); err != nil {
			return d.fail(SenseAbortedCommand, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
		}
		if err := dropEncTagsFrom(volPath, d.position); err != nil {
			return d.fail(SenseAbortedCommand, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
		}
	}
	return d.ok()
}

// verify6 implements SSC's VERIFY(6) (0x13): CDB byte1 bit1=BYTCMP,
// bytes2-4=VERIFICATION LENGTH (a byte count, consistent with this type's
// own read/write model - see the type's doc comment). Only BYTCMP=0
// (verify the medium is readable; no host-supplied data-out comparison
// phase) is implemented - BYTCMP=1 needs a data-out phase whose exact
// direction handling at the TCMU/kernel level wasn't verified against a
// real initiator in this environment (VERIFY is unusual among the
// opcodes this package handles in having a CDB-bit-dependent data
// direction, not a fixed one), so it's rejected outright (ILLEGAL
// REQUEST/INVALID FIELD IN CDB) rather than guessed at - same
// conservative posture this package already takes for LOCATE's CP bit.
//
// Shares read6's own filemark-stop/BLANK CHECK/ILI semantics (see its doc
// comment) since VERIFY must honor the same recorded-filemark boundary a
// real read would and advances d.position identically - it just never
// transfers the bytes read to the initiator, so it checks how many bytes
// are actually available via os.Stat rather than performing a real
// ReadAt into a throwaway buffer.
func (d *Drive) verify6(cdb []byte) tcmu.Response {
	if len(cdb) < 6 {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	if cdb[1]&0x02 != 0 { // BYTCMP
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	n := int(cdb[2])<<16 | int(cdb[3])<<8 | int(cdb[4])

	vol, err := d.volume()
	if err != nil {
		return d.fail(SenseNotReady, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
	}
	if vol == nil {
		return d.fail(SenseNotReady, AscMediumNotPresent, AscqMediumNotPresent, senseFlags{})
	}

	volPath := d.currentVolPath(vol)
	readLen := n
	hitFilemark := false
	if marks, mErr := readFilemarks(volPath); mErr == nil {
		sort.Slice(marks, func(i, j int) bool { return marks[i] < marks[j] })
		for _, m := range marks {
			if m > d.position {
				if m <= d.position+int64(n) {
					readLen = int(m - d.position)
					hitFilemark = true
				}
				break
			}
		}
	}

	fi, statErr := os.Stat(volPath)
	if statErr != nil {
		return d.fail(SenseAbortedCommand, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
	}
	available := fi.Size() - d.position
	if available < 0 {
		available = 0
	}
	read := int64(readLen)
	if read > available {
		read = available
	}
	if read == 0 && n > 0 && !hitFilemark {
		return d.fail(SenseBlankCheck, AscNoAdditionalSenseInfo, AscqNoAdditionalSenseInfo, senseFlags{})
	}
	d.position += read

	if hitFilemark && read == int64(readLen) {
		d.lastSense = FixedSenseWithInfo(SenseNoSense, AscNoAdditionalSenseInfo, AscqNoAdditionalSenseInfo, senseFlags{Filemark: true}, uint32(int64(n)-read))
		return tcmu.Response{Status: StatusCheckCondition, Sense: d.lastSense}
	}
	if read < int64(n) {
		d.lastSense = FixedSenseWithInfo(SenseNoSense, AscNoAdditionalSenseInfo, AscqNoAdditionalSenseInfo, senseFlags{ILI: true}, uint32(int64(n)-read))
		return tcmu.Response{Status: StatusCheckCondition, Sense: d.lastSense}
	}
	return d.ok()
}

// allowOverwrite implements ALLOW OVERWRITE (0x82) as an accept-and-
// ignore stub: this project's write path already always allows overwrite
// from the current position (see write6), so there's no append-only
// enforcement here for this command to relax.
func (d *Drive) allowOverwrite(cdb []byte) tcmu.Response {
	if len(cdb) < 6 {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	return d.ok()
}

// effectiveCapacity is vol's capacity as write6/readPosition's EOP
// reporting should treat it: vol.CapacityBytes, unless a SET CAPACITY
// call already narrowed it for this mount (see
// Drive.sessionCapacityLimitBytes' own doc comment).
func (d *Drive) effectiveCapacity(vol *library.Volume) int64 {
	if d.sessionCapacityLimitBytes > 0 {
		return d.sessionCapacityLimitBytes
	}
	return vol.CapacityBytes
}

// setCapacity implements SSC's SET CAPACITY (0x0B): CDB bytes2-3=
// PROPORTION, a 16-bit fraction (denominator 0xFFFF) of the mounted
// volume's own configured CapacityBytes - the only "maximum capacity"
// figure this project tracks at all (real SET CAPACITY expresses
// PROPORTION against a drive's true native maximum capacity for the
// loaded medium, a concept this project has no separate representation
// of, so CapacityBytes is used as that ceiling instead). PROPORTION=0
// means "full capacity", SSC's own convention for requesting the
// maximum.
//
// Deliberately session-scoped (Drive.sessionCapacityLimitBytes, reset by
// loadUnload's Load case) rather than a Volume.CapacityBytes mutation:
// real SET CAPACITY's own semantics already last only "until the volume
// is unloaded", and mutating the persisted domain field would misreport
// a volume's actual, physically-unchanged capacity to every other part
// of this project (Admin UI, REST API, Prometheus) that reports it -
// this needs no new LibraryClient method at all, unlike a domain-level
// change would.
func (d *Drive) setCapacity(cdb []byte) tcmu.Response {
	if len(cdb) < 6 {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	vol, err := d.volume()
	if err != nil {
		return d.fail(SenseNotReady, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
	}
	if vol == nil {
		return d.fail(SenseNotReady, AscMediumNotPresent, AscqMediumNotPresent, senseFlags{})
	}
	proportion := binary.BigEndian.Uint16(cdb[2:4])
	if proportion == 0 {
		d.sessionCapacityLimitBytes = 0
		return d.ok()
	}
	d.sessionCapacityLimitBytes = vol.CapacityBytes * int64(proportion) / 0xFFFF
	return d.ok()
}

// formatMedium implements SSC's FORMAT MEDIUM (0x04, Milestone 8): CDB
// byte1 bits1-0=FORMAT - only 00b ("use the format specified by the
// current Medium Partition mode page settings", i.e. whatever was most
// recently staged via MODE SELECT - see Drive.modeSelect6/
// pendingPartitionCount) is implemented; any other value is rejected
// rather than guessed at, same moderate-confidence posture as
// buildMediumPartitionPage. Opcode/overall CDB shape is long-stable,
// high-confidence SCSI-2/SSC-2.
//
// Applies the staged partition count for real, persisted via
// Client.SetDriveVolumeNumberOfPartitions - unlike SET CAPACITY, a real
// FORMAT MEDIUM permanently changes the physical medium's own partition
// layout, so this can't be a session-scoped clamp the way SET CAPACITY
// is (see that function's own doc comment for the contrast).
//
// Destructive, matching this project's own established model of what a
// real drive operation does to a backing file (see Drive.erase's own doc
// comment for the same reasoning): erases partition 0 entirely (same as
// ERASE(6) Long=1) and removes every existing additional-partition
// backing file/filemarks sidecar outright, so a reformat - with a
// different or the same partition count - always starts every partition
// completely fresh rather than leaving stale data an initiator never
// asked to keep. Repositions to partition 0, BOT.
func (d *Drive) formatMedium(cdb []byte) tcmu.Response {
	if len(cdb) < 6 {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	if cdb[1]&0x03 != 0 { // FORMAT field
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}

	vol, err := d.volume()
	if err != nil {
		return d.fail(SenseNotReady, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
	}
	if vol == nil {
		return d.fail(SenseNotReady, AscMediumNotPresent, AscqMediumNotPresent, senseFlags{})
	}
	if vol.WriteProtected {
		return d.fail(SenseDataProtect, AscWriteProtected, AscqWriteProtected, senseFlags{})
	}

	n := d.pendingPartitionCount
	if n <= 0 {
		n = 1
	}

	if err := truncateToEOD(vol.Path, 0); err != nil {
		return d.fail(SenseAbortedCommand, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
	}
	if err := writeFilemarksFile(vol.Path, nil); err != nil {
		return d.fail(SenseAbortedCommand, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
	}
	// Drop every existing additional-partition backing file - 64 is a
	// generous, arbitrary bound (this project's own declared ceiling,
	// maxAdditionalPartitions, is 1; this just stops promptly once no
	// more sibling files exist rather than hardcoding that ceiling here
	// too).
	for i := 1; i < 64; i++ {
		p := partitionPath(vol.Path, i)
		if _, statErr := os.Stat(p); statErr != nil {
			break
		}
		_ = os.Remove(p)
		_ = os.Remove(filemarksPath(p))
	}

	if err := d.Client.SetDriveVolumeNumberOfPartitions(d.Index, n); err != nil {
		return d.fail(SenseAbortedCommand, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
	}
	d.partition = 0
	d.position = 0
	d.pendingPartitionCount = 0
	return d.ok()
}

// mamAttributes builds this package's implemented MAM attribute set
// (see mam.go's own doc comment for which IDs and why) for the mounted
// volume/drive record - shared only by readAttribute, kept separate
// purely so this doc comment can enumerate what each value actually
// comes from without cluttering that dispatch logic:
//
//   - Remaining/Maximum Capacity In Partition: the *current* partition's
//     own backing file size (see currentVolPath) against this Volume's
//     CapacityBytes ceiling - the same ceiling every partition
//     independently grows against (see library.Volume.NumberOfPartitions'
//     own doc comment), converted to MiB.
//   - TapeAlert Flags: reuses driveTapeAlertParams (Milestone 4) as a
//     64-bit bitmask, one bit per TapeAlert flag number (bit0 = flag 1,
//     matching the T10 TapeAlert flag numbering itself starting at 1).
//   - Load Count: library.Volume.LoadCount, incremented by Library.Load
//     on every real mount.
//   - Volume Identifier: this Volume's own Barcode.
//   - Medium Serial Number: left blank - this project has no separate
//     serial number concept distinct from Barcode, and reporting Barcode
//     again under a different attribute ID would misrepresent it as a
//     second, genuinely distinct identifier a real cartridge has.
//   - Application Vendor/Name/Version, User Medium Text Label: the
//     mutable fields WRITE ATTRIBUTE can set (see writeAttribute).
func (d *Drive) mamAttributes(vol *library.Volume, drv *library.Drive, st library.Status) []mamAttribute {
	volPath := d.currentVolPath(vol)
	var currentSize int64
	if fi, err := os.Stat(volPath); err == nil {
		currentSize = fi.Size()
	}
	const mib = 1024 * 1024
	maxCapMiB := uint64(vol.CapacityBytes / mib)
	var remainingMiB uint64
	if vol.CapacityBytes > currentSize {
		remainingMiB = uint64((vol.CapacityBytes - currentSize) / mib)
	}

	var tapeAlertBits uint64
	for _, p := range driveTapeAlertParams(st, drv, vol) {
		if p.active {
			tapeAlertBits |= 1 << (p.code - 1)
		}
	}

	return []mamAttribute{
		{id: mamRemainingCapacity, format: mamFormatBinary, readOnly: true, value: mamBinary8(remainingMiB)},
		{id: mamMaximumCapacity, format: mamFormatBinary, readOnly: true, value: mamBinary8(maxCapMiB)},
		{id: mamTapeAlertFlags, format: mamFormatBinary, readOnly: true, value: mamBinary8(tapeAlertBits)},
		{id: mamLoadCount, format: mamFormatBinary, readOnly: true, value: mamBinary8(uint64(vol.LoadCount))},
		{id: mamVolumeIdentifier, format: mamFormatASCII, readOnly: true, value: padASCII(vol.Barcode, 32)},
		{id: mamMediumSerialNumber, format: mamFormatASCII, readOnly: true, value: padASCII("", 32)},
		{id: mamApplicationVendor, format: mamFormatASCII, readOnly: false, value: padASCII(vol.ApplicationVendor, 8)},
		{id: mamApplicationName, format: mamFormatASCII, readOnly: false, value: padASCII(vol.ApplicationName, 32)},
		{id: mamApplicationVersion, format: mamFormatASCII, readOnly: false, value: padASCII(vol.ApplicationVersion, 8)},
		{id: mamUserMediumTextLabel, format: mamFormatText, readOnly: false, value: padASCII(vol.UserMediumTextLabel, 160)},
	}
}

// readAttribute implements SSC's READ ATTRIBUTE (0x8C): CDB byte1
// bits0-4=Service Action, byte7=Partition Number (not distinguished -
// this package reports the same attribute set regardless, since it
// doesn't track per-partition MAM values separately), bytes8-9=First
// Attribute Identifier, bytes10-13=Allocation Length - verified against
// sg3_utils' sg_read_attr.c (see mam.go's own doc comment). Only Service
// Action 0 ("attribute values") is implemented; every other service
// action (attribute list, logical volume list, partition list, SMC-2,
// supported attributes) is rejected outright rather than guessed at.
//
// Response: a 4-byte Available Data Length header (matching the shape
// WRITE ATTRIBUTE's own parameter list is confirmed to use, by the same
// source) followed by one encodeMAMAttribute entry per implemented
// attribute whose ID is >= First Attribute Identifier, in ascending ID
// order - the mechanism a real initiator uses to page through a long
// attribute list; this package's own list is short enough to always
// return in full once filtered.
func (d *Drive) readAttribute(cdb []byte, buffers [][]byte) tcmu.Response {
	if len(cdb) < 14 {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	serviceAction := cdb[1] & 0x1F
	if serviceAction != 0 {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	firstID := binary.BigEndian.Uint16(cdb[8:10])
	allocLen := binary.BigEndian.Uint32(cdb[10:14])

	vol, err := d.volume()
	if err != nil {
		return d.fail(SenseNotReady, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
	}
	if vol == nil {
		return d.fail(SenseNotReady, AscMediumNotPresent, AscqMediumNotPresent, senseFlags{})
	}
	st, err := d.Client.Status()
	if err != nil {
		return d.fail(SenseNotReady, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
	}
	drv, err := driveRecord(st, d.Index)
	if err != nil {
		return d.fail(SenseNotReady, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
	}

	var body []byte
	for _, a := range d.mamAttributes(vol, drv, st) {
		if a.id < firstID {
			continue
		}
		body = append(body, encodeMAMAttribute(a)...)
	}
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(body)))
	full := append(header, body...)
	if uint32(len(full)) > allocLen {
		full = full[:allocLen]
	}
	n := writeToBuffers(buffers, full)
	return tcmu.Response{Status: StatusGood, ReadLen: uint32(n)}
}

// writeAttribute implements SSC's WRITE ATTRIBUTE (0x8D): CDB byte1
// bit0=WTC (ignored, every command here already completes
// synchronously), byte7=Partition Number (not distinguished, same
// reasoning as readAttribute), bytes10-13=Parameter List Length -
// verified against sg3_utils' sg_write_attr.c (see mam.go's own doc
// comment). Parameter list: a 4-byte Attribute List Length header
// followed by encodeMAMAttribute-shaped entries.
//
// Only the mutable attributes this package defines (Application Vendor/
// Name/Version, User Medium Text Label) can actually be changed; writing
// any other attribute ID (including the read-only ones readAttribute
// also reports) is silently ignored rather than rejected outright - real
// devices vary in whether they hard-reject an attempt to write a
// read-only attribute or simply ignore it, and silently ignoring avoids
// failing a real initiator's WRITE ATTRIBUTE call outright just because
// it also touched something this package doesn't let it change.
// Persisted via Client.SetDriveVolumeMAMAttributes - like FORMAT MEDIUM,
// and unlike SET CAPACITY, these attributes must survive across mounts,
// so this can't be Drive-local session state.
func (d *Drive) writeAttribute(cdb []byte, buffers [][]byte) tcmu.Response {
	if len(cdb) < 14 {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	paramLen := binary.BigEndian.Uint32(cdb[10:14])
	if paramLen == 0 {
		return d.ok()
	}
	if _, err := d.volume(); err != nil {
		return d.fail(SenseNotReady, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
	}

	data := make([]byte, 0, paramLen)
	for _, buf := range buffers {
		if uint32(len(data)) >= paramLen {
			break
		}
		take := int(paramLen) - len(data)
		if take > len(buf) {
			take = len(buf)
		}
		data = append(data, buf[:take]...)
	}
	if len(data) < 4 {
		return d.ok()
	}
	body := data[4:]

	var attrs library.MAMAttributes
	for len(body) >= 5 {
		id := binary.BigEndian.Uint16(body[0:2])
		valLen := int(binary.BigEndian.Uint16(body[3:5]))
		if len(body) < 5+valLen {
			break
		}
		value := strings.TrimRight(string(body[5:5+valLen]), " \x00")
		switch id {
		case mamApplicationVendor:
			attrs.ApplicationVendor = &value
		case mamApplicationName:
			attrs.ApplicationName = &value
		case mamApplicationVersion:
			attrs.ApplicationVersion = &value
		case mamUserMediumTextLabel:
			attrs.UserMediumTextLabel = &value
		}
		body = body[5+valLen:]
	}

	if err := d.Client.SetDriveVolumeMAMAttributes(d.Index, attrs); err != nil {
		return d.fail(SenseAbortedCommand, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
	}
	return d.ok()
}

// failSecurity reports a decryption/key failure - a wrong or missing
// AES key on an encrypted volume (Milestone 10). Sense key DATA
// PROTECT/ASC 0x74/ASCQ 0x71 ("Logical Unit Access Not Authorized") is
// confirmed against real-world usage (Linux kernel/FreeBSD SCSI error-
// handling patches and hardware-encrypted-device reports that
// specifically key off this triple for a locked/inaccessible encrypted
// device), at moderate confidence for this exact tape-encryption
// scenario - a primary source distinguishing this from the several other
// 0x74-family ASCQ values real LTO documentation defines for more
// specific encryption failure sub-cases (e.g. a genuinely distinct code
// for "no key" vs. "wrong key") was not reachable in this environment
// (the one candidate source, IBM's own TS4500 "encryption-related
// ASC/ASCQ codes" page, returned 403). Using one consistent code for
// both "no key set" and "wrong key" rather than guessing at which of
// several plausible specific codes is correct for each.
func (d *Drive) failSecurity() tcmu.Response {
	return d.fail(SenseDataProtect, 0x74, 0x71, senseFlags{})
}

// securityProtocolIn implements SPC's SECURITY PROTOCOL IN (0xA2): CDB
// byte1=SECURITY PROTOCOL, bytes2-3=SECURITY PROTOCOL SPECIFIC (the page
// code within that protocol), bytes6-9=ALLOCATION LENGTH (4-byte
// big-endian) - verified against `stenc` (see encryption.go's own doc
// comment). Only security protocol 0x20 ("Tape Data Encryption") is
// implemented; any other protocol (including 0x00, "list supported
// security protocols" - a real initiator that already knows it wants TDE,
// like stenc itself, queries protocol 0x20 directly rather than that
// discovery step) is rejected outright.
func (d *Drive) securityProtocolIn(cdb []byte, buffers [][]byte) tcmu.Response {
	if len(cdb) < 10 {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	protocol := cdb[1]
	pageCode := binary.BigEndian.Uint16(cdb[2:4])
	allocLen := binary.BigEndian.Uint32(cdb[6:10])
	if protocol != securityProtocolTDE {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}

	var full []byte
	switch pageCode {
	case secPageSupported:
		full = buildSupportedSecurityPages()
	case secPageDeviceEncryptionStatus:
		full = d.buildDeviceEncryptionStatusPage()
	default:
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	if uint32(len(full)) > allocLen {
		full = full[:allocLen]
	}
	n := writeToBuffers(buffers, full)
	return tcmu.Response{Status: StatusGood, ReadLen: uint32(n)}
}

// securityProtocolOut implements SPC's SECURITY PROTOCOL OUT (0xB5): CDB
// byte1=SECURITY PROTOCOL, bytes2-3=SECURITY PROTOCOL SPECIFIC (page
// code), bytes6-9=parameter list length (4-byte big-endian) - verified
// against `stenc`. Only security protocol 0x20 and page 0x10 ("Set Data
// Encryption") are implemented.
//
// Set Data Encryption page (parameter data, verified against stenc's own
// page_sde struct): bytes0-1=page code, bytes2-3=page length, byte4=
// control, byte5=flags (this package's own encryption model has no
// per-session-scope/RDMC/SDK/CKOD/CKORP/CKORL distinctions to make - see
// library.Volume.Encrypted's own doc comment - so these are read but not
// acted on), byte6=encryption mode, byte7=decryption mode, byte8=
// algorithm index, byte9=key format, byte10=KAD format, bytes11-17=
// reserved, bytes18-19=key length, bytes20+=key.
//
// Only encryption/decryption mode values off(0x00) and on(0x02) are
// accepted (T10 also defines external/raw/mixed - real-hardware
// behaviors this project's whole-volume-at-a-time model has no analogue
// for, see encryption.go's own doc comment); only key format 0x00 (the
// plaintext key bytes themselves, not an externally-wrapped key this
// project has no key manager to unwrap) is accepted; turning encryption
// on requires exactly a 32-byte (AES-256) key. Any other value is
// rejected outright rather than guessed at or silently mishandled.
func (d *Drive) securityProtocolOut(cdb []byte, buffers [][]byte) tcmu.Response {
	if len(cdb) < 10 {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	protocol := cdb[1]
	pageCode := binary.BigEndian.Uint16(cdb[2:4])
	paramLen := binary.BigEndian.Uint32(cdb[6:10])
	if protocol != securityProtocolTDE || pageCode != secPageSetDataEncryption {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	if paramLen == 0 {
		return d.ok()
	}

	data := make([]byte, 0, paramLen)
	for _, buf := range buffers {
		if uint32(len(data)) >= paramLen {
			break
		}
		take := int(paramLen) - len(data)
		if take > len(buf) {
			take = len(buf)
		}
		data = append(data, buf[:take]...)
	}
	if len(data) < 20 {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	encMode := data[6]
	decMode := data[7]
	algIndex := data[8]
	keyFormat := data[9]
	keyLen := binary.BigEndian.Uint16(data[18:20])
	if len(data) < 20+int(keyLen) {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	key := data[20 : 20+int(keyLen)]

	if encMode != sdeModeOff && encMode != sdeModeOn {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	if decMode != sdeModeOff && decMode != sdeModeOn {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	if encMode == sdeModeOn {
		if keyFormat != sdeKeyFormatPlaintext {
			return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
		}
		if len(key) != aesKeyLen {
			return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
		}
	}

	d.encryptMode = encMode
	d.decryptMode = decMode
	d.algorithmIndex = algIndex
	if encMode == sdeModeOn {
		d.encryptionKey = append([]byte(nil), key...)
		d.keyInstanceCounter++
	} else {
		d.encryptionKey = nil
	}
	return d.ok()
}
