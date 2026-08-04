package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/swenske/gotochanger/internal/config"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s := New(filepath.Join(t.TempDir(), "state.db"))
	if err := s.Open(); err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestTapeTypeCRUDRoundTrip(t *testing.T) {
	s := newTestStore(t)
	tt := config.TapeType{Name: "LTO-8", Capacity: "12TB", Description: "test", BarcodeFamily: "lto", MediaID: "L8", VolSerLength: 6}
	if err := s.CreateTapeType(tt); err != nil {
		t.Fatalf("create tape type: %v", err)
	}
	got, err := s.ListTapeTypes()
	if err != nil {
		t.Fatalf("list tape types: %v", err)
	}
	if len(got) != 1 || got[0].BarcodeFamily != "lto" || got[0].MediaID != "L8" || got[0].VolSerLength != 6 {
		t.Fatalf("unexpected tape types: %+v", got)
	}

	tt.MediaID = "L9"
	if err := s.UpdateTapeType("LTO-8", tt); err != nil {
		t.Fatalf("update tape type: %v", err)
	}
	got, err = s.ListTapeTypes()
	if err != nil {
		t.Fatalf("list tape types after update: %v", err)
	}
	if got[0].MediaID != "L9" {
		t.Fatalf("expected updated media_id L9, got %q", got[0].MediaID)
	}

	if err := s.DeleteTapeType("LTO-8"); err != nil {
		t.Fatalf("delete tape type: %v", err)
	}
	got, err = s.ListTapeTypes()
	if err != nil {
		t.Fatalf("list tape types after delete: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no tape types after delete, got %+v", got)
	}
}

// TestMigrateTapeTypesSchemaBackfillsExistingRows simulates upgrading a
// database created before the barcode-format columns existed: a bare
// (name, capacity, description) row must survive Open() and come back with
// permissive "generic" backfill defaults, not an error.
func TestMigrateTapeTypesSchemaBackfillsExistingRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE tape_types (name TEXT PRIMARY KEY, capacity TEXT NOT NULL, description TEXT NOT NULL)`); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO tape_types (name, capacity, description) VALUES ('Legacy', '1TB', 'pre-existing row')`); err != nil {
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

	tts, err := s.ListTapeTypes()
	if err != nil {
		t.Fatalf("list tape types: %v", err)
	}
	var legacy *config.TapeType
	for i := range tts {
		if tts[i].Name == "Legacy" {
			legacy = &tts[i]
		}
	}
	if legacy == nil {
		t.Fatalf("expected legacy row to survive migration, got %+v", tts)
	}
	if legacy.BarcodeFamily != "generic" || legacy.MediaID != "" || legacy.VolSerLength != 8 {
		t.Fatalf("expected permissive generic backfill defaults, got %+v", legacy)
	}

	// Re-opening again must be a no-op (idempotent ALTER TABLE guard).
	if err := s.migrateTapeTypesSchema(); err != nil {
		t.Fatalf("re-running migration should be a no-op, got: %v", err)
	}
}

