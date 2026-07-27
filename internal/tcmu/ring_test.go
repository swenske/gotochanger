package tcmu

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// --- test-only ring/entry construction helpers, standing in for what a
// real kernel would lay out in a TCMU device's shared memory. ---

// newFakeMem builds a zeroed region of totalSize bytes with a valid
// mailbox at offset 0 describing a command ring at [cmdrOff, cmdrOff+cmdrSize).
func newFakeMem(t *testing.T, totalSize int, cmdrOff, cmdrSize uint32) []byte {
	t.Helper()
	mem := make([]byte, totalSize)
	encodeMailboxHeader(mem, Mailbox{Version: MailboxVersion, CmdrOff: cmdrOff, CmdrSize: cmdrSize})
	return mem
}

func setCmdHead(mem []byte, head uint32) {
	binary.LittleEndian.PutUint32(mem[12:16], head)
}

// entryLen computes the on-ring length a real kernel would allocate for a
// command with iovCount iovecs - large enough for both the request fields
// and (since the two overlay the same bytes) the eventual response,
// rounded up to opAlignSize. Mirrors the sizing constraint documented on
// Cursor.Complete.
func entryLen(iovCount int) uint32 {
	need := cmdEntryHdrSize + reqHeaderSize + iovCount*iovecSize
	if cmdEntryHdrSize+rspSize > need {
		need = cmdEntryHdrSize + rspSize
	}
	return uint32((need + opAlignSize - 1) &^ (opAlignSize - 1))
}

// writePad writes a PAD entry of the given length at ring offset pos.
func writePad(mem []byte, cmdrOff, pos, length uint32) {
	abs := cmdrOff + pos
	lenOp := length | uint32(OpPad)
	binary.LittleEndian.PutUint32(mem[abs:abs+4], lenOp)
}

// writeCmd writes a real OpCmd entry of cmdID/cdb/buffers at ring offset
// pos, placing cdb and each buffer's initial content into the data area at
// *dataCursor (advanced as space is consumed). Returns the entry's length
// and the absolute mem offset each buffer was placed at (so tests can
// independently verify zero-copy aliasing without reverse-engineering the
// on-ring layout a second time).
func writeCmd(mem []byte, cmdrOff, pos uint32, cmdID uint16, cdb []byte, buffers [][]byte, dataCursor *uint32) (length uint32, bufOffsets []uint32) {
	length = entryLen(len(buffers))
	abs := uint64(cmdrOff) + uint64(pos)

	lenOp := length | uint32(OpCmd)
	binary.LittleEndian.PutUint32(mem[abs:abs+4], lenOp)
	binary.LittleEndian.PutUint16(mem[abs+4:abs+6], cmdID)
	mem[abs+6] = 0
	mem[abs+7] = 0

	cdbOff := *dataCursor
	copy(mem[cdbOff:], cdb)
	*dataCursor += uint32(len(cdb))

	reqStart := abs + cmdEntryHdrSize
	binary.LittleEndian.PutUint32(mem[reqStart:reqStart+4], uint32(len(buffers)))
	binary.LittleEndian.PutUint32(mem[reqStart+4:reqStart+8], 0)
	binary.LittleEndian.PutUint32(mem[reqStart+8:reqStart+12], 0)
	binary.LittleEndian.PutUint64(mem[reqStart+reqCDBOffOffset:reqStart+reqCDBOffOffset+8], uint64(cdbOff))

	iovStart := reqStart + reqHeaderSize
	bufOffsets = make([]uint32, len(buffers))
	for i, buf := range buffers {
		off := *dataCursor
		copy(mem[off:], buf)
		*dataCursor += uint32(len(buf))
		bufOffsets[i] = off

		e := iovStart + uint64(i)*iovecSize
		binary.LittleEndian.PutUint64(mem[e:e+8], uint64(off))
		binary.LittleEndian.PutUint64(mem[e+8:e+16], uint64(len(buf)))
	}
	return length, bufOffsets
}

func decodeRspAt(mem []byte, cmdrOff, pos uint32) (status uint8, readLen uint32, sense []byte) {
	abs := uint64(cmdrOff) + uint64(pos) + cmdEntryHdrSize
	status = mem[abs]
	readLen = binary.LittleEndian.Uint32(mem[abs+4 : abs+8])
	sense = mem[abs+8 : abs+8+SenseBufferSize]
	return
}

// decodeUFlagsAt reads back entryHdr.uflags (byte 7 of the entry's own
// header, not part of rsp - see Cursor.Complete's own doc comment) for a
// command at ring offset pos.
func decodeUFlagsAt(mem []byte, cmdrOff, pos uint32) uint8 {
	abs := uint64(cmdrOff) + uint64(pos)
	return mem[abs+7]
}

