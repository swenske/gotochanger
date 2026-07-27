package scsi

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/swenske/gotochanger/internal/library"
)

// driveStatusWithFile creates a temp backing file (initial content) and
// returns a Status with one drive holding a Volume pointed at it, plus the
// file's path for direct inspection.
func driveStatusWithFile(t *testing.T, initial []byte, capacity int64) (library.Status, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "VOL001")
	if err := os.WriteFile(path, initial, 0o644); err != nil {
		t.Fatalf("write backing file: %v", err)
	}
	return library.Status{
		Drives: []*library.Drive{{Index: 0, Volume: &library.Volume{Barcode: "VOL001", Path: path, CapacityBytes: capacity}}},
	}, path
}

func readCDB(sili bool, n int) []byte {
	cdb := make([]byte, 6)
	cdb[0] = OpRead6
	if sili {
		cdb[1] |= 0x02
	}
	cdb[2] = byte(n >> 16)
	cdb[3] = byte(n >> 8)
	cdb[4] = byte(n)
	return cdb
}

func writeCDB(n int) []byte {
	cdb := make([]byte, 6)
	cdb[0] = OpWrite6
	cdb[2] = byte(n >> 16)
	cdb[3] = byte(n >> 8)
	cdb[4] = byte(n)
	return cdb
}

func TestDriveTestUnitReadyEmptyVsLoaded(t *testing.T) {
	empty := &Drive{Client: &fakeClient{st: library.Status{Drives: []*library.Drive{{Index: 0}}}}}
	resp := empty.Handle(entryWithCDB([]byte{OpTestUnitReady, 0, 0, 0, 0, 0}))
	if resp.Status != StatusCheckCondition || resp.Sense[12] != AscMediumNotPresent {
		t.Fatalf("empty drive resp = %+v", resp)
	}

	st, _ := driveStatusWithFile(t, nil, 0)
	loaded := &Drive{Client: &fakeClient{st: st}}
	resp2 := loaded.Handle(entryWithCDB([]byte{OpTestUnitReady, 0, 0, 0, 0, 0}))
	if resp2.Status != StatusGood {
		t.Fatalf("loaded drive resp = %+v", resp2)
	}
}

// TestDriveVolumeLooksUpByPhysicalIndexNotArrayPosition reproduces the
// real bug in Drive.volume's doc comment: a logical-library-scoped
// Status().Drives slice (as d.Client.Status() returns once
// gotochanger-tcmud is scoped via --logical-library) positions entries
// by array order starting at 0, not by physical index - mirroring this
// project's own real Library2, whose drives are physically indexed 4/5.
// Before the fix, d.volume() indexed st.Drives directly by d.Index
// (st.Drives[4] against a 2-element slice), always failing with "drive
// not found" regardless of whether a volume was actually loaded.
func TestDriveVolumeLooksUpByPhysicalIndexNotArrayPosition(t *testing.T) {
	st := library.Status{Drives: []*library.Drive{
		{Index: 4, Volume: &library.Volume{Barcode: "VOL004"}},
		{Index: 5},
	}}
	d := &Drive{Client: &fakeClient{st: st}, Index: 4}
	resp := d.Handle(entryWithCDB([]byte{OpTestUnitReady, 0, 0, 0, 0, 0}))
	if resp.Status != StatusGood {
		t.Fatalf("resp = %+v, want GOOD (a real volume is loaded at physical index 4)", resp)
	}
}

func TestDriveInquiry(t *testing.T) {
	st, _ := driveStatusWithFile(t, nil, 0)
	naa := [8]byte{0x50, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x11}
	d := &Drive{Client: &fakeClient{st: st}, Family: FamilyFor("LTO-9"), NAA: naa}
	buf := make([]byte, StandardInquiryLength)
	resp := d.Handle(entryWithCDB([]byte{OpInquiry, 0, 0, 0, byte(StandardInquiryLength), 0}, buf))
	if resp.Status != StatusGood {
		t.Fatalf("resp = %+v", resp)
	}
	if devType := buf[0] & 0x1F; devType != PeripheralDeviceTypeSequentialAccess {
		t.Errorf("peripheral device type = %#x, want sequential access", devType)
	}
	product := string(buf[16:32])
	if product[:13] != "Virtual LTO-9" {
		t.Errorf("product = %q, want prefix %q", product, "Virtual LTO-9")
	}

	// EVPD page 0x00 - Supported VPD Pages.
	vpdBuf := make([]byte, 32)
	resp0 := d.Handle(entryWithCDB([]byte{OpInquiry, 0x01, 0x00, 0, byte(len(vpdBuf)), 0}, vpdBuf))
	if resp0.Status != StatusGood {
		t.Fatalf("EVPD page 0x00 status = %#x, want GOOD", resp0.Status)
	}
	want0 := SupportedVPDPages(PeripheralDeviceTypeSequentialAccess)
	if got := vpdBuf[:resp0.ReadLen]; !bytes.Equal(got, want0) {
		t.Errorf("EVPD page 0x00 = % x, want % x", got, want0)
	}

	// EVPD page 0x83 - Device Identification.
	resp83 := d.Handle(entryWithCDB([]byte{OpInquiry, 0x01, 0x83, 0, byte(len(vpdBuf)), 0}, vpdBuf))
	if resp83.Status != StatusGood {
		t.Fatalf("EVPD page 0x83 status = %#x, want GOOD", resp83.Status)
	}
	want83 := DeviceIdentificationVPD(PeripheralDeviceTypeSequentialAccess, naa)
	if got := vpdBuf[:resp83.ReadLen]; !bytes.Equal(got, want83) {
		t.Errorf("EVPD page 0x83 = % x, want % x", got, want83)
	}

	// EVPD page not supported.
	respUnsupported := d.Handle(entryWithCDB([]byte{OpInquiry, 0x01, 0x89, 0, byte(len(vpdBuf)), 0}, vpdBuf))
	if respUnsupported.Status != StatusCheckCondition {
		t.Fatalf("EVPD unsupported page status = %#x, want CHECK CONDITION", respUnsupported.Status)
	}
}

