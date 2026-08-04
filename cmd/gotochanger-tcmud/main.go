// Command gotochanger-tcmud is the TCMU/LIO-backed kernel-mode backend for
// gotochanger: it exposes real SCSI medium-changer and tape-drive devices
// (/dev/sg*, /dev/nst*) to the kernel, translating real SCSI CDBs into
// calls against the same gotochangerd REST API cmd/gotochanger-changer
// uses. See internal/tcmu (the TCMU protocol: configfs/UIO/netlink) and
// internal/scsi (the SMC-3/SSC-3 command handlers) for the layers this
// binary wires together, and the project's kernel-mode plan for the
// overall architecture and why this exists as a separate binary/package
// rather than folded into gotochangerd.
//
// One changer LUN plus one LUN per configured drive, speaking the SMC-3/
// SSC-3 command subset internal/scsi implements (see its own doc
// comment). Requires root and a loaded target_core_user kernel module.
//
// By default this exposes the entire physical library, unscoped - the
// same shape gotochanger-changer has with no --logical-library flag. Pass
// --logical-library to scope it to one logical library's own elements
// instead (its own changer LUN + only that library's drives), the kernel-
// mode equivalent of one Autochanger-per-logical-library in changer-
// script mode: run one gotochanger-tcmud instance per logical library, in
// which case Library.LogicalLibraryStatus (called transparently by every
// apiclient.Client method once SetLogicalLibrary is set) does the actual
// scoping - internal/scsi's handlers are unaware of the distinction, they
// only ever see whatever Status they're given.
//
// Devices are set up sequentially, one at a time (create backstore,
// enable it, wait for that specific device's ADDED_DEVICE netlink event,
// open its UIO device, expose it via a loopback fabric target) - this
// keeps the netlink event-matching logic simple (no concurrent dispatch
// needed, since only one backstore is ever "pending" its ADDED_DEVICE
// event at a time), at the cost of slightly slower startup than setting
// every device up in parallel. Fine for the handful of devices a single
// physical library has.
//
// Real-hardware-verified (see CLAUDE.md's "Kernel mode (TCMU/LIO)"
// section): the full backstore->UIO->loopback->/dev/sg* chain, the exact
// TCMU_ATTR_DEVICE string format matchesDevice compares against, and the
// loopback target's configfs attribute set have all been confirmed
// against a real kernel, not just unit-tested.
package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"hash/fnv"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/swenske/gotochanger/internal/apiclient"
	"github.com/swenske/gotochanger/internal/config"
	"github.com/swenske/gotochanger/internal/scsi"
	"github.com/swenske/gotochanger/internal/tcmu"
)

func main() {
	socketPath := flag.String("socket", defaultSocketPath(), "gotochangerd trusted Unix socket")
	configfsRoot := flag.String("configfs-root", tcmu.DefaultConfigFSRoot, "configfs mount point (override only for testing against a fake root)")
	// The kernel's target_core_mod parses everything after "user_" as a
	// plain decimal HBA number and rejects anything else with EINVAL
	// (mkdir: Invalid argument) - found by hand against a real kernel
	// (this project's original default, "user_gotochanger", doesn't
	// work). "user_1" is arbitrary; only its numeric form matters.
	hba := flag.String("hba", "user_1", "TCMU HBA name to create backstores under (must be \"user_<N>\" - the kernel rejects any other suffix)")
	logicalLibrary := flag.String("logical-library", "", "scope this instance to one logical library's own elements (empty = the whole physical library, unscoped) - mirrors gotochanger-changer's own --logical-library flag")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := run(*socketPath, *configfsRoot, *hba, *logicalLibrary, log); err != nil {
		log.Error("gotochanger-tcmud exited with error", "error", err)
		os.Exit(1)
	}
}

func defaultSocketPath() string {
	if p := os.Getenv("GOTOCHANGER_SOCKET"); p != "" {
		return p
	}
	return "/run/gotochanger/gotochanger.sock"
}

// managedDevice bundles one live TCMU device with the configfs identity
// used to set it up (so shutdown can tear the loopback target/backstore
// back down in the right order).
type managedDevice struct {
	name        string
	backstore   tcmu.BackstoreConfig
	loopback    tcmu.LoopbackConfig
	dev         *tcmu.Device
	hostPath    string           // /sys/class/scsi_host/hostN for this device's loopback target - see scanDevice
	devicePaths tcmu.DevicePaths // real kernel-assigned /dev/sg*(/dev/nst*) node(s) - see reportDevicePaths
	driveIndex  *int             // this drive's real physical index (gotochangerd's own Drive.Index), nil for the changer - see reportDevicePaths
}

