package scsi

import (
	"encoding/binary"
	"path/filepath"
	"sort"
	"strings"

	"github.com/swenske/gotochanger/internal/library"
	"github.com/swenske/gotochanger/internal/tcmu"
)

// LibraryClient is the subset of apiclient.Client's methods the SCSI
// command handlers need - defined structurally (the same convention
// internal/api's TopologyStore interface already uses for its own store
// dependency) so tests can fake it without a real gotochangerd.
// *apiclient.Client satisfies this today; internal/scsi deliberately
// doesn't import internal/apiclient itself, to keep this package
// independent of the HTTP transport.
type LibraryClient interface {
	Status() (library.Status, error)
	Load(fromKind string, fromAddr, drive int) error
	Unload(drive int, toKind string, toAddr int) error
	Move(fromKind string, fromAddr int, toKind string, toAddr int) error

	// OpenIODoor/CloseIODoor (Milestone 7) back
	// Changer.openCloseImportExportElement - *apiclient.Client already
	// implements both with this exact signature (they exist for the web
	// UI's own mailbox door dialog), so no apiclient/API/Library change
	// was needed to add this pair here, unlike Load/Unload/Move above.
	OpenIODoor(mailboxID, pin string) error
	CloseIODoor(mailboxID string, actions []library.DoorAction) error

	// SetDriveVolumeNumberOfPartitions (Milestone 8) backs
	// Drive.formatMedium - see Library.SetDriveVolumeNumberOfPartitions's
	// own doc comment for why this is addressed by drive index.
	SetDriveVolumeNumberOfPartitions(index, n int) error

	// SetDriveVolumeMAMAttributes (Milestone 9) backs
	// Drive.writeAttribute - see Library.SetDriveVolumeMAMAttributes's own
	// doc comment.
	SetDriveVolumeMAMAttributes(index int, attrs library.MAMAttributes) error

	// SetDriveVolumeEncrypted (Milestone 10) backs Drive.write6 - see
	// Library.SetDriveVolumeEncrypted's own doc comment.
	SetDriveVolumeEncrypted(index int, encrypted bool) error
}

// elementKind is an SMC-3 element type code (byte1 bits 3-0 of a READ
// ELEMENT STATUS CDB / byte0 bits 3-0 of an element status page header).
type elementKind uint8

const (
	elemAllTypes     elementKind = 0
	elemStorage      elementKind = 2
	elemImportExport elementKind = 3
	elemDataTransfer elementKind = 4
)

// elementDescriptorLen/elementDescriptorLenVolTag are the sizes of the
// element descriptor this project's changer reports, for every element
// type it supports, without and with a volume tag (barcode) respectively -
// verified against the Oracle StorageTek SL150 SCSI Reference Guide's
// per-type descriptor figures (storage/import-export/data-transfer element
// descriptors are all 20 bytes in their PVolTag=0/DvcID=0 form, 56 bytes
// with PVolTag=1). The volume tag sub-structure itself (bytes 12-47 of the
// PVolTag=1 form) is reported here as a 32-byte barcode field followed by
// 4 zeroed/reserved bytes - the exact internal split of those 36 bytes
// wasn't independently confirmed against the same source and should be
// double-checked against a real initiator if barcode reporting looks
// wrong there.
const (
	elementDescriptorLen       = 20
	elementDescriptorLenVolTag = 56
)

// Milestone 1 doesn't report a medium transport element descriptor for the
// robotic arm itself - a reasonable future addition once this has been
// proven against a real initiator.

