//go:build linux

package tcmu

import (
	"fmt"
	"syscall"
)

// This file is Linux-only for the same reason uio_linux.go is (see its
// doc comment) - generic netlink is a Linux kernel facility with no
// portable equivalent. It supplies real sockets/bytes to
// netlink_message.go's pure encode/decode logic, mirroring this package's
// existing wire-format-vs-syscalls split.

// solNetlink is SOL_NETLINK (linux/socket.h) - not exported by the
// syscall package, unlike most SOL_*/IPPROTO_* levels, so it's defined
// here directly. Stable across every architecture Linux runs on.
const solNetlink = 270

// tcmuGenlName is the generic-netlink family name the kernel's
// target_core_user driver registers (see protocol.go's doc comment for
// where the rest of the TCMU ABI was verified).
const tcmuGenlName = "TCM-USER"

// tcmuMCastGroup is the one multicast group the TCM-USER family publishes
// (device lifecycle events - ADDED_DEVICE/REMOVED_DEVICE/RECONFIG_DEVICE).
const tcmuMCastGroup = "config"

// TCM-USER's own generic-netlink commands and attributes
// (enum tcmu_genl_cmd / enum tcmu_genl_attr), verified against the kernel
// UAPI header alongside the shared-memory ABI in protocol.go.
const (
	tcmuCmdAddedDevice        = 1
	tcmuCmdRemovedDevice      = 2
	tcmuCmdReconfigDevice     = 3
	tcmuCmdAddedDeviceDone    = 4
	tcmuCmdRemovedDeviceDone  = 5
	tcmuCmdReconfigDeviceDone = 6
	tcmuCmdSetFeatures        = 7
	tcmuAttrDevice            = 1 // NLA_STRING: the configfs device name, e.g. "changer0"
	tcmuAttrMinor             = 2 // NLA_U32: the UIO minor number - what OpenUIODevice needs
	tcmuAttrDeviceID          = 8 // NLA_U32: echoed back in our *_DONE reply
	tcmuAttrCmdStatus         = 7 // NLA_S32: 0 for success, negative errno otherwise, in our *_DONE reply
	tcmuAttrSuppKernCmdReply  = 9 // NLA_U8
)

func openGenlSocket() (int, error) {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW, syscall.NETLINK_GENERIC)
	if err != nil {
		return 0, fmt.Errorf("tcmu: open netlink socket: %w", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		_ = syscall.Close(fd)
		return 0, fmt.Errorf("tcmu: bind netlink socket: %w", err)
	}
	return fd, nil
}

// kernelAddr addresses the kernel itself (pid 0), the destination for
// every request/reply this package sends - it never talks to another
// userspace netlink socket.
var kernelAddr = &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}

// resolveGenlFamily asks the kernel's generic-netlink controller (a fixed,
// always-present family) to resolve name to its dynamically-assigned
// family id and multicast-group name->id map.
func resolveGenlFamily(fd int, name string, seq uint32) (familyID uint16, mcastGroups map[string]uint32, err error) {
	req := buildGetFamilyRequest(name, seq, 0)
	if err := syscall.Sendto(fd, req, 0, kernelAddr); err != nil {
		return 0, nil, fmt.Errorf("tcmu: send GETFAMILY: %w", err)
	}

	buf := make([]byte, 32*1024)
	n, _, err := syscall.Recvfrom(fd, buf, 0)
	if err != nil {
		return 0, nil, fmt.Errorf("tcmu: receive GETFAMILY reply: %w", err)
	}
	msg, _, err := parseMessage(buf[:n])
	if err != nil {
		return 0, nil, fmt.Errorf("tcmu: parse GETFAMILY reply: %w", err)
	}
	if msg.Header.Type == nlmsgError {
		if msg.Errno == 0 {
			return 0, nil, fmt.Errorf("tcmu: GETFAMILY(%s): unexpected ACK with no family info", name)
		}
		return 0, nil, fmt.Errorf("tcmu: GETFAMILY(%s): %w", name, syscall.Errno(-msg.Errno))
	}
	return parseFamilyReply(msg)
}

// Listener receives TCM-USER device lifecycle events (ADDED_DEVICE/
// REMOVED_DEVICE/RECONFIG_DEVICE) and lets the caller acknowledge them
// (required when the backstore was created with nl_reply_supported=1, see
// configfs.go's CreateBackstore) - one per gotochanger-tcmud process, not
// per device: a single multicast group carries every TCMU device's events
// system-wide, and ReadEvent's TCMU_ATTR_DEVICE attribute is how a caller
// tells which configfs device an event is about.
type Listener struct {
	fd       int
	familyID uint16
	seq      uint32
}

