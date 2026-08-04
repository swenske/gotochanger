package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/swenske/gotochanger/internal/config"
	"github.com/swenske/gotochanger/internal/instanceid"
)

// topologySchema is applied in addition to initSchema's state/config tables.
// Every piece of library topology (previously read from config.yaml) lives
// here instead: a fresh database has none of it, which is what lets a fresh
// install start with nothing configured until the setup wizard runs.
const topologySchema = `
CREATE TABLE IF NOT EXISTS drive_types (
	name TEXT PRIMARY KEY,
	speed TEXT NOT NULL,
	capacity TEXT NOT NULL,
	description TEXT NOT NULL,
	model TEXT NOT NULL DEFAULT '',
	generation TEXT NOT NULL DEFAULT '',
	scsi_identity TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS tape_types (
	name TEXT PRIMARY KEY,
	capacity TEXT NOT NULL,
	description TEXT NOT NULL,
	barcode_family TEXT NOT NULL DEFAULT 'generic',
	media_id TEXT NOT NULL DEFAULT '',
	volser_length INTEGER NOT NULL DEFAULT 8
);
CREATE TABLE IF NOT EXISTS magazines (
	id TEXT PRIMARY KEY,
	slots INTEGER NOT NULL,
	base_address INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS mailboxes (
	id TEXT PRIMARY KEY,
	slots INTEGER NOT NULL,
	base_address INTEGER NOT NULL DEFAULT 0,
	pin_hash TEXT
);
CREATE TABLE IF NOT EXISTS drive_devices (
	idx INTEGER PRIMARY KEY,
	device_path TEXT NOT NULL,
	drive_type TEXT
);
CREATE TABLE IF NOT EXISTS logical_libraries (
	name TEXT PRIMARY KEY,
	color TEXT NOT NULL,
	changer_model TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS logical_library_drives (
	logical_library TEXT NOT NULL REFERENCES logical_libraries(name) ON DELETE CASCADE,
	drive_index INTEGER NOT NULL,
	PRIMARY KEY (logical_library, drive_index)
);
CREATE TABLE IF NOT EXISTS logical_library_magazines (
	logical_library TEXT NOT NULL REFERENCES logical_libraries(name) ON DELETE CASCADE,
	magazine_id TEXT NOT NULL,
	PRIMARY KEY (logical_library, magazine_id)
);
CREATE TABLE IF NOT EXISTS logical_library_mailboxes (
	logical_library TEXT NOT NULL REFERENCES logical_libraries(name) ON DELETE CASCADE,
	mailbox_id TEXT NOT NULL,
	PRIMARY KEY (logical_library, mailbox_id)
);
CREATE TABLE IF NOT EXISTS tape_sets (
	name TEXT PRIMARY KEY,
	tape_type TEXT NOT NULL,
	storage_folder TEXT NOT NULL
);
`

// initTopologySchema is called by Open() alongside initSchema.
func (s *Store) initTopologySchema() error {
	if _, err := s.db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		return err
	}
	if _, err := s.db.Exec(topologySchema); err != nil {
		return err
	}
	if err := s.migrateTapeTypesSchema(); err != nil {
		return err
	}
	if err := s.migrateDriveTypesSchema(); err != nil {
		return err
	}
	if err := s.migrateDriveDevicesSchema(); err != nil {
		return err
	}
	if err := s.migrateLogicalLibrariesSchema(); err != nil {
		return err
	}
	if err := s.migrateMailboxesPINSchema(); err != nil {
		return err
	}
	return s.migrateLegacyLatencySetting()
}

// legacyLatencyPresets carries forward the three numbers each pre-rework
// named profile (internal/config's old LatencyProfile/LatencyProfiles,
// removed) used to set, keyed by profile name. Used only by
// migrateLegacyLatencySetting - "none" is deliberately absent, since it
// always meant Enabled=false with every duration irrelevant.
var legacyLatencyPresets = map[string]struct{ robotics, driveLoad, driveUnload string }{
	"low":    {robotics: "1s", driveLoad: "500ms", driveUnload: "500ms"},
	"medium": {robotics: "3s", driveLoad: "1s", driveUnload: "1s"},
	"high":   {robotics: "5s", driveLoad: "2s", driveUnload: "2s"},
}

// migrateLegacyLatencySetting translates a pre-rework "latency_profile"
// singleton (one of none/low/medium/high, or unset) into the new
// per-delay "latency_*" singletons the rest of this file reads/writes,
// instead of silently discarding it - the same lesson
// repairLegacyTapeTypeBarcodeFormats above already teaches: a migration
// that quietly produces a different-but-not-erroring value (here,
// silently reverting an operator's chosen latency profile to "disabled")
// is worse than doing the extra translation work. Guarded by
// "latency_enabled not yet present" so it only ever runs once per
// database - the very first Admin > Latency save (or SeedDefaults, on a
// database that never had latency_profile at all) leaves latency_enabled
// set from then on. The four new delay dimensions with no legacy
// equivalent (TapePositioning/RobotMoveScan/MagazineScan/DoorAction)
// always get DefaultLatencySettings()'s value, regardless of which old
// profile was selected.
func (s *Store) migrateLegacyLatencySetting() error {
	if _, migrated, err := s.GetSetting("latency_enabled"); err != nil {
		return err
	} else if migrated {
		return nil
	}
	old, ok, err := s.GetSetting("latency_profile")
	if err != nil {
		return err
	}
	ls := config.DefaultLatencySettings()
	if ok && old != "" && old != "none" {
		ls.Enabled = true
		if preset, known := legacyLatencyPresets[old]; known {
			ls.RobotMoveTape = preset.robotics
			ls.DriveLoad = preset.driveLoad
			ls.DriveUnload = preset.driveUnload
		}
	}
	return s.SetLatencySettings(ls)
}