// Changer implements every SMC-3 command in scope through Milestone 2 (see
// opcodes.go's package doc comment for the full list) against a
// LibraryClient. This is the changer-side half of cmd/gotochanger-tcmud's
// SCSI emulation, mirroring the role cmd/gotochanger-changer plays for
// Bareos's own changer-script convention (see that binary's own doc
// comment). Not yet implemented: EXCHANGE MEDIUM (explicitly deferred by
// the plan) and a medium transport element descriptor in READ ELEMENT
// STATUS (see elementDescriptorLen's doc comment).
type Changer struct {
	Client LibraryClient

	// NAA is this changer's 8-byte Device Identification VPD (page 0x83)
	// identifier - see inquiry's own doc comment and vpd.go. Set once at
	// construction (cmd/gotochanger-tcmud derives it from this device's
	// own unique backstore name, the same input deviceWWN already uses
	// for its own, unrelated internal fabric identity).
	NAA [8]byte

	// Identity (Milestone 5) is the vendor/product/revision INQUIRY
	// reports for this changer - see inquiry's own doc comment. The zero
	// value (Identity{}) means "use DefaultChangerIdentity"
	// (families.go): every existing caller/test that doesn't set this
	// field explicitly keeps reporting exactly what Changer.inquiry
	// always hardcoded before this field existed, unchanged.
	// cmd/gotochanger-tcmud is the only caller that ever sets this to
	// something else (RealisticChangerIdentity, opted into per logical
	// library via config.LogicalLibraryConfig.ChangerModel).
	Identity Identity

	lastSense []byte // set by any command that fails; reported (and cleared) by the next REQUEST SENSE

	// reserved/preventRemoval are RESERVE/RELEASE ELEMENT and PREVENT/
	// ALLOW MEDIUM REMOVAL's session-scoped state - a SCSI initiator's own
	// reservation is a per-I_T-nexus concept this project doesn't track at
	// the TCMU layer, so RESERVE/RELEASE are deliberately simplified to
	// always grant/always succeed rather than detecting a real conflict
	// between distinct initiators.
	//
	// Milestone 6 confirmed - empirically, against a real kernel, not just
	// from documentation - that this simplification is very likely never
	// actually exercised in kernel mode at all: Linux's LIO target core
	// answers PERSISTENT RESERVE IN/OUT (0x5E/0x5F) generically, before
	// the CDB ever reaches a TCMU backstore's userspace handler. Confirmed
	// directly with `sg_persist` against a real gotochanger-tcmud device
	// (register/reserve/read-reservation/release/unregister all completed
	// with correct, stateful PR semantics - key tracking, PR generation
	// incrementing, reservation type reported correctly - while this
	// package's own Handle, which has no case at all for 0x5E/0x5F,
	// logged zero activity for any of it; a CDB actually reaching Handle's
	// default case would have surfaced as CHECK CONDITION/ILLEGAL
	// REQUEST/INVALID COMMAND OPERATION CODE instead). The legacy
	// RESERVE(6)/RELEASE(6) case below wasn't independently distinguished
	// by that same test (Changer.reserve()/release() already return GOOD
	// unconditionally too, so a clean `sg_raw` GOOD status is consistent
	// with either code path actually running) - but the kernel's own
	// generic device attribute controlling this (target_core_base.h's
	// DA_EMULATE_PR, default on) is documented as covering "SCSI2
	// RESERVE/RELEASE and Persistent Reservations" together as the same
	// mechanism, so the same conclusion almost certainly holds here too.
	// Kept as-is (not deleted, not built out into full multi-initiator
	// PERSISTENT RESERVE IN/OUT) rather than either extreme: still useful
	// as a defensive fallback if this ever runs under a TCMU backstore
	// with emulate_pr disabled, and full userspace PR support would just
	// be redundant with correct behavior the kernel already provides for
	// free. PREVENT is tracked but not yet enforced anywhere (e.g. against
	// CloseStorageDoor) - a reasonable Milestone 7 addition once this is
	// exercised for real.
	reserved       bool
	preventRemoval bool

	// armPosition is POSITION TO ELEMENT's own bookkeeping: the unified
	// address the arm was last told to position at, no medium moved. Not
	// surfaced anywhere yet (no command reports it back) - tracked now so
	// a later addition (e.g. a status/diagnostic page) doesn't need to
	// revisit this handler.
	armPosition uint16

	// searchResults is SEND VOLUME TAG's own bookkeeping: the elements its
	// last search matched, returned by a subsequent REQUEST VOLUME ELEMENT
	// ADDRESS - see both methods' doc comments. Session-scoped, like
	// reserved/preventRemoval above: a real device would also treat this
	// as a per-I_T-nexus search result, not persisted domain state.
	searchResults []unifiedElement
}

// Handle dispatches one parsed command entry to the right SMC-3 handler,
// returning the tcmu.Response Cursor.Complete should write back.
func (c *Changer) Handle(entry tcmu.Entry) tcmu.Response {
	if len(entry.CDB) == 0 {
		return c.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB)
	}
	switch entry.CDB[0] {
	case OpTestUnitReady:
		return c.testUnitReady()
	case OpRequestSense:
		return c.requestSense(entry.Buffers)
	case OpInquiry:
		return c.inquiry(entry.CDB, entry.Buffers)
	case OpReadElementStatus:
		return c.readElementStatus(entry.CDB, entry.Buffers)
	case OpMoveMedium:
		return c.moveMedium(entry.CDB)
	case OpReserve6:
		return c.reserve()
	case OpRelease6:
		return c.release()
	case OpPreventAllowMediumRemoval:
		return c.preventAllowMediumRemoval(entry.CDB)
	case OpPositionToElement:
		return c.positionToElement(entry.CDB)
	case OpSendVolumeTag:
		return c.sendVolumeTag(entry.CDB, entry.Buffers)
	case OpRequestVolumeElementAddress:
		return c.requestVolumeElementAddress(entry.CDB, entry.Buffers)
	case OpModeSense6:
		return c.modeSense6(entry.CDB, entry.Buffers)
	case OpInitializeElementStatus:
		return c.initializeElementStatus()
	case OpInitializeElementStatusWithRange:
		return c.initializeElementStatus()
	case OpRezeroUnit:
		return c.rezeroUnit()
	case OpLogSense:
		return c.logSense(entry.CDB, entry.Buffers)
	case OpLogSelect:
		return c.logSelect(entry.CDB)
	case OpExchangeMedium:
		return c.exchangeMedium(entry.CDB)
	case OpOpenCloseImportExportElement:
		return c.openCloseImportExportElement(entry.CDB)
	default:
		return c.fail(SenseIllegalRequest, AscInvalidCommandOperationCode, AscqInvalidCommandOperationCode)
	}
}

func (c *Changer) ok() tcmu.Response { return tcmu.Response{Status: StatusGood} }

func (c *Changer) fail(key, asc, ascq uint8) tcmu.Response {
	c.lastSense = FixedSense(key, asc, ascq, senseFlags{})
	return tcmu.Response{Status: StatusCheckCondition, Sense: c.lastSense}
}

// reserve implements RESERVE(6) (0x16). See the Changer.reserved field's
// doc comment for why this always grants the reservation rather than
// detecting a real conflict between distinct initiators.
func (c *Changer) reserve() tcmu.Response {
	c.reserved = true
	return c.ok()
}

// release implements RELEASE(6) (0x17) - releasing when nothing is
// reserved is not an error, matching real SPC semantics.
func (c *Changer) release() tcmu.Response {
	c.reserved = false
	return c.ok()
}

// preventAllowMediumRemoval implements PREVENT ALLOW MEDIUM REMOVAL
// (0x1E): CDB byte4 bit0 is the Prevent flag. Tracked but not yet enforced
// anywhere - see the Changer.preventRemoval field's doc comment.
func (c *Changer) preventAllowMediumRemoval(cdb []byte) tcmu.Response {
	if len(cdb) < 5 {
		return c.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB)
	}
	c.preventRemoval = cdb[4]&0x01 != 0
	return c.ok()
}

