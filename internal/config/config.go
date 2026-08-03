// Package config loads and validates the gotochanger daemon configuration.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/swenske/gotochanger/internal/barcode"
	"gopkg.in/yaml.v3"
)

// SNMPTarget is a single SNMP trap receiver.
type SNMPTarget struct {
	Host      string `yaml:"host" json:"host"`
	Port      int    `yaml:"port" json:"port"`
	Community string `yaml:"community" json:"community"`
	Version   string `yaml:"version" json:"version"` // "2c" (only supported version today)
}

// SNMPConfig configures trap emission.
type SNMPConfig struct {
	Enabled       bool         `yaml:"enabled" json:"enabled"`
	Targets       []SNMPTarget `yaml:"targets" json:"targets"`
	EnterpriseOID string       `yaml:"enterprise_oid" json:"enterprise_oid"`
	AgentAddress  string       `yaml:"agent_address" json:"agent_address"`
}

// PrometheusConfig controls the /metrics exporter. Enabled defaults to
// false, matching every other optional feature (SNMP, latency simulation,
// cleaning) - an operator opts in explicitly.
type PrometheusConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
}

// ListenConfig configures the two listeners exposed by the daemon.
type ListenConfig struct {
	HTTP        string `yaml:"http" json:"http"`               // TCP address for the token-authenticated API + Web UI
	UnixSocket  string `yaml:"unix_socket" json:"unix_socket"` // Unix socket for trusted local clients (CLI shim), no token required
	SocketMode  string `yaml:"socket_mode" json:"socket_mode"` // octal file mode for the unix socket, e.g. "0660"
	SocketGroup string `yaml:"socket_group" json:"socket_group"`
}

// DriveType defines the properties of a drive type.
type DriveType struct {
	Name        string `yaml:"name" json:"name"`
	Speed       string `yaml:"speed" json:"speed"`       // e.g., "100MB/s"
	Capacity    string `yaml:"capacity" json:"capacity"` // e.g., "10GiB"
	Description string `yaml:"description" json:"description"`
	Model       string `yaml:"model" json:"model,omitempty"`           // e.g., "IBM TS1160"
	Generation  string `yaml:"generation" json:"generation,omitempty"` // e.g., "LTO-9"
}

// DriveDeviceConfig describes one physical drive device: its backing device
// path plus, optionally, which DriveType catalog entry it's linked to (by
// name, no FK - same "constrain new writes only, never retroactively"
// convention TapeSetConfig.TapeType already uses). The link is what lets
// Admin > Drives display a real model/generation/capacity instead of just
// an index and a path.
type DriveDeviceConfig struct {
	DevicePath string `yaml:"device_path" json:"device_path"`
	DriveType  string `yaml:"drive_type" json:"drive_type,omitempty"`
}

// TapeType defines the properties of a tape/media type. Tracked separately
// from DriveType: a tape set groups cartridges by media type (LTOx, DDSx,
// DLTxxxx), independent of which physical drive happens to read them, even
// though the two catalogs often share names (an "LTO-8" tape read by an
// "LTO-8" drive). BarcodeFamily/MediaID/VolSerLength define this tape
// type's barcode format (see internal/barcode) - every cartridge created
// for a tape set of this type gets a barcode conforming to this format.
type TapeType struct {
	Name          string `yaml:"name" json:"name"`
	Capacity      string `yaml:"capacity" json:"capacity"`
	Description   string `yaml:"description" json:"description"`
	BarcodeFamily string `yaml:"barcode_family" json:"barcode_family"`
	MediaID       string `yaml:"media_id" json:"media_id,omitempty"`
	VolSerLength  int    `yaml:"volser_length" json:"volser_length"`
}

// MagazineConfig describes a magazine with configurable slots. It carries
// no address of its own - Slot.Address/Slot.Label are computed live from
// this magazine's position among Config.Library.Magazines and its Slots
// count (see library.Library.buildTopologyLocked), not stored here.
type MagazineConfig struct {
	ID    string `yaml:"id" json:"id"`
	Slots int    `yaml:"slots" json:"slots"`
}

