// Command gotochanger-changer is a drop-in replacement for Bareos's
// disk-changer.in "Changer Command" script, backed by the gotochangerd
// daemon instead of loose files. It understands the exact positional
// argument convention Bareos uses:
//
//	gotochanger-changer ctl-device command slot archive-device drive-index [volume]
//
// ctl-device is accepted for compatibility but ignored: topology comes from
// gotochangerd's own configuration. Commands: load, unload, list, listall,
// slots, loaded, transfer.
//
// A handful of extra commands are also understood for convenience when
// invoking this tool by hand (never used by Bareos itself): outside,
// outside-delete, io-door, storage-door, ioslots, offsite-send,
// offsite-recall. Cartridge creation (outside-create) was removed - this
// binary is the Bareos-facing changer script, not the admin tool; use
// gotochangerctl's "tape-set" subcommand to create cartridges.
//
// To scope a Bareos Autochanger to one logical library (see gotochangerctl's
// logical-library subcommand), append a static "--logical-library=NAME" flag
// to that Autochanger resource's Changer Command line, e.g.:
//
//	Changer Command = "/usr/bin/gotochanger-changer %c %o %S %a %d %V --logical-library=Library1"
//
// Bareos has no substitution variable for this - it's a fixed suffix per
// Autochanger resource, since each one is already permanently bound to one
// logical library. This flag can appear anywhere in the argument list; it's
// extracted before the positional arguments below are parsed. (Earlier
// versions of this tool inferred a logical library from a trailing bare
// argument, which was ambiguous with the optional trailing tape barcode on
// load/unload - that heuristic has been replaced by this explicit flag.)
//
// Every slot address shown to (or accepted from) Bareos is renumbered into
// a dense, 1-based range (storage slots, then I/O slots right after) scoped
// to whatever's in view for this invocation - see internal/addressing.
// Unscoped, that's the whole physical library, whose addresses are already
// dense from 1, so this is invisible. Scoped to a logical library via
// --logical-library, it matters: a logical library's elements can sit at
// arbitrary, non-contiguous physical addresses (e.g. a second logical
// library carved out of magazines/mailboxes added after the first), and
// Bareos's Storage-resource "max slots" bookkeeping assumes a changer's
// element addresses never exceed its total slot count.
//
// The daemon is reached over its trusted local Unix socket
// (/run/gotochanger/gotochanger.sock by default, override with the
// GOTOCHANGER_SOCKET environment variable) so no API token is needed for
// local Bareos integration.
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/swenske/gotochanger/internal/addressing"
	"github.com/swenske/gotochanger/internal/apiclient"
	"github.com/swenske/gotochanger/internal/library"
)

func socketPath() string {
	if p := os.Getenv("GOTOCHANGER_SOCKET"); p != "" {
		return p
	}
	return "/run/gotochanger/gotochanger.sock"
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: gotochanger-changer [--logical-library=name] ctl-device command [slot archive-device drive-index [volume]]")
	fmt.Fprintln(os.Stderr, "  commands: load unload list listall slots loaded transfer ioslots outside outside-delete io-door storage-door offsite-send offsite-recall")
	fmt.Fprintln(os.Stderr, "  note: ioslot addresses use the contiguous element range (after storage slots), renumbered from 1 for the current scope (whole library, or one logical library with --logical-library)")
	fmt.Fprintln(os.Stderr, "  --logical-library: scope this invocation to one logical library; put a static instance of this flag on the Autochanger's Changer Command line")
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// extractLogicalLibraryFlag pulls a "--logical-library=NAME" or
// "--logical-library NAME" argument out of args, wherever it appears
// (Bareos appends a static suffix to the whole Changer Command line, so it
// isn't necessarily adjacent to any particular positional argument), and
// returns the logical library name plus the remaining arguments in order.
func extractLogicalLibraryFlag(args []string) (logicalLibrary string, rest []string) {
	rest = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--logical-library" && i+1 < len(args):
			logicalLibrary = args[i+1]
			i++
		case strings.HasPrefix(a, "--logical-library="):
			logicalLibrary = strings.TrimPrefix(a, "--logical-library=")
		default:
			rest = append(rest, a)
		}
	}
	return logicalLibrary, rest
}