// positionToElement implements SMC-3's POSITION TO ELEMENT (0x2B): moves
// only the robotic arm to the destination element's location, without
// picking up or placing any medium (see Changer.armPosition's doc
// comment) - this project has no Library call for "just move the arm", so
// unlike MOVE MEDIUM this never touches gotochangerd at all, only bumps
// local bookkeeping once the destination is confirmed to be a real
// element. The CDB layout was verified against the Oracle StorageTek
// SL150 SCSI Reference Guide.
func (c *Changer) positionToElement(cdb []byte) tcmu.Response {
	if len(cdb) < 9 {
		return c.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB)
	}
	dst := binary.BigEndian.Uint16(cdb[4:6])

	st, err := c.Client.Status()
	if err != nil {
		return c.fail(SenseNotReady, AscLogicalUnitNotReady, AscqCauseNotReportable)
	}
	found := false
	for _, e := range buildUnifiedElements(st) {
		if e.address == dst {
			found = true
			break
		}
	}
	if !found {
		return c.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB)
	}
	c.armPosition = dst
	return c.ok()
}

// initializeElementStatus implements both SMC's INITIALIZE ELEMENT STATUS
// (0x07) and INITIALIZE ELEMENT STATUS WITH RANGE (0x37) - verified
// against the Oracle StorageTek SL150 SCSI Reference Guide's own command
// list (checked specifically for this Milestone 3 addition), which
// documents both as bare, no-data-phase commands. A real changer uses
// these to trigger a physical barcode/occupancy re-scan; this project has
// no such cache to invalidate - Changer.readElementStatus and every other
// status-reporting command already call c.Client.Status() fresh on every
// invocation (see buildUnifiedElements), so there is nothing stale for
// this command to refresh. Accept-and-succeed, matching
// Drive.modeSelect6's existing "nothing to actually do" posture.
func (c *Changer) initializeElementStatus() tcmu.Response { return c.ok() }

// rezeroUnit implements SMC-2's legacy REZERO UNIT (0x01, not documented
// by the SL150 guide - an older, optional command real modern changers
// often no-op or reject): repositions the robotic arm to its home
// position, address 1 (the medium transport element - see
// buildUnifiedElements' own doc comment on why address 1 is reserved for
// it). Same bookkeeping-only posture as positionToElement: bumps
// Changer.armPosition without any Client call, since this project has no
// concept of "just move the arm" beyond that local field.
func (c *Changer) rezeroUnit() tcmu.Response {
	c.armPosition = 1
	return c.ok()
}

func (c *Changer) testUnitReady() tcmu.Response {
	if _, err := c.Client.Status(); err != nil {
		return c.fail(SenseNotReady, AscLogicalUnitNotReady, AscqCauseNotReportable)
	}
	return c.ok()
}

func (c *Changer) requestSense(buffers [][]byte) tcmu.Response {
	sense := c.lastSense
	if sense == nil {
		sense = FixedSense(SenseNoSense, AscNoAdditionalSenseInfo, AscqNoAdditionalSenseInfo, senseFlags{})
	}
	c.lastSense = nil
	n := writeToBuffers(buffers, sense)
	return tcmu.Response{Status: StatusGood, ReadLen: uint32(n)}
}

func (c *Changer) inquiry(cdb []byte, buffers [][]byte) tcmu.Response {
	if len(cdb) < 5 {
		return c.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB)
	}
	if cdb[1]&0x01 != 0 { // EVPD - vital product data page requested
		switch cdb[2] {
		case vpdPageSupportedPages:
			n := writeToBuffers(buffers, SupportedVPDPages(PeripheralDeviceTypeMediumChanger))
			return tcmu.Response{Status: StatusGood, ReadLen: uint32(n)}
		case vpdPageDeviceIdentification:
			n := writeToBuffers(buffers, DeviceIdentificationVPD(PeripheralDeviceTypeMediumChanger, c.NAA))
			return tcmu.Response{Status: StatusGood, ReadLen: uint32(n)}
		default:
			return c.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB)
		}
	}
	identity := c.Identity
	if identity == (Identity{}) {
		identity = DefaultChangerIdentity
	}
	resp := StandardInquiry(PeripheralDeviceTypeMediumChanger, identity)
	n := writeToBuffers(buffers, resp)
	return tcmu.Response{Status: StatusGood, ReadLen: uint32(n)}
}

