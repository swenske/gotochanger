package scsi

import (
	"fmt"
	"os"
	"sort"
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

	position  int64 // current byte offset into the mounted volume; reset to 0 by REWIND or a successful LOAD
	lastSense []byte
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
	case OpModeSense6:
		return d.modeSense6(entry.CDB, entry.Buffers)
	case OpModeSelect6:
		return d.modeSelect6(entry.CDB)
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
	for _, drv := range st.Drives {
		if drv.Index == d.Index {
			return drv.Volume, nil
		}
	}
	return nil, fmt.Errorf("drive %d not found", d.Index)
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

	readLen := n
	hitFilemark := false
	if marks, mErr := readFilemarks(vol.Path); mErr == nil {
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

	f, err := os.Open(vol.Path)
	if err != nil {
		return d.fail(SenseAbortedCommand, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
	}
	defer f.Close()

	buf := make([]byte, readLen)
	read, _ := f.ReadAt(buf, d.position)
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
	if vol.CapacityBytes > 0 && d.position+int64(n) > vol.CapacityBytes {
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

	f, err := os.OpenFile(vol.Path, os.O_WRONLY, 0)
	if err != nil {
		return d.fail(SenseAbortedCommand, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
	}
	defer f.Close()
	if _, err := f.WriteAt(data, d.position); err != nil {
		return d.fail(SenseAbortedCommand, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
	}
	// Real data written at d.position makes any filemark strictly beyond
	// this point stale - see invalidateFilemarksFrom's own doc comment,
	// including why this must NOT also drop a mark exactly at d.position
	// (that's the ordinary, expected case of writing the next file right
	// after the previous one's own filemark - dropping it broke every
	// real multi-file sequential write, found against a real kernel).
	_ = invalidateFilemarksFrom(vol.Path, d.position)
	// It makes every *byte* beyond this write stale too - end-of-data now
	// sits immediately after it, exactly as on real tape. See
	// truncateToEOD; a no-op for an ordinary sequential append, where the
	// file already ends where this write did.
	_ = truncateToEOD(vol.Path, d.position+int64(len(data)))
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
	if count > 0 {
		if err := truncateToEOD(vol.Path, d.position); err != nil {
			return d.fail(SenseAbortedCommand, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
		}
	}

	for i := 0; i < count; i++ {
		if err := recordFilemark(vol.Path, d.position); err != nil {
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
		fi, statErr := os.Stat(vol.Path)
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
		marks, mErr := readFilemarks(vol.Path)
		if mErr != nil {
			return d.fail(SenseAbortedCommand, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
		}
		sort.Slice(marks, func(i, j int) bool { return marks[i] < marks[j] })

		if count > 0 {
			newPos, found := filemarkForward(marks, d.position, count)
			d.position = newPos
			if found < count {
				fi, statErr := os.Stat(vol.Path)
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
		fi, statErr := os.Stat(vol.Path)
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
// moderate, not high, confidence. CP (change partition) is rejected
// outright, since this project has no concept of tape partitions at all.
func (d *Drive) locate(cdb []byte) tcmu.Response {
	if len(cdb) < 10 {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	if cdb[1]&0x02 != 0 { // CP - change partition
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	blockAddr := uint32(cdb[3])<<24 | uint32(cdb[4])<<16 | uint32(cdb[5])<<8 | uint32(cdb[6])

	if _, err := d.volume(); err != nil {
		return d.fail(SenseNotReady, AscLogicalUnitNotReady, AscqCauseNotReportable, senseFlags{})
	}
	d.position = int64(blockAddr)
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

// modeSense6 implements a minimal MODE SENSE(6): the 4-byte mode parameter
// header plus (unless DBD suppresses it) one block descriptor reporting a
// block length of 1 - consistent with this type's byte-count READ/WRITE(6)
// model. No mode pages are implemented yet; a page code other than 0
// (current page only, none returned) or 0x3F (all pages, none returned)
// is rejected.
func (d *Drive) modeSense6(cdb []byte, buffers [][]byte) tcmu.Response {
	if len(cdb) < 5 {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	dbd := cdb[1]&0x08 != 0
	pageCode := cdb[2] & 0x3F
	if pageCode != 0x00 && pageCode != 0x3F {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}

	full := make([]byte, 4)
	if !dbd {
		full = append(full, buildModeBlockDescriptor(1)...)
		full[3] = modeBlockDescriptorLen
	}
	full[0] = byte(len(full) - 1) // mode data length: n-1

	n := writeToBuffers(buffers, full)
	return tcmu.Response{Status: StatusGood, ReadLen: uint32(n)}
}

// modeSelect6 implements MODE SELECT(6) as an accept-but-ignore stub:
// this project's READ/WRITE(6) always treat transfer length as a literal
// byte count (see the type's doc comment), so there is no block-size
// setting a real MODE SELECT would change that this project currently
// acts on - a reasonable Milestone 3 addition alongside real block-size
// enforcement.
func (d *Drive) modeSelect6(cdb []byte) tcmu.Response {
	if len(cdb) < 5 {
		return d.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB, senseFlags{})
	}
	return d.ok()
}
