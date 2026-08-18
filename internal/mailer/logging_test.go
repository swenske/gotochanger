package mailer

import (
	"log/slog"
	"testing"

	"github.com/swenske/gotochanger/internal/library"
)

type capturingRecorder struct {
	events []library.Event
}

func (r *capturingRecorder) RecordEvent(evt library.Event) {
	r.events = append(r.events, evt)
}

func TestLoggingMailerRecordsAnEventInsteadOfSending(t *testing.T) {
	rec := &capturingRecorder{}
	m := NewLoggingMailer(rec, slog.Default())

	msg := Message{To: []string{"ops@example.com"}, Subject: "test subject", Body: "test body"}
	if err := m.Send(msg); err != nil {
		t.Fatalf("send: %v", err)
	}

	if len(rec.events) != 1 {
		t.Fatalf("expected exactly one recorded event, got %d", len(rec.events))
	}
	evt := rec.events[0]
	if evt.Code != library.EventCodeOffsiteRecapEmailSuccess {
		t.Fatalf("expected EventCodeOffsiteRecapEmailSuccess, got %q", evt.Code)
	}
	if evt.Message != msg.Subject {
		t.Fatalf("expected the event message to be the email subject, got %q", evt.Message)
	}
	if evt.Detail["to"] != "ops@example.com" || evt.Detail["body"] != "test body" {
		t.Fatalf("expected recipient/body in the event detail, got %+v", evt.Detail)
	}
}

func TestNoopMailerDiscardsSilently(t *testing.T) {
	if err := (NoopMailer{}).Send(Message{Subject: "x"}); err != nil {
		t.Fatalf("expected NoopMailer to never error, got %v", err)
	}
}