// MailboxConfig describes a mailbox (I/O door) with configurable slots,
// exactly mirroring MagazineConfig — mailboxes are real, independently
// addressable groups of I/O slots, not just a flat count.
type MailboxConfig struct {
	ID    string `yaml:"id" json:"id"`
	Slots int    `yaml:"slots" json:"slots"`

	// PINHash is this mailbox's own PIN-code hash (see secrethash), or
	// empty if no PIN is configured for it - presence implies protection,
	// there is no separate enabled/disabled flag. Never serialized: the API
	// layer exposes only a PINSet bool, mirroring how UserInfo excludes
	// PasswordHash.
	PINHash string `yaml:"-" json:"-"`
}

// LogicalLibraryConfig describes a logical library partition.
type LogicalLibraryConfig struct {
	Name      string   `yaml:"name" json:"name"`
	Drives    []int    `yaml:"drives" json:"drives"`       // Indices of assigned drives
	Magazines []string `yaml:"magazines" json:"magazines"` // IDs of assigned magazines
	Mailboxes []string `yaml:"mailboxes" json:"mailboxes"` // IDs of assigned mailboxes
	Color     string   `yaml:"color" json:"color"`         // Color for UI identification
}

// TapeSetConfig groups cartridges of one tape type under a dedicated
// storage folder.
type TapeSetConfig struct {
	Name          string `yaml:"name" json:"name"`
	TapeType      string `yaml:"tape_type" json:"tape_type"`
	StorageFolder string `yaml:"storage_folder" json:"storage_folder"`
}

