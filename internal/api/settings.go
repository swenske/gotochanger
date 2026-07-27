package api

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/swenske/gotochanger/internal/config"
	"github.com/swenske/gotochanger/internal/library"
	"github.com/swenske/gotochanger/internal/snmp"
)

// Settings exposes the admin "everything we can configure" section: it
// holds the effective configuration, persists every change to the topology
// store (the sole source of truth for everything except Listen/DataDir -
// see internal/store/topology.go and config.Config's doc comment), and
// live-applies whichever fields can safely change without a daemon restart.
type Settings struct {
	mu       sync.Mutex
	cfg      config.Config
	lib      *library.Library
	snmp     *snmp.Sender
	logLevel *slog.LevelVar
	topology TopologyStore
}

// NewSettings builds a Settings manager. topology may be nil in tests that
// don't exercise setting persistence.
func NewSettings(cfg config.Config, lib *library.Library, sender *snmp.Sender, logLevel *slog.LevelVar, topology TopologyStore) *Settings {
	return &Settings{cfg: cfg, lib: lib, snmp: sender, logLevel: logLevel, topology: topology}
}

// Current returns the effective configuration, with the store-backed
// singleton settings re-read first (see refreshStoreBackedLocked).
func (s *Settings) Current() config.Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshStoreBackedLocked()
	return s.cfg
}

// refreshStoreBackedLocked re-reads the Library settings that other code
// paths write straight to the topology store, bypassing Settings entirely:
// the setup wizard (vtl_name, offsite_location), and the PIN handler
// (magazine_pin_hash). Callers must hold s.mu.
//
// s.cfg is otherwise only ever populated once at daemon startup and then by
// Update itself - reconfigureFromStore refreshes Server.cfg, not this one -
// so without this it goes stale the moment the wizard runs. That was doing
// real damage in two directions. Reading: Admin > Settings and the manual
// backup's filename both showed the boot-time vtl_name (empty, on a fresh
// install) rather than the one the wizard had just set. Writing: Update
// persists the full resulting config, so the first settings save of any
// kind - even one only touching the log level - wrote that stale vtl_name,
// offsite_location and default_capacity straight back over what the wizard
// had stored. CurrentLatency/CurrentCleaning already each did their own
// narrower version of this for exactly the same reason; this generalizes it
// so the flat settings blob stops being the odd one out.
func (s *Settings) refreshStoreBackedLocked() {
	if s.topology == nil {
		return
	}
	if v, ok, err := s.topology.GetSetting("vtl_name"); err == nil && ok {
		s.cfg.Library.Name = v
	}
	if v, ok, err := s.topology.GetSetting("default_capacity"); err == nil && ok {
		s.cfg.Library.DefaultCapacity = v
	}
	if v, ok, err := s.topology.GetSetting("offsite_location"); err == nil && ok {
		s.cfg.Library.OffsiteLocation = v == "true"
	}
	if v, ok, err := s.topology.GetSetting("offsite_rotation_interval"); err == nil && ok {
		s.cfg.Library.OffsiteRotationInterval = v
	}
	if v, ok, err := s.topology.GetSetting("offsite_rotation_count"); err == nil && ok {
		if n, convErr := strconv.Atoi(v); convErr == nil {
			s.cfg.Library.OffsiteRotationCount = n
		}
	}
	if v, ok, err := s.topology.GetSetting("magazine_pin_hash"); err == nil && ok {
		s.cfg.Library.MagazinePINHash = v
	}
}

// restartRequiredFields lists the settings that can only take effect on the
// next daemon restart, even though (as of this rewrite) every one of them
// now lives in the database rather than config.yaml: data_dir and listen
// are process-level resources (listeners, the database's own location) this
// daemon does not attempt to hot-swap; tokens_file/users_file point at the
// credential JSON stores, which are likewise only ever read once at
// startup. Library topology (magazines, drive devices, I/O slot count,
// logical libraries) and everything else in UpdateSettingsRequest below
// hot-applies via Library.Reconfigure/UpdateLiveSettings, so it isn't
// listed here.
var restartRequiredFields = []string{
	"data_dir", "listen", "tokens_file", "users_file",
}