// modeSense6 implements MODE SENSE(6) with the SMC Element Address
// Assignment page (0x1D) - a real medium changer's own real element
// addresses/counts, which mtx (and anything using mtx-changer, including
// Bareos's own bundled script) probes for before falling back to a full
// READ ELEMENT STATUS scan. Byte layout verified against two independent
// authoritative sources (an IBM TS4500/3584 SCSI reference and the Oracle
// SL150 SCSI Reference Guide), which agree exactly: a 2-byte page header
// (page code, page length=0x12) followed by 9 tightly-packed 2-byte
// big-endian fields (no reserved padding between them) - First/Number of
// Medium Transport, Storage, Import/Export, then Data Transfer elements,
// in that order.
//
// Found necessary for real, not defensively: without this, mtx (run for
// real against this project's own SMC-3 implementation, real hardware,
// 2026-07-25) issued this exact MODE SENSE, got back the previous
// unconditional "unsupported opcode" response, and printed a multi-line
// "mtx: Request Sense: ..." diagnostic dump to stderr before falling back
// to READ ELEMENT STATUS and still reporting the correct slot count -
// mtx itself handled the failure gracefully. Bareos's own bundled
// mtx-changer script did not: its "slots" case pipes the whole child
// process's combined stdout+stderr into Bareos SD's own AutochangerCmd(),
// which reads only the *first* line and parses it as the slot count
// (stored/autochanger.cc) - so mtx's diagnostic noise (landing first,
// ahead of the real "10" mtx-changer's own script echoes afterward) was
// silently parsed as "slots=0", making `bconsole update slots` report
// "Device Library1 has 0 slots" against a changer that was otherwise
// working correctly end-to-end. Implementing this page makes mtx's own
// probe succeed cleanly, so it never emits that diagnostic at all.
//
// Only page 0x1D (and 0x00/0x3F, matching Drive.modeSense6's existing
// convention: 0x00 returns just the header with no pages, 0x3F returns
// every implemented page) is supported; any other page code is rejected,
// same as Drive. A changer has no concept of a block descriptor (it's not
// a block device), so unlike Drive.modeSense6 the DBD bit is irrelevant
// here and none is ever returned.
func (c *Changer) modeSense6(cdb []byte, buffers [][]byte) tcmu.Response {
	if len(cdb) < 5 {
		return c.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB)
	}
	pageCode := cdb[2] & 0x3F
	if pageCode != 0x00 && pageCode != 0x1D && pageCode != 0x3F {
		return c.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB)
	}

	full := make([]byte, 4)
	if pageCode == 0x1D || pageCode == 0x3F {
		st, err := c.Client.Status()
		if err != nil {
			return c.fail(SenseNotReady, AscLogicalUnitNotReady, AscqCauseNotReportable)
		}
		full = append(full, buildElementAddressAssignmentPage(st)...)
	}
	full[0] = byte(len(full) - 1) // mode data length: n-1

	n := writeToBuffers(buffers, full)
	return tcmu.Response{Status: StatusGood, ReadLen: uint32(n)}
}

// buildElementAddressAssignmentPage builds the 20-byte (2-byte header +
// 18-byte payload, per its own Page Length=0x12) Element Address
// Assignment mode page - see modeSense6's doc comment for the byte layout
// and its source. Element addresses/counts are derived from
// buildUnifiedElements, the same single source of truth READ ELEMENT
// STATUS/MOVE MEDIUM already use, so this can never disagree with what
// those commands report. The medium transport element is hardcoded to
// address 1, count 1 - buildUnifiedElements reserves that address but
// never emits an actual descriptor for it (see its own doc comment), so
// it isn't present in the element list to derive a range from the way the
// other three kinds are.
func buildElementAddressAssignmentPage(st library.Status) []byte {
	elements := buildUnifiedElements(st)
	storageFirst, storageCount := elementAddressRange(elements, elemStorage)
	ioFirst, ioCount := elementAddressRange(elements, elemImportExport)
	driveFirst, driveCount := elementAddressRange(elements, elemDataTransfer)

	p := make([]byte, 20)
	p[0] = 0x1D
	p[1] = 0x12
	binary.BigEndian.PutUint16(p[2:4], 1) // first medium transport element address
	binary.BigEndian.PutUint16(p[4:6], 1) // number of medium transport elements
	binary.BigEndian.PutUint16(p[6:8], storageFirst)
	binary.BigEndian.PutUint16(p[8:10], storageCount)
	binary.BigEndian.PutUint16(p[10:12], ioFirst)
	binary.BigEndian.PutUint16(p[12:14], ioCount)
	binary.BigEndian.PutUint16(p[14:16], driveFirst)
	binary.BigEndian.PutUint16(p[16:18], driveCount)
	return p
}

// elementAddressRange returns the lowest address and count of elements of
// the given kind - safe to take the first match's address as the minimum
// without scanning every element, since buildUnifiedElements always
// appends one kind's whole contiguous group (sorted, gapless) before
// moving to the next.
func elementAddressRange(elements []unifiedElement, kind elementKind) (first, count uint16) {
	for _, e := range elements {
		if e.kind != kind {
			continue
		}
		if count == 0 {
			first = e.address
		}
		count++
	}
	return first, count
}

// unifiedElement is one SMC element in the single, shared address space
// every element type occupies together in real SCSI SMC. This is
// deliberately NOT internal/addressing's Addressing type: that package
// keeps drives numbered separately from slots/ioslots to match Bareos's
// own two-positional-argument changer-script convention, but real SCSI SMC
// has no such split - every element type (medium transport, storage,
// import/export, data transfer) shares one dense address space, so
// READ ELEMENT STATUS/MOVE MEDIUM need their own numbering here instead.
type unifiedElement struct {
	kind          elementKind
	address       uint16
	physicalAddr  int // physical slot/ioslot address (library.Slot.Address/library.IOSlot.Address) - meaningful for elemStorage/elemImportExport
	physicalDrive int // physical drive index (library.Drive.Index) - meaningful for elemDataTransfer
	full          bool
	except        bool
	barcode       string // this element's occupying Volume.Barcode, empty if not full
	mailboxID     string // library.IOSlot.MailboxID - meaningful for elemImportExport only, used by Changer.openCloseImportExportElement (Milestone 7)
}

// buildUnifiedElements assigns one dense address space across storage
// slots, then I/O slots, then drives. Address 1 is conventionally the
// medium transport element's (the robot arm) own address in a real
// changer; Milestone 1 doesn't report a transport element descriptor, so
// it's simply skipped, keeping every other address stable if that's added
// later.
func buildUnifiedElements(st library.Status) []unifiedElement {
	slots := append([]*library.Slot(nil), st.Slots...)
	sort.Slice(slots, func(i, j int) bool { return slots[i].Address < slots[j].Address })
	ioslots := append([]*library.IOSlot(nil), st.IOSlots...)
	sort.Slice(ioslots, func(i, j int) bool { return ioslots[i].Address < ioslots[j].Address })
	drives := append([]*library.Drive(nil), st.Drives...)
	sort.Slice(drives, func(i, j int) bool { return drives[i].Index < drives[j].Index })

	var out []unifiedElement
	addr := uint16(2)
	for _, s := range slots {
		e := unifiedElement{kind: elemStorage, address: addr, physicalAddr: s.Address, full: s.Volume != nil}
		if s.Volume != nil {
			e.barcode = s.Volume.Barcode
		}
		out = append(out, e)
		addr++
	}
	for _, io := range ioslots {
		e := unifiedElement{kind: elemImportExport, address: addr, physicalAddr: io.Address, full: io.Volume != nil, mailboxID: io.MailboxID}
		if io.Volume != nil {
			e.barcode = io.Volume.Barcode
		}
		out = append(out, e)
		addr++
	}
	for _, d := range drives {
		e := unifiedElement{kind: elemDataTransfer, address: addr, physicalDrive: d.Index, full: d.Volume != nil, except: d.Fault}
		if d.Volume != nil {
			e.barcode = d.Volume.Barcode
		}
		out = append(out, e)
		addr++
	}
	return out
}

