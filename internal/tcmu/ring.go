package tcmu

import "fmt"

// maxCDBLen bounds how many bytes Entry.CDB ever holds. Real SCSI CDBs are
// at most 16 bytes for every command this project implements (see
// internal/scsi) — 32 leaves headroom without the risk of reading deep
// into unrelated data-area memory for a corrupt/malicious entry.
const maxCDBLen = 32

// Ring is a parsed view of one TCMU device's shared-memory region (the
// bytes backing a real /dev/uioN mmap - see uio_linux.go - or, in tests, a
// plain []byte standing in for one). It only ever reads mem's mailbox
// fields and cmd_head (both kernel-owned) and writes cmd_tail and response
// bytes (the only fields userspace is allowed to touch) - see the kernel
// UAPI header's own division of the mailbox/entry fields between "updated
// by kernel" and "updated by userspace".
type Ring struct {
	mem      []byte
	cmdrOff  uint32
	cmdrSize uint32
}

// NewRing validates mem's mailbox (version, and that the command ring it
// describes actually fits within mem) and returns a Ring ready for
// NewCursor. mem is not copied - callers own its lifetime (an actual mmap
// for a real device, or a plain slice in tests).
func NewRing(mem []byte) (*Ring, error) {
	if len(mem) < mailboxSize {
		return nil, fmt.Errorf("tcmu: region too small for a mailbox (%d bytes, need at least %d)", len(mem), mailboxSize)
	}
	mb := decodeMailbox(mem)
	if mb.Version != MailboxVersion {
		return nil, fmt.Errorf("tcmu: unsupported mailbox version %d (want %d)", mb.Version, MailboxVersion)
	}
	end := uint64(mb.CmdrOff) + uint64(mb.CmdrSize)
	if end < uint64(mb.CmdrOff) || end > uint64(len(mem)) {
		return nil, fmt.Errorf("tcmu: command ring [%d:%d) exceeds mapped region (%d bytes)", mb.CmdrOff, end, len(mem))
	}
	return &Ring{mem: mem, cmdrOff: mb.CmdrOff, cmdrSize: mb.CmdrSize}, nil
}

// Mailbox re-reads the live mailbox fields. Cheap; safe to call as often as
// needed since cmd_head can change at any time on a real device (updated
// by the kernel, potentially concurrently with this process).
func (r *Ring) Mailbox() Mailbox { return decodeMailbox(r.mem) }

// NewCursor starts a Cursor at the mailbox's current cmd_tail - the point
// up to which entries have already been completed (by a prior run of this
// process, or 0 on a freshly enabled device).
func (r *Ring) NewCursor() *Cursor { return &Cursor{ring: r, pos: r.Mailbox().CmdTail} }

// Entry is one parsed command-ring entry, ready for a handler to act on.
// For Opcode == OpCmd, CDB and Buffers are populated; for any other opcode
// (only OpTMR exists today, unimplemented - task management is out of
// scope for this project's supported command set) they're left nil, and a
// caller that doesn't recognize the opcode should still call Complete with
// an appropriate error response so the ring keeps moving.
type Entry struct {
	CmdID  uint16
	Opcode Opcode

	// CDB holds up to maxCDBLen bytes starting at the real CDB's offset -
	// its true length depends on the CDB's own opcode/group code (SCSI's
	// own self-describing framing), which is internal/scsi's job to
	// decode, not this package's.
	CDB []byte

	// Buffers is one []byte per iovec (iov_cnt of them), each a direct
	// view into the shared mmap region: already populated with
	// initiator-written data for an outbound (WRITE-like) command, or
	// empty scratch space a handler must fill in-place for an inbound
	// (READ-like) one. Writes to these slices are visible to the kernel
	// as soon as they happen - no separate "commit" step, unlike the
	// response itself (see Complete).
	Buffers [][]byte

	start  uint32 // ring-relative offset of this entry - Complete needs it, callers never do
	length uint32 // this entry's total on-ring length - Complete needs it, callers never do
}

// Response is what a handler hands back to Cursor.Complete after acting on
// an Entry.
type Response struct {
	Status  uint8  // SCSI status, e.g. 0x00 (GOOD) or 0x02 (CHECK CONDITION)
	Sense   []byte // sense data; truncated or zero-padded to SenseBufferSize
	ReadLen uint32 // actual bytes transferred for a data-in command; leave 0 when there's no data-in phase
}

