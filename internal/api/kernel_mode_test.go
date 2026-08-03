package api

import (
	"os"
	"path/filepath"
	"testing"
)

// withKernelModePaths points the package-level path vars at fake files
// under t.TempDir() for the duration of the test, restoring the real
// (packaging/Docker-derived) paths afterward.
func withKernelModePaths(t *testing.T, tcmudExists, moduleExists, dockerEnvExists bool) {
	t.Helper()
	dir := t.TempDir()
	origTCMUD, origModule, origDocker := kernelTCMUDPath, kernelModulePath, dockerEnvPath
	t.Cleanup(func() { kernelTCMUDPath, kernelModulePath, dockerEnvPath = origTCMUD, origModule, origDocker })

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
	dockerEnvPath = filepath.Join(dir, ".dockerenv")
	if dockerEnvExists {
		if err := os.WriteFile(dockerEnvPath, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCurrentKernelModeStatusBothPresent(t *testing.T) {
	withKernelModePaths(t, true, true, false)
	st := currentKernelModeStatus()
	if !st.Available || st.MissingPackage || st.MissingKernelModule || st.InContainer {
		t.Fatalf("st = %+v, want fully available", st)
	}
}

func TestCurrentKernelModeStatusMissingPackage(t *testing.T) {
	withKernelModePaths(t, false, true, false)
	st := currentKernelModeStatus()
	if st.Available || !st.MissingPackage || st.MissingKernelModule {
		t.Fatalf("st = %+v, want only missing_package", st)
	}
}

func TestCurrentKernelModeStatusMissingKernelModule(t *testing.T) {
	withKernelModePaths(t, true, false, false)
	st := currentKernelModeStatus()
	if st.Available || st.MissingPackage || !st.MissingKernelModule {
		t.Fatalf("st = %+v, want only missing_kernel_module", st)
	}
}

func TestCurrentKernelModeStatusNeitherPresent(t *testing.T) {
	withKernelModePaths(t, false, false, false)
	st := currentKernelModeStatus()
	if st.Available || !st.MissingPackage || !st.MissingKernelModule {
		t.Fatalf("st = %+v, want both missing", st)
	}
}

func TestCurrentKernelModeStatusInContainer(t *testing.T) {
	withKernelModePaths(t, false, false, true)
	st := currentKernelModeStatus()
	if !st.InContainer {
		t.Fatalf("st = %+v, want in_container", st)
	}
}

// InContainer must never gate Available: even a container that happens to
// have both other checks pass (e.g. a privileged container with the
// package baked in and the host's module visible through sysfs) reports
// itself available - InContainer is a UI-messaging signal only.
func TestCurrentKernelModeStatusInContainerDoesNotGateAvailable(t *testing.T) {
	withKernelModePaths(t, true, true, true)
	st := currentKernelModeStatus()
	if !st.Available || !st.InContainer {
		t.Fatalf("st = %+v, want available and in_container both true", st)
	}
}
