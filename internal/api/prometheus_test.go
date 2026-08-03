package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swenske/gotochanger/internal/config"
	"github.com/swenske/gotochanger/internal/library"
	"github.com/swenske/gotochanger/internal/store"
)

// newPrometheusTestServer is newPublicTestServer plus a topology/backup
// store actually wired into New() (newPublicTestServer passes nil for both,
// which is fine for the routes it's used to exercise, but the Prometheus
// settings/dashboard endpoints and gotochanger_magazines_total/
// gotochanger_last_backup_timestamp all need a real TopologyStore/
// BackupStore). SaveMagazines mirrors what the setup wizard does in real
// operation (internal/api/wizard.go) - without it the store's own
// magazines table stays empty even though the Library was constructed with
// one magazine, since Library builds its topology straight from cfg, never
// through the store.
func newPrometheusTestServer(t *testing.T) *Server {
	t.Helper()
	tmp := t.TempDir()

	cfg := config.Default()
	cfg.DataDir = tmp
	cfg.Library.Magazines = []config.MagazineConfig{{ID: "Magazine1", Slots: 5}}
	cfg.Library.Mailboxes = []config.MailboxConfig{{ID: "Mailbox1", Slots: 1}}
	cfg.Library.DriveDevices = []config.DriveDeviceConfig{{DevicePath: filepath.Join(tmp, "drives", "drive0")}}
	cfg.Library.TapeTypes = []config.TapeType{{Name: "TESTTYPE", Capacity: "1MiB", BarcodeFamily: "generic", VolSerLength: 8}}
	cfg.Library.TapeSets = []config.TapeSetConfig{{Name: testTapeSet, TapeType: "TESTTYPE", StorageFolder: filepath.Join(tmp, "ts1")}}

	st := store.New(filepath.Join(tmp, "state.db"))
	if err := st.Open(); err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SeedDefaults(); err != nil {
		t.Fatalf("seed defaults: %v", err)
	}
	if err := st.SaveMagazines(cfg.Library.Magazines); err != nil {
		t.Fatalf("save magazines: %v", err)
	}

	lib, err := library.New(cfg, nil, nil, nil)
	if err != nil {
		t.Fatalf("new library: %v", err)
	}
	tokens, _, err := LoadOrBootstrapTokenStore(st)
	if err != nil {
		t.Fatalf("load token store: %v", err)
	}
	users, err := LoadOrBootstrapUserStore(st)
	if err != nil {
		t.Fatalf("load user store: %v", err)
	}
	if err := users.SetInitialAdminPassword("AdminPass123!"); err != nil {
		t.Fatalf("set initial admin password: %v", err)
	}
	sessions := NewSessionStore()
	settings := NewSettings(cfg, lib, nil, nil, st)

	return New(lib, tokens, users, sessions, settings, cfg, nil, nil, st, st, filepath.Join(tmp, "backups"))
}

func enablePrometheus(t *testing.T, h http.Handler, cookie *http.Cookie) {
	t.Helper()
	req := reqJSON(t, http.MethodPut, "/api/v1/settings/prometheus", map[string]any{"enabled": true})
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("enable prometheus: expected 200 got %d body=%s", rr.Code, rr.Body.String())
	}
}

func scrapeMetrics(t *testing.T, h http.Handler) string {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("scrape /metrics: expected 200 got %d body=%s", rr.Code, rr.Body.String())
	}
	return rr.Body.String()
}