func kindString(k elementKind) string {
	switch k {
	case elemStorage:
		return "slot"
	case elemImportExport:
		return "ioslot"
	default:
		return ""
	}
}

// buildElementDescriptor builds one element descriptor, in the no-volume-
// tag (20-byte) form or, when voltag is true, the with-volume-tag (56-byte)
// form - see elementDescriptorLen/elementDescriptorLenVolTag's doc comment
// for the source and the one unconfirmed detail (the volume tag
// sub-structure's internal 32+4 byte split).
func buildElementDescriptor(e unifiedElement, voltag bool) []byte {
	length := elementDescriptorLen
	if voltag {
		length = elementDescriptorLenVolTag
	}
	d := make([]byte, length)
	binary.BigEndian.PutUint16(d[0:2], e.address)
	var b2 uint8
	if e.kind == elemImportExport {
		b2 |= 1<<5 | 1<<4 // InEnab=1, ExEnab=1 - always report import/export enabled
	}
	b2 |= 1 << 3 // Access=1 - always accessible in Milestone 1 (no robotic-fault-aware Access=0 yet)
	if e.except {
		b2 |= 1 << 2
	}
	if e.full {
		b2 |= 1 << 0
	}
	d[2] = b2
	if voltag && e.barcode != "" {
		copy(d[12:44], padASCII(e.barcode, 32))
		// d[44:48] (volume sequence number) left zero - this project has
		// no concept of a multi-volume sequence.
	}
	return d
}

// selectElements filters elements to those with address >= startAddr and
// (unless typ is elemAllTypes) of the given kind, capped at numElements -
// the common selection logic behind READ ELEMENT STATUS's own filtering
// and (applied to Changer.searchResults instead of every element) REQUEST
// VOLUME ELEMENT ADDRESS's.
func selectElements(elements []unifiedElement, typ elementKind, startAddr, numElements uint16) []unifiedElement {
	var selected []unifiedElement
	for _, e := range elements {
		if e.address < startAddr {
			continue
		}
		if typ != elemAllTypes && typ != e.kind {
			continue
		}
		if len(selected) >= int(numElements) {
			break
		}
		selected = append(selected, e)
	}
	return selected
}

// buildElementStatusBody builds the paginated element-descriptor body
// both READ ELEMENT STATUS and REQUEST VOLUME ELEMENT ADDRESS return: one
// 8-byte "element status page" header per element type present in
// selected, each followed by that type's element descriptors.
//
// page[1]'s top bit (PVolTag) must be set whenever voltag is true - found
// missing for real, not defensively: a real `mtx` invocation (2026-07-25,
// against bareos-disk-sd-int-fr1) correctly requested voltag data (CDB
// byte1 bit4 set) and this project correctly sized every descriptor as
// the 56-byte voltag form and correctly embedded each cartridge's real
// barcode at the right offset (confirmed byte-for-byte via `strace` on
// the real mtx process) - but with PVolTag left at 0 in the page header,
// a spec-compliant initiator has no signal that volume tag data is
// present at all and correctly ignores it, so `mtx status` showed every
// full slot with no ":VolumeTag=..." annotation, and Bareos's own
// barcode-driven `update slots`/"list" scan (which parses exactly that
// annotation) reported "No Volumes found to update, or no barcodes."
// despite the real cartridges/barcodes being right there in the
// response. AVolTag (bit6, "alternate volume tag") is never set - this
// project has no concept of a secondary/alternate volume identifier to
// report.
func buildElementStatusBody(selected []unifiedElement, voltag bool) []byte {
	descLen := elementDescriptorLen
	if voltag {
		descLen = elementDescriptorLenVolTag
	}
	var body []byte
	for i := 0; i < len(selected); {
		kind := selected[i].kind
		j := i
		var descs []byte
		for j < len(selected) && selected[j].kind == kind {
			descs = append(descs, buildElementDescriptor(selected[j], voltag)...)
			j++
		}
		page := make([]byte, 8)
		page[0] = byte(kind)
		if voltag {
			page[1] |= 1 << 7 // PVolTag
		}
		binary.BigEndian.PutUint16(page[2:4], uint16(descLen))
		byteCount := (j - i) * descLen
		page[5] = byte(byteCount >> 16)
		page[6] = byte(byteCount >> 8)
		page[7] = byte(byteCount)
		body = append(body, page...)
		body = append(body, descs...)
		i = j
	}
	return body
}

// buildElementStatusHeader builds the 8-byte header shared by READ ELEMENT
// STATUS and REQUEST VOLUME ELEMENT ADDRESS - byte4's low 5 bits differ
// (always 0 for READ ELEMENT STATUS; a fixed Send Action Code of 5 for
// REQUEST VOLUME ELEMENT ADDRESS, echoing the code SEND VOLUME TAG itself
// always uses - see sendActionCode).
func buildElementStatusHeader(selected []unifiedElement, body []byte, byte4LowBits uint8) []byte {
	header := make([]byte, 8)
	if len(selected) > 0 {
		binary.BigEndian.PutUint16(header[0:2], selected[0].address)
	}
	binary.BigEndian.PutUint16(header[2:4], uint16(len(selected)))
	header[4] = byte4LowBits & 0x1F
	total := len(body)
	header[5] = byte(total >> 16)
	header[6] = byte(total >> 8)
	header[7] = byte(total)
	return header
}

