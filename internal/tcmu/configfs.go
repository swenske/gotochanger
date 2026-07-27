package tcmu

import (
	"fmt"
	"os"
	"path/filepath"
)

// DefaultConfigFSRoot is where a real system mounts configfs - LIO's target
// subsystem lives in a directory tree under here. Every function in this
// file takes root explicitly instead of hardcoding this constant, so tests
// can point it at a temp directory standing in for a real mount (see
// configfs_test.go) - the same "parameterize the kernel-facing root path"
// approach the plan calls for, since a real /sys/kernel/config isn't
// writable (or even present) in this project's build/CI sandbox.
const DefaultConfigFSRoot = "/sys/kernel/config"

// BackstoreConfig describes one TCMU-backed SCSI logical unit to create -
// the "core" half of LIO's configfs tree, before it's exposed to the host
// via a fabric (see LoopbackConfig below). A backstore alone creates no
// device node; it only becomes a real /dev/sg*//dev/nst* once mapped into
// an enabled loopback target.
type BackstoreConfig struct {
	// HBA is the TCMU "HBA" directory name - confirmed against a real
	// kernel (6.18, target_core_mod) that this must be exactly
	// "user_<N>" for a decimal N; anything else (e.g. a descriptive
	// name like "user_gotochanger") is rejected at mkdir time with
	// EINVAL. The number itself is arbitrary - it isn't a real piece of
	// hardware, just a grouping level LIO's configfs requires.
	HBA       string
	Name      string // device name within the HBA, e.g. "changer0" or "drive0"
	Subtype   string // handler name embedded in dev_config, e.g. "gotochanger" - matched against this project's own netlink listener identity
	CfgString string // handler-specific config string, e.g. a stable per-drive identifier
	SizeBytes int64  // this device's reported size
	BlockSize int    // hw_block_size
}

func backstoreDir(root string, cfg BackstoreConfig) string {
	return filepath.Join(root, "target", "core", cfg.HBA, cfg.Name)
}

// CreateBackstore creates (but does not enable) a TCMU backstore, writing
// the same configfs files a human would via mkdir+echo:
//
//	mkdir target/core/<hba>/<name>
//	echo "dev_config=<subtype>/<cfgstring>,dev_size=<n>,hw_block_size=<n>,nl_reply_supported=1" \
//	    > target/core/<hba>/<name>/control
//
// nl_reply_supported=1 is what makes the kernel wait for our own
// ADDED_DEVICE_DONE netlink reply (see netlink.go) instead of assuming
// success immediately - needed to reliably learn the assigned UIO minor
// number before opening the device.
func CreateBackstore(root string, cfg BackstoreConfig) error {
	dir := backstoreDir(root, cfg)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("tcmu: create backstore dir %s: %w", dir, err)
	}
	control := fmt.Sprintf("dev_config=%s/%s,dev_size=%d,hw_block_size=%d,nl_reply_supported=1",
		cfg.Subtype, cfg.CfgString, cfg.SizeBytes, cfg.BlockSize)
	if err := os.WriteFile(filepath.Join(dir, "control"), []byte(control), 0o644); err != nil {
		return fmt.Errorf("tcmu: write control for %s: %w", dir, err)
	}
	return nil
}

// EnableBackstore flips a created backstore's "enable" attribute - this is
// what actually triggers the kernel to allocate the UIO device and send
// the TCMU_CMD_ADDED_DEVICE netlink event, not CreateBackstore alone
// (mirroring the real two-step configfs sequence: write control, then
// enable, as two separate writes).
//
// Confirmed against a real kernel (6.18): because this project's own
// control string sets nl_reply_supported=1 (see CreateBackstore), this
// write(2) does not return until the kernel receives our
// ADDED_DEVICE_DONE netlink reply back (Listener.AckAddedDevice) - it
// blocks in-kernel, not just slowly completes. Calling this synchronously
// before a caller is listening for (and ready to ack) the ADDED_DEVICE
// event deadlocks forever - matches a real upstream kernel bug report
// describing the same hang from the other direction (a crashing tcmu
// handler that never sends its reply). Callers must run this concurrently
// with, not before, waiting for and acking the event - see
// cmd/gotochanger-tcmud's setupDevice for the pattern (a goroutine plus a
// buffered error channel).
func EnableBackstore(root string, cfg BackstoreConfig) error {
	return os.WriteFile(filepath.Join(backstoreDir(root, cfg), "enable"), []byte("1"), 0o644)
}

// There is deliberately no DisableBackstore. An earlier version of this
// file had one (writing "0" to "enable", mirroring EnableBackstore's own
// write) - confirmed against a real kernel (6.18) that this is rejected
// outright: dmesg logs "For dev_enable ops, only valid value is '1'" and
// the write fails. A TCMU backstore's enable is one-way; the only
// supported teardown is removing the item entirely once nothing
// references it (RemoveBackstore), after that item's own UIO device has
// been closed and any LUN exporting it has been unmapped.

