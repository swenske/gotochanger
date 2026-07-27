package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/swenske/gotochanger/internal/config"
	"github.com/swenske/gotochanger/internal/library"
	"github.com/swenske/gotochanger/internal/store"
)

// newResetTestServer is like newPublicTestServer but wires the real
// *store.Store in as persist/topology/backup too (newPublicTestServer
// passes nil for all three), since handleReset needs all of them: a
// vtl_name to confirm against, a real database file to wipe, and a place
// to persist the post-reset audit event.
func newResetTestServer(t *testing.T) (*Server, *store.Store, string) {
	t.Helper()
	tmp := t.TempDir()

	cfg := config.Default()
	cfg.DataDir = tmp
	cfg.Library.Magazines = []config.MagazineConfig{{ID: "Magazine1", Slots: 2, BaseAddress: 1}}
	cfg.Library.DriveDevices = []config.DriveDeviceConfig{{DevicePath: filepath.Join(tmp, "drives", "drive0")}}
	cfg.Library.TapeTypes = []config.TapeType{{Name: "TESTTYPE", Capacity: "1MiB", BarcodeFamily: "generic", VolSerLength: 8}}
	cfg.Library.TapeSets = []config.TapeSetConfig{{Name: testTapeSet, TapeType: "TESTTYPE", StorageFolder: filepath.Join(tmp, "ts1")}}

	st := store.New(filepath.Join(tmp, "state.db"))
	if err := st.Open(); err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SetSetting("vtl_name", "VTL0"); err != nil {
		t.Fatalf("set vtl_name: %v", err)
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

	backupsDir := filepath.Join(tmp, "backups")
	s := New(lib, tokens, users, sessions, settings, cfg, nil, st, st, st, backupsDir)
	return s, st, tmp
}

func TestHandleResetRejectsWrongConfirmName(t *testing.T) {
	s, _, _ := newResetTestServer(t)
	rr := httptest.NewRecorder()
	req := reqJSON(t, http.MethodPost, "/api/v1/reset", map[string]any{"confirm_name": "not-the-name"})
	s.handleReset(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected %d, got %d body=%s", http.StatusBadRequest, rr.Code, rr.Body.String())
	}
}

func TestHandleResetSucceedsAndRestarts(t *testing.T) {
	s, st, _ := newResetTestServer(t)

	restarted := false
	s.SetRestartFunc(func() { restarted = true })

	rr := httptest.NewRecorder()
	req := reqJSON(t, http.MethodPost, "/api/v1/reset", map[string]any{"confirm_name": "VTL0"})
	s.handleReset(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	if !restarted {
		t.Fatal("expected restartFunc to be invoked on successful reset")
	}

	mags, err := st.ListMagazines()
	if err != nil {
		t.Fatalf("list magazines after reset: %v", err)
	}
	if len(mags) != 0 {
		t.Fatalf("expected no magazines after reset, got %+v", mags)
	}

	if name, ok, err := st.GetSetting("vtl_name"); err != nil {
		t.Fatalf("get vtl_name after reset: %v", err)
	} else if ok {
		t.Fatalf("expected vtl_name unset after reset, got %q", name)
	}

	backups, err := st.ListBackupFiles(filepath.Join(filepath.Dir(st.Path()), "backups"))
	if err != nil {
		t.Fatalf("list backups: %v", err)
	}
	if len(backups) != 1 {
		t.Fatalf("expected exactly one safety backup to have been taken, got %d", len(backups))
	}
}

func TestHandleResetDeleteVolumesRemovesFiles(t *testing.T) {
	s, _, tmp := newResetTestServer(t)

	rr := httptest.NewRecorder()
	req := reqJSON(t, http.MethodPost, "/api/v1/tape-sets/"+testTapeSet+"/tapes", map[string]any{"barcode": "TAPE0001"})
	req.SetPathValue("name", testTapeSet)
	s.handleAddTapeSetTapes(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create tape: expected %d got %d body=%s", http.StatusCreated, rr.Code, rr.Body.String())
	}
	volPath := filepath.Join(tmp, "ts1", "TAPE0001")
	if _, err := os.Stat(volPath); err != nil {
		t.Fatalf("expected cartridge file to exist before reset: %v", err)
	}

	resetRR := httptest.NewRecorder()
	resetReq := reqJSON(t, http.MethodPost, "/api/v1/reset", map[string]any{"confirm_name": "VTL0", "delete_volumes": true})
	s.handleReset(resetRR, resetReq)
	if resetRR.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d body=%s", http.StatusOK, resetRR.Code, resetRR.Body.String())
	}

	if _, err := os.Stat(volPath); !os.IsNotExist(err) {
		t.Fatalf("expected cartridge file to be deleted after reset with delete_volumes=true, stat err=%v", err)
	}
}