// TestMigrateTapeTypesSchemaRepairsKnownLegacyNames reproduces the bug
// found on the real bareos-disk-sd-int-fr1 deployment: a tape-type row
// named "LTO-8" that existed before the barcode-format columns did (every
// install prior to this feature seeded exactly this name via the old
// 5-entry default catalog) must come back with the *real* LTO barcode
// format after Open(), not the generic backfill - otherwise cartridges
// created against it get plain numeric barcodes instead of the real
// "<6 digits>L8" format.
func TestMigrateTapeTypesSchemaRepairsKnownLegacyNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE tape_types (name TEXT PRIMARY KEY, capacity TEXT NOT NULL, description TEXT NOT NULL)`); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	// Exactly what every pre-0.4.0 install had: the old 5-entry default
	// catalog, no barcode-format columns.
	for _, row := range []struct{ name, capacity, description string }{
		{"Unlimited", "unlimited", "Unlimited capacity, limited only by disk"},
		{"LTO-8", "12TB", "LTO-8 cartridge"},
		{"LTO-9", "18TB", "LTO-9 cartridge"},
		{"DDS", "80GB", "DDS (DAT) cartridge"},
		{"DLT", "40GB", "DLT cartridge"},
		{"MyCustomType", "1TB", "an admin-created type unrelated to the catalog"},
	} {
		if _, err := db.Exec(`INSERT INTO tape_types (name, capacity, description) VALUES (?, ?, ?)`, row.name, row.capacity, row.description); err != nil {
			t.Fatalf("seed legacy row %s: %v", row.name, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	s := New(path)
	if err := s.Open(); err != nil {
		t.Fatalf("open store over legacy schema: %v", err)
	}
	defer s.Close()

	tts, err := s.ListTapeTypes()
	if err != nil {
		t.Fatalf("list tape types: %v", err)
	}
	byName := make(map[string]config.TapeType, len(tts))
	for _, tt := range tts {
		byName[tt.Name] = tt
	}

	cases := []struct {
		name                    string
		wantFamily, wantMediaID string
		wantVolSer              int
	}{
		{"LTO-8", "lto", "L8", 6},
		{"LTO-9", "lto", "L9", 6},
		{"DDS", "dds", "D6", 6},         // bare legacy name, no longer in the current catalog - mapped via legacyTapeTypeBarcodeFormats
		{"DLT", "dlt", "4", 6},          // same
		{"Unlimited", "generic", "", 8}, // was already correct; must stay a no-op
	}
	for _, c := range cases {
		tt, ok := byName[c.name]
		if !ok {
			t.Fatalf("expected tape type %s to survive migration", c.name)
		}
		if tt.BarcodeFamily != c.wantFamily || tt.MediaID != c.wantMediaID || tt.VolSerLength != c.wantVolSer {
			t.Errorf("%s: got {family=%q media_id=%q volser=%d}, want {family=%q media_id=%q volser=%d}",
				c.name, tt.BarcodeFamily, tt.MediaID, tt.VolSerLength, c.wantFamily, c.wantMediaID, c.wantVolSer)
		}
	}

	// A row whose name doesn't match anything in the shipped catalog has
	// no better information available and must stay generic/permissive.
	custom, ok := byName["MyCustomType"]
	if !ok {
		t.Fatalf("expected MyCustomType to survive migration")
	}
	if custom.BarcodeFamily != "generic" || custom.MediaID != "" || custom.VolSerLength != 8 {
		t.Errorf("MyCustomType should stay generic, got %+v", custom)
	}
}

func TestDriveTypeCRUDRoundTrip(t *testing.T) {
	s := newTestStore(t)
	dt := config.DriveType{Name: "LTO-9", Speed: "400MB/s", Capacity: "18TB", Description: "test", Model: "LTO Ultrium 9", Generation: "LTO-9", SCSIIdentity: config.SCSIIdentityRealistic}
	if err := s.CreateDriveType(dt); err != nil {
		t.Fatalf("create drive type: %v", err)
	}
	got, err := s.ListDriveTypes()
	if err != nil {
		t.Fatalf("list drive types: %v", err)
	}
	if len(got) != 1 || got[0].Model != "LTO Ultrium 9" || got[0].Generation != "LTO-9" || got[0].SCSIIdentity != config.SCSIIdentityRealistic {
		t.Fatalf("unexpected drive types: %+v", got)
	}

	dt.Model = "LTO Ultrium 9 (updated)"
	dt.SCSIIdentity = ""
	if err := s.UpdateDriveType("LTO-9", dt); err != nil {
		t.Fatalf("update drive type: %v", err)
	}
	got, err = s.ListDriveTypes()
	if err != nil {
		t.Fatalf("list drive types after update: %v", err)
	}
	if got[0].Model != "LTO Ultrium 9 (updated)" {
		t.Fatalf("expected updated model, got %q", got[0].Model)
	}
	if got[0].SCSIIdentity != "" {
		t.Fatalf("expected SCSIIdentity cleared back to default, got %q", got[0].SCSIIdentity)
	}
}

// TestMigrateDriveTypesSchemaBackfillsExistingRows simulates upgrading a
// database created before the model/generation columns existed: a bare
// (name, speed, capacity, description) row must survive Open() with blank
// model/generation, not an error, and re-running the migration must be a
// no-op.
func TestMigrateDriveTypesSchemaBackfillsExistingRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE drive_types (name TEXT PRIMARY KEY, speed TEXT NOT NULL, capacity TEXT NOT NULL, description TEXT NOT NULL)`); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO drive_types (name, speed, capacity, description) VALUES ('LTO-8', '300MB/s', '12TB', 'pre-existing row')`); err != nil {
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

	dts, err := s.ListDriveTypes()
	if err != nil {
		t.Fatalf("list drive types: %v", err)
	}
	if len(dts) != 1 || dts[0].Name != "LTO-8" || dts[0].Model != "" || dts[0].Generation != "" {
		t.Fatalf("expected legacy row to survive migration with blank model/generation, got %+v", dts)
	}

	// Re-running must be a no-op (idempotent ALTER TABLE guard).
	if err := s.migrateDriveTypesSchema(); err != nil {
		t.Fatalf("re-running migration should be a no-op, got: %v", err)
	}
}

// TestMigrateDriveTypesSchemaBackfillsScsiIdentity is
// TestMigrateDriveTypesSchemaBackfillsExistingRows' own scenario, run
// against a database that already has model/generation (a post-that-
// migration, pre-Milestone-5 database) to confirm scsi_identity backfills
// independently of the other two columns, not just alongside a
// from-scratch legacy row.
func TestMigrateDriveTypesSchemaBackfillsScsiIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE drive_types (name TEXT PRIMARY KEY, speed TEXT NOT NULL, capacity TEXT NOT NULL, description TEXT NOT NULL, model TEXT NOT NULL DEFAULT '', generation TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatalf("create pre-Milestone-5 schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO drive_types (name, speed, capacity, description, model, generation) VALUES ('LTO-9', '400MB/s', '18TB', 'pre-existing row', 'LTO Ultrium 9', 'LTO-9')`); err != nil {
		t.Fatalf("seed pre-existing row: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	s := New(path)
	if err := s.Open(); err != nil {
		t.Fatalf("open store over pre-Milestone-5 schema: %v", err)
	}
	defer s.Close()

	dts, err := s.ListDriveTypes()
	if err != nil {
		t.Fatalf("list drive types: %v", err)
	}
	if len(dts) != 1 || dts[0].Generation != "LTO-9" || dts[0].SCSIIdentity != "" {
		t.Fatalf("expected pre-existing row to survive with Generation intact and blank SCSIIdentity, got %+v", dts)
	}
}

func TestLogicalLibraryChangerModelCRUDRoundTrip(t *testing.T) {
	s := newTestStore(t)
	lib := config.LogicalLibraryConfig{Name: "Library1", Color: "#ff0000", Drives: []int{0}, Magazines: []string{"mag1"}, ChangerModel: config.ChangerModelRealistic}
	if err := s.CreateLogicalLibrary(lib); err != nil {
		t.Fatalf("create logical library: %v", err)
	}
	got, err := s.ListLogicalLibraries()
	if err != nil {
		t.Fatalf("list logical libraries: %v", err)
	}
	if len(got) != 1 || got[0].ChangerModel != config.ChangerModelRealistic {
		t.Fatalf("unexpected logical libraries: %+v", got)
	}

	lib.ChangerModel = ""
	if err := s.UpdateLogicalLibrary("Library1", lib); err != nil {
		t.Fatalf("update logical library: %v", err)
	}
	got, err = s.ListLogicalLibraries()
	if err != nil {
		t.Fatalf("list logical libraries after update: %v", err)
	}
	if got[0].ChangerModel != "" {
		t.Fatalf("expected ChangerModel cleared back to default, got %q", got[0].ChangerModel)
	}
}

// TestMigrateLogicalLibrariesSchemaBackfillsChangerModel mirrors
// TestMigrateDriveTypesSchemaBackfillsExistingRows: a bare (name, color)
// row from before Milestone 5 must survive Open() with a blank
// changer_model, not an error.
func TestMigrateLogicalLibrariesSchemaBackfillsChangerModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE logical_libraries (name TEXT PRIMARY KEY, color TEXT NOT NULL)`); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO logical_libraries (name, color) VALUES ('Library1', '#ff0000')`); err != nil {
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

	libs, err := s.ListLogicalLibraries()
	if err != nil {
		t.Fatalf("list logical libraries: %v", err)
	}
	if len(libs) != 1 || libs[0].Name != "Library1" || libs[0].ChangerModel != "" {
		t.Fatalf("expected legacy row to survive migration with blank changer_model, got %+v", libs)
	}

	// Re-running must be a no-op (idempotent ALTER TABLE guard).
	if err := s.migrateLogicalLibrariesSchema(); err != nil {
		t.Fatalf("re-running migration should be a no-op, got: %v", err)
	}
}