// UserRow/TokenRow are the SQLite-backed replacement for what used to be
// users.json/tokens.json - neutral DTOs (like DriveType/TapeType above)
// shared between internal/store (which persists them) and internal/api
// (which owns the actual UserStore/TokenStore business logic: hashing,
// rate-limiting, RBAC role types). Named "*Row" to stay distinct from
// internal/api's own UserInfo (the public, hash-free API/UI view) and
// TokenRecord (the API response DTO). Role is a plain string here rather
// than internal/api.Role, since internal/config must not import
// internal/api (internal/api already imports internal/config, and
// internal/library imports internal/config too - importing api back would
// create a cycle).
type UserRow struct {
	Username           string
	PasswordHash       string
	Role               string
	MustChangePassword bool
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type TokenRow struct {
	Name      string
	Hash      string
	Role      string
	CreatedAt time.Time
}

// LatencySettings defines the simulated timing delays applied across the
// whole physical library (every logical library sharing one physical
// gotochangerd instance shares one timing model - see internal/library's
// Library struct, which is daemon-wide, not per logical library). All
// durations are human-friendly strings (e.g. "2s", "500ms"), parsed via
// ParseDuration. These represent simulated delays for exercising backup
// software against realistic tape-library timing, not measurements of
// real hardware.
type LatencySettings struct {
	Enabled         bool   `yaml:"enabled" json:"enabled"`
	DriveLoad       string `yaml:"drive_load" json:"drive_load"`             // mechanical load of a cartridge into a drive
	DriveUnload     string `yaml:"drive_unload" json:"drive_unload"`         // mechanical unload of a cartridge from a drive
	TapePositioning string `yaml:"tape_positioning" json:"tape_positioning"` // seek/positioning inside the drive after load, before ready
	RobotMoveTape   string `yaml:"robot_move_tape" json:"robot_move_tape"`   // arm movement for a slot/ioslot/drive tape move
	RobotMoveScan   string `yaml:"robot_move_scan" json:"robot_move_scan"`   // arm movement while scanning a magazine after it's closed
	MagazineScan    string `yaml:"magazine_scan" json:"magazine_scan"`       // barcode/inventory scan time after a magazine is closed
	DoorAction      string `yaml:"door_action" json:"door_action"`           // mailbox/storage door open or close movement time
}

// DefaultLatencySettings returns conservative, realistic factory-default
// delays, used to seed a fresh install and to power the Admin > Latency
// page's "Load defaults" button. Latency simulation is disabled by
// default (Enabled: false), matching the previous design's behavior of
// having no latency configured until an admin opts in.
func DefaultLatencySettings() LatencySettings {
	return LatencySettings{
		Enabled:         false,
		DriveLoad:       "8s",
		DriveUnload:     "6s",
		TapePositioning: "4s",
		RobotMoveTape:   "5s",
		RobotMoveScan:   "6s",
		MagazineScan:    "12s",
		DoorAction:      "2s",
	}
}

// CleaningSettings defines the cleaning-tape management feature: whether
// it's enabled, which of the two operating modes is active (see Mode's
// doc comment), and the three global tunables (max uses per cartridge,
// drive mounts before a cleaning cycle is required, cleaning cycle
// duration). Applies to the whole physical library, like LatencySettings.
type CleaningSettings struct {
	Enabled        bool   `yaml:"enabled" json:"enabled"`
	Mode           string `yaml:"mode" json:"mode"`                       // "backup_software" or "backup_robot"
	MaxUses        int    `yaml:"max_uses" json:"max_uses"`               // uses before a cleaning cartridge expires
	MountThreshold int    `yaml:"mount_threshold" json:"mount_threshold"` // drive mounts before a cleaning cycle is required
	Duration       string `yaml:"duration" json:"duration"`               // simulated cleaning cycle duration, e.g. "2m"
}

// Cleaning operating modes: CleaningModeSoftware means cleaning cartridges
// sit in a magazine attached to a logical library the backup software
// uses, and the backup software itself decides when to mount/unmount them
// (gotochangerd only tracks usage/expiry). CleaningModeRobot means
// cleaning cartridges sit in a magazine not assigned to any logical
// library - structurally invisible to Bareos - so gotochangerd's own
// automatic sweep performs the cleaning cycle itself.
const (
	CleaningModeSoftware = "backup_software"
	CleaningModeRobot    = "backup_robot"
)

// DefaultCleaningSettings returns conservative, realistic factory-default
// values, used to seed a fresh install and to power the Admin > Cleaning
// Tapes page's "Load defaults" button. Disabled by default, matching
// DefaultLatencySettings' convention of leaving simulated-timing features
// off until an admin opts in.
func DefaultCleaningSettings() CleaningSettings {
	return CleaningSettings{
		Enabled:        false,
		Mode:           CleaningModeSoftware,
		MaxUses:        20,
		MountThreshold: 50,
		Duration:       "2m",
	}
}

// ValidateCleaningSettings checks that Mode is one of the two known
// values and that MaxUses/MountThreshold/Duration fall within sane
// ranges. A zero-value CleaningSettings (an entirely empty struct) is
// left alone and always passes, the same "not yet configured" allowance
// ValidateLatencySettings makes per-field - Config.Validate runs against
// whatever config.yaml carries before the database's topology (including
// Cleaning) has been loaded and merged in (see Load's doc comment), so a
// fresh install's zero-value Library.Cleaning must not fail validation
// here. Once SeedDefaults or the Admin API writes a real value, it's
// always the complete struct, so full validation applies from then on.
func ValidateCleaningSettings(cs CleaningSettings) error {
	if cs == (CleaningSettings{}) {
		return nil
	}
	if cs.Mode != CleaningModeSoftware && cs.Mode != CleaningModeRobot {
		return fmt.Errorf("mode must be %q or %q, got %q", CleaningModeSoftware, CleaningModeRobot, cs.Mode)
	}
	if cs.MaxUses < 1 || cs.MaxUses > 100 {
		return fmt.Errorf("max_uses must be between 1 and 100")
	}
	if cs.MountThreshold < 1 || cs.MountThreshold > 1000 {
		return fmt.Errorf("mount_threshold must be between 1 and 1000")
	}
	d, err := ParseDuration(cs.Duration)
	if err != nil {
		return fmt.Errorf("duration: %w", err)
	}
	if d < 0 || d > 30*time.Minute {
		return fmt.Errorf("duration: must be between 0s and 30m")
	}
	return nil
}

// LibraryConfig describes the topology of the simulated autochanger. Every
// field here is sourced from the SQLite-backed topology store (see
// internal/store/topology.go), not from this YAML file — a fresh install
// starts with a zero-value LibraryConfig (nothing configured) until the
// setup wizard or the Admin API populates it. The yaml tags remain so the
// on-disk config.yaml still shows a human-readable snapshot, but Load()
// discards whatever is under `library:` in the file and never treats it as
// authoritative.
type LibraryConfig struct {
	Name                    string                 `yaml:"name" json:"name"`
	OperationalMode         string                 `yaml:"operational_mode" json:"operational_mode"`
	DriveTypes              []DriveType            `yaml:"drive_types" json:"drive_types"`
	TapeTypes               []TapeType             `yaml:"tape_types" json:"tape_types"`
	Magazines               []MagazineConfig       `yaml:"magazines" json:"magazines"`
	Mailboxes               []MailboxConfig        `yaml:"mailboxes" json:"mailboxes"`
	DriveDevices            []DriveDeviceConfig    `yaml:"drive_devices" json:"drive_devices"`
	DefaultCapacity         string                 `yaml:"default_capacity" json:"default_capacity"`
	LogicalLibraries        []LogicalLibraryConfig `yaml:"logical_libraries" json:"logical_libraries"`
	TapeSets                []TapeSetConfig        `yaml:"tape_sets" json:"tape_sets"`
	Latency                 LatencySettings        `yaml:"latency" json:"latency"`
	Cleaning                CleaningSettings       `yaml:"cleaning" json:"cleaning"`
	OffsiteLocation         bool                   `yaml:"offsite_location" json:"offsite_location"`
	OffsiteRotationInterval string                 `yaml:"offsite_rotation_interval" json:"offsite_rotation_interval"`
	OffsiteRotationCount    int                    `yaml:"offsite_rotation_count" json:"offsite_rotation_count"`

	// MagazinePINHash is the single PIN-code hash (see secrethash) that
	// applies to every magazine's storage door, or empty if no PIN is
	// configured - presence implies protection, there is no separate
	// enabled/disabled flag. Never serialized: the API layer exposes only a
	// "configured" bool.
	MagazinePINHash string `yaml:"-" json:"-"`
}

// Config is the top level daemon configuration. Only DataDir and Listen are
// actually sourced from config.yaml on disk (see Load) - every other field
// here is populated at startup from the SQLite-backed store
// (internal/store/topology.go's LoadTopology/LoadDaemonSettings) and kept in
// sync there by Settings.Update. DataDir is the one unavoidable exception to
// "everything lives in the database": it's the path used to *locate* the
// database itself, so it cannot itself come from the database.
type Config struct {
	DataDir         string           `yaml:"data_dir" json:"data_dir"`
	Listen          ListenConfig     `yaml:"listen" json:"listen"`
	Library         LibraryConfig    `yaml:"-" json:"library"`
	SNMP            SNMPConfig       `yaml:"-" json:"snmp"`
	Prometheus      PrometheusConfig `yaml:"-" json:"prometheus"`
	PollInterval    time.Duration    `yaml:"-" json:"-"`
	PollIntervalRaw string           `yaml:"-" json:"poll_interval"`
	LogLevel        string           `yaml:"-" json:"log_level"`
}

// DefaultTokensFile and DefaultUsersFile are not a runtime setting - they're
// the fixed paths a pre-SQLite install used to keep users.json/tokens.json
// at. gotochangerd checks these once at startup (main.go, via
// store.Store.MigrateUsersAndTokensFromJSON) to auto-import their content
// into the database verbatim the first time the users/tokens tables are
// empty; a no-op on every later restart and on a fresh install with no
// legacy files.
const (
	DefaultTokensFile = "/etc/gotochanger/tokens.json"
	DefaultUsersFile  = "/etc/gotochanger/users.json"
)

// Default returns a Config populated with sane defaults for service-level
// settings. Library (topology) is deliberately left at its zero value —
// nothing is pre-configured. It's populated from the SQLite-backed
// topology store (internal/store/topology.go's LoadTopology/SeedDefaults),
// which is what actually seeds the drive/tape type catalogs and lets the
// setup wizard start from a genuinely empty library.
func Default() Config {
	return Config{
		DataDir: "/var/lib/gotochanger",
		Listen: ListenConfig{
			HTTP:        "0.0.0.0:8480",
			UnixSocket:  "/run/gotochanger/gotochanger.sock",
			SocketMode:  "0660",
			SocketGroup: "gotochanger",
		},
		SNMP: SNMPConfig{
			Enabled:       false,
			EnterpriseOID: "1.3.6.1.4.1.55555.1",
			AgentAddress:  "127.0.0.1",
		},
		Prometheus:      PrometheusConfig{Enabled: false},
		PollIntervalRaw: "5s",
		LogLevel:        "info",
	}
}

// Load reads and validates a YAML configuration file. Only DataDir and
// Listen carry `yaml` tags (see the Config doc comment), so this only ever
// reads those two things from the file - every other field is left at
// Default()'s value here and must be overwritten by the caller from the
// database (Store.LoadTopology/LoadDaemonSettings) once it's open, which is
// what main.go does. Any leftover `library:`/`snmp:`/`tokens_file:`/etc.
// section in an old config.yaml (e.g. from a pre-0.3.0 install) is silently
// ignored rather than erroring, since yaml.Unmarshal skips fields with no
// matching tag.
func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
	}
	d, err := time.ParseDuration(cfg.PollIntervalRaw)
	if err != nil {
		return cfg, fmt.Errorf("invalid poll_interval %q: %w", cfg.PollIntervalRaw, err)
	}
	cfg.PollInterval = d
	return cfg, cfg.Validate()
}

