package tcmu

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// sysClassSCSIDevice is /sys/class/scsi_device, where every SCSI logical
// unit (real or virtual) shows up as "H:B:T:L" (host:bus:target:lun).
// Not rooted at configfsRoot, same reasoning as sysClassSCSIHost.
const sysClassSCSIDevice = "/sys/class/scsi_device"

// DevicePaths is the real kernel-assigned device node(s) for one
// TCMU-backed LUN, found by DiscoverDevicePaths after ScanSCSIHost has
// already run.
type DevicePaths struct {
	Generic string // e.g. "/dev/sg4" - the SCSI generic passthrough device, always present once discovery succeeds
	Tape    string // e.g. "/dev/nst0" - the non-rewinding tape device, only present when the kernel's "st" driver is bound to this LU (see this function's doc comment)

	// StableGeneric/StableTape are the /dev/tape/by-id/... symlink paths
	// udev creates from this device's own VPD page 0x83 identity (see
	// internal/scsi/vpd.go, and Changer/Drive.NAA) - populated by
	// DiscoverStablePaths, separately and after DiscoverDevicePaths,
	// since they depend on udev having already processed the device (not
	// guaranteed to have happened yet the moment the sysfs symlinks
	// DiscoverDevicePaths reads appear). Empty when no matching by-id
	// symlink has shown up yet, or - for StableGeneric specifically - for
	// most drives, since the stock 60-persistent-storage-tape.rules only
	// creates a scsi_generic by-id symlink for a medium changer
	// (ATTRS{type}=="8"), not a sequential-access device; a drive's
	// stable identity is StableTape, not StableGeneric.
	StableGeneric string
	StableTape    string
}

// DiscoverDevicePaths finds the real device node(s) the kernel assigned
// to hostPath's (e.g. "/sys/class/scsi_host/host1") single LUN, after
// ScanSCSIHost has already triggered discovery. This project's loopback
// fabric always uses channel 0, target 1, LUN 0 for every device
// (confirmed against a real kernel repeatedly across this project's
// real-hardware verification sessions - every device consistently shows
// up as host:0:1:0 in lsscsi), so the SCSI device directory is always
// /sys/class/scsi_device/<hostN>:0:1:0/.
//
// The "generic" (/dev/sg*) half is real-hardware-verified: reading
// device/generic's symlink target and taking its basename ("sg4") is
// exactly how real tools like lsscsi resolve this, confirmed against
// this project's own real devices during Milestone 3 follow-up work.
//
// The "tape" (/dev/nst*) half is NOT verified against a real kernel -
// every host this project has been verified on has no "st" driver built
// (see CLAUDE.md's "Kernel mode (TCMU/LIO)" real-hardware-verification
// notes), so device/tape's existence and its "stN" naming convention is
// inferred from the same "device/<class-shortcut>" pattern
// device/generic already demonstrably follows, not confirmed firsthand.
// Best-effort: absence of device/tape is not an error, just leaves
// Tape empty - callers must treat a populated Generic with an empty Tape
// as the expected common case, not a partial failure.
func DiscoverDevicePaths(hostPath string) (DevicePaths, error) {
	hostNum := strings.TrimPrefix(filepath.Base(hostPath), "host")
	scsiDevDir := filepath.Join(sysClassSCSIDevice, hostNum+":0:1:0", "device")

	var out DevicePaths
	genericLink, err := os.Readlink(filepath.Join(scsiDevDir, "generic"))
	if err != nil {
		return out, fmt.Errorf("tcmu: read generic device link under %s: %w", scsiDevDir, err)
	}
	out.Generic = "/dev/" + filepath.Base(genericLink)

	if tapeLink, err := os.Readlink(filepath.Join(scsiDevDir, "tape")); err == nil {
		// The rewind-on-close device (e.g. "st0") is what device/tape
		// points at; Bareos and most backup software conventionally want
		// the non-rewinding variant instead ("n" + the same name), per
		// the standard Linux st driver naming convention.
		out.Tape = "/dev/n" + filepath.Base(tapeLink)
	}
	return out, nil
}

// changerSuffix is the stock 60-persistent-storage-tape.rules' own suffix
// for a medium changer's second by-id symlink ("scsi-$ID_SERIAL-changer",
// alongside a bare "scsi-$ID_SERIAL" pointing at the same device) - see
// DiscoverStablePaths' doc comment for why this one is preferred.
const changerSuffix = "-changer"

// DiscoverStablePaths fills in paths.StableGeneric/StableTape by scanning
// byIDDir (normally "/dev/tape/by-id", the directory the stock
// 60-persistent-storage-tape.rules udev rules populate - see
// internal/scsi/vpd.go's doc comment for the full mechanism) for a
// symlink whose real target resolves to paths.Generic or paths.Tape.
// Matching by resolved target, not by reconstructing udev's own
// "scsi-3<hex>[-nst]" filename convention by hand, so this stays correct
// even if that naming detail ever changes.
//
// A medium changer gets exactly two by-id symlinks pointing at the same
// target - a bare "scsi-$ID_SERIAL" and a "scsi-$ID_SERIAL-changer" (see
// the rule file itself) - and only the changer, since a drive's own
// generic device never gets a by-id symlink at all (see DevicePaths'
// doc comment). Between those two, the "-changer"-suffixed one is
// preferred for StableGeneric: it's the self-documenting name (matches
// what a human browsing /dev/tape/by-id/ or reading a Bareos
// "Changer Device" line would expect - requested directly by the user
// after seeing the bare name in a real generated config), not just
// whichever one os.ReadDir happens to list first (which, being
// alphabetically shorter, would otherwise always be the bare name - a
// name-length coincidence, not a meaningful choice). A drive's Tape
// target only ever gets one matching symlink in practice, so this
// preference is a no-op there.
//
// Returns paths unchanged (not an error) when byIDDir doesn't exist yet -
// this is the expected state before udev has processed the device, not a
// failure; callers retry (see cmd/gotochanger-tcmud's setupDevice).
func DiscoverStablePaths(paths DevicePaths, byIDDir string) DevicePaths {
	entries, err := os.ReadDir(byIDDir)
	if err != nil {
		return paths
	}
	var genericPreferred, tapePreferred bool
	for _, entry := range entries {
		linkPath := filepath.Join(byIDDir, entry.Name())
		target, err := filepath.EvalSymlinks(linkPath)
		if err != nil {
			continue
		}
		preferred := strings.HasSuffix(entry.Name(), changerSuffix)
		switch target {
		case paths.Generic:
			if paths.StableGeneric == "" || (preferred && !genericPreferred) {
				paths.StableGeneric = linkPath
				genericPreferred = preferred
			}
		case paths.Tape:
			if paths.StableTape == "" || (preferred && !tapePreferred) {
				paths.StableTape = linkPath
				tapePreferred = preferred
			}
		}
	}
	return paths
}
