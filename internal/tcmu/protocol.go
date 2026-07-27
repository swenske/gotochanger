// Package tcmu implements the userspace half of the Linux TCMU
// (target_core_user) protocol in pure Go — no cgo, no dependency on
// tcmu-runner or libtcmu, matching this codebase's existing convention of
// hand-rolling a kernel-facing primitive from stdlib (see the inotify
// watcher in internal/library/activity_linux.go and the PBKDF2 password
// hashing in internal/api/password_hash.go).
//
// This file holds the wire layout only: encoding/decoding the shared-memory
// mailbox and command-entry structures, verified against the kernel UAPI
// header include/uapi/linux/target_core_user.h (TCMU_MAILBOX_VERSION 2).
// It has no dependency on mmap, configfs, or netlink — see ring.go for the
// ring-buffer logic built on top of these layouts (testable against a plain
// []byte, no real kernel needed), and uio_linux.go/configfs.go/netlink.go
// (added separately) for the real syscalls that supply that []byte from an
// actual /dev/uioN device.
package tcmu

import "encoding/binary"

// alignSize mirrors the kernel's ALIGN_SIZE ("should be enough for most
// CPUs"): struct tcmu_mailbox.cmd_tail is placed at the first offset that's
// a multiple of this from the mailbox's own start, and every command
// entry's length is rounded up to a multiple of opAlignSize below.
const alignSize = 64

// mailboxSize is sizeof(struct tcmu_mailbox): version(2)+flags(2)+
// cmdr_off(4)+cmdr_size(4)+cmd_head(4) = 16 bytes, then cmd_tail forced to
// the next 64-byte-aligned offset (64), then the whole struct's size
// rounds up to a multiple of its own 64-byte alignment requirement (128).
// In practice the kernel places cmdr_off at exactly this offset.
const mailboxSize = 128

// cmdTailOffset is cmd_tail's byte offset within the mailbox, per the
// alignment derivation above.
const cmdTailOffset = 64

// cmdEntryHdrSize is sizeof(struct tcmu_cmd_entry_hdr): len_op(4) +
// cmd_id(2) + kflags(1) + uflags(1).
const cmdEntryHdrSize = 8

// reqHeaderSize is the size of tcmu_cmd_entry.req's fixed portion (before
// its flexible iov[] array): iov_cnt(4) + iov_bidi_cnt(4) + iov_dif_cnt(4)
// + 4 bytes of compiler-inserted padding + cdb_off(8) + __pad1(8) +
// __pad2(8) = 40 bytes.
//
// That padding is not in the kernel header's field list and was missed on
// first reading it (an earlier version of this file had reqHeaderSize=36,
// cdb_off immediately after iov_dif_cnt with no gap) - found and fixed
// against a real kernel, by dumping a live entry's raw bytes and comparing
// them to a known-good CDB found elsewhere in the same entry (an INQUIRY
// with a recognizable "12 00 00 00 24 00" - opcode, EVPD=0, page=0,
// alloc_len=0x24, control - sitting exactly where cdb_off should have
// pointed once the extra 4 bytes were accounted for, verified byte for
// byte). Root cause: tcmu_cmd_entry's outer struct is __packed, but req is
// an *anonymous inner struct* inside a union, and that outer __packed
// does not propagate to force cdb_off (a __u64) out of its own natural
// 8-byte alignment *within* req - the compiler still inserts padding so
// cdb_off starts on an 8-byte boundary relative to req's own start, same
// as it would in a completely ordinary (non-packed) struct. Whether this
// generalizes to other still-unverified structures in this file (there
// are none with a similar mixed-width-then-u64 shape left) isn't
// something to assume from this one instance - the mailbox's cmd_tail
// (see cmdTailOffset) relies on an *explicit* aligned(64) attribute, a
// different and independently-confirmed mechanism, not this same gap.
const reqHeaderSize = 40

// reqCDBOffOffset is cdb_off's byte offset within req, per the padding
// explained on reqHeaderSize above (iov_cnt+iov_bidi_cnt+iov_dif_cnt(12)
// rounded up to the next 8-byte boundary, i.e. 16 - not 12).
const reqCDBOffOffset = 16

// iovecSize is sizeof(struct iovec) on 64-bit Linux: iov_base(8) + iov_len(8).
const iovecSize = 16

// rspSize is the size of tcmu_cmd_entry.rsp: scsi_status(1) + __pad1(1) +
// __pad2(2) + read_len(4) + sense_buffer(96) = 104 bytes.
const rspSize = 104

// SenseBufferSize is TCMU_SENSE_BUFFERSIZE.
const SenseBufferSize = 96

// opAlignSize is TCMU_OP_ALIGN_SIZE (sizeof(uint64_t)): every command
// entry's length, encoded in the low bits of len_op, is a multiple of this.
const opAlignSize = 8

// opMask is TCMU_OP_MASK: the low 3 bits of len_op hold the opcode, the
// rest holds the entry's total length (already a multiple of opAlignSize,
// so no information is lost by masking it off).
const opMask = 0x7

// Opcode is tcmu_opcode.
type Opcode uint8

// Opcodes, per enum tcmu_opcode. Note PAD is 0, not CMD — a zeroed ring
// region (e.g. one that was never written) reads back as an all-PAD
// entry, never mistaken for a real command.
const (
	OpPad Opcode = 0
	OpCmd Opcode = 1
	OpTMR Opcode = 2
)

