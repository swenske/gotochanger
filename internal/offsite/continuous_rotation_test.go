package offsite

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/swenske/gotochanger/internal/mailer"
)

func TestContinuousRotationModeTickRespectsIntervalAndPersistsLastRun(t *testing.T) {
	lib := newTestLibrary(t)
	st := newTestStore(t)
	eng := NewEngine(lib, st, mailer.NoopMailer{}, NewRegistry())
	mode := NewContinuousRotationMode()

	raw, _ := json.Marshal(ContinuousRotationConfig{IntervalRaw: "1h", Count: 1})
	if err := st.SetOffsiteModeConfig(mode.Name(), raw); err != nil {
		t.Fatalf("save continuous_rotation config: %v", err)
	}

	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := mode.Tick(context.Background(), eng, t0); err != nil {
		t.Fatalf("tick at t0: %v", err)
	}
	lastRun, ok, err := st.GetSetting("offsite_mode_continuous_rotation_last_run")
	if err != nil || !ok || lastRun != t0.UTC().Format(time.RFC3339) {
		t.Fatalf("expected last-run to be persisted as t0, got %q (ok=%v, err=%v)", lastRun, ok, err)
	}

	// 30 minutes later, well inside the 1h interval: must not re-fire (and
	// therefore must not overwrite last-run).
	t1 := t0.Add(30 * time.Minute)
	if err := mode.Tick(context.Background(), eng, t1); err != nil {
		t.Fatalf("tick at t1: %v", err)
	}
	lastRun2, _, _ := st.GetSetting("offsite_mode_continuous_rotation_last_run")
	if lastRun2 != lastRun {
		t.Fatalf("expected last-run to stay at t0 before the interval elapses, got %q", lastRun2)
	}

	// Past the interval: fires again, advancing last-run.
	t2 := t0.Add(90 * time.Minute)
	if err := mode.Tick(context.Background(), eng, t2); err != nil {
		t.Fatalf("tick at t2: %v", err)
	}
	lastRun3, _, _ := st.GetSetting("offsite_mode_continuous_rotation_last_run")
	if lastRun3 != t2.UTC().Format(time.RFC3339) {
		t.Fatalf("expected last-run to advance to t2 once the interval elapsed, got %q", lastRun3)
	}
}

func TestContinuousRotationModeTickNoOpsWithoutConfig(t *testing.T) {
	lib := newTestLibrary(t)
	st := newTestStore(t)
	eng := NewEngine(lib, st, mailer.NoopMailer{}, NewRegistry())
	mode := NewContinuousRotationMode()

	// No config saved at all: Tick must be a clean no-op, not an error.
	if err := mode.Tick(context.Background(), eng, time.Now()); err != nil {
		t.Fatalf("expected no error ticking an unconfigured mode, got %v", err)
	}
	if _, ok, _ := st.GetSetting("offsite_mode_continuous_rotation_last_run"); ok {
		t.Fatalf("expected no last-run to be recorded for an unconfigured mode")
	}
}

func TestContinuousRotationModeHasNoManualRequestPath(t *testing.T) {
	eng := &Engine{} // never dereferenced - this mode never touches eng
	mode := NewContinuousRotationMode()

	results := mode.RequestPickup(context.Background(), eng, []string{"VOLA0001"}, "Admin")
	if len(results) != 1 || results[0].OK {
		t.Fatalf("expected continuous rotation to reject a manual pickup request, got %+v", results)
	}
	results = mode.RequestReturn(context.Background(), eng, []string{"VOLA0001"}, false, "Admin")
	if len(results) != 1 || results[0].OK {
		t.Fatalf("expected continuous rotation to reject a manual return request, got %+v", results)
	}
}
