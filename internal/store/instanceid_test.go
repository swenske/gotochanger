package store

import (
	"path/filepath"
	"testing"
)

func TestInstanceIDGeneratesAndPersists(t *testing.T) {
	s := newTestStore(t)

	id, err := s.InstanceID()
	if err != nil {
		t.Fatalf("InstanceID: %v", err)
	}
	if id == "" {
		t.Fatal("InstanceID returned empty id")
	}

	again, err := s.InstanceID()
	if err != nil {
		t.Fatalf("InstanceID (second call): %v", err)
	}
	if again != id {
		t.Fatalf("second InstanceID() = %q, want unchanged %q", again, id)
	}
}

func TestInstanceIDSurvivesBackupRestore(t *testing.T) {
	s := newTestStore(t)
	id, err := s.InstanceID()
	if err != nil {
		t.Fatalf("InstanceID: %v", err)
	}

	backupPath := filepath.Join(t.TempDir(), "backup.db")
	if err := s.VacuumSnapshot(backupPath); err != nil {
		t.Fatalf("VacuumSnapshot: %v", err)
	}
	if err := s.Restore(backupPath); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	after, err := s.InstanceID()
	if err != nil {
		t.Fatalf("InstanceID after restore: %v", err)
	}
	if after != id {
		t.Fatalf("InstanceID after restore = %q, want unchanged %q", after, id)
	}
}

func TestSeedDefaultsIdempotentForInstanceID(t *testing.T) {
	s := newTestStore(t)
	if err := s.SeedDefaults(); err != nil {
		t.Fatalf("SeedDefaults (first): %v", err)
	}
	id, ok, err := s.GetSetting(instanceIDSettingKey)
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if !ok || id == "" {
		t.Fatal("SeedDefaults did not seed telemetry_instance_id")
	}

	if err := s.SeedDefaults(); err != nil {
		t.Fatalf("SeedDefaults (second): %v", err)
	}
	again, _, err := s.GetSetting(instanceIDSettingKey)
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if again != id {
		t.Fatalf("telemetry_instance_id changed across SeedDefaults calls: %q -> %q", id, again)
	}
}
