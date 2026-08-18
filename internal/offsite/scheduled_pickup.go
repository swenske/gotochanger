package offsite

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/swenske/gotochanger/internal/config"
	"github.com/swenske/gotochanger/internal/library"
	"github.com/swenske/gotochanger/internal/mailer"
)

// ScheduledPickupConfig holds Mode 1's parameters. RecapRecipients and
// explicit WindowStart/WindowEnd fields are necessary additions beyond the
// feature request's literal parameter list (day, window, retrieval delay,
// mailboxes) - an email needs recipients, and a window needs two concrete
// times, not one string. There is deliberately no PIN field: mailbox PINs
// in this codebase are one-way hashes (see config.MailboxConfig.PINHash),
// which can't be printed in a recap email, and reversing that convention
// just for this feature was explicitly declined.
type ScheduledPickupConfig struct {
	Weekday                   string   `json:"weekday"`      // "monday".."sunday"
	WindowStart               string   `json:"window_start"` // "HH:MM"
	WindowEnd                 string   `json:"window_end"`   // "HH:MM", informational/recap-email only
	ExceptionalRetrievalDelay string   `json:"exceptional_retrieval_delay"`
	MailboxIDs                []string `json:"mailbox_ids"`
	RecapRecipients           []string `json:"recap_recipients"`
}

// ScheduledPickupMode simulates a vendor who visits on a fixed weekday and
// time window to pick up and drop off tapes, with a D-1 recap email and an
// exceptional (emergency, out-of-cycle) retrieval delay for early returns.
type ScheduledPickupMode struct{}

func NewScheduledPickupMode() *ScheduledPickupMode { return &ScheduledPickupMode{} }

func (m *ScheduledPickupMode) Name() string  { return "scheduled_pickup" }
func (m *ScheduledPickupMode) Label() string { return "Scheduled Pickup" }
func (m *ScheduledPickupMode) Describe() string {
	return "A vendor visits on a fixed weekday and time window to pick up and drop off tapes. A recap email lists what's involved the day before the visit."
}

func (m *ScheduledPickupMode) ParameterSchema() []ParameterField {
	return []ParameterField{
		{Key: "weekday", Label: "Visit day", Type: ParameterTypeWeekday, Required: true},
		{Key: "window_start", Label: "Visit window start", Type: ParameterTypeTimeOfDay, Required: true, Placeholder: "10:00"},
		{Key: "window_end", Label: "Visit window end", Type: ParameterTypeTimeOfDay, Required: true, Placeholder: "14:00"},
		{Key: "exceptional_retrieval_delay", Label: "Emergency retrieval delay", Type: ParameterTypeDuration, Required: true,
			Help: `How long an emergency out-of-cycle return takes, e.g. "4h".`},
		{Key: "mailbox_ids", Label: "Mailboxes", Type: ParameterTypeMailboxes, Required: true,
			Help: "Where the vendor finds tapes to pick up and drops off returns."},
		{Key: "recap_recipients", Label: "Recap email recipients", Type: ParameterTypeEmailList, Required: false},
	}
}

func (m *ScheduledPickupMode) DefaultConfig() json.RawMessage {
	raw, _ := json.Marshal(ScheduledPickupConfig{
		Weekday:                   "thursday",
		WindowStart:               "10:00",
		WindowEnd:                 "14:00",
		ExceptionalRetrievalDelay: "4h",
	})
	return raw
}

func (m *ScheduledPickupMode) ValidateConfig(raw json.RawMessage) error {
	var cfg ScheduledPickupConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("invalid scheduled_pickup config: %w", err)
	}
	if _, err := parseWeekday(cfg.Weekday); err != nil {
		return err
	}
	if _, _, err := parseTimeOfDay(cfg.WindowStart); err != nil {
		return fmt.Errorf("window_start: %w", err)
	}
	if _, _, err := parseTimeOfDay(cfg.WindowEnd); err != nil {
		return fmt.Errorf("window_end: %w", err)
	}
	if _, err := config.ParseDuration(cfg.ExceptionalRetrievalDelay); err != nil {
		return fmt.Errorf("exceptional_retrieval_delay: %w", err)
	}
	if len(cfg.MailboxIDs) == 0 {
		return fmt.Errorf("mailbox_ids must include at least one mailbox")
	}
	return nil
}

func (m *ScheduledPickupMode) RequestPickup(ctx context.Context, eng *Engine, barcodes []string, actor string) []ItemResult {
	cfg, err := m.loadConfig(eng)
	if err != nil {
		return unsupportedResults(barcodes, err.Error())
	}
	due, err := nextVisitAt(time.Now(), cfg.weekday, cfg.WindowStart)
	if err != nil {
		return unsupportedResults(barcodes, err.Error())
	}
	_, results := eng.RequestPickupBatch(barcodes, m.Name(), due, actor)
	return results
}

func (m *ScheduledPickupMode) RequestReturn(ctx context.Context, eng *Engine, barcodes []string, urgent bool, actor string) []ItemResult {
	cfg, err := m.loadConfig(eng)
	if err != nil {
		return unsupportedResults(barcodes, err.Error())
	}
	var due time.Time
	if urgent {
		delay, err := config.ParseDuration(cfg.ExceptionalRetrievalDelay)
		if err != nil {
			return unsupportedResults(barcodes, err.Error())
		}
		due = time.Now().Add(delay)
	} else {
		due, err = nextVisitAt(time.Now(), cfg.weekday, cfg.WindowStart)
		if err != nil {
			return unsupportedResults(barcodes, err.Error())
		}
	}
	_, results := eng.RequestReturnBatch(barcodes, m.Name(), due, urgent, actor)
	return results
}

