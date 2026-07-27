// Package snmp implements a minimal, dependency-free SNMPv2c trap sender.
//
// Only what is needed to emit SNMPv2-Trap-PDUs (RFC 3416) is implemented:
// BER/DER encoding of INTEGER, OCTET STRING, OBJECT IDENTIFIER and TimeTicks,
// wrapped in the standard SNMP message envelope, sent over UDP. This avoids
// pulling in a third-party SNMP library for what is otherwise a very small
// amount of well-specified wire format.
package snmp

import (
	"bytes"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/swenske/gotochanger/internal/config"
	"github.com/swenske/gotochanger/internal/library"
)

// Standard OIDs required in every SNMPv2 trap PDU.
const (
	oidSysUpTime   = "1.3.6.1.2.1.1.3.0"
	oidSnmpTrapOID = "1.3.6.1.6.3.1.1.4.1.0"
)

// Sender emits SNMPv2c traps to one or more configured targets whenever it
// receives a library.Event. It implements library.Notifier.
type Sender struct {
	cfg       config.SNMPConfig
	startTime time.Time
	requestID atomic.Uint32
	mu        sync.Mutex
}

var _ library.Notifier = (*Sender)(nil)

// New builds a Sender from the daemon's SNMP configuration. If disabled, the
// returned Sender's Notify is a no-op, so callers can wire it in
// unconditionally.
func New(cfg config.SNMPConfig) *Sender {
	return &Sender{cfg: cfg, startTime: time.Now()}
}

// SetConfig atomically replaces the sender's configuration, letting the
// admin Settings API apply SNMP changes (enable/disable, targets,
// enterprise OID) without restarting the daemon.
func (s *Sender) SetConfig(cfg config.SNMPConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
}

// Config returns the sender's current configuration.
func (s *Sender) Config() config.SNMPConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// trapOIDSuffix maps structured event codes to trap suffixes under the
// configured enterprise OID.
var trapOIDSuffix = map[string]string{
	library.EventCodeRoboticsLoadSuccess:             "1",
	library.EventCodeRoboticsUnloadSuccess:           "2",
	library.EventCodeRoboticsMoveSuccess:             "3",
	library.EventCodeMediaImportSuccess:              "4",
	library.EventCodeMediaEjectSuccess:               "5",
	library.EventCodeDriveFaultSetSuccess:            "6",
	library.EventCodeMediaVolumeFullWarning:          "7",
	library.EventCodeMediaVolumeCreateSuccess:        "8",
	library.EventCodeMediaVolumeDeleteSuccess:        "9",
	library.EventCodeMediaOutsideCreateSuccess:       "10",
	library.EventCodeMediaOutsideDeleteSuccess:       "11",
	library.EventCodeRoboticsDoorIOOpenSuccess:       "12",
	library.EventCodeRoboticsDoorIOCloseSuccess:      "13",
	library.EventCodeRoboticsDoorStorageOpenSuccess:  "14",
	library.EventCodeRoboticsDoorStorageCloseSuccess: "15",
	library.EventCodeAuthLoginSuccess:                "20",
	library.EventCodeAuthLoginFailure:                "21",
	library.EventCodeAuthLogoutSuccess:               "22",
	library.EventCodeAuthBootstrapSuccess:            "23",
	library.EventCodeAuthBootstrapFailure:            "24",
	library.EventCodeAuthChangePasswordSuccess:       "25",
	library.EventCodeAuthChangePasswordFailure:       "26",
	library.EventCodeConfigSettingsUpdateSuccess:     "30",
	library.EventCodeConfigSettingsUpdateFailure:     "31",
	library.EventCodeConfigUserCreateSuccess:         "32",
	library.EventCodeConfigUserCreateFailure:         "33",
	library.EventCodeConfigUserDeleteSuccess:         "34",
	library.EventCodeConfigUserDeleteFailure:         "35",
	library.EventCodeConfigUserRoleSetSuccess:        "36",
	library.EventCodeConfigUserRoleSetFailure:        "37",
	library.EventCodeConfigUserPasswordResetSuccess:  "38",
	library.EventCodeConfigUserPasswordResetFailure:  "39",
	library.EventCodeConfigTokenCreateSuccess:        "40",
	library.EventCodeConfigTokenCreateFailure:        "41",
	library.EventCodeConfigTokenRevokeSuccess:        "42",
	library.EventCodeConfigTokenRevokeFailure:        "43",
	library.EventCodeRoboticsFaultSetSuccess:         "44",
	library.EventCodeRoboticsFaultSetFailure:         "45",
	library.EventCodeCleaningCycleSuccess:            "46",
	library.EventCodeCleaningTapeExpired:             "47",
	library.EventCodeCleaningTapeUnavailable:         "48",
	library.EventCodeCleaningTapeCreateSuccess:       "49",
	library.EventCodeCleaningTapeCreateFailure:       "50",
	library.EventCodeCleaningTapeEjectFallback:       "51",
	library.EventCodeCleaningCycleStart:              "52",
	library.EventCodeRoboticsLoadStarted:             "53",
	library.EventCodeRoboticsUnloadStarted:           "54",
	library.EventCodeConfigBackupScheduledRunSuccess: "55",
	library.EventCodeConfigBackupScheduledRunFailure: "56",
	library.EventCodeSystemPersistFailure:            "90",
}

