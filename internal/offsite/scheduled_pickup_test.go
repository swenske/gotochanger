package offsite

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/swenske/gotochanger/internal/mailer"
)

func TestNextVisitAt(t *testing.T) {
	monday := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	if monday.Weekday() != time.Monday {
		t.Fatalf("test setup: %v is not a Monday", monday)
	}

	tests := []struct {
		name     string
		now      time.Time
		wd       time.Weekday
		window   string
		wantDiff time.Duration // nextVisitAt's result, expressed as an offset from now
	}{
		{
			name:     "later today, before the window",
			now:      monday.Add(9 * time.Hour),
			wd:       time.Monday,
			window:   "10:00",
			wantDiff: time.Hour,
		},
		{
			name:     "today's window already passed, rolls to next week",
			now:      monday.Add(11 * time.Hour),
			wd:       time.Monday,
			window:   "10:00",
			wantDiff: 7*24*time.Hour - time.Hour,
		},
		{
			name:     "target weekday later this week",
			now:      monday.Add(9 * time.Hour),
			wd:       time.Thursday,
			window:   "10:00",
			wantDiff: 3*24*time.Hour + time.Hour,
		},
		{
			name:     "target weekday already passed this week, rolls to next week",
			now:      monday.AddDate(0, 0, 4).Add(9 * time.Hour), // Friday 09:00
			wd:       time.Thursday,
			window:   "10:00",
			wantDiff: 6*24*time.Hour + time.Hour,
		},
		{
			name:     "exactly at the window instant does not roll forward",
			now:      monday.Add(10 * time.Hour),
			wd:       time.Monday,
			window:   "10:00",
			wantDiff: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := nextVisitAt(tt.now, tt.wd, tt.window)
			if err != nil {
				t.Fatalf("nextVisitAt: %v", err)
			}
			want := tt.now.Add(tt.wantDiff)
			if !got.Equal(want) {
				t.Fatalf("nextVisitAt(%v, %v, %q) = %v, want %v", tt.now, tt.wd, tt.window, got, want)
			}
			if got.Before(tt.now) {
				t.Fatalf("nextVisitAt must never return an instant before now: got %v, now %v", got, tt.now)
			}
		})
	}
}

func TestParseWeekdayAndTimeOfDay(t *testing.T) {
	if _, err := parseWeekday("Thursday"); err != nil {
		t.Fatalf("expected case-insensitive weekday parsing to succeed: %v", err)
	}
	if _, err := parseWeekday("funday"); err == nil {
		t.Fatalf("expected an invalid weekday name to fail")
	}
	if h, m, err := parseTimeOfDay("09:05"); err != nil || h != 9 || m != 5 {
		t.Fatalf("parseTimeOfDay(09:05) = %d:%d, %v", h, m, err)
	}
	if h, m, err := parseTimeOfDay("9:5"); err != nil || h != 9 || m != 5 {
		t.Fatalf("expected non-zero-padded input to parse leniently, got %d:%d, %v", h, m, err)
	}
	for _, bad := range []string{"24:00", "10:60", "abc", "10-00"} {
		if _, _, err := parseTimeOfDay(bad); err == nil {
			t.Fatalf("expected %q to fail to parse", bad)
		}
	}
}

func TestScheduledPickupValidateConfig(t *testing.T) {
	m := NewScheduledPickupMode()
	valid := ScheduledPickupConfig{
		Weekday:                   "thursday",
		WindowStart:               "10:00",
		WindowEnd:                 "14:00",
		ExceptionalRetrievalDelay: "4h",
		MailboxIDs:                []string{"Mailbox1"},
	}
	raw, _ := json.Marshal(valid)
	if err := m.ValidateConfig(raw); err != nil {
		t.Fatalf("expected a fully populated config to validate, got %v", err)
	}

	tests := []struct {
		name   string
		mutate func(c *ScheduledPickupConfig)
	}{
		{"bad weekday", func(c *ScheduledPickupConfig) { c.Weekday = "someday" }},
		{"bad window_start", func(c *ScheduledPickupConfig) { c.WindowStart = "25:00" }},
		{"bad window_end", func(c *ScheduledPickupConfig) { c.WindowEnd = "" }},
		{"bad exceptional_retrieval_delay", func(c *ScheduledPickupConfig) { c.ExceptionalRetrievalDelay = "soon" }},
		{"no mailboxes", func(c *ScheduledPickupConfig) { c.MailboxIDs = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid
			tt.mutate(&cfg)
			raw, _ := json.Marshal(cfg)
			if err := m.ValidateConfig(raw); err == nil {
				t.Fatalf("expected validation to fail")
			}
		})
	}
}

func saveScheduledPickupConfig(t *testing.T, st interface {
	SetOffsiteModeConfig(mode string, raw json.RawMessage) error
}, cfg ScheduledPickupConfig) {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := st.SetOffsiteModeConfig("scheduled_pickup", raw); err != nil {
		t.Fatalf("save scheduled_pickup config: %v", err)
	}
}