// Tick completes anything due, then - independently - checks whether the
// next visit's D-1 recap email is due to go out. Completion runs even if
// the saved config has since become unloadable, so pending items are
// never stranded by a config change; the recap check simply no-ops in
// that case (nothing new to schedule).
func (m *ScheduledPickupMode) Tick(ctx context.Context, eng *Engine, now time.Time) error {
	if !eng.lib.OffsiteEnabled() {
		return nil
	}
	eng.CompleteDueTransfers(m.Name(), now)

	cfg, err := m.loadConfig(eng)
	if err != nil {
		return nil
	}
	due, err := nextVisitAt(now, cfg.weekday, cfg.WindowStart)
	if err != nil {
		return nil
	}
	// nextVisitAt always returns an instant >= now, so due is guaranteed
	// to still be in the future here - only the D-1 threshold needs
	// checking.
	if now.Before(due.Add(-24 * time.Hour)) {
		return nil
	}
	lastSentRaw, _, _ := eng.store.GetSetting("offsite_recap_last_sent_for")
	if lastSentRaw == due.UTC().Format(time.RFC3339) {
		return nil // already sent for this specific visit
	}
	if err := sendRecapEmail(eng, m.Name(), cfg.ScheduledPickupConfig, due); err != nil {
		eng.lib.RecordEvent(library.Event{
			Code:    library.EventCodeOffsiteRecapEmailFailure,
			Message: "failed to send offsite recap email",
			Detail:  map[string]string{"error": err.Error()},
		})
		return err
	}
	return eng.store.SetSetting("offsite_recap_last_sent_for", due.UTC().Format(time.RFC3339))
}

// RecapPreview renders what the next scheduled-pickup recap email would
// contain right now, without sending it - used by both the D-1 send path
// (via sendRecapEmail) and GET /api/v1/offsite/recap-preview, so the two
// can never drift from each other.
func (m *ScheduledPickupMode) RecapPreview(eng *Engine, now time.Time) (mailer.Message, error) {
	cfg, err := m.loadConfig(eng)
	if err != nil {
		return mailer.Message{}, err
	}
	due, err := nextVisitAt(now, cfg.weekday, cfg.WindowStart)
	if err != nil {
		return mailer.Message{}, err
	}
	return renderRecapEmail(eng, m.Name(), cfg.ScheduledPickupConfig, due), nil
}

// resolvedScheduledPickupConfig is ScheduledPickupConfig with its weekday
// pre-parsed, since every call site above needs it as a time.Weekday.
type resolvedScheduledPickupConfig struct {
	ScheduledPickupConfig
	weekday time.Weekday
}

func (m *ScheduledPickupMode) loadConfig(eng *Engine) (resolvedScheduledPickupConfig, error) {
	raw, ok, err := eng.store.GetOffsiteModeConfig(m.Name())
	if err != nil {
		return resolvedScheduledPickupConfig{}, err
	}
	if !ok {
		return resolvedScheduledPickupConfig{}, fmt.Errorf("scheduled_pickup is not configured yet")
	}
	var cfg ScheduledPickupConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return resolvedScheduledPickupConfig{}, fmt.Errorf("stored scheduled_pickup config is invalid: %w", err)
	}
	wd, err := parseWeekday(cfg.Weekday)
	if err != nil {
		return resolvedScheduledPickupConfig{}, err
	}
	return resolvedScheduledPickupConfig{ScheduledPickupConfig: cfg, weekday: wd}, nil
}

var weekdayByName = map[string]time.Weekday{
	"sunday":    time.Sunday,
	"monday":    time.Monday,
	"tuesday":   time.Tuesday,
	"wednesday": time.Wednesday,
	"thursday":  time.Thursday,
	"friday":    time.Friday,
	"saturday":  time.Saturday,
}

func parseWeekday(s string) (time.Weekday, error) {
	wd, ok := weekdayByName[strings.ToLower(s)]
	if !ok {
		return 0, fmt.Errorf("invalid weekday %q", s)
	}
	return wd, nil
}

func parseTimeOfDay(s string) (hour, minute int, err error) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid time %q, want HH:MM", s)
	}
	hour, err = strconv.Atoi(parts[0])
	if err != nil || hour < 0 || hour > 23 {
		return 0, 0, fmt.Errorf("invalid time %q, want HH:MM", s)
	}
	minute, err = strconv.Atoi(parts[1])
	if err != nil || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("invalid time %q, want HH:MM", s)
	}
	return hour, minute, nil
}

// nextVisitAt returns the next instant at or after now whose weekday and
// clock time match wd/windowStart - written from scratch, no day-of-week/
// cron parser exists anywhere else in this codebase.
func nextVisitAt(now time.Time, wd time.Weekday, windowStart string) (time.Time, error) {
	h, mi, err := parseTimeOfDay(windowStart)
	if err != nil {
		return time.Time{}, err
	}
	loc := now.Location()
	candidate := time.Date(now.Year(), now.Month(), now.Day(), h, mi, 0, 0, loc)
	daysUntil := (int(wd) - int(now.Weekday()) + 7) % 7
	candidate = candidate.AddDate(0, 0, daysUntil)
	if candidate.Before(now) {
		candidate = candidate.AddDate(0, 0, 7)
	}
	return candidate, nil
}
