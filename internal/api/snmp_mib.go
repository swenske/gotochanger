package api

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

const (
	defaultMIBEnterpriseOID = "1.3.6.1.4.1.55555.1"
	templatePENLine         = "::= { enterprises 55555 }"
	templateRootLine        = "gotochangerVtl OBJECT IDENTIFIER ::= { gotochanger 1 }"
)

// handleSNMPMIB serves a generated MIB whose enterprise base reflects the
// daemon's current SNMP settings, so operators can import the exact OID tree
// currently emitted in traps.
func (s *Server) handleSNMPMIB(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg
	if s.settings != nil {
		cfg = s.settings.Current()
	}

	mib, err := renderSNMPMIB(cfg.SNMP.EnterpriseOID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=gotochanger.mib")
	_, _ = io.WriteString(w, mib)
}

func renderSNMPMIB(enterpriseOID string) (string, error) {
	raw, err := staticAssets.Open("gotochanger.mib")
	if err != nil {
		return "", err
	}
	defer raw.Close()

	tplBytes, err := io.ReadAll(raw)
	if err != nil {
		return "", err
	}
	tpl := string(tplBytes)

	oid, arcs, err := normalizeOID(enterpriseOID)
	if err != nil {
		return "", fmt.Errorf("snmp.enterprise_oid: %w", err)
	}

	if len(arcs) < 7 || strings.Join(arcs[:6], ".") != "1.3.6.1.4.1" {
		return "", fmt.Errorf("must start with 1.3.6.1.4.1 (got %s)", oid)
	}

	pen := arcs[6]
	suffix := arcs[7:]

	penLine := "::= { enterprises " + pen + " }"
	rootDef := makeMIBRootDefinition(suffix)

	mib := strings.Replace(tpl, templatePENLine, penLine, 1)
	mib = strings.Replace(mib, templateRootLine, rootDef, 1)
	mib = strings.ReplaceAll(mib, defaultMIBEnterpriseOID+".<event-id>", oid+".<event-id>")
	mib = strings.ReplaceAll(mib, ".3.1    detail string", ".3      detail string")
	return mib, nil
}

func makeMIBRootDefinition(suffix []string) string {
	if len(suffix) == 0 {
		return "gotochangerVtl OBJECT IDENTIFIER ::= { gotochanger }"
	}
	if len(suffix) == 1 {
		return "gotochangerVtl OBJECT IDENTIFIER ::= { gotochanger " + suffix[0] + " }"
	}

	var b strings.Builder
	b.WriteString("gotochangerVtlBase1 OBJECT IDENTIFIER ::= { gotochanger ")
	b.WriteString(suffix[0])
	b.WriteString(" }\n")
	for i := 1; i < len(suffix)-1; i++ {
		b.WriteString("gotochangerVtlBase")
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString(" OBJECT IDENTIFIER ::= { gotochangerVtlBase")
		b.WriteString(strconv.Itoa(i))
		b.WriteString(" ")
		b.WriteString(suffix[i])
		b.WriteString(" }\n")
	}
	b.WriteString("gotochangerVtl OBJECT IDENTIFIER ::= { gotochangerVtlBase")
	b.WriteString(strconv.Itoa(len(suffix) - 1))
	b.WriteString(" ")
	b.WriteString(suffix[len(suffix)-1])
	b.WriteString(" }")
	return b.String()
}

func normalizeOID(s string) (string, []string, error) {
	oid := strings.Trim(strings.TrimSpace(s), ".")
	if oid == "" {
		return "", nil, fmt.Errorf("must not be empty")
	}
	parts := strings.Split(oid, ".")
	for _, p := range parts {
		if p == "" {
			return "", nil, fmt.Errorf("contains empty arc")
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return "", nil, fmt.Errorf("contains invalid arc %q", p)
		}
	}
	return oid, parts, nil
}
