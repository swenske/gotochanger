package mailer

import (
	"bufio"
	"encoding/base64"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/swenske/gotochanger/internal/config"
)

func TestSanitizeHeaderStripsCRLF(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "plain", "plain"},
		{"CR only", "a\rb", "ab"},
		{"LF only", "a\nb", "ab"},
		{"CRLF", "a\r\nb", "ab"},
		{"header injection attempt", "evil\r\nBcc: attacker@example.com", "evilBcc: attacker@example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeHeader(tc.in); got != tc.want {
				t.Fatalf("sanitizeHeader(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestToCRLFNormalizesLineEndings(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare LF", "a\nb", "a\r\nb"},
		{"already CRLF", "a\r\nb", "a\r\nb"},
		{"mixed", "a\nb\r\nc", "a\r\nb\r\nc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := toCRLF(tc.in); got != tc.want {
				t.Fatalf("toCRLF(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestBuildMessageEscapesInjectionAttempts proves the sanitizeHeader calls
// inside buildMessage actually neutralize a CRLF-based header injection
// attempt, not just that sanitizeHeader itself works in isolation - a
// Subject/To containing an embedded "\r\nBcc: ..." must never surface as
// its own header line in the rendered message.
func TestBuildMessageEscapesInjectionAttempts(t *testing.T) {
	msg := Message{
		To:      []string{"victim@example.com", "evil\r\nBcc: attacker@example.com"},
		Subject: "Hello\r\nBcc: attacker2@example.com",
		Body:    "harmless body",
	}
	built := string(buildMessage("gotochanger@example.com", msg))

	if strings.Contains(built, "\nBcc:") || strings.Contains(built, "\rBcc:") {
		t.Fatalf("expected no injected Bcc header line, got message:\n%s", built)
	}
	if !strings.Contains(built, "Subject: Hello"+"Bcc: attacker2@example.com\r\n") {
		t.Fatalf("expected the injected CRLF to be stripped and merged into the Subject line, got:\n%s", built)
	}
	if !strings.Contains(built, "evilBcc: attacker@example.com") {
		t.Fatalf("expected the injected CRLF to be stripped and merged into the To line, got:\n%s", built)
	}
}

func TestSMTPMailerSendDisabledIsNoop(t *testing.T) {
	// Port 1 has nothing listening on it - if Send ever attempted to dial
	// despite Enabled=false, this would surface as a non-nil error (a fast
	// connection-refused, not a hang) rather than silently passing.
	cfg := config.SMTPConfig{Enabled: false, Host: "127.0.0.1", Port: 1}
	if err := NewSMTPMailer(cfg).Send(Message{To: []string{"x@example.com"}, Subject: "s", Body: "b"}); err != nil {
		t.Fatalf("expected Send to no-op when disabled, got %v", err)
	}
}

func TestSMTPMailerSetConfigConfigRoundTrip(t *testing.T) {
	m := NewSMTPMailer(config.SMTPConfig{Host: "a", Port: 1})
	if got := m.Config(); got.Host != "a" || got.Port != 1 {
		t.Fatalf("expected initial config to round-trip, got %+v", got)
	}
	m.SetConfig(config.SMTPConfig{Host: "b", Port: 2, Enabled: true})
	got := m.Config()
	if got.Host != "b" || got.Port != 2 || !got.Enabled {
		t.Fatalf("expected SetConfig to replace the config, got %+v", got)
	}
}

// fakeSMTPServer is a minimal, hand-rolled server-side implementation of
// just enough SMTP (EHLO/AUTH PLAIN/MAIL FROM/RCPT TO/DATA/QUIT) to
// complete one real exchange with a real *SMTPMailer over a real TCP
// socket - matching this codebase's stated preference for a real instance
// over a mock, and its stdlib-only posture (no third-party SMTP server
// package). Every test using it makes exactly one SMTPMailer.Send call,
// so acceptOne only ever serves a single connection.
type fakeSMTPServer struct {
	ln            net.Listener
	advertiseAuth bool

	mu    sync.Mutex
	auth  string
	from  string
	rcpts []string
	data  string
}

func newFakeSMTPServer(t *testing.T, advertiseAuth bool) *fakeSMTPServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeSMTPServer{ln: ln, advertiseAuth: advertiseAuth}
	t.Cleanup(func() { ln.Close() })
	go s.acceptOne()
	return s
}

// hostPort returns the loopback host/port SMTPMailer should dial. Host is
// deliberately the literal "127.0.0.1" (not e.g. a hostname) since
// net/smtp.PlainAuth refuses to send credentials over a non-TLS
// connection unless the server name is localhost/127.0.0.1/::1.
func (s *fakeSMTPServer) hostPort(t *testing.T) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(s.ln.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	return host, port
}

func (s *fakeSMTPServer) snapshot() (auth, from string, rcpts []string, data string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.auth, s.from, append([]string(nil), s.rcpts...), s.data
}

// acceptOne never calls t.Fatal/t.Errorf - those are unsafe from a
// goroutine other than the test's own (per the testing package's own
// documented contract). On any protocol mismatch it just returns; the
// resulting client-side error/timeout is what each test actually asserts
// against.
func (s *fakeSMTPServer) acceptOne() {
	conn, err := s.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	writeLine := func(line string) {
		_, _ = w.WriteString(line + "\r\n")
		_ = w.Flush()
	}

	writeLine("220 fake.test ESMTP ready")

	line, err := r.ReadString('\n')
	if err != nil || !strings.HasPrefix(strings.ToUpper(line), "EHLO") {
		return
	}
	if s.advertiseAuth {
		_, _ = w.WriteString("250-fake.test greets you\r\n")
		_, _ = w.WriteString("250 AUTH PLAIN\r\n")
	} else {
		_, _ = w.WriteString("250 fake.test greets you\r\n")
	}
	_ = w.Flush()

	line, err = r.ReadString('\n')
	if err != nil {
		return
	}
	line = strings.TrimRight(line, "\r\n")
	if strings.HasPrefix(strings.ToUpper(line), "AUTH PLAIN ") {
		decoded, _ := base64.StdEncoding.DecodeString(line[len("AUTH PLAIN "):])
		s.mu.Lock()
		s.auth = string(decoded)
		s.mu.Unlock()
		writeLine("235 2.7.0 Authentication successful")
		line, err = r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
	}

	if !strings.HasPrefix(strings.ToUpper(line), "MAIL FROM:") {
		return
	}
	s.mu.Lock()
	s.from = extractAngleAddr(line, "MAIL FROM:")
	s.mu.Unlock()
	writeLine("250 OK")

	for {
		line, err = r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(line)
		if strings.HasPrefix(upper, "RCPT TO:") {
			s.mu.Lock()
			s.rcpts = append(s.rcpts, extractAngleAddr(line, "RCPT TO:"))
			s.mu.Unlock()
			writeLine("250 OK")
			continue
		}
		if upper == "DATA" {
			writeLine("354 Start mail input; end with <CRLF>.<CRLF>")
			break
		}
		return
	}

	var data strings.Builder
	for {
		l, err := r.ReadString('\n')
		if err != nil {
			return
		}
		if l == ".\r\n" || l == ".\n" {
			break
		}
		if strings.HasPrefix(l, "..") {
			l = l[1:]
		}
		data.WriteString(l)
	}
	s.mu.Lock()
	s.data = data.String()
	s.mu.Unlock()
	writeLine("250 OK: queued")

	if line, err := r.ReadString('\n'); err == nil && strings.HasPrefix(strings.ToUpper(line), "QUIT") {
		writeLine("221 Bye")
	}
}

func extractAngleAddr(line, prefix string) string {
	v := strings.TrimPrefix(line, prefix)
	v = strings.TrimPrefix(v, "<")
	v = strings.TrimSuffix(v, ">")
	return v
}

func TestSMTPMailerSendFullExchange(t *testing.T) {
	srv := newFakeSMTPServer(t, true)
	host, port := srv.hostPort(t)

	cfg := config.SMTPConfig{
		Enabled:  true,
		Host:     host,
		Port:     port,
		Username: "testuser",
		Password: "testpass",
		From:     "gotochanger@example.com",
	}
	msg := Message{
		To:      []string{"ops@example.com", "backups@example.com"},
		Subject: "D-1 recap",
		Body:    "tapes due tomorrow\nsee dashboard",
	}
	if err := NewSMTPMailer(cfg).Send(msg); err != nil {
		t.Fatalf("send: %v", err)
	}

	auth, from, rcpts, data := srv.snapshot()
	wantAuth := "\x00testuser\x00testpass"
	if auth != wantAuth {
		t.Fatalf("expected decoded PLAIN auth %q, got %q", wantAuth, auth)
	}
	if from != cfg.From {
		t.Fatalf("expected MAIL FROM %q, got %q", cfg.From, from)
	}
	if len(rcpts) != 2 || rcpts[0] != msg.To[0] || rcpts[1] != msg.To[1] {
		t.Fatalf("expected recipients %v, got %v", msg.To, rcpts)
	}
	if !strings.Contains(data, "Subject: D-1 recap\r\n") {
		t.Fatalf("expected subject header in received message data, got %q", data)
	}
	if !strings.Contains(data, "tapes due tomorrow\r\nsee dashboard") {
		t.Fatalf("expected a CRLF-normalized body in received message data, got %q", data)
	}
}

func TestSMTPMailerSendStartTLSUnsupported(t *testing.T) {
	srv := newFakeSMTPServer(t, false) // advertises neither AUTH nor STARTTLS
	host, port := srv.hostPort(t)

	cfg := config.SMTPConfig{Enabled: true, Host: host, Port: port, StartTLS: true, From: "gotochanger@example.com"}
	err := NewSMTPMailer(cfg).Send(Message{To: []string{"ops@example.com"}, Subject: "x", Body: "y"})
	if err == nil {
		t.Fatal("expected an error when the server doesn't advertise STARTTLS")
	}
	if !strings.Contains(err.Error(), "does not support STARTTLS") {
		t.Fatalf("expected a STARTTLS-not-supported error, got %v", err)
	}
}

func TestSMTPMailerSendUseTLSAgainstPlaintextServer(t *testing.T) {
	srv := newFakeSMTPServer(t, false)
	host, port := srv.hostPort(t)

	cfg := config.SMTPConfig{Enabled: true, Host: host, Port: port, UseTLS: true, From: "gotochanger@example.com"}
	err := NewSMTPMailer(cfg).Send(Message{To: []string{"ops@example.com"}, Subject: "x", Body: "y"})
	if err == nil {
		t.Fatal("expected an error when dialing a plaintext server with UseTLS enabled")
	}
	if !strings.Contains(err.Error(), "dial") {
		t.Fatalf("expected a dial/TLS-handshake-shaped error, got %v", err)
	}
}
