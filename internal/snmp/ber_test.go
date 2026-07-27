package snmp

import (
	"encoding/asn1"
	"strings"
	"testing"
	"time"

	"github.com/swenske/gotochanger/internal/config"
	"github.com/swenske/gotochanger/internal/library"
)

// TestBuildTrapParses ensures the hand-rolled BER encoder produces a byte
// stream that a standard ASN.1 BER/DER parser can walk without error: the
// outer SEQUENCE, the INTEGER version, the OCTET STRING community, and
// (recursively) every varbind's OID/value pair.
func TestBuildTrapParses(t *testing.T) {
	s := New(config.SNMPConfig{Enabled: true, EnterpriseOID: "1.3.6.1.4.1.55555.1"})
	pkt := s.buildTrap("public", []varbind{
		{oid: oidSysUpTime, value: timeTicks(time.Second)},
		{oid: oidSnmpTrapOID, value: oidValue("1.3.6.1.4.1.55555.1.1")},
		{oid: "1.3.6.1.4.1.55555.1.1.1", value: octetString("hello world")},
	})

	var outer asn1.RawValue
	rest, err := asn1.Unmarshal(pkt, &outer)
	if err != nil {
		t.Fatalf("outer SEQUENCE did not parse: %v", err)
	}
	if len(rest) != 0 {
		t.Fatalf("unexpected trailing bytes: %d", len(rest))
	}
	if outer.Class != asn1.ClassUniversal || outer.Tag != asn1.TagSequence {
		t.Fatalf("outer element is not a universal SEQUENCE: class=%d tag=%d", outer.Class, outer.Tag)
	}

	var version int
	body := outer.Bytes
	body, err = asn1.Unmarshal(body, &version)
	if err != nil || version != 1 {
		t.Fatalf("version did not parse as INTEGER(1): version=%d err=%v", version, err)
	}
	var community []byte
	body, err = asn1.Unmarshal(body, &community)
	if err != nil || string(community) != "public" {
		t.Fatalf("community did not parse as OCTET STRING(public): %q err=%v", community, err)
	}

	var pdu asn1.RawValue
	if _, err := asn1.Unmarshal(body, &pdu); err != nil {
		t.Fatalf("PDU did not parse: %v", err)
	}
	if pdu.Class != asn1.ClassContextSpecific || pdu.Tag != 7 {
		t.Fatalf("PDU is not context-tagged [7] (SNMPv2-Trap-PDU): class=%d tag=%d", pdu.Class, pdu.Tag)
	}

	// Walk request-id, error-status, error-index, then the varbind SEQUENCE.
	rem := pdu.Bytes
	var reqID, errStatus, errIndex int
	for _, dst := range []*int{&reqID, &errStatus, &errIndex} {
		rem, err = asn1.Unmarshal(rem, dst)
		if err != nil {
			t.Fatalf("PDU integer field did not parse: %v", err)
		}
	}
	var vbSeq asn1.RawValue
	if _, err := asn1.Unmarshal(rem, &vbSeq); err != nil {
		t.Fatalf("varbind list did not parse as SEQUENCE: %v", err)
	}

	count := 0
	rest2 := vbSeq.Bytes
	for len(rest2) > 0 {
		var vb asn1.RawValue
		rest2, err = asn1.Unmarshal(rest2, &vb)
		if err != nil {
			t.Fatalf("varbind #%d did not parse: %v", count, err)
		}
		var oid asn1.ObjectIdentifier
		if _, err := asn1.Unmarshal(vb.Bytes, &oid); err != nil {
			t.Fatalf("varbind #%d OID did not parse: %v", count, err)
		}
		count++
	}
	if count != 3 {
		t.Fatalf("expected 3 varbinds, got %d", count)
	}
}

func TestNotifyDisabledIsNoop(t *testing.T) {
	s := New(config.SNMPConfig{Enabled: false})
	// Must not panic and must not attempt any network I/O.
	s.Notify(library.Event{Type: "load", Message: "test"})
}

func TestOIDEncodeRoundTrip(t *testing.T) {
	want := "1.3.6.1.4.1.55555.1.7"
	full := oidValue(want)
	var got asn1.ObjectIdentifier
	if _, err := asn1.Unmarshal(full, &got); err != nil {
		t.Fatalf("oidValue output did not parse: %v", err)
	}
	if got.String() != want {
		t.Fatalf("OID round-trip mismatch: want %s got %s", want, got.String())
	}
}

func TestTrapOIDForEventCode(t *testing.T) {
	cfg := config.SNMPConfig{EnterpriseOID: "1.3.6.1.4.1.55555.1"}
	oid := trapOIDForEvent(cfg, library.CanonicalizeEvent(library.Event{Code: library.EventCodeAuthLoginFailure, Message: "bad login"}))
	if oid != "1.3.6.1.4.1.55555.1.21" {
		t.Fatalf("unexpected auth failure trap OID: %s", oid)
	}

	unknown := trapOIDForEvent(cfg, library.CanonicalizeEvent(library.Event{Code: "CUSTOM.EVENT.SUCCESS", Message: "custom"}))
	if unknown != "1.3.6.1.4.1.55555.1.0" {
		t.Fatalf("unexpected unknown trap OID: %s", unknown)
	}
}

func TestTrapVarbindsDetailOrdering(t *testing.T) {
	evt := library.CanonicalizeEvent(library.Event{
		Code:    library.EventCodeConfigSettingsUpdateFailure,
		Message: "settings update failed",
		Detail: map[string]string{
			"zeta":  "3",
			"alpha": "1",
			"beta":  "2",
		},
	})
	trapOID := "1.3.6.1.4.1.55555.1.31"
	vbs := trapVarbindsForEvent(time.Now().Add(-time.Second), evt, trapOID)

	if len(vbs) != 9 {
		t.Fatalf("expected 9 varbinds (8 base + 1 detail), got %d", len(vbs))
	}

	if vbs[3].oid != trapOID+".2.1" || !strings.Contains(string(vbs[3].value), evt.Code) {
		t.Fatalf("expected code varbind at .2.1")
	}

	if vbs[len(vbs)-1].oid != trapOID+".3" {
		t.Fatalf("unexpected detail OID: got %s want %s", vbs[len(vbs)-1].oid, trapOID+".3")
	}
	detail := string(vbs[len(vbs)-1].value)
	if !strings.Contains(detail, "alpha=1") {
		t.Fatalf("expected consolidated detail to contain alpha=1, got %q", detail)
	}
	if !strings.Contains(detail, "beta=2") {
		t.Fatalf("expected consolidated detail to contain beta=2, got %q", detail)
	}
	if !strings.Contains(detail, "zeta=3") {
		t.Fatalf("expected consolidated detail to contain zeta=3, got %q", detail)
	}
}
