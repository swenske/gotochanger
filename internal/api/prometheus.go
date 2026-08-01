package api

import (
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/swenske/gotochanger/internal/library"
	"github.com/swenske/gotochanger/internal/metrics"
)

// metricsPath is the fixed, unauthenticated scrape endpoint - surfaced back
// to the Admin UI/CLI purely for reference, not configurable.
const metricsPath = "/metrics"

// prometheusMetrics holds every gotochanger_* metric handle, created once
// per Server (never a package-level global - this codebase has hit
// shared-state read/write races across Server instances before, see
// CLAUDE.md's "Correctness invariants"). Gauges are overwritten wholesale
// on every scrape from a fresh Status() snapshot (see refreshGaugeMetrics);
// counters and the histogram accumulate for the life of the process, fed
// by metricsMiddleware.
type prometheusMetrics struct {
	registry *metrics.Registry

	slotsTotal    *metrics.Gauge
	slotsFree     *metrics.Gauge
	slotsOccupied *metrics.Gauge

	readersTotal  *metrics.Gauge
	readersIdle   *metrics.Gauge
	readersActive *metrics.Gauge
	readersFree   *metrics.Gauge
	readersError  *metrics.Gauge

	volumesTotal    *metrics.Gauge
	volumesByStatus *metrics.Gauge

	magazinesTotal *metrics.Gauge

	capacityUtilizationPercent *metrics.Gauge
	queueDepth                 *metrics.Gauge
	uptimeSeconds              *metrics.Gauge
	lastBackupTimestamp        *metrics.Gauge

	operationsTotal          *metrics.Counter
	operationDurationSeconds *metrics.Histogram
	errorsTotal              *metrics.Counter
}

func newPrometheusMetrics() *prometheusMetrics {
	reg := metrics.NewRegistry()
	return &prometheusMetrics{
		registry: reg,

		slotsTotal:    reg.NewGauge("gotochanger_slots_total", "Total storage slots"),
		slotsFree:     reg.NewGauge("gotochanger_slots_free", "Free storage slots"),
		slotsOccupied: reg.NewGauge("gotochanger_slots_occupied", "Occupied storage slots"),

		readersTotal:  reg.NewGauge("gotochanger_readers_total", "Total tape drives"),
		readersIdle:   reg.NewGauge("gotochanger_readers_idle", "Drives with a volume loaded but not currently reading or writing"),
		readersActive: reg.NewGauge("gotochanger_readers_active", "Drives currently reading or writing"),
		readersFree:   reg.NewGauge("gotochanger_readers_free", "Drives with no volume loaded"),
		readersError:  reg.NewGauge("gotochanger_readers_error", "Drives in a simulated fault state"),

		volumesTotal:    reg.NewGauge("gotochanger_volumes_total", "Total tape volumes known to the library"),
		volumesByStatus: reg.NewGauge("gotochanger_volumes_by_status", "Tape volumes by location (status label: in_slot, in_ioslot, in_drive, outside, offsite)"),

		magazinesTotal: reg.NewGauge("gotochanger_magazines_total", "Total storage magazines"),

		capacityUtilizationPercent: reg.NewGauge("gotochanger_capacity_utilization_percent", "Occupied storage slots as a percentage of total storage slots"),
		queueDepth:                 reg.NewGauge("gotochanger_queue_depth", "1 if the single robotic arm is currently busy, 0 if idle (this simulator has one arm and no operation queue)"),
		uptimeSeconds:              reg.NewGauge("gotochanger_uptime_seconds", "Seconds since the daemon started"),
		lastBackupTimestamp:        reg.NewGauge("gotochanger_last_backup_timestamp", "Unix timestamp of the last configuration backup (state.db snapshot); absent if none has ever been taken"),

		operationsTotal:          reg.NewCounter("gotochanger_operations_total", "Total library operations executed, labeled by operation_type"),
		operationDurationSeconds: reg.NewHistogram("gotochanger_operation_duration_seconds", "Library operation latency in seconds, labeled by operation_type", []float64{.01, .05, .1, .5, 1, 2, 5, 10, 30}),
		errorsTotal:              reg.NewCounter("gotochanger_errors_total", "Total request errors, labeled by error_type"),
	}
}