// sendActionCode is the one Send Action Code this project (and, per the
// Oracle StorageTek SL150 SCSI Reference Guide, real hardware) supports:
// "translate and search primary volume tag" - echoed in REQUEST VOLUME
// ELEMENT ADDRESS's response header.
const sendActionCode = 0x05

// readElementStatus implements SMC-3's READ ELEMENT STATUS (0xB8), whose
// CDB and response byte layouts were verified against the Oracle
// StorageTek SL150 SCSI Reference Guide's own figures (not reconstructed
// from memory): a CDB requesting an element type code, starting address,
// element count, and allocation length; a response of an 8-byte header,
// then one 8-byte "element status page" header per element type present
// in the selection, each followed by that many element descriptors.
func (c *Changer) readElementStatus(cdb []byte, buffers [][]byte) tcmu.Response {
	if len(cdb) < 10 {
		return c.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB)
	}
	requestedType := elementKind(cdb[1] & 0x0F)
	voltag := cdb[1]&0x10 != 0
	startAddr := binary.BigEndian.Uint16(cdb[2:4])
	numElements := binary.BigEndian.Uint16(cdb[4:6])
	allocLen := int(cdb[7])<<16 | int(cdb[8])<<8 | int(cdb[9])

	st, err := c.Client.Status()
	if err != nil {
		return c.fail(SenseNotReady, AscLogicalUnitNotReady, AscqCauseNotReportable)
	}

	selected := selectElements(buildUnifiedElements(st), requestedType, startAddr, numElements)
	body := buildElementStatusBody(selected, voltag)
	header := buildElementStatusHeader(selected, body, 0)

	full := append(header, body...)
	if len(full) > allocLen {
		full = full[:allocLen]
	}
	n := writeToBuffers(buffers, full)
	return tcmu.Response{Status: StatusGood, ReadLen: uint32(n)}
}

// sendVolumeTag implements SMC-3's SEND VOLUME TAG (0xB6): the data-out
// phase carries a 40-byte parameter list whose first 32 bytes are a NUL-
// terminated ASCII volume-identification search template (wildcards '*'
// and '?', per the Oracle StorageTek SL150 SCSI Reference Guide, the same
// convention path/filepath.Match already implements - the one difference,
// Match's extra "[...]" character-class support, is harmless for barcode
// strings). A zero parameter list length is valid and simply clears any
// previous search. Matches are stored for a subsequent REQUEST VOLUME
// ELEMENT ADDRESS to return (see Changer.searchResults) - this project
// resolves the search immediately, synchronously, unlike real hardware,
// which may need to physically scan barcodes first.
func (c *Changer) sendVolumeTag(cdb []byte, buffers [][]byte) tcmu.Response {
	if len(cdb) < 10 {
		return c.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB)
	}
	requestedType := elementKind(cdb[1] & 0x0F)
	paramListLen := int(cdb[8])<<8 | int(cdb[9])

	if paramListLen == 0 {
		c.searchResults = nil
		return c.ok()
	}

	data := make([]byte, 0, paramListLen)
	for _, buf := range buffers {
		if len(data) >= paramListLen {
			break
		}
		take := paramListLen - len(data)
		if take > len(buf) {
			take = len(buf)
		}
		data = append(data, buf[:take]...)
	}
	if len(data) < 32 {
		return c.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB)
	}
	template := string(data[:32])
	if i := strings.IndexByte(template, 0); i >= 0 {
		template = template[:i]
	}
	template = strings.TrimSpace(template)
	if template == "" {
		c.searchResults = nil
		return c.ok()
	}

	st, err := c.Client.Status()
	if err != nil {
		return c.fail(SenseNotReady, AscLogicalUnitNotReady, AscqCauseNotReportable)
	}
	var matched []unifiedElement
	for _, e := range buildUnifiedElements(st) {
		if requestedType != elemAllTypes && requestedType != e.kind {
			continue
		}
		if e.barcode == "" {
			continue
		}
		if ok, _ := filepath.Match(template, e.barcode); ok {
			matched = append(matched, e)
		}
	}
	c.searchResults = matched
	return c.ok()
}

// requestVolumeElementAddress implements SMC-3's REQUEST VOLUME ELEMENT
// ADDRESS (0xB5): returns element status pages for whatever the last
// SEND VOLUME TAG matched (empty if none has run yet, or the last search
// found nothing) - see Changer.searchResults and Changer.sendVolumeTag.
func (c *Changer) requestVolumeElementAddress(cdb []byte, buffers [][]byte) tcmu.Response {
	if len(cdb) < 10 {
		return c.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB)
	}
	voltag := cdb[1]&0x10 != 0
	startAddr := binary.BigEndian.Uint16(cdb[2:4])
	numElements := binary.BigEndian.Uint16(cdb[4:6])
	allocLen := int(cdb[7])<<16 | int(cdb[8])<<8 | int(cdb[9])

	selected := selectElements(c.searchResults, elemAllTypes, startAddr, numElements)
	body := buildElementStatusBody(selected, voltag)
	header := buildElementStatusHeader(selected, body, sendActionCode)

	full := append(header, body...)
	if len(full) > allocLen {
		full = full[:allocLen]
	}
	n := writeToBuffers(buffers, full)
	return tcmu.Response{Status: StatusGood, ReadLen: uint32(n)}
}

