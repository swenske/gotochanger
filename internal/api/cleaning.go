package api

import (
	"github.com/swenske/gotochanger/internal/config"
)

// CleaningSettingsResult is returned by GET/PUT .../settings/cleaning: the
// effective settings plus a static Defaults payload the Admin > Cleaning
// Tapes UI's "Load defaults" button uses to prefill the form - mirrors
// LatencySettingsResult exactly.
type CleaningSettingsResult struct {
	Settings config.CleaningSettings `json:"settings"`
	Defaults config.CleaningSettings `json:"defaults"`
}

// CurrentCleaning returns the effective cleaning settings, read fresh
// from the topology store rather than from s.cfg.Library.Cleaning -
// mirrors CurrentLatency's own "read fresh, don't trust the cached copy"
// choice, made from the start here rather than rediscovered the way
// CurrentLatency's staleness bug was (see CLAUDE.md's "Latency
// simulation" writeup).
func (s *Settings) CurrentCleaning() CleaningSettingsResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	cs := s.cfg.Library.Cleaning
	if s.topology != nil {
		if fresh, err := s.topology.GetCleaningSettings(); err == nil {
			cs = fresh
			s.cfg.Library.Cleaning = fresh
		}
	}
	return CleaningSettingsResult{Settings: cs, Defaults: config.DefaultCleaningSettings()}
}

// UpdateCleaning validates and persists a full replacement
// CleaningSettings, then live-applies it via Library.UpdateCleaningSettings,
// mirroring UpdateLatency's persist-then-live-apply shape.
func (s *Settings) UpdateCleaning(cs config.CleaningSettings) (CleaningSettingsResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := config.ValidateCleaningSettings(cs); err != nil {
		return CleaningSettingsResult{}, err
	}

	if s.topology != nil {
		if err := s.topology.SetCleaningSettings(cs); err != nil {
			return CleaningSettingsResult{}, err
		}
	}

	s.cfg.Library.Cleaning = cs
	if s.lib != nil {
		s.lib.UpdateCleaningSettings(cs)
	}

	return CleaningSettingsResult{Settings: cs, Defaults: config.DefaultCleaningSettings()}, nil
}