// Validate performs basic sanity checks on the service-level settings so
// obviously broken configuration is rejected at startup rather than causing
// confusing failures later. It deliberately does not require any library
// topology to be present — a fresh install legitimately has none until the
// setup wizard (or the Admin API) creates it; per-entity constraints for
// magazines/logical libraries/etc. are enforced by ValidateMagazine/
// ValidateMailbox/ValidateLogicalLibrary at the point each one is written.
func (c Config) Validate() error {
	if c.DataDir == "" {
		return fmt.Errorf("data_dir must be set")
	}
	if c.Listen.HTTP == "" && c.Listen.UnixSocket == "" {
		return fmt.Errorf("at least one of listen.http or listen.unix_socket must be set")
	}
	if c.Library.DefaultCapacity != "" {
		if _, err := ParseSize(c.Library.DefaultCapacity); err != nil {
			return fmt.Errorf("library.default_capacity: %w", err)
		}
	}
	if err := ValidateLatencySettings(c.Library.Latency); err != nil {
		return fmt.Errorf("library.latency: %w", err)
	}
	if err := ValidateCleaningSettings(c.Library.Cleaning); err != nil {
		return fmt.Errorf("library.cleaning: %w", err)
	}
	return nil
}

// ValidateLatencySettings checks that every configured delay parses as a
// duration within a sane range (0 to 5 minutes). An empty field is left
// alone (not an error) so a zero-value LatencySettings - what a fresh
// install has before SeedDefaults/the wizard/Admin > Latency has ever
// written one - always passes; once the Admin API writes real values
// (always all 7 fields at once, see internal/api's latency settings
// handler), each one is checked.
func ValidateLatencySettings(ls LatencySettings) error {
	fields := []struct {
		name string
		val  string
	}{
		{"drive_load", ls.DriveLoad},
		{"drive_unload", ls.DriveUnload},
		{"tape_positioning", ls.TapePositioning},
		{"robot_move_tape", ls.RobotMoveTape},
		{"robot_move_scan", ls.RobotMoveScan},
		{"magazine_scan", ls.MagazineScan},
		{"door_action", ls.DoorAction},
	}
	for _, f := range fields {
		if f.val == "" {
			continue
		}
		d, err := ParseDuration(f.val)
		if err != nil {
			return fmt.Errorf("%s: %w", f.name, err)
		}
		if d < 0 || d > 5*time.Minute {
			return fmt.Errorf("%s: must be between 0s and 5m", f.name)
		}
	}
	return nil
}