// Listen opens a generic-netlink socket, resolves the TCM-USER family, and
// joins its "config" multicast group.
func Listen() (*Listener, error) {
	fd, err := openGenlSocket()
	if err != nil {
		return nil, err
	}
	familyID, groups, err := resolveGenlFamily(fd, tcmuGenlName, 1)
	if err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("tcmu: resolve %s family (is target_core_user loaded?): %w", tcmuGenlName, err)
	}
	groupID, ok := groups[tcmuMCastGroup]
	if !ok {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("tcmu: %s family has no %q multicast group", tcmuGenlName, tcmuMCastGroup)
	}
	if err := syscall.SetsockoptInt(fd, solNetlink, syscall.NETLINK_ADD_MEMBERSHIP, int(groupID)); err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("tcmu: join %q multicast group: %w", tcmuMCastGroup, err)
	}
	return &Listener{fd: fd, familyID: familyID, seq: 2}, nil
}

// Device lifecycle event kinds - exported so callers outside this package
// can compare against DeviceEvent.Cmd without needing the underlying
// TCM-USER genl command numbers themselves.
const (
	DeviceEventAdded        = tcmuCmdAddedDevice
	DeviceEventRemoved      = tcmuCmdRemovedDevice
	DeviceEventReconfigured = tcmuCmdReconfigDevice
)

// DeviceEvent is one decoded ADDED_DEVICE/REMOVED_DEVICE/RECONFIG_DEVICE
// notification.
type DeviceEvent struct {
	Cmd      uint8
	Name     string // TCMU_ATTR_DEVICE: the configfs device name - exact string format (e.g. "changer0" vs "user_gotochanger/changer0") still needs confirming against a real kernel event, see cmd/gotochanger-tcmud's matching logic
	Minor    uint32 // TCMU_ATTR_MINOR: valid for ADDED_DEVICE, identifies /dev/uio<Minor>
	DeviceID uint32 // TCMU_ATTR_DEVICE_ID: echo this back in AckAddedDevice
}

// ReadEvent blocks for the next device lifecycle event.
func (l *Listener) ReadEvent() (DeviceEvent, error) {
	buf := make([]byte, 32*1024)
	for {
		n, _, err := syscall.Recvfrom(l.fd, buf, 0)
		if err != nil {
			return DeviceEvent{}, fmt.Errorf("tcmu: receive event: %w", err)
		}
		data := buf[:n]
		for len(data) > 0 {
			msg, consumed, err := parseMessage(data)
			if err != nil {
				return DeviceEvent{}, fmt.Errorf("tcmu: parse event: %w", err)
			}
			if consumed == 0 {
				break
			}
			data = data[consumed:]
			if msg.Header.Type != l.familyID {
				continue // not TCM-USER traffic - ignore (this socket should only ever see it, but be defensive)
			}
			switch msg.GenlCmd {
			case tcmuCmdAddedDevice, tcmuCmdRemovedDevice, tcmuCmdReconfigDevice:
				ev := DeviceEvent{Cmd: msg.GenlCmd}
				for _, a := range msg.Attrs {
					switch a.Type {
					case tcmuAttrDevice:
						ev.Name = a.String()
					case tcmuAttrMinor:
						ev.Minor, _ = a.Uint32()
					case tcmuAttrDeviceID:
						ev.DeviceID, _ = a.Uint32()
					}
				}
				return ev, nil
			}
		}
	}
}

// AckAddedDevice replies to an ADDED_DEVICE event with ADDED_DEVICE_DONE,
// status 0 (success) - required for the kernel to finish bringing up a
// backstore created with nl_reply_supported=1 (see CreateBackstore).
func (l *Listener) AckAddedDevice(deviceID uint32) error {
	l.seq++
	attrs := []Attr{attrU32(tcmuAttrDeviceID, deviceID), attrS32(tcmuAttrCmdStatus, 0)}
	msg := buildMessage(l.familyID, nlmFRequest, l.seq, 0, tcmuCmdAddedDeviceDone, 2, attrs)
	if err := syscall.Sendto(l.fd, msg, 0, kernelAddr); err != nil {
		return fmt.Errorf("tcmu: send ADDED_DEVICE_DONE: %w", err)
	}
	return nil
}

// Close closes the underlying netlink socket.
func (l *Listener) Close() error {
	return syscall.Close(l.fd)
}
