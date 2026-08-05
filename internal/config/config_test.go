package config

import "testing"

func TestValidateCleaningSettingsAcceptsZeroValue(t *testing.T) {
	// A fresh install (before SeedDefaults/the Admin > Cleaning Tapes page
	// has ever written one) has a zero-value CleaningSettings; it must not
	// fail Config.Validate() at startup.
	if err := ValidateCleaningSettings(CleaningSettings{}); err != nil {
		t.Fatalf("unexpected error for zero-value CleaningSettings: %v", err)
	}
}

func TestValidateCleaningSettingsAcceptsDefaults(t *testing.T) {
	if err := ValidateCleaningSettings(DefaultCleaningSettings()); err != nil {
		t.Fatalf("unexpected error for DefaultCleaningSettings(): %v", err)
	}
}

func TestValidateCleaningSettingsRejectsUnknownMode(t *testing.T) {
	cs := DefaultCleaningSettings()
	cs.Mode = "some_other_mode"
	if err := ValidateCleaningSettings(cs); err == nil {
		t.Fatalf("expected error for an unknown mode")
	}
}

func TestValidateCleaningSettingsRejectsOutOfRangeMaxUses(t *testing.T) {
	cs := DefaultCleaningSettings()
	cs.MaxUses = 0
	if err := ValidateCleaningSettings(cs); err == nil {
		t.Fatalf("expected error for max_uses below the minimum")
	}
}

func TestValidateCleaningSettingsRejectsUnparseableDuration(t *testing.T) {
	cs := DefaultCleaningSettings()
	cs.Duration = "not-a-duration"
	if err := ValidateCleaningSettings(cs); err == nil {
		t.Fatalf("expected error for unparseable duration")
	}
}

func TestValidateCleaningSettingsRejectsOutOfRangeDuration(t *testing.T) {
	cs := DefaultCleaningSettings()
	cs.Duration = "31m"
	if err := ValidateCleaningSettings(cs); err == nil {
		t.Fatalf("expected error for duration exceeding the 30m ceiling")
	}
}

