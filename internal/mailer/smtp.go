package mailer

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/swenske/gotochanger/internal/config"
)

var _ Mailer = (*SMTPMailer)(nil)

// smtpTimeout bounds one Send call's total connect+protocol time - the
// offsite scheduler (cmd/gotochangerd) drives Send from a single
// dedicated goroutine shared with CompleteDueTransfers for every mode, so
// an unreachable/misconfigured host must not be able to stall it
// indefinitely.
const smtpTimeout = 15 * time.Second

// SMTPMailer delivers real mail via net/smtp, mirroring internal/snmp.
// Sender's New/SetConfig/Config shape so Settings.Update can hot-apply
// configuration changes without a daemon restart.
type SMTPMailer struct {
	mu  sync.Mutex
	cfg config.SMTPConfig
}

func NewSMTPMailer(cfg config.SMTPConfig) *SMTPMailer {
	return &SMTPMailer{cfg: cfg}
}

func (m *SMTPMailer) SetConfig(cfg config.SMTPConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg = cfg
}

func (m *SMTPMailer) Config() config.SMTPConfig {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

func (m *SMTPMailer) Send(msg Message) error {
	cfg := m.Config()
	if !cfg.Enabled {
		return nil
	}

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	dialer := net.Dialer{Timeout: smtpTimeout}
	var conn net.Conn
	var err error
	if cfg.UseTLS {
		conn, err = tls.DialWithDialer(&dialer, "tcp", addr, &tls.Config{ServerName: cfg.Host})
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("smtp: dial %s: %w", addr, err)
	}
	_ = conn.SetDeadline(time.Now().Add(smtpTimeout))

	c, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("smtp: handshake: %w", err)
	}
	defer c.Close()

	if cfg.StartTLS && !cfg.UseTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return fmt.Errorf("smtp: server does not support STARTTLS")
		}
		if err := c.StartTLS(&tls.Config{ServerName: cfg.Host}); err != nil {
			return fmt.Errorf("smtp: starttls: %w", err)
		}
	}

	if cfg.Username != "" {
		auth := smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("smtp: auth: %w", err)
		}
	}

	from := sanitizeHeader(cfg.From)
	if err := c.Mail(from); err != nil {
		return fmt.Errorf("smtp: mail from: %w", err)
	}
	for _, to := range msg.To {
		if err := c.Rcpt(sanitizeHeader(to)); err != nil {
			return fmt.Errorf("smtp: rcpt to %s: %w", to, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp: data: %w", err)
	}
	if _, err := w.Write(buildMessage(from, msg)); err != nil {
		return fmt.Errorf("smtp: write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp: close data: %w", err)
	}
	return c.Quit()
}

// buildMessage renders msg as a minimal RFC 5322 message - no
// attachments/HTML, matching Message's own "plain-text email" doc
// comment. from is expected pre-sanitized (Send already does this once
// and reuses it for both the envelope MAIL FROM and this header).
func buildMessage(from string, msg Message) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", sanitizeHeader(strings.Join(msg.To, ", ")))
	fmt.Fprintf(&b, "Subject: %s\r\n", sanitizeHeader(msg.Subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=\"utf-8\"\r\n")
	b.WriteString("\r\n")
	b.WriteString(toCRLF(msg.Body))
	return []byte(b.String())
}

// sanitizeHeader strips CR/LF before a value is interpolated into a raw
// SMTP command or RFC 5322 header line - defense in depth against
// header/command injection, even though every value here comes from
// admin-supplied config (host/from) or this feature's own generated
// content (subject), never arbitrary untrusted input.
func sanitizeHeader(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	return strings.ReplaceAll(s, "\n", "")
}

// toCRLF normalizes body line endings to CRLF, which RFC 5321/5322
// require on the wire - Message.Body (built with plain "\n" by
// internal/offsite/recap_email.go) does not provide this itself. Leading
// dots are escaped by net/smtp's own DotWriter (via Client.Data), not
// here.
func toCRLF(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}
