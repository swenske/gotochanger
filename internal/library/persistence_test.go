package library

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swenske/gotochanger/internal/config"
)

// rackCartridge creates a cartridge and racks it into slotAddr via the
// storage-door staged-action flow - the only path that moves a fresh
// tape-set cartridge from "outside" into a slot (there is no direct
// outside->slot verb).
func rackCartridge(t *testing.T, lib *Library, barcode string, slotAddr int) {
	t.Helper()
	if _, err := lib.CreateManualCartridge(testTapeSet, barcode); err != nil {
		t.Fatalf("create cartridge %s: %v", barcode, err)
	}
	if err := lib.OpenStorageDoor("Magazine1", ""); err != nil {
		t.Fatalf("open storage door: %v", err)
	}
	if err := lib.CloseStorageDoor("Magazine1", []DoorAction{{Action: "load", Address: slotAddr, Barcode: barcode}}); err != nil {
		t.Fatalf("rack %s into slot %d: %v", barcode, slotAddr, err)
	}
}

// TestOffsiteVolumesArePersisted covers a real data-loss bug: saveLocked
// enumerated the State fields by hand and simply omitted OffsiteVolumes,
// while State() (used only by tests) included it. OffsiteSend clears the
// source slot and moves the volume into l.offsite alone, so the persisted
// state recorded the cartridge as existing nowhere at all and the daemon
// forgot it on the next restart - the backing file survived on disk, but
// nothing referenced it any more.
func TestOffsiteVolumesArePersisted(t *testing.T) {
	lib := newTestLibrary(t)
	lib.cfg.Library.OffsiteLocation = true

	st := lib.Status()
	slotAddr := st.Slots[0].Address
	rackCartridge(t, lib, "VOLOFF01", slotAddr)

	persist := &capturingPersister{}
	lib.persist = persist

	if _, err := lib.OffsiteSend(ElementRef{Kind: KindSlot, Address: slotAddr}); err != nil {
		t.Fatalf("offsite send: %v", err)
	}

	saved := persist.last()
	if saved == nil {
		t.Fatalf("expected OffsiteSend to persist state")
	}
	if !containsBarcode(saved.OffsiteVolumes, "VOLOFF01") {
		t.Fatalf("persisted state does not carry the offsite volume: %+v", saved.OffsiteVolumes)
	}

	restored, err := New(lib.cfg, saved, nil, nil)
	if err != nil {
		t.Fatalf("restore library: %v", err)
	}
	if !containsBarcode(restored.OffsiteVolumes(), "VOLOFF01") {
		t.Fatalf("offsite volume lost across restart, got %+v", restored.OffsiteVolumes())
	}
}

// TestDriveOriginSurvivesRestart covers the other half of the same class of
// bug: Drive.Origin is persisted by saveLocked but restore() never copied it
// back, so after a daemon restart with a tape still mounted, DriveOriginSlot
// answered 0. That is what gotochanger-changer's "loaded"/"listall" output
// reports to Bareos, which uses it to know which slot to unload the tape
// back into.
func TestDriveOriginSurvivesRestart(t *testing.T) {
	lib := newTestLibrary(t)
	st := lib.Status()
	slotAddr := st.Slots[0].Address
	rackCartridge(t, lib, "VOLORG01", slotAddr)

	if err := lib.Load(ElementRef{Kind: KindSlot, Address: slotAddr}, 0, ""); err != nil {
		t.Fatalf("load: %v", err)
	}
	if got, err := lib.DriveOriginSlot(0); err != nil || got != slotAddr {
		t.Fatalf("before restart: DriveOriginSlot(0) = %d (err %v), want %d", got, err, slotAddr)
	}

	saved := lib.State()
	restored, err := New(lib.cfg, &saved, nil, nil)
	if err != nil {
		t.Fatalf("restore library: %v", err)
	}
	got, err := restored.DriveOriginSlot(0)
	if err != nil {
		t.Fatalf("DriveOriginSlot after restart: %v", err)
	}
	if got != slotAddr {
		t.Fatalf("after restart: DriveOriginSlot(0) = %d, want %d", got, slotAddr)
	}
}