// RemoveBackstore deletes a disabled backstore's configfs directory via a
// single rmdir (os.Remove), deliberately not os.RemoveAll: on real
// configfs, "control"/"enable" and friends are kernel-managed attribute
// pseudo-files that cannot be unlinked individually (only the item/
// directory itself can be removed, which atomically tears down its
// attributes with it) - RemoveAll's readdir-then-unlink-each-file approach
// would fail with EPERM on a real mount before it ever reached the rmdir.
// A plain-directory test fixture standing in for configfs (see
// configfs_test.go) has to pre-clean its fake attribute files itself
// before calling this, since an ordinary filesystem has no equivalent to
// configfs's "rmdir cleans up everything" behavior.
func RemoveBackstore(root string, cfg BackstoreConfig) error {
	return os.Remove(backstoreDir(root, cfg))
}

// LoopbackConfig identifies one loopback fabric target (tcm_loop) exposing
// one or more backstores as real SCSI devices on this same host - what
// makes /dev/sg*//dev/nst* actually appear, distinct from a backstore
// (which only exists inside LIO's configfs until a fabric exposes it).
type LoopbackConfig struct {
	WWN string // the target's own identifier, e.g. a naa.<16 hex digits> string, without the "naa." prefix
}

func loopbackTPGTDir(root string, cfg LoopbackConfig) string {
	return filepath.Join(root, "target", "loopback", "naa."+cfg.WWN, "tpgt_1")
}

// CreateLoopbackTarget creates a loopback fabric target's single target
// portal group (tcm_loop only ever has tpgt_1 - it has no notion of
// multiple portal groups per WWN the way a real multi-port fabric like
// iSCSI does).
func CreateLoopbackTarget(root string, cfg LoopbackConfig) error {
	dir := loopbackTPGTDir(root, cfg)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("tcmu: create loopback target %s: %w", dir, err)
	}
	return nil
}

// CreateLoopbackLUN maps backstoreCfg into loopbackCfg's target as LUN
// number lun, by symlinking the LUN's configfs directory to the backstore
// - the same mechanism targetcli uses under the hood.
func CreateLoopbackLUN(root string, loopbackCfg LoopbackConfig, lun int, backstoreCfg BackstoreConfig) error {
	lunDir := filepath.Join(loopbackTPGTDir(root, loopbackCfg), "lun", fmt.Sprintf("lun_%d", lun))
	if err := os.MkdirAll(lunDir, 0o755); err != nil {
		return fmt.Errorf("tcmu: create lun dir %s: %w", lunDir, err)
	}
	link := filepath.Join(lunDir, backstoreCfg.Name)
	target := backstoreDir(root, backstoreCfg)
	if err := os.Symlink(target, link); err != nil {
		return fmt.Errorf("tcmu: link lun %s to backstore %s: %w", link, target, err)
	}
	return nil
}

// SetLoopbackNexus establishes the local initiator I_T nexus a tcm_loop
// target portal group needs before it will process any command - found
// against a real kernel (6.18) during this project's own real-hardware
// verification pass: unlike other fabrics, tcm_loop's tpgt_1 has no
// "enable" attribute at all (an earlier version of this function wrote
// "1" to one, which fails with EACCES - configfs rejects creating an
// attribute file that doesn't exist). Creating the TPG directory alone
// (CreateLoopbackTarget) already registers a Scsi_Host
// ("scsi hostN: TCM_Loopback" in dmesg), but the kernel logs
// "TCM_Loop I_T Nexus does not exist" and drops every command until a
// nexus is written here. A self-referential nexus (the target's own WWN
// as its own initiator) is the simplest working setup for a single-host
// loopback target like this project's, and is what real-world tcm_loop
// usage does for the same reason. Must be called before CreateLoopbackLUN
// and before the host's SCSI bus is scanned (see cmd/gotochanger-tcmud).
func SetLoopbackNexus(root string, cfg LoopbackConfig) error {
	return os.WriteFile(filepath.Join(loopbackTPGTDir(root, cfg), "nexus"), []byte("naa."+cfg.WWN), 0o644)
}

// ScanSCSIHost triggers a full bus rescan on a SCSI host - confirmed
// necessary during real-hardware verification: even after SetLoopbackNexus
// and CreateLoopbackLUN, no /dev/sg*/nst* node appears until the host is
// explicitly told to scan (the kernel does not appear to do this
// automatically for a loopback target coming up). "- - -" is the standard
// Linux SCSI "scan every channel/target/LUN on this host" request (see
// Documentation/scsi/scsi-parameters, or any real hot-add-a-LUN
// procedure).
func ScanSCSIHost(hostPath string) error {
	return os.WriteFile(filepath.Join(hostPath, "scan"), []byte("- - -"), 0o644)
}