// UpdateSettingsRequest carries the subset of configuration that can be
// changed, all fields optional (nil = leave unchanged). Every field here
// persists to the database (internal/store/topology.go); only
// TokensFile/UsersFile require a restart to actually take effect (see
// restartRequiredFields).
type UpdateSettingsRequest struct {
	VTLName                 *string              `json:"vtl_name,omitempty"`
	DefaultCapacity         *string              `json:"default_capacity,omitempty"`
	PollInterval            *string              `json:"poll_interval,omitempty"`
	LogLevel                *string              `json:"log_level,omitempty"`
	SNMPEnabled             *bool                `json:"snmp_enabled,omitempty"`
	SNMPEnterpriseOID       *string              `json:"snmp_enterprise_oid,omitempty"`
	SNMPAgentAddress        *string              `json:"snmp_agent_address,omitempty"`
	SNMPTargets             *[]config.SNMPTarget `json:"snmp_targets,omitempty"`
	OffsiteLocation         *bool                `json:"offsite_location,omitempty"`
	OffsiteRotationInterval *string              `json:"offsite_rotation_interval,omitempty"`
	OffsiteRotationCount    *int                 `json:"offsite_rotation_count,omitempty"`
	TokensFile              *string              `json:"tokens_file,omitempty"`
	UsersFile               *string              `json:"users_file,omitempty"`
}

// UpdateSettingsResult is returned after applying a settings update.
type UpdateSettingsResult struct {
	Config          config.Config `json:"config"`
	RestartRequired []string      `json:"restart_required_fields"`
}