func TestDriveDeviceCRUDRoundTripWithDriveTypeLink(t *testing.T) {
	s := newTestStore(t)
	devices := []config.DriveDeviceConfig{
		{DevicePath: "/var/lib/gotochanger/drives/drive0", DriveType: "LTO-8"},
		{DevicePath: "/var/lib/gotochanger/drives/drive1"}, // deliberately unlinked
	}
	if err := s.SaveDriveDevices(devices); err != nil {
		t.Fatalf("save drive devices: %v", err)
	}
	got, err := s.ListDriveDevices()
	if err != nil {
		t.Fatalf("list drive devices: %v", err)
	}
	if len(got) != 2 || got[0].DriveType != "LTO-8" || got[1].DriveType != "" {
		t.Fatalf("unexpected drive devices: %+v", got)
	}
}

// TestMigrateDriveDevicesSchemaBackfillsExistingRows simulates upgrading a
// database created before the drive_type link column existed: a bare
// (idx, device_path) row must survive Open() as unlinked (empty DriveType),
// not an error, and re-running the migration must be a no-op.
func TestMigrateDriveDevicesSchemaBackfillsExistingRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE drive_devices (idx INTEGER PRIMARY KEY, device_path TEXT NOT NULL)`); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO drive_devices (idx, device_path) VALUES (0, '/var/lib/gotochanger/drives/drive0')`); err != nil {
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

	devices, err := s.ListDriveDevices()
	if err != nil {
		t.Fatalf("list drive devices: %v", err)
	}
	if len(devices) != 1 || devices[0].DevicePath != "/var/lib/gotochanger/drives/drive0" || devices[0].DriveType != "" {
		t.Fatalf("expected legacy row to survive migration unlinked, got %+v", devices)
	}

	// Re-running must be a no-op (idempotent ALTER TABLE guard).
	if err := s.migrateDriveDevicesSchema(); err != nil {
		t.Fatalf("re-running migration should be a no-op, got: %v", err)
	}
}

