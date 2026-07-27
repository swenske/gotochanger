package tcmu

import (
	"encoding/binary"
	"fmt"
)

// This file implements the slice of Linux netlink message framing (struct
// nlmsghdr, struct genlmsghdr, struct nlattr TLVs) needed to talk to the
// kernel's generic-netlink controller and the TCM-USER family: pure
// encoding/decoding over []byte, no socket syscalls - see netlink_linux.go
// for those. Mirrors this package's existing split (protocol.go/ring.go)
// between wire-format logic, testable without any real kernel resource,
// and the syscalls that supply real bytes to it.
//
// Constants below are from the stable, long-established parts of the ABI
// (include/uapi/linux/netlink.h, include/uapi/linux/genetlink.h) - unlike
// TCM-USER's own attrs (see protocol.go's doc comment), these haven't
// changed in over a decade and are common to every generic-netlink
// consumer, not just TCMU.
const (
	nlMsgAlign  = 4 // NLMSG_ALIGNTO / NLA_ALIGNTO - both 4 on every arch Linux runs on
	nlMsgHdrLen = 16
	genlHdrLen  = 4
	nlaHdrLen   = 4
)

const (
	nlmsgError = 0x2
	nlmsgDone  = 0x3

	nlmFRequest = 0x1
	nlmFAck     = 0x4
)

// The generic-netlink controller family: a fixed, well-known family id
// (unlike every other genl family, including TCM-USER, whose id is
// assigned dynamically at module load time) used to resolve a family by
// name and discover its multicast groups.
const (
	genlIDCtrl = 0x10

	ctrlCmdGetFamily = 3

	ctrlAttrFamilyID    = 1
	ctrlAttrFamilyName  = 2
	ctrlAttrMCastGroups = 7

	ctrlAttrMCastGrpName = 1
	ctrlAttrMCastGrpID   = 2
)

func alignNL(n int) int { return (n + nlMsgAlign - 1) &^ (nlMsgAlign - 1) }

// Attr is one decoded (or to-be-encoded) netlink attribute (a single TLV).
type Attr struct {
	Type uint16
	Data []byte
}

func attrU8(t uint16, v uint8) Attr { return Attr{Type: t, Data: []byte{v}} }

func attrU32(t uint16, v uint32) Attr {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, v)
	return Attr{Type: t, Data: b}
}

func attrS32(t uint16, v int32) Attr { return attrU32(t, uint32(v)) }

// attrString builds an NLA_STRING attribute, which (unlike a plain byte
// blob) includes a trailing NUL in its encoded payload.
func attrString(t uint16, s string) Attr {
	return Attr{Type: t, Data: append([]byte(s), 0)}
}

func (a Attr) Uint16() (uint16, error) {
	if len(a.Data) < 2 {
		return 0, fmt.Errorf("attribute %d too short for uint16 (%d bytes)", a.Type, len(a.Data))
	}
	return binary.LittleEndian.Uint16(a.Data), nil
}

func (a Attr) Uint32() (uint32, error) {
	if len(a.Data) < 4 {
		return 0, fmt.Errorf("attribute %d too short for uint32 (%d bytes)", a.Type, len(a.Data))
	}
	return binary.LittleEndian.Uint32(a.Data), nil
}

// String strips NLA_STRING's trailing NUL, if present.
func (a Attr) String() string {
	s := a.Data
	if n := len(s); n > 0 && s[n-1] == 0 {
		s = s[:n-1]
	}
	return string(s)
}

// encodeAttrs concatenates attrs into a properly-padded TLV byte sequence.
func encodeAttrs(attrs []Attr) []byte {
	var out []byte
	for _, a := range attrs {
		length := nlaHdrLen + len(a.Data)
		hdr := make([]byte, nlaHdrLen)
		binary.LittleEndian.PutUint16(hdr[0:2], uint16(length))
		binary.LittleEndian.PutUint16(hdr[2:4], a.Type)
		out = append(out, hdr...)
		out = append(out, a.Data...)
		if pad := alignNL(length) - length; pad > 0 {
			out = append(out, make([]byte, pad)...)
		}
	}
	return out
}

// decodeAttrs parses a TLV byte sequence back into Attrs, erroring on any
// length that would run past the end of b rather than panicking - this
// data ultimately comes from the kernel over a socket, so a malformed or
// truncated read must produce an error, not a crash. Used both for a
// message's top-level attributes and, recursively, for nested attributes
// like CTRL_ATTR_MCAST_GROUPS (the format is identical at every nesting
// level).
func decodeAttrs(b []byte) ([]Attr, error) {
	var attrs []Attr
	for len(b) > 0 {
		if len(b) < nlaHdrLen {
			return nil, fmt.Errorf("truncated attribute header (%d bytes left)", len(b))
		}
		length := int(binary.LittleEndian.Uint16(b[0:2]))
		typ := binary.LittleEndian.Uint16(b[2:4])
		if length < nlaHdrLen || length > len(b) {
			return nil, fmt.Errorf("attribute %d: invalid length %d (%d bytes left)", typ, length, len(b))
		}
		attrs = append(attrs, Attr{Type: typ, Data: b[nlaHdrLen:length]})
		adv := alignNL(length)
		if adv > len(b) {
			adv = len(b)
		}
		b = b[adv:]
	}
	return attrs, nil
}

// NLMsgHeader mirrors struct nlmsghdr.
type NLMsgHeader struct {
	Len   uint32
	Type  uint16
	Flags uint16
	Seq   uint32
	PID   uint32
}