func run(socketPath, configfsRoot, hba, logicalLibrary string, log *slog.Logger) error {
	client := apiclient.NewUnix(socketPath)
	if logicalLibrary != "" {
		client.SetLogicalLibrary(logicalLibrary)
	}
	st, err := client.Status()
	if err != nil {
		return fmt.Errorf("connect to gotochangerd: %w", err)
	}

	// FamilyFor is keyed by a drive type's Generation (e.g. "LTO-9"), not
	// its catalog name - library.Drive.DriveType only ever carries the
	// name (the link is by name, see config.DriveDeviceConfig's doc
	// comment), so the actual DriveType row has to be looked up to find
	// its Generation. This matters once a deployment has a custom-named
	// drive type (e.g. "LTO-9-Fast" with Generation "LTO-9") - passing the
	// name straight through only ever worked by coincidence against the
	// stock catalog, where every name already equals its own generation.
	driveTypes, err := client.ListDriveTypes()
	if err != nil {
		return fmt.Errorf("list drive types: %w", err)
	}
	generationByName := make(map[string]string, len(driveTypes))
	realisticByName := make(map[string]bool, len(driveTypes))
	for _, dt := range driveTypes {
		generationByName[dt.Name] = dt.Generation
		realisticByName[dt.Name] = dt.SCSIIdentity == config.SCSIIdentityRealistic
	}

	// changerIdentity (Milestone 5): resolved from this instance's own
	// logical library's ChangerModel, when scoped to one at all - an
	// unscoped instance (logicalLibrary == "") has no LogicalLibraryConfig
	// row to read this from (see config.LogicalLibraryConfig.ChangerModel's
	// own doc comment on why that's an accepted, documented scope limit
	// rather than an oversight), so it always reports
	// scsi.DefaultChangerIdentity.
	changerIdentity := scsi.Identity{}
	if logicalLibrary != "" {
		libs, err := client.ListLogicalLibraries()
		if err != nil {
			return fmt.Errorf("list logical libraries: %w", err)
		}
		for _, lib := range libs {
			if lib.Name == logicalLibrary && lib.ChangerModel == config.ChangerModelRealistic {
				changerIdentity = scsi.RealisticChangerIdentity
				break
			}
		}
	}

	listener, err := tcmu.Listen()
	if err != nil {
		return fmt.Errorf("listen for TCMU device events (is target_core_user loaded?): %w", err)
	}
	defer listener.Close()

	var managed []*managedDevice
	teardown := func() {
		for i := len(managed) - 1; i >= 0; i-- {
			m := managed[i]
			if m.dev != nil {
				_ = m.dev.Close()
			}
			// Order matters and was confirmed against a real kernel: a
			// LUN must be unmapped before its target portal group can be
			// removed, and a backstore can't be removed while any LUN
			// still exports it (see RemoveLoopbackLUN/RemoveLoopbackTarget/
			// RemoveBackstore's own doc comments).
			if err := tcmu.RemoveLoopbackLUN(configfsRoot, m.loopback, 0, m.backstore); err != nil {
				log.Warn("remove loopback lun failed", "device", m.name, "error", err)
			}
			if err := tcmu.RemoveLoopbackTarget(configfsRoot, m.loopback); err != nil {
				log.Warn("remove loopback target failed", "device", m.name, "error", err)
			}
			if err := tcmu.RemoveBackstore(configfsRoot, m.backstore); err != nil {
				log.Warn("remove backstore failed", "device", m.name, "error", err)
			}
		}
	}

	var wg sync.WaitGroup

	// Every backstore name below is prefixed with this instance's own
	// identity (its logical library's name, or "default") - found the
	// hard way that it must be: two gotochanger-tcmud instances (one per
	// logical library) share the same HBA (both default to "user_1"),
	// and a bare "changer0"/"drive0"/"drive1" collides directly between
	// them - a real kernel confirms this outright ("write .../user_1/
	// changer0/enable: file exists"), not just a cosmetic name clash: the
	// second instance's backstore creation silently reuses the first
	// instance's already-registered kernel object (see "se_dev->
	// se_dev_ptr already set for storage object" in dmesg), so it can
	// never actually enable its own device and retries forever via
	// Restart=on-failure. A single HBA hosting multiple *uniquely named*
	// backstores together is fine (this is exactly how one instance's own
	// changer+multiple drives already coexist under it) - the fix is
	// purely about the name, not needing a separate HBA per instance.
	instance := kernelModeInstanceName(logicalLibrary)

	changerName := instance + "-changer0"
	changerHandler := (&scsi.Changer{Client: client, NAA: vpdIdentifier(changerName), Identity: changerIdentity}).Handle
	changerDev, err := setupDevice(configfsRoot, hba, changerName, listener, changerHandler, &wg, log)
	if err != nil {
		return fmt.Errorf("set up changer device: %w", err)
	}
	managed = append(managed, changerDev)

	for i, d := range st.Drives {
		// d.Index (gotochangerd's own physical drive index), not the loop
		// position i: they only coincide when st.Drives is the full,
		// unscoped, gap-free physical drive list - a --logical-library
		// scope can hold a non-contiguous subset (e.g. physical drives 1
		// and 3), where loop position and physical index diverge. Every
		// Library.Load/Unload call this Index is eventually used for
		// (via internal/scsi.Drive) addresses drives by their real
		// physical index regardless of scoping, so passing the loop
		// position here was a latent bug for exactly that
		// non-contiguous-scope case - not previously caught because no
		// deployment exercised it. Using it in the backstore name too
		// (not just scsi.Drive.Index) is also what keeps two
		// logical libraries' drive names from colliding even when both
		// happen to hold a drive at the same loop position.
		physicalIndex := d.Index
		driveName := fmt.Sprintf("%s-drive%d", instance, physicalIndex)
		fam := scsi.FamilyFor(generationByName[d.DriveType])
		if realisticByName[d.DriveType] && fam.RealisticIdentity != (scsi.Identity{}) {
			fam.Identity = fam.RealisticIdentity
		}
		drv := &scsi.Drive{Client: client, Index: physicalIndex, Family: fam, NAA: vpdIdentifier(driveName)}
		driveDev, err := setupDevice(configfsRoot, hba, driveName, listener, drv.Handle, &wg, log)
		if err != nil {
			teardown()
			return fmt.Errorf("set up drive %d device: %w", i, err)
		}
		driveDev.driveIndex = &physicalIndex
		managed = append(managed, driveDev)
	}

	reportDevicePaths(client, logicalLibrary, managed, log)

	log.Info("gotochanger-tcmud ready", "drives", len(st.Drives), "logical_library", logicalLibrary)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	log.Info("shutting down")
	clearDevicePaths(client, logicalLibrary, log)
	teardown()
	wg.Wait()
	return nil
}

