package tcmu

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverStablePathsMatchesGenericAndTape(t *testing.T) {
	dir := t.TempDir()
	byID := filepath.Join(dir, "by-id")
	if err := os.Mkdir(byID, 0o755); err != nil {
		t.Fatal(err)
	}

	// Real target paths don't need to exist on disk for EvalSymlinks to
	// resolve them correctly as long as every hop in the chain is itself
	// a real, existing symlink - so point them at other symlinks within
	// the temp dir rather than /dev/sgN, which won't exist in a test
	// environment.
	genericTarget := filepath.Join(dir, "sg4")
	tapeTarget := filepath.Join(dir, "nst0")
	// Create the "real" targets as plain files so EvalSymlinks has
	// something concrete to resolve to.
	if err := os.WriteFile(genericTarget, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tapeTarget, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(genericTarget, filepath.Join(byID, "scsi-3deadbeef-changer")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(tapeTarget, filepath.Join(byID, "scsi-3deadbeef-nst")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "unrelated"), filepath.Join(byID, "scsi-3unrelated")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "unrelated"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	in := DevicePaths{Generic: genericTarget, Tape: tapeTarget}
	got := DiscoverStablePaths(in, byID)

	if got.StableGeneric != filepath.Join(byID, "scsi-3deadbeef-changer") {
		t.Errorf("StableGeneric = %q, want the changer symlink", got.StableGeneric)
	}
	if got.StableTape != filepath.Join(byID, "scsi-3deadbeef-nst") {
		t.Errorf("StableTape = %q, want the nst symlink", got.StableTape)
	}
}

func TestDiscoverStablePathsMissingByIDDirReturnsUnchanged(t *testing.T) {
	in := DevicePaths{Generic: "/dev/sg4", Tape: "/dev/nst0"}
	got := DiscoverStablePaths(in, filepath.Join(t.TempDir(), "does-not-exist"))
	if got != in {
		t.Errorf("got %+v, want unchanged %+v", got, in)
	}
}

func TestDiscoverStablePathsNoMatchLeavesStableFieldsEmpty(t *testing.T) {
	dir := t.TempDir()
	byID := filepath.Join(dir, "by-id")
	if err := os.Mkdir(byID, 0o755); err != nil {
		t.Fatal(err)
	}
	unrelatedTarget := filepath.Join(dir, "unrelated")
	if err := os.WriteFile(unrelatedTarget, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(unrelatedTarget, filepath.Join(byID, "scsi-3unrelated")); err != nil {
		t.Fatal(err)
	}

	in := DevicePaths{Generic: "/dev/sg4", Tape: "/dev/nst0"}
	got := DiscoverStablePaths(in, byID)
	if got.StableGeneric != "" || got.StableTape != "" {
		t.Errorf("got %+v, want both stable fields empty", got)
	}
}

func TestDiscoverStablePathsTwoSymlinksSameTargetPrefersChangerSuffix(t *testing.T) {
	// Mirrors a real changer, which gets both a plain "scsi-$ID" and a
	// "scsi-$ID-changer" symlink pointing at the same generic device -
	// the "-changer"-suffixed one is preferred (self-documenting, matches
	// what a human browsing /dev/tape/by-id/ or a generated Bareos
	// "Changer Device" line would expect), regardless of which one
	// os.ReadDir happens to list first.
	dir := t.TempDir()
	byID := filepath.Join(dir, "by-id")
	if err := os.Mkdir(byID, 0o755); err != nil {
		t.Fatal(err)
	}
	genericTarget := filepath.Join(dir, "sg4")
	if err := os.WriteFile(genericTarget, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(genericTarget, filepath.Join(byID, "scsi-3deadbeef")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(genericTarget, filepath.Join(byID, "scsi-3deadbeef-changer")); err != nil {
		t.Fatal(err)
	}

	in := DevicePaths{Generic: genericTarget}
	got := DiscoverStablePaths(in, byID)
	if got.StableGeneric != filepath.Join(byID, "scsi-3deadbeef-changer") {
		t.Errorf("StableGeneric = %q, want the -changer-suffixed symlink", got.StableGeneric)
	}
}
