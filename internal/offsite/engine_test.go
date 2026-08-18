package offsite

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/swenske/gotochanger/internal/config"
	"github.com/swenske/gotochanger/internal/library"
	"github.com/swenske/gotochanger/internal/mailer"
	"github.com/swenske/gotochanger/internal/store"
)

const testTapeSet = "TS1"

// newTestLibrary/newTestStore use real, small *library.Library/*store.Store
// instances rather than hand-rolled fakes - this package's own interfaces
// (LibraryOffsite/Store) are narrow cuts of those two concrete types
// specifically so tests can exercise the real thing, matching this
// codebase's stated preference (see CLAUDE.md's Tests section) for a real
// instance over mocks wherever practical.

func newTestLibrary(t *testing.T) *library.Library {
	t.Helper()
	tmp := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = tmp
	cfg.Library.OffsiteLocation = true
	cfg.Library.Magazines = []config.MagazineConfig{{ID: "Magazine1", Slots: 3}}
	cfg.Library.DriveDevices = []config.DriveDeviceConfig{{DevicePath: filepath.Join(tmp, "drives", "drive0")}}
	cfg.Library.DefaultCapacity = "1MiB"
	cfg.Library.TapeTypes = []config.TapeType{{Name: "TESTTYPE", Capacity: "1MiB", BarcodeFamily: "generic", VolSerLength: 8}}
	cfg.Library.TapeSets = []config.TapeSetConfig{{Name: testTapeSet, TapeType: "TESTTYPE", StorageFolder: filepath.Join(tmp, "ts1")}}
	lib, err := library.New(cfg, nil, nil, nil)
	if err != nil {
		t.Fatalf("new library: %v", err)
	}
	return lib
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s := store.New(filepath.Join(t.TempDir(), "state.db"))
	if err := s.Open(); err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// rackCartridge creates a cartridge and racks it into slotAddr via the
// storage-door staged-action flow - mirrors internal/library's own test
// helper of the same name, rewritten against the public API since this
// package can't reach Library's unexported fields directly.
func rackCartridge(t *testing.T, lib *library.Library, barcode string, slotAddr int) {
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

// capturingMailer records every Send call - mirrors internal/library's own
// capturingPersister test helper, the house style for observing a side
// effect without a full mock framework.
type capturingMailer struct {
	mu   sync.Mutex
	sent []mailer.Message
}

func (m *capturingMailer) Send(msg mailer.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, msg)
	return nil
}

func (m *capturingMailer) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent)
}

func (m *capturingMailer) last() mailer.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sent[len(m.sent)-1]
}

func containsBarcode(vols []*library.Volume, barcode string) bool {
	for _, v := range vols {
		if v != nil && v.Barcode == barcode {
			return true
		}
	}
	return false
}

func TestEngineSendNowRecordsMovementAndEvent(t *testing.T) {
	lib := newTestLibrary(t)
	st := newTestStore(t)
	eng := NewEngine(lib, st, mailer.NoopMailer{}, NewRegistry())

	status := lib.Status()
	slotAddr := status.Slots[0].Address
	rackCartridge(t, lib, "VOLA0001", slotAddr)

	vol, err := eng.SendNow(library.ElementRef{Kind: library.KindSlot, Address: slotAddr}, "continuous_rotation")
	if err != nil {
		t.Fatalf("send now: %v", err)
	}
	if vol.Offsite == nil || vol.Offsite.Mode != "continuous_rotation" {
		t.Fatalf("expected mode-attributed Offsite record, got %+v", vol.Offsite)
	}
	if !containsBarcode(lib.OffsiteVolumes(), "VOLA0001") {
		t.Fatalf("expected the volume to actually be vaulted")
	}

	movements, err := st.ListOffsiteMovements("", 10)
	if err != nil {
		t.Fatalf("list movements: %v", err)
	}
	if len(movements) != 1 || movements[0].Direction != "sent_offsite" || movements[0].Mode != "continuous_rotation" {
		t.Fatalf("expected one sent_offsite movement, got %+v", movements)
	}
}

func TestEngineRequestPickupOneThenCompleteDueTransfers(t *testing.T) {
	lib := newTestLibrary(t)
	st := newTestStore(t)
	eng := NewEngine(lib, st, mailer.NoopMailer{}, NewRegistry())

	status := lib.Status()
	slotAddr := status.Slots[0].Address
	rackCartridge(t, lib, "VOLA0001", slotAddr)

	// Already due (dueAt in the past) - the next sweep should complete it.
	res := eng.RequestPickupOne("VOLA0001", "scheduled_pickup", time.Now().Add(-time.Minute), "", "")
	if !res.OK {
		t.Fatalf("request pickup: %v", res.Error)
	}

	completedPickups, completedReturns := eng.CompleteDueTransfers("scheduled_pickup", time.Now())
	if len(completedReturns) != 0 {
		t.Fatalf("expected no completed returns, got %+v", completedReturns)
	}
	if len(completedPickups) != 1 || completedPickups[0].Barcode != "VOLA0001" {
		t.Fatalf("expected VOLA0001 to be completed, got %+v", completedPickups)
	}
	if !containsBarcode(lib.OffsiteVolumes(), "VOLA0001") {
		t.Fatalf("expected the volume to be vaulted after completion")
	}

	movements, err := st.ListOffsiteMovements("VOLA0001", 10)
	if err != nil {
		t.Fatalf("list movements: %v", err)
	}
	if len(movements) != 2 {
		t.Fatalf("expected pickup_requested + sent_offsite movements, got %+v", movements)
	}
}

func TestEngineCompleteDueTransfersSkipsNotYetDue(t *testing.T) {
	lib := newTestLibrary(t)
	st := newTestStore(t)
	eng := NewEngine(lib, st, mailer.NoopMailer{}, NewRegistry())

	status := lib.Status()
	slotAddr := status.Slots[0].Address
	rackCartridge(t, lib, "VOLA0001", slotAddr)

	res := eng.RequestPickupOne("VOLA0001", "scheduled_pickup", time.Now().Add(time.Hour), "", "")
	if !res.OK {
		t.Fatalf("request pickup: %v", res.Error)
	}

	completedPickups, _ := eng.CompleteDueTransfers("scheduled_pickup", time.Now())
	if len(completedPickups) != 0 {
		t.Fatalf("expected nothing completed before its due time, got %+v", completedPickups)
	}
	if containsBarcode(lib.OffsiteVolumes(), "VOLA0001") {
		t.Fatalf("expected the volume to remain onsite (not yet vaulted)")
	}
}