// kernelModeInstanceName is the reporting/systemd-instance name for a
// gotochanger-tcmud invocation: the logical library's own name, or
// "default" for the whole-physical-library, unscoped case - matching
// gotochanger-tcmud@default.service's own convention (see
// systemd/gotochanger-tcmud@.service and internal/api's
// kernelModeDefaultInstance, which this must stay in sync with).
func kernelModeInstanceName(logicalLibrary string) string {
	if logicalLibrary == "" {
		return "default"
	}
	return logicalLibrary
}

// reportDevicePaths tells gotochangerd the real device paths discovered
// for each managed device (see setupDevice/tcmu.DiscoverDevicePaths), for
// the Admin UI/Bareos Config generator to display - best-effort, exactly
// like the discovery itself: a device whose path couldn't be discovered
// (paths.Generic == "") is simply left out of the report rather than
// failing the whole call, and a failure to reach gotochangerd here must
// never take down an otherwise fully working set of devices.
func reportDevicePaths(client *apiclient.Client, logicalLibrary string, managed []*managedDevice, log *slog.Logger) {
	report := apiclient.KernelModeDeviceReportInfo{Drives: map[int]apiclient.KernelModeDrivePathsInfo{}}
	for _, m := range managed {
		if m.devicePaths.Generic == "" {
			continue
		}
		paths := apiclient.KernelModeDrivePathsInfo{
			Generic:       m.devicePaths.Generic,
			Tape:          m.devicePaths.Tape,
			StableGeneric: m.devicePaths.StableGeneric,
			StableTape:    m.devicePaths.StableTape,
		}
		if m.driveIndex == nil {
			report.Changer = paths.Generic
			report.ChangerStable = paths.StableGeneric
			continue
		}
		report.Drives[*m.driveIndex] = paths
	}
	instance := kernelModeInstanceName(logicalLibrary)
	if err := client.ReportKernelModeDevices(instance, report); err != nil {
		log.Warn("report device paths to gotochangerd failed (Admin UI/Bareos Config will show a placeholder instead)", "instance", instance, "error", err)
	}
}