func TestDriveRewindResetsPosition(t *testing.T) {
	st, _ := driveStatusWithFile(t, []byte("0123456789"), 0)
	d := &Drive{Client: &fakeClient{st: st}}
	d.position = 5
	resp := d.Handle(entryWithCDB([]byte{OpRewind, 0, 0, 0, 0, 0}))
	if resp.Status != StatusGood || d.position != 0 {
		t.Fatalf("resp = %+v, position = %d", resp, d.position)
	}
}

func TestDriveLoadUnload(t *testing.T) {
	st, _ := driveStatusWithFile(t, nil, 0)
	d := &Drive{Client: &fakeClient{st: st}}
	d.position = 42

	loadCDB := []byte{OpLoadUnload, 0, 0, 0, 0x01, 0} // Load bit set
	if resp := d.Handle(entryWithCDB(loadCDB)); resp.Status != StatusGood || d.position != 0 {
		t.Fatalf("load resp = %+v, position = %d", resp, d.position)
	}

	unloadCDB := []byte{OpLoadUnload, 0, 0, 0, 0x00, 0} // Load bit clear
	if resp := d.Handle(entryWithCDB(unloadCDB)); resp.Status != StatusGood {
		t.Fatalf("unload resp = %+v", resp)
	}

	emptyDrive := &Drive{Client: &fakeClient{st: library.Status{Drives: []*library.Drive{{Index: 0}}}}}
	if resp := emptyDrive.Handle(entryWithCDB(loadCDB)); resp.Status != StatusCheckCondition || resp.Sense[12] != AscMediumNotPresent {
		t.Fatalf("load-with-no-medium resp = %+v", resp)
	}
}

func writeFilemarksCDB(count int) []byte {
	cdb := make([]byte, 6)
	cdb[0] = OpWriteFilemarks
	cdb[2] = byte(count >> 16)
	cdb[3] = byte(count >> 8)
	cdb[4] = byte(count)
	return cdb
}

// TestDriveWriteFilemarks reproduces the real bug: this opcode had no
// handler at all before this fix, so a real Bareos job's own MTWEOF at
// end-of-job (and the second one immediately after, for logical
// end-of-tape) both failed outright. Confirms the command now succeeds
// and records the filemark's position via the sidecar file mechanism.
func TestDriveWriteFilemarks(t *testing.T) {
	st, path := driveStatusWithFile(t, []byte("0123456789"), 0)
	d := &Drive{Client: &fakeClient{st: st}}
	d.position = 10

	resp := d.Handle(entryWithCDB(writeFilemarksCDB(1)))
	if resp.Status != StatusGood {
		t.Fatalf("resp = %+v, want GOOD", resp)
	}
	if d.position != 10 {
		t.Errorf("position = %d, want unchanged at 10 (a filemark doesn't consume byte-stream space)", d.position)
	}
	got, err := readFilemarks(path)
	if err != nil {
		t.Fatalf("readFilemarks: %v", err)
	}
	if want := []int64{10}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("recorded filemarks = %v, want %v", got, want)
	}

	// A second WRITE FILEMARKS(6) at the same position (matching real
	// backup software writing two consecutive EOF marks at the true end
	// of a volume) must also succeed.
	resp2 := d.Handle(entryWithCDB(writeFilemarksCDB(1)))
	if resp2.Status != StatusGood {
		t.Fatalf("second resp = %+v, want GOOD", resp2)
	}
}

func TestDriveWriteFilemarksNoMedium(t *testing.T) {
	d := &Drive{Client: &fakeClient{st: library.Status{Drives: []*library.Drive{{Index: 0}}}}}
	resp := d.Handle(entryWithCDB(writeFilemarksCDB(1)))
	if resp.Status != StatusCheckCondition || resp.Sense[12] != AscMediumNotPresent {
		t.Fatalf("resp = %+v, want CHECK CONDITION/MEDIUM NOT PRESENT", resp)
	}
}

