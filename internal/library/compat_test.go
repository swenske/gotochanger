package library

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/swenske/gotochanger/internal/config"
)

// newFamilyTestLibrary builds a library with a small drive-type/tape-type
// catalog spanning two distinct physical families (lto, dds) plus a
// generic wildcard of each, for Library.Load's family-compatibility check
// (see barcode.Family / DriveType.BarcodeFamily). Five drive devices cover
// the scenarios that check must handle: index 0 is an "LTO"-family drive,
// 1 a "DDS"-family drive, 2 a "GENERIC" (wildcard) drive, 3 has no
// DriveType linked at all, 4 references a drive type name absent from
// DriveTypes (a dangling reference) - both 3 and 4 must fail open.
func newFamilyTestLibrary(t *testing.T) *Library {
	t.Helper()
	tmp := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = tmp
	cfg.Library.Magazines = []config.MagazineConfig{{ID: "Magazine1", Slots: 2}}
	cfg.Library.DriveDevices = []config.DriveDeviceConfig{
		{DevicePath: filepath.Join(tmp, "drives", "drive0"), DriveType: "LTO"},
		{DevicePath: filepath.Join(tmp, "drives", "drive1"), DriveType: "DDS"},
		{DevicePath: filepath.Join(tmp, "drives", "drive2"), DriveType: "GENERIC"},
		{DevicePath: filepath.Join(tmp, "drives", "drive3")},
		{DevicePath: filepath.Join(tmp, "drives", "drive4"), DriveType: "MISSING"},
	}
	cfg.Library.DefaultCapacity = "1MiB"
	cfg.Library.DriveTypes = []config.DriveType{
		{Name: "LTO", Capacity: "unlimited", BarcodeFamily: "lto"},
		{Name: "DDS", Capacity: "unlimited", BarcodeFamily: "dds"},
		{Name: "GENERIC", Capacity: "unlimited", BarcodeFamily: "generic"},
	}
	cfg.Library.TapeTypes = []config.TapeType{
		{Name: "LTOTAPE", Capacity: "1MiB", BarcodeFamily: "lto", VolSerLength: 8},
		{Name: "DDSTAPE", Capacity: "1MiB", BarcodeFamily: "dds", VolSerLength: 8},
		{Name: "GENERICTAPE", Capacity: "1MiB", BarcodeFamily: "generic", VolSerLength: 8},
	}
	cfg.Library.TapeSets = []config.TapeSetConfig{
		{Name: "LTOSET", TapeType: "LTOTAPE", StorageFolder: filepath.Join(tmp, "ltoset")},
		{Name: "DDSSET", TapeType: "DDSTAPE", StorageFolder: filepath.Join(tmp, "ddsset")},
		{Name: "GENERICSET", TapeType: "GENERICTAPE", StorageFolder: filepath.Join(tmp, "genericset")},
	}
	lib, err := New(cfg, nil, nil, nil)
	if err != nil {
		t.Fatalf("new library: %v", err)
	}
	return lib
}

// placeVolumeInFirstSlotWithTapeSet mirrors placeVolumeInFirstSlot, but
// lets the caller pick which tape set (and therefore which barcode family)
// the placed volume belongs to.
func placeVolumeInFirstSlotWithTapeSet(lib *Library, barcode, tapeSet string) {
	lib.slots[0].Volume = &Volume{Barcode: barcode, TapeSet: tapeSet, Path: filepath.Join(lib.cfg.DataDir, barcode)}
}

func TestLoadRejectsIncompatibleFamily(t *testing.T) {
	lib := newFamilyTestLibrary(t)
	placeVolumeInFirstSlotWithTapeSet(lib, "VOLA0001", "DDSSET")
	fromAddr := lib.slots[0].Address

	err := lib.Load(ElementRef{Kind: KindSlot, Address: fromAddr}, 0, "") // drive 0 is LTO-family
	if !errors.Is(err, ErrIncompatibleTapeFamily) {
		t.Fatalf("Load(DDS tape -> LTO drive) err = %v, want ErrIncompatibleTapeFamily", err)
	}
	if lib.drives[0].Volume != nil {
		t.Fatalf("expected the rejected load to leave the drive empty, got %+v", lib.drives[0].Volume)
	}
}

func TestLoadAllowsMatchingFamily(t *testing.T) {
	lib := newFamilyTestLibrary(t)
	placeVolumeInFirstSlotWithTapeSet(lib, "VOLA0001", "LTOSET")
	fromAddr := lib.slots[0].Address

	if err := lib.Load(ElementRef{Kind: KindSlot, Address: fromAddr}, 0, ""); err != nil { // drive 0 is LTO-family
		t.Fatalf("Load(LTO tape -> LTO drive): %v", err)
	}
	if lib.drives[0].Volume == nil || lib.drives[0].Volume.Barcode != "VOLA0001" {
		t.Fatalf("expected the tape to be loaded, got %+v", lib.drives[0].Volume)
	}
}