// clearDevicePaths removes this instance's previously-reported device
// paths on a clean shutdown - see api.handleClearKernelModeDevices' doc
// comment for why an unclean shutdown can't do this.
func clearDevicePaths(client *apiclient.Client, logicalLibrary string, log *slog.Logger) {
	instance := kernelModeInstanceName(logicalLibrary)
	if err := client.ClearKernelModeDevices(instance); err != nil {
		log.Warn("clear reported device paths failed", "instance", instance, "error", err)
	}
}

// setupDevice creates one TCMU backstore, waits for its ADDED_DEVICE
// event, opens the resulting UIO device, starts servicing its ring buffer,
// and only then exposes it via a loopback fabric target so a real
// /dev/sg*//dev/nst* appears. The exact sequence below (ring service
// started before any loopback/LUN/scan step, nexus before LUN, no
// "enable" write for the loopback target) was worked out against a real
// kernel, not assumed - see tcmu.SetLoopbackNexus/ScanSCSIHost's own doc
// comments, and the comment above the serviceDevice call below, for what
// didn't work first. handler answers this device's SCSI commands (see
// internal/scsi.Changer.Handle/Drive.Handle); wg is the caller's shared
// WaitGroup, so the caller can wait for every device's service loop to
// exit during shutdown regardless of which setupDevice call started it.
// byIDTapeDir is where the stock systemd/udev package's own
// 60-persistent-storage-tape.rules creates stable "scsi-3<hex>[-nst]"
// symlinks for a device once it answers INQUIRY EVPD page 0x83 - see
// internal/scsi/vpd.go's doc comment for the full mechanism. Not
// configurable via a flag: this is a fixed convention of that shipped
// udev rule file, not something this project controls.
const byIDTapeDir = "/dev/tape/by-id"