func TestMetricsEndpointDisabledByDefault(t *testing.T) {
	s := newPrometheusTestServer(t)
	h := s.PublicHandler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 while disabled, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestMetricsEndpointUnauthenticatedOnceEnabled is the ticket's core
// security requirement: /metrics must be reachable with zero credentials
// (no session cookie, no API token), even on the authenticated TCP
// listener (PublicHandler) - only the settings toggle that enables it is
// admin-gated.
func TestMetricsEndpointUnauthenticatedOnceEnabled(t *testing.T) {
	s := newPrometheusTestServer(t)
	h := s.PublicHandler()
	cookie := loginCookie(t, h, DefaultAdminUsername, "AdminPass123!")
	enablePrometheus(t, h, cookie)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	// Deliberately no Authorization header and no cookie.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 with no credentials, got %d body=%s", rr.Code, rr.Body.String())
	}

	body := rr.Body.String()
	for _, want := range []string{
		"gotochanger_slots_total 5",
		"gotochanger_slots_free 5",
		"gotochanger_slots_occupied 0",
		"gotochanger_readers_total 1",
		"gotochanger_readers_free 1",
		"gotochanger_magazines_total 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in exposition, got:\n%s", want, body)
		}
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("expected text/plain content type, got %q", ct)
	}
}

func TestPrometheusSettingsToggle(t *testing.T) {
	s := newPrometheusTestServer(t)
	h := s.PublicHandler()

	// Admin-only: unauthenticated GET is rejected.
	unauth := httptest.NewRecorder()
	h.ServeHTTP(unauth, httptest.NewRequest(http.MethodGet, "/api/v1/settings/prometheus", nil))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauth GET: expected %d got %d body=%s", http.StatusUnauthorized, unauth.Code, unauth.Body.String())
	}

	cookie := loginCookie(t, h, DefaultAdminUsername, "AdminPass123!")

	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/settings/prometheus", nil)
	getReq.AddCookie(cookie)
	getRR := httptest.NewRecorder()
	h.ServeHTTP(getRR, getReq)
	if getRR.Code != http.StatusOK {
		t.Fatalf("GET: expected 200 got %d body=%s", getRR.Code, getRR.Body.String())
	}
	if !strings.Contains(getRR.Body.String(), `"enabled":false`) {
		t.Fatalf("expected enabled:false by default, got %s", getRR.Body.String())
	}
	if !strings.Contains(getRR.Body.String(), `"metrics_path":"/metrics"`) {
		t.Fatalf("expected metrics_path in response, got %s", getRR.Body.String())
	}

	enablePrometheus(t, h, cookie)

	getRR2 := httptest.NewRecorder()
	getReq2 := httptest.NewRequest(http.MethodGet, "/api/v1/settings/prometheus", nil)
	getReq2.AddCookie(cookie)
	h.ServeHTTP(getRR2, getReq2)
	if !strings.Contains(getRR2.Body.String(), `"enabled":true`) {
		t.Fatalf("expected enabled:true after toggle, got %s", getRR2.Body.String())
	}
}

// TestPrometheusSettingPersistsAcrossFreshLoad simulates a daemon restart:
// the setting is written through the HTTP API, then re-read straight from
// the database via LoadDaemonSettings (what cmd/gotochangerd calls once at
// startup), bypassing the running Server/Settings entirely.
func TestPrometheusSettingPersistsAcrossFreshLoad(t *testing.T) {
	s := newPrometheusTestServer(t)
	h := s.PublicHandler()
	cookie := loginCookie(t, h, DefaultAdminUsername, "AdminPass123!")
	enablePrometheus(t, h, cookie)

	ds, err := s.topology.(*store.Store).LoadDaemonSettings()
	if err != nil {
		t.Fatalf("load daemon settings: %v", err)
	}
	if !ds.Prometheus.Enabled {
		t.Fatalf("expected prometheus.enabled=true to survive a fresh LoadDaemonSettings, got false")
	}
}