func TestRingSingleCommandRoundTrip(t *testing.T) {
	const cmdrOff, cmdrSize uint32 = 256, 1024
	mem := newFakeMem(t, 4096, cmdrOff, cmdrSize)
	dataCursor := cmdrOff + cmdrSize

	cdb := []byte{0x08, 0, 0, 0, 16, 0} // READ(6)-shaped, content irrelevant to this layer
	payload := []byte("HELLO, TCMU!!!!!")
	length, bufOffsets := writeCmd(mem, cmdrOff, 0, 7, cdb, [][]byte{payload}, &dataCursor)
	setCmdHead(mem, length)

	ring, err := NewRing(mem)
	if err != nil {
		t.Fatalf("NewRing: %v", err)
	}
	cur := ring.NewCursor()

	entry, ok, err := cur.Next()
	if err != nil || !ok {
		t.Fatalf("Next: ok=%v err=%v", ok, err)
	}
	if entry.CmdID != 7 {
		t.Errorf("CmdID = %d, want 7", entry.CmdID)
	}
	if !bytes.Equal(entry.CDB[:len(cdb)], cdb) {
		t.Errorf("CDB = %v, want prefix %v", entry.CDB, cdb)
	}
	if len(entry.Buffers) != 1 || !bytes.Equal(entry.Buffers[0], payload) {
		t.Fatalf("Buffers = %v, want [%v]", entry.Buffers, payload)
	}

	// Buffers must be a zero-copy view into mem, not a defensive copy -
	// this is what lets a handler fill a READ response directly.
	entry.Buffers[0][0] = 'X'
	if mem[bufOffsets[0]] != 'X' {
		t.Fatalf("writing through entry.Buffers[0] did not mutate the underlying mem")
	}

	if err := cur.Complete(entry, Response{Status: 0, ReadLen: uint32(len(payload)), Sense: nil}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := ring.Mailbox().CmdTail; got != length {
		t.Errorf("CmdTail after Complete = %d, want %d", got, length)
	}
	status, readLen, sense := decodeRspAt(mem, cmdrOff, 0)
	if status != 0 || readLen != uint32(len(payload)) {
		t.Errorf("response = status=%d readLen=%d, want 0/%d", status, readLen, len(payload))
	}
	for _, b := range sense {
		if b != 0 {
			t.Fatalf("sense buffer not zero-padded: %v", sense)
		}
	}

	if _, ok, err := cur.Next(); err != nil || ok {
		t.Fatalf("Next after catching up to head: ok=%v err=%v, want ok=false", ok, err)
	}
}

// TestCursorCompleteSetsUFlagReadLen reproduces the real bug found via a
// throwaway instance against a real kernel (2026-07-26): a real `mt`/dd
// read that stops early (a CHECK CONDITION completion carrying real
// partial data - the short-read/ILI path, or the newer filemark path)
// returned all zeros to the initiator even though the backing file on
// disk correctly held the real bytes, because Complete never told the
// kernel to trust rsp.read_len for a non-GOOD completion at all. This
// only asserts the wire-level fix (the uflags byte) - the "does a real
// kernel actually honor it" half was verified live against real
// hardware, not something a fake-mem unit test can prove on its own.
func TestCursorCompleteSetsUFlagReadLen(t *testing.T) {
	const cmdrOff, cmdrSize uint32 = 256, 1024
	mem := newFakeMem(t, 4096, cmdrOff, cmdrSize)
	dataCursor := cmdrOff + cmdrSize

	cdb := []byte{0x08, 0, 0, 0, 16, 0}
	length, _ := writeCmd(mem, cmdrOff, 0, 1, cdb, [][]byte{make([]byte, 16)}, &dataCursor)
	setCmdHead(mem, length)

	ring, err := NewRing(mem)
	if err != nil {
		t.Fatalf("NewRing: %v", err)
	}
	cur := ring.NewCursor()
	entry, ok, err := cur.Next()
	if err != nil || !ok {
		t.Fatalf("Next: ok=%v err=%v", ok, err)
	}

	// A CHECK CONDITION completion carrying partial data - exactly the
	// shape read6's short-read/ILI and filemark paths both produce.
	if err := cur.Complete(entry, Response{Status: 2, ReadLen: 5, Sense: make([]byte, SenseBufferSize)}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := decodeUFlagsAt(mem, cmdrOff, 0); got != UFlagReadLen {
		t.Errorf("uflags = %#x, want UFlagReadLen (%#x) - without this the kernel discards read_len (and the data with it) for any non-GOOD completion", got, UFlagReadLen)
	}
}

func TestRingSkipsPadAndWraps(t *testing.T) {
	const cmdrOff, cmdrSize uint32 = 128, 256
	mem := newFakeMem(t, 4096, cmdrOff, cmdrSize)
	dataCursor := cmdrOff + cmdrSize

	// First real command sits after an initial PAD spanning [0,128).
	writePad(mem, cmdrOff, 0, 128)
	cdb1 := []byte{0, 0, 0, 0, 0, 0}
	len1, _ := writeCmd(mem, cmdrOff, 128, 1, cdb1, nil, &dataCursor)
	if 128+len1 != 240 {
		t.Fatalf("test setup: expected first command to end at 240, got %d", 128+len1)
	}
	setCmdHead(mem, 240)

	ring, err := NewRing(mem)
	if err != nil {
		t.Fatalf("NewRing: %v", err)
	}
	cur := ring.NewCursor()

	entry, ok, err := cur.Next()
	if err != nil || !ok {
		t.Fatalf("Next (past pad): ok=%v err=%v", ok, err)
	}
	if entry.CmdID != 1 || !bytes.Equal(entry.CDB[:len(cdb1)], cdb1) {
		t.Fatalf("entry = %+v, want CmdID=1 CDB=%v", entry, cdb1)
	}
	if err := cur.Complete(entry, Response{Status: 0}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := ring.Mailbox().CmdTail; got != 240 {
		t.Fatalf("CmdTail = %d, want 240", got)
	}

	// Now wrap: a PAD fills the remaining 16 bytes to the ring boundary,
	// and a second command reuses the now-free space at ring offset 0.
	writePad(mem, cmdrOff, 240, 16)
	cdb2 := []byte{1, 1, 1, 1, 1, 1}
	len2, _ := writeCmd(mem, cmdrOff, 0, 2, cdb2, nil, &dataCursor)
	setCmdHead(mem, len2)

	entry2, ok, err := cur.Next()
	if err != nil || !ok {
		t.Fatalf("Next (after wrap): ok=%v err=%v", ok, err)
	}
	if entry2.CmdID != 2 || !bytes.Equal(entry2.CDB[:len(cdb2)], cdb2) {
		t.Fatalf("entry2 = %+v, want CmdID=2 CDB=%v", entry2, cdb2)
	}
	if err := cur.Complete(entry2, Response{Status: 0}); err != nil {
		t.Fatalf("Complete (after wrap): %v", err)
	}
	if got := ring.Mailbox().CmdTail; got != len2 {
		t.Errorf("CmdTail after wrap = %d, want %d", got, len2)
	}
	if _, ok, err := cur.Next(); err != nil || ok {
		t.Fatalf("Next after catching up post-wrap: ok=%v err=%v, want ok=false", ok, err)
	}
}

func TestNewRingRejectsBadVersion(t *testing.T) {
	mem := newFakeMem(t, 4096, 256, 1024)
	binary.LittleEndian.PutUint16(mem[0:2], 99)
	if _, err := NewRing(mem); err == nil {
		t.Fatal("expected an error for an unsupported mailbox version")
	}
}

func TestNewRingRejectsOversizedRing(t *testing.T) {
	mem := newFakeMem(t, 4096, 256, 1<<30) // cmdrSize far exceeds the region
	if _, err := NewRing(mem); err == nil {
		t.Fatal("expected an error for a command ring that doesn't fit in the mapped region")
	}
}

func TestCursorNextCorruptLength(t *testing.T) {
	const cmdrOff, cmdrSize = 256, 1024
	mem := newFakeMem(t, 4096, cmdrOff, cmdrSize)
	// A length of 0 is never valid (would spin forever without the guard
	// in Next) and must be reported as an error, not silently looped on.
	binary.LittleEndian.PutUint32(mem[cmdrOff:cmdrOff+4], uint32(OpCmd))
	setCmdHead(mem, 8)

	ring, err := NewRing(mem)
	if err != nil {
		t.Fatalf("NewRing: %v", err)
	}
	if _, _, err := ring.NewCursor().Next(); err == nil {
		t.Fatal("expected an error for a zero-length entry")
	}
}

func TestCursorNextIdempotentWithoutComplete(t *testing.T) {
	const cmdrOff, cmdrSize uint32 = 256, 1024
	mem := newFakeMem(t, 4096, cmdrOff, cmdrSize)
	dataCursor := cmdrOff + cmdrSize
	cdb := []byte{0, 0, 0, 0, 0, 0}
	length, _ := writeCmd(mem, cmdrOff, 0, 42, cdb, nil, &dataCursor)
	setCmdHead(mem, length)

	ring, err := NewRing(mem)
	if err != nil {
		t.Fatalf("NewRing: %v", err)
	}
	cur := ring.NewCursor()

	e1, ok1, err1 := cur.Next()
	e2, ok2, err2 := cur.Next()
	if err1 != nil || err2 != nil || !ok1 || !ok2 {
		t.Fatalf("Next/Next = (%v,%v,%v)/(%v,%v,%v)", e1, ok1, err1, e2, ok2, err2)
	}
	if e1.CmdID != e2.CmdID || e1.start != e2.start {
		t.Fatalf("repeated Next() without Complete returned different entries: %+v vs %+v", e1, e2)
	}
}