// TestListMagazinesOrdersByInsertionNotAlphabetically is a regression
// test for a critical bug: ListMagazines used to ORDER BY id
// (alphabetical), and buildTopologyLocked assigns Slot.Address
// sequentially in whatever order ListMagazines returns - so a magazine
// created *after* an existing one, but whose id sorts *before* it
// alphabetically (e.g. "Cleaning Tapes" before "Magazine1"), would steal
// the existing magazine's low slot addresses and shift everything else,
// silently reassigning already-placed volumes to the wrong magazine on
// the next Reconfigure. Ordering by rowid instead guarantees a newly
// created magazine always sorts last.
func TestListMagazinesOrdersByInsertionNotAlphabetically(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateMagazine(config.MagazineConfig{ID: "Magazine1", Slots: 5}); err != nil {
		t.Fatalf("create Magazine1: %v", err)
	}
	if err := s.CreateMagazine(config.MagazineConfig{ID: "Cleaning Tapes", Slots: 5}); err != nil {
		t.Fatalf("create Cleaning Tapes: %v", err)
	}
	got, err := s.ListMagazines()
	if err != nil {
		t.Fatalf("list magazines: %v", err)
	}
	if len(got) != 2 || got[0].ID != "Magazine1" || got[1].ID != "Cleaning Tapes" {
		t.Fatalf("expected insertion order [Magazine1, Cleaning Tapes], got %+v", got)
	}
}