func TestPrometheusDashboardDownload(t *testing.T) {
	s := newPrometheusTestServer(t)
	h := s.PublicHandler()

	unauth := httptest.NewRecorder()
	h.ServeHTTP(unauth, httptest.NewRequest(http.MethodGet, "/api/v1/prometheus/dashboard", nil))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauth: expected %d got %d body=%s", http.StatusUnauthorized, unauth.Code, unauth.Body.String())
	}

	cookie := loginCookie(t, h, DefaultAdminUsername, "AdminPass123!")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/prometheus/dashboard", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("auth: expected %d got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	if cd := rr.Header().Get("Content-Disposition"); !strings.Contains(cd, "gotochanger-dashboard.json") {
		t.Fatalf("expected attachment filename in Content-Disposition, got %q", cd)
	}
	if !strings.Contains(rr.Body.String(), `"gotochanger_slots_total"`) {
		t.Fatalf("expected dashboard JSON referencing gotochanger metrics, got:\n%s", rr.Body.String())
	}
}

// TestMetricsReflectsLoadUnloadCycle drives Load/Unload through
// TrustedHandler (the transport gotochanger-changer/gotochangerctl/
// gotochanger-tcmud actually use in production, per CLAUDE.md) rather than
// PublicHandler, proving metricsMiddleware captures that path too - both
// handler trees share one mux (see PublicHandler/TrustedHandler in
// server.go), so this is what actually matters for real Bareos-driven
// usage, not just the HTTP API.
func TestMetricsReflectsLoadUnloadCycle(t *testing.T) {
	s := newPrometheusTestServer(t)
	public := s.PublicHandler()
	cookie := loginCookie(t, public, DefaultAdminUsername, "AdminPass123!")
	enablePrometheus(t, public, cookie)

	if _, err := s.lib.CreateManualCartridge(testTapeSet, "VOLA0001"); err != nil {
		t.Fatalf("create cartridge: %v", err)
	}
	slotAddr := s.lib.Status().Slots[0].Address
	if err := s.lib.OpenStorageDoor("Magazine1", ""); err != nil {
		t.Fatalf("open storage door: %v", err)
	}
	if err := s.lib.CloseStorageDoor("Magazine1", []library.DoorAction{{Action: "load", Address: slotAddr, Barcode: "VOLA0001"}}); err != nil {
		t.Fatalf("close storage door with load action: %v", err)
	}

	before := scrapeMetrics(t, public)
	if !strings.Contains(before, "gotochanger_readers_free 1") {
		t.Fatalf("expected one free reader before Load, got:\n%s", before)
	}
	if !strings.Contains(before, "gotochanger_slots_occupied 1") {
		t.Fatalf("expected one occupied slot before Load, got:\n%s", before)
	}

	trusted := s.TrustedHandler()
	loadRR := httptest.NewRecorder()
	trusted.ServeHTTP(loadRR, reqJSON(t, http.MethodPost, "/api/v1/load", map[string]any{
		"from_kind": "slot", "from_address": slotAddr, "drive": 0,
	}))
	if loadRR.Code != http.StatusOK {
		t.Fatalf("load via trusted socket: expected 200 got %d body=%s", loadRR.Code, loadRR.Body.String())
	}

	afterLoad := scrapeMetrics(t, public)
	for _, want := range []string{
		"gotochanger_readers_idle 1",
		"gotochanger_readers_free 0",
		"gotochanger_slots_occupied 0",
		`gotochanger_operations_total{operation_type="load"} 1`,
		`gotochanger_operation_duration_seconds_count{operation_type="load"} 1`,
	} {
		if !strings.Contains(afterLoad, want) {
			t.Errorf("after Load: expected %q, got:\n%s", want, afterLoad)
		}
	}

	unloadRR := httptest.NewRecorder()
	trusted.ServeHTTP(unloadRR, reqJSON(t, http.MethodPost, "/api/v1/unload", map[string]any{
		"drive": 0, "to_kind": "slot", "to_address": slotAddr,
	}))
	if unloadRR.Code != http.StatusOK {
		t.Fatalf("unload via trusted socket: expected 200 got %d body=%s", unloadRR.Code, unloadRR.Body.String())
	}

	afterUnload := scrapeMetrics(t, public)
	for _, want := range []string{
		"gotochanger_readers_free 1",
		"gotochanger_slots_occupied 1",
		`gotochanger_operations_total{operation_type="unload"} 1`,
		`gotochanger_operation_duration_seconds_count{operation_type="unload"} 1`,
	} {
		if !strings.Contains(afterUnload, want) {
			t.Errorf("after Unload: expected %q, got:\n%s", want, afterUnload)
		}
	}
}