// Response header flags (uflags, entryHdr.uflags - a header field, not
// part of rsp itself, see Cursor.Complete's own doc comment for exactly
// where and why this gets written). UFlagReadLen is the only one this
// implementation ever sets, unconditionally, on every completed command -
// see Cursor.Complete. UFlagUnknownOp/UFlagKeepBuf are documented for
// completeness only; nothing here sets them.
const (
	UFlagUnknownOp = 0x1
	UFlagReadLen   = 0x2
	UFlagKeepBuf   = 0x4
)

// MailboxVersion is TCMU_MAILBOX_VERSION. NewRing rejects any other value
// read from a real mailbox rather than guessing at a different layout.
const MailboxVersion = 2

// Mailbox mirrors struct tcmu_mailbox.
type Mailbox struct {
	Version  uint16
	Flags    uint16
	CmdrOff  uint32 // offset of the command ring from the start of the mmap'd region
	CmdrSize uint32 // size of the command ring, in bytes
	CmdHead  uint32 // ring-relative offset, advanced by the kernel as it submits new entries
	CmdTail  uint32 // ring-relative offset, advanced by userspace as it completes entries
}

func decodeMailbox(b []byte) Mailbox {
	return Mailbox{
		Version:  binary.LittleEndian.Uint16(b[0:2]),
		Flags:    binary.LittleEndian.Uint16(b[2:4]),
		CmdrOff:  binary.LittleEndian.Uint32(b[4:8]),
		CmdrSize: binary.LittleEndian.Uint32(b[8:12]),
		CmdHead:  binary.LittleEndian.Uint32(b[12:16]),
		CmdTail:  binary.LittleEndian.Uint32(b[cmdTailOffset : cmdTailOffset+4]),
	}
}

// encodeMailboxHeader writes the fields userspace is ever allowed to set at
// mailbox construction time (version/flags/cmdr_off/cmdr_size/cmd_head) —
// used only by tests building a fake mailbox to exercise Ring against; a
// real mailbox is always written by the kernel, never by this package.
func encodeMailboxHeader(b []byte, mb Mailbox) {
	binary.LittleEndian.PutUint16(b[0:2], mb.Version)
	binary.LittleEndian.PutUint16(b[2:4], mb.Flags)
	binary.LittleEndian.PutUint32(b[4:8], mb.CmdrOff)
	binary.LittleEndian.PutUint32(b[8:12], mb.CmdrSize)
	binary.LittleEndian.PutUint32(b[12:16], mb.CmdHead)
}

// writeCmdTail writes only cmd_tail — the one mailbox field this package
// (acting as userspace) is ever allowed to update on a real mailbox.
func writeCmdTail(b []byte, tail uint32) {
	binary.LittleEndian.PutUint32(b[cmdTailOffset:cmdTailOffset+4], tail)
}

// decodeCmdHead re-reads just cmd_head, without needing to redecode the
// whole mailbox — used by Cursor.Next to check for newly-submitted entries
// against a kernel that may have updated it concurrently.
func decodeCmdHead(b []byte) uint32 {
	return binary.LittleEndian.Uint32(b[12:16])
}

// entryHdr mirrors struct tcmu_cmd_entry_hdr.
type entryHdr struct {
	lenOp  uint32
	cmdID  uint16
	kflags uint8
	uflags uint8
}

func decodeEntryHdr(b []byte) entryHdr {
	return entryHdr{
		lenOp:  binary.LittleEndian.Uint32(b[0:4]),
		cmdID:  binary.LittleEndian.Uint16(b[4:6]),
		kflags: b[6],
		uflags: b[7],
	}
}

func (h entryHdr) opcode() Opcode { return Opcode(h.lenOp & opMask) }
func (h entryHdr) length() uint32 { return h.lenOp &^ opMask }

// reqHeader mirrors tcmu_cmd_entry.req's fixed portion (see reqHeaderSize).
type reqHeader struct {
	iovCnt     uint32
	iovBidiCnt uint32
	iovDifCnt  uint32
	cdbOff     uint64 // offset of the CDB bytes from the start of the mmap'd region, not from this entry
}

func decodeReqHeader(b []byte) reqHeader {
	return reqHeader{
		iovCnt:     binary.LittleEndian.Uint32(b[0:4]),
		iovBidiCnt: binary.LittleEndian.Uint32(b[4:8]),
		iovDifCnt:  binary.LittleEndian.Uint32(b[8:12]),
		cdbOff:     binary.LittleEndian.Uint64(b[reqCDBOffOffset : reqCDBOffOffset+8]),
	}
}

// decodeIovec reads one struct iovec. Both fields are offsets/lengths
// relative to the mmap'd region's start, not real pointers — the kernel
// and userspace map the same shared memory at different virtual addresses,
// so a raw pointer would be meaningless across that boundary.
func decodeIovec(b []byte) (offset, length uint64) {
	return binary.LittleEndian.Uint64(b[0:8]), binary.LittleEndian.Uint64(b[8:16])
}

// encodeRsp writes tcmu_cmd_entry.rsp into b (which must be rspSize bytes,
// the same memory the req fields were just read from — the two are a C
// union over the same bytes, so every req field must already have been
// consumed before this is called).
func encodeRsp(b []byte, status uint8, readLen uint32, sense []byte) {
	b[0] = status
	b[1] = 0
	binary.LittleEndian.PutUint16(b[2:4], 0)
	binary.LittleEndian.PutUint32(b[4:8], readLen)
	n := copy(b[8:8+SenseBufferSize], sense)
	for i := 8 + n; i < 8+SenseBufferSize; i++ {
		b[i] = 0
	}
}
