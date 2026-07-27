package tcmu

import "testing"

func TestAttrEncodeDecodeRoundTrip(t *testing.T) {
	attrs := []Attr{
		attrString(ctrlAttrFamilyName, "TCM-USER"),
		attrU32(ctrlAttrFamilyID, 42),
		attrU8(7, 3),
	}
	encoded := encodeAttrs(attrs)
	decoded, err := decodeAttrs(encoded)
	if err != nil {
		t.Fatalf("decodeAttrs: %v", err)
	}
	if len(decoded) != len(attrs) {
		t.Fatalf("decoded %d attrs, want %d", len(decoded), len(attrs))
	}
	if got := decoded[0].String(); got != "TCM-USER" {
		t.Errorf("attr 0 = %q, want %q", got, "TCM-USER")
	}
	if v, err := decoded[1].Uint32(); err != nil || v != 42 {
		t.Errorf("attr 1 = %v (err=%v), want 42", v, err)
	}
	if len(decoded[2].Data) != 1 || decoded[2].Data[0] != 3 {
		t.Errorf("attr 2 = %v, want [3]", decoded[2].Data)
	}
}

func TestDecodeAttrsRejectsTruncatedHeader(t *testing.T) {
	if _, err := decodeAttrs([]byte{1, 2}); err == nil {
		t.Fatal("expected an error for a truncated attribute header")
	}
}

func TestDecodeAttrsRejectsOversizedLength(t *testing.T) {
	// nla_len claims 100 bytes but only 4 (the header itself) are present.
	buf := make([]byte, 4)
	buf[0], buf[1] = 100, 0
	if _, err := decodeAttrs(buf); err == nil {
		t.Fatal("expected an error for an attribute length exceeding the buffer")
	}
}

func TestBuildAndParseMessageRoundTrip(t *testing.T) {
	attrs := []Attr{attrString(ctrlAttrFamilyName, "TCM-USER")}
	raw := buildMessage(genlIDCtrl, nlmFRequest|nlmFAck, 7, 1234, ctrlCmdGetFamily, 1, attrs)

	msg, consumed, err := parseMessage(raw)
	if err != nil {
		t.Fatalf("parseMessage: %v", err)
	}
	if consumed != len(raw) {
		t.Errorf("consumed = %d, want %d", consumed, len(raw))
	}
	if msg.Header.Type != genlIDCtrl || msg.Header.Seq != 7 || msg.Header.PID != 1234 {
		t.Errorf("header = %+v", msg.Header)
	}
	if msg.GenlCmd != ctrlCmdGetFamily || msg.GenlVer != 1 {
		t.Errorf("genl cmd/version = %d/%d, want %d/%d", msg.GenlCmd, msg.GenlVer, ctrlCmdGetFamily, 1)
	}
	if len(msg.Attrs) != 1 || msg.Attrs[0].String() != "TCM-USER" {
		t.Fatalf("attrs = %+v", msg.Attrs)
	}
}

func TestParseMessageDecodesNetlinkError(t *testing.T) {
	body := make([]byte, 4+nlMsgHdrLen)                         // errno(4) + the echoed original nlmsghdr
	body[0], body[1], body[2], body[3] = 0xf6, 0xff, 0xff, 0xff // -10, little-endian
	buf := make([]byte, nlMsgHdrLen+len(body))
	total := len(buf)
	buf[0] = byte(total)
	buf[4], buf[5] = nlmsgError, 0
	copy(buf[nlMsgHdrLen:], body)

	msg, _, err := parseMessage(buf)
	if err != nil {
		t.Fatalf("parseMessage: %v", err)
	}
	if msg.Header.Type != nlmsgError {
		t.Fatalf("Header.Type = %d, want nlmsgError", msg.Header.Type)
	}
	if msg.Errno != -10 {
		t.Errorf("Errno = %d, want -10", msg.Errno)
	}
}

func TestParseFamilyReplyExtractsIDAndMCastGroups(t *testing.T) {
	// One nested mcast-group entry: index attr (type is just an array
	// index, ignored) wrapping CTRL_ATTR_MCAST_GRP_NAME/ID.
	group := encodeAttrs([]Attr{
		attrString(ctrlAttrMCastGrpName, "config"),
		attrU32(ctrlAttrMCastGrpID, 5),
	})
	groups := encodeAttrs([]Attr{{Type: 1, Data: group}})

	msg := parsedMessage{
		Attrs: []Attr{
			attrU16(ctrlAttrFamilyID, 99),
			{Type: ctrlAttrMCastGroups, Data: groups},
		},
	}

	familyID, mcast, err := parseFamilyReply(msg)
	if err != nil {
		t.Fatalf("parseFamilyReply: %v", err)
	}
	if familyID != 99 {
		t.Errorf("familyID = %d, want 99", familyID)
	}
	if mcast["config"] != 5 {
		t.Errorf("mcast[config] = %d, want 5", mcast["config"])
	}
}

func TestParseFamilyReplyErrorsWithoutFamilyID(t *testing.T) {
	if _, _, err := parseFamilyReply(parsedMessage{}); err == nil {
		t.Fatal("expected an error when the reply carries no family id")
	}
}

// attrU16 mirrors attrU32 for the one attribute (CTRL_ATTR_FAMILY_ID)
// that's actually a u16 on the wire - only needed by this test, since
// production code only ever decodes it (via Attr.Uint16), never encodes
// it (this package is never the one answering a GETFAMILY request).
func attrU16(t uint16, v uint16) Attr {
	return Attr{Type: t, Data: []byte{byte(v), byte(v >> 8)}}
}
