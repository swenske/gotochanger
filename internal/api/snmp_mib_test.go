package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRenderSNMPMIB_DefaultOID(t *testing.T) {
	mib, err := renderSNMPMIB("1.3.6.1.4.1.55555.1")
	if err != nil {
		t.Fatalf("renderSNMPMIB: %v", err)
	}
	if !strings.Contains(mib, "::= { enterprises 55555 }") {
		t.Fatalf("expected PEN 55555 in rendered MIB")
	}
	if !strings.Contains(mib, "gotochangerVtl OBJECT IDENTIFIER ::= { gotochanger 1 }") {
		t.Fatalf("expected root suffix 1 in rendered MIB")
	}
	if !strings.Contains(mib, ".3      detail string") {
		t.Fatalf("expected detail field documentation at .3")
	}
}

func TestRenderSNMPMIB_MultiArcSuffix(t *testing.T) {
	mib, err := renderSNMPMIB("1.3.6.1.4.1.424242.9.7")
	if err != nil {
		t.Fatalf("renderSNMPMIB: %v", err)
	}
	if !strings.Contains(mib, "::= { enterprises 424242 }") {
		t.Fatalf("expected PEN 424242 in rendered MIB")
	}
	if !strings.Contains(mib, "gotochangerVtlBase1 OBJECT IDENTIFIER ::= { gotochanger 9 }") {
		t.Fatalf("expected first suffix arc in rendered MIB")
	}
	if !strings.Contains(mib, "gotochangerVtl OBJECT IDENTIFIER ::= { gotochangerVtlBase1 7 }") {
		t.Fatalf("expected final suffix arc in rendered MIB")
	}
	if !strings.Contains(mib, "1.3.6.1.4.1.424242.9.7.<event-id>") {
		t.Fatalf("expected rendered enterprise OID in description")
	}
}

func TestRenderSNMPMIB_RejectsInvalidOID(t *testing.T) {
	_, err := renderSNMPMIB("foo.bar")
	if err == nil {
		t.Fatalf("expected invalid OID error")
	}
}

func TestSNMPMIBEndpoint_AuthAndContent(t *testing.T) {
	s := newPublicTestServer(t)
	h := s.PublicHandler()

	unauth := httptest.NewRecorder()
	h.ServeHTTP(unauth, httptest.NewRequest(http.MethodGet, "/api/v1/snmp/mib", nil))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauth: expected %d got %d body=%s", http.StatusUnauthorized, unauth.Code, unauth.Body.String())
	}

	cookie := loginCookie(t, h, "Admin", "AdminPass123!")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/snmp/mib", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("auth: expected %d got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Fatalf("expected text/plain content type, got %q", ct)
	}
	if !strings.Contains(rr.Body.String(), "GOTOCHANGER-MIB DEFINITIONS ::= BEGIN") {
		t.Fatalf("expected MIB payload")
	}
}
