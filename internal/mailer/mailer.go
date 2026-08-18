// Package mailer sends the offsite feature's recap emails, behind a
// small interface so the concrete delivery mechanism is swappable.
// LoggingMailer (see logging.go) never delivers real mail - it logs the
// message and records it as a library.Event, matching gotochanger being
// a full simulator by default. SMTPMailer (see smtp.go) is a real,
// stdlib-only (net/smtp + crypto/tls) sender, gated by its own Enabled
// flag and hot-reloadable via SetConfig, mirroring internal/snmp.Sender.
// cmd/gotochangerd always wires both together via MultiMailer, so the
// recap is unconditionally logged/audited *and*, when SMTP is enabled
// and configured, actually delivered.
package mailer

import "errors"

// Message is a plain-text email - no HTML/attachments, matching this
// codebase's minimal-dependency posture. JSON tags exist because this
// type is also served directly by GET /api/v1/offsite/recap-preview.
type Message struct {
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	Body    string   `json:"body"`
}

type Mailer interface {
	Send(msg Message) error
}

// NoopMailer discards every message. Useful as a zero-value default in
// tests; gotochangerd itself always constructs a LoggingMailer (see
// logging.go) so simulated sends are still visible in the event log.
type NoopMailer struct{}

func (NoopMailer) Send(Message) error { return nil }

// MultiMailer fans a single Send call out to every non-nil child,
// mirroring library.MultiNotifier's shape - unlike Notify, Send can fail,
// so every child is still attempted unconditionally (best-effort, the
// same "call everyone regardless" spirit) and every non-nil error is
// joined rather than the first one short-circuiting the rest.
type MultiMailer []Mailer

func (m MultiMailer) Send(msg Message) error {
	var errs []error
	for _, mm := range m {
		if mm == nil {
			continue
		}
		if err := mm.Send(msg); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