// migrateTapeTypesSchema adds the barcode-format columns introduced
// alongside per-tape-type barcode generation to a pre-existing tape_types
// table. CREATE TABLE IF NOT EXISTS (above) only covers a brand-new
// database; an existing tape_types table from before this feature needs an
// explicit ALTER TABLE. Idempotent (checks the column set first), so it's a
// no-op on every Open() once the columns exist. Backfill defaults treat any
// pre-existing tape type as a permissive, non-physical "generic" type so
// nothing crashes on upgrade - repairLegacyTapeTypeBarcodeFormats then
// upgrades the ones it safely can to their real format.
func (s *Store) migrateTapeTypesSchema() error {
	cols, err := s.tableColumns("tape_types")
	if err != nil {
		return fmt.Errorf("inspect tape_types schema: %w", err)
	}
	for col, ddl := range map[string]string{
		"barcode_family": `ALTER TABLE tape_types ADD COLUMN barcode_family TEXT NOT NULL DEFAULT 'generic'`,
		"media_id":       `ALTER TABLE tape_types ADD COLUMN media_id TEXT NOT NULL DEFAULT ''`,
		"volser_length":  `ALTER TABLE tape_types ADD COLUMN volser_length INTEGER NOT NULL DEFAULT 8`,
	} {
		if !cols[col] {
			if _, err := s.db.Exec(ddl); err != nil {
				return fmt.Errorf("add tape_types.%s: %w", col, err)
			}
		}
	}
	return s.repairLegacyTapeTypeBarcodeFormats()
}

// repairLegacyTapeTypeBarcodeFormats fixes a real gap the plain
// column-add migration above leaves behind: a tape-type row that existed
// before the barcode-format columns did (e.g. "LTO-8", seeded by every
// install prior to this feature) survives the ALTER TABLE with the
// safe-but-wrong generic/""/8 backfill instead of the real LTO format its
// name implies - SeedDefaults() only seeds the catalog into an empty
// table, so it never gets a chance to correct an already-populated one
// either. Confirmed on the real bareos-disk-sd-int-fr1 deployment: two
// tape sets created against its pre-existing "LTO-8" type generated plain
// 8-digit numeric barcodes ("00000021", ...) instead of the real 8-char
// LTO format ("000021L8").
//
// legacyTapeTypeBarcodeFormats covers the exact 5 names every install
// prior to this feature could possibly have auto-seeded (the pre-0.4.0
// DefaultTapeTypes() catalog) - "DDS" and "DLT" no longer exist as bare
// names in the current, generation-specific catalog (superseded by
// "DDS-1".."DDS-4"/"DAT-72" etc. and "DLT-III"/"DLT-IV"), so a plain
// name-match against today's config.DefaultTapeTypes() would silently
// miss them. Mapped to the current catalog entry matching their old
// capacity (DDS was 80GB -> DAT-160; DLT was 40GB -> DLT-IV) so the
// repair is at least a reasonable, non-arbitrary choice.
var legacyTapeTypeBarcodeFormats = map[string]config.TapeType{
	"Unlimited": {Name: "Unlimited", BarcodeFamily: "generic", VolSerLength: 8},
	"LTO-8":     {Name: "LTO-8", BarcodeFamily: "lto", MediaID: "L8", VolSerLength: 6},
	"LTO-9":     {Name: "LTO-9", BarcodeFamily: "lto", MediaID: "L9", VolSerLength: 6},
	"DDS":       {Name: "DDS", BarcodeFamily: "dds", MediaID: "D6", VolSerLength: 6},
	"DLT":       {Name: "DLT", BarcodeFamily: "dlt", MediaID: "4", VolSerLength: 6},
}