// ClearLoopbackNexus clears a tcm_loop target's I_T nexus (see
// SetLoopbackNexus) - a precondition for RemoveLoopbackTarget, confirmed
// against a real kernel: rmdir on tpgt_1 while a nexus is still set fails
// (dmesg: "Unable to remove TCM_Loop I_T Nexus with active TPG port
// count").
//
// Deliberately does not use os.WriteFile (which always issues a write(2)
// call, even for a zero-length buffer): confirmed against a real kernel
// that an explicit zero-length write(2) to this attribute fails with
// EFAULT ("bad address"), while opening the file O_TRUNC and closing it
// without ever calling write(2) at all - the same thing a shell's
// `echo -n ” > file` redirection does, since echo -n produces no output
// to write - succeeds and genuinely clears the nexus. Whatever this
// attribute's kernel-side handler does with an empty write buffer, it
// doesn't handle a deliberate zero-length one the same way as never
// writing at all.
func ClearLoopbackNexus(root string, cfg LoopbackConfig) error {
	f, err := os.OpenFile(filepath.Join(loopbackTPGTDir(root, cfg), "nexus"), os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

// RemoveLoopbackLUN un-maps a backstore from a loopback target's LUN
// number - the reverse of CreateLoopbackLUN, and (along with
// RemoveLoopbackTarget) a precondition for RemoveBackstore, which fails
// while anything still exports the backstore.
func RemoveLoopbackLUN(root string, loopbackCfg LoopbackConfig, lun int, backstoreCfg BackstoreConfig) error {
	link := filepath.Join(loopbackTPGTDir(root, loopbackCfg), "lun", fmt.Sprintf("lun_%d", lun), backstoreCfg.Name)
	if err := os.Remove(link); err != nil {
		return fmt.Errorf("tcmu: remove lun link %s: %w", link, err)
	}
	lunDir := filepath.Join(loopbackTPGTDir(root, loopbackCfg), "lun", fmt.Sprintf("lun_%d", lun))
	if err := os.Remove(lunDir); err != nil {
		return fmt.Errorf("tcmu: remove lun dir %s: %w", lunDir, err)
	}
	return nil
}

// RemoveLoopbackTarget clears the nexus (see ClearLoopbackNexus) and
// removes a loopback target's tpgt_1 directory and its own naa.<wwn>
// directory - the reverse of SetLoopbackNexus+CreateLoopbackTarget.
// RemoveLoopbackLUN must be called first (a TPG can't be removed while it
// still exports a LUN).
func RemoveLoopbackTarget(root string, cfg LoopbackConfig) error {
	if err := ClearLoopbackNexus(root, cfg); err != nil {
		return fmt.Errorf("tcmu: clear nexus: %w", err)
	}
	dir := loopbackTPGTDir(root, cfg)
	if err := os.Remove(dir); err != nil {
		return fmt.Errorf("tcmu: remove %s: %w", dir, err)
	}
	parent := filepath.Dir(dir)
	if err := os.Remove(parent); err != nil {
		return fmt.Errorf("tcmu: remove %s: %w", parent, err)
	}
	return nil
}

// sysClassSCSIHost is /sys/class/scsi_host, where every registered SCSI
// host (real or virtual, including each tcm_loop target) shows up as
// hostN. Not rooted at configfsRoot - it's a different sysfs mount, always
// at this fixed path on a real system - callers that need a fake root for
// testing should not call ListSCSIHosts/ScanSCSIHost at all, only the
// pure configfs functions in this file.
const sysClassSCSIHost = "/sys/class/scsi_host"

// ListSCSIHosts returns every currently registered SCSI host's name
// (e.g. "host1"). Used with a before/after snapshot around
// CreateLoopbackTarget to identify the Scsi_Host that call just created -
// found during real-hardware verification that configfs itself exposes no
// direct "which scsi_host is this tcm_loop target" link, so a diff against
// this list is the only way to find it.
func ListSCSIHosts() ([]string, error) {
	entries, err := os.ReadDir(sysClassSCSIHost)
	if err != nil {
		return nil, fmt.Errorf("tcmu: list %s: %w", sysClassSCSIHost, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}

// NewSCSIHost returns the one entry in after that wasn't in before (see
// ListSCSIHosts), and its full /sys/class/scsi_host/hostN path.
func NewSCSIHost(before, after []string) (name, path string, ok bool) {
	seen := make(map[string]bool, len(before))
	for _, h := range before {
		seen[h] = true
	}
	for _, h := range after {
		if !seen[h] {
			return h, filepath.Join(sysClassSCSIHost, h), true
		}
	}
	return "", "", false
}