func TestListMailboxesOrdersByInsertionNotAlphabetically(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateMailbox(config.MailboxConfig{ID: "Mailbox1", Slots: 1}); err != nil {
		t.Fatalf("create Mailbox1: %v", err)
	}
	if err := s.CreateMailbox(config.MailboxConfig{ID: "Aardvark", Slots: 1}); err != nil {
		t.Fatalf("create Aardvark: %v", err)
	}
	got, err := s.ListMailboxes()
	if err != nil {
		t.Fatalf("list mailboxes: %v", err)
	}
	if len(got) != 2 || got[0].ID != "Mailbox1" || got[1].ID != "Aardvark" {
		t.Fatalf("expected insertion order [Mailbox1, Aardvark], got %+v", got)
	}
}

// TestDeleteMagazineOnlyRemovesTargetRow confirms the store layer's
// DeleteMagazine is now plain row deletion - no addressing bookkeeping to
// verify here anymore. Physical/label addressing is computed live from
// this list's order and slot counts entirely by
// library.Library.buildTopologyLocked (see its doc comment); the store's
// only remaining job is persisting id/slots correctly and returning them
// in stable insertion order (see TestListMagazinesOrdersByInsertionNotAlphabetically
// above, which is what that live computation actually depends on).
func TestDeleteMagazineOnlyRemovesTargetRow(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateMagazine(config.MagazineConfig{ID: "Magazine1", Slots: 10}); err != nil {
		t.Fatalf("create Magazine1: %v", err)
	}
	if err := s.CreateMagazine(config.MagazineConfig{ID: "Magazine2", Slots: 5}); err != nil {
		t.Fatalf("create Magazine2: %v", err)
	}
	if err := s.DeleteMagazine("Magazine1"); err != nil {
		t.Fatalf("delete Magazine1: %v", err)
	}
	got, err := s.ListMagazines()
	if err != nil {
		t.Fatalf("list magazines: %v", err)
	}
	if len(got) != 1 || got[0].ID != "Magazine2" || got[0].Slots != 5 {
		t.Fatalf("expected only Magazine2 (5 slots) to remain, got %+v", got)
	}
}

