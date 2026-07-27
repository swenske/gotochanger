package api

import (
	"net/http"
	"os"
)

// kernelTCMUDPath/kernelModulePath are fixed Debian packaging conventions
// (see Makefile's install target and systemd/gotochanger-tcmud@.service),
// not admin-configurable - vars rather than consts only so tests can
// point them at a fake path instead of the real filesystem.
var (
	kernelTCMUDPath  = "/usr/sbin/gotochanger-tcmud"
	kernelModulePath = "/sys/module/target_core_user"
)

// KernelModeStatus reports whether this host is ready to run
// gotochanger-tcmud (see cmd/gotochanger-tcmud) - two independent,
// purely filesystem-based checks, nothing privileged attempted: whether
// the gotochanger-kernel package is installed (its binary exists at the
// path Makefile's install target puts it at) and whether the
// target_core_user kernel module is loaded right now. The two are
// deliberately reported separately (not just one combined bool) so the
// wizard/Admin UI can show a specific, actionable hint - "install the
// package" vs. "loaded automatically at next boot, or run modprobe
// target_core_user now" are different instructions for the operator.
type KernelModeStatus struct {
	Available           bool `json:"available"`
	MissingPackage      bool `json:"missing_package"`
	MissingKernelModule bool `json:"missing_kernel_module"`
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
	st := KernelModeStatus{
		MissingPackage:      pkgErr != nil,
		MissingKernelModule: modErr != nil,
	}
	st.Available = !st.MissingPackage && !st.MissingKernelModule
	return st
}

func (s *Server) handleKernelModeStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, currentKernelModeStatus())
}
