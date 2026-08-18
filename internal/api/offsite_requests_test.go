package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swenske/gotochanger/internal/config"
	"github.com/swenske/gotochanger/internal/library"
	"github.com/swenske/gotochanger/internal/mailer"
	"github.com/swenske/gotochanger/internal/offsite"
	"github.com/swenske/gotochanger/internal/store"
)

// newOffsiteTestServer builds a full test Server wired to a real
// *library.Library and *store.Store (topology=persist=backup=st, mirroring
// newResetTestServer/newTopologyTestServer's exact wiring), plus a real
// *offsite.Engine sharing those same lib/st instances and a registered,
// configured, active scheduled_pickup mode. internal/api has no existing
// offsite-engine test wiring to reuse (this feature's handlers were
// previously untested at this layer), so this is new.
func newOffsiteTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	tmp := t.TempDir()

	cfg := config.Default()
	cfg.DataDir = tmp
	cfg.Library.OffsiteLocation = true
	cfg.Library.Magazines = []config.MagazineConfig{{ID: "Magazine1", Slots: 3}}
	cfg.Library.DriveDevices = []config.DriveDeviceConfig{{DevicePath: filepath.Join(tmp, "drives", "drive0")}}
	cfg.Library.TapeTypes = []config.TapeType{{Name: "TESTTYPE", Capacity: "1MiB", BarcodeFamily: "generic", VolSerLength: 8}}
	cfg.Library.TapeSets = []config.TapeSetConfig{{Name: testTapeSet, TapeType: "TESTTYPE", StorageFolder: filepath.Join(tmp, "ts1")}}

	st := store.New(filepath.Join(tmp, "state.db"))
	if err := st.Open(); err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

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
	settings := NewSettings(cfg, lib, nil, nil, nil, st)

	s := New(lib, tokens, users, sessions, settings, cfg, nil, st, st, st, filepath.Join(tmp, "backups"))

	registry := offsite.NewRegistry()
	registry.Register(offsite.NewScheduledPickupMode())
	eng := offsite.NewEngine(lib, st, mailer.NoopMailer{}, registry)
	s.SetOffsiteEngine(eng)

	spCfg, _ := json.Marshal(offsite.ScheduledPickupConfig{
		Weekday:                   strings.ToLower(time.Now().Weekday().String()),
		WindowStart:               "23:59",
		WindowEnd:                 "23:59",
		ExceptionalRetrievalDelay: "4h",
		MailboxIDs:                []string{"Mailbox1"},
	})
	if err := st.SetOffsiteModeConfig("scheduled_pickup", spCfg); err != nil {
		t.Fatalf("save scheduled_pickup config: %v", err)
	}
	if err := st.SetOffsiteActiveMode("scheduled_pickup"); err != nil {
		t.Fatalf("set active mode: %v", err)
	}

	return s, st
}

func rackOffsiteTestCartridge(t *testing.T, lib *library.Library, barcode string, slotAddr int) {
	t.Helper()
	if _, err := lib.CreateManualCartridge(testTapeSet, barcode); err != nil {
		t.Fatalf("create cartridge %s: %v", barcode, err)
	}
	if err := lib.OpenStorageDoor("Magazine1", ""); err != nil {
		t.Fatalf("open storage door: %v", err)
	}
	if err := lib.CloseStorageDoor("Magazine1", []library.DoorAction{{Action: "load", Address: slotAddr, Barcode: barcode}}); err != nil {
		t.Fatalf("rack %s into slot %d: %v", barcode, slotAddr, err)
	}
}

func TestOffsiteRequestsListRequiresAuth(t *testing.T) {
	s, _ := newOffsiteTestServer(t)
	h := s.PublicHandler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/offsite/requests", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected %d for an anonymous request, got %d body=%s", http.StatusUnauthorized, rr.Code, rr.Body.String())
	}
}