func TestDriveWriteFilemarksWriteProtected(t *testing.T) {
	st, _ := driveStatusWithFile(t, nil, 0)
	st.Drives[0].Volume.WriteProtected = true
	d := &Drive{Client: &fakeClient{st: st}}
	resp := d.Handle(entryWithCDB(writeFilemarksCDB(1)))
	if resp.Status != StatusCheckCondition || resp.Sense[2]&0x0F != SenseDataProtect {
		t.Fatalf("resp = %+v, want CHECK CONDITION/DATA PROTECT", resp)
	}
}

// TestDriveWrite6InvalidatesStaleFilemarks reproduces re-labeling/
// reusing a volume that already has recorded filemarks from a previous
// session: rewinding and writing new data must drop any now-stale
// filemark past the new write position, or a later SPACE/read against
// this volume could be misled by leftover structure from the old data.
func TestDriveWrite6InvalidatesStaleFilemarks(t *testing.T) {
	st, path := driveStatusWithFile(t, make([]byte, 20), 0)
	d := &Drive{Client: &fakeClient{st: st}}
	d.position = 10
	if err := recordFilemark(path, 10); err != nil {
		t.Fatalf("recordFilemark: %v", err)
	}

	d.position = 0
	resp := d.Handle(entryWithCDB(writeCDB(5), []byte("HELLO")))
	if resp.Status != StatusGood {
		t.Fatalf("write resp = %+v", resp)
	}
	got, err := readFilemarks(path)
	if err != nil {
		t.Fatalf("readFilemarks: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("filemarks after overwrite = %v, want empty (the old filemark at 10 should be invalidated)", got)
	}
}

// TestDriveWrite6TruncatesStaleDataBeyondWrite is the byte-stream
// counterpart of TestDriveWrite6InvalidatesStaleFilemarks above: real tape
// puts end-of-data immediately after whatever was just written, so
// rewinding and overwriting must leave none of the previous pass's data
// logically on the volume. See truncateToEOD for the real-deployment bug
// this fixes.
func TestDriveWrite6TruncatesStaleDataBeyondWrite(t *testing.T) {
	st, path := driveStatusWithFile(t, bytes.Repeat([]byte("OLD"), 1000), 0) // 3000 bytes of a previous pass
	d := &Drive{Client: &fakeClient{st: st}}

	if resp := d.Handle(entryWithCDB(writeCDB(5), []byte("FRESH"))); resp.Status != StatusGood {
		t.Fatalf("write resp = %+v", resp)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read backing file: %v", err)
	}
	if string(got) != "FRESH" {
		t.Errorf("backing file = %q (%d bytes), want %q - stale data beyond the write is still on the volume", got, len(got), "FRESH")
	}
}

func TestDriveWrite6SequentialAppendKeepsEarlierData(t *testing.T) {
	st, path := driveStatusWithFile(t, nil, 0)
	d := &Drive{Client: &fakeClient{st: st}}

	for _, s := range []string{"FIRST", "SECOND"} {
		if resp := d.Handle(entryWithCDB(writeCDB(len(s)), []byte(s))); resp.Status != StatusGood {
			t.Fatalf("write(%q) resp = %+v", s, resp)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read backing file: %v", err)
	}
	if string(got) != "FIRSTSECOND" {
		t.Errorf("backing file = %q, want %q - truncating to end-of-data must be a no-op for a plain append", got, "FIRSTSECOND")
	}
}

// TestDriveWriteFilemarksAtBOTErasesVolume covers the standard `mt rewind;
// mt weof` erase idiom an operator reached for on the real deployment: a
// filemark written at BOT puts end-of-data there, so the volume really is
// empty afterwards. Before this fix the sidecar was rewritten and the data
// was left entirely untouched, so the volume still read as fully recorded
// (and still carried its old Bareos label, which is what made a second
// `label barcodes` fail with "already labeled").
func TestDriveWriteFilemarksAtBOTErasesVolume(t *testing.T) {
	st, path := driveStatusWithFile(t, bytes.Repeat([]byte("x"), 4096), 0)
	d := &Drive{Client: &fakeClient{st: st}}
	if err := recordFilemark(path, 4096); err != nil {
		t.Fatalf("recordFilemark: %v", err)
	}

	d.position = 0 // rewind
	if resp := d.Handle(entryWithCDB(writeFilemarksCDB(1))); resp.Status != StatusGood {
		t.Fatalf("resp = %+v, want GOOD", resp)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat backing file: %v", err)
	}
	if fi.Size() != 0 {
		t.Errorf("backing file = %d bytes, want 0 (a filemark at BOT sets end-of-data there)", fi.Size())
	}
	marks, err := readFilemarks(path)
	if err != nil {
		t.Fatalf("readFilemarks: %v", err)
	}
	if len(marks) != 1 || marks[0] != 0 {
		t.Errorf("filemarks = %v, want [0] (the stale mark at 4096 is past the new end-of-data)", marks)
	}
}

// TestDriveWriteFilemarksZeroCountKeepsEndOfData pins the count > 0 guard
// in writeFilemarks: WRITE FILEMARKS with a zero transfer length writes
// nothing to the medium (it's the conventional buffer flush), so it must
// not move end-of-data. Without the guard, a flush issued anywhere but
// end-of-data would silently discard everything after it.
func TestDriveWriteFilemarksZeroCountKeepsEndOfData(t *testing.T) {
	st, path := driveStatusWithFile(t, []byte("0123456789"), 0)
	d := &Drive{Client: &fakeClient{st: st}}
	d.position = 4

	if resp := d.Handle(entryWithCDB(writeFilemarksCDB(0))); resp.Status != StatusGood {
		t.Fatalf("resp = %+v, want GOOD", resp)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read backing file: %v", err)
	}
	if string(got) != "0123456789" {
		t.Errorf("backing file = %q, want it untouched by a zero-count flush", got)
	}
}

// TestDriveWriteFilemarksNeverExtendsVolume covers truncateToEOD's
// shrink-only rule: LOCATE accepts any absolute block address, so
// d.position can sit past end-of-data, and meeting it would invent
// zero-filled recorded data that was never written.
func TestDriveWriteFilemarksNeverExtendsVolume(t *testing.T) {
	st, path := driveStatusWithFile(t, []byte("0123456789"), 0)
	d := &Drive{Client: &fakeClient{st: st}}

	if resp := d.Handle(entryWithCDB(locateCDB(1 << 20))); resp.Status != StatusGood {
		t.Fatalf("locate resp = %+v", resp)
	}
	if resp := d.Handle(entryWithCDB(writeFilemarksCDB(1))); resp.Status != StatusGood {
		t.Fatalf("writeFilemarks resp = %+v", resp)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat backing file: %v", err)
	}
	if fi.Size() != 10 {
		t.Errorf("backing file = %d bytes, want 10 (truncateToEOD must never extend)", fi.Size())
	}
}

// TestDriveRelabelOverRecordedVolume reproduces the real-deployment
// failure end-to-end through Handle(), in Bareos's own order: a volume
// carrying a previous job's data and filemarks is rewound, a fresh label
// block is written at BOT, and a filemark closes it. The volume must end
// up byte-for-byte what a freshly labeled cartridge looks like - the label
// and nothing else - rather than the label followed by the whole previous
// job, which is the state a real `label barcodes` actually left behind on
// bareos-disk-sd-int-fr1 (see truncateToEOD).
func TestDriveRelabelOverRecordedVolume(t *testing.T) {
	const labelLen = 512
	oldJob := bytes.Repeat([]byte("J"), 8192)
	st, path := driveStatusWithFile(t, oldJob, 0)
	d := &Drive{Client: &fakeClient{st: st}}
	for _, pos := range []int64{512, 8192} { // the old job's own label and trailing filemarks
		if err := recordFilemark(path, pos); err != nil {
			t.Fatalf("recordFilemark(%d): %v", pos, err)
		}
	}

	if resp := d.Handle(entryWithCDB([]byte{OpRewind, 0, 0, 0, 0, 0})); resp.Status != StatusGood {
		t.Fatalf("rewind resp = %+v", resp)
	}
	label := bytes.Repeat([]byte("L"), labelLen)
	if resp := d.Handle(entryWithCDB(writeCDB(labelLen), label)); resp.Status != StatusGood {
		t.Fatalf("label write resp = %+v", resp)
	}
	if resp := d.Handle(entryWithCDB(writeFilemarksCDB(1))); resp.Status != StatusGood {
		t.Fatalf("weof resp = %+v", resp)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read backing file: %v", err)
	}
	if !bytes.Equal(got, label) {
		t.Errorf("backing file = %d bytes, want exactly the %d-byte label", len(got), labelLen)
	}
	marks, err := readFilemarks(path)
	if err != nil {
		t.Fatalf("readFilemarks: %v", err)
	}
	if len(marks) != 1 || marks[0] != labelLen {
		t.Errorf("filemarks = %v, want [%d]", marks, labelLen)
	}

	// End-of-data must now be the end of the label, so Bareos's own
	// space-to-end-of-data before an append lands there and not deep inside
	// the previous job's data.
	if resp := d.Handle(entryWithCDB(spaceCDB(3, 0))); resp.Status != StatusGood || d.position != labelLen {
		t.Errorf("space to end-of-data: resp = %+v, position = %d, want GOOD at %d", resp, d.position, labelLen)
	}
}

// TestDriveSequentialMultiFileWriteThenRead reproduces, end-to-end
// through Handle(), the exact real-world sequence that broke against a
// real kernel (2026-07-26): write file1, write its filemark, write file2
// immediately after, write its own filemark too - then rewind and read
// both files back, confirming each read stops at the right boundary and
// both filemarks survived (file2's own write must not have erased
// file1's trailing mark just because it started writing at that exact
// position).
func TestDriveSequentialMultiFileWriteThenRead(t *testing.T) {
	st, _ := driveStatusWithFile(t, make([]byte, 0), 0)
	d := &Drive{Client: &fakeClient{st: st}}

	write := func(s string) {
		t.Helper()
		if resp := d.Handle(entryWithCDB(writeCDB(len(s)), []byte(s))); resp.Status != StatusGood {
			t.Fatalf("write(%q) resp = %+v", s, resp)
		}
	}
	weof := func() {
		t.Helper()
		if resp := d.Handle(entryWithCDB(writeFilemarksCDB(1))); resp.Status != StatusGood {
			t.Fatalf("writeFilemarks resp = %+v", resp)
		}
	}

	write("FILEONE")
	weof()
	write("FILETWO")
	weof()

	d.position = 0
	buf1 := make([]byte, 7)
	resp1 := d.Handle(entryWithCDB(readCDB(false, 7), buf1))
	if resp1.Status != StatusCheckCondition || resp1.Sense[2]&0x80 == 0 {
		t.Fatalf("read file1 resp = %+v, want CHECK CONDITION/FILEMARK", resp1)
	}
	if string(buf1) != "FILEONE" {
		t.Fatalf("file1 = %q, want %q", buf1, "FILEONE")
	}

	buf2 := make([]byte, 7)
	resp2 := d.Handle(entryWithCDB(readCDB(false, 7), buf2))
	if resp2.Status != StatusCheckCondition || resp2.Sense[2]&0x80 == 0 {
		t.Fatalf("read file2 resp = %+v, want CHECK CONDITION/FILEMARK", resp2)
	}
	if string(buf2) != "FILETWO" {
		t.Fatalf("file2 = %q, want %q", buf2, "FILETWO")
	}
}

// TestDriveRead6StopsAtFilemark reproduces what a real restore/copy job
// needs: reading a file's data must stop exactly at its trailing
// filemark, not read straight through into whatever job was written
// after it on the same volume.
func TestDriveRead6StopsAtFilemark(t *testing.T) {
	content := []byte("HELLOWORLDMORE") // "HELLOWORLD" (10 bytes) then more data
	st, path := driveStatusWithFile(t, content, 0)
	d := &Drive{Client: &fakeClient{st: st}}
	if err := recordFilemark(path, 10); err != nil {
		t.Fatalf("recordFilemark: %v", err)
	}

	buf := make([]byte, 20)
	resp := d.Handle(entryWithCDB(readCDB(false, 20), buf))
	if resp.Status != StatusCheckCondition || resp.Sense[2]&0x80 == 0 {
		t.Fatalf("resp = %+v, want CHECK CONDITION with FILEMARK set", resp)
	}
	if resp.ReadLen != 10 || string(buf[:10]) != "HELLOWORLD" {
		t.Fatalf("read %d bytes (%q), want 10 (\"HELLOWORLD\")", resp.ReadLen, buf[:resp.ReadLen])
	}
	if d.position != 10 {
		t.Errorf("position = %d, want 10 (positioned right after the filemark)", d.position)
	}

	// The next read, starting exactly at the filemark's position, reads
	// the following file's data normally - the boundary isn't "hit"
	// twice.
	buf2 := make([]byte, 4)
	resp2 := d.Handle(entryWithCDB(readCDB(false, 4), buf2))
	if resp2.Status != StatusGood {
		t.Fatalf("resp2 = %+v, want GOOD", resp2)
	}
	if string(buf2) != "MORE" {
		t.Errorf("read %q, want %q", buf2, "MORE")
	}
}

func TestDriveRead6FullRead(t *testing.T) {
	content := []byte("HELLO WORLD")
	st, _ := driveStatusWithFile(t, content, 0)
	d := &Drive{Client: &fakeClient{st: st}}

	buf := make([]byte, len(content))
	resp := d.Handle(entryWithCDB(readCDB(false, len(content)), buf))
	if resp.Status != StatusGood || resp.ReadLen != uint32(len(content)) {
		t.Fatalf("resp = %+v", resp)
	}
	if string(buf) != string(content) {
		t.Errorf("read %q, want %q", buf, content)
	}
	if d.position != int64(len(content)) {
		t.Errorf("position = %d, want %d", d.position, len(content))
	}
}

func TestDriveRead6ShortReadReportsILIUnlessSILI(t *testing.T) {
	content := []byte("SHORT")
	st, _ := driveStatusWithFile(t, content, 0)
	d := &Drive{Client: &fakeClient{st: st}}

	buf := make([]byte, 100)
	resp := d.Handle(entryWithCDB(readCDB(false, 100), buf))
	if resp.Status != StatusCheckCondition || resp.ReadLen != uint32(len(content)) {
		t.Fatalf("resp = %+v", resp)
	}
	if ili := resp.Sense[2]&0x20 != 0; !ili {
		t.Errorf("ILI bit not set in sense: %v", resp.Sense)
	}

	d2 := &Drive{Client: &fakeClient{st: st}}
	buf2 := make([]byte, 100)
	resp2 := d2.Handle(entryWithCDB(readCDB(true, 100), buf2))
	if resp2.Status != StatusGood || resp2.ReadLen != uint32(len(content)) {
		t.Fatalf("SILI resp = %+v", resp2)
	}
}

func TestDriveRead6PastEndOfDataIsBlankCheck(t *testing.T) {
	st, _ := driveStatusWithFile(t, []byte("DATA"), 0)
	d := &Drive{Client: &fakeClient{st: st}}
	d.position = 4 // already at end of the 4-byte file

	buf := make([]byte, 10)
	resp := d.Handle(entryWithCDB(readCDB(false, 10), buf))
	if resp.Status != StatusCheckCondition || resp.Sense[2]&0x0F != SenseBlankCheck {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestDriveWrite6Basic(t *testing.T) {
	st, path := driveStatusWithFile(t, nil, 0)
	d := &Drive{Client: &fakeClient{st: st}}
	payload := []byte("NEW DATA")

	resp := d.Handle(entryWithCDB(writeCDB(len(payload)), payload))
	if resp.Status != StatusGood {
		t.Fatalf("resp = %+v", resp)
	}
	if d.position != int64(len(payload)) {
		t.Errorf("position = %d, want %d", d.position, len(payload))
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read backing file: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("backing file = %q, want %q", got, payload)
	}
}

func TestDriveWrite6RejectsPastCapacity(t *testing.T) {
	st, _ := driveStatusWithFile(t, []byte("0123456789"), 12) // 10 bytes written, capacity 12
	d := &Drive{Client: &fakeClient{st: st}}
	d.position = 10

	payload := []byte("ABCD") // would land at byte 14, past the 12-byte capacity
	resp := d.Handle(entryWithCDB(writeCDB(len(payload)), payload))
	if resp.Status != StatusCheckCondition || resp.Sense[2]&0x0F != SenseVolumeOverflow {
		t.Fatalf("resp = %+v", resp)
	}
	if eom := resp.Sense[2]&0x40 != 0; !eom {
		t.Errorf("EOM bit not set in sense: %v", resp.Sense)
	}
}

func TestDriveWrite6RejectsWriteProtected(t *testing.T) {
	st, path := driveStatusWithFile(t, []byte("0123456789"), 0)
	st.Drives[0].Volume.WriteProtected = true
	d := &Drive{Client: &fakeClient{st: st}}

	payload := []byte("ABCD")
	resp := d.Handle(entryWithCDB(writeCDB(len(payload)), payload))
	if resp.Status != StatusCheckCondition || resp.Sense[2]&0x0F != SenseDataProtect {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.Sense[12] != AscWriteProtected || resp.Sense[13] != AscqWriteProtected {
		t.Fatalf("asc/ascq = %#x/%#x, want %#x/%#x", resp.Sense[12], resp.Sense[13], AscWriteProtected, AscqWriteProtected)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read backing file: %v", err)
	}
	if string(got) != "0123456789" {
		t.Errorf("backing file was modified despite write-protect: %q", got)
	}
}

func TestDriveRead6IgnoresWriteProtected(t *testing.T) {
	content := []byte("HELLO")
	st, _ := driveStatusWithFile(t, content, 0)
	st.Drives[0].Volume.WriteProtected = true
	d := &Drive{Client: &fakeClient{st: st}}

	buf := make([]byte, len(content))
	resp := d.Handle(entryWithCDB(readCDB(false, len(content)), buf))
	if resp.Status != StatusGood {
		t.Fatalf("resp = %+v", resp)
	}
	if string(buf) != string(content) {
		t.Errorf("read %q, want %q", buf, content)
	}
}

func spaceCDB(code uint8, count int32) []byte {
	cdb := make([]byte, 6)
	cdb[0] = OpSpace6
	cdb[1] = code & 0x07
	raw := uint32(count) & 0xFFFFFF
	cdb[2] = byte(raw >> 16)
	cdb[3] = byte(raw >> 8)
	cdb[4] = byte(raw)
	return cdb
}

func TestDriveSpace6Blocks(t *testing.T) {
	st, _ := driveStatusWithFile(t, []byte("0123456789"), 0)
	d := &Drive{Client: &fakeClient{st: st}}
	d.position = 2

	if resp := d.Handle(entryWithCDB(spaceCDB(0, 3))); resp.Status != StatusGood || d.position != 5 {
		t.Fatalf("forward space resp = %+v, position = %d", resp, d.position)
	}
	if resp := d.Handle(entryWithCDB(spaceCDB(0, -4))); resp.Status != StatusGood || d.position != 1 {
		t.Fatalf("backward space resp = %+v, position = %d", resp, d.position)
	}
	// Backward past BOT clamps to 0, not an error.
	if resp := d.Handle(entryWithCDB(spaceCDB(0, -100))); resp.Status != StatusGood || d.position != 0 {
		t.Fatalf("space past BOT resp = %+v, position = %d", resp, d.position)
	}
	// Forward past the end of recorded data is BLANK CHECK, and position
	// lands at EOD (10), not past it.
	if resp := d.Handle(entryWithCDB(spaceCDB(0, 100))); resp.Status != StatusCheckCondition || resp.Sense[2]&0x0F != SenseBlankCheck || d.position != 10 {
		t.Fatalf("space past EOD resp = %+v, position = %d", resp, d.position)
	}
}

func TestDriveSpace6EndOfData(t *testing.T) {
	st, _ := driveStatusWithFile(t, []byte("0123456789"), 0)
	d := &Drive{Client: &fakeClient{st: st}}
	if resp := d.Handle(entryWithCDB(spaceCDB(3, 0))); resp.Status != StatusGood || d.position != 10 {
		t.Fatalf("resp = %+v, position = %d", resp, d.position)
	}
}

func TestDriveSpace6RejectsUnsupportedCode(t *testing.T) {
	st, _ := driveStatusWithFile(t, []byte("0123456789"), 0)
	d := &Drive{Client: &fakeClient{st: st}}
	if resp := d.Handle(entryWithCDB(spaceCDB(2, 1))); resp.Status != StatusCheckCondition { // sequential filemarks, not implemented
		t.Fatalf("resp = %+v, want CHECK CONDITION", resp)
	}
}

// TestDriveSpace6Filemarks reproduces the real scenario this was built
// for: a real Bareos job (2026-07-26) doing "space to end of data" via
// SPACE(6) code=1 with an enormous count, and a copy job forward-spacing
// to a specific file - both go through this same code path.
func TestDriveSpace6Filemarks(t *testing.T) {
	st, path := driveStatusWithFile(t, make([]byte, 30), 0)
	d := &Drive{Client: &fakeClient{st: st}}
	for _, pos := range []int64{10, 20} {
		if err := recordFilemark(path, pos); err != nil {
			t.Fatalf("recordFilemark(%d): %v", pos, err)
		}
	}

	// Forward one file from BOT lands exactly at the first filemark.
	if resp := d.Handle(entryWithCDB(spaceCDB(1, 1))); resp.Status != StatusGood || d.position != 10 {
		t.Fatalf("fsf 1 resp = %+v, position = %d", resp, d.position)
	}
	// Forward one more lands at the second.
	if resp := d.Handle(entryWithCDB(spaceCDB(1, 1))); resp.Status != StatusGood || d.position != 20 {
		t.Fatalf("fsf 1 (again) resp = %+v, position = %d", resp, d.position)
	}
	// Reverse one lands back at the first.
	if resp := d.Handle(entryWithCDB(spaceCDB(1, -1))); resp.Status != StatusGood || d.position != 10 {
		t.Fatalf("bsf 1 resp = %+v, position = %d", resp, d.position)
	}
	// Reverse past BOT clamps to 0, no error - mirrors code=0's own
	// existing backward-underrun convention.
	if resp := d.Handle(entryWithCDB(spaceCDB(1, -100))); resp.Status != StatusGood || d.position != 0 {
		t.Fatalf("bsf past BOT resp = %+v, position = %d", resp, d.position)
	}
	// Forward with an enormous count (matching Bareos's own real "space
	// to end of data" idiom) runs out of real filemarks and lands at true
	// end-of-data (30), reporting BLANK CHECK with the spec's own
	// residual-count convention: requested (8388607) minus actual (2).
	resp := d.Handle(entryWithCDB(spaceCDB(1, 8388607)))
	if resp.Status != StatusCheckCondition || resp.Sense[2]&0x0F != SenseBlankCheck || d.position != 30 {
		t.Fatalf("huge fsf resp = %+v, position = %d", resp, d.position)
	}
	if resp.Sense[0]&0x80 == 0 {
		t.Fatalf("VALID bit not set: sense = %v", resp.Sense)
	}
	wantResidual := uint32(8388607 - 2)
	gotResidual := uint32(resp.Sense[3])<<24 | uint32(resp.Sense[4])<<16 | uint32(resp.Sense[5])<<8 | uint32(resp.Sense[6])
	if gotResidual != wantResidual {
		t.Errorf("residual = %d, want %d", gotResidual, wantResidual)
	}

	// A zero count is a documented no-op, not an error.
	d.position = 5
	if resp := d.Handle(entryWithCDB(spaceCDB(1, 0))); resp.Status != StatusGood || d.position != 5 {
		t.Fatalf("fsf 0 resp = %+v, position = %d", resp, d.position)
	}
}

func locateCDB(blockAddr uint32) []byte {
	cdb := make([]byte, 10)
	cdb[0] = OpLocate
	cdb[3] = byte(blockAddr >> 24)
	cdb[4] = byte(blockAddr >> 16)
	cdb[5] = byte(blockAddr >> 8)
	cdb[6] = byte(blockAddr)
	return cdb
}

func TestDriveLocate(t *testing.T) {
	st, _ := driveStatusWithFile(t, []byte("0123456789"), 0)
	d := &Drive{Client: &fakeClient{st: st}}
	d.position = 9
	if resp := d.Handle(entryWithCDB(locateCDB(3))); resp.Status != StatusGood || d.position != 3 {
		t.Fatalf("resp = %+v, position = %d", resp, d.position)
	}
}

func TestDriveLocateRejectsChangePartition(t *testing.T) {
	st, _ := driveStatusWithFile(t, []byte("0123456789"), 0)
	d := &Drive{Client: &fakeClient{st: st}}
	cdb := locateCDB(0)
	cdb[1] |= 0x02 // CP
	if resp := d.Handle(entryWithCDB(cdb)); resp.Status != StatusCheckCondition {
		t.Fatalf("resp = %+v, want CHECK CONDITION", resp)
	}
}

func TestDriveModeSense6(t *testing.T) {
	st, _ := driveStatusWithFile(t, nil, 0)
	d := &Drive{Client: &fakeClient{st: st}}
	buf := make([]byte, 64)

	cdb := []byte{OpModeSense6, 0, 0x00, 0, 64, 0} // DBD=0, page code 0
	resp := d.Handle(entryWithCDB(cdb, buf))
	if resp.Status != StatusGood {
		t.Fatalf("resp = %+v", resp)
	}
	data := buf[:resp.ReadLen]
	if len(data) != 4+modeBlockDescriptorLen {
		t.Fatalf("response length = %d, want %d", len(data), 4+modeBlockDescriptorLen)
	}
	if int(data[0]) != len(data)-1 {
		t.Errorf("mode data length = %d, want %d", data[0], len(data)-1)
	}
	if data[3] != modeBlockDescriptorLen {
		t.Errorf("block descriptor length = %d, want %d", data[3], modeBlockDescriptorLen)
	}
	blockLen := uint32(data[4+5])<<16 | uint32(data[4+6])<<8 | uint32(data[4+7])
	if blockLen != 1 {
		t.Errorf("block length = %d, want 1", blockLen)
	}

	// DBD=1 - no block descriptor.
	cdbDBD := []byte{OpModeSense6, 0x08, 0x00, 0, 64, 0}
	resp2 := d.Handle(entryWithCDB(cdbDBD, buf))
	if resp2.Status != StatusGood || resp2.ReadLen != 4 {
		t.Fatalf("DBD resp = %+v", resp2)
	}

	// Unsupported page code.
	cdbBadPage := []byte{OpModeSense6, 0, 0x02, 0, 64, 0}
	if resp3 := d.Handle(entryWithCDB(cdbBadPage, buf)); resp3.Status != StatusCheckCondition {
		t.Fatalf("resp = %+v, want CHECK CONDITION", resp3)
	}
}

func TestDriveModeSelect6(t *testing.T) {
	st, _ := driveStatusWithFile(t, nil, 0)
	d := &Drive{Client: &fakeClient{st: st}}
	cdb := []byte{OpModeSelect6, 0x10, 0, 0, 12, 0}
	if resp := d.Handle(entryWithCDB(cdb)); resp.Status != StatusGood {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestDriveThrottleDuration(t *testing.T) {
	d := &Drive{Family: DriveFamily{NativeSpeed: 1000}} // 1000 bytes/sec
	if got, want := d.throttleDuration(1000), time.Second; got != want {
		t.Errorf("throttleDuration(1000) = %v, want %v", got, want)
	}
	if got, want := d.throttleDuration(500), 500*time.Millisecond; got != want {
		t.Errorf("throttleDuration(500) = %v, want %v", got, want)
	}
	if got := d.throttleDuration(0); got != 0 {
		t.Errorf("throttleDuration(0) = %v, want 0", got)
	}

	unthrottled := &Drive{} // zero-value Family: NativeSpeed unset
	if got := unthrottled.throttleDuration(1_000_000); got != 0 {
		t.Errorf("throttleDuration with no native speed = %v, want 0", got)
	}
}

func TestDriveReadWriteActuallySleepForThrottledFamily(t *testing.T) {
	// A deliberately tiny native speed so the resulting sleep (tens of
	// milliseconds) is easy to assert on without a flaky, longer-running
	// test - still a real time.Sleep, not a mocked one.
	const nativeSpeed = 2000 // bytes/sec
	payload := make([]byte, 50)
	minExpected := 20 * time.Millisecond // 50 bytes @ 2000B/s = 25ms

	st, _ := driveStatusWithFile(t, nil, int64(len(payload)))
	d := &Drive{Client: &fakeClient{st: st}, Family: DriveFamily{NativeSpeed: nativeSpeed}}
	start := time.Now()
	if resp := d.Handle(entryWithCDB(writeCDB(len(payload)), payload)); resp.Status != StatusGood {
		t.Fatalf("write resp = %+v", resp)
	}
	if elapsed := time.Since(start); elapsed < minExpected {
		t.Errorf("write6 took %v, want at least %v (throttled)", elapsed, minExpected)
	}

	d2 := &Drive{Client: &fakeClient{st: st}, Family: DriveFamily{NativeSpeed: nativeSpeed}}
	buf := make([]byte, len(payload))
	start2 := time.Now()
	if resp := d2.Handle(entryWithCDB(readCDB(false, len(payload)), buf)); resp.Status != StatusGood {
		t.Fatalf("read resp = %+v", resp)
	}
	if elapsed := time.Since(start2); elapsed < minExpected {
		t.Errorf("read6 took %v, want at least %v (throttled)", elapsed, minExpected)
	}
}
