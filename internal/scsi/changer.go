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

	lastSense []byte // set by any command that fails; reported (and cleared) by the next REQUEST SENSE

	// reserved/preventRemoval are RESERVE/RELEASE ELEMENT and PREVENT/
	// ALLOW MEDIUM REMOVAL's session-scoped state (see the plan's
	// decision on why this isn't persisted domain state) - a SCSI
	// initiator's own reservation is a per-I_T-nexus concept this project
	// doesn't track at the TCMU layer, so RESERVE/RELEASE are
	// deliberately simplified to always grant/always succeed rather than
	// detecting a real conflict between distinct initiators. PREVENT is
	// tracked but not yet enforced anywhere (e.g. against CloseStorageDoor)
	// - a reasonable Milestone 3 addition once this is exercised for real.
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
	resp := StandardInquiry(PeripheralDeviceTypeMediumChanger, Identity{Vendor: "GOTOCHNG", Product: "Virtual Changer", Revision: "0100"})
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
		e := unifiedElement{kind: elemImportExport, address: addr, physicalAddr: io.Address, full: io.Volume != nil}
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

	var moveErr error
	switch {
	case srcElem.kind == elemDataTransfer:
		moveErr = c.Client.Unload(srcElem.physicalDrive, kindString(dstElem.kind), dstElem.physicalAddr)
	case dstElem.kind == elemDataTransfer:
		moveErr = c.Client.Load(kindString(srcElem.kind), srcElem.physicalAddr, dstElem.physicalDrive)
	default:
		moveErr = c.Client.Move(kindString(srcElem.kind), srcElem.physicalAddr, kindString(dstElem.kind), dstElem.physicalAddr)
	}
	if moveErr != nil {
		key, asc, ascq := senseForLibraryError(moveErr)
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