// refreshGaugeMetrics recomputes every gauge from a single fresh Status()
// snapshot, exactly like handleStatus already does - no new locking, and
// every value is always current as of the moment of the scrape rather than
// polled/cached, trivially satisfying "metrics update in real time."
func (s *Server) refreshGaugeMetrics() {
	pm := s.pm
	if pm == nil {
		return
	}
	st := s.lib.Status()

	slotsTotal, slotsOccupied := 0, 0
	for _, sl := range st.Slots {
		slotsTotal++
		if sl.Volume != nil {
			slotsOccupied++
		}
	}
	pm.slotsTotal.Set(float64(slotsTotal), nil)
	pm.slotsOccupied.Set(float64(slotsOccupied), nil)
	pm.slotsFree.Set(float64(slotsTotal-slotsOccupied), nil)
	utilization := 0.0
	if slotsTotal > 0 {
		utilization = float64(slotsOccupied) / float64(slotsTotal) * 100
	}
	pm.capacityUtilizationPercent.Set(utilization, nil)

	var idle, active, free, errored int
	for _, d := range st.Drives {
		switch {
		case d.Fault:
			errored++
		case d.Volume != nil && d.Activity != "":
			active++
		case d.Volume != nil:
			idle++
		default:
			free++
		}
	}
	pm.readersTotal.Set(float64(len(st.Drives)), nil)
	pm.readersIdle.Set(float64(idle), nil)
	pm.readersActive.Set(float64(active), nil)
	pm.readersFree.Set(float64(free), nil)
	pm.readersError.Set(float64(errored), nil)

	volumesTotal := 0
	byStatus := map[string]int{}
	for _, sl := range st.Slots {
		if sl.Volume != nil {
			volumesTotal++
			byStatus["in_slot"]++
		}
	}
	for _, io := range st.IOSlots {
		if io.Volume != nil {
			volumesTotal++
			byStatus["in_ioslot"]++
		}
	}
	for _, d := range st.Drives {
		if d.Volume != nil {
			volumesTotal++
			byStatus["in_drive"]++
		}
	}
	volumesTotal += len(st.OutsideVolumes)
	byStatus["outside"] += len(st.OutsideVolumes)
	volumesTotal += len(st.OffsiteVolumes)
	byStatus["offsite"] += len(st.OffsiteVolumes)

	pm.volumesTotal.Set(float64(volumesTotal), nil)
	pm.volumesByStatus.Reset()
	for status, n := range byStatus {
		pm.volumesByStatus.Set(float64(n), map[string]string{"status": status})
	}

	magazinesTotal := 0
	if s.topology != nil {
		if mags, err := s.topology.ListMagazines(); err == nil {
			magazinesTotal = len(mags)
		}
	}
	pm.magazinesTotal.Set(float64(magazinesTotal), nil)

	busy := 0.0
	if st.ArmState.Busy {
		busy = 1
	}
	pm.queueDepth.Set(busy, nil)

	pm.uptimeSeconds.Set(time.Since(s.startedAt).Seconds(), nil)

	if s.topology != nil {
		if v, ok, err := s.topology.GetSetting("last_backup_at"); err == nil && ok {
			if ts, convErr := strconv.ParseInt(v, 10, 64); convErr == nil {
				pm.lastBackupTimestamp.Set(float64(ts), nil)
			}
		} else {
			pm.lastBackupTimestamp.Delete(nil)
		}
	}
}

// handleMetrics serves the Prometheus text exposition format, deliberately
// with no requireRole wrapper (registered like /healthz) - the ticket this
// implements requires unauthenticated scraping, matching standard
// Prometheus deployment practice.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if !s.currentPrometheusSettings().Enabled {
		http.NotFound(w, r)
		return
	}
	s.refreshGaugeMetrics()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if s.pm != nil {
		_ = s.pm.registry.WriteExposition(w)
	}
}

type prometheusSettingsResponse struct {
	Enabled     bool   `json:"enabled"`
	MetricsPath string `json:"metrics_path"`
}

func (s *Server) currentPrometheusSettings() prometheusSettingsResponse {
	enabled := false
	if s.topology != nil {
		if v, ok, err := s.topology.GetSetting("prometheus_enabled"); err == nil && ok {
			enabled = v == "true"
		}
	}
	return prometheusSettingsResponse{Enabled: enabled, MetricsPath: metricsPath}
}

func (s *Server) handleGetPrometheusSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.currentPrometheusSettings())
}

type updatePrometheusRequest struct {
	Enabled bool `json:"enabled"`
}

