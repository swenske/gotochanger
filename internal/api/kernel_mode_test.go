package api

import (
	"os"
	"path/filepath"
	"testing"
)

// withKernelModePaths points the package-level path vars at fake files
// under t.TempDir() for the duration of the test, restoring the real
// (packaging-derived) paths afterward.
func withKernelModePaths(t *testing.T, tcmudExists, moduleExists bool) {
	t.Helper()
	dir := t.TempDir()
	origTCMUD, origModule := kernelTCMUDPath, kernelModulePath
	t.Cleanup(func() { kernelTCMUDPath, kernelModulePath = origTCMUD, origModule })

	kernelTCMUDPath = filepath.Join(dir, "gotochanger-tcmud")
	if tcmudExists {
		if err := os.WriteFile(kernelTCMUDPath, nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	kernelModulePath = filepath.Join(dir, "target_core_user")
	if moduleExists {
		if err := os.Mkdir(kernelModulePath, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCurrentKernelModeStatusBothPresent(t *testing.T) {
	withKernelModePaths(t, true, true)
	st := currentKernelModeStatus()
	if !st.Available || st.MissingPackage || st.MissingKernelModule {
		t.Fatalf("st = %+v, want fully available", st)
	}
}

func TestCurrentKernelModeStatusMissingPackage(t *testing.T) {
	withKernelModePaths(t, false, true)
	st := currentKernelModeStatus()
	if st.Available || !st.MissingPackage || st.MissingKernelModule {
		t.Fatalf("st = %+v, want only missing_package", st)
	}
}

func TestCurrentKernelModeStatusMissingKernelModule(t *testing.T) {
	withKernelModePaths(t, true, false)
	st := currentKernelModeStatus()
	if st.Available || st.MissingPackage || !st.MissingKernelModule {
		t.Fatalf("st = %+v, want only missing_kernel_module", st)
	}
}

func TestCurrentKernelModeStatusNeitherPresent(t *testing.T) {
	withKernelModePaths(t, false, false)
	st := currentKernelModeStatus()
	if st.Available || !st.MissingPackage || !st.MissingKernelModule {
		t.Fatalf("st = %+v, want both missing", st)
	}
}