// Slot/drive address renumbering (formerly presentedAddressing/
// buildPresentedAddressing, private to this file) now lives in
// internal/addressing, shared with cmd/gotochanger-tcmud - see that
// package's doc comment for the full rationale.

func run(args []string) error {
	logicalLib, args := extractLogicalLibraryFlag(args)
	if len(args) < 2 {
		usage()
		return fmt.Errorf("insufficient arguments")
	}
	cmd := args[1]
	rest := args[2:]

	c := apiclient.NewUnix(socketPath())
	if logicalLib != "" {
		c.SetLogicalLibrary(logicalLib)
	}

	switch cmd {
	case "load":
		presented, device, drivePresented, _, err := parseFive(rest)
		if err != nil {
			return err
		}
		_ = device
		st, err := c.Status()
		if err != nil {
			return err
		}
		pa := addressing.Build(st)
		slot, err := pa.Physical(presented)
		if err != nil {
			return err
		}
		drive, err := pa.PhysicalDrive(drivePresented)
		if err != nil {
			return err
		}
		kind, err := elementKindByAddress(st, slot)
		if err != nil {
			return fmt.Errorf("invalid slot address %d: %w", presented, err)
		}
		if err := c.Load(kind, slot, drive); err != nil {
			return err
		}
		return nil

	case "unload":
		presented, _, drivePresented, _, err := parseFive(rest)
		if err != nil {
			return err
		}
		st, err := c.Status()
		if err != nil {
			return err
		}
		pa := addressing.Build(st)
		slot, err := pa.Physical(presented)
		if err != nil {
			return err
		}
		drive, err := pa.PhysicalDrive(drivePresented)
		if err != nil {
			return err
		}
		kind, err := elementKindByAddress(st, slot)
		if err != nil {
			return fmt.Errorf("invalid slot address %d: %w", presented, err)
		}
		if err := c.Unload(drive, kind, slot); err != nil {
			return err
		}
		return nil

	case "transfer":
		if len(rest) < 2 {
			return fmt.Errorf("transfer requires: slot slotdest")
		}
		srcPresented, err := strconv.Atoi(rest[0])
		if err != nil {
			return fmt.Errorf("invalid source slot: %w", err)
		}
		dstPresented, err := strconv.Atoi(rest[1])
		if err != nil {
			return fmt.Errorf("invalid destination slot: %w", err)
		}
		st, err := c.Status()
		if err != nil {
			return err
		}
		pa := addressing.Build(st)
		src, err := pa.Physical(srcPresented)
		if err != nil {
			return fmt.Errorf("invalid source address %d: %w", srcPresented, err)
		}
		dst, err := pa.Physical(dstPresented)
		if err != nil {
			return fmt.Errorf("invalid destination address %d: %w", dstPresented, err)
		}
		fromKind, toKind, err := transferKinds(st, src, dst)
		if err != nil {
			return err
		}
		return c.Move(fromKind, src, toKind, dst)

	case "list":
		st, err := c.Status()
		if err != nil {
			return err
		}
		pa := addressing.Build(st)
		for _, s := range st.Slots {
			addr := pa.Present(s.Address)
			if s.Volume != nil {
				fmt.Printf("%d:%s\n", addr, s.Volume.Barcode)
			} else {
				fmt.Printf("%d:\n", addr)
			}
		}
		return nil

	case "listall":
		return listAll(c)

	case "slots":
		// Mirrors mtx-changer's "slots" command, which parses real mtx's
		// "T Slots ( M Import/Export )" header and returns T: the *total*
		// addressable element count, storage slots plus I/O slots, not
		// just the storage slot count. Bareos relies on this total to know
		// the valid address range before it maps individual addresses to
		// regular vs. import/export slots via "listall".
		st, err := c.Status()
		if err != nil {
			return err
		}
		fmt.Println(len(st.Slots) + len(st.IOSlots))
		return nil

	case "ioslots":
		st, err := c.Status()
		if err != nil {
			return err
		}
		fmt.Println(len(st.IOSlots))
		return nil

	case "loaded":
		if len(rest) < 3 {
			return fmt.Errorf("loaded requires: slot archive-device drive-index")
		}
		drivePresented, err := strconv.Atoi(rest[2])
		if err != nil {
			return fmt.Errorf("invalid drive index: %w", err)
		}
		st, err := c.Status()
		if err != nil {
			return err
		}
		pa := addressing.Build(st)
		drive, err := pa.PhysicalDrive(drivePresented)
		if err != nil {
			return err
		}
		for _, d := range st.Drives {
			if d.Index == drive {
				if d.Origin != nil && d.Origin.Kind == library.KindSlot {
					fmt.Println(pa.Present(d.Origin.Address))
				} else {
					fmt.Println(0)
				}
				return nil
			}
		}
		return fmt.Errorf("drive %d not found", drivePresented)

	case "outside":
		vs, err := c.OutsideVolumes()
		if err != nil {
			return err
		}
		for _, v := range vs {
			fmt.Println(v.Barcode)
		}
		return nil

	case "outside-create":
		return fmt.Errorf("subcommand %q was removed; use gotochangerctl's \"tape-set new\"/\"tape-set add-tapes\"/\"tape-set add-tape\" to create cartridges", cmd)

	case "outside-delete":
		if len(rest) != 1 {
			return fmt.Errorf("outside-delete requires: barcode")
		}
		return c.DeleteOutsideVolume(rest[0])

	case "io-door":
		if len(rest) < 2 {
			return fmt.Errorf("io-door requires: mailbox-id open|close [pin]")
		}
		mailboxID := rest[0]
		if rest[1] == "open" {
			pin := ""
			if len(rest) > 2 {
				pin = rest[2]
			}
			return c.OpenIODoor(mailboxID, pin)
		}
		if rest[1] != "close" {
			return fmt.Errorf("io-door requires: mailbox-id open|close [pin]")
		}
		return c.CloseIODoor(mailboxID, nil)

	case "storage-door":
		if len(rest) < 2 {
			return fmt.Errorf("storage-door requires: magazine-id open|close [pin]")
		}
		magazineID := rest[0]
		if rest[1] == "open" {
			pin := ""
			if len(rest) > 2 {
				pin = rest[2]
			}
			return c.OpenStorageDoor(magazineID, pin)
		}
		if rest[1] != "close" {
			return fmt.Errorf("storage-door requires: magazine-id open|close [pin]")
		}
		return c.CloseStorageDoor(magazineID, nil)

	case "offsite-send":
		if len(rest) != 1 {
			return fmt.Errorf("offsite-send requires: slot")
		}
		presented, err := strconv.Atoi(rest[0])
		if err != nil {
			return fmt.Errorf("invalid slot: %w", err)
		}
		st, err := c.Status()
		if err != nil {
			return err
		}
		slot, err := addressing.Build(st).Physical(presented)
		if err != nil {
			return err
		}
		kind, err := elementKindByAddress(st, slot)
		if err != nil {
			return fmt.Errorf("invalid slot address %d: %w", presented, err)
		}
		vol, err := c.OffsiteSend(kind, slot)
		if err != nil {
			return err
		}
		fmt.Printf("sent %s offsite\n", vol.Barcode)
		return nil

	case "offsite-recall":
		if len(rest) != 2 {
			return fmt.Errorf("offsite-recall requires: barcode slot")
		}
		presented, err := strconv.Atoi(rest[1])
		if err != nil {
			return fmt.Errorf("invalid slot: %w", err)
		}
		st, err := c.Status()
		if err != nil {
			return err
		}
		slot, err := addressing.Build(st).Physical(presented)
		if err != nil {
			return err
		}
		kind, err := elementKindByAddress(st, slot)
		if err != nil {
			return fmt.Errorf("invalid slot address %d: %w", presented, err)
		}
		return c.OffsiteRecall(rest[0], kind, slot)

	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// parseFive parses the common "slot archive-device drive-index [volume]"
// tail shared by load/unload.
func parseFive(rest []string) (slot int, device string, drive int, volume string, err error) {
	if len(rest) < 3 {
		return 0, "", 0, "", fmt.Errorf("expected: slot archive-device drive-index [volume]")
	}
	slot, err = strconv.Atoi(rest[0])
	if err != nil {
		return 0, "", 0, "", fmt.Errorf("invalid slot: %w", err)
	}
	device = rest[1]
	drive, err = strconv.Atoi(rest[2])
	if err != nil {
		return 0, "", 0, "", fmt.Errorf("invalid drive index: %w", err)
	}
	if len(rest) > 3 {
		volume = rest[3]
	}
	return slot, device, drive, volume, nil
}

func transferKinds(st library.Status, src, dst int) (string, string, error) {
	fromKind, err := elementKindByAddress(st, src)
	if err != nil {
		return "", "", fmt.Errorf("invalid source address %d: %w", src, err)
	}
	toKind, err := elementKindByAddress(st, dst)
	if err != nil {
		return "", "", fmt.Errorf("invalid destination address %d: %w", dst, err)
	}
	return fromKind, toKind, nil
}

// elementKindByAddress resolves a physical address to the element kind the
// API expects ("slot" or "ioslot").
//
// Every command that takes an address goes through this, not just transfer.
// load/unload/offsite-send/offsite-recall used to hardcode "slot", which
// silently made every one of them fail with "element not found" whenever
// Bareos addressed an import/export slot - even though "slots" reports the
// combined storage+I/O total and "listall" advertises those very addresses
// with an I: prefix, so Bareos is entitled to use them. The addresses are
// one contiguous range by design (see this file's package comment), so the
// kind can only ever come from looking the address up, never from assuming.
func elementKindByAddress(st library.Status, addr int) (string, error) {
	for _, s := range st.Slots {
		if s.Address == addr {
			return "slot", nil
		}
	}
	for _, io := range st.IOSlots {
		if io.Address == addr {
			return "ioslot", nil
		}
	}
	return "", fmt.Errorf("element not found")
}

func listAll(c *apiclient.Client) error {
	st, err := c.Status()
	if err != nil {
		return err
	}
	pa := addressing.Build(st)
	for _, d := range st.Drives {
		drive := pa.PresentDrive(d.Index)
		if d.Volume != nil && d.Origin != nil && (d.Origin.Kind == library.KindSlot || d.Origin.Kind == library.KindIOSlot) {
			fmt.Printf("D:%d:F:%d:%s\n", drive, pa.Present(d.Origin.Address), d.Volume.Barcode)
		} else if d.Volume != nil {
			fmt.Printf("D:%d:F:0:%s\n", drive, d.Volume.Barcode)
		} else {
			fmt.Printf("D:%d:E\n", drive)
		}
	}
	for _, s := range st.Slots {
		addr := pa.Present(s.Address)
		if s.Volume != nil {
			fmt.Printf("S:%d:F:%s\n", addr, s.Volume.Barcode)
		} else {
			fmt.Printf("S:%d:E\n", addr)
		}
	}
	for _, io := range st.IOSlots {
		addr := pa.Present(io.Address)
		if io.Volume != nil {
			fmt.Printf("I:%d:F:%s\n", addr, io.Volume.Barcode)
		} else {
			fmt.Printf("I:%d:E\n", addr)
		}
	}
	return nil
}