// TestSaveMagazinesReplacesFullListInSubmittedOrder confirms SaveMagazines
// (used by the wizard, which resubmits its full list on every step) is a
// true replace: the stored list afterward exactly matches what was
// submitted, in submission order. Order matters more than ever now -
// buildTopologyLocked derives every magazine's live ordinal/address/label
// directly from this order (see its doc comment) - so a resubmission that
// silently reordered magazines would silently renumber them too.
func TestSaveMagazinesReplacesFullListInSubmittedOrder(t *testing.T) {
	s := newTestStore(t)
	mags := []config.MagazineConfig{
		{ID: "Magazine1", Slots: 10},
		{ID: "Magazine2", Slots: 5},
	}
	if err := s.SaveMagazines(mags); err != nil {
		t.Fatalf("save magazines: %v", err)
	}
	got, err := s.ListMagazines()
	if err != nil {
		t.Fatalf("list magazines: %v", err)
	}
	if len(got) != 2 || got[0].ID != "Magazine1" || got[1].ID != "Magazine2" {
		t.Fatalf("expected [Magazine1, Magazine2] in order, got %+v", got)
	}

	// Resubmitting with one entry dropped and a new one added replaces the
	// whole list, in the new submission's order - not an incremental
	// add/remove that could leave stale ordering behind.
	mags = []config.MagazineConfig{
		{ID: "Magazine2", Slots: 5},
		{ID: "Magazine3", Slots: 20},
	}
	if err := s.SaveMagazines(mags); err != nil {
		t.Fatalf("resave magazines: %v", err)
	}
	got, err = s.ListMagazines()
	if err != nil {
		t.Fatalf("list magazines after resave: %v", err)
	}
	if len(got) != 2 || got[0].ID != "Magazine2" || got[1].ID != "Magazine3" {
		t.Fatalf("expected [Magazine2, Magazine3] in order after resave, got %+v", got)
	}
}

// TestSaveMailboxesReplacesFullListInSubmittedOrder mirrors
// TestSaveMagazinesReplacesFullListInSubmittedOrder for SaveMailboxes,
// which the wizard's mailbox step (step 4) drives identically.
func TestSaveMailboxesReplacesFullListInSubmittedOrder(t *testing.T) {
	s := newTestStore(t)
	mbs := []config.MailboxConfig{
		{ID: "Mailbox1", Slots: 4},
		{ID: "Mailbox2", Slots: 2},
	}
	if err := s.SaveMailboxes(mbs); err != nil {
		t.Fatalf("save mailboxes: %v", err)
	}
	got, err := s.ListMailboxes()
	if err != nil {
		t.Fatalf("list mailboxes: %v", err)
	}
	if len(got) != 2 || got[0].ID != "Mailbox1" || got[1].ID != "Mailbox2" {
		t.Fatalf("expected [Mailbox1, Mailbox2] in order, got %+v", got)
	}
}

func TestSeedDefaultsPopulatesBarcodeFormatColumns(t *testing.T) {
	s := newTestStore(t)
	if err := s.SeedDefaults(); err != nil {
		t.Fatalf("seed defaults: %v", err)
	}
	tts, err := s.ListTapeTypes()
	if err != nil {
		t.Fatalf("list tape types: %v", err)
	}
	if len(tts) == 0 {
		t.Fatalf("expected seeded tape types")
	}
	for _, tt := range tts {
		if tt.BarcodeFamily == "" {
			t.Errorf("seeded tape type %s has empty barcode_family", tt.Name)
		}
	}
}

func TestSeedDefaultsPopulatesCleaningSettings(t *testing.T) {
	s := newTestStore(t)
	if err := s.SeedDefaults(); err != nil {
		t.Fatalf("seed defaults: %v", err)
	}
	cs, err := s.GetCleaningSettings()
	if err != nil {
		t.Fatalf("get cleaning settings: %v", err)
	}
	want := config.DefaultCleaningSettings()
	if cs != want {
		t.Fatalf("expected seeded cleaning settings %+v, got %+v", want, cs)
	}
}

func TestCleaningSettingsRoundTrip(t *testing.T) {
	s := newTestStore(t)
	cs := config.CleaningSettings{Enabled: true, Mode: config.CleaningModeRobot, MaxUses: 15, MountThreshold: 30, Duration: "90s"}
	if err := s.SetCleaningSettings(cs); err != nil {
		t.Fatalf("set cleaning settings: %v", err)
	}
	got, err := s.GetCleaningSettings()
	if err != nil {
		t.Fatalf("get cleaning settings: %v", err)
	}
	if got != cs {
		t.Fatalf("expected %+v, got %+v", cs, got)
	}
}

