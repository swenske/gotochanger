package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/swenske/gotochanger/internal/config"
	"github.com/swenske/gotochanger/internal/library"
	"github.com/swenske/gotochanger/internal/store"
)

// newTopologyTestServer builds a Server backed by a real *store.Store (like
// newResetTestServer), with driveCount drives and a tape type/tape set
// already seeded so a test can place real volumes via
// Library.CreateManualCartridge + the storage-door load flow. Every
// topology-affecting handler under test calls reconfigureFromStore, which
// rebuilds the Library entirely from what's in the store - so unlike
// newTestServer's bare in-memory config, everything the Library needs
// (drive devices, tape types, tape sets) must be persisted to the store
// too, not just present in the initial cfg.
func newTopologyTestServer(t *testing.T, driveCount int) *Server {
	t.Helper()
	tmp := t.TempDir()

	cfg := config.Default()
	cfg.DataDir = tmp
	for i := 0; i < driveCount; i++ {
		cfg.Library.DriveDevices = append(cfg.Library.DriveDevices, config.DriveDeviceConfig{DevicePath: filepath.Join(tmp, "drives", fmt.Sprintf("drive%d", i))})
	}
	cfg.Library.DefaultCapacity = "1MiB"
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
	if err := st.SaveDriveDevices(cfg.Library.DriveDevices); err != nil {
		t.Fatalf("save drive devices: %v", err)
	}
	if err := st.CreateTapeType(cfg.Library.TapeTypes[0]); err != nil {
		t.Fatalf("create tape type: %v", err)
	}
	if err := st.SaveTapeSets(cfg.Library.TapeSets); err != nil {
		t.Fatalf("save tape sets: %v", err)
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
	return New(lib, tokens, users, sessions, settings, cfg, nil, st, st, st, filepath.Join(tmp, "backups"))
}

func createMagazine(t *testing.T, h http.Handler, id string, slots int) {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, reqJSON(t, http.MethodPost, "/api/v1/magazines", map[string]any{"id": id, "slots": slots}))
	if rr.Code != http.StatusCreated {
		t.Fatalf("create magazine %s: expected %d got %d body=%s", id, http.StatusCreated, rr.Code, rr.Body.String())
	}
}

// loadVolumeIntoSlot places a freshly created outside volume into address
// via the public storage-door load flow (open door, stage a "load" action,
// close door) - the only way to get a volume into a slot from outside
// package library, which owns the fields a direct assignment would need.
func loadVolumeIntoSlot(t *testing.T, lib *library.Library, magazineID string, address int, barcode string) {
	t.Helper()
	if _, err := lib.CreateManualCartridge(testTapeSet, barcode); err != nil {
		t.Fatalf("create manual cartridge %s: %v", barcode, err)
	}
	if err := lib.OpenStorageDoor(magazineID, ""); err != nil {
		t.Fatalf("open storage door %s: %v", magazineID, err)
	}
	if err := lib.CloseStorageDoor(magazineID, []library.DoorAction{{Action: "load", Address: address, Barcode: barcode}}); err != nil {
		t.Fatalf("close storage door %s: %v", magazineID, err)
	}
}

func slotsInMagazineForTest(slots []*library.Slot, magazineID string) []*library.Slot {
	var out []*library.Slot
	for _, s := range slots {
		if s.MagazineID == magazineID {
			out = append(out, s)
		}
	}
	return out
}

// TestMagazineCreateProducesContiguousAddressesAndLabels is an end-to-end
// repro of the originally reported bug: creating magazine1 (10 slots) then
// magazine2 (10 slots) must leave magazine2 starting immediately after
// magazine1 - flat addresses 11-20, and magazine-relative labels
// "2.1".."2.10" - not a gap-reserving jump to address 21+.
func TestMagazineCreateProducesContiguousAddressesAndLabels(t *testing.T) {
	s := newTopologyTestServer(t, 1)
	h := s.TrustedHandler()

	createMagazine(t, h, "Magazine1", 10)
	createMagazine(t, h, "Magazine2", 10)

	mag2 := slotsInMagazineForTest(s.lib.Status().Slots, "Magazine2")
	if len(mag2) != 10 {
		t.Fatalf("expected 10 slots for Magazine2, got %d", len(mag2))
	}
	if mag2[0].Address != 11 || mag2[9].Address != 20 {
		t.Fatalf("expected Magazine2 addresses 11-20, got %d-%d", mag2[0].Address, mag2[9].Address)
	}
	if mag2[0].Label != "2.1" || mag2[9].Label != "2.10" {
		t.Fatalf("expected Magazine2 labels 2.1..2.10, got %q..%q", mag2[0].Label, mag2[9].Label)
	}
}

// TestUpdateMagazineRefusesShrinkWithOccupiedTail is a regression test for
// a pre-existing, addressing-adjacent gap the redesign surfaced: resize
// never checked occupancy at all, so shrinking a magazine below a
// currently-occupied slot count used to silently drop that slot's volume
// from tracking.
func TestUpdateMagazineRefusesShrinkWithOccupiedTail(t *testing.T) {
	s := newTopologyTestServer(t, 1)
	h := s.TrustedHandler()
	createMagazine(t, h, "Magazine1", 10)

	tail := slotsInMagazineForTest(s.lib.Status().Slots, "Magazine1")[9]
	loadVolumeIntoSlot(t, s.lib, "Magazine1", tail.Address, "VOLA0001")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, reqJSON(t, http.MethodPut, "/api/v1/magazines/Magazine1", map[string]any{"slots": 5}))
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected %d shrinking a magazine with an occupied tail slot, got %d body=%s", http.StatusConflict, rr.Code, rr.Body.String())
	}
	if got := slotsInMagazineForTest(s.lib.Status().Slots, "Magazine1"); len(got) != 10 {
		t.Fatalf("expected the refused shrink to leave all 10 slots in place, got %d", len(got))
	}
}