// Update applies req, persists the resulting configuration to disk, and
// live-applies whichever parts can take effect immediately.
func (s *Settings) Update(req UpdateSettingsRequest) (UpdateSettingsResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Start from what's actually in the store, not from a cached copy that
	// the wizard/PIN handler may have invalidated - every field below is
	// persisted unconditionally at the end, so a stale starting point would
	// be written back over live values (see refreshStoreBackedLocked).
	s.refreshStoreBackedLocked()
	next := s.cfg

	if req.VTLName != nil {
		if strings.TrimSpace(*req.VTLName) == "" {
			return UpdateSettingsResult{}, fmt.Errorf("vtl_name must not be empty")
		}
		next.Library.Name = *req.VTLName
	}
	if req.DefaultCapacity != nil {
		if _, err := config.ParseSize(*req.DefaultCapacity); err != nil {
			return UpdateSettingsResult{}, fmt.Errorf("default_capacity: %w", err)
		}
		next.Library.DefaultCapacity = *req.DefaultCapacity
	}
	if req.PollInterval != nil {
		d, err := time.ParseDuration(*req.PollInterval)
		if err != nil {
			return UpdateSettingsResult{}, fmt.Errorf("poll_interval: %w", err)
		}
		next.PollIntervalRaw = *req.PollInterval
		next.PollInterval = d
	}
	if req.LogLevel != nil {
		lvl, err := parseLogLevel(*req.LogLevel)
		if err != nil {
			return UpdateSettingsResult{}, err
		}
		next.LogLevel = *req.LogLevel
		if s.logLevel != nil {
			s.logLevel.Set(lvl)
		}
	}
	if req.SNMPEnabled != nil {
		next.SNMP.Enabled = *req.SNMPEnabled
	}
	if req.SNMPEnterpriseOID != nil {
		next.SNMP.EnterpriseOID = *req.SNMPEnterpriseOID
	}
	if req.SNMPAgentAddress != nil {
		next.SNMP.AgentAddress = *req.SNMPAgentAddress
	}
	if req.SNMPTargets != nil {
		next.SNMP.Targets = *req.SNMPTargets
	}
	if req.OffsiteLocation != nil {
		next.Library.OffsiteLocation = *req.OffsiteLocation
	}
	if req.OffsiteRotationInterval != nil {
		if *req.OffsiteRotationInterval != "" {
			if _, err := config.ParseDuration(*req.OffsiteRotationInterval); err != nil {
				return UpdateSettingsResult{}, fmt.Errorf("offsite_rotation_interval: %w", err)
			}
		}
		next.Library.OffsiteRotationInterval = *req.OffsiteRotationInterval
	}
	if req.OffsiteRotationCount != nil {
		next.Library.OffsiteRotationCount = *req.OffsiteRotationCount
	}
	if req.TokensFile != nil {
		if *req.TokensFile == "" || (*req.TokensFile)[0] != '/' {
			return UpdateSettingsResult{}, fmt.Errorf("tokens_file must be an absolute path")
		}
		next.TokensFile = *req.TokensFile
	}
	if req.UsersFile != nil {
		if *req.UsersFile == "" || (*req.UsersFile)[0] != '/' {
			return UpdateSettingsResult{}, fmt.Errorf("users_file must be an absolute path")
		}
		next.UsersFile = *req.UsersFile
	}

	if err := next.Validate(); err != nil {
		return UpdateSettingsResult{}, err
	}

	if s.topology != nil {
		if err := s.topology.SetSetting("vtl_name", next.Library.Name); err != nil {
			return UpdateSettingsResult{}, err
		}
		if err := s.topology.SetSetting("default_capacity", next.Library.DefaultCapacity); err != nil {
			return UpdateSettingsResult{}, err
		}
		offsiteLocation := "false"
		if next.Library.OffsiteLocation {
			offsiteLocation = "true"
		}
		if err := s.topology.SetSetting("offsite_location", offsiteLocation); err != nil {
			return UpdateSettingsResult{}, err
		}
		if err := s.topology.SetSetting("offsite_rotation_interval", next.Library.OffsiteRotationInterval); err != nil {
			return UpdateSettingsResult{}, err
		}
		if err := s.topology.SetSetting("offsite_rotation_count", strconv.Itoa(next.Library.OffsiteRotationCount)); err != nil {
			return UpdateSettingsResult{}, err
		}
		if err := s.topology.SetSetting("poll_interval", next.PollIntervalRaw); err != nil {
			return UpdateSettingsResult{}, err
		}
		if err := s.topology.SetSetting("log_level", next.LogLevel); err != nil {
			return UpdateSettingsResult{}, err
		}
		snmpEnabled := "false"
		if next.SNMP.Enabled {
			snmpEnabled = "true"
		}
		if err := s.topology.SetSetting("snmp_enabled", snmpEnabled); err != nil {
			return UpdateSettingsResult{}, err
		}
		if err := s.topology.SetSetting("snmp_enterprise_oid", next.SNMP.EnterpriseOID); err != nil {
			return UpdateSettingsResult{}, err
		}
		if err := s.topology.SetSetting("snmp_agent_address", next.SNMP.AgentAddress); err != nil {
			return UpdateSettingsResult{}, err
		}
		if err := s.topology.SetSNMPTargets(next.SNMP.Targets); err != nil {
			return UpdateSettingsResult{}, err
		}
		if err := s.topology.SetSetting("tokens_file", next.TokensFile); err != nil {
			return UpdateSettingsResult{}, err
		}
		if err := s.topology.SetSetting("users_file", next.UsersFile); err != nil {
			return UpdateSettingsResult{}, err
		}
	}

	s.cfg = next
	if s.snmp != nil {
		s.snmp.SetConfig(next.SNMP)
	}
	if s.lib != nil {
		s.lib.UpdateLiveSettings(next.Library.DefaultCapacity, next.Library.OffsiteLocation)
	}

	return UpdateSettingsResult{Config: next, RestartRequired: restartRequiredFields}, nil
}

func parseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("invalid log_level %q: must be one of debug, info, warn, error", s)
	}
}
