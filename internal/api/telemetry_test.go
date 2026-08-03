package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/swenske/gotochanger/internal/telemetry"
)

// withFakeTelemetryEndpoint points telemetryEndpoint at srv for the
// duration of the test, restoring the real default endpoint afterward -
// so no test ever makes a real outbound call to the actual collector
// hostname.
func withFakeTelemetryEndpoint(t *testing.T, srv *httptest.Server) {
	t.Helper()
	orig := telemetryEndpoint
	telemetryEndpoint = srv.URL
	t.Cleanup(func() { telemetryEndpoint = orig })
}

func TestBuildTelemetryPayloadCounts(t *testing.T) {
	s := newPrometheusTestServer(t)

	p := s.buildTelemetryPayload()

	if p.OS != runtime.GOOS {
		t.Errorf("OS = %q, want %q", p.OS, runtime.GOOS)
	}
	if p.Arch != runtime.GOARCH {
		t.Errorf("Arch = %q, want %q", p.Arch, runtime.GOARCH)
	}
	if p.InstanceID == "" {
		t.Error("InstanceID is empty, want a generated instance id")
	}
	if p.MagazinesTotal != 1 {
		t.Errorf("MagazinesTotal = %d, want 1", p.MagazinesTotal)
	}
	if p.MailboxesTotal != 1 {
		t.Errorf("MailboxesTotal = %d, want 1", p.MailboxesTotal)
	}
	if p.DrivesTotal != 1 {
		t.Errorf("DrivesTotal = %d, want 1", p.DrivesTotal)
	}
	if p.SlotsTotal != 5 {
		t.Errorf("SlotsTotal = %d, want 5", p.SlotsTotal)
	}
	if p.IOSlotsTotal != 1 {
		t.Errorf("IOSlotsTotal = %d, want 1", p.IOSlotsTotal)
	}
	if p.VolumesTotal != 0 {
		t.Errorf("VolumesTotal = %d, want 0 (nothing loaded)", p.VolumesTotal)
	}
	if p.UsersTotal != 1 {
		t.Errorf("UsersTotal = %d, want 1 (just the bootstrap Admin)", p.UsersTotal)
	}
}

// TestTelemetrySettingsRBACAndToggle mirrors TestPrometheusSettingsToggle:
// the same Admin-only + toggle-persists shape, for the telemetry
// settings pair instead of the Prometheus one.
func TestTelemetrySettingsRBACAndToggle(t *testing.T) {
	s := newPrometheusTestServer(t)
	h := s.PublicHandler()

	unauth := httptest.NewRecorder()
	h.ServeHTTP(unauth, httptest.NewRequest(http.MethodGet, "/api/v1/settings/telemetry", nil))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauth GET: expected %d got %d body=%s", http.StatusUnauthorized, unauth.Code, unauth.Body.String())
	}

	cookie := loginCookie(t, h, DefaultAdminUsername, "AdminPass123!")

	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/settings/telemetry", nil)
	getReq.AddCookie(cookie)
	getRR := httptest.NewRecorder()
	h.ServeHTTP(getRR, getReq)
	if getRR.Code != http.StatusOK {
		t.Fatalf("GET: expected 200 got %d body=%s", getRR.Code, getRR.Body.String())
	}
	if !strings.Contains(getRR.Body.String(), `"enabled":false`) {
		t.Fatalf("expected enabled:false by default, got %s", getRR.Body.String())
	}

	putReq := reqJSON(t, http.MethodPut, "/api/v1/settings/telemetry", map[string]any{"enabled": true})
	putReq.AddCookie(cookie)
	putRR := httptest.NewRecorder()
	h.ServeHTTP(putRR, putReq)
	if putRR.Code != http.StatusOK {
		t.Fatalf("PUT: expected 200 got %d body=%s", putRR.Code, putRR.Body.String())
	}

	getRR2 := httptest.NewRecorder()
	getReq2 := httptest.NewRequest(http.MethodGet, "/api/v1/settings/telemetry", nil)
	getReq2.AddCookie(cookie)
	h.ServeHTTP(getRR2, getReq2)
	if !strings.Contains(getRR2.Body.String(), `"enabled":true`) {
		t.Fatalf("expected enabled:true after toggle, got %s", getRR2.Body.String())
	}
}

// TestTelemetryEnablingSendsImmediately is the regression test for the
// "opt-in takes effect right away" decision (sendTelemetryAsync's doc
// comment): flipping enabled false->true must fire exactly one send
// without waiting for a restart.
func TestTelemetryEnablingSendsImmediately(t *testing.T) {
	received := make(chan telemetry.Payload, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p telemetry.Payload
		_ = json.NewDecoder(r.Body).Decode(&p)
		received <- p
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	s := newPrometheusTestServer(t)
	withFakeTelemetryEndpoint(t, srv)
	h := s.PublicHandler()
	cookie := loginCookie(t, h, DefaultAdminUsername, "AdminPass123!")

	putReq := reqJSON(t, http.MethodPut, "/api/v1/settings/telemetry", map[string]any{"enabled": true})
	putReq.AddCookie(cookie)
	putRR := httptest.NewRecorder()
	h.ServeHTTP(putRR, putReq)
	if putRR.Code != http.StatusOK {
		t.Fatalf("PUT: expected 200 got %d body=%s", putRR.Code, putRR.Body.String())
	}

	select {
	case p := <-received:
		if p.InstanceID == "" {
			t.Error("received payload has empty InstanceID")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the immediate send triggered by enabling telemetry")
	}
}

// TestTelemetryReenablingAlreadyEnabledDoesNotResend proves the
// wasEnabled guard in handleUpdateTelemetrySettings actually gates on
// the false->true transition, not on every PUT with enabled:true.
func TestTelemetryReenablingAlreadyEnabledDoesNotResend(t *testing.T) {
	count := 0
	received := make(chan struct{}, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	s := newPrometheusTestServer(t)
	withFakeTelemetryEndpoint(t, srv)
	h := s.PublicHandler()
	cookie := loginCookie(t, h, DefaultAdminUsername, "AdminPass123!")

	enable := func() {
		req := reqJSON(t, http.MethodPut, "/api/v1/settings/telemetry", map[string]any{"enabled": true})
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("PUT: expected 200 got %d body=%s", rr.Code, rr.Body.String())
		}
	}

	enable() // false -> true: should send
	select {
	case <-received:
		count++
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the first send")
	}

	enable() // true -> true: should not send again
	select {
	case <-received:
		t.Fatal("received a second send for an already-enabled toggle, want none")
	case <-time.After(300 * time.Millisecond):
	}

	if count != 1 {
		t.Fatalf("count = %d, want exactly 1 send", count)
	}
}