func setupDevice(configfsRoot, hba, name string, listener *tcmu.Listener, handler func(tcmu.Entry) tcmu.Response, wg *sync.WaitGroup, log *slog.Logger) (md *managedDevice, err error) {
	backstore := tcmu.BackstoreConfig{HBA: hba, Name: name, Subtype: "gotochanger", CfgString: name, SizeBytes: 0, BlockSize: 512}
	if err := tcmu.CreateBackstore(configfsRoot, backstore); err != nil {
		return nil, fmt.Errorf("create backstore: %w", err)
	}
	log.Info("backstore created", "device", name)

	// EnableBackstore's write(2) to "enable" does not return until the
	// kernel gets our ADDED_DEVICE_DONE netlink reply back (our own
	// control string sets nl_reply_supported=1) - confirmed against a
	// real kernel: calling it synchronously before waiting for the event
	// deadlocks forever (a real upstream bug report - a crashing tcmu
	// handler that never replies - describes the same kernel-side hang
	// from the other direction). It must run concurrently with
	// waitForAddedDevice/AckAddedDevice, not before them.
	enableErr := make(chan error, 1)
	go func() {
		log.Info("enable goroutine: calling EnableBackstore", "device", name)
		err := tcmu.EnableBackstore(configfsRoot, backstore)
		log.Info("enable goroutine: EnableBackstore returned", "device", name, "error", err)
		enableErr <- err
	}()

	log.Info("waiting for ADDED_DEVICE event", "device", name)
	eventCh := make(chan tcmu.DeviceEvent, 1)
	eventErrCh := make(chan error, 1)
	go func() {
		ev, err := waitForAddedDevice(listener, name)
		if err != nil {
			eventErrCh <- err
			return
		}
		eventCh <- ev
	}()

	var ev tcmu.DeviceEvent
	select {
	case ev = <-eventCh:
		log.Info("received ADDED_DEVICE event", "device", name, "minor", ev.Minor, "device_id", ev.DeviceID, "event_name", ev.Name)
	case waitErr := <-eventErrCh:
		return nil, fmt.Errorf("wait for ADDED_DEVICE: %w", waitErr)
	case <-time.After(8 * time.Second):
		// A hard timeout here (rather than hanging indefinitely, or
		// relying on an external `timeout`/SIGTERM to kill this process)
		// is deliberate: found during real-hardware verification that
		// killing this process while EnableBackstore's write(2) is
		// blocked in-kernel can leave the backstore in a stuck, EBUSY,
		// un-removable state until the kernel modules are fully
		// unloaded and reloaded - a clean, fast failure here is much
		// easier to recover from than an external kill.
		return nil, fmt.Errorf("timed out waiting for ADDED_DEVICE event after 8s (device=%s) - the enable goroutine may still be blocked in-kernel", name)
	}

	log.Info("acking ADDED_DEVICE event", "device", name)
	if err := listener.AckAddedDevice(ev.DeviceID); err != nil {
		log.Warn("ack ADDED_DEVICE failed (device may still work if nl_reply_supported wasn't required)", "device", name, "error", err)
	}
	log.Info("waiting for enable goroutine to finish", "device", name)
	select {
	case err := <-enableErr:
		if err != nil {
			return nil, fmt.Errorf("enable backstore: %w", err)
		}
		log.Info("enable backstore confirmed", "device", name)
	case <-time.After(5 * time.Second):
		return nil, fmt.Errorf("timed out waiting for EnableBackstore to return after acking (device=%s) - our ack may not have matched the kernel's expected device id", name)
	}

	dev, err := tcmu.OpenUIODevice(ev.Minor)
	if err != nil {
		return nil, fmt.Errorf("open uio device (minor %d): %w", ev.Minor, err)
	}
	log.Info("uio device opened", "device", name, "minor", ev.Minor)
	// Every error return from here on must close dev first - an orphaned
	// open UIO device with no handler servicing its ring leaves the
	// kernel repeatedly retrying/aborting commands against it forever
	// (found leaking during real-hardware verification).
	defer func() {
		if err != nil {
			_ = dev.Close()
		}
	}()

	// The ring must be serviced *before* the loopback target/LUN/scan
	// steps below, not after setupDevice returns - found against a real
	// kernel: mapping a backstore into a LUN (CreateLoopbackLUN's
	// symlink) synchronously triggers the SCSI core to probe the new
	// device (a real INQUIRY), which blocks until something answers it.
	// The very first version of this function returned a struct for the
	// caller to start servicing afterward, which deadlocked here in
	// exactly the same shape as the earlier EnableBackstore/netlink-ack
	// deadlock, just one layer further out: the probe can't complete
	// until the goroutine that would answer it exists, and that goroutine
	// never got created because setupDevice never returned.
	wg.Add(1)
	go serviceDevice(dev, handler, log, name, wg)
	log.Info("ring buffer service started", "device", name)

	// The before/after ListSCSIHosts diff below identifies "the scsi_host
	// our own CreateLoopbackTarget call just created" by looking at global
	// /sys/class/scsi_host state - ambiguous the moment another
	// gotochanger-tcmud instance (a separate OS process, e.g. a sibling
	// logical library's instance) is doing the same thing concurrently.
	// Found for real: restarting two instances at the same moment produced
	// two different backstores both reporting the same /dev/sgN. A
	// cross-process flock serializes this whole discovery window across
	// every instance on the host - see tcmu.AcquireHostDiscoveryLock's doc
	// comment for the full story.
	releaseHostLock, err := tcmu.AcquireHostDiscoveryLock()
	if err != nil {
		return nil, fmt.Errorf("acquire host discovery lock: %w", err)
	}
	hostsBefore, err := tcmu.ListSCSIHosts()
	if err != nil {
		_ = releaseHostLock()
		return nil, fmt.Errorf("list scsi hosts (before): %w", err)
	}
	loopback := tcmu.LoopbackConfig{WWN: deviceWWN(name)}
	if err := tcmu.CreateLoopbackTarget(configfsRoot, loopback); err != nil {
		_ = releaseHostLock()
		return nil, fmt.Errorf("create loopback target: %w", err)
	}
	log.Info("loopback target created", "device", name, "wwn", loopback.WWN)
	hostsAfter, err := tcmu.ListSCSIHosts()
	if err != nil {
		_ = releaseHostLock()
		return nil, fmt.Errorf("list scsi hosts (after): %w", err)
	}
	_, hostPath, ok := tcmu.NewSCSIHost(hostsBefore, hostsAfter)
	_ = releaseHostLock()
	if !ok {
		return nil, fmt.Errorf("no new scsi host appeared after creating loopback target %s", loopback.WWN)
	}
	log.Info("new scsi host found", "device", name, "host", hostPath)

	if err := tcmu.SetLoopbackNexus(configfsRoot, loopback); err != nil {
		return nil, fmt.Errorf("set loopback nexus: %w", err)
	}
	log.Info("loopback nexus set", "device", name)
	if err := tcmu.CreateLoopbackLUN(configfsRoot, loopback, 0, backstore); err != nil {
		return nil, fmt.Errorf("create loopback lun: %w", err)
	}
	log.Info("loopback lun created", "device", name)
	if err := tcmu.ScanSCSIHost(hostPath); err != nil {
		return nil, fmt.Errorf("scan scsi host %s: %w", hostPath, err)
	}
	log.Info("scsi host scan triggered", "device", name)

	// Best-effort: not knowing the real device path is a real, but
	// non-fatal, degradation (see reportDevicePaths) - a stale or
	// mis-timed sysfs read here must never take down an otherwise
	// working device.
	//
	// Retried a few times with a short backoff, not read exactly once -
	// found on real hardware (bareos-disk-sd-int-fr1, not reproducible on
	// the WSL2 dev host this was first verified against): a medium
	// changer's own device/generic symlink was still missing on the very
	// first check immediately after ScanSCSIHost, 100% reproducible across
	// two separate instances/changers in the same test, while every
	// drive's own symlink was already present on its own first check every
	// time (6/6). ScanSCSIHost only *triggers* an asynchronous kernel scan
	// - it doesn't wait for device nodes to actually appear - and that
	// completion latency apparently isn't uniform across SCSI peripheral
	// device types (mediumx vs tape) on at least this kernel. A drive's own
	// read never needed more than the first attempt in that same test, so
	// this stays cheap in the common case and only pays the extra latency
	// (up to ~250ms) when it's actually needed.
	var paths tcmu.DevicePaths
	var pathErr error
	for attempt := 0; attempt < 5; attempt++ {
		paths, pathErr = tcmu.DiscoverDevicePaths(hostPath)
		if pathErr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if pathErr != nil {
		log.Warn("could not discover real device path (Admin UI/Bareos Config will show a placeholder instead)", "device", name, "error", pathErr)
	} else {
		log.Info("discovered device path", "device", name, "generic", paths.Generic, "tape", paths.Tape)

		// Stable /dev/tape/by-id/... paths (see internal/scsi/vpd.go)
		// depend on udev's own scsi_id VPD 0x83 query having already run
		// against this device - a separate, asynchronous process from
		// the sysfs symlink appearing above, so this is its own retry
		// loop rather than folded into the one above. Best-effort, same
		// as the raw path discovery: no stable symlink yet just means
		// the Admin UI/Bareos Config fall back to the raw /dev/sg*(/dev/
		// nst*) path above, not a failure.
		for attempt := 0; attempt < 5; attempt++ {
			withStable := tcmu.DiscoverStablePaths(paths, byIDTapeDir)
			if withStable.StableGeneric != "" || withStable.StableTape != "" {
				paths = withStable
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if paths.StableGeneric == "" && paths.StableTape == "" {
			log.Info("no stable /dev/tape/by-id path found yet (udev may not have processed this device yet)", "device", name)
		} else {
			log.Info("discovered stable device path", "device", name, "stable_generic", paths.StableGeneric, "stable_tape", paths.StableTape)
		}
	}

	log.Info("device ready", "device", name, "minor", ev.Minor, "wwn", loopback.WWN, "scsi_host", hostPath)
	return &managedDevice{name: name, backstore: backstore, loopback: loopback, dev: dev, hostPath: hostPath, devicePaths: paths}, nil
}

// waitForAddedDevice blocks until an ADDED_DEVICE event naming this device
// arrives, ignoring any other event (there shouldn't be one in flight,
// since setupDevice is called sequentially - see this file's doc comment
// - but being tolerant of an unrelated event costs nothing).
//
// matchesDevice's exact comparison (a substring match against the
// TCMU_ATTR_DEVICE string) is a deliberate hedge: this project hasn't yet
// confirmed against a real kernel whether that attribute is the bare
// configfs device name ("changer0") or a "<hba>/<name>" path - a substring
// match is correct either way. Tighten to an exact match once verified.
func waitForAddedDevice(listener *tcmu.Listener, name string) (tcmu.DeviceEvent, error) {
	for {
		ev, err := listener.ReadEvent()
		if err != nil {
			return tcmu.DeviceEvent{}, err
		}
		if ev.Cmd == tcmu.DeviceEventAdded && matchesDevice(ev, name) {
			return ev, nil
		}
	}
}

func matchesDevice(ev tcmu.DeviceEvent, name string) bool {
	return strings.Contains(ev.Name, name)
}

// deviceWWN derives a stable, deterministic loopback target WWN from a
// device name (so the same emulated device gets the same SCSI identity
// across a restart) - purely internal LIO/configfs fabric naming
// (naa.<hex> loopback target directories), never seen by a real SCSI
// initiator at all. Not the same thing as vpdIdentifier below, which IS
// reported to real initiators (via INQUIRY VPD page 0x83) and is what
// actually makes udev's /dev/tape/by-id symlinks stable - the two are
// computed independently on purpose, so a change to one's format can
// never accidentally affect the other.
func deviceWWN(name string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	return fmt.Sprintf("%016x", h.Sum64())
}

// vpdIdentifier derives a stable, deterministic 8-byte NAA identifier
// from a device name, for internal/scsi.Changer/Drive to report via
// INQUIRY VPD page 0x83 (Device Identification) - see internal/scsi/vpd.go.
// This is what actually solves the real problem this was built for: the
// kernel assigns /dev/sg*/dev/nst* numbers by registration order, which
// is NOT stable across a gotochanger-tcmud restart (confirmed
// reassigned differently on nearly every restart during real testing) -
// but a real INQUIRY VPD 0x83 response is exactly what Linux's own
// stock 60-persistent-storage-tape.rules (shipped by the systemd/udev
// package on every Debian install, no gotochanger-side udev rule or
// packaging change needed) uses to create a persistent
// /dev/tape/by-id/scsi-<designator><hex>[-nst|-changer] symlink - same
// mechanism, same naming convention, real tape libraries use for this
// exact purpose (confirmed against the user's own real ML3 hardware,
// which reports a WWN like 5000E111704BE05B and is configured in Bareos
// via /dev/tape/by-id/scsi-3<hex>-nst instead of a raw device path).
//
// The top nibble of the first byte is forced to 5 (NAA "IEEE Registered"
// format - the same type the user's own real hardware reports, hence
// the WWN starting with "5") so this is a syntactically valid NAA
// identifier; it is NOT a real registered-OUI NAA-5 value (a real one
// splits its remaining 60 bits into a 24-bit IEEE-assigned OUI + 36-bit
// vendor ID) - just a stable, distinct-per-device 8-byte value with a
// valid-looking type nibble, which is all a VPD consumer like udev's
// scsi_id actually needs.
func vpdIdentifier(name string) [8]byte {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], h.Sum64())
	b[0] = 0x50 | (b[0] & 0x0F) // force the NAA type nibble to 5, keep the rest of the hash
	return b
}

func serviceDevice(dev *tcmu.Device, handle func(tcmu.Entry) tcmu.Response, log *slog.Logger, name string, wg *sync.WaitGroup) {
	defer wg.Done()
	cur := dev.Ring.NewCursor()
	for {
		entry, ok, err := cur.Next()
		if err != nil {
			log.Info("device service loop stopping", "device", name, "error", err)
			return
		}
		if !ok {
			if err := dev.WaitForCommand(); err != nil {
				// Expected on shutdown: closing dev's fd (see run's
				// teardown) unblocks this read with an error, which is
				// how this loop learns to exit - not a real failure.
				log.Info("device service loop stopping", "device", name, "error", err)
				return
			}
			continue
		}
		resp := handle(entry)
		if err := cur.Complete(entry, resp); err != nil {
			log.Error("complete entry failed", "device", name, "error", err)
			continue
		}
		if err := dev.Notify(); err != nil {
			log.Error("notify kernel failed", "device", name, "error", err)
		}
	}
}