// TestUpdateMagazineRefusesShrinkWithDriveOriginInTail covers the other
// half of the same protection: a slot a drive has since checked a tape out
// of reads as empty (Volume == nil) even though it's still very much in
// use - only Drive.Origin still references it.
func TestUpdateMagazineRefusesShrinkWithDriveOriginInTail(t *testing.T) {
	s := newTopologyTestServer(t, 1)
	h := s.TrustedHandler()
	createMagazine(t, h, "Magazine1", 10)

	tail := slotsInMagazineForTest(s.lib.Status().Slots, "Magazine1")[9]
	loadVolumeIntoSlot(t, s.lib, "Magazine1", tail.Address, "VOLA0001")
	if err := s.lib.Load(library.ElementRef{Kind: library.KindSlot, Address: tail.Address}, 0, ""); err != nil {
		t.Fatalf("load tail slot into drive: %v", err)
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, reqJSON(t, http.MethodPut, "/api/v1/magazines/Magazine1", map[string]any{"slots": 5}))
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected %d shrinking a magazine whose tail a drive has a tape checked out of, got %d body=%s", http.StatusConflict, rr.Code, rr.Body.String())
	}
}

// TestDeleteMagazineRefusedWhenDriveOriginPointsIntoIt mirrors the
// existing "refuse delete of an occupied magazine" protection for the
// checked-out-to-a-drive case, which a plain Volume==nil scan can't see.
func TestDeleteMagazineRefusedWhenDriveOriginPointsIntoIt(t *testing.T) {
	s := newTopologyTestServer(t, 1)
	h := s.TrustedHandler()
	createMagazine(t, h, "Magazine1", 5)

	first := slotsInMagazineForTest(s.lib.Status().Slots, "Magazine1")[0]
	loadVolumeIntoSlot(t, s.lib, "Magazine1", first.Address, "VOLA0001")
	if err := s.lib.Load(library.ElementRef{Kind: library.KindSlot, Address: first.Address}, 0, ""); err != nil {
		t.Fatalf("load into drive: %v", err)
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, reqJSON(t, http.MethodDelete, "/api/v1/magazines/Magazine1", nil))
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected %d deleting a magazine a drive has a tape checked out of, got %d body=%s", http.StatusConflict, rr.Code, rr.Body.String())
	}
}

// TestUpdateMagazineGrowShiftsLaterEntities confirms growing an earlier
// magazine correctly pushes a later one's addresses (and flat address
// only - not its label, which is ordinal-based and unaffected by a
// resize) forward, with no gap and no collision.
func TestUpdateMagazineGrowShiftsLaterEntities(t *testing.T) {
	s := newTopologyTestServer(t, 1)
	h := s.TrustedHandler()
	createMagazine(t, h, "Magazine1", 5)
	createMagazine(t, h, "Magazine2", 5)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, reqJSON(t, http.MethodPut, "/api/v1/magazines/Magazine1", map[string]any{"slots": 10}))
	if rr.Code != http.StatusOK {
		t.Fatalf("grow Magazine1: expected %d got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}

	mag2 := slotsInMagazineForTest(s.lib.Status().Slots, "Magazine2")
	if len(mag2) != 5 {
		t.Fatalf("expected Magazine2 to keep 5 slots, got %d", len(mag2))
	}
	if mag2[0].Address != 11 {
		t.Fatalf("expected Magazine2 to shift to address 11 after Magazine1 grew to 10 slots, got %d", mag2[0].Address)
	}
	if mag2[0].Label != "2.1" {
		t.Fatalf("expected Magazine2's label to stay \"2.1\" (ordinal is unaffected by a resize), got %q", mag2[0].Label)
	}
}

// TestMailboxCreateProducesContiguousAddressesAndLabels mirrors the
// magazine creation test for mailboxes, which are numbered independently
// (see IOSlot.Label).
func TestMailboxCreateProducesContiguousAddressesAndLabels(t *testing.T) {
	s := newTopologyTestServer(t, 1)
	h := s.TrustedHandler()

	rr1 := httptest.NewRecorder()
	h.ServeHTTP(rr1, reqJSON(t, http.MethodPost, "/api/v1/mailboxes", map[string]any{"id": "Mailbox1", "slots": 3}))
	if rr1.Code != http.StatusCreated {
		t.Fatalf("create Mailbox1: expected %d got %d body=%s", http.StatusCreated, rr1.Code, rr1.Body.String())
	}
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, reqJSON(t, http.MethodPost, "/api/v1/mailboxes", map[string]any{"id": "Mailbox2", "slots": 2}))
	if rr2.Code != http.StatusCreated {
		t.Fatalf("create Mailbox2: expected %d got %d body=%s", http.StatusCreated, rr2.Code, rr2.Body.String())
	}

	var mb2 []*library.IOSlot
	for _, io := range s.lib.Status().IOSlots {
		if io.MailboxID == "Mailbox2" {
			mb2 = append(mb2, io)
		}
	}
	if len(mb2) != 2 {
		t.Fatalf("expected 2 ioslots for Mailbox2, got %d", len(mb2))
	}
	if mb2[0].Address != 4 || mb2[1].Address != 5 {
		t.Fatalf("expected Mailbox2 addresses 4-5 (right after Mailbox1's 3), got %d-%d", mb2[0].Address, mb2[1].Address)
	}
	if mb2[0].Label != "2.1" || mb2[1].Label != "2.2" {
		t.Fatalf("expected Mailbox2 labels 2.1/2.2, got %q/%q", mb2[0].Label, mb2[1].Label)
	}
}
