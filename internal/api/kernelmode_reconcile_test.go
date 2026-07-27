package api

import (
	"reflect"
	"testing"

	"github.com/swenske/gotochanger/internal/library"
)

func TestKernelModeUnitName(t *testing.T) {
	if got, want := kernelModeUnitName("Library1"), "gotochanger-tcmud@Library1.service"; got != want {
		t.Errorf("kernelModeUnitName(%q) = %q, want %q", "Library1", got, want)
	}
}

func TestDesiredKernelModeInstancesNoLogicalLibraries(t *testing.T) {
	got := desiredKernelModeInstances(nil)
	want := map[string]bool{"default": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("desiredKernelModeInstances(nil) = %v, want %v", got, want)
	}
}

func TestDesiredKernelModeInstancesOnePerLogicalLibrary(t *testing.T) {
	libs := []*library.LogicalLibrary{{Name: "Library1"}, {Name: "Library2"}}
	got := desiredKernelModeInstances(libs)
	want := map[string]bool{"Library1": true, "Library2": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("desiredKernelModeInstances(libs) = %v, want %v", got, want)
	}
}

func TestParseActiveKernelModeInstances(t *testing.T) {
	// Real shape of `systemctl list-units --type=service --no-legend
	// --plain --state=active gotochanger-tcmud@*.service` (UNIT LOAD
	// ACTIVE SUB DESCRIPTION) - --state=active already filters server-side,
	// so every line here is an instance that's actually running.
	output := "gotochanger-tcmud@Library1.service loaded active running gotochanger kernel-mode backend (TCMU/LIO) for Library1\n"
	got := parseActiveKernelModeInstances(output)
	want := map[string]bool{"Library1": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseActiveKernelModeInstances(...) = %v, want %v", got, want)
	}
}

func TestParseActiveKernelModeInstancesEmpty(t *testing.T) {
	got := parseActiveKernelModeInstances("")
	if len(got) != 0 {
		t.Errorf("parseActiveKernelModeInstances(\"\") = %v, want empty", got)
	}
}

func TestParseActiveKernelModeInstancesIgnoresUnrelatedUnits(t *testing.T) {
	output := "gotochanger.service loaded active running gotochanger - fake tape autochanger simulator\n" +
		"gotochanger-tcmud@Library1.service loaded active running gotochanger kernel-mode backend (TCMU/LIO) for Library1\n"
	got := parseActiveKernelModeInstances(output)
	want := map[string]bool{"Library1": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseActiveKernelModeInstances(...) = %v, want %v (unrelated unit leaked in)", got, want)
	}
}

func TestExpectedKernelModeDriveSetsNoLogicalLibraries(t *testing.T) {
	drives := []*library.Drive{{Index: 0}, {Index: 1}, {Index: 4}}
	got := expectedKernelModeDriveSets(nil, drives)
	want := map[string]map[int]bool{"default": {0: true, 1: true, 4: true}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("expectedKernelModeDriveSets(nil, drives) = %v, want %v", got, want)
	}
}

