package store

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

// openStoreWithLegacyOffsiteRotation mirrors
// openStoreWithLegacyLatencyProfile: seeds a raw config table with the
// pre-mode-architecture offsite_rotation_interval/offsite_rotation_count
// keys before Open() (and therefore migrateLegacyOffsiteRotationSetting)
// ever runs over them.
func openStoreWithLegacyOffsiteRotation(t *testing.T, interval, count string) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")

	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE config (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatalf("create legacy config table: %v", err)
	}
	if interval != "" {
		if _, err := db.Exec(`INSERT INTO config (key, value) VALUES ('offsite_rotation_interval', ?)`, interval); err != nil {
			t.Fatalf("seed legacy offsite_rotation_interval: %v", err)
		}
		if _, err := db.Exec(`INSERT INTO config (key, value) VALUES ('offsite_rotation_count', ?)`, count); err != nil {
			t.Fatalf("seed legacy offsite_rotation_count: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	s := New(path)
	if err := s.Open(); err != nil {
		t.Fatalf("open store over legacy schema: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestMigrateLegacyOffsiteRotationSettingTranslatesExistingInterval(t *testing.T) {
	s := openStoreWithLegacyOffsiteRotation(t, "24h", "2")

	active, err := s.GetOffsiteActiveMode()
	if err != nil {
		t.Fatalf("get active mode: %v", err)
	}
	if active != "continuous_rotation" {
		t.Fatalf("expected continuous_rotation to become the active mode, got %q", active)
	}
	raw, ok, err := s.GetOffsiteModeConfig("continuous_rotation")
	if err != nil || !ok {
		t.Fatalf("get continuous_rotation config: ok=%v err=%v", ok, err)
	}
	var cfg struct {
		Interval string `json:"interval"`
		Count    int    `json:"count"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal migrated config: %v", err)
	}
	if cfg.Interval != "24h" || cfg.Count != 2 {
		t.Fatalf("expected migrated config {24h, 2}, got %+v", cfg)
	}
}

func TestMigrateLegacyOffsiteRotationSettingNoPriorValueStaysUnset(t *testing.T) {
	s := openStoreWithLegacyOffsiteRotation(t, "", "")
	active, err := s.GetOffsiteActiveMode()
	if err != nil {
		t.Fatalf("get active mode: %v", err)
	}
	if active != "" {
		t.Fatalf("expected no active mode with no legacy rotation setting present, got %q", active)
	}
}

func TestMigrateLegacyOffsiteRotationSettingIsIdempotent(t *testing.T) {
	// Once migrated (or once explicitly set to "" by an admin who never
	// had legacy rotation configured), a later Open() (e.g. a daemon
	// restart) must not re-derive from the legacy keys again and clobber
	// whatever mode an admin has since picked.
	s := openStoreWithLegacyOffsiteRotation(t, "24h", "2")

	if err := s.SetOffsiteActiveMode("scheduled_pickup"); err != nil {
		t.Fatalf("set active mode: %v", err)
	}
	if err := s.initTopologySchema(); err != nil {
		t.Fatalf("re-run topology schema init: %v", err)
	}
	active, err := s.GetOffsiteActiveMode()
	if err != nil {
		t.Fatalf("get active mode: %v", err)
	}
	if active != "scheduled_pickup" {
		t.Fatalf("expected the admin's later choice to survive re-migration, got %q", active)
	}
}

func TestOffsiteActiveModeRoundTrip(t *testing.T) {
	s := newTestStore(t)
	if active, err := s.GetOffsiteActiveMode(); err != nil || active != "" {
		t.Fatalf("expected no active mode on a fresh store, got %q (err %v)", active, err)
	}
	if err := s.SetOffsiteActiveMode("scheduled_pickup"); err != nil {
		t.Fatalf("set active mode: %v", err)
	}
	if active, err := s.GetOffsiteActiveMode(); err != nil || active != "scheduled_pickup" {
		t.Fatalf("expected scheduled_pickup, got %q (err %v)", active, err)
	}
}

func TestOffsiteModeConfigRoundTrip(t *testing.T) {
	s := newTestStore(t)
	if _, ok, err := s.GetOffsiteModeConfig("scheduled_pickup"); err != nil || ok {
		t.Fatalf("expected no config on a fresh store, got ok=%v err=%v", ok, err)
	}
	raw := json.RawMessage(`{"weekday":"thursday","mailbox_ids":["Mailbox1"]}`)
	if err := s.SetOffsiteModeConfig("scheduled_pickup", raw); err != nil {
		t.Fatalf("set config: %v", err)
	}
	got, ok, err := s.GetOffsiteModeConfig("scheduled_pickup")
	if err != nil || !ok {
		t.Fatalf("get config: ok=%v err=%v", ok, err)
	}
	if string(got) != string(raw) {
		t.Fatalf("got %s, want %s", got, raw)
	}
}

func TestRecordAndListOffsiteMovements(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	movements := []OffsiteMovement{
		{Time: base, Barcode: "VOLA0001", Direction: "pickup_requested", Status: "pending_pickup", Mode: "scheduled_pickup"},
		{Time: base.Add(time.Hour), Barcode: "VOLA0001", Direction: "sent_offsite", Status: "vaulted", Mode: "scheduled_pickup"},
		{Time: base.Add(2 * time.Hour), Barcode: "VOLB0001", Direction: "sent_offsite", Status: "vaulted", Mode: "continuous_rotation"},
	}
	for _, m := range movements {
		if err := s.RecordOffsiteMovement(m); err != nil {
			t.Fatalf("record movement %+v: %v", m, err)
		}
	}

	all, err := s.ListOffsiteMovements("", 10)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 movements, got %d", len(all))
	}
	// Newest first.
	if all[0].Barcode != "VOLB0001" || all[2].Barcode != "VOLA0001" || all[2].Direction != "pickup_requested" {
		t.Fatalf("expected newest-first ordering, got %+v", all)
	}

	filtered, err := s.ListOffsiteMovements("VOLA0001", 10)
	if err != nil {
		t.Fatalf("list filtered: %v", err)
	}
	if len(filtered) != 2 {
		t.Fatalf("expected 2 movements for VOLA0001, got %d: %+v", len(filtered), filtered)
	}

	limited, err := s.ListOffsiteMovements("", 1)
	if err != nil {
		t.Fatalf("list with limit: %v", err)
	}
	if len(limited) != 1 || limited[0].Barcode != "VOLB0001" {
		t.Fatalf("expected limit=1 to return only the newest movement, got %+v", limited)
	}
}

func TestRecordAndListOffsiteMovementsRoundTripsRequestID(t *testing.T) {
	s := newTestStore(t)
	if err := s.RecordOffsiteMovement(OffsiteMovement{
		Barcode: "VOLA0001", Direction: "pickup_requested", Status: "pending_pickup", Mode: "scheduled_pickup", RequestID: "req-abc123",
	}); err != nil {
		t.Fatalf("record movement: %v", err)
	}
	if err := s.RecordOffsiteMovement(OffsiteMovement{
		Barcode: "VOLB0001", Direction: "sent_offsite", Status: "vaulted", Mode: "continuous_rotation",
	}); err != nil {
		t.Fatalf("record movement with no request id: %v", err)
	}

	all, err := s.ListOffsiteMovements("", 10)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 movements, got %d", len(all))
	}
	// Newest first: VOLB0001 (no request id) then VOLA0001 (req-abc123).
	if all[0].RequestID != "" {
		t.Fatalf("expected no request id on a manual/automatic movement, got %q", all[0].RequestID)
	}
	if all[1].RequestID != "req-abc123" {
		t.Fatalf("expected request id req-abc123, got %q", all[1].RequestID)
	}
}

func TestOffsiteRequestCRUDRoundTrip(t *testing.T) {
	s := newTestStore(t)
	created := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	req := OffsiteRequest{
		ID: "req-abc123", Kind: "pickup", Mode: "scheduled_pickup",
		Barcodes: []string{"VOLA0001", "VOLA0002"}, CreatedAt: created, Actor: "Admin",
	}
	if err := s.CreateOffsiteRequest(req); err != nil {
		t.Fatalf("create offsite request: %v", err)
	}

	got, ok, err := s.GetOffsiteRequest("req-abc123")
	if err != nil || !ok {
		t.Fatalf("get offsite request: ok=%v err=%v", ok, err)
	}
	if got.Kind != "pickup" || got.Mode != "scheduled_pickup" || got.Actor != "Admin" {
		t.Fatalf("unexpected request fields: %+v", got)
	}
	if len(got.Barcodes) != 2 || got.Barcodes[0] != "VOLA0001" || got.Barcodes[1] != "VOLA0002" {
		t.Fatalf("expected both barcodes in insertion order, got %+v", got.Barcodes)
	}
	if got.Urgent {
		t.Fatalf("expected urgent=false, got true")
	}
	if got.CancelledAt != nil {
		t.Fatalf("expected a freshly created request to have no cancelled_at, got %v", got.CancelledAt)
	}
	if !got.CreatedAt.Equal(created) {
		t.Fatalf("expected created_at %v, got %v", created, got.CreatedAt)
	}
}

func TestGetOffsiteRequestUnknownIDReturnsNotOK(t *testing.T) {
	s := newTestStore(t)
	_, ok, err := s.GetOffsiteRequest("does-not-exist")
	if err != nil {
		t.Fatalf("expected no error for an unknown id, got %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for an unknown id")
	}
}

func TestListOffsiteRequestsOrdersNewestFirst(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	reqs := []OffsiteRequest{
		{ID: "req-1", Kind: "pickup", Mode: "scheduled_pickup", Barcodes: []string{"VOLA0001"}, CreatedAt: base},
		{ID: "req-2", Kind: "return", Mode: "scheduled_pickup", Barcodes: []string{"VOLB0001"}, Urgent: true, CreatedAt: base.Add(time.Hour)},
	}
	for _, r := range reqs {
		if err := s.CreateOffsiteRequest(r); err != nil {
			t.Fatalf("create offsite request %s: %v", r.ID, err)
		}
	}

	got, err := s.ListOffsiteRequests(10)
	if err != nil {
		t.Fatalf("list offsite requests: %v", err)
	}
	if len(got) != 2 || got[0].ID != "req-2" || got[1].ID != "req-1" {
		t.Fatalf("expected newest-first ordering [req-2, req-1], got %+v", got)
	}
	if !got[0].Urgent {
		t.Fatalf("expected req-2 to be urgent, got %+v", got[0])
	}
}

func TestCancelOffsiteRequestRecordIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateOffsiteRequest(OffsiteRequest{
		ID: "req-abc123", Kind: "pickup", Mode: "scheduled_pickup", Barcodes: []string{"VOLA0001"}, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create offsite request: %v", err)
	}

	// Zero nanoseconds: cancelled_at round-trips through time.RFC3339
	// (second precision), matching RecordOffsiteMovement's own existing
	// choice for this same table family.
	cancelledAt := time.Date(2024, 6, 1, 8, 30, 0, 0, time.UTC)
	flipped, err := s.CancelOffsiteRequestRecord("req-abc123", cancelledAt)
	if err != nil {
		t.Fatalf("cancel offsite request: %v", err)
	}
	if !flipped {
		t.Fatal("expected the first cancel to flip the row")
	}
	got, ok, err := s.GetOffsiteRequest("req-abc123")
	if err != nil || !ok {
		t.Fatalf("get offsite request: ok=%v err=%v", ok, err)
	}
	if got.CancelledAt == nil || !got.CancelledAt.Equal(cancelledAt) {
		t.Fatalf("expected cancelled_at %v, got %v", cancelledAt, got.CancelledAt)
	}

	flipped, err = s.CancelOffsiteRequestRecord("req-abc123", time.Now().UTC())
	if err != nil {
		t.Fatalf("second cancel: %v", err)
	}
	if flipped {
		t.Fatal("expected a second cancel of an already-cancelled request to be a no-op")
	}
}

func TestCancelOffsiteRequestRecordUnknownIDReturnsNotFlipped(t *testing.T) {
	s := newTestStore(t)
	flipped, err := s.CancelOffsiteRequestRecord("does-not-exist", time.Now().UTC())
	if err != nil {
		t.Fatalf("expected no error for an unknown id, got %v", err)
	}
	if flipped {
		t.Fatal("expected flipped=false for an unknown id")
	}
}

// TestMigrateOffsiteMovementsSchemaBackfillsExistingRows simulates
// upgrading a database created before cancelable offsite requests
// existed: a bare pre-request_id row must survive Open() with an empty
// RequestID, not an error, and re-running the migration must be a no-op -
// mirrors TestMigrateDriveDevicesSchemaBackfillsExistingRows's exact
// shape.
func TestMigrateOffsiteMovementsSchemaBackfillsExistingRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE offsite_movements (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		time TEXT NOT NULL,
		barcode TEXT NOT NULL,
		direction TEXT NOT NULL,
		status TEXT NOT NULL,
		mode TEXT NOT NULL,
		mailbox_id TEXT NOT NULL DEFAULT '',
		actor TEXT NOT NULL DEFAULT '',
		detail TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO offsite_movements (time, barcode, direction, status, mode) VALUES ('2024-01-01T00:00:00Z', 'VOLA0001', 'sent_offsite', 'vaulted', 'continuous_rotation')`); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	s := New(path)
	if err := s.Open(); err != nil {
		t.Fatalf("open store over legacy schema: %v", err)
	}
	defer s.Close()

	movements, err := s.ListOffsiteMovements("", 10)
	if err != nil {
		t.Fatalf("list offsite movements: %v", err)
	}
	if len(movements) != 1 || movements[0].Barcode != "VOLA0001" || movements[0].RequestID != "" {
		t.Fatalf("expected legacy row to survive migration with an empty request_id, got %+v", movements)
	}

	// Re-running must be a no-op (idempotent ALTER TABLE guard).
	if err := s.migrateOffsiteMovementsSchema(); err != nil {
		t.Fatalf("re-running migration should be a no-op, got: %v", err)
	}
}