// moveMedium implements SMC-3's MOVE MEDIUM (0xA5): resolves the CDB's
// source/destination unified element addresses back to physical terms and
// dispatches to whichever of Load/Unload/Move actually applies, exactly
// like cmd/gotochanger-changer's own transfer/load/unload dispatch does
// for Bareos's changer-script convention - the CDB layout was verified
// against the Oracle StorageTek SL150 SCSI Reference Guide.
func (c *Changer) moveMedium(cdb []byte) tcmu.Response {
	if len(cdb) < 11 {
		return c.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB)
	}
	src := binary.BigEndian.Uint16(cdb[4:6])
	dst := binary.BigEndian.Uint16(cdb[6:8])

	st, err := c.Client.Status()
	if err != nil {
		return c.fail(SenseNotReady, AscLogicalUnitNotReady, AscqCauseNotReportable)
	}
	elements := buildUnifiedElements(st)
	byAddr := make(map[uint16]unifiedElement, len(elements))
	for _, e := range elements {
		byAddr[e.address] = e
	}
	srcElem, ok := byAddr[src]
	if !ok {
		return c.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB)
	}
	dstElem, ok := byAddr[dst]
	if !ok {
		return c.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB)
	}
	if !srcElem.full {
		return c.fail(SenseIllegalRequest, AscMediumSourceElementEmpty, AscqMediumSourceElementEmpty)
	}
	if dstElem.full {
		return c.fail(SenseIllegalRequest, AscMediumDestinationElementFull, AscqMediumDestinationElementFull)
	}

	if err := c.moveOne(srcElem, dstElem); err != nil {
		return c.failMoveErr(err)
	}
	return c.ok()
}

// logSense implements SMC's LOG SENSE (0x4D), the changer-side twin of
// Drive.logSense - same CDB shape, same two supported pages (0x00/0x2E),
// but built from Changer's own robotic-fault/door state (see
// changerTapeAlertParams in logsense.go) rather than a Drive's.
func (c *Changer) logSense(cdb []byte, buffers [][]byte) tcmu.Response {
	if len(cdb) < 9 {
		return c.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB)
	}
	pageCode := cdb[2] & 0x3F
	allocLen := int(binary.BigEndian.Uint16(cdb[7:9]))

	var full []byte
	switch pageCode {
	case logPageSupportedPages:
		full = buildSupportedLogPages(logPageTapeAlert)
	case logPageTapeAlert:
		st, err := c.Client.Status()
		if err != nil {
			return c.fail(SenseNotReady, AscLogicalUnitNotReady, AscqCauseNotReportable)
		}
		full = buildLogSensePage(logPageTapeAlert, changerTapeAlertParams(st))
	default:
		return c.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB)
	}
	if len(full) > allocLen {
		full = full[:allocLen]
	}
	n := writeToBuffers(buffers, full)
	return tcmu.Response{Status: StatusGood, ReadLen: uint32(n)}
}

// logSelect implements LOG SELECT(6) as an accept-but-ignore stub, same
// posture as Drive.logSelect/modeSelect6.
func (c *Changer) logSelect(cdb []byte) tcmu.Response {
	if len(cdb) < 9 {
		return c.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB)
	}
	return c.ok()
}

// moveOne dispatches one element-to-element transfer through the right
// LibraryClient call (Load/Unload/Move), exactly like moveMedium's own
// dispatch always has - factored out (Milestone 7) so exchangeMedium can
// sequence several without duplicating this logic.
func (c *Changer) moveOne(src, dst unifiedElement) error {
	switch {
	case src.kind == elemDataTransfer:
		return c.Client.Unload(src.physicalDrive, kindString(dst.kind), dst.physicalAddr)
	case dst.kind == elemDataTransfer:
		return c.Client.Load(kindString(src.kind), src.physicalAddr, dst.physicalDrive)
	default:
		return c.Client.Move(kindString(src.kind), src.physicalAddr, kindString(dst.kind), dst.physicalAddr)
	}
}

// failMoveErr classifies a LibraryClient error from moveOne into a sense
// triple via senseForLibraryError, matching moveMedium's own existing
// failure handling.
func (c *Changer) failMoveErr(err error) tcmu.Response {
	key, asc, ascq := senseForLibraryError(err)
	return c.fail(key, asc, ascq)
}