// Only touches rows still at the exact backfill signature
// (barcode_family='generic', media_id=”, volser_length=8) whose name
// matches either legacyTapeTypeBarcodeFormats above or an entry in the
// current config.DefaultTapeTypes() catalog (covers an admin having
// manually created a tape type before 0.4.0 whose name happens to match
// today's richer catalog, e.g. "SDLT-220") - i.e. rows nobody has
// deliberately edited since upgrading - and sets them to the matched
// entry's real barcode format. Safe to run on every startup (idempotent:
// a row already repaired, or already something other than the exact
// backfill signature, is left alone). Does not touch any already-created
// cartridge's barcode - only affects tape types going forward from the
// point this runs.
func (s *Store) repairLegacyTapeTypeBarcodeFormats() error {
	byName := make(map[string]config.TapeType, len(config.DefaultTapeTypes())+len(legacyTapeTypeBarcodeFormats))
	for _, tt := range config.DefaultTapeTypes() {
		byName[tt.Name] = tt
	}
	for name, tt := range legacyTapeTypeBarcodeFormats {
		byName[name] = tt
	}

	rows, err := s.db.Query(`SELECT name FROM tape_types WHERE barcode_family = 'generic' AND media_id = '' AND volser_length = 8`)
	if err != nil {
		return fmt.Errorf("scan for legacy tape types: %w", err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return fmt.Errorf("scan legacy tape type name: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, name := range names {
		tt, ok := byName[name]
		if !ok || tt.BarcodeFamily == "generic" {
			continue // no catalog entry to correct against, or it really is generic
		}
		if _, err := s.db.Exec(`UPDATE tape_types SET barcode_family = ?, media_id = ?, volser_length = ? WHERE name = ?`,
			tt.BarcodeFamily, tt.MediaID, tt.VolSerLength, name); err != nil {
			return fmt.Errorf("repair legacy tape type %s: %w", name, err)
		}
	}
	return nil
}

// migrateDriveTypesSchema adds the model/generation columns to a pre-
// existing drive_types table (idempotent - a no-op once both columns
// exist). Unlike tape types' barcode-format repair, no backfill step is
// needed afterward: Model/Generation are purely descriptive labels, so a
// blank value on a pre-existing row is simply blank, not silently *wrong*
// the way tape-type's generic-barcode backfill was (that one affected real
// generated filenames; this one only affects a UI display column).
func (s *Store) migrateDriveTypesSchema() error {
	cols, err := s.tableColumns("drive_types")
	if err != nil {
		return fmt.Errorf("inspect drive_types schema: %w", err)
	}
	for col, ddl := range map[string]string{
		"model":         `ALTER TABLE drive_types ADD COLUMN model TEXT NOT NULL DEFAULT ''`,
		"generation":    `ALTER TABLE drive_types ADD COLUMN generation TEXT NOT NULL DEFAULT ''`,
		"scsi_identity": `ALTER TABLE drive_types ADD COLUMN scsi_identity TEXT NOT NULL DEFAULT ''`,
	} {
		if !cols[col] {
			if _, err := s.db.Exec(ddl); err != nil {
				return fmt.Errorf("add drive_types.%s: %w", col, err)
			}
		}
	}
	return nil
}

// migrateLogicalLibrariesSchema adds the changer_model column (Milestone
// 5) to a pre-existing logical_libraries table - idempotent, same
// "blank on a pre-existing row is simply blank, not silently wrong"
// posture as migrateDriveTypesSchema above (see its own doc comment):
// changer_model is opt-in display/identity data, not something a blank
// value could misrepresent.
func (s *Store) migrateLogicalLibrariesSchema() error {
	cols, err := s.tableColumns("logical_libraries")
	if err != nil {
		return fmt.Errorf("inspect logical_libraries schema: %w", err)
	}
	if !cols["changer_model"] {
		if _, err := s.db.Exec(`ALTER TABLE logical_libraries ADD COLUMN changer_model TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add logical_libraries.changer_model: %w", err)
		}
	}
	return nil
}

// migrateDriveDevicesSchema adds the drive_type link column to a pre-
// existing drive_devices table (idempotent). A NULL/absent value means
// "unlinked" - a pre-existing drive device simply displays no
// model/generation/capacity in Admin > Drives until an admin links it to a
// Drive Type, same "constrain new writes only, never retroactively"
// convention this codebase already uses for tape_sets.tape_type.
func (s *Store) migrateDriveDevicesSchema() error {
	cols, err := s.tableColumns("drive_devices")
	if err != nil {
		return fmt.Errorf("inspect drive_devices schema: %w", err)
	}
	if !cols["drive_type"] {
		if _, err := s.db.Exec(`ALTER TABLE drive_devices ADD COLUMN drive_type TEXT`); err != nil {
			return fmt.Errorf("add drive_devices.drive_type: %w", err)
		}
	}
	return nil
}

// migrateMailboxesPINSchema adds the per-mailbox PIN hash column to a pre-
// existing mailboxes table (idempotent). A NULL/absent value means "no PIN
// configured for this mailbox", matching the presence-implies-protection
// design (see Library.checkMailboxPINLocked): a pre-existing mailbox simply
// stays unprotected until an admin sets a PIN.
func (s *Store) migrateMailboxesPINSchema() error {
	cols, err := s.tableColumns("mailboxes")
	if err != nil {
		return fmt.Errorf("inspect mailboxes schema: %w", err)
	}
	if !cols["pin_hash"] {
		if _, err := s.db.Exec(`ALTER TABLE mailboxes ADD COLUMN pin_hash TEXT`); err != nil {
			return fmt.Errorf("add mailboxes.pin_hash: %w", err)
		}
	}
	return nil
}

// tableColumns returns the set of column names table currently has, via
// PRAGMA table_info. table is always a hardcoded literal from our own
// code, never user input, so building the PRAGMA statement with Sprintf is
// safe.
func (s *Store) tableColumns(table string) (map[string]bool, error) {
	rows, err := s.db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	return cols, rows.Err()
}

// SeedDefaults inserts the suggested drive/tape type catalogs the first time
// the database is opened (idempotent: a no-op once rows already exist).
// Everything else (magazines, drive devices, logical libraries, tape sets,
// and the singleton settings) is left empty — that's what the setup wizard
// is for.
func (s *Store) SeedDefaults() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM drive_types").Scan(&n); err != nil {
		return fmt.Errorf("count drive_types: %w", err)
	}
	if n == 0 {
		for _, dt := range config.DefaultDriveTypes() {
			if _, err := s.db.Exec("INSERT INTO drive_types (name, speed, capacity, description, model, generation, scsi_identity) VALUES (?, ?, ?, ?, ?, ?, ?)",
				dt.Name, dt.Speed, dt.Capacity, dt.Description, dt.Model, dt.Generation, dt.SCSIIdentity); err != nil {
				return fmt.Errorf("seed drive type %s: %w", dt.Name, err)
			}
		}
	}

	if err := s.db.QueryRow("SELECT COUNT(*) FROM tape_types").Scan(&n); err != nil {
		return fmt.Errorf("count tape_types: %w", err)
	}
	if n == 0 {
		for _, tt := range config.DefaultTapeTypes() {
			if _, err := s.db.Exec("INSERT INTO tape_types (name, capacity, description, barcode_family, media_id, volser_length) VALUES (?, ?, ?, ?, ?, ?)",
				tt.Name, tt.Capacity, tt.Description, tt.BarcodeFamily, tt.MediaID, tt.VolSerLength); err != nil {
				return fmt.Errorf("seed tape type %s: %w", tt.Name, err)
			}
		}
	}

	// Sensible starting values for the settings an admin can already edit
	// today without running the wizard (these aren't "topology" the wizard
	// gates on, unlike drives/magazines/io-slots/logical libraries).
	if _, ok, err := s.GetSetting("default_capacity"); err != nil {
		return err
	} else if !ok {
		if err := s.SetSetting("default_capacity", "10GiB"); err != nil {
			return err
		}
	}

	// A stable, anonymous instance ID for a future telemetry feature (not
	// built yet). Seeded here, not lazily in InstanceID alone, so it's
	// generated at most once per SeedDefaults call - including right
	// after a factory reset's process restart, at which point it
	// re-derives the same value from hardware on bare-metal/VM installs
	// (see internal/instanceid's doc comment).
	if _, ok, err := s.GetSetting(instanceIDSettingKey); err != nil {
		return err
	} else if !ok {
		id, err := instanceid.Generate()
		if err != nil {
			return fmt.Errorf("generate instance id: %w", err)
		}
		if err := s.SetSetting(instanceIDSettingKey, id); err != nil {
			return err
		}
	}

	// Daemon-level settings that used to live in config.yaml (snmp,
	// poll_interval, log_level) - see LoadDaemonSettings. Seeded here the
	// same way as the three above so a fresh install has sane values from
	// the first run, editable afterwards through the Admin Settings API
	// without ever touching config.yaml again.
	daemonDefaults := map[string]string{
		"poll_interval":       "5s",
		"log_level":           "info",
		"snmp_enabled":        "false",
		"snmp_enterprise_oid": "1.3.6.1.4.1.55555.1",
		"snmp_agent_address":  "127.0.0.1",
		"snmp_targets":        "[]",
		"prometheus_enabled":  "false",
		"telemetry_enabled":   "false",
	}
	for key, def := range daemonDefaults {
		if _, ok, err := s.GetSetting(key); err != nil {
			return err
		} else if !ok {
			if err := s.SetSetting(key, def); err != nil {
				return err
			}
		}
	}

	// Latency simulation delays (see internal/config's LatencySettings) -
	// seeded the same idempotent way as default_capacity above. Usually a
	// no-op by the time SeedDefaults runs, since migrateLegacyLatencySetting
	// (called earlier, from initTopologySchema) already writes these on a
	// database that's never had them; this only fires for a database that
	// also never had the old latency_profile setting either (a genuinely
	// brand-new install).
	def := config.DefaultLatencySettings()
	for key, val := range latencySettingValues(def) {
		if _, ok, err := s.GetSetting(key); err != nil {
			return err
		} else if !ok {
			if err := s.SetSetting(key, val); err != nil {
				return err
			}
		}
	}

	// Cleaning-tape management settings (see internal/config's
	// CleaningSettings) - seeded the same idempotent way as latency above.
	// A brand-new feature, so there's no legacy setting to migrate first;
	// this is the only place these keys are ever seeded.
	cleaningDef := config.DefaultCleaningSettings()
	for key, val := range cleaningSettingValues(cleaningDef) {
		if _, ok, err := s.GetSetting(key); err != nil {
			return err
		} else if !ok {
			if err := s.SetSetting(key, val); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---- singleton key/value settings (reuses the existing `config` table) ----

// GetSetting returns the raw string value for key, and whether it was set.
func (s *Store) GetSetting(key string) (string, bool, error) {
	var v string
	err := s.db.QueryRow("SELECT value FROM config WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read setting %s: %w", key, err)
	}
	return v, true, nil
}

// SetSetting upserts key=value.
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec("INSERT OR REPLACE INTO config (key, value) VALUES (?, ?)", key, value)
	if err != nil {
		return fmt.Errorf("save setting %s: %w", key, err)
	}
	return nil
}

func (s *Store) getStringSetting(key, def string) (string, error) {
	v, ok, err := s.GetSetting(key)
	if err != nil {
		return "", err
	}
	if !ok {
		return def, nil
	}
	return v, nil
}

func (s *Store) getBoolSetting(key string, def bool) (bool, error) {
	v, ok, err := s.GetSetting(key)
	if err != nil {
		return false, err
	}
	if !ok {
		return def, nil
	}
	return v == "true", nil
}

func (s *Store) getIntSetting(key string, def int) (int, error) {
	v, ok, err := s.GetSetting(key)
	if err != nil {
		return 0, err
	}
	if !ok {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def, nil
	}
	return n, nil
}

// ---- latency settings (see config.LatencySettings; reuses the existing
// singleton key/value primitives above, one key per duration field) ----

func latencySettingValues(ls config.LatencySettings) map[string]string {
	enabled := "false"
	if ls.Enabled {
		enabled = "true"
	}
	return map[string]string{
		"latency_enabled":          enabled,
		"latency_drive_load":       ls.DriveLoad,
		"latency_drive_unload":     ls.DriveUnload,
		"latency_tape_positioning": ls.TapePositioning,
		"latency_robot_move_tape":  ls.RobotMoveTape,
		"latency_robot_move_scan":  ls.RobotMoveScan,
		"latency_magazine_scan":    ls.MagazineScan,
		"latency_door_action":      ls.DoorAction,
	}
}

// GetLatencySettings reads the 8 latency singleton keys, defaulting any
// unset field to config.DefaultLatencySettings()'s corresponding value.
func (s *Store) GetLatencySettings() (config.LatencySettings, error) {
	def := config.DefaultLatencySettings()
	var ls config.LatencySettings
	var err error
	if ls.Enabled, err = s.getBoolSetting("latency_enabled", def.Enabled); err != nil {
		return ls, err
	}
	if ls.DriveLoad, err = s.getStringSetting("latency_drive_load", def.DriveLoad); err != nil {
		return ls, err
	}
	if ls.DriveUnload, err = s.getStringSetting("latency_drive_unload", def.DriveUnload); err != nil {
		return ls, err
	}
	if ls.TapePositioning, err = s.getStringSetting("latency_tape_positioning", def.TapePositioning); err != nil {
		return ls, err
	}
	if ls.RobotMoveTape, err = s.getStringSetting("latency_robot_move_tape", def.RobotMoveTape); err != nil {
		return ls, err
	}
	if ls.RobotMoveScan, err = s.getStringSetting("latency_robot_move_scan", def.RobotMoveScan); err != nil {
		return ls, err
	}
	if ls.MagazineScan, err = s.getStringSetting("latency_magazine_scan", def.MagazineScan); err != nil {
		return ls, err
	}
	if ls.DoorAction, err = s.getStringSetting("latency_door_action", def.DoorAction); err != nil {
		return ls, err
	}
	return ls, nil
}

// SetLatencySettings upserts all 8 latency singleton keys at once, used by
// migrateLegacyLatencySetting/SeedDefaults and the Admin > Latency API
// handler (internal/api/latency.go).
func (s *Store) SetLatencySettings(ls config.LatencySettings) error {
	for key, val := range latencySettingValues(ls) {
		if err := s.SetSetting(key, val); err != nil {
			return err
		}
	}
	return nil
}

// ---- cleaning-tape management settings (see config.CleaningSettings;
// reuses the existing singleton key/value primitives, one key per field) ----

func cleaningSettingValues(cs config.CleaningSettings) map[string]string {
	enabled := "false"
	if cs.Enabled {
		enabled = "true"
	}
	return map[string]string{
		"cleaning_enabled":         enabled,
		"cleaning_mode":            cs.Mode,
		"cleaning_max_uses":        strconv.Itoa(cs.MaxUses),
		"cleaning_mount_threshold": strconv.Itoa(cs.MountThreshold),
		"cleaning_duration":        cs.Duration,
	}
}

// GetCleaningSettings reads the 5 cleaning singleton keys, defaulting any
// unset field to config.DefaultCleaningSettings()'s corresponding value.
func (s *Store) GetCleaningSettings() (config.CleaningSettings, error) {
	def := config.DefaultCleaningSettings()
	var cs config.CleaningSettings
	var err error
	if cs.Enabled, err = s.getBoolSetting("cleaning_enabled", def.Enabled); err != nil {
		return cs, err
	}
	if cs.Mode, err = s.getStringSetting("cleaning_mode", def.Mode); err != nil {
		return cs, err
	}
	if cs.MaxUses, err = s.getIntSetting("cleaning_max_uses", def.MaxUses); err != nil {
		return cs, err
	}
	if cs.MountThreshold, err = s.getIntSetting("cleaning_mount_threshold", def.MountThreshold); err != nil {
		return cs, err
	}
	if cs.Duration, err = s.getStringSetting("cleaning_duration", def.Duration); err != nil {
		return cs, err
	}
	return cs, nil
}

// SetCleaningSettings upserts all 5 cleaning singleton keys at once, used
// by SeedDefaults and the Admin > Cleaning Tapes API handler
// (internal/api/cleaning.go).
func (s *Store) SetCleaningSettings(cs config.CleaningSettings) error {
	for key, val := range cleaningSettingValues(cs) {
		if err := s.SetSetting(key, val); err != nil {
			return err
		}
	}
	return nil
}

// ---- drive types ----

func (s *Store) ListDriveTypes() ([]config.DriveType, error) {
	rows, err := s.db.Query("SELECT name, speed, capacity, description, model, generation, scsi_identity FROM drive_types ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("list drive types: %w", err)
	}
	defer rows.Close()
	var out []config.DriveType
	for rows.Next() {
		var dt config.DriveType
		if err := rows.Scan(&dt.Name, &dt.Speed, &dt.Capacity, &dt.Description, &dt.Model, &dt.Generation, &dt.SCSIIdentity); err != nil {
			return nil, fmt.Errorf("scan drive type: %w", err)
		}
		out = append(out, dt)
	}
	return out, rows.Err()
}

// CreateDriveType inserts a new drive type, rejecting a duplicate name.
func (s *Store) CreateDriveType(dt config.DriveType) error {
	_, err := s.db.Exec("INSERT INTO drive_types (name, speed, capacity, description, model, generation, scsi_identity) VALUES (?, ?, ?, ?, ?, ?, ?)",
		dt.Name, dt.Speed, dt.Capacity, dt.Description, dt.Model, dt.Generation, dt.SCSIIdentity)
	if err != nil {
		return fmt.Errorf("drive type %s already exists or is invalid: %w", dt.Name, err)
	}
	return nil
}

// UpdateDriveType replaces an existing drive type's fields.
func (s *Store) UpdateDriveType(name string, dt config.DriveType) error {
	res, err := s.db.Exec("UPDATE drive_types SET speed = ?, capacity = ?, description = ?, model = ?, generation = ?, scsi_identity = ? WHERE name = ?",
		dt.Speed, dt.Capacity, dt.Description, dt.Model, dt.Generation, dt.SCSIIdentity, name)
	if err != nil {
		return fmt.Errorf("update drive type %s: %w", name, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("drive type %s not found", name)
	}
	return nil
}

func (s *Store) DeleteDriveType(name string) error {
	res, err := s.db.Exec("DELETE FROM drive_types WHERE name = ?", name)
	if err != nil {
		return fmt.Errorf("delete drive type %s: %w", name, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("drive type %s not found", name)
	}
	return nil
}

// ---- tape types ----

func (s *Store) ListTapeTypes() ([]config.TapeType, error) {
	rows, err := s.db.Query("SELECT name, capacity, description, barcode_family, media_id, volser_length FROM tape_types ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("list tape types: %w", err)
	}
	defer rows.Close()
	var out []config.TapeType
	for rows.Next() {
		var tt config.TapeType
		if err := rows.Scan(&tt.Name, &tt.Capacity, &tt.Description, &tt.BarcodeFamily, &tt.MediaID, &tt.VolSerLength); err != nil {
			return nil, fmt.Errorf("scan tape type: %w", err)
		}
		out = append(out, tt)
	}
	return out, rows.Err()
}

func (s *Store) CreateTapeType(tt config.TapeType) error {
	_, err := s.db.Exec("INSERT INTO tape_types (name, capacity, description, barcode_family, media_id, volser_length) VALUES (?, ?, ?, ?, ?, ?)",
		tt.Name, tt.Capacity, tt.Description, tt.BarcodeFamily, tt.MediaID, tt.VolSerLength)
	if err != nil {
		return fmt.Errorf("tape type %s already exists or is invalid: %w", tt.Name, err)
	}
	return nil
}

func (s *Store) UpdateTapeType(name string, tt config.TapeType) error {
	res, err := s.db.Exec("UPDATE tape_types SET capacity = ?, description = ?, barcode_family = ?, media_id = ?, volser_length = ? WHERE name = ?",
		tt.Capacity, tt.Description, tt.BarcodeFamily, tt.MediaID, tt.VolSerLength, name)
	if err != nil {
		return fmt.Errorf("update tape type %s: %w", name, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("tape type %s not found", name)
	}
	return nil
}

func (s *Store) DeleteTapeType(name string) error {
	res, err := s.db.Exec("DELETE FROM tape_types WHERE name = ?", name)
	if err != nil {
		return fmt.Errorf("delete tape type %s: %w", name, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("tape type %s not found", name)
	}
	return nil
}

// ---- magazines ----

// ListMagazines returns every magazine ordered by rowid (insertion order),
// not by id (alphabetical). This ordering is load-bearing now:
// library.Library.buildTopologyLocked walks magazines in exactly this
// order to compute both each slot's flat Address (a running counter) and
// its magazine-relative Label ("<ordinal>.<offset>", ordinal = this
// magazine's 1-based position in this same list) - live, on every
// topology rebuild, never persisted. Ordering by rowid means a newly
// created magazine always sorts last (and gets the highest ordinal),
// regardless of its ID's alphabetical position.
func (s *Store) ListMagazines() ([]config.MagazineConfig, error) {
	rows, err := s.db.Query("SELECT id, slots FROM magazines ORDER BY rowid")
	if err != nil {
		return nil, fmt.Errorf("list magazines: %w", err)
	}
	defer rows.Close()
	var out []config.MagazineConfig
	for rows.Next() {
		var m config.MagazineConfig
		if err := rows.Scan(&m.ID, &m.Slots); err != nil {
			return nil, fmt.Errorf("scan magazine: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CreateMagazine inserts a new magazine, rejecting a duplicate ID. Callers
// must validate slot-count constraints (config.ValidateMagazine) first.
func (s *Store) CreateMagazine(m config.MagazineConfig) error {
	if _, err := s.db.Exec("INSERT INTO magazines (id, slots) VALUES (?, ?)", m.ID, m.Slots); err != nil {
		return fmt.Errorf("magazine %s already exists or is invalid: %w", m.ID, err)
	}
	return nil
}

func (s *Store) UpdateMagazine(id string, m config.MagazineConfig) error {
	res, err := s.db.Exec("UPDATE magazines SET slots = ? WHERE id = ?", m.Slots, id)
	if err != nil {
		return fmt.Errorf("update magazine %s: %w", id, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("magazine %s not found", id)
	}
	return nil
}

// DeleteMagazine removes a magazine. Callers are responsible for checking
// its slots are empty first (the caller, not the store, holds the live
// slot/volume state needed to check that).
func (s *Store) DeleteMagazine(id string) error {
	res, err := s.db.Exec("DELETE FROM magazines WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete magazine %s: %w", id, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("magazine %s not found", id)
	}
	return nil
}

// SaveMagazines replaces the whole magazines list. Used by the setup
// wizard, which resubmits its full current list on every step (rather than
// individual create/update/delete calls, which is what the Admin API uses
// instead - see CreateMagazine/UpdateMagazine/DeleteMagazine).
func (s *Store) SaveMagazines(mags []config.MagazineConfig) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM magazines"); err != nil {
		return fmt.Errorf("clear magazines: %w", err)
	}
	for _, m := range mags {
		if _, err := tx.Exec("INSERT INTO magazines (id, slots) VALUES (?, ?)", m.ID, m.Slots); err != nil {
			return fmt.Errorf("save magazine %s: %w", m.ID, err)
		}
	}
	return tx.Commit()
}

// ---- mailboxes (mirrors magazines exactly - real, independently
// addressable groups of I/O slots, not just a flat count) ----

// ListMailboxes returns every mailbox ordered by rowid (insertion order) -
// see ListMagazines' doc comment for why: the identical live-addressing
// scheme applies to IOSlot.Address/Label via mailboxes, numbered
// independently from magazines.
func (s *Store) ListMailboxes() ([]config.MailboxConfig, error) {
	rows, err := s.db.Query("SELECT id, slots, pin_hash FROM mailboxes ORDER BY rowid")
	if err != nil {
		return nil, fmt.Errorf("list mailboxes: %w", err)
	}
	defer rows.Close()
	var out []config.MailboxConfig
	for rows.Next() {
		var m config.MailboxConfig
		var pinHash sql.NullString
		if err := rows.Scan(&m.ID, &m.Slots, &pinHash); err != nil {
			return nil, fmt.Errorf("scan mailbox: %w", err)
		}
		m.PINHash = pinHash.String
		out = append(out, m)
	}
	return out, rows.Err()
}

// CreateMailbox inserts a new mailbox, rejecting a duplicate ID - see
// CreateMagazine, which this mirrors exactly. Callers must validate
// slot-count constraints (config.ValidateMailbox) first.
func (s *Store) CreateMailbox(m config.MailboxConfig) error {
	if _, err := s.db.Exec("INSERT INTO mailboxes (id, slots, pin_hash) VALUES (?, ?, ?)", m.ID, m.Slots, nullIfEmpty(m.PINHash)); err != nil {
		return fmt.Errorf("mailbox %s already exists or is invalid: %w", m.ID, err)
	}
	return nil
}

// UpdateMailbox updates slots and pin_hash. Callers that only want to
// change slots (not touch the PIN) must first read the existing mailbox's
// PINHash (e.g. via ListMailboxes) and carry it forward in m - this
// function always writes whatever m.PINHash holds, it does not merge.
func (s *Store) UpdateMailbox(id string, m config.MailboxConfig) error {
	res, err := s.db.Exec("UPDATE mailboxes SET slots = ?, pin_hash = ? WHERE id = ?", m.Slots, nullIfEmpty(m.PINHash), id)
	if err != nil {
		return fmt.Errorf("update mailbox %s: %w", id, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("mailbox %s not found", id)
	}
	return nil
}

// DeleteMailbox removes a mailbox. Callers are responsible for checking its
// slots are empty first (the caller, not the store, holds the live
// slot/volume state needed to check that).
func (s *Store) DeleteMailbox(id string) error {
	res, err := s.db.Exec("DELETE FROM mailboxes WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete mailbox %s: %w", id, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("mailbox %s not found", id)
	}
	return nil
}

// SaveMailboxes replaces the whole mailboxes list. Used by the setup
// wizard; see SaveMagazines.
func (s *Store) SaveMailboxes(mbs []config.MailboxConfig) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM mailboxes"); err != nil {
		return fmt.Errorf("clear mailboxes: %w", err)
	}
	for _, m := range mbs {
		if _, err := tx.Exec("INSERT INTO mailboxes (id, slots, pin_hash) VALUES (?, ?, ?)", m.ID, m.Slots, nullIfEmpty(m.PINHash)); err != nil {
			return fmt.Errorf("save mailbox %s: %w", m.ID, err)
		}
	}
	return tx.Commit()
}

// SaveLogicalLibraries replaces the whole logical libraries list. Used by
// the setup wizard; see SaveMagazines.
func (s *Store) SaveLogicalLibraries(libs []config.LogicalLibraryConfig) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM logical_libraries"); err != nil {
		return fmt.Errorf("clear logical libraries: %w", err)
	}
	for _, lib := range libs {
		if err := saveLogicalLibraryTx(tx, lib, true); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SaveTapeSets replaces the whole tape sets list. Used by the setup wizard;
// see SaveMagazines.
func (s *Store) SaveTapeSets(sets []config.TapeSetConfig) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM tape_sets"); err != nil {
		return fmt.Errorf("clear tape sets: %w", err)
	}
	for _, ts := range sets {
		if _, err := tx.Exec("INSERT INTO tape_sets (name, tape_type, storage_folder) VALUES (?, ?, ?)",
			ts.Name, ts.TapeType, ts.StorageFolder); err != nil {
			return fmt.Errorf("save tape set %s: %w", ts.Name, err)
		}
	}
	return tx.Commit()
}

// ---- drive devices (positionally indexed, whole-list replace) ----

func (s *Store) ListDriveDevices() ([]config.DriveDeviceConfig, error) {
	rows, err := s.db.Query("SELECT device_path, drive_type FROM drive_devices ORDER BY idx")
	if err != nil {
		return nil, fmt.Errorf("list drive devices: %w", err)
	}
	defer rows.Close()
	var out []config.DriveDeviceConfig
	for rows.Next() {
		var d config.DriveDeviceConfig
		var driveType sql.NullString
		if err := rows.Scan(&d.DevicePath, &driveType); err != nil {
			return nil, fmt.Errorf("scan drive device: %w", err)
		}
		d.DriveType = driveType.String
		out = append(out, d)
	}
	return out, rows.Err()
}

// SaveDriveDevices replaces the whole drive-devices list.
func (s *Store) SaveDriveDevices(devices []config.DriveDeviceConfig) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM drive_devices"); err != nil {
		return fmt.Errorf("clear drive devices: %w", err)
	}
	for i, d := range devices {
		if _, err := tx.Exec("INSERT INTO drive_devices (idx, device_path, drive_type) VALUES (?, ?, ?)", i, d.DevicePath, nullIfEmpty(d.DriveType)); err != nil {
			return fmt.Errorf("save drive device %d: %w", i, err)
		}
	}
	return tx.Commit()
}

// nullIfEmpty maps an empty string to a genuine SQL NULL rather than
// storing an empty string for "unlinked" - keeps the column's absence
// unambiguous from a (currently unused) empty-string drive type name.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---- logical libraries ----

func (s *Store) ListLogicalLibraries() ([]config.LogicalLibraryConfig, error) {
	rows, err := s.db.Query("SELECT name, color, changer_model FROM logical_libraries ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("list logical libraries: %w", err)
	}
	defer rows.Close()
	var out []config.LogicalLibraryConfig
	for rows.Next() {
		var lib config.LogicalLibraryConfig
		if err := rows.Scan(&lib.Name, &lib.Color, &lib.ChangerModel); err != nil {
			return nil, fmt.Errorf("scan logical library: %w", err)
		}
		out = append(out, lib)
	}
	rows.Close()
	for i := range out {
		drives, err := s.queryIntColumn("SELECT drive_index FROM logical_library_drives WHERE logical_library = ? ORDER BY drive_index", out[i].Name)
		if err != nil {
			return nil, err
		}
		out[i].Drives = drives
		mags, err := s.queryStringColumn("SELECT magazine_id FROM logical_library_magazines WHERE logical_library = ? ORDER BY magazine_id", out[i].Name)
		if err != nil {
			return nil, err
		}
		out[i].Magazines = mags
		mbs, err := s.queryStringColumn("SELECT mailbox_id FROM logical_library_mailboxes WHERE logical_library = ? ORDER BY mailbox_id", out[i].Name)
		if err != nil {
			return nil, err
		}
		out[i].Mailboxes = mbs
	}
	return out, nil
}

func (s *Store) queryIntColumn(query, arg string) ([]int, error) {
	rows, err := s.db.Query(query, arg)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) queryStringColumn(query, arg string) ([]string, error) {
	rows, err := s.db.Query(query, arg)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// saveLogicalLibraryTx writes (insert or update) a logical library's row and
// fully replaces its junction rows, in the given transaction.
func saveLogicalLibraryTx(tx *sql.Tx, lib config.LogicalLibraryConfig, isCreate bool) error {
	if isCreate {
		if _, err := tx.Exec("INSERT INTO logical_libraries (name, color, changer_model) VALUES (?, ?, ?)", lib.Name, lib.Color, lib.ChangerModel); err != nil {
			return fmt.Errorf("logical library %s already exists or is invalid: %w", lib.Name, err)
		}
	} else {
		if _, err := tx.Exec("UPDATE logical_libraries SET color = ?, changer_model = ? WHERE name = ?", lib.Color, lib.ChangerModel, lib.Name); err != nil {
			return fmt.Errorf("update logical library %s: %w", lib.Name, err)
		}
	}
	if _, err := tx.Exec("DELETE FROM logical_library_drives WHERE logical_library = ?", lib.Name); err != nil {
		return err
	}
	for _, d := range lib.Drives {
		if _, err := tx.Exec("INSERT INTO logical_library_drives (logical_library, drive_index) VALUES (?, ?)", lib.Name, d); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("DELETE FROM logical_library_magazines WHERE logical_library = ?", lib.Name); err != nil {
		return err
	}
	for _, m := range lib.Magazines {
		if _, err := tx.Exec("INSERT INTO logical_library_magazines (logical_library, magazine_id) VALUES (?, ?)", lib.Name, m); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("DELETE FROM logical_library_mailboxes WHERE logical_library = ?", lib.Name); err != nil {
		return err
	}
	for _, mb := range lib.Mailboxes {
		if _, err := tx.Exec("INSERT INTO logical_library_mailboxes (logical_library, mailbox_id) VALUES (?, ?)", lib.Name, mb); err != nil {
			return err
		}
	}
	return nil
}

// CreateLogicalLibrary persists a brand-new logical library and its element
// assignments.
func (s *Store) CreateLogicalLibrary(lib config.LogicalLibraryConfig) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()
	if err := saveLogicalLibraryTx(tx, lib, true); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateLogicalLibrary replaces an existing logical library's color and
// element assignments. name is the library's current name; lib.Name may
// differ only if renaming is supported by the caller (it currently is not
// - name equality is required).
func (s *Store) UpdateLogicalLibrary(name string, lib config.LogicalLibraryConfig) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.Exec("UPDATE logical_libraries SET color = ?, changer_model = ? WHERE name = ?", lib.Color, lib.ChangerModel, name)
	if err != nil {
		return fmt.Errorf("update logical library %s: %w", name, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("logical library %s not found", name)
	}
	lib.Name = name
	if _, err := tx.Exec("DELETE FROM logical_library_drives WHERE logical_library = ?", name); err != nil {
		return err
	}
	for _, d := range lib.Drives {
		if _, err := tx.Exec("INSERT INTO logical_library_drives (logical_library, drive_index) VALUES (?, ?)", name, d); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("DELETE FROM logical_library_magazines WHERE logical_library = ?", name); err != nil {
		return err
	}
	for _, m := range lib.Magazines {
		if _, err := tx.Exec("INSERT INTO logical_library_magazines (logical_library, magazine_id) VALUES (?, ?)", name, m); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("DELETE FROM logical_library_mailboxes WHERE logical_library = ?", name); err != nil {
		return err
	}
	for _, mb := range lib.Mailboxes {
		if _, err := tx.Exec("INSERT INTO logical_library_mailboxes (logical_library, mailbox_id) VALUES (?, ?)", name, mb); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) DeleteLogicalLibrary(name string) error {
	res, err := s.db.Exec("DELETE FROM logical_libraries WHERE name = ?", name)
	if err != nil {
		return fmt.Errorf("delete logical library %s: %w", name, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("logical library %s not found", name)
	}
	return nil
}

// ---- tape sets ----

func (s *Store) ListTapeSets() ([]config.TapeSetConfig, error) {
	rows, err := s.db.Query("SELECT name, tape_type, storage_folder FROM tape_sets ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("list tape sets: %w", err)
	}
	defer rows.Close()
	var out []config.TapeSetConfig
	for rows.Next() {
		var ts config.TapeSetConfig
		if err := rows.Scan(&ts.Name, &ts.TapeType, &ts.StorageFolder); err != nil {
			return nil, fmt.Errorf("scan tape set: %w", err)
		}
		out = append(out, ts)
	}
	return out, rows.Err()
}

func (s *Store) CreateTapeSet(ts config.TapeSetConfig) error {
	_, err := s.db.Exec("INSERT INTO tape_sets (name, tape_type, storage_folder) VALUES (?, ?, ?)",
		ts.Name, ts.TapeType, ts.StorageFolder)
	if err != nil {
		return fmt.Errorf("tape set %s already exists or is invalid: %w", ts.Name, err)
	}
	return nil
}

func (s *Store) UpdateTapeSet(name string, ts config.TapeSetConfig) error {
	res, err := s.db.Exec("UPDATE tape_sets SET tape_type = ?, storage_folder = ? WHERE name = ?",
		ts.TapeType, ts.StorageFolder, name)
	if err != nil {
		return fmt.Errorf("update tape set %s: %w", name, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("tape set %s not found", name)
	}
	return nil
}

func (s *Store) DeleteTapeSet(name string) error {
	res, err := s.db.Exec("DELETE FROM tape_sets WHERE name = ?", name)
	if err != nil {
		return fmt.Errorf("delete tape set %s: %w", name, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("tape set %s not found", name)
	}
	return nil
}

// ---- whole-topology load, used at daemon startup ----

// LoadTopology reads the full library topology from the database. A
// brand-new database yields an all-zero-value LibraryConfig aside from the
// seeded catalogs and the three settings SeedDefaults() populates.
func (s *Store) LoadTopology() (config.LibraryConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var lc config.LibraryConfig
	var err error

	if lc.DriveTypes, err = s.ListDriveTypes(); err != nil {
		return lc, err
	}
	if lc.TapeTypes, err = s.ListTapeTypes(); err != nil {
		return lc, err
	}
	if lc.Magazines, err = s.ListMagazines(); err != nil {
		return lc, err
	}
	if lc.Mailboxes, err = s.ListMailboxes(); err != nil {
		return lc, err
	}
	if lc.DriveDevices, err = s.ListDriveDevices(); err != nil {
		return lc, err
	}
	if lc.LogicalLibraries, err = s.ListLogicalLibraries(); err != nil {
		return lc, err
	}
	if lc.TapeSets, err = s.ListTapeSets(); err != nil {
		return lc, err
	}

	if lc.Name, err = s.getStringSetting("vtl_name", ""); err != nil {
		return lc, err
	}
	if lc.OperationalMode, err = s.getStringSetting("operational_mode", ""); err != nil {
		return lc, err
	}
	if lc.DefaultCapacity, err = s.getStringSetting("default_capacity", "10GiB"); err != nil {
		return lc, err
	}
	if lc.Latency, err = s.GetLatencySettings(); err != nil {
		return lc, err
	}
	if lc.Cleaning, err = s.GetCleaningSettings(); err != nil {
		return lc, err
	}
	if lc.OffsiteLocation, err = s.getBoolSetting("offsite_location", false); err != nil {
		return lc, err
	}
	if lc.OffsiteRotationInterval, err = s.getStringSetting("offsite_rotation_interval", ""); err != nil {
		return lc, err
	}
	if lc.OffsiteRotationCount, err = s.getIntSetting("offsite_rotation_count", 0); err != nil {
		return lc, err
	}
	if lc.MagazinePINHash, err = s.getStringSetting("magazine_pin_hash", ""); err != nil {
		return lc, err
	}
	return lc, nil
}

// DaemonSettings is the subset of config.Config that isn't "library"
// topology but still, as of this rewrite, no longer lives in config.yaml
// either (see internal/config.Config's doc comment) - it's read once at
// daemon startup from here instead, and updated live through the Admin
// Settings API (internal/api/settings.go), the same as LibraryConfig's
// settings fields above.
type DaemonSettings struct {
	SNMP            config.SNMPConfig
	Prometheus      config.PrometheusConfig
	PollIntervalRaw string
	LogLevel        string
}

// LoadDaemonSettings reads DaemonSettings from the database. Called once at
// startup (main.go); every field here hot-applies afterwards through
// Settings.Update without needing to be re-read.
func (s *Store) LoadDaemonSettings() (DaemonSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var ds DaemonSettings
	var err error

	if ds.SNMP.Enabled, err = s.getBoolSetting("snmp_enabled", false); err != nil {
		return ds, err
	}
	if ds.SNMP.EnterpriseOID, err = s.getStringSetting("snmp_enterprise_oid", "1.3.6.1.4.1.55555.1"); err != nil {
		return ds, err
	}
	if ds.SNMP.AgentAddress, err = s.getStringSetting("snmp_agent_address", "127.0.0.1"); err != nil {
		return ds, err
	}
	targetsJSON, err := s.getStringSetting("snmp_targets", "[]")
	if err != nil {
		return ds, err
	}
	if targetsJSON != "" {
		if err := json.Unmarshal([]byte(targetsJSON), &ds.SNMP.Targets); err != nil {
			return ds, fmt.Errorf("parse snmp_targets: %w", err)
		}
	}
	if ds.Prometheus.Enabled, err = s.getBoolSetting("prometheus_enabled", false); err != nil {
		return ds, err
	}
	if ds.PollIntervalRaw, err = s.getStringSetting("poll_interval", "5s"); err != nil {
		return ds, err
	}
	if ds.LogLevel, err = s.getStringSetting("log_level", "info"); err != nil {
		return ds, err
	}
	return ds, nil
}

// SetSNMPTargets persists the SNMP target list as JSON, used by
// Settings.Update.
func (s *Store) SetSNMPTargets(targets []config.SNMPTarget) error {
	if targets == nil {
		targets = []config.SNMPTarget{}
	}
	data, err := json.Marshal(targets)
	if err != nil {
		return fmt.Errorf("marshal snmp_targets: %w", err)
	}
	return s.SetSetting("snmp_targets", string(data))
}

// Wizard progress (the "wizard_current_step"/"wizard_completed" singletons)
// is read and written by internal/api's own loadWizardState/
// saveWizardProgress, straight through GetSetting/SetSetting above. This
// file used to carry a parallel WizardProgress type with its own
// Load/SaveWizardProgress pair, which nothing ever called - removed rather
// than left as a second, drifting definition of the same two keys.