// ValidateMagazine checks a single magazine's slot-count constraints (5-20
// slots, in increments of 5), used by the topology store's write path and
// the wizard/Admin API before a magazine is saved.
func ValidateMagazine(m MagazineConfig) error {
	if m.Slots < 5 || m.Slots > 20 {
		return fmt.Errorf("magazine %s: slots must be between 5 and 20", m.ID)
	}
	if m.Slots%5 != 0 {
		return fmt.Errorf("magazine %s: slots must be in increments of 5", m.ID)
	}
	return nil
}

// ValidateMailbox checks a single mailbox's slot-count constraint (1-5
// slots), used by the topology store's write path and the wizard/Admin API
// before a mailbox is saved. Mirrors ValidateMagazine; a mailbox door is
// physically small, so the per-mailbox cap is tighter than a magazine's.
func ValidateMailbox(m MailboxConfig) error {
	if m.Slots < 1 || m.Slots > 5 {
		return fmt.Errorf("mailbox %s: slots must be between 1 and 5", m.ID)
	}
	return nil
}

// ValidateLogicalLibrary checks that a logical library has at least one
// drive and one magazine assigned, used by the topology store's write path
// and the wizard/Admin API before a logical library is saved (a logical
// library that partitions no storage isn't meaningful). Mailboxes are
// deliberately optional - a deployment that never needs import/export I/O
// for a given logical library shouldn't be forced to have one.
func ValidateLogicalLibrary(lib LogicalLibraryConfig) error {
	if len(lib.Drives) < 1 {
		return fmt.Errorf("logical library %s must have at least one drive", lib.Name)
	}
	if len(lib.Magazines) < 1 {
		return fmt.Errorf("logical library %s must have at least one magazine", lib.Name)
	}
	return nil
}