// exchangeMedium implements SMC-3's EXCHANGE MEDIUM (0xA6): CDB bytes2-3=
// Medium Transport Address (source), bytes4-5=First Exchange Destination
// Address, bytes6-7=Second Exchange Destination Address (0 = "use the
// source element", the classic two-location swap case, per SMC-3
// convention). Opcode/CDB field layout is from established SMC knowledge
// at moderate, not high, confidence - the SL150 guide (this package's
// usual changer-side source) doesn't document this opcode at all, and a
// real SL150 doesn't implement it either - consistent with EXCHANGE
// MEDIUM being genuinely rarely-used/optional in real SMC-3 hardware, not
// evidence this package is missing something commonly needed.
//
// This project's Library API only exposes element-to-element Load/Unload/
// Move, each requiring an empty destination - there's no "hold in the
// robotic arm's own gripper" primitive a real device's single mechanical
// EXCHANGE MEDIUM operation uses to swap two already-occupied elements
// directly. The classic swap case (second destination 0, or equal to the
// source) is simulated instead by borrowing any currently-free storage
// slot as temporary scratch space - invisible to the initiator, since the
// net result at completion is exactly a swap - via three sequential
// moveOne calls; MediumDestinationElementFull is reported if no free slot
// exists anywhere to borrow (a genuine, if narrow, simulation gap on an
// entirely full library that real hardware wouldn't have, since a real
// arm's gripper is always available as its own temporary slot). The
// second-destination-is-a-distinct-third-location variant needs only two
// moveOne calls and no borrowed scratch space at all. Neither variant's
// multi-step sequence is atomic across a mid-sequence failure the way a
// real single mechanical operation would be - same no-rollback posture
// moveMedium's own MOVE MEDIUM handling already has, not a new limitation
// introduced here.
func (c *Changer) exchangeMedium(cdb []byte) tcmu.Response {
	if len(cdb) < 11 {
		return c.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB)
	}
	src := binary.BigEndian.Uint16(cdb[2:4])
	dst1 := binary.BigEndian.Uint16(cdb[4:6])
	dst2 := binary.BigEndian.Uint16(cdb[6:8])
	if dst2 == 0 {
		dst2 = src
	}

	st, err := c.Client.Status()
	if err != nil {
		return c.fail(SenseNotReady, AscLogicalUnitNotReady, AscqCauseNotReportable)
	}
	elements := buildUnifiedElements(st)
	byAddr := make(map[uint16]unifiedElement, len(elements))
	for _, e := range elements {
		byAddr[e.address] = e
	}
	srcElem, ok := byAddr[src]
	if !ok {
		return c.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB)
	}
	dst1Elem, ok := byAddr[dst1]
	if !ok {
		return c.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB)
	}
	if !srcElem.full {
		return c.fail(SenseIllegalRequest, AscMediumSourceElementEmpty, AscqMediumSourceElementEmpty)
	}
	if !dst1Elem.full {
		return c.fail(SenseIllegalRequest, AscMediumSourceElementEmpty, AscqMediumSourceElementEmpty)
	}

	if dst2 == src {
		var scratch *unifiedElement
		for i := range elements {
			if elements[i].kind == elemStorage && !elements[i].full {
				scratch = &elements[i]
				break
			}
		}
		if scratch == nil {
			return c.fail(SenseIllegalRequest, AscMediumDestinationElementFull, AscqMediumDestinationElementFull)
		}
		if err := c.moveOne(srcElem, *scratch); err != nil {
			return c.failMoveErr(err)
		}
		if err := c.moveOne(dst1Elem, srcElem); err != nil {
			return c.failMoveErr(err)
		}
		if err := c.moveOne(*scratch, dst1Elem); err != nil {
			return c.failMoveErr(err)
		}
		return c.ok()
	}

	dst2Elem, ok := byAddr[dst2]
	if !ok {
		return c.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB)
	}
	if dst2Elem.full {
		return c.fail(SenseIllegalRequest, AscMediumDestinationElementFull, AscqMediumDestinationElementFull)
	}
	if err := c.moveOne(dst1Elem, dst2Elem); err != nil {
		return c.failMoveErr(err)
	}
	if err := c.moveOne(srcElem, dst1Elem); err != nil {
		return c.failMoveErr(err)
	}
	return c.ok()
}

// openCloseImportExportElement implements SMC's OPEN/CLOSE IMPORT/EXPORT
// ELEMENT (0x1B) - opcode/CDB field layout from established SMC knowledge
// at moderate, not high, confidence (same posture as exchangeMedium
// above): byte1 bit0=IMMED (ignored, every operation here already
// completes synchronously), bytes2-3=Element Address (a unified
// import/export element address), byte4 bits1-0=Action Code (0=close,
// 1=open; any other value rejected rather than guessed at).
//
// Maps directly onto this project's existing mailbox-door concept
// (Library.OpenIODoor/CloseIODoor) via the addressed I/O element's own
// MailboxID (see unifiedElement.mailboxID/buildUnifiedElements) - a real
// device opens/closes one physical door per command, the same
// granularity. CLOSE always passes an empty action list: a raw SCSI
// command carries no per-cartridge routing information the way the web
// UI's own close-door dialog does, so there is nothing to stage here - an
// initiator that wants specific placement uses MOVE MEDIUM/READ ELEMENT
// STATUS itself once the door is physically closed, same as real hardware
// expects. OPEN always passes an empty PIN: a raw SCSI command has no CDB
// field to carry one, so a PIN-protected mailbox's door genuinely cannot
// be opened via this command - it fails with whatever sense
// senseForLibraryError's generic fallback reports for ErrPINRequired, a
// real and documented limitation, not an oversight.
func (c *Changer) openCloseImportExportElement(cdb []byte) tcmu.Response {
	if len(cdb) < 5 {
		return c.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB)
	}
	addr := binary.BigEndian.Uint16(cdb[2:4])
	action := cdb[4] & 0x03

	st, err := c.Client.Status()
	if err != nil {
		return c.fail(SenseNotReady, AscLogicalUnitNotReady, AscqCauseNotReportable)
	}
	var mailboxID string
	found := false
	for _, e := range buildUnifiedElements(st) {
		if e.address == addr && e.kind == elemImportExport {
			mailboxID = e.mailboxID
			found = true
			break
		}
	}
	if !found {
		return c.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB)
	}

	var doErr error
	switch action {
	case 0:
		doErr = c.Client.CloseIODoor(mailboxID, nil)
	case 1:
		doErr = c.Client.OpenIODoor(mailboxID, "")
	default:
		return c.fail(SenseIllegalRequest, AscInvalidFieldInCDB, AscqInvalidFieldInCDB)
	}
	if doErr != nil {
		key, asc, ascq := senseForLibraryError(doErr)
		return c.fail(key, asc, ascq)
	}
	return c.ok()
}

// writeToBuffers copies data into buffers (one []byte per iovec, as
// tcmu.Entry provides them) in order, stopping once either is exhausted,
// and returns how many bytes were actually copied - callers report this as
// Response.ReadLen.
func writeToBuffers(buffers [][]byte, data []byte) int {
	n := 0
	for _, buf := range buffers {
		if len(data) == 0 {
			break
		}
		c := copy(buf, data)
		data = data[c:]
		n += c
	}
	return n
}
