package api

import (
	"github.com/swenske/gotochanger/internal/config"
)

// LatencySettingsResult is returned by GET/PUT .../settings/latency: the
// effective settings plus a static Defaults payload the Admin > Latency
// UI's "Load defaults" button uses to prefill the form (the admin still
// reviews and clicks Save to actually persist - confirmed with the user,
// no separate reset endpoint is needed for this).
type LatencySettingsResult struct {
	Settings config.LatencySettings `json:"settings"`
	Defaults config.LatencySettings `json:"defaults"`
}

// CurrentLatency returns the effective latency settings, read fresh from
// the topology store rather than from s.cfg.Library.Latency - unlike
// every other Settings field, latency can be changed out from under this
// cached copy by the setup wizard (step 8 writes "latency_enabled"
// straight to the store via TopologyStore.SetSetting, the same way
// vtl_name/offsite_location etc. already do), and Settings.cfg is only
// ever refreshed by Settings.Update, never by reconfigureFromStore()'s
// wizard-completion path. Reading fresh here avoids shipping a page that
// shows "disabled" right after the wizard enabled it.
func (s *Settings) CurrentLatency() LatencySettingsResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	ls := s.cfg.Library.Latency
	if s.topology != nil {
		if fresh, err := s.topology.GetLatencySettings(); err == nil {
			ls = fresh
			s.cfg.Library.Latency = fresh
		}
	}
	return LatencySettingsResult{Settings: ls, Defaults: config.DefaultLatencySettings()}
}

// UpdateLatency validates and persists a full replacement LatencySettings
// (unlike Settings.Update's UpdateSettingsRequest, every field here is
// required/authoritative, not optional-pointer-typed - the Admin >
// Latency form always submits the whole set of 7 delays plus Enabled at
// once), then live-applies it via Library.UpdateLatencySettings, mirroring
// Settings.Update's own persist-then-live-apply shape.
func (s *Settings) UpdateLatency(ls config.LatencySettings) (LatencySettingsResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := config.ValidateLatencySettings(ls); err != nil {
		return LatencySettingsResult{}, err
	}

	if s.topology != nil {
		if err := s.topology.SetLatencySettings(ls); err != nil {
			return LatencySettingsResult{}, err
		}
	}

	s.cfg.Library.Latency = ls
	if s.lib != nil {
		s.lib.UpdateLatencySettings(ls)
	}

	return LatencySettingsResult{Settings: ls, Defaults: config.DefaultLatencySettings()}, nil
}
