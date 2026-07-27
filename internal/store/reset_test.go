package store

import (
	"time"

	"testing"

	"github.com/swenske/gotochanger/internal/config"
)

// TestResetToFactory populates a store with topology, settings, a user, and
// a token, then verifies ResetToFactory leaves every table genuinely
// empty (not just topology - settings, auth included) and that the
// original database file remains open and usable afterward (same
// expectation Restore already has to meet). It also verifies the
// following boot's SeedDefaults() call - which ResetToFactory
// deliberately does not invoke itself, matching Restore's "the caller
// restarts the process" contract - repopulates the factory catalogs.
func TestResetToFactory(t *testing.T) {
	s := newTestStore(t)

	if err := s.SeedDefaults(); err != nil {
		t.Fatalf("seed defaults: %v", err)
	}
	if err := s.SetSetting("vtl_name", "VTL0"); err != nil {
		t.Fatalf("set vtl_name: %v", err)
	}
	if err := s.SaveMagazines([]config.MagazineConfig{{ID: "M1", Slots: 10}}); err != nil {
		t.Fatalf("save magazines: %v", err)
	}
	if err := s.CreateUserRow(config.UserRow{
		Username: "Admin", PasswordHash: "hash", Role: "admin",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create user row: %v", err)
	}
	if err := s.CreateTokenRow(config.TokenRow{
		Name: "tok1", Hash: "hash1", Role: "operator", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create token row: %v", err)
	}

	dtsBefore, err := s.ListDriveTypes()
	if err != nil {
		t.Fatalf("list drive types before reset: %v", err)
	}
	if len(dtsBefore) == 0 {
		t.Fatal("expected SeedDefaults to have seeded drive types before reset")
	}

	if err := s.ResetToFactory(); err != nil {
		t.Fatalf("reset to factory: %v", err)
	}

	if mags, err := s.ListMagazines(); err != nil {
		t.Fatalf("list magazines after reset: %v", err)
	} else if len(mags) != 0 {
		t.Fatalf("expected no magazines after reset, got %+v", mags)
	}

	if name, ok, err := s.GetSetting("vtl_name"); err != nil {
		t.Fatalf("get vtl_name after reset: %v", err)
	} else if ok {
		t.Fatalf("expected vtl_name unset after reset, got %q", name)
	}

	if users, err := s.ListUserRows(); err != nil {
		t.Fatalf("list users after reset: %v", err)
	} else if len(users) != 0 {
		t.Fatalf("expected no users after reset, got %+v", users)
	}

	if toks, err := s.ListTokenRows(); err != nil {
		t.Fatalf("list tokens after reset: %v", err)
	} else if len(toks) != 0 {
		t.Fatalf("expected no tokens after reset, got %+v", toks)
	}

	if dts, err := s.ListDriveTypes(); err != nil {
		t.Fatalf("list drive types after reset: %v", err)
	} else if len(dts) != 0 {
		t.Fatalf("expected drive type catalog empty immediately after reset (before the next boot's SeedDefaults runs), got %+v", dts)
	}

	// Simulate the next daemon boot: SeedDefaults is what actually
	// repopulates the catalog, exactly as it would for a brand new install.
	if err := s.SeedDefaults(); err != nil {
		t.Fatalf("seed defaults after reset: %v", err)
	}
	dtsAfter, err := s.ListDriveTypes()
	if err != nil {
		t.Fatalf("list drive types after post-reset seed: %v", err)
	}
	if len(dtsAfter) != len(dtsBefore) {
		t.Fatalf("expected drive type catalog to be repopulated to %d entries, got %d", len(dtsBefore), len(dtsAfter))
	}
}