// TestMetricsErrorsTotalCountsFailedOperations proves a rejected operation
// (Load from a slot with no volume) still counts as an attempted operation
// and as an error, both via metricsMiddleware/observeRequest.
func TestMetricsErrorsTotalCountsFailedOperations(t *testing.T) {
	s := newPrometheusTestServer(t)
	public := s.PublicHandler()
	cookie := loginCookie(t, public, DefaultAdminUsername, "AdminPass123!")
	enablePrometheus(t, public, cookie)

	emptySlotAddr := s.lib.Status().Slots[0].Address
	trusted := s.TrustedHandler()
	rr := httptest.NewRecorder()
	trusted.ServeHTTP(rr, reqJSON(t, http.MethodPost, "/api/v1/load", map[string]any{
		"from_kind": "slot", "from_address": emptySlotAddr, "drive": 0,
	}))
	if rr.Code == http.StatusOK {
		t.Fatalf("expected loading an empty slot to fail, got 200")
	}

	body := scrapeMetrics(t, public)
	if !strings.Contains(body, `gotochanger_operations_total{operation_type="load"} 1`) {
		t.Errorf("expected the failed attempt to still count as an operation, got:\n%s", body)
	}
	if !strings.Contains(body, "gotochanger_errors_total{error_type=") {
		t.Errorf("expected an errors_total sample for the failed load, got:\n%s", body)
	}
}

// TestLastBackupTimestampAfterManualDownload exercises the ticket's
// user-specified meaning of gotochanger_last_backup_timestamp: the last
// configuration backup taken via Admin > Backup. The manual download
// handler creates, streams, and deletes its backup file (see
// store.CreateBackupFile's doc comment) - last_backup_at must still be
// recorded even though no file is left behind, and must survive being
// re-read from a fresh topology load (simulating a restart).
//
// gotochanger_last_backup_timestamp reads 0 (not absent) before the first
// backup: a label-less prometheus.Gauge, once registered, has no cheap way
// to become absent again on a later scrape - "0" is this metric's
// documented "never happened yet" value (see prometheus.go's
// refreshGaugeMetrics comment).
func TestLastBackupTimestampAfterManualDownload(t *testing.T) {
	s := newPrometheusTestServer(t)
	h := s.PublicHandler()
	cookie := loginCookie(t, h, DefaultAdminUsername, "AdminPass123!")
	enablePrometheus(t, h, cookie)

	before := scrapeMetrics(t, h)
	if !strings.Contains(before, "gotochanger_last_backup_timestamp 0") {
		t.Fatalf("expected last_backup_timestamp 0 before any backup, got:\n%s", before)
	}

	dlReq := httptest.NewRequest(http.MethodGet, "/api/v1/backup/download", nil)
	dlReq.AddCookie(cookie)
	dlRR := httptest.NewRecorder()
	h.ServeHTTP(dlRR, dlReq)
	if dlRR.Code != http.StatusOK {
		t.Fatalf("backup download: expected 200 got %d body=%s", dlRR.Code, dlRR.Body.String())
	}

	after := scrapeMetrics(t, h)
	if strings.Contains(after, "gotochanger_last_backup_timestamp 0") {
		t.Fatalf("expected last_backup_timestamp to move off 0 after a manual download, got:\n%s", after)
	}

	v, ok, err := s.topology.GetSetting("last_backup_at")
	if err != nil || !ok || v == "" {
		t.Fatalf("expected last_backup_at persisted in the topology store, got %q ok=%v err=%v", v, ok, err)
	}
}
