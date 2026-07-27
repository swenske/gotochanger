package tcmu

import (
	"fmt"
	"os"
	"syscall"
)

// HostDiscoveryLockPath serializes the scsi-host-discovery critical
// section (see AcquireHostDiscoveryLock) across every gotochanger-tcmud
// instance on this host. Lives under the same /run/gotochanger directory
// every instance already depends on for the trusted socket
// (defaultSocketPath in cmd/gotochanger-tcmud/main.go), so if that
// directory doesn't exist, gotochanger-tcmud can't function at all
// regardless of this lock.
const HostDiscoveryLockPath = "/run/gotochanger/tcmud-hostscan.lock"

// hostDiscoveryLockPath is what AcquireHostDiscoveryLock actually opens -
// a variable, not the constant directly, purely so hostlock_test.go can
// point it at a throwaway temp file instead of flock-ing the real,
// possibly-in-use system path during `go test`.
var hostDiscoveryLockPath = HostDiscoveryLockPath

// AcquireHostDiscoveryLock takes an exclusive, blocking flock on
// HostDiscoveryLockPath - callers must hold it for the entire
// ListSCSIHosts-before / CreateLoopbackTarget / ListSCSIHosts-after /
// NewSCSIHost sequence in cmd/gotochanger-tcmud's setupDevice, and release
// it (via the returned func) as soon as NewSCSIHost has returned.
//
// Found necessary for real, not defensively: this whole sequence
// identifies "which scsi_host is the one we just created" by diffing
// /sys/class/scsi_host's contents before and after - global system state,
// not scoped to this process. One gotochanger-tcmud instance alone never
// notices, since its own setupDevice calls for its own changer/drives run
// sequentially, one at a time. But a real deployment normally runs one
// instance per logical library concurrently (separate OS processes,
// started together by gotochangerd's reconciler or by systemd at boot),
// and their host-discovery windows can overlap: if instance A's "after"
// snapshot happens after instance B has also registered its own new host,
// NewSCSIHost's before/after diff can hand instance A instance B's host
// (or vice versa) instead of its own - confirmed reproducing on a real
// kernel by restarting two instances at the same moment, which produced
// two different backstores both reporting the same /dev/sgN. Serializing
// this section with a cross-process lock closes the window: whichever
// instance holds the lock is guaranteed to be the only one whose
// CreateLoopbackTarget call can make a new host appear during that
// window, so its own before/after diff can never be ambiguous.
//
// Deliberately does not extend to SetLoopbackNexus/CreateLoopbackLUN/
// ScanSCSIHost - those only ever act on the caller's own already-uniquely-
// identified loopback target/host path, with no shared global state left
// to race on, so holding the lock any longer would only add unnecessary
// contention between instances setting up unrelated devices.
func AcquireHostDiscoveryLock() (release func() error, err error) {
	f, err := os.OpenFile(hostDiscoveryLockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("tcmu: open host discovery lock %s: %w", hostDiscoveryLockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("tcmu: flock host discovery lock: %w", err)
	}
	return func() error {
		unlockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		closeErr := f.Close()
		if unlockErr != nil {
			return unlockErr
		}
		return closeErr
	}, nil
}