// ValidateTapeType checks a tape type's capacity and barcode-format
// definition, used by the topology store's write path and the wizard/Admin
// API before a tape type is saved.
func ValidateTapeType(tt TapeType) error {
	if strings.TrimSpace(tt.Name) == "" {
		return fmt.Errorf("tape type name must not be empty")
	}
	if tt.Capacity != "unlimited" {
		if _, err := ParseSize(tt.Capacity); err != nil {
			return fmt.Errorf("tape type %s: capacity: %w", tt.Name, err)
		}
	}
	spec, err := barcode.SpecFor(tt.BarcodeFamily, tt.MediaID, tt.VolSerLength)
	if err != nil {
		return fmt.Errorf("tape type %s: %w", tt.Name, err)
	}
	if err := barcode.ValidateSpec(spec); err != nil {
		return fmt.Errorf("tape type %s: barcode format: %w", tt.Name, err)
	}
	return nil
}

// tapeSetNameRE restricts tape set names to safe characters, since a tape
// set's name is also used to derive its default storage-folder suggestion.
var tapeSetNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// ValidateTapeSet checks a tape set's name and, when knownTapeTypes is
// non-nil, that it references a tape type that actually exists (pass nil to
// skip that check in contexts that haven't loaded the catalog). The storage
// folder's absolute-path requirement is checked separately by the API layer
// (internal/api's validateTapeSetFolder), since that check has the
// side effect of creating the directory and doesn't belong in a pure
// validator.
func ValidateTapeSet(ts TapeSetConfig, knownTapeTypes map[string]bool) error {
	if !tapeSetNameRE.MatchString(ts.Name) {
		return fmt.Errorf("tape set name %q must be 1-32 characters from [A-Za-z0-9_-]", ts.Name)
	}
	if ts.TapeType == "" {
		return fmt.Errorf("tape set %s: tape_type is required", ts.Name)
	}
	if knownTapeTypes != nil && !knownTapeTypes[ts.TapeType] {
		return fmt.Errorf("tape set %s: tape type %q does not exist", ts.Name, ts.TapeType)
	}
	return nil
}

// DefaultDriveTypes returns the suggested drive-type catalog used to seed a
// brand-new installation's topology store.
func DefaultDriveTypes() []DriveType {
	return []DriveType{
		{Name: "Unlimited", Speed: "unlimited", Capacity: "unlimited", Description: "Unlimited capacity/performance", Model: "Unlimited", Generation: "Unlimited"},
		{Name: "LTO-8", Speed: "300MB/s", Capacity: "12TB", Description: "LTO-8 tape drive", Model: "LTO Ultrium 8", Generation: "LTO-8"},
		{Name: "LTO-9", Speed: "400MB/s", Capacity: "18TB", Description: "LTO-9 tape drive", Model: "LTO Ultrium 9", Generation: "LTO-9"},
		{Name: "DDS", Speed: "12MB/s", Capacity: "80GB", Description: "DDS (DAT) tape drive", Model: "DDS", Generation: "DDS"},
		{Name: "DLT", Speed: "10MB/s", Capacity: "40GB", Description: "DLT tape drive", Model: "DLT", Generation: "DLT"},
	}
}

