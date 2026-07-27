package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"

	"github.com/swenske/gotochanger/internal/library"
)

// stubDaemon is a minimal stand-in for gotochangerd on a Unix socket: it
// answers GET /api/v1/status with a fixed topology and records the decoded
// body of whatever mutating call the binary makes. Deliberately not the real
// internal/api server - the thing under test is which element kind
// gotochanger-changer *sends*, so a recorder pins it far more directly (and
// without this binary's test growing a dependency on the whole API layer).
type stubDaemon struct {
	mu       sync.Mutex
	status   library.Status
	lastPath string
	lastBody map[string]any
}

func (d *stubDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/v1/status" {
		d.mu.Lock()
		st := d.status
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(st)
		return
	}
	body, _ := io.ReadAll(r.Body)
	decoded := map[string]any{}
	_ = json.Unmarshal(body, &decoded)
	d.mu.Lock()
	d.lastPath = r.URL.Path
	d.lastBody = decoded
	d.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (d *stubDaemon) recorded() (string, map[string]any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastPath, d.lastBody
}

// startStubDaemon serves d on a Unix socket and points the binary at it via
// GOTOCHANGER_SOCKET, the same override a real deployment uses.
func startStubDaemon(t *testing.T, d *stubDaemon) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "gotochanger.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	srv := &http.Server{Handler: d}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	t.Setenv("GOTOCHANGER_SOCKET", sock)
}

// ioSlotStatus is a topology whose I/O slots sit right after the storage
// slots in one contiguous physical range, which is the addressing convention
// this binary presents to Bareos (see the package comment).
func ioSlotStatus() library.Status {
	return library.Status{
		Slots: []*library.Slot{
			{Address: 1},
			{Address: 2, Volume: &library.Volume{Barcode: "VOLA0001"}},
		},
		IOSlots: []*library.IOSlot{
			{Address: 21, Volume: &library.Volume{Barcode: "VOLB0001"}},
			{Address: 22},
		},
		Drives: []*library.Drive{{Index: 0}},
	}
}

// TestLoadFromIOSlotSendsIOSlotKind is the regression: load hardcoded the
// element kind "slot", so Bareos addressing an import/export slot - an
// address this binary itself advertises via "slots" (which reports the
// combined storage+I/O total) and "listall" (which prints it with an I:
// prefix) - was rejected with "element not found" even though the address
// was perfectly valid.
func TestLoadFromIOSlotSendsIOSlotKind(t *testing.T) {
	d := &stubDaemon{status: ioSlotStatus()}
	startStubDaemon(t, d)

	// Presented address 3 is the first I/O slot: storage slots renumber to
	// 1-2, then I/O slots continue at 3-4 (see internal/addressing).
	if err := run([]string{"ctl", "load", "3", "/dev/null", "0"}); err != nil {
		t.Fatalf("load from I/O slot: %v", err)
	}

	path, body := d.recorded()
	if path != "/api/v1/load" {
		t.Fatalf("expected a load call, got %q", path)
	}
	if body["from_kind"] != "ioslot" {
		t.Fatalf("from_kind = %v, want \"ioslot\"", body["from_kind"])
	}
	if got, ok := body["from_address"].(float64); !ok || int(got) != 21 {
		t.Fatalf("from_address = %v, want physical address 21", body["from_address"])
	}
}

// TestUnloadToIOSlotSendsIOSlotKind is the same regression on the unload
// path (exporting a tape from a drive straight into a mail slot).
func TestUnloadToIOSlotSendsIOSlotKind(t *testing.T) {
	d := &stubDaemon{status: ioSlotStatus()}
	startStubDaemon(t, d)

	// Presented address 4 is the second (empty) I/O slot.
	if err := run([]string{"ctl", "unload", "4", "/dev/null", "0"}); err != nil {
		t.Fatalf("unload to I/O slot: %v", err)
	}

	path, body := d.recorded()
	if path != "/api/v1/unload" {
		t.Fatalf("expected an unload call, got %q", path)
	}
	if body["to_kind"] != "ioslot" {
		t.Fatalf("to_kind = %v, want \"ioslot\"", body["to_kind"])
	}
	if got, ok := body["to_address"].(float64); !ok || int(got) != 22 {
		t.Fatalf("to_address = %v, want physical address 22", body["to_address"])
	}
}

// TestLoadFromStorageSlotStillSendsSlotKind guards the ordinary path the fix
// must not disturb.
func TestLoadFromStorageSlotStillSendsSlotKind(t *testing.T) {
	d := &stubDaemon{status: ioSlotStatus()}
	startStubDaemon(t, d)

	if err := run([]string{"ctl", "load", "2", "/dev/null", "0"}); err != nil {
		t.Fatalf("load from storage slot: %v", err)
	}

	_, body := d.recorded()
	if body["from_kind"] != "slot" {
		t.Fatalf("from_kind = %v, want \"slot\"", body["from_kind"])
	}
	if got, ok := body["from_address"].(float64); !ok || int(got) != 2 {
		t.Fatalf("from_address = %v, want physical address 2", body["from_address"])
	}
}