// TestRestoreMatchesElementsByAddressNotPosition pins the change from
// position-based to address-based matching in restore(). The old code
// applied persisted placements by array index behind an all-or-nothing
// "only if the element counts still match" guard, so a topology that gained
// or lost an element while the daemon was down silently dropped *every*
// volume placement rather than the ones genuinely affected.
func TestRestoreMatchesElementsByAddressNotPosition(t *testing.T) {
	lib := newTestLibrary(t)
	st := lib.Status()
	rackCartridge(t, lib, "VOLADR01", st.Slots[1].Address)
	saved := lib.State()

	// Grow the magazine while "the daemon is down": same base address, more
	// slots, so every existing address still exists but the element count
	// no longer matches what was persisted.
	cfg := lib.cfg
	cfg.Library.Magazines = []config.MagazineConfig{{ID: "Magazine1", Slots: 5}}

	restored, err := New(cfg, &saved, nil, nil)
	if err != nil {
		t.Fatalf("restore library: %v", err)
	}
	found := false
	for _, s := range restored.Status().Slots {
		if s.Volume != nil && s.Volume.Barcode == "VOLADR01" {
			if s.Address != st.Slots[1].Address {
				t.Fatalf("volume restored to slot %d, want %d", s.Address, st.Slots[1].Address)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("volume placement dropped when the magazine grew from 2 to 5 slots")
	}
}

// TestStatusIsADeepSnapshot proves Status() no longer hands out the live
// element/volume structs. Callers (the HTTP handlers) marshal the result
// after Library's lock has been released, so aliasing live state means
// json.Marshal can read a field while Load/Unload/refreshVolumeSizeLocked
// writes it - the same unsynchronized-access class as the Event.Detail map
// crash cloneEventForNotify already fixed on the notify path.
func TestStatusIsADeepSnapshot(t *testing.T) {
	lib := newTestLibrary(t)
	slotAddr := lib.Status().Slots[0].Address
	rackCartridge(t, lib, "VOLSNP01", slotAddr)

	before := lib.Status()
	if err := lib.Load(ElementRef{Kind: KindSlot, Address: slotAddr}, 0, ""); err != nil {
		t.Fatalf("load: %v", err)
	}

	// The snapshot taken before the load must still describe the pre-load
	// world: a snapshot that aliased live state would now show the slot
	// empty and the drive full.
	for _, s := range before.Slots {
		if s.Address == slotAddr && s.Volume == nil {
			t.Fatalf("snapshot slot %d went empty after a later Load - Status() is aliasing live state", slotAddr)
		}
	}
	if before.Drives[0].Volume != nil {
		t.Fatalf("snapshot drive gained a volume after a later Load - Status() is aliasing live state")
	}

	// Mutating a snapshot must not reach back into the library either.
	before.Slots[0].Volume = nil
	if lib.Status().Slots[0].Volume != nil && before.Slots[0].Volume != nil {
		t.Fatalf("unexpected aliasing")
	}
}

// TestStatusLogicalLibrariesShareSnapshotPointers pins an invariant the web
// UI depends on: within one Status, a logical library's element lists hold
// the *same* pointers as the top-level Slots/IOSlots/Drives, so the two can
// be cross-referenced by identity. resolveLogicalLibraryLocked guarantees
// this for the live model, and the snapshot has to preserve it.
func TestStatusLogicalLibrariesShareSnapshotPointers(t *testing.T) {
	lib := newTestLibrary(t)
	if _, err := lib.AddLogicalLibrary(config.LogicalLibraryConfig{
		Name:      "Library1",
		Drives:    []int{0},
		Magazines: []string{"Magazine1"},
		Mailboxes: []string{"Mailbox1"},
	}); err != nil {
		t.Fatalf("add logical library: %v", err)
	}

	st := lib.Status()
	if len(st.LogicalLibs) != 1 {
		t.Fatalf("expected one logical library, got %d", len(st.LogicalLibs))
	}
	byAddr := map[int]*Slot{}
	for _, s := range st.Slots {
		byAddr[s.Address] = s
	}
	for _, s := range st.LogicalLibs[0].Slots {
		if byAddr[s.Address] != s {
			t.Fatalf("logical library slot %d is a different object from the top-level one", s.Address)
		}
	}
	if len(st.LogicalLibs[0].Drives) != 1 || st.LogicalLibs[0].Drives[0] != st.Drives[0] {
		t.Fatalf("logical library drive is a different object from the top-level one")
	}
}

// TestEventsDetailIsClonedForCallers is the reader-side counterpart to
// TestEmitNotifyDoesNotRaceWithAnnotateEventsSince: Events() is marshaled by
// an HTTP handler after the lock is released, so it must not hand back the
// same Detail map AnnotateEventsSince keeps writing into.
func TestEventsDetailIsClonedForCallers(t *testing.T) {
	lib := newTestLibrary(t)
	lib.RecordEvent(Event{Code: EventCodeAuthLoginFailure, Message: "failed login", Detail: map[string]string{"username": "Admin"}})

	first := lib.Events()
	if len(first) == 0 {
		t.Fatalf("expected at least one event")
	}
	// Writing into a returned event's Detail must not corrupt the library's
	// own copy, which is the same aliasing the race depends on.
	first[0].Detail["injected"] = "yes"
	for _, e := range lib.Events() {
		if _, ok := e.Detail["injected"]; ok {
			t.Fatalf("Events() aliases the stored Detail map")
		}
	}
}

// TestEventsMarshalDoesNotRaceWithAnnotate exercises the exact production
// shape: a handler JSON-marshaling Events()' result with no lock held while
// another goroutine annotates the same events. Only meaningful under -race.
func TestEventsMarshalDoesNotRaceWithAnnotate(t *testing.T) {
	lib := newTestLibrary(t)
	start := time.Now().UTC().Add(-time.Minute)
	for i := 0; i < 20; i++ {
		lib.RecordEvent(Event{Code: EventCodeAuthLoginFailure, Message: "failed login", Detail: map[string]string{"username": "Admin"}})
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			if _, err := json.Marshal(lib.Events()); err != nil {
				t.Errorf("marshal events: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			lib.AnnotateEventsSince(start, "Admin", "session", map[string]string{
				"source_ip": "127.0.0.1",
				"path":      "/api/v1/events",
			})
		}
	}()
	wg.Wait()
}

// TestStatusMarshalDoesNotRaceWithLoadUnload is the Status() equivalent:
// marshal snapshots with no lock held while the library really moves media.
// Only meaningful under -race.
func TestStatusMarshalDoesNotRaceWithLoadUnload(t *testing.T) {
	lib := newTestLibrary(t)
	slotAddr := lib.Status().Slots[0].Address
	rackCartridge(t, lib, "VOLRCE01", slotAddr)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			if _, err := json.Marshal(lib.Status()); err != nil {
				t.Errorf("marshal status: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 25; i++ {
			if err := lib.Load(ElementRef{Kind: KindSlot, Address: slotAddr}, 0, ""); err != nil {
				t.Errorf("load: %v", err)
				return
			}
			if err := lib.Unload(0, ElementRef{Kind: KindSlot, Address: slotAddr}, ""); err != nil {
				t.Errorf("unload: %v", err)
				return
			}
		}
	}()
	wg.Wait()
}

// TestOutsideOrderIsStableAcrossDoorClose pins the sort added to
// applyStorageActionsLocked/applyIOActionsLocked: both rebuild l.outside
// from a map, whose iteration order is random, so the dashboard's "Outside
// Tapes" card reshuffled on every door close.
func TestOutsideOrderIsStableAcrossDoorClose(t *testing.T) {
	lib := newTestLibrary(t)
	for _, bc := range []string{"VOLC0001", "VOLA0001", "VOLB0001"} {
		if _, err := lib.CreateManualCartridge(testTapeSet, bc); err != nil {
			t.Fatalf("create %s: %v", bc, err)
		}
	}
	slotAddr := lib.Status().Slots[0].Address

	// A door close that stages no action at all still rebuilds l.outside.
	for i := 0; i < 5; i++ {
		if err := lib.OpenStorageDoor("Magazine1", ""); err != nil {
			t.Fatalf("open storage door: %v", err)
		}
		if err := lib.CloseStorageDoor("Magazine1", nil); err != nil {
			t.Fatalf("close storage door: %v", err)
		}
		got := barcodesOf(lib.OutsideVolumes())
		want := []string{"VOLA0001", "VOLB0001", "VOLC0001"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("outside order = %v, want %v", got, want)
		}
	}
	_ = slotAddr
}

func barcodesOf(vols []*Volume) []string {
	out := make([]string, 0, len(vols))
	for _, v := range vols {
		out = append(out, v.Barcode)
	}
	return out
}

// capturingPersister records the most recent State handed to Save, standing
// in for store.Store's real SQLite write.
type capturingPersister struct {
	mu    sync.Mutex
	saved *State
}

func (p *capturingPersister) Save(s State) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := s
	p.saved = &st
	return nil
}

func (p *capturingPersister) last() *State {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.saved
}