// DefaultTapeTypes returns the suggested tape/media-type catalog used to
// seed a brand-new installation's topology store. Barcode formats follow
// published vendor specs where one exists (LTO, DLT, SDLT); DDS/AIT/3592
// have no published external barcode standard, so these use gotochanger's
// own convention (mirroring the LTO/SDLT 6-char-volser+2-char-media-id
// shape for consistency - see internal/barcode's package doc).
func DefaultTapeTypes() []TapeType {
	return []TapeType{
		{Name: "Unlimited", Capacity: "unlimited", Description: "Unlimited capacity, limited only by disk", BarcodeFamily: "generic", VolSerLength: 8},
		{Name: "LTO-1", Capacity: "100GB", Description: "LTO-1 cartridge", BarcodeFamily: "lto", MediaID: "L1", VolSerLength: 6},
		{Name: "LTO-2", Capacity: "200GB", Description: "LTO-2 cartridge", BarcodeFamily: "lto", MediaID: "L2", VolSerLength: 6},
		{Name: "LTO-3", Capacity: "400GB", Description: "LTO-3 cartridge", BarcodeFamily: "lto", MediaID: "L3", VolSerLength: 6},
		{Name: "LTO-4", Capacity: "800GB", Description: "LTO-4 cartridge", BarcodeFamily: "lto", MediaID: "L4", VolSerLength: 6},
		{Name: "LTO-5", Capacity: "1.5TB", Description: "LTO-5 cartridge", BarcodeFamily: "lto", MediaID: "L5", VolSerLength: 6},
		{Name: "LTO-6", Capacity: "2.5TB", Description: "LTO-6 cartridge", BarcodeFamily: "lto", MediaID: "L6", VolSerLength: 6},
		{Name: "LTO-7", Capacity: "6TB", Description: "LTO-7 cartridge", BarcodeFamily: "lto", MediaID: "L7", VolSerLength: 6},
		{Name: "LTO-7 Type M", Capacity: "9TB", Description: "LTO-7 media reformatted for use only in LTO-8 drives", BarcodeFamily: "lto", MediaID: "M8", VolSerLength: 6},
		{Name: "LTO-8", Capacity: "12TB", Description: "LTO-8 cartridge", BarcodeFamily: "lto", MediaID: "L8", VolSerLength: 6},
		{Name: "LTO-9", Capacity: "18TB", Description: "LTO-9 cartridge", BarcodeFamily: "lto", MediaID: "L9", VolSerLength: 6},
		{Name: "DLT-III", Capacity: "15GB", Description: "DLT-III cartridge", BarcodeFamily: "dlt", MediaID: "3", VolSerLength: 6},
		{Name: "DLT-IV", Capacity: "40GB", Description: "DLT-IV cartridge", BarcodeFamily: "dlt", MediaID: "4", VolSerLength: 6},
		{Name: "SDLT-220", Capacity: "220GB", Description: "SDLT1 (220GB) cartridge", BarcodeFamily: "sdlt", MediaID: "S1", VolSerLength: 6},
		{Name: "SDLT-600", Capacity: "300GB", Description: "SDLT2 (600GB compressed) cartridge", BarcodeFamily: "sdlt", MediaID: "S2", VolSerLength: 6},
		{Name: "DDS-1", Capacity: "2GB", Description: "DDS-1 (DAT) cartridge", BarcodeFamily: "dds", MediaID: "D1", VolSerLength: 6},
		{Name: "DDS-2", Capacity: "4GB", Description: "DDS-2 (DAT) cartridge", BarcodeFamily: "dds", MediaID: "D2", VolSerLength: 6},
		{Name: "DDS-3", Capacity: "12GB", Description: "DDS-3 (DAT) cartridge", BarcodeFamily: "dds", MediaID: "D3", VolSerLength: 6},
		{Name: "DDS-4", Capacity: "20GB", Description: "DDS-4 (DAT) cartridge", BarcodeFamily: "dds", MediaID: "D4", VolSerLength: 6},
		{Name: "DAT-72", Capacity: "36GB", Description: "DAT-72 cartridge", BarcodeFamily: "dds", MediaID: "D5", VolSerLength: 6},
		{Name: "DAT-160", Capacity: "80GB", Description: "DAT-160 cartridge", BarcodeFamily: "dds", MediaID: "D6", VolSerLength: 6},
		{Name: "DAT-320", Capacity: "160GB", Description: "DAT-320 cartridge", BarcodeFamily: "dds", MediaID: "D7", VolSerLength: 6},
		{Name: "AIT-1", Capacity: "25GB", Description: "AIT-1 cartridge", BarcodeFamily: "ait", MediaID: "A1", VolSerLength: 6},
		{Name: "AIT-2", Capacity: "50GB", Description: "AIT-2 cartridge", BarcodeFamily: "ait", MediaID: "A2", VolSerLength: 6},
		{Name: "AIT-3", Capacity: "100GB", Description: "AIT-3 cartridge", BarcodeFamily: "ait", MediaID: "A3", VolSerLength: 6},
		{Name: "AIT-4", Capacity: "200GB", Description: "AIT-4 cartridge", BarcodeFamily: "ait", MediaID: "A4", VolSerLength: 6},
		{Name: "SAIT-1", Capacity: "500GB", Description: "SAIT-1 cartridge", BarcodeFamily: "ait", MediaID: "A5", VolSerLength: 6},
		{Name: "IBM-3592", Capacity: "7TB", Description: "IBM 3592 cartridge (single representative generation)", BarcodeFamily: "3592", MediaID: "J1", VolSerLength: 6},
	}
}