func TestScheduledPickupModeRequestPickupUsesNextVisit(t *testing.T) {
	lib := newTestLibrary(t)
	st := newTestStore(t)
	eng := NewEngine(lib, st, mailer.NoopMailer{}, NewRegistry())
	mode := NewScheduledPickupMode()

	status := lib.Status()
	rackCartridge(t, lib, "VOLA0001", status.Slots[0].Address)

	// A window that's already passed today for every weekday but has not
	// yet arrived this instant is awkward to guarantee deterministically,
	// so instead pick today's own weekday with a window guaranteed not to
	// have passed yet: 23:59.
	saveScheduledPickupConfig(t, st, ScheduledPickupConfig{
		Weekday:                   strings.ToLower(time.Now().Weekday().String()),
		WindowStart:               "23:59",
		WindowEnd:                 "23:59",
		ExceptionalRetrievalDelay: "4h",
		MailboxIDs:                []string{"Mailbox1"},
	})

	results := mode.RequestPickup(context.Background(), eng, []string{"VOLA0001"}, "Admin")
	if len(results) != 1 || !results[0].OK {
		t.Fatalf("expected pickup request to succeed, got %+v", results)
	}
	if results[0].DueAt == nil {
		t.Fatalf("expected a DueAt on a successful pickup request")
	}
	if results[0].DueAt.Before(time.Now()) {
		t.Fatalf("expected DueAt to be in the future, got %v", results[0].DueAt)
	}
	if results[0].RequestID == "" {
		t.Fatalf("expected a non-empty RequestID on a successful pickup request")
	}
}

// TestScheduledPickupModeTickSendsD1RecapOnceAndCompletesDueItems drives
// Tick with a fully controlled `now` (rather than real wall-clock time) so
// the D-1/idempotency/completion logic is deterministic: more than 24h out
// sends nothing, the D-1 window sends exactly one recap naming the pending
// barcode, a second tick still inside that window doesn't duplicate it,
// and once the visit's own due time arrives the same Tick call completes
// the pickup.
func TestScheduledPickupModeTickSendsD1RecapOnceAndCompletesDueItems(t *testing.T) {
	lib := newTestLibrary(t)
	st := newTestStore(t)
	mail := &capturingMailer{}
	eng := NewEngine(lib, st, mail, NewRegistry())
	mode := NewScheduledPickupMode()

	status := lib.Status()
	rackCartridge(t, lib, "VOLA0001", status.Slots[0].Address)

	anchor := time.Date(2024, 1, 1, 8, 0, 0, 0, time.UTC) // a Monday
	saveScheduledPickupConfig(t, st, ScheduledPickupConfig{
		Weekday:                   "thursday",
		WindowStart:               "10:00",
		WindowEnd:                 "14:00",
		ExceptionalRetrievalDelay: "4h",
		MailboxIDs:                []string{"Mailbox1"},
		RecapRecipients:           []string{"ops@example.com"},
	})

	due, err := nextVisitAt(anchor, time.Thursday, "10:00")
	if err != nil {
		t.Fatalf("nextVisitAt: %v", err)
	}
	res := eng.RequestPickupOne("VOLA0001", mode.Name(), due, "", "")
	if !res.OK {
		t.Fatalf("request pickup: %v", res.Error)
	}

	// More than 24h before the visit: no recap yet.
	if err := mode.Tick(context.Background(), eng, due.Add(-25*time.Hour)); err != nil {
		t.Fatalf("tick (D-2): %v", err)
	}
	if mail.count() != 0 {
		t.Fatalf("expected no recap email more than 24h before the visit, got %d sent", mail.count())
	}

	// Within the D-1 window: recap goes out, containing the pending barcode.
	if err := mode.Tick(context.Background(), eng, due.Add(-23*time.Hour)); err != nil {
		t.Fatalf("tick (D-1): %v", err)
	}
	if mail.count() != 1 {
		t.Fatalf("expected exactly one recap email, got %d", mail.count())
	}
	if !strings.Contains(mail.last().Body, "VOLA0001") {
		t.Fatalf("expected the recap body to list the pending pickup, got: %s", mail.last().Body)
	}
	if len(mail.last().To) != 1 || mail.last().To[0] != "ops@example.com" {
		t.Fatalf("expected recap recipients from config, got %v", mail.last().To)
	}

	// A second tick still inside the same D-1 window must not re-send.
	if err := mode.Tick(context.Background(), eng, due.Add(-22*time.Hour)); err != nil {
		t.Fatalf("tick (still D-1): %v", err)
	}
	if mail.count() != 1 {
		t.Fatalf("expected no duplicate recap email, got %d sent", mail.count())
	}

	// Once due, the same Tick call also completes the pickup.
	if err := mode.Tick(context.Background(), eng, due.Add(time.Minute)); err != nil {
		t.Fatalf("tick (past due): %v", err)
	}
	if !containsBarcode(lib.OffsiteVolumes(), "VOLA0001") {
		t.Fatalf("expected the pickup to be completed once due")
	}
}
