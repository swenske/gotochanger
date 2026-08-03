package api

import (
	"net/http"
	"os"
)

// kernelTCMUDPath/kernelModulePath/dockerEnvPath are fixed Debian
// packaging/Docker conventions (see Makefile's install target,
// systemd/gotochanger-tcmud@.service, and Docker's own convention of
// creating /.dockerenv in every container it starts, regardless of base
// image), not admin-configurable - vars rather than consts only so tests
// can point them at a fake path instead of the real filesystem.
var (
	kernelTCMUDPath  = "/usr/sbin/gotochanger-tcmud"
	kernelModulePath = "/sys/module/target_core_user"
	dockerEnvPath    = "/.dockerenv"
)

// KernelModeStatus reports whether this host is ready to run
// gotochanger-tcmud (see cmd/gotochanger-tcmud) - three independent,
// purely filesystem-based checks, nothing privileged attempted: whether
// the gotochanger-kernel package is installed (its binary exists at the
// path Makefile's install target puts it at), whether the
// target_core_user kernel module is loaded right now, and whether
// gotochangerd itself is running inside a Docker container. The three
// are deliberately reported separately (not just one combined bool) so
// the wizard/Admin UI can show a specific, actionable hint - "install
// the package" vs. "loaded automatically at next boot, or run modprobe
// target_core_user now" vs. "not possible inside Docker" are different
// instructions for the operator. InContainer is intentionally not folded
// into MissingKernelModule: inside a container, /sys is often passed
// through from the host, so the module can show as loaded even though
// kernel mode still can't work there (no host systemd/polkit/TCMU
// access) - it needs its own signal rather than being inferred from the
// other two. It also never affects Available, which stays a pure
// function of the two real capability checks.
type KernelModeStatus struct {
	Available           bool `json:"available"`
	MissingPackage      bool `json:"missing_package"`
	MissingKernelModule bool `json:"missing_kernel_module"`
	InContainer         bool `json:"in_container"`
}

// currentKernelModeStatus is the shared implementation behind both
// handleKernelModeStatus and GetWizardOptions (see wizard.go) - a fresh
// check every call, deliberately not cached: whether the package/module
// is present can change between requests (an admin installing the
// package or running modprobe while the wizard/Admin UI happens to be
// open), and these are cheap os.Stat calls, not worth caching.
func currentKernelModeStatus() KernelModeStatus {
	_, pkgErr := os.Stat(kernelTCMUDPath)
	_, modErr := os.Stat(kernelModulePath)
	_, dockerErr := os.Stat(dockerEnvPath)
	st := KernelModeStatus{
		MissingPackage:      pkgErr != nil,
		MissingKernelModule: modErr != nil,
		InContainer:         dockerErr == nil,
	}
	st.Available = !st.MissingPackage && !st.MissingKernelModule
	return st
}

func (s *Server) handleKernelModeStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, currentKernelModeStatus())
}
