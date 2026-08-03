package api

import (
	"context"
	"log/slog"
	"net/http"
	"runtime"
	"time"

	"github.com/swenske/gotochanger/internal/library"
	"github.com/swenske/gotochanger/internal/telemetry"
)

// telemetryEndpoint is where sendTelemetryAsync reports, a var (not a
// const) so tests can point it at an httptest.Server instead of the real
// collector - same idiom as internal/api/kernel_mode.go's path vars and
// internal/instanceid's hardwareIDPaths.
var telemetryEndpoint = telemetry.DefaultEndpoint

// buildTelemetryPayload is the single source of truth for what an
// anonymous usage-statistics report contains - called by both the actual
// sender (sendTelemetryAsync) and every preview surface (the wizard's
// last step, Admin > Settings > Telemetry) so a preview can never drift
// from what's actually transmitted. Every lookup is best-effort (a
// missing/erroring source just leaves that field at its zero value)
// since a preview or a best-effort send must never fail outright over a
// single unavailable data point.
func (s *Server) buildTelemetryPayload() telemetry.Payload {
	p := telemetry.Payload{
		Version:           s.version,
		OS:                runtime.GOOS,
		Arch:              runtime.GOARCH,
		InContainer:       currentKernelModeStatus().InContainer,
		PrometheusEnabled: s.currentPrometheusSettings().Enabled,
	}

	if s.topology != nil {
		if id, err := s.topology.InstanceID(); err == nil {
			p.InstanceID = id
		}
		if mode, _, err := s.topology.GetSetting("operational_mode"); err == nil {
			p.OperationalMode = mode
			p.KernelModeActive = mode == "kernel"
		}
		if v, ok, err := s.topology.GetSetting("snmp_enabled"); err == nil && ok {
			p.SNMPEnabled = v == "true"
		}
		if v, ok, err := s.topology.GetSetting("offsite_location"); err == nil && ok {
			p.OffsiteRotationEnabled = v == "true"
		}
		if cs, err := s.topology.GetCleaningSettings(); err == nil {
			p.CleaningSimEnabled = cs.Enabled
		}
		if ls, err := s.topology.GetLatencySettings(); err == nil {
			p.LatencySimEnabled = ls.Enabled
		}
		if mags, err := s.topology.ListMagazines(); err == nil {
			p.MagazinesTotal = len(mags)
		}
		if mbs, err := s.topology.ListMailboxes(); err == nil {
			p.MailboxesTotal = len(mbs)
		}
		if sets, err := s.topology.ListTapeSets(); err == nil {
			p.TapeSetsTotal = len(sets)
		}
	}

	if s.lib != nil {
		st := s.lib.Status()
		p.SlotsTotal = len(st.Slots)
		p.IOSlotsTotal = len(st.IOSlots)
		p.DrivesTotal = len(st.Drives)
		p.LogicalLibrariesTotal = len(st.LogicalLibs)

		// Same volumesTotal computation as refreshGaugeMetrics
		// (prometheus.go) - a cartridge is counted wherever it currently
		// sits, never by name/barcode.
		volumesTotal := 0
		for _, sl := range st.Slots {
			if sl.Volume != nil {
				volumesTotal++
			}
		}
		for _, io := range st.IOSlots {
			if io.Volume != nil {
				volumesTotal++
			}
		}
		for _, d := range st.Drives {
			if d.Volume != nil {
				volumesTotal++
			}
		}
		volumesTotal += len(st.OutsideVolumes) + len(st.OffsiteVolumes)
		p.VolumesTotal = volumesTotal
	}

	if s.users != nil {
		p.UsersTotal = len(s.users.List())
	}

	return p
}

type telemetrySettingsResponse struct {
	Enabled  bool              `json:"enabled"`
	Endpoint string            `json:"endpoint"`
	Payload  telemetry.Payload `json:"payload"`
}

func (s *Server) telemetryEnabled() bool {
	if s.topology == nil {
		return false
	}
	v, ok, err := s.topology.GetSetting("telemetry_enabled")
	return err == nil && ok && v == "true"
}

func (s *Server) currentTelemetrySettings() telemetrySettingsResponse {
	return telemetrySettingsResponse{
		Enabled:  s.telemetryEnabled(),
		Endpoint: telemetryEndpoint,
		Payload:  s.buildTelemetryPayload(),
	}
}

func (s *Server) handleGetTelemetrySettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.currentTelemetrySettings())
}

type updateTelemetryRequest struct {
	Enabled bool `json:"enabled"`
}

func (s *Server) handleUpdateTelemetrySettings(w http.ResponseWriter, r *http.Request) {
	var req updateTelemetryRequest
	if err := decodeJSON(r, &req); err != nil {
		s.emitFailure(r, library.EventCodeConfigSettingsUpdateFailure, "failed to update telemetry settings", err, nil)
		writeError(w, http.StatusBadRequest, err)
		return
	}

	wasEnabled := s.telemetryEnabled()
	value := "false"
	if req.Enabled {
		value = "true"
	}
	if err := s.topology.SetSetting("telemetry_enabled", value); err != nil {
		s.emitFailure(r, library.EventCodeConfigSettingsUpdateFailure, "failed to update telemetry settings", err, nil)
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// The opt-in takes effect right away rather than silently waiting for
	// the next daemon restart - see sendTelemetryAsync's doc comment.
	if req.Enabled && !wasEnabled {
		s.sendTelemetryAsync()
	}

	s.emitEvent(r, library.Event{Code: library.EventCodeConfigSettingsUpdateSuccess, Message: "updated telemetry settings"})
	writeJSON(w, http.StatusOK, s.currentTelemetrySettings())
}

// sendTelemetryAsync fires a single best-effort, non-blocking anonymous
// usage-statistics report to telemetryEndpoint (see internal/telemetry's
// Payload doc comment for exactly what is and isn't included). Called
// once at daemon startup when telemetry is already enabled (see
// cmd/gotochangerd/main.go, alongside ReconcileKernelModeInstancesAsyncOnStartup)
// and once more immediately whenever telemetry is newly enabled mid-run
// (wizard completion, handleUpdateTelemetrySettings above) - never on a
// recurring ticker. A send failure (most likely: no collector reachable
// yet) must never be visible on any request path or block the caller,
// so it's logged at Debug only, never Warn/Error - it isn't actionable
// by the operator and is entirely expected before a collector exists.
func (s *Server) sendTelemetryAsync() {
	payload := s.buildTelemetryPayload()
	log := s.log
	if log == nil {
		log = slog.Default()
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := telemetry.Send(ctx, &http.Client{}, telemetryEndpoint, payload); err != nil {
			log.Debug("telemetry send failed", "error", err)
			return
		}
		log.Debug("telemetry sent")
	}()
}

// SendTelemetryAsync is sendTelemetryAsync exported for
// cmd/gotochangerd's startup path, which lives outside this package.
func (s *Server) SendTelemetryAsync() {
	s.sendTelemetryAsync()
}