// Cursor walks a Ring's command entries in order, from wherever cmd_tail
// last stopped. It is not safe for concurrent use - a real TCMU device has
// exactly one such reader (mirroring this project's single-robotic-arm,
// one-thing-at-a-time model elsewhere), so no locking is needed or
// provided.
//
// Next and Complete must alternate: call Next, act on the returned Entry
// (including finishing every read of Entry.CDB/Buffers - Complete
// overwrites the same bytes), then call Complete for it before calling
// Next again. Calling Next twice without an intervening Complete just
// re-returns the same entry (nothing has advanced), rather than skipping
// or corrupting anything.
type Cursor struct {
	ring *Ring
	pos  uint32
}

// Next returns the next command entry to process, skipping any PAD
// entries the kernel inserted (ring-wraparound filler, or otherwise),
// or ok == false if the cursor has caught up to the live cmd_head (nothing
// new since the last check - a real caller should then block on the
// device's UIO fd, see uio_linux.go, rather than busy-poll this).
func (c *Cursor) Next() (entry Entry, ok bool, err error) {
	for {
		head := decodeCmdHead(c.ring.mem)
		if c.pos == head {
			return Entry{}, false, nil
		}
		absStart := uint64(c.ring.cmdrOff) + uint64(c.pos)
		hdrBytes, err := sliceAt(c.ring.mem, absStart, cmdEntryHdrSize)
		if err != nil {
			return Entry{}, false, fmt.Errorf("tcmu: read entry header at ring offset %d: %w", c.pos, err)
		}
		hdr := decodeEntryHdr(hdrBytes)
		length := hdr.length()
		if length == 0 || length%opAlignSize != 0 || uint64(length) > uint64(c.ring.cmdrSize) {
			return Entry{}, false, fmt.Errorf("tcmu: corrupt entry at ring offset %d: invalid length %d", c.pos, length)
		}
		if hdr.opcode() == OpPad {
			c.pos = uint32((uint64(c.pos) + uint64(length)) % uint64(c.ring.cmdrSize))
			continue
		}
		entry := Entry{CmdID: hdr.cmdID, Opcode: hdr.opcode(), start: c.pos, length: length}
		if hdr.opcode() == OpCmd {
			if err := c.ring.parseCmd(&entry, absStart); err != nil {
				return Entry{}, false, fmt.Errorf("tcmu: parse command at ring offset %d: %w", c.pos, err)
			}
		}
		return entry, true, nil
	}
}

// Complete writes resp into e's response region - the same bytes e.CDB's
// request fields were read from (a C union in the kernel ABI) - so callers
// must be finished with e.CDB/e.Buffers before calling this. It then
// advances the ring past e and persists that as the mailbox's cmd_tail,
// telling the kernel this entry (and anything skipped before it, e.g. a
// PAD) is fully handled.
func (c *Cursor) Complete(e Entry, resp Response) error {
	if e.length < cmdEntryHdrSize+rspSize {
		return fmt.Errorf("tcmu: entry at ring offset %d (length %d) too small to hold a response (need %d)", e.start, e.length, cmdEntryHdrSize+rspSize)
	}
	absStart := uint64(c.ring.cmdrOff) + uint64(e.start)

	// UFlagReadLen must be set in the entry's own *header* (byte 7,
	// uflags - see cmdEntryHdrSize's doc comment for the header layout),
	// not anywhere inside rsp itself, or the kernel silently ignores
	// rsp.read_len for any completion that isn't a plain GOOD status and
	// transfers zero bytes back to the initiator - regardless of what
	// data was actually written into e.Buffers via a zero-copy fill.
	// UFlagReadLen has existed as a named constant in this package since
	// its own doc comment first claimed "this implementation only ever
	// sets ReadLen explicitly" - but nothing ever actually wrote it here,
	// so that claim was never true. Found for real, not defensively: a
	// real Bareos restore/copy job (2026-07-26) needed READ(6) to stop
	// exactly at a filemark and report the real partial data it read up
	// to that point (SILI/CHECK CONDITION with Filemark set, ReadLen>0) -
	// confirmed via a completely isolated throwaway gotochangerd/
	// gotochanger-tcmud instance (own HBA number, own data dir, no real
	// data touched) that the *backing file on disk* correctly held the
	// real bytes, but reading them back through a real `mt`/dd against
	// the real kernel returned all zeros - proving the bytes were
	// correctly placed into the shared mmap buffer but the kernel wasn't
	// told to trust read_len for that non-GOOD completion at all. The
	// exact same test against the *pre-existing* short-read/ILI path
	// (READ(6) requesting more than a volume's recorded length, nothing
	// to do with filemarks) reproduced the identical data loss - this was
	// never specific to the new filemark work, it's a general gap in
	// every partial-read-with-data response this package has ever sent,
	// only now actually exercised against a real kernel for the first
	// time (every prior real-hardware pass only verified a plain,
	// full-length GOOD-status read, per CLAUDE.md's Milestone 1 writeup).
	// Confirmed safe to set unconditionally on every completion (not just
	// non-GOOD ones): per the kernel's own read_len handling, it's only
	// ever consulted for a data-in command in the first place, and when
	// it's present it only ever *clamps* the transfer down to the real
	// value already provided - which for a full GOOD read is the same
	// value the kernel already had, a no-op.
	hdrBytes, err := sliceAt(c.ring.mem, absStart, cmdEntryHdrSize)
	if err != nil {
		return fmt.Errorf("tcmu: write uflags at ring offset %d: %w", e.start, err)
	}
	hdrBytes[7] = UFlagReadLen

	rspBytes, err := sliceAt(c.ring.mem, absStart+cmdEntryHdrSize, rspSize)
	if err != nil {
		return fmt.Errorf("tcmu: write response at ring offset %d: %w", e.start, err)
	}
	encodeRsp(rspBytes, resp.Status, resp.ReadLen, resp.Sense)
	c.pos = uint32((uint64(e.start) + uint64(e.length)) % uint64(c.ring.cmdrSize))
	writeCmdTail(c.ring.mem, c.pos)
	return nil
}

