package api

import (
	"path/filepath"
	"testing"

	"github.com/swenske/gotochanger/internal/config"
	"github.com/swenske/gotochanger/internal/store"
)

func newSettingsTestStore(t *testing.T) *store.Store {
	t.Helper()
	st := store.New(filepath.Join(t.TempDir(), "state.db"))
	if err := st.Open(); err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SeedDefaults(); err != nil {
		t.Fatalf("seed defaults: %v", err)
	}
	return st
}

// TestSettingsUpdateDoesNotClobberWizardWrittenValues is the regression for
// a silent data-corruption bug. Settings caches the config it was built with
// at daemon startup and persists *every* field of it on any update, but the
// setup wizard writes vtl_name/offsite_location straight to the store,
// bypassing Settings entirely - and nothing ever refreshed that cache
// (reconfigureFromStore refreshes Server.cfg, not this one). So the first
// settings save of any kind, even one touching only the log level, wrote the
// stale boot-time values back over whatever the wizard had stored.
func TestSettingsUpdateDoesNotClobberWizardWrittenValues(t *testing.T) {
	st := newSettingsTestStore(t)

	// Startup: Settings caches a config with no VTL name, which is exactly
	// what a fresh install looks like before the wizard runs.
	cfg := config.Default()
	settings := NewSettings(cfg, nil, nil, nil, st)

	// The wizard writes straight to the store, as UpdateWizardState does.
	if err := st.SetSetting("vtl_name", "VTL0"); err != nil {
		t.Fatalf("set vtl_name: %v", err)
	}
	if err := st.SetSetting("offsite_location", "true"); err != nil {
		t.Fatalf("set offsite_location: %v", err)
	}

	// An unrelated settings save - the request doesn't mention vtl_name or
	// offsite_location at all.
	level := "debug"
	if _, err := settings.Update(UpdateSettingsRequest{LogLevel: &level}); err != nil {
		t.Fatalf("update settings: %v", err)
	}

	if got, _, _ := st.GetSetting("vtl_name"); got != "VTL0" {
		t.Errorf("vtl_name = %q after an unrelated settings save, want %q", got, "VTL0")
	}
	if got, _, _ := st.GetSetting("offsite_location"); got != "true" {
		t.Errorf("offsite_location = %q after an unrelated settings save, want %q", got, "true")
	}
}

// TestSettingsCurrentReflectsWizardWrittenValues covers the read side of the
// same staleness: Admin > Settings and the manual backup's filename both go
// through Current(), and both showed the boot-time value (empty, on a fresh
// install) rather than the name the wizard had just set.
func TestSettingsCurrentReflectsWizardWrittenValues(t *testing.T) {
	st := newSettingsTestStore(t)
	settings := NewSettings(config.Default(), nil, nil, nil, st)

	if err := st.SetSetting("vtl_name", "VTL0"); err != nil {
		t.Fatalf("set vtl_name: %v", err)
	}
	if got := settings.Current().Library.Name; got != "VTL0" {
		t.Fatalf("Current().Library.Name = %q, want %q", got, "VTL0")
	}
}

// TestSettingsUpdateStillPersistsRequestedFields guards the obvious way to
// get the fix wrong: refreshing from the store must not swallow the values
// the request actually asked to change.
func TestSettingsUpdateStillPersistsRequestedFields(t *testing.T) {
	st := newSettingsTestStore(t)
	settings := NewSettings(config.Default(), nil, nil, nil, st)

	name := "RENAMED"
	offsite := true
	capacity := "42GiB"
	if _, err := settings.Update(UpdateSettingsRequest{
		VTLName:         &name,
		OffsiteLocation: &offsite,
		DefaultCapacity: &capacity,
	}); err != nil {
		t.Fatalf("update settings: %v", err)
	}

	if got, _, _ := st.GetSetting("vtl_name"); got != name {
		t.Errorf("vtl_name = %q, want %q", got, name)
	}
	if got, _, _ := st.GetSetting("offsite_location"); got != "true" {
		t.Errorf("offsite_location = %q, want \"true\"", got)
	}
	if got, _, _ := st.GetSetting("default_capacity"); got != capacity {
		t.Errorf("default_capacity = %q, want %q", got, capacity)
	}
}
