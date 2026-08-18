package mailer

import (
	"log/slog"
	"strings"

	"github.com/swenske/gotochanger/internal/library"
)

// eventRecorder is the one method LoggingMailer needs from *library.Library
// - narrow on purpose so this package doesn't need the whole Library
// surface just to audit a simulated send.
type eventRecorder interface {
	RecordEvent(evt library.Event)
}

// LoggingMailer never delivers real mail - it logs the message and
// records it as a library.Event (so it's visible in the Admin UI's
// activity log and the offsite recap-preview endpoint doesn't need a
// separate stored-history table), matching this product being a full
// simulator rather than a real mail sender by default.
type LoggingMailer struct {
	log *slog.Logger
	rec eventRecorder
}

func NewLoggingMailer(rec eventRecorder, log *slog.Logger) *LoggingMailer {
	return &LoggingMailer{log: log, rec: rec}
}

func (m *LoggingMailer) Send(msg Message) error {
	m.log.Info("simulated email", "to", strings.Join(msg.To, ","), "subject", msg.Subject)
	m.rec.RecordEvent(library.Event{
		Code:    library.EventCodeOffsiteRecapEmailSuccess,
		Message: msg.Subject,
		Detail:  map[string]string{"to": strings.Join(msg.To, ","), "body": msg.Body},
	})
	return nil
}