// Notify sends a trap for evt to every configured target. Errors are not
// returned (traps are fire-and-forget/best-effort) but never panic.
func (s *Sender) Notify(evt library.Event) {
	if s == nil {
		return
	}
	cfg := s.Config()
	if !cfg.Enabled || len(cfg.Targets) == 0 {
		return
	}
	canon := library.CanonicalizeEvent(evt)
	trapOID := trapOIDForEvent(cfg, canon)
	varbinds := trapVarbindsForEvent(s.startTime, canon, trapOID)

	for _, t := range cfg.Targets {
		community := t.Community
		if community == "" {
			community = "public"
		}
		pkt := s.buildTrap(community, varbinds)
		s.send(t, pkt)
	}
}

func trapOIDForEvent(cfg config.SNMPConfig, evt library.Event) string {
	suffix, ok := trapOIDSuffix[evt.Code]
	if !ok {
		suffix = "0"
	}
	return strings.TrimSuffix(cfg.EnterpriseOID, ".") + "." + suffix
}

func trapVarbindsForEvent(start time.Time, evt library.Event, trapOID string) []varbind {
	varbinds := []varbind{
		{oid: oidSysUpTime, value: timeTicks(time.Since(start))},
		{oid: oidSnmpTrapOID, value: oidValue(trapOID)},
		{oid: trapOID + ".1", value: octetString(evt.Message)},
		{oid: trapOID + ".2.1", value: octetString(evt.Code)},
		{oid: trapOID + ".2.2", value: octetString(evt.Category)},
		{oid: trapOID + ".2.3", value: octetString(evt.Severity)},
		{oid: trapOID + ".2.4", value: octetString(evt.Outcome)},
		{oid: trapOID + ".2.5", value: octetString(evt.Operation)},
	}

	// Stable ordering of detail keys keeps traps deterministic and easy to test.
	keys := make([]string, 0, len(evt.Detail))
	for k := range evt.Detail {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > 0 {
		pairs := make([]string, 0, len(keys))
		for _, k := range keys {
			pairs = append(pairs, k+"="+evt.Detail[k])
		}
		varbinds = append(varbinds, varbind{oid: trapOID + ".3", value: octetString(strings.Join(pairs, "; "))})
	}
	return varbinds
}

func (s *Sender) send(t config.SNMPTarget, pkt []byte) {
	addr := net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
	conn, err := net.DialTimeout("udp", addr, 2*time.Second)
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = conn.Write(pkt)
}

func (s *Sender) buildTrap(community string, varbinds []varbind) []byte {
	reqID := s.requestID.Add(1)

	var vbSeq bytes.Buffer
	for _, vb := range varbinds {
		vbSeq.Write(sequence(append(oidValue(vb.oid), vb.value...)))
	}

	pdu := bytes.Buffer{}
	pdu.Write(integer(int64(reqID)))
	pdu.Write(integer(0)) // error-status
	pdu.Write(integer(0)) // error-index
	pdu.Write(sequence(vbSeq.Bytes()))

	pduTLV := tlv(0xA7, pdu.Bytes()) // SNMPv2-Trap-PDU

	msg := bytes.Buffer{}
	msg.Write(integer(1)) // SNMP version: 1 == v2c
	msg.Write(octetString(community))
	msg.Write(pduTLV)

	return sequence(msg.Bytes())
}

type varbind struct {
	oid   string
	value []byte
}