// openStoreWithLegacyLatencyProfile builds a database with just the
// `config` table (matching initSchema's DDL) pre-seeded with a
// pre-0.5.0 `latency_profile` value, then opens a Store over it -
// exercising migrateLegacyLatencySetting exactly like a real upgrade
// would (Open() calls initTopologySchema() unconditionally).
func openStoreWithLegacyLatencyProfile(t *testing.T, profile string) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")

	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE config (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatalf("create legacy config table: %v", err)
	}
	if profile != "" {
		if _, err := db.Exec(`INSERT INTO config (key, value) VALUES ('latency_profile', ?)`, profile); err != nil {
			t.Fatalf("seed legacy latency_profile: %v", err)
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

func TestMigrateLegacyLatencySettingTranslatesKnownProfile(t *testing.T) {
	s := openStoreWithLegacyLatencyProfile(t, "medium")

	ls, err := s.GetLatencySettings()
	if err != nil {
		t.Fatalf("get latency settings: %v", err)
	}
	if !ls.Enabled {
		t.Errorf("expected Enabled=true for a non-none legacy profile, got false")
	}
	if ls.RobotMoveTape != "3s" || ls.DriveLoad != "1s" || ls.DriveUnload != "1s" {
		t.Errorf("expected medium profile's values (3s/1s/1s) carried over, got robot_move_tape=%q drive_load=%q drive_unload=%q",
			ls.RobotMoveTape, ls.DriveLoad, ls.DriveUnload)
	}
	def := config.DefaultLatencySettings()
	if ls.TapePositioning != def.TapePositioning || ls.RobotMoveScan != def.RobotMoveScan ||
		ls.MagazineScan != def.MagazineScan || ls.DoorAction != def.DoorAction {
		t.Errorf("expected the 4 new fields (no legacy equivalent) to get factory defaults, got %+v", ls)
	}
}

func TestMigrateLegacyLatencySettingNoneMeansDisabled(t *testing.T) {
	s := openStoreWithLegacyLatencyProfile(t, "none")
	ls, err := s.GetLatencySettings()
	if err != nil {
		t.Fatalf("get latency settings: %v", err)
	}
	if ls.Enabled {
		t.Errorf("expected Enabled=false for the \"none\" legacy profile, got true")
	}
}

func TestMigrateLegacyLatencySettingNoPriorValueStaysDisabled(t *testing.T) {
	// A database that never had latency_profile at all (a genuinely
	// fresh install, or one that predates even the old profile catalog)
	// must not spuriously enable latency simulation.
	s := openStoreWithLegacyLatencyProfile(t, "")
	ls, err := s.GetLatencySettings()
	if err != nil {
		t.Fatalf("get latency settings: %v", err)
	}
	if ls.Enabled {
		t.Errorf("expected Enabled=false with no legacy latency_profile present, got true")
	}
	def := config.DefaultLatencySettings()
	if ls != def {
		t.Errorf("expected factory defaults with no legacy value present, got %+v want %+v", ls, def)
	}
}

func TestMigrateLegacyLatencySettingIsIdempotent(t *testing.T) {
	// Once migrated, a later Open() (e.g. a daemon restart) must not
	// re-derive from latency_profile again and clobber an admin's
	// subsequent Admin > Latency edit.
	s := openStoreWithLegacyLatencyProfile(t, "high")

	custom := config.LatencySettings{
		Enabled: true, DriveLoad: "9s", DriveUnload: "9s", TapePositioning: "9s",
		RobotMoveTape: "9s", RobotMoveScan: "9s", MagazineScan: "9s", DoorAction: "9s",
	}
	if err := s.SetLatencySettings(custom); err != nil {
		t.Fatalf("set latency settings: %v", err)
	}

	if err := s.initTopologySchema(); err != nil {
		t.Fatalf("re-run topology schema init: %v", err)
	}

	ls, err := s.GetLatencySettings()
	if err != nil {
		t.Fatalf("get latency settings: %v", err)
	}
	if ls != custom {
		t.Errorf("migration re-ran and clobbered a later edit: got %+v, want %+v", ls, custom)
	}
}