var sizeRE = regexp.MustCompile(`(?i)^\s*([0-9]+(?:\.[0-9]+)?)\s*([kmgt]i?b?)?\s*$`)
var durationRE = regexp.MustCompile(`(?i)^\s*([0-9]+(?:\.[0-9]+)?)\s*([mhd]s?|s)\s*$`)

// ParseSize parses human friendly sizes such as "10GiB", "500MB", "2TiB" or a
// plain byte count, returning the number of bytes.
func ParseSize(s string) (int64, error) {
	m := sizeRE.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	val, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", s, err)
	}
	unit := strings.ToLower(m[2])
	var mult float64
	switch {
	case unit == "" || unit == "b":
		mult = 1
	case strings.HasPrefix(unit, "k"):
		mult = mul(unit, 1000, 1024)
	case strings.HasPrefix(unit, "m"):
		mult = mul(unit, 1000*1000, 1024*1024)
	case strings.HasPrefix(unit, "g"):
		mult = mul(unit, 1000*1000*1000, 1024*1024*1024)
	case strings.HasPrefix(unit, "t"):
		mult = mul(unit, 1000*1000*1000*1000, 1024*1024*1024*1024)
	default:
		return 0, fmt.Errorf("unknown size unit in %q", s)
	}
	return int64(val * mult), nil
}

// ParseDuration parses human friendly durations such as "1s", "500ms", "2h", etc.
func ParseDuration(s string) (time.Duration, error) {
	if s == "unlimited" {
		return 0, nil
	}
	m := durationRE.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	val, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", s, err)
	}
	unit := strings.ToLower(m[2])
	var mult time.Duration
	switch unit {
	case "ns":
		mult = time.Nanosecond
	case "us":
		mult = time.Microsecond
	case "ms":
		mult = time.Millisecond
	case "s":
		mult = time.Second
	case "m":
		mult = time.Minute
	case "h":
		mult = time.Hour
	case "d":
		mult = time.Hour * 24
	default:
		return 0, fmt.Errorf("unknown duration unit in %q", s)
	}
	return time.Duration(val * float64(mult)), nil
}

func mul(unit string, decimal, binary float64) float64 {
	if strings.Contains(unit, "i") {
		return binary
	}
	return decimal
}
