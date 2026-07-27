package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestKernelModeDevicesReportListClear(t *testing.T) {
	s := newTestServer(t)

	report := KernelModeDeviceReport{
		Changer: "/dev/sg4",
		Drives:  map[int]KernelModeDrivePaths{0: {Generic: "/dev/sg5"}, 2: {Generic: "/dev/sg6", Tape: "/dev/nst1"}},
	}

	rr := httptest.NewRecorder()
	req := reqJSON(t, http.MethodPost, "/api/v1/kernel-mode/devices/Library1", report)
	req.SetPathValue("instance", "Library1")
	s.handleReportKernelModeDevices(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("report: expected %d, got %d body=%s", http.StatusNoContent, rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	req = reqJSON(t, http.MethodGet, "/api/v1/kernel-mode/devices", nil)
	s.handleListKernelModeDevices(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list: expected %d, got %d", http.StatusOK, rr.Code)
	}
	if got := rr.Body.String(); got == "" || got == "{}\n" || got == "{}" {
		t.Fatalf("list: expected the reported instance to be present, got %q", got)
	}

	rr = httptest.NewRecorder()
	req = reqJSON(t, http.MethodDelete, "/api/v1/kernel-mode/devices/Library1", nil)
	req.SetPathValue("instance", "Library1")
	s.handleClearKernelModeDevices(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("clear: expected %d, got %d body=%s", http.StatusNoContent, rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	req = reqJSON(t, http.MethodGet, "/api/v1/kernel-mode/devices", nil)
	s.handleListKernelModeDevices(rr, req)
	if got := rr.Body.String(); got != "{}\n" && got != "{}" {
		t.Fatalf("list after clear: expected empty, got %q", got)
	}
}

func TestKernelModeDevicesReportBadJSON(t *testing.T) {
	s := newTestServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/kernel-mode/devices/Library1", nil)
	req.SetPathValue("instance", "Library1")
	s.handleReportKernelModeDevices(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected %d for an empty/invalid body, got %d", http.StatusBadRequest, rr.Code)
	}
}
