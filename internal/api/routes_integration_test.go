package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/swenske/gotochanger/internal/config"
	"github.com/swenske/gotochanger/internal/library"
	"github.com/swenske/gotochanger/internal/store"
)

func newPublicTestServer(t *testing.T) *Server {
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
	settings := NewSettings(cfg, lib, nil, nil, nil)

	return New(lib, tokens, users, sessions, settings, cfg, nil, nil, nil, nil, filepath.Join(tmp, "backups"))
}

func loginCookie(t *testing.T, h http.Handler, username, password string) *http.Cookie {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, reqJSON(t, http.MethodPost, "/api/v1/auth/login", map[string]any{
		"username": username,
		"password": password,
	}))
	if rr.Code != http.StatusOK {
		t.Fatalf("login: expected %d got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == sessionCookieName {
			return c
		}
	}
	t.Fatalf("session cookie not found")
	return nil
}

func hasEventCode(events []library.Event, code string) bool {
	for _, e := range events {
		if e.Code == code {
			return true
		}
	}
	return false
}

func TestTrustedHandlerOutsideAndDoorRoutes(t *testing.T) {
	s := newTestServer(t)
	h := s.TrustedHandler()

	create := httptest.NewRecorder()
	h.ServeHTTP(create, reqJSON(t, http.MethodPost, "/api/v1/tape-sets/"+testTapeSet+"/tapes", map[string]any{
		"barcode": "TAPE0001",
	}))
	if create.Code != http.StatusCreated {
		t.Fatalf("create tape set tape: expected %d got %d body=%s", http.StatusCreated, create.Code, create.Body.String())
	}

	list := httptest.NewRecorder()
	h.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/v1/outside", nil))
	if list.Code != http.StatusOK {
		t.Fatalf("list outside: expected %d got %d body=%s", http.StatusOK, list.Code, list.Body.String())
	}
	var outside []map[string]any
	if err := json.Unmarshal(list.Body.Bytes(), &outside); err != nil {
		t.Fatalf("decode outside list: %v", err)
	}
	if len(outside) != 1 {
		t.Fatalf("expected one outside volume, got %d", len(outside))
	}

	ioAddr := s.lib.Status().IOSlots[0].Address

	open := httptest.NewRecorder()
	h.ServeHTTP(open, reqJSON(t, http.MethodPost, "/api/v1/doors/io/Mailbox1/open", map[string]any{}))
	if open.Code != http.StatusOK {
		t.Fatalf("open io door: expected %d got %d body=%s", http.StatusOK, open.Code, open.Body.String())
	}

	close := httptest.NewRecorder()
	h.ServeHTTP(close, reqJSON(t, http.MethodPost, "/api/v1/doors/io/Mailbox1/close", map[string]any{
		"actions": []map[string]any{{
			"action":  "load",
			"address": ioAddr,
			"barcode": "TAPE0001",
		}},
	}))
	if close.Code != http.StatusOK {
		t.Fatalf("close io door: expected %d got %d body=%s", http.StatusOK, close.Code, close.Body.String())
	}

	statusRR := httptest.NewRecorder()
	h.ServeHTTP(statusRR, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))
	if statusRR.Code != http.StatusOK {
		t.Fatalf("status: expected %d got %d body=%s", http.StatusOK, statusRR.Code, statusRR.Body.String())
	}
	st := s.lib.Status()
	for _, mbOpen := range st.Doors.OpenMailboxes {
		if mbOpen == "Mailbox1" {
			t.Fatalf("expected io door closed after close route")
		}
	}
	if st.IOSlots[0].Volume == nil || st.IOSlots[0].Volume.Barcode != "TAPE0001" {
		t.Fatalf("expected TAPE0001 in ioslot after close route")
	}
}