func (s *Server) handleUpdatePrometheusSettings(w http.ResponseWriter, r *http.Request) {
	var req updatePrometheusRequest
	if err := decodeJSON(r, &req); err != nil {
		s.emitFailure(r, library.EventCodeConfigSettingsUpdateFailure, "failed to update prometheus settings", err, nil)
		writeError(w, http.StatusBadRequest, err)
		return
	}
	value := "false"
	if req.Enabled {
		value = "true"
	}
	if err := s.topology.SetSetting("prometheus_enabled", value); err != nil {
		s.emitFailure(r, library.EventCodeConfigSettingsUpdateFailure, "failed to update prometheus settings", err, nil)
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.emitEvent(r, library.Event{Code: library.EventCodeConfigSettingsUpdateSuccess, Message: "updated prometheus settings"})
	writeJSON(w, http.StatusOK, s.currentPrometheusSettings())
}

// handleDownloadGrafanaDashboard serves the static, embedded Grafana
// dashboard JSON exactly like handleSNMPMIB serves its MIB (snmp_mib.go) -
// same embed, same download headers - except this dashboard needs no
// per-installation templating.
func (s *Server) handleDownloadGrafanaDashboard(w http.ResponseWriter, r *http.Request) {
	raw, err := staticAssets.Open("gotochanger-dashboard.json")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer raw.Close()
	data, err := io.ReadAll(raw)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=gotochanger-dashboard.json")
	_, _ = w.Write(data)
}

// operationTypeByPattern maps a registered mux pattern to the
// operation_type label recorded for gotochanger_operations_total /
// gotochanger_operation_duration_seconds. Only state-changing library
// operations are listed - everything else (reads, admin CRUD, auth) is
// left uncounted as an "operation" but still contributes to
// gotochanger_errors_total via observeRequest below.
var operationTypeByPattern = map[string]string{
	"POST /api/v1/load":                     "load",
	"POST /api/v1/unload":                   "unload",
	"POST /api/v1/move":                     "move",
	"POST /api/v1/doors/io/{id}/open":       "door_open",
	"POST /api/v1/doors/io/{id}/close":      "door_close",
	"POST /api/v1/doors/storage/{id}/open":  "door_open",
	"POST /api/v1/doors/storage/{id}/close": "door_close",
	"POST /api/v1/offsite/send":             "offsite_send",
	"POST /api/v1/offsite/recall":           "offsite_recall",
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (rec *statusRecorder) WriteHeader(status int) {
	rec.status = status
	rec.ResponseWriter.WriteHeader(status)
}

// Flush forwards to the underlying ResponseWriter's http.Flusher, if any.
// Without this, statusRecorder (embedding http.ResponseWriter but not
// re-exposing Flush) would silently break the handleStream SSE handler's
// `w.(http.Flusher)` type assertion for every request that passes through
// metricsMiddleware - which, since it wraps the whole mux, is all of them
// - causing every SSE connection to fail with "streaming unsupported".
func (rec *statusRecorder) Flush() {
	if f, ok := rec.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// metricsMiddleware wraps the built mux (not just an http.Handler) so it
// can look up the matched route pattern via mux.Handler(r) before
// delegating - this is what lets gotochanger_operations_total/
// gotochanger_operation_duration_seconds/gotochanger_errors_total be
// recorded from one place with zero changes to any existing handler file.
// Applied identically to both PublicHandler (TCP) and TrustedHandler (Unix
// socket) - see server.go - so it captures gotochanger-changer/
// gotochangerctl/gotochanger-tcmud traffic on the trusted socket too, not
// just the HTTP API.
func (s *Server) metricsMiddleware(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pattern := mux.Handler(r)
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		mux.ServeHTTP(rec, r)
		s.observeRequest(pattern, rec.status, time.Since(start))
	})
}

func (s *Server) observeRequest(pattern string, status int, elapsed time.Duration) {
	if s.pm == nil {
		return
	}
	if opType, ok := operationTypeByPattern[pattern]; ok {
		s.pm.operationsTotal.Inc(map[string]string{"operation_type": opType})
		s.pm.operationDurationSeconds.Observe(elapsed.Seconds(), map[string]string{"operation_type": opType})
	}
	if status >= 400 {
		s.pm.errorsTotal.Inc(map[string]string{"error_type": errorTypeForStatus(status)})
	}
}

func errorTypeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusConflict:
		return "conflict"
	case http.StatusInternalServerError:
		return "internal"
	default:
		return "other"
	}
}