// buildMessage assembles one complete generic-netlink datagram: nlmsghdr,
// then genlmsghdr, then attrs' TLVs. Every message this package ever sends
// is a generic-netlink one (control-family GETFAMILY requests, and
// TCM-USER ADDED_DEVICE_DONE/RECONFIG_DEVICE_DONE replies), so there's no
// need for a variant without a genl header.
func buildMessage(nlType, flags uint16, seq, pid uint32, genlCmd, genlVersion uint8, attrs []Attr) []byte {
	body := encodeAttrs(attrs)
	total := nlMsgHdrLen + genlHdrLen + len(body)
	buf := make([]byte, total)
	binary.LittleEndian.PutUint32(buf[0:4], uint32(total))
	binary.LittleEndian.PutUint16(buf[4:6], nlType)
	binary.LittleEndian.PutUint16(buf[6:8], flags)
	binary.LittleEndian.PutUint32(buf[8:12], seq)
	binary.LittleEndian.PutUint32(buf[12:16], pid)
	buf[16] = genlCmd
	buf[17] = genlVersion
	// buf[18:20] is genlmsghdr's reserved field, already zero.
	copy(buf[20:], body)
	return buf
}

// parsedMessage is one decoded netlink message: its header, generic-netlink
// command/version (meaningful only when Header.Type addresses a genl
// family, i.e. isn't one of the fixed control types below), and
// attributes.
type parsedMessage struct {
	Header  NLMsgHeader
	GenlCmd uint8
	GenlVer uint8
	Attrs   []Attr
	Errno   int32 // only meaningful when Header.Type == nlmsgError
}

// parseMessage decodes exactly one netlink message from the front of b,
// returning how many bytes it consumed (aligned) - callers loop over
// multiple messages in a single recvfrom() buffer themselves (see
// netlink_linux.go), since the kernel is free to coalesce several
// messages into one datagram.
func parseMessage(b []byte) (parsedMessage, int, error) {
	if len(b) < nlMsgHdrLen {
		return parsedMessage{}, 0, fmt.Errorf("truncated netlink header (%d bytes)", len(b))
	}
	length := int(binary.LittleEndian.Uint32(b[0:4]))
	if length < nlMsgHdrLen || length > len(b) {
		return parsedMessage{}, 0, fmt.Errorf("invalid netlink message length %d (%d bytes available)", length, len(b))
	}
	hdr := NLMsgHeader{
		Len:   uint32(length),
		Type:  binary.LittleEndian.Uint16(b[4:6]),
		Flags: binary.LittleEndian.Uint16(b[6:8]),
		Seq:   binary.LittleEndian.Uint32(b[8:12]),
		PID:   binary.LittleEndian.Uint32(b[12:16]),
	}
	msg := parsedMessage{Header: hdr}
	body := b[nlMsgHdrLen:length]
	consumed := alignNL(length)

	switch hdr.Type {
	case nlmsgError:
		if len(body) < 4 {
			return parsedMessage{}, 0, fmt.Errorf("truncated netlink error body")
		}
		msg.Errno = int32(binary.LittleEndian.Uint32(body[0:4]))
	case nlmsgDone:
		// no body to parse
	default:
		if len(body) < genlHdrLen {
			return parsedMessage{}, 0, fmt.Errorf("truncated genl header")
		}
		msg.GenlCmd = body[0]
		msg.GenlVer = body[1]
		attrs, err := decodeAttrs(body[genlHdrLen:])
		if err != nil {
			return parsedMessage{}, 0, fmt.Errorf("decode attrs: %w", err)
		}
		msg.Attrs = attrs
	}
	return msg, consumed, nil
}

// buildGetFamilyRequest builds a CTRL_CMD_GETFAMILY request for the given
// family name, addressed to the fixed controller family id.
func buildGetFamilyRequest(name string, seq, pid uint32) []byte {
	attrs := []Attr{attrString(ctrlAttrFamilyName, name)}
	return buildMessage(genlIDCtrl, nlmFRequest|nlmFAck, seq, pid, ctrlCmdGetFamily, 1, attrs)
}

// parseFamilyReply extracts the resolved family id and multicast-group
// name -> id map from a CTRL_CMD_GETFAMILY response's attributes. Every
// value the controller doesn't happen to include (e.g. no multicast groups
// at all) is simply absent from the returned map, not an error.
func parseFamilyReply(msg parsedMessage) (familyID uint16, mcastGroups map[string]uint32, err error) {
	mcastGroups = map[string]uint32{}
	for _, a := range msg.Attrs {
		switch a.Type {
		case ctrlAttrFamilyID:
			familyID, err = a.Uint16()
			if err != nil {
				return 0, nil, fmt.Errorf("family id: %w", err)
			}
		case ctrlAttrMCastGroups:
			groups, err := decodeAttrs(a.Data)
			if err != nil {
				return 0, nil, fmt.Errorf("mcast groups: %w", err)
			}
			for _, grp := range groups {
				inner, err := decodeAttrs(grp.Data)
				if err != nil {
					return 0, nil, fmt.Errorf("mcast group entry: %w", err)
				}
				var name string
				var id uint32
				for _, ia := range inner {
					switch ia.Type {
					case ctrlAttrMCastGrpName:
						name = ia.String()
					case ctrlAttrMCastGrpID:
						id, _ = ia.Uint32()
					}
				}
				if name != "" {
					mcastGroups[name] = id
				}
			}
		}
	}
	if familyID == 0 {
		return 0, nil, fmt.Errorf("reply carried no family id")
	}
	return familyID, mcastGroups, nil
}
