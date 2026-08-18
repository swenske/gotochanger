package offsite

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/swenske/gotochanger/internal/config"
)

// ContinuousRotationConfig mirrors the pre-mode-architecture
// offsite_rotation_interval/offsite_rotation_count settings exactly (see
// internal/store's migrateLegacyOffsiteRotationSetting) - empty
// IntervalRaw means "configured but paused", a valid, saveable state that
// matches that legacy setting's own on/off convention.
type ContinuousRotationConfig struct {
	IntervalRaw string `json:"interval"`
	Count       int    `json:"count"`
}

// ContinuousRotationMode wraps library.RotateOffsite's existing selection
// logic (via Library.OffsiteRotationCandidates) in the OffsiteMode
// interface - a faithful port of this project's original, only offsite
// behavior, not a new feature.
type ContinuousRotationMode struct{}

func NewContinuousRotationMode() *ContinuousRotationMode { return &ContinuousRotationMode{} }

func (m *ContinuousRotationMode) Name() string  { return "continuous_rotation" }
func (m *ContinuousRotationMode) Label() string { return "Continuous Rotation" }
func (m *ContinuousRotationMode) Describe() string {
	return "Automatically vaults full tapes offsite on a fixed interval, up to a quota per sweep - no vendor schedule and no manual pickup/return requests."
}

func (m *ContinuousRotationMode) ParameterSchema() []ParameterField {
	return []ParameterField{
		{Key: "interval", Label: "Rotation interval", Type: ParameterTypeDuration, Required: false,
			Help: `How often to sweep for full tapes to send offsite, e.g. "24h". Leave empty to pause.`},
		{Key: "count", Label: "Tapes per sweep", Type: ParameterTypeInt, Required: true,
			Help: "Maximum number of tapes to send offsite per sweep."},
	}
}

func (m *ContinuousRotationMode) DefaultConfig() json.RawMessage {
	raw, _ := json.Marshal(ContinuousRotationConfig{IntervalRaw: "24h", Count: 1})
	return raw
}

func (m *ContinuousRotationMode) ValidateConfig(raw json.RawMessage) error {
	var cfg ContinuousRotationConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("invalid continuous_rotation config: %w", err)
	}
	if cfg.IntervalRaw != "" {
		if _, err := config.ParseDuration(cfg.IntervalRaw); err != nil {
			return fmt.Errorf("interval: %w", err)
		}
	}
	if cfg.Count < 0 {
		return fmt.Errorf("count must be >= 0")
	}
	return nil
}

// RequestPickup/RequestReturn: this mode has never had a manual per-tape
// request concept (RotateOffsite, which it wraps, doesn't either) - use
// the generic manual /offsite/send and /offsite/recall endpoints for
// one-off overrides regardless of which mode is active.
func (m *ContinuousRotationMode) RequestPickup(ctx context.Context, eng *Engine, barcodes []string, actor string) []ItemResult {
	return unsupportedResults(barcodes, "continuous rotation has no manual pickup request - use POST /api/v1/offsite/send")
}

func (m *ContinuousRotationMode) RequestReturn(ctx context.Context, eng *Engine, barcodes []string, urgent bool, actor string) []ItemResult {
	return unsupportedResults(barcodes, "continuous rotation has no manual return request - use POST /api/v1/offsite/recall")
}

func (m *ContinuousRotationMode) Tick(ctx context.Context, eng *Engine, now time.Time) error {
	if !eng.lib.OffsiteEnabled() {
		return nil
	}
	raw, ok, err := eng.store.GetOffsiteModeConfig(m.Name())
	if err != nil || !ok {
		return err
	}
	var cfg ContinuousRotationConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	interval, err := config.ParseDuration(cfg.IntervalRaw)
	if err != nil || interval <= 0 || cfg.Count <= 0 {
		return nil
	}
	lastRaw, _, _ := eng.store.GetSetting("offsite_mode_continuous_rotation_last_run")
	last, _ := time.Parse(time.RFC3339, lastRaw)
	if now.Sub(last) < interval {
		return nil
	}
	for _, ref := range eng.lib.OffsiteRotationCandidates(cfg.Count) {
		eng.SendNow(ref, m.Name()) // best-effort, errors already logged by SendNow
	}
	return eng.store.SetSetting("offsite_mode_continuous_rotation_last_run", now.UTC().Format(time.RFC3339))
}