func TestExpectedKernelModeDriveSetsPerLogicalLibrary(t *testing.T) {
	libs := []*library.LogicalLibrary{
		{Name: "Library1", Drives: []*library.Drive{{Index: 0}, {Index: 1}}},
		{Name: "Library2", Drives: []*library.Drive{{Index: 2}, {Index: 3}}},
	}
	got := expectedKernelModeDriveSets(libs, nil)
	want := map[string]map[int]bool{
		"Library1": {0: true, 1: true},
		"Library2": {2: true, 3: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("expectedKernelModeDriveSets(libs, nil) = %v, want %v", got, want)
	}
}

func TestDriveIndexSetsEqual(t *testing.T) {
	cases := []struct {
		name string
		a, b map[int]bool
		want bool
	}{
		{"both empty", map[int]bool{}, map[int]bool{}, true},
		{"nil vs empty", nil, map[int]bool{}, true},
		{"same members", map[int]bool{0: true, 1: true}, map[int]bool{1: true, 0: true}, true},
		{"different size", map[int]bool{0: true}, map[int]bool{0: true, 1: true}, false},
		{"same size different members", map[int]bool{0: true, 2: true}, map[int]bool{0: true, 1: true}, false},
	}
	for _, c := range cases {
		if got := driveIndexSetsEqual(c.a, c.b); got != c.want {
			t.Errorf("%s: driveIndexSetsEqual(%v, %v) = %v, want %v", c.name, c.a, c.b, got, c.want)
		}
	}
}

func TestRestartKernelModeInstanceIfDriveSetChangedNoReportDoesNothing(t *testing.T) {
	s := newTestServer(t)
	// No report exists at all for "Library1" - must not attempt a restart
	// (there's a real systemctl call behind runSystemctl, which would fail
	// loudly in this test environment; asserting "no panic, no attempt" is
	// the point, not asserting on that failure).
	s.restartKernelModeInstanceIfDriveSetChanged("Library1", map[int]bool{0: true}, false)
}

func TestRestartKernelModeInstanceIfDriveSetChangedMatchingSetDoesNothing(t *testing.T) {
	s := newTestServer(t)
	s.kernelModeDevices = map[string]KernelModeDeviceReport{}
	s.kernelModeDevices["Library1"] = KernelModeDeviceReport{
		Changer: "/dev/sg4",
		Drives:  map[int]KernelModeDrivePaths{0: {Generic: "/dev/sg5"}, 1: {Generic: "/dev/sg6"}},
	}
	// Matches exactly - restartKernelModeInstanceIfDriveSetChanged must
	// return before ever calling runSystemctl.
	s.restartKernelModeInstanceIfDriveSetChanged("Library1", map[int]bool{0: true, 1: true}, false)
}

func TestShouldRestartKernelModeInstance(t *testing.T) {
	cases := []struct {
		name       string
		reported   map[int]bool
		reportedOK bool
		expected   map[int]bool
		force      bool
		want       bool
	}{
		{"no report, no force", nil, false, map[int]bool{0: true}, false, false},
		{"matching set, no force", map[int]bool{0: true}, true, map[int]bool{0: true}, false, false},
		{"mismatched set, no force", map[int]bool{0: true}, true, map[int]bool{1: true}, false, true},
		{"no report, forced", nil, false, map[int]bool{0: true}, true, true},
		{"matching set, forced", map[int]bool{0: true}, true, map[int]bool{0: true}, true, true},
		{"mismatched set, forced", map[int]bool{0: true}, true, map[int]bool{1: true}, true, true},
	}
	for _, c := range cases {
		if got := shouldRestartKernelModeInstance(c.reported, c.reportedOK, c.expected, c.force); got != c.want {
			t.Errorf("%s: shouldRestartKernelModeInstance(%v, %v, %v, %v) = %v, want %v",
				c.name, c.reported, c.reportedOK, c.expected, c.force, got, c.want)
		}
	}
}

func TestReportedKernelModeDriveIndices(t *testing.T) {
	s := newTestServer(t)
	if _, ok := s.reportedKernelModeDriveIndices("Library1"); ok {
		t.Fatal("expected no report before one is ever posted")
	}
	s.kernelModeDevices = map[string]KernelModeDeviceReport{}
	s.kernelModeDevices["Library1"] = KernelModeDeviceReport{
		Drives: map[int]KernelModeDrivePaths{2: {Generic: "/dev/sg8"}, 3: {Generic: "/dev/sg9"}},
	}
	got, ok := s.reportedKernelModeDriveIndices("Library1")
	if !ok {
		t.Fatal("expected a report to be present")
	}
	want := map[int]bool{2: true, 3: true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("reportedKernelModeDriveIndices(Library1) = %v, want %v", got, want)
	}
}