func TestValidateTapeTypeAcceptsWellFormed(t *testing.T) {
	tt := TapeType{Name: "LTO-8", Capacity: "12TB", BarcodeFamily: "lto", MediaID: "L8", VolSerLength: 6}
	if err := ValidateTapeType(tt); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateTapeTypeAcceptsUnlimitedCapacity(t *testing.T) {
	tt := TapeType{Name: "Unlimited", Capacity: "unlimited", BarcodeFamily: "generic", VolSerLength: 8}
	if err := ValidateTapeType(tt); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateTapeTypeRejectsEmptyName(t *testing.T) {
	tt := TapeType{Capacity: "12TB", BarcodeFamily: "lto", MediaID: "L8", VolSerLength: 6}
	if err := ValidateTapeType(tt); err == nil {
		t.Fatalf("expected error for empty name")
	}
}

func TestValidateTapeTypeRejectsBadCapacity(t *testing.T) {
	tt := TapeType{Name: "LTO-8", Capacity: "not-a-size", BarcodeFamily: "lto", MediaID: "L8", VolSerLength: 6}
	if err := ValidateTapeType(tt); err == nil {
		t.Fatalf("expected error for invalid capacity")
	}
}

func TestValidateTapeTypeRejectsBadBarcodeFormat(t *testing.T) {
	cases := []TapeType{
		{Name: "LTO-Bad", Capacity: "12TB", BarcodeFamily: "lto", MediaID: "L8", VolSerLength: 5},    // wrong volser length
		{Name: "LTO-Bad2", Capacity: "12TB", BarcodeFamily: "lto", MediaID: "L", VolSerLength: 6},    // media id too short
		{Name: "LTO-Bad3", Capacity: "12TB", BarcodeFamily: "bogus", MediaID: "L8", VolSerLength: 6}, // unknown family
	}
	for _, tt := range cases {
		if err := ValidateTapeType(tt); err == nil {
			t.Errorf("ValidateTapeType(%+v): expected error, got nil", tt)
		}
	}
}

func TestValidateTapeSetAcceptsWellFormed(t *testing.T) {
	ts := TapeSetConfig{Name: "TapeSet1", TapeType: "LTO-8", StorageFolder: "/var/lib/gotochanger/tapesets/ts1"}
	known := map[string]bool{"LTO-8": true}
	if err := ValidateTapeSet(ts, known); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateTapeSetSkipsExistenceCheckWhenKnownIsNil(t *testing.T) {
	ts := TapeSetConfig{Name: "TapeSet1", TapeType: "AnythingGoes", StorageFolder: "/var/lib/gotochanger/tapesets/ts1"}
	if err := ValidateTapeSet(ts, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateTapeSetRejectsBadName(t *testing.T) {
	ts := TapeSetConfig{Name: "bad name with spaces", TapeType: "LTO-8", StorageFolder: "/x"}
	if err := ValidateTapeSet(ts, nil); err == nil {
		t.Fatalf("expected error for invalid name")
	}
}

func TestValidateTapeSetRejectsEmptyTapeType(t *testing.T) {
	ts := TapeSetConfig{Name: "TapeSet1", StorageFolder: "/x"}
	if err := ValidateTapeSet(ts, nil); err == nil {
		t.Fatalf("expected error for empty tape_type")
	}
}

func TestValidateTapeSetRejectsUnknownTapeType(t *testing.T) {
	ts := TapeSetConfig{Name: "TapeSet1", TapeType: "NoSuchType", StorageFolder: "/x"}
	known := map[string]bool{"LTO-8": true}
	if err := ValidateTapeSet(ts, known); err == nil {
		t.Fatalf("expected error for unknown tape type")
	}
}

func TestDefaultTapeTypesAreAllValid(t *testing.T) {
	for _, tt := range DefaultTapeTypes() {
		if err := ValidateTapeType(tt); err != nil {
			t.Errorf("default tape type %s failed validation: %v", tt.Name, err)
		}
	}
}

func TestValidateDriveTypeAcceptsWellFormed(t *testing.T) {
	dt := DriveType{Name: "LTO-9", Speed: "400MB/s", Capacity: "18TB", BarcodeFamily: "lto"}
	if err := ValidateDriveType(dt); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateDriveTypeAcceptsUnlimitedCapacity(t *testing.T) {
	dt := DriveType{Name: "Unlimited", Capacity: "unlimited", BarcodeFamily: "generic"}
	if err := ValidateDriveType(dt); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateDriveTypeRejectsEmptyName(t *testing.T) {
	dt := DriveType{Capacity: "18TB", BarcodeFamily: "lto"}
	if err := ValidateDriveType(dt); err == nil {
		t.Fatalf("expected error for empty name")
	}
}

func TestValidateDriveTypeRejectsBadCapacity(t *testing.T) {
	dt := DriveType{Name: "LTO-9", Capacity: "not-a-size", BarcodeFamily: "lto"}
	if err := ValidateDriveType(dt); err == nil {
		t.Fatalf("expected error for invalid capacity")
	}
}

func TestValidateDriveTypeRejectsUnknownBarcodeFamily(t *testing.T) {
	dt := DriveType{Name: "LTO-9", Capacity: "18TB", BarcodeFamily: "bogus"}
	if err := ValidateDriveType(dt); err == nil {
		t.Fatalf("expected error for unknown barcode family")
	}
}

func TestDefaultDriveTypesAreAllValid(t *testing.T) {
	for _, dt := range DefaultDriveTypes() {
		if err := ValidateDriveType(dt); err != nil {
			t.Errorf("default drive type %s failed validation: %v", dt.Name, err)
		}
	}
}

// TestDefaultCatalogsCoverEveryFamily is the regression test for the
// original bug report: SDLT/AIT/3592 tape types shipped with zero
// matching default drive type. Every barcode.Family (except the
// meaningless "no tape uses this family" case, which doesn't apply here
// since every physical family below has at least one default tape type)
// must have at least one default drive type of the same family.
func TestDefaultCatalogsCoverEveryFamily(t *testing.T) {
	driveFamilies := map[string]bool{}
	for _, dt := range DefaultDriveTypes() {
		driveFamilies[dt.BarcodeFamily] = true
	}
	tapeFamilies := map[string]bool{}
	for _, tt := range DefaultTapeTypes() {
		tapeFamilies[tt.BarcodeFamily] = true
	}
	for family := range tapeFamilies {
		if !driveFamilies[family] {
			t.Errorf("tape type family %q has no matching default drive type", family)
		}
	}
}

func TestValidateLogicalLibraryAcceptsNoMailboxes(t *testing.T) {
	// Mailboxes are optional: a logical library with drives and magazines
	// but zero mailboxes is a legitimate deployment that never needs
	// import/export I/O.
	lib := LogicalLibraryConfig{Name: "Library1", Drives: []int{0}, Magazines: []string{"Magazine1"}}
	if err := ValidateLogicalLibrary(lib); err != nil {
		t.Fatalf("unexpected error for a logical library with no mailboxes: %v", err)
	}
}

func TestValidateLogicalLibraryRejectsNoDrives(t *testing.T) {
	lib := LogicalLibraryConfig{Name: "Library1", Magazines: []string{"Magazine1"}}
	if err := ValidateLogicalLibrary(lib); err == nil {
		t.Fatalf("expected error for a logical library with no drives")
	}
}

func TestValidateLogicalLibraryRejectsNoMagazines(t *testing.T) {
	lib := LogicalLibraryConfig{Name: "Library1", Drives: []int{0}}
	if err := ValidateLogicalLibrary(lib); err == nil {
		t.Fatalf("expected error for a logical library with no magazines")
	}
}

func TestValidateLatencySettingsAcceptsZeroValue(t *testing.T) {
	// A fresh install (before SeedDefaults/the wizard/Admin > Latency has
	// ever written one) has a zero-value LatencySettings; it must not
	// fail Config.Validate() at startup.
	if err := ValidateLatencySettings(LatencySettings{}); err != nil {
		t.Fatalf("unexpected error for zero-value LatencySettings: %v", err)
	}
}

func TestValidateLatencySettingsAcceptsDefaults(t *testing.T) {
	if err := ValidateLatencySettings(DefaultLatencySettings()); err != nil {
		t.Fatalf("unexpected error for DefaultLatencySettings(): %v", err)
	}
}

func TestValidateLatencySettingsRejectsUnparseableDuration(t *testing.T) {
	ls := DefaultLatencySettings()
	ls.DriveLoad = "not-a-duration"
	if err := ValidateLatencySettings(ls); err == nil {
		t.Fatalf("expected error for unparseable drive_load")
	}
}

func TestValidateLatencySettingsRejectsOutOfRange(t *testing.T) {
	ls := DefaultLatencySettings()
	ls.RobotMoveTape = "10m"
	if err := ValidateLatencySettings(ls); err == nil {
		t.Fatalf("expected error for robot_move_tape exceeding the 5m ceiling")
	}
}

func TestValidateLatencySettingsAcceptsZeroDuration(t *testing.T) {
	ls := DefaultLatencySettings()
	ls.DoorAction = "0s"
	if err := ValidateLatencySettings(ls); err != nil {
		t.Fatalf("unexpected error for a 0s delay: %v", err)
	}
}