func TestLoadAllowsGenericDriveWithAnyTapeFamily(t *testing.T) {
	lib := newFamilyTestLibrary(t)
	placeVolumeInFirstSlotWithTapeSet(lib, "VOLA0001", "DDSSET")
	fromAddr := lib.slots[0].Address

	if err := lib.Load(ElementRef{Kind: KindSlot, Address: fromAddr}, 2, ""); err != nil { // drive 2 is GENERIC (wildcard)
		t.Fatalf("Load(DDS tape -> generic drive): %v", err)
	}
}

func TestLoadAllowsGenericTapeWithAnySpecificDrive(t *testing.T) {
	lib := newFamilyTestLibrary(t)
	placeVolumeInFirstSlotWithTapeSet(lib, "VOLA0001", "GENERICSET")
	fromAddr := lib.slots[0].Address

	if err := lib.Load(ElementRef{Kind: KindSlot, Address: fromAddr}, 1, ""); err != nil { // drive 1 is DDS-family
		t.Fatalf("Load(generic tape -> DDS drive): %v", err)
	}
}

func TestLoadExemptsCleaningTapeRegardlessOfFamily(t *testing.T) {
	lib := newFamilyTestLibrary(t)
	placeCleaningTapeInFirstSlot(lib, "00001CLN")
	fromAddr := lib.slots[0].Address

	if err := lib.Load(ElementRef{Kind: KindSlot, Address: fromAddr}, 0, ""); err != nil { // drive 0 is LTO-family
		t.Fatalf("Load(cleaning tape -> specific-family drive): %v", err)
	}
}

func TestLoadAllowsWhenDriveTypeUnlinked(t *testing.T) {
	lib := newFamilyTestLibrary(t)
	placeVolumeInFirstSlotWithTapeSet(lib, "VOLA0001", "DDSSET")
	fromAddr := lib.slots[0].Address

	if err := lib.Load(ElementRef{Kind: KindSlot, Address: fromAddr}, 3, ""); err != nil { // drive 3 has no DriveType linked
		t.Fatalf("Load(any tape -> unlinked drive): %v", err)
	}
}

func TestLoadAllowsWhenDriveTypeUnknown(t *testing.T) {
	lib := newFamilyTestLibrary(t)
	placeVolumeInFirstSlotWithTapeSet(lib, "VOLA0001", "DDSSET")
	fromAddr := lib.slots[0].Address

	if err := lib.Load(ElementRef{Kind: KindSlot, Address: fromAddr}, 4, ""); err != nil { // drive 4 references a nonexistent drive type
		t.Fatalf("Load(any tape -> dangling drive type reference): %v", err)
	}
}

func TestLoadAllowsWhenTapeSetUnresolvable(t *testing.T) {
	lib := newFamilyTestLibrary(t)
	placeVolumeInFirstSlotWithTapeSet(lib, "VOLA0001", "NoSuchTapeSet")
	fromAddr := lib.slots[0].Address

	if err := lib.Load(ElementRef{Kind: KindSlot, Address: fromAddr}, 0, ""); err != nil { // drive 0 is LTO-family
		t.Fatalf("Load(volume with unresolvable tape set -> specific-family drive): %v", err)
	}
}

// TestLoadExemptsCleaningTapeWhenCleaningManagementDisabled is a
// regression guard for gating the cleaning exemption on vol.Cleaning
// directly rather than on `l.cleaningEnabled && vol.Cleaning`: with
// cleaning management globally disabled, a cleaning-marked volume must
// still load into a specific-family drive like any other exempt cleaning
// tape (mirrors TestCleaningDisabledActsAsOrdinaryVolume in
// cleaning_test.go, plus a linked, specific-family drive type).
func TestLoadExemptsCleaningTapeWhenCleaningManagementDisabled(t *testing.T) {
	tmp := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = tmp
	cfg.Library.Magazines = []config.MagazineConfig{{ID: "Magazine1", Slots: 2}}
	cfg.Library.DriveDevices = []config.DriveDeviceConfig{{DevicePath: filepath.Join(tmp, "drives", "drive0"), DriveType: "LTO"}}
	cfg.Library.DriveTypes = []config.DriveType{{Name: "LTO", Capacity: "unlimited", BarcodeFamily: "lto"}}
	cfg.Library.Cleaning = config.CleaningSettings{Enabled: false, Mode: config.CleaningModeSoftware, MaxUses: 1, MountThreshold: 50, Duration: "0s"}
	lib, err := New(cfg, nil, nil, nil)
	if err != nil {
		t.Fatalf("new library: %v", err)
	}
	lib.slots[0].Volume = &Volume{Barcode: "00001CLN", Path: filepath.Join(tmp, "00001CLN"), Cleaning: true, CleaningState: CleaningTapeExpired}
	fromAddr := lib.slots[0].Address

	if err := lib.Load(ElementRef{Kind: KindSlot, Address: fromAddr}, 0, ""); err != nil {
		t.Fatalf("expected an expired cleaning tape to load into a specific-family drive while cleaning is disabled, got %v", err)
	}
}
