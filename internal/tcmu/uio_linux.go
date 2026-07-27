//go:build linux

package tcmu

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// This file is Linux-only with no portable fallback, unlike the inotify/
// polling pair in internal/library/activity_linux.go and activity_other.go
// - UIO and TCMU are Linux kernel facilities with no cross-platform
// equivalent to fall back to (inotify's fallback was a plain stat-poll
// loop, which still made sense on any OS; there's no analogous "poll for a
// SCSI command" primitive on a system with no TCMU at all). Kernel mode is
// simply unavailable on a non-Linux build - see cmd/gotochanger-tcmud,
// which is itself expected to only ever be built/shipped for the Debian
// trixie target this whole project already targets exclusively.

// Device is one open TCMU-backed UIO device: the mmap'd shared-memory
// region (wrapped in a Ring, see ring.go) plus the file descriptor used to
// block for new commands and to notify the kernel once responses are
// ready. Real TCMU devices are only ever accessed this way in production;
// tests exercise Ring directly against a plain []byte instead (see
// ring_test.go), since opening a real /dev/uioN needs root and a loaded
// target_core_user kernel module, neither available in this project's
// build/CI sandbox.
type Device struct {
	fd   int
	mem  []byte
	Ring *Ring
}

// OpenUIODevice opens and mmaps /dev/uio<minor> - the device TCMU creates
// for a backstore once its "enable" configfs attribute is set to 1 and the
// kernel has sent a TCMU_CMD_ADDED_DEVICE netlink event carrying this minor
// number (see netlink.go/ADDED_DEVICE_DONE reply).
func OpenUIODevice(minor uint32) (*Device, error) {
	path := fmt.Sprintf("/dev/uio%d", minor)
	size, err := uioMapSize(minor)
	if err != nil {
		return nil, fmt.Errorf("tcmu: determine mmap size for %s: %w", path, err)
	}

	fd, err := syscall.Open(path, syscall.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("tcmu: open %s: %w", path, err)
	}
	mem, err := syscall.Mmap(fd, 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("tcmu: mmap %s (%d bytes): %w", path, size, err)
	}
	ring, err := NewRing(mem)
	if err != nil {
		_ = syscall.Munmap(mem)
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("tcmu: %s: %w", path, err)
	}
	return &Device{fd: fd, mem: mem, Ring: ring}, nil
}

// uioMapSize reads the size of a UIO device's first (and, for TCMU, only)
// memory mapping from sysfs, e.g. /sys/class/uio/uio3/maps/map0/size
// ("0x100000\n") - this has to be known up front since mmap itself takes an
// explicit length, and there's no way to ask the kernel "how big is this
// device" via the device node alone.
func uioMapSize(minor uint32) (int, error) {
	path := fmt.Sprintf("/sys/class/uio/uio%d/maps/map0/size", minor)
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	s := strings.TrimPrefix(strings.TrimSpace(string(data)), "0x")
	n, err := strconv.ParseUint(s, 16, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s (%q): %w", path, s, err)
	}
	return int(n), nil
}

// WaitForCommand blocks until the kernel has submitted at least one new
// command since the last call (or since open, for the first call) - a
// plain blocking read of the UIO interrupt-count word, per the standard
// UIO userspace protocol (a uio device's read(2) "blocks until an
// interrupt occurs"). The count itself isn't meaningful here: Cursor.Next
// re-checks the mailbox's live cmd_head directly rather than trusting the
// count to say how many commands arrived, so a spurious extra wakeup is
// harmless (Next simply reports "nothing new").
func (d *Device) WaitForCommand() error {
	buf := make([]byte, 4)
	_, err := syscall.Read(d.fd, buf)
	return err
}

// Notify tells the kernel driver that userspace has updated the ring
// (advanced cmd_tail / written responses) - a plain write of the UIO
// interrupt-count word, per the same UIO userspace protocol WaitForCommand
// documents. The value written is not interpreted by the driver; only the
// write() call itself matters.
func (d *Device) Notify() error {
	buf := []byte{1, 0, 0, 0}
	_, err := syscall.Write(d.fd, buf)
	return err
}

// Close unmaps the shared memory and closes the device file descriptor.
func (d *Device) Close() error {
	err := syscall.Munmap(d.mem)
	if cerr := syscall.Close(d.fd); err == nil {
		err = cerr
	}
	return err
}