func TestOffsiteRequestsListAllowsViewer(t *testing.T) {
	s, _ := newOffsiteTestServer(t)
	h := s.PublicHandler()

	token, err := s.tokens.Add("viewer-token", RoleViewer)
	if err != nil {
		t.Fatalf("add viewer token: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/offsite/requests", nil)
	req.Header.Set("X-Api-Key", token)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected %d for a viewer, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
}

func TestOffsiteRequestsCancelRejectsViewer(t *testing.T) {
	s, _ := newOffsiteTestServer(t)
	h := s.PublicHandler()

	token, err := s.tokens.Add("viewer-token", RoleViewer)
	if err != nil {
		t.Fatalf("add viewer token: %v", err)
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/offsite/requests/does-not-matter", nil)
	req.Header.Set("X-Api-Key", token)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected %d for a viewer, got %d body=%s", http.StatusForbidden, rr.Code, rr.Body.String())
	}
}

func TestOffsiteRequestsCancelUnknownIDReturns404(t *testing.T) {
	s, _ := newOffsiteTestServer(t)
	h := s.PublicHandler()

	adminToken, err := s.tokens.Add("admin-token", RoleAdmin)
	if err != nil {
		t.Fatalf("add admin token: %v", err)
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/offsite/requests/does-not-exist", nil)
	req.Header.Set("X-Api-Key", adminToken)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected %d for an unknown request id, got %d body=%s", http.StatusNotFound, rr.Code, rr.Body.String())
	}
}

// TestOffsiteRequestsCancelNothingLeftReturns409AndClearsRBACForAdmin
// covers two things at once: an Admin token clears the RBAC gate (not
// 401/403), and a request whose id is real but has nothing live left to
// revert (here: never actually referenced by any volume) is a 409, not a
// 404 - distinct failure modes per internal/offsite.ErrRequestNotCancellable
// vs library.ErrNotFound.
func TestOffsiteRequestsCancelNothingLeftReturns409AndClearsRBACForAdmin(t *testing.T) {
	s, st := newOffsiteTestServer(t)
	h := s.PublicHandler()

	if err := st.CreateOffsiteRequest(store.OffsiteRequest{
		ID: "req-nothing-left", Kind: "pickup", Mode: "scheduled_pickup", Barcodes: []string{"VOLA0001"}, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed offsite request: %v", err)
	}

	adminToken, err := s.tokens.Add("admin-token", RoleAdmin)
	if err != nil {
		t.Fatalf("add admin token: %v", err)
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/offsite/requests/req-nothing-left", nil)
	req.Header.Set("X-Api-Key", adminToken)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected %d (nothing left to cancel), got %d body=%s", http.StatusConflict, rr.Code, rr.Body.String())
	}
}

func TestOffsiteRequestsCancelAlreadyCancelledReturns409(t *testing.T) {
	s, _ := newOffsiteTestServer(t)
	h := s.PublicHandler()
	lib := s.lib

	status := lib.Status()
	rackOffsiteTestCartridge(t, lib, "VOLA0001", status.Slots[0].Address)

	adminToken, err := s.tokens.Add("admin-token", RoleAdmin)
	if err != nil {
		t.Fatalf("add admin token: %v", err)
	}

	createRR := httptest.NewRecorder()
	createReq := reqJSON(t, http.MethodPost, "/api/v1/offsite/request", map[string]any{"pickup": []string{"VOLA0001"}})
	createReq.Header.Set("X-Api-Key", adminToken)
	h.ServeHTTP(createRR, createReq)
	if createRR.Code != http.StatusOK {
		t.Fatalf("create request: expected %d got %d body=%s", http.StatusOK, createRR.Code, createRR.Body.String())
	}
	var created offsiteRequestResult
	if err := json.Unmarshal(createRR.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create response: %v", err)
	}
	if created.PickupRequestID == "" {
		t.Fatalf("expected a pickup_request_id in the response, got %+v", created)
	}

	firstCancel := httptest.NewRecorder()
	firstReq := httptest.NewRequest(http.MethodDelete, "/api/v1/offsite/requests/"+created.PickupRequestID, nil)
	firstReq.Header.Set("X-Api-Key", adminToken)
	h.ServeHTTP(firstCancel, firstReq)
	if firstCancel.Code != http.StatusOK {
		t.Fatalf("first cancel: expected %d got %d body=%s", http.StatusOK, firstCancel.Code, firstCancel.Body.String())
	}

	secondCancel := httptest.NewRecorder()
	secondReq := httptest.NewRequest(http.MethodDelete, "/api/v1/offsite/requests/"+created.PickupRequestID, nil)
	secondReq.Header.Set("X-Api-Key", adminToken)
	h.ServeHTTP(secondCancel, secondReq)
	if secondCancel.Code != http.StatusConflict {
		t.Fatalf("second cancel: expected %d got %d body=%s", http.StatusConflict, secondCancel.Code, secondCancel.Body.String())
	}
}

// TestOffsiteRequestFullLifecycleThroughHTTP is the end-to-end flow: submit
// a real pickup request through POST /api/v1/offsite/request, see it
// listed as pending via GET /api/v1/offsite/requests, cancel it via
// DELETE, and see the status transition to cancelled - entirely through
// real HTTP handlers (RBAC middleware included), not direct handler/Engine
// calls.
func TestOffsiteRequestFullLifecycleThroughHTTP(t *testing.T) {
	s, _ := newOffsiteTestServer(t)
	h := s.PublicHandler()
	lib := s.lib

	status := lib.Status()
	rackOffsiteTestCartridge(t, lib, "VOLA0001", status.Slots[0].Address)

	adminToken, err := s.tokens.Add("admin-token", RoleAdmin)
	if err != nil {
		t.Fatalf("add admin token: %v", err)
	}

	createRR := httptest.NewRecorder()
	createReq := reqJSON(t, http.MethodPost, "/api/v1/offsite/request", map[string]any{"pickup": []string{"VOLA0001"}})
	createReq.Header.Set("X-Api-Key", adminToken)
	h.ServeHTTP(createRR, createReq)
	if createRR.Code != http.StatusOK {
		t.Fatalf("create request: expected %d got %d body=%s", http.StatusOK, createRR.Code, createRR.Body.String())
	}
	var created offsiteRequestResult
	if err := json.Unmarshal(createRR.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create response: %v", err)
	}
	if created.PickupRequestID == "" || len(created.Pickup) != 1 || !created.Pickup[0].OK {
		t.Fatalf("expected a successful pickup with a request id, got %+v", created)
	}

	list1RR := httptest.NewRecorder()
	list1Req := httptest.NewRequest(http.MethodGet, "/api/v1/offsite/requests", nil)
	list1Req.Header.Set("X-Api-Key", adminToken)
	h.ServeHTTP(list1RR, list1Req)
	if list1RR.Code != http.StatusOK {
		t.Fatalf("list (before cancel): expected %d got %d body=%s", http.StatusOK, list1RR.Code, list1RR.Body.String())
	}
	var views1 []offsiteRequestView
	if err := json.Unmarshal(list1RR.Body.Bytes(), &views1); err != nil {
		t.Fatalf("unmarshal list response: %v", err)
	}
	if len(views1) != 1 || views1[0].ID != created.PickupRequestID || views1[0].Status != "pending" {
		t.Fatalf("expected exactly one pending request matching the created id, got %+v", views1)
	}
	if len(views1[0].Barcodes) != 1 || views1[0].Barcodes[0] != "VOLA0001" {
		t.Fatalf("expected the listed request to carry its barcode, got %+v", views1[0])
	}

	cancelRR := httptest.NewRecorder()
	cancelReq := httptest.NewRequest(http.MethodDelete, "/api/v1/offsite/requests/"+created.PickupRequestID, nil)
	cancelReq.Header.Set("X-Api-Key", adminToken)
	h.ServeHTTP(cancelRR, cancelReq)
	if cancelRR.Code != http.StatusOK {
		t.Fatalf("cancel: expected %d got %d body=%s", http.StatusOK, cancelRR.Code, cancelRR.Body.String())
	}

	list2RR := httptest.NewRecorder()
	list2Req := httptest.NewRequest(http.MethodGet, "/api/v1/offsite/requests", nil)
	list2Req.Header.Set("X-Api-Key", adminToken)
	h.ServeHTTP(list2RR, list2Req)
	var views2 []offsiteRequestView
	if err := json.Unmarshal(list2RR.Body.Bytes(), &views2); err != nil {
		t.Fatalf("unmarshal second list response: %v", err)
	}
	if len(views2) != 1 || views2[0].Status != "cancelled" || views2[0].CancelledAt == nil {
		t.Fatalf("expected the request's status to transition to cancelled, got %+v", views2)
	}

	// The volume itself must be back to ordinary onsite state.
	slotVol := lib.Status().Slots[0].Volume
	if slotVol == nil || slotVol.Barcode != "VOLA0001" || slotVol.Offsite != nil {
		t.Fatalf("expected VOLA0001 back to ordinary onsite state after cancel, got %+v", slotVol)
	}
}
