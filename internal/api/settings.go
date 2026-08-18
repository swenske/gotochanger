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
	"github.com/swenske/gotochanger/internal/mailer"
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
	mailer   *mailer.SMTPMailer
	logLevel *slog.LevelVar
	topology TopologyStore
}

// NewSettings builds a Settings manager. topology may be nil in tests that
// don't exercise setting persistence.
func NewSettings(cfg config.Config, lib *library.Library, sender *snmp.Sender, smtpMailer *mailer.SMTPMailer, logLevel *slog.LevelVar, topology TopologyStore) *Settings {
	return &Settings{cfg: cfg, lib: lib, snmp: sender, mailer: smtpMailer, logLevel: logLevel, topology: topology}
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
	if v, ok, err := s.topology.GetSetting("magazine_pin_hash"); err == nil && ok {
		s.cfg.Library.MagazinePINHash = v
	}
}

// restartRequiredFields lists the settings that can only take effect on the
// next daemon restart: data_dir and listen are process-level resources
// (listeners, the database's own location) this daemon does not attempt to
// hot-swap. Library topology (magazines, drive devices, I/O slot count,
// logical libraries) and everything in UpdateSettingsRequest below
// hot-applies via Library.Reconfigure/UpdateLiveSettings, so it isn't
// listed here.
var restartRequiredFields = []string{
	"data_dir", "listen",
}

// UpdateSettingsRequest carries the subset of configuration that can be
// changed, all fields optional (nil = leave unchanged). Every field here
// persists to the database (internal/store/topology.go) and hot-applies
// immediately.
type UpdateSettingsRequest struct {
	VTLName           *string              `json:"vtl_name,omitempty"`
	DefaultCapacity   *string              `json:"default_capacity,omitempty"`
	PollInterval      *string              `json:"poll_interval,omitempty"`
	LogLevel          *string              `json:"log_level,omitempty"`
	SNMPEnabled       *bool                `json:"snmp_enabled,omitempty"`
	SNMPEnterpriseOID *string              `json:"snmp_enterprise_oid,omitempty"`
	SNMPAgentAddress  *string              `json:"snmp_agent_address,omitempty"`
	SNMPTargets       *[]config.SNMPTarget `json:"snmp_targets,omitempty"`
	OffsiteLocation   *bool                `json:"offsite_location,omitempty"`
	SMTPEnabled       *bool                `json:"smtp_enabled,omitempty"`
	SMTPHost          *string              `json:"smtp_host,omitempty"`
	SMTPPort          *int                 `json:"smtp_port,omitempty"`
	SMTPUsername      *string              `json:"smtp_username,omitempty"`
	SMTPPassword      *string              `json:"smtp_password,omitempty"`
	SMTPFrom          *string              `json:"smtp_from,omitempty"`
	SMTPUseTLS        *bool                `json:"smtp_use_tls,omitempty"`
	SMTPStartTLS      *bool                `json:"smtp_start_tls,omitempty"`
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
	if req.SMTPEnabled != nil {
		next.SMTP.Enabled = *req.SMTPEnabled
	}
	if req.SMTPHost != nil {
		next.SMTP.Host = *req.SMTPHost
	}
	if req.SMTPPort != nil {
		next.SMTP.Port = *req.SMTPPort
	}
	if req.SMTPUsername != nil {
		next.SMTP.Username = *req.SMTPUsername
	}
	// Blank/omitted always means "leave unchanged," never "clear": the
	// Settings page submits every field together on every save, and this
	// input always starts blank (GET never returns the real password), so
	// "blank = clear" would silently wipe a working password the moment an
	// admin saved any unrelated setting - mirrors the mailbox-edit
	// dialog's PIN field (app.js), which has the same pointer-semantics
	// comment for the same reason.
	if req.SMTPPassword != nil && *req.SMTPPassword != "" {
		next.SMTP.Password = *req.SMTPPassword
	}
	if req.SMTPFrom != nil {
		next.SMTP.From = *req.SMTPFrom
	}
	if req.SMTPUseTLS != nil {
		next.SMTP.UseTLS = *req.SMTPUseTLS
	}
	if req.SMTPStartTLS != nil {
		next.SMTP.StartTLS = *req.SMTPStartTLS
	}
	next.SMTP.PasswordConfigured = next.SMTP.Password != ""

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
		smtpEnabled := "false"
		if next.SMTP.Enabled {
			smtpEnabled = "true"
		}
		if err := s.topology.SetSetting("smtp_enabled", smtpEnabled); err != nil {
			return UpdateSettingsResult{}, err
		}
		if err := s.topology.SetSetting("smtp_host", next.SMTP.Host); err != nil {
			return UpdateSettingsResult{}, err
		}
		if err := s.topology.SetSetting("smtp_port", strconv.Itoa(next.SMTP.Port)); err != nil {
			return UpdateSettingsResult{}, err
		}
		if err := s.topology.SetSetting("smtp_username", next.SMTP.Username); err != nil {
			return UpdateSettingsResult{}, err
		}
		// Persisted unconditionally (not just when non-blank): by this
		// point next.SMTP.Password is already correct from the
		// leave-unchanged-unless-non-blank logic applied above, so there's
		// nothing left to special-case here - same as every other field.
		if err := s.topology.SetSetting("smtp_password", next.SMTP.Password); err != nil {
			return UpdateSettingsResult{}, err
		}
		if err := s.topology.SetSetting("smtp_from", next.SMTP.From); err != nil {
			return UpdateSettingsResult{}, err
		}
		smtpUseTLS := "false"
		if next.SMTP.UseTLS {
			smtpUseTLS = "true"
		}
		if err := s.topology.SetSetting("smtp_use_tls", smtpUseTLS); err != nil {
			return UpdateSettingsResult{}, err
		}
		smtpStartTLS := "false"
		if next.SMTP.StartTLS {
			smtpStartTLS = "true"
		}
		if err := s.topology.SetSetting("smtp_start_tls", smtpStartTLS); err != nil {
			return UpdateSettingsResult{}, err
		}
	}

	s.cfg = next
	if s.snmp != nil {
		s.snmp.SetConfig(next.SNMP)
	}
	if s.mailer != nil {
		s.mailer.SetConfig(next.SMTP)
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
