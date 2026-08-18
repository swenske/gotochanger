package mailer

import (
	"errors"
	"testing"
)

type fakeMailer struct {
	err  error
	sent []Message
}

func (f *fakeMailer) Send(msg Message) error {
	f.sent = append(f.sent, msg)
	return f.err
}

func TestMultiMailerSend(t *testing.T) {
	msg := Message{To: []string{"a@example.com"}, Subject: "s", Body: "b"}

	t.Run("zero mailers", func(t *testing.T) {
		var m MultiMailer
		if err := m.Send(msg); err != nil {
			t.Fatalf("expected nil error for an empty MultiMailer, got %v", err)
		}
	})

	t.Run("one succeeding", func(t *testing.T) {
		f := &fakeMailer{}
		m := MultiMailer{f}
		if err := m.Send(msg); err != nil {
			t.Fatalf("expected nil error, got %v", err)
		}
		if len(f.sent) != 1 {
			t.Fatalf("expected 1 sent message, got %d", len(f.sent))
		}
	})

	t.Run("one failing", func(t *testing.T) {
		errBoom := errors.New("boom")
		f := &fakeMailer{err: errBoom}
		m := MultiMailer{f}
		err := m.Send(msg)
		if !errors.Is(err, errBoom) {
			t.Fatalf("expected an error wrapping errBoom, got %v", err)
		}
	})

	t.Run("two succeeding", func(t *testing.T) {
		f1, f2 := &fakeMailer{}, &fakeMailer{}
		m := MultiMailer{f1, f2}
		if err := m.Send(msg); err != nil {
			t.Fatalf("expected nil error, got %v", err)
		}
		if len(f1.sent) != 1 || len(f2.sent) != 1 {
			t.Fatalf("expected both mailers to receive the message, got %d and %d", len(f1.sent), len(f2.sent))
		}
	})

	t.Run("first fails but second still runs", func(t *testing.T) {
		errBoom := errors.New("boom")
		f1 := &fakeMailer{err: errBoom}
		f2 := &fakeMailer{}
		m := MultiMailer{f1, f2}
		err := m.Send(msg)
		if !errors.Is(err, errBoom) {
			t.Fatalf("expected an error wrapping errBoom, got %v", err)
		}
		if len(f1.sent) != 1 || len(f2.sent) != 1 {
			t.Fatalf("expected both mailers to be attempted despite the first failing, got %d and %d", len(f1.sent), len(f2.sent))
		}
	})

	t.Run("both fail", func(t *testing.T) {
		errA := errors.New("mailer a failed")
		errB := errors.New("mailer b failed")
		f1 := &fakeMailer{err: errA}
		f2 := &fakeMailer{err: errB}
		m := MultiMailer{f1, f2}
		err := m.Send(msg)
		if !errors.Is(err, errA) || !errors.Is(err, errB) {
			t.Fatalf("expected the joined error to contain both failures, got %v", err)
		}
	})

	t.Run("nil entry is skipped without panicking", func(t *testing.T) {
		f := &fakeMailer{}
		m := MultiMailer{nil, f}
		if err := m.Send(msg); err != nil {
			t.Fatalf("expected nil error, got %v", err)
		}
		if len(f.sent) != 1 {
			t.Fatalf("expected the non-nil mailer to still receive the message, got %d", len(f.sent))
		}
	})
}