func TestTrustedHandlerRemovedRoutesReturnNotFound(t *testing.T) {
	s := newTestServer(t)
	h := s.TrustedHandler()

	for _, tc := range []struct {
		method string
		path   string
		body   any
	}{
		{method: http.MethodPost, path: "/api/v1/import", body: map[string]any{"ioslot": 21, "barcode": "TAPE0001"}},
		{method: http.MethodPost, path: "/api/v1/export", body: map[string]any{"slot": 1, "ioslot": 21}},
		{method: http.MethodPost, path: "/api/v1/eject", body: map[string]any{"ioslot": 21}},
		{method: http.MethodPost, path: "/api/v1/outside", body: map[string]any{"barcode": "TAPE0001", "capacity": "1MiB"}},
	} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, reqJSON(t, tc.method, tc.path, tc.body))
		if rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s: expected %d got %d body=%s", tc.method, tc.path, http.StatusMethodNotAllowed, rr.Code, rr.Body.String())
		}
	}
}

func TestPublicHandlerLoginLogoutEvents(t *testing.T) {
	s := newPublicTestServer(t)
	h := s.PublicHandler()

	bad := httptest.NewRecorder()
	h.ServeHTTP(bad, reqJSON(t, http.MethodPost, "/api/v1/auth/login", map[string]any{
		"username": "Admin",
		"password": "wrong-password",
	}))
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("bad login: expected %d got %d body=%s", http.StatusUnauthorized, bad.Code, bad.Body.String())
	}
	if !hasEventCode(s.lib.Events(), library.EventCodeAuthLoginFailure) {
		t.Fatalf("expected auth login failure event")
	}

	cookie := loginCookie(t, h, "Admin", "AdminPass123!")
	if !hasEventCode(s.lib.Events(), library.EventCodeAuthLoginSuccess) {
		t.Fatalf("expected auth login success event")
	}

	out := httptest.NewRecorder()
	logoutReq := reqJSON(t, http.MethodPost, "/api/v1/auth/logout", map[string]any{})
	logoutReq.AddCookie(cookie)
	h.ServeHTTP(out, logoutReq)
	if out.Code != http.StatusNoContent {
		t.Fatalf("logout: expected %d got %d body=%s", http.StatusNoContent, out.Code, out.Body.String())
	}
	if !hasEventCode(s.lib.Events(), library.EventCodeAuthLogoutSuccess) {
		t.Fatalf("expected auth logout success event")
	}
}

func TestPublicHandlerSettingsEventsWithSessionAuth(t *testing.T) {
	s := newPublicTestServer(t)
	h := s.PublicHandler()
	cookie := loginCookie(t, h, "Admin", "AdminPass123!")

	badBody := bytes.NewBufferString(`{"poll_interval":"not-a-duration"}`)
	badReq := httptest.NewRequest(http.MethodPut, "/api/v1/settings", badBody)
	badReq.Header.Set("Content-Type", "application/json")
	badReq.AddCookie(cookie)
	badRR := httptest.NewRecorder()
	h.ServeHTTP(badRR, badReq)
	if badRR.Code != http.StatusBadRequest {
		t.Fatalf("bad settings update: expected %d got %d body=%s", http.StatusBadRequest, badRR.Code, badRR.Body.String())
	}
	if !hasEventCode(s.lib.Events(), library.EventCodeConfigSettingsUpdateFailure) {
		t.Fatalf("expected config settings failure event")
	}

	okReq := reqJSON(t, http.MethodPut, "/api/v1/settings", map[string]any{"poll_interval": "7s"})
	okReq.AddCookie(cookie)
	okRR := httptest.NewRecorder()
	h.ServeHTTP(okRR, okReq)
	if okRR.Code != http.StatusOK {
		t.Fatalf("good settings update: expected %d got %d body=%s", http.StatusOK, okRR.Code, okRR.Body.String())
	}
	if !hasEventCode(s.lib.Events(), library.EventCodeConfigSettingsUpdateSuccess) {
		t.Fatalf("expected config settings success event")
	}
}
