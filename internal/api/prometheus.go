package api

import (
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/swenske/gotochanger/internal/library"
)

// metricsPath is the fixed, unauthenticated scrape endpoint - surfaced back
// to the Admin UI/CLI purely for reference, not configurable.
const metricsPath = "/metrics"

// prometheusMetrics holds every gotochanger_* metric handle, created once
// per Server against its own private *prometheus.Registry - never
// prometheus.DefaultRegisterer, so tests building multiple *Server
// instances (e.g. the bare &Server{} in handlers_test.go) never share
// state or hit "duplicate metrics collector registration" panics (this
// codebase has hit shared-state read/write races across Server instances
// before, see CLAUDE.md's "Correctness invariants"). Gauges are
// overwritten wholesale on every scrape from a fresh Status() snapshot
// (see refreshGaugeMetrics); counters and the histogram accumulate for the
// life of the process, fed by metricsMiddleware.
type prometheusMetrics struct {
	registry *prometheus.Registry

	slotsTotal    prometheus.Gauge
	slotsFree     prometheus.Gauge
	slotsOccupied prometheus.Gauge

	readersTotal  prometheus.Gauge
	readersIdle   prometheus.Gauge
	readersActive prometheus.Gauge
	readersFree   prometheus.Gauge
	readersError  prometheus.Gauge

	volumesTotal    prometheus.Gauge
	volumesByStatus *prometheus.GaugeVec

	magazinesTotal prometheus.Gauge

	capacityUtilizationPercent prometheus.Gauge
	queueDepth                 prometheus.Gauge
	uptimeSeconds              prometheus.Gauge
	lastBackupTimestamp        prometheus.Gauge

	operationsTotal          *prometheus.CounterVec
	operationDurationSeconds *prometheus.HistogramVec
	errorsTotal              *prometheus.CounterVec
}

func newPrometheusMetrics() *prometheusMetrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	f := promauto.With(reg)
	return &prometheusMetrics{
		registry: reg,

		slotsTotal:    f.NewGauge(prometheus.GaugeOpts{Name: "gotochanger_slots_total", Help: "Total storage slots"}),
		slotsFree:     f.NewGauge(prometheus.GaugeOpts{Name: "gotochanger_slots_free", Help: "Free storage slots"}),
		slotsOccupied: f.NewGauge(prometheus.GaugeOpts{Name: "gotochanger_slots_occupied", Help: "Occupied storage slots"}),

		readersTotal:  f.NewGauge(prometheus.GaugeOpts{Name: "gotochanger_readers_total", Help: "Total tape drives"}),
		readersIdle:   f.NewGauge(prometheus.GaugeOpts{Name: "gotochanger_readers_idle", Help: "Drives with a volume loaded but not currently reading or writing"}),
		readersActive: f.NewGauge(prometheus.GaugeOpts{Name: "gotochanger_readers_active", Help: "Drives currently reading or writing"}),
		readersFree:   f.NewGauge(prometheus.GaugeOpts{Name: "gotochanger_readers_free", Help: "Drives with no volume loaded"}),
		readersError:  f.NewGauge(prometheus.GaugeOpts{Name: "gotochanger_readers_error", Help: "Drives in a simulated fault state"}),

		volumesTotal:    f.NewGauge(prometheus.GaugeOpts{Name: "gotochanger_volumes_total", Help: "Total tape volumes known to the library"}),
		volumesByStatus: f.NewGaugeVec(prometheus.GaugeOpts{Name: "gotochanger_volumes_by_status", Help: "Tape volumes by location (status label: in_slot, in_ioslot, in_drive, outside, offsite)"}, []string{"status"}),

		magazinesTotal: f.NewGauge(prometheus.GaugeOpts{Name: "gotochanger_magazines_total", Help: "Total storage magazines"}),

		capacityUtilizationPercent: f.NewGauge(prometheus.GaugeOpts{Name: "gotochanger_capacity_utilization_percent", Help: "Occupied storage slots as a percentage of total storage slots"}),
		queueDepth:                 f.NewGauge(prometheus.GaugeOpts{Name: "gotochanger_queue_depth", Help: "1 if the single robotic arm is currently busy, 0 if idle (this simulator has one arm and no operation queue)"}),
		uptimeSeconds:              f.NewGauge(prometheus.GaugeOpts{Name: "gotochanger_uptime_seconds", Help: "Seconds since the daemon started"}),
		lastBackupTimestamp:        f.NewGauge(prometheus.GaugeOpts{Name: "gotochanger_last_backup_timestamp", Help: "Unix timestamp of the last configuration backup (state.db snapshot); 0 if none has ever been taken"}),

		operationsTotal:          f.NewCounterVec(prometheus.CounterOpts{Name: "gotochanger_operations_total", Help: "Total library operations executed, labeled by operation_type"}, []string{"operation_type"}),
		operationDurationSeconds: f.NewHistogramVec(prometheus.HistogramOpts{Name: "gotochanger_operation_duration_seconds", Help: "Library operation latency in seconds, labeled by operation_type", Buckets: []float64{.01, .05, .1, .5, 1, 2, 5, 10, 30}}, []string{"operation_type"}),
		errorsTotal:              f.NewCounterVec(prometheus.CounterOpts{Name: "gotochanger_errors_total", Help: "Total request errors, labeled by error_type"}, []string{"error_type"}),
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
	pm.slotsTotal.Set(float64(slotsTotal))
	pm.slotsOccupied.Set(float64(slotsOccupied))
	pm.slotsFree.Set(float64(slotsTotal - slotsOccupied))
	utilization := 0.0
	if slotsTotal > 0 {
		utilization = float64(slotsOccupied) / float64(slotsTotal) * 100
	}
	pm.capacityUtilizationPercent.Set(utilization)

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
	pm.readersTotal.Set(float64(len(st.Drives)))
	pm.readersIdle.Set(float64(idle))
	pm.readersActive.Set(float64(active))
	pm.readersFree.Set(float64(free))
	pm.readersError.Set(float64(errored))

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

	pm.volumesTotal.Set(float64(volumesTotal))
	pm.volumesByStatus.Reset()
	for status, n := range byStatus {
		pm.volumesByStatus.WithLabelValues(status).Set(float64(n))
	}

	magazinesTotal := 0
	if s.topology != nil {
		if mags, err := s.topology.ListMagazines(); err == nil {
			magazinesTotal = len(mags)
		}
	}
	pm.magazinesTotal.Set(float64(magazinesTotal))

	busy := 0.0
	if st.ArmState.Busy {
		busy = 1
	}
	pm.queueDepth.Set(busy)

	pm.uptimeSeconds.Set(time.Since(s.startedAt).Seconds())

	// Left at its zero value (0) until the first successful backup - there
	// is no cheap way to make a single, label-less prometheus.Gauge absent
	// again once registered (unlike the old hand-rolled Gauge.Delete),
	// so "0" is this metric's documented "never happened yet" value.
	if s.topology != nil {
		if v, ok, err := s.topology.GetSetting("last_backup_at"); err == nil && ok {
			if ts, convErr := strconv.ParseInt(v, 10, 64); convErr == nil {
				pm.lastBackupTimestamp.Set(float64(ts))
			}
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
	if s.pm != nil {
		promhttp.HandlerFor(s.pm.registry, promhttp.HandlerOpts{}).ServeHTTP(w, r)
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
		s.pm.operationsTotal.WithLabelValues(opType).Inc()
		s.pm.operationDurationSeconds.WithLabelValues(opType).Observe(elapsed.Seconds())
	}
	if status >= 400 {
		s.pm.errorsTotal.WithLabelValues(errorTypeForStatus(status)).Inc()
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