// parseCmd fills in e.CDB/e.Buffers for an OpCmd entry starting at absStart
// (an absolute offset into r.mem, already validated by the caller).
func (r *Ring) parseCmd(e *Entry, absStart uint64) error {
	reqStart := absStart + cmdEntryHdrSize
	reqBytes, err := sliceAt(r.mem, reqStart, reqHeaderSize)
	if err != nil {
		return fmt.Errorf("read req header: %w", err)
	}
	req := decodeReqHeader(reqBytes)

	cdb, err := sliceUpTo(r.mem, req.cdbOff, maxCDBLen)
	if err != nil {
		return fmt.Errorf("read cdb: %w", err)
	}
	e.CDB = cdb

	iovStart := reqStart + reqHeaderSize
	buffers := make([][]byte, 0, req.iovCnt)
	for i := uint32(0); i < req.iovCnt; i++ {
		iovBytes, err := sliceAt(r.mem, iovStart+uint64(i)*iovecSize, iovecSize)
		if err != nil {
			return fmt.Errorf("read iovec %d: %w", i, err)
		}
		off, length := decodeIovec(iovBytes)
		buf, err := sliceAt(r.mem, off, length)
		if err != nil {
			return fmt.Errorf("read iovec %d data (off=%d len=%d): %w", i, off, length, err)
		}
		buffers = append(buffers, buf)
	}
	e.Buffers = buffers
	return nil
}

// sliceAt returns exactly length bytes starting at off, erroring rather
// than panicking if that range doesn't fit in mem - every offset/length
// here ultimately comes from kernel-shared memory, so a corrupt or
// adversarial mailbox must produce an error, not a crash.
func sliceAt(mem []byte, off, length uint64) ([]byte, error) {
	if length == 0 {
		return nil, nil
	}
	end := off + length
	if end < off || end > uint64(len(mem)) {
		return nil, fmt.Errorf("out of bounds: offset %d length %d exceeds region size %d", off, length, len(mem))
	}
	return mem[off:end], nil
}

// sliceUpTo returns up to n bytes starting at off, clamped to what's
// actually present in mem. Used only for the CDB, whose real length isn't
// known until the caller decodes its own leading opcode byte, so this
// package only guarantees "at least the real CDB fits", not exactly n
// bytes.
func sliceUpTo(mem []byte, off uint64, n uint64) ([]byte, error) {
	if off > uint64(len(mem)) {
		return nil, fmt.Errorf("out of bounds: offset %d exceeds region size %d", off, len(mem))
	}
	avail := uint64(len(mem)) - off
	if avail < n {
		n = avail
	}
	return mem[off : off+n], nil
}
