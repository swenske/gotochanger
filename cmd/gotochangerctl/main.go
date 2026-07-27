// Command gotochangerctl is the general purpose administration CLI for
// gotochangerd: status inspection, manual moves, volume management and API
// token management. By default it talks to the daemon over the trusted
// local Unix socket; pass --url/--token to manage a remote instance over
// its authenticated TCP listener.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/swenske/gotochanger/internal/apiclient"
	"github.com/swenske/gotochanger/internal/config"
	"github.com/swenske/gotochanger/internal/library"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("gotochangerctl", flag.ExitOnError)
	socket := fs.String("socket", envOr("GOTOCHANGER_SOCKET", "/run/gotochanger/gotochanger.sock"), "path to the trusted local Unix socket")
	url := fs.String("url", "", "base URL of a remote gotochangerd HTTP API, e.g. http://host:8480 (requires --token)")
	token := fs.String("token", envOr("GOTOCHANGER_TOKEN", ""), "API token, required when --url is set")
	jsonOut := fs.Bool("json", false, "print raw JSON responses instead of a formatted summary")
	logicalLibrary := fs.String("logical-library", "", "scope load/unload/move/status to this logical library")
	// fs is flag.ExitOnError - Parse never returns a non-nil error, it
	// calls os.Exit(2) itself on a bad flag.
	_ = fs.Parse(args)

	rest := fs.Args()
	if len(rest) == 0 {
		printUsage()
		return fmt.Errorf("missing subcommand")
	}

	var c *apiclient.Client
	if *url != "" {
		c = apiclient.NewHTTP(*url, *token)
	} else {
		c = apiclient.NewUnix(*socket)
	}
	if *logicalLibrary != "" {
		c.SetLogicalLibrary(*logicalLibrary)
	}

	cmd, cmdArgs := rest[0], rest[1:]
	switch cmd {
	case "status":
		st, err := c.Status()
		if err != nil {
			return err
		}
		return printResult(st, *jsonOut, func() { printStatus(st) })

	case "events":
		evs, err := c.Events()
		if err != nil {
			return err
		}
		return printResult(evs, *jsonOut, func() {
			for _, e := range evs {
				code := e.Code
				if code == "" {
					code = e.Type
				}
				fmt.Printf("%s  %-38s %-8s %s\n", e.Time.Format("2006-01-02T15:04:05Z"), code, e.Outcome, e.Message)
			}
		})

	case "volumes":
		vs, err := c.Volumes()
		if err != nil {
			return err
		}
		return printResult(vs, *jsonOut, func() {
			for _, v := range vs {
				full := ""
				if v.Full {
					full = " FULL"
				}
				fmt.Printf("%-24s %12d/%-12d bytes%s  %s\n", v.Barcode, v.WrittenBytes, v.CapacityBytes, full, v.Path)
			}
		})

	case "outside":
		vs, err := c.OutsideVolumes()
		if err != nil {
			return err
		}
		return printResult(vs, *jsonOut, func() {
			for _, v := range vs {
				full := ""
				if v.Full {
					full = " FULL"
				}
				fmt.Printf("%-24s %12d/%-12d bytes%s  %s\n", v.Barcode, v.WrittenBytes, v.CapacityBytes, full, v.Path)
			}
		})

	case "load":
		if len(cmdArgs) != 3 {
			return fmt.Errorf("usage: load <slot|ioslot> <address> <drive>")
		}
		addr, err := strconv.Atoi(cmdArgs[1])
		if err != nil {
			return err
		}
		drive, err := strconv.Atoi(cmdArgs[2])
		if err != nil {
			return err
		}
		return c.Load(cmdArgs[0], addr, drive)

	case "unload":
		if len(cmdArgs) != 3 {
			return fmt.Errorf("usage: unload <drive> <slot|ioslot> <address>")
		}
		drive, err := strconv.Atoi(cmdArgs[0])
		if err != nil {
			return err
		}
		addr, err := strconv.Atoi(cmdArgs[2])
		if err != nil {
			return err
		}
		return c.Unload(drive, cmdArgs[1], addr)

	case "move":
		if len(cmdArgs) != 4 {
			return fmt.Errorf("usage: move <slot|ioslot> <address> <slot|ioslot> <address>")
		}
		fromAddr, err := strconv.Atoi(cmdArgs[1])
		if err != nil {
			return err
		}
		toAddr, err := strconv.Atoi(cmdArgs[3])
		if err != nil {
			return err
		}
		return c.Move(cmdArgs[0], fromAddr, cmdArgs[2], toAddr)

	case "outside-create":
		return fmt.Errorf("subcommand %q was removed; use \"tape-set new\" (with an initial tape count) or \"tape-set add-tapes\"/\"tape-set add-tape\" to add cartridges to a tape set", cmd)

	case "outside-delete":
		if len(cmdArgs) != 1 {
			return fmt.Errorf("usage: outside-delete <barcode>")
		}
		return c.DeleteOutsideVolume(cmdArgs[0])

	case "io-door":
		if len(cmdArgs) < 2 {
			return fmt.Errorf("usage: io-door <mailbox-id> open [pin] | io-door <mailbox-id> close [actions-json]")
		}
		mailboxID := cmdArgs[0]
		if cmdArgs[1] == "open" {
			pin := ""
			if len(cmdArgs) > 2 {
				pin = cmdArgs[2]
			}
			return c.OpenIODoor(mailboxID, pin)
		}
		if cmdArgs[1] != "close" {
			return fmt.Errorf("usage: io-door <mailbox-id> open [pin] | io-door <mailbox-id> close [actions-json]")
		}
		actions := []library.DoorAction{}
		if len(cmdArgs) > 2 {
			if err := json.Unmarshal([]byte(cmdArgs[2]), &actions); err != nil {
				return fmt.Errorf("invalid actions-json: %w", err)
			}
		}
		return c.CloseIODoor(mailboxID, actions)

	case "storage-door":
		if len(cmdArgs) < 2 {
			return fmt.Errorf("usage: storage-door <magazine-id> open [pin] | storage-door <magazine-id> close [actions-json]")
		}
		magazineID := cmdArgs[0]
		if cmdArgs[1] == "open" {
			pin := ""
			if len(cmdArgs) > 2 {
				pin = cmdArgs[2]
			}
			return c.OpenStorageDoor(magazineID, pin)
		}
		if cmdArgs[1] != "close" {
			return fmt.Errorf("usage: storage-door <magazine-id> open [pin] | storage-door <magazine-id> close [actions-json]")
		}
		actions := []library.DoorAction{}
		if len(cmdArgs) > 2 {
			if err := json.Unmarshal([]byte(cmdArgs[2]), &actions); err != nil {
				return fmt.Errorf("invalid actions-json: %w", err)
			}
		}
		return c.CloseStorageDoor(magazineID, actions)

	case "import", "create-volume", "export":
		return fmt.Errorf("subcommand %q was removed; use tape-set/outside-delete/io-door/storage-door workflow", cmd)

	case "fault":
		if len(cmdArgs) != 2 {
			return fmt.Errorf("usage: fault <drive> <on|off>")
		}
		drive, err := strconv.Atoi(cmdArgs[0])
		if err != nil {
			return err
		}
		return c.DriveFault(drive, cmdArgs[1] == "on")

	case "write-protect":
		if len(cmdArgs) != 2 {
			return fmt.Errorf("usage: write-protect <barcode> <on|off>")
		}
		return c.SetVolumeWriteProtect(cmdArgs[0], cmdArgs[1] == "on")

	case "robotic-fault":
		return runRoboticFault(c, cmdArgs)

	case "token":
		return runToken(c, cmdArgs)

	case "user":
		return runUser(c, cmdArgs)

	case "settings":
		return runSettings(c, cmdArgs, *jsonOut)

	case "latency":
		return runLatency(c, cmdArgs, *jsonOut)

	case "cleaning":
		return runCleaning(c, cmdArgs, *jsonOut)

	case "logical-library":
		return runLogicalLibrary(c, cmdArgs, *jsonOut)

	case "drive-type":
		return runDriveType(c, cmdArgs, *jsonOut)

	case "tape-type":
		return runTapeType(c, cmdArgs, *jsonOut)

	case "tape-set":
		return runTapeSet(c, cmdArgs, *jsonOut)

	case "magazine":
		return runMagazine(c, cmdArgs, *jsonOut)

	case "mailbox":
		return runMailbox(c, cmdArgs, *jsonOut)

	case "drive":
		return runDrive(c, cmdArgs, *jsonOut)

	case "unassigned":
		u, err := c.UnassignedElements()
		if err != nil {
			return err
		}
		return printResult(u, *jsonOut, func() {
			fmt.Println("Unassigned drives:")
			for _, d := range u.Drives {
				fmt.Printf("  drive %d %s\n", d.Index, d.DevicePath)
			}
			fmt.Println("Unassigned slots:")
			for _, s := range u.Slots {
				fmt.Printf("  slot %d (magazine %s)\n", s.Address, s.MagazineID)
			}
			fmt.Println("Unassigned I/O slots:")
			for _, io := range u.IOSlots {
				fmt.Printf("  ioslot %d\n", io.Address)
			}
		})

	case "offsite":
		return runOffsite(c, cmdArgs, *jsonOut)

	case "backup":
		return runBackup(c, cmdArgs, *jsonOut)

	case "restore":
		if len(cmdArgs) != 1 {
			return fmt.Errorf("usage: restore <file>")
		}
		data, err := os.ReadFile(cmdArgs[0])
		if err != nil {
			return err
		}
		if err := c.Restore(data); err != nil {
			return err
		}
		fmt.Println("restore successful; the service is restarting to apply it")
		return nil

	case "reset":
		if len(cmdArgs) < 1 {
			return fmt.Errorf("usage: reset <confirm-name> [--delete-volumes]")
		}
		confirmName := cmdArgs[0]
		deleteVolumes := false
		for _, a := range cmdArgs[1:] {
			if a == "--delete-volumes" {
				deleteVolumes = true
				continue
			}
			return fmt.Errorf("usage: reset <confirm-name> [--delete-volumes]")
		}
		if err := c.Reset(confirmName, deleteVolumes); err != nil {
			return err
		}
		fmt.Println("reset successful; the service is restarting to apply it")
		return nil

	case "wizard":
		return runWizard(c, cmdArgs, *jsonOut)

	default:
		printUsage()
		return fmt.Errorf("unknown subcommand %q", cmd)
	}
}

func runLogicalLibrary(c *apiclient.Client, args []string, jsonOut bool) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: logical-library <new|list|show|update|delete> ...")
	}
	switch args[0] {
	case "new":
		if len(args) != 5 {
			return fmt.Errorf("usage: logical-library new <name> <drive-indices csv> <magazine-ids csv> <mailbox-ids csv>")
		}
		lib := config.LogicalLibraryConfig{Name: args[1], Drives: parseIntList(args[2]), Magazines: parseStringList(args[3]), Mailboxes: parseStringList(args[4])}
		_, err := c.CreateLogicalLibrary(lib)
		return err
	case "list":
		libs, err := c.ListLogicalLibraries()
		if err != nil {
			return err
		}
		return printResult(libs, jsonOut, func() {
			for _, l := range libs {
				fmt.Printf("%-20s drives=%d slots=%d ioslots=%d color=%s\n", l.Name, len(l.Drives), len(l.Slots), len(l.IOSlots), l.Color)
			}
		})
	case "show":
		if len(args) != 2 {
			return fmt.Errorf("usage: logical-library show <name>")
		}
		lib, err := c.GetLogicalLibrary(args[1])
		if err != nil {
			return err
		}
		return printResult(lib, jsonOut, func() { fmt.Printf("%+v\n", lib) })
	case "update":
		if len(args) != 6 {
			return fmt.Errorf("usage: logical-library update <name> <color> <drive-indices csv> <magazine-ids csv> <mailbox-ids csv>")
		}
		lib := config.LogicalLibraryConfig{Name: args[1], Color: args[2], Drives: parseIntList(args[3]), Magazines: parseStringList(args[4]), Mailboxes: parseStringList(args[5])}
		_, err := c.UpdateLogicalLibrary(args[1], lib)
		return err
	case "delete":
		if len(args) != 2 {
			return fmt.Errorf("usage: logical-library delete <name>")
		}
		return c.DeleteLogicalLibrary(args[1])
	default:
		return fmt.Errorf("unknown logical-library subcommand %q", args[0])
	}
}

func runDriveType(c *apiclient.Client, args []string, jsonOut bool) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: drive-type <new|list|update|delete> ...")
	}
	switch args[0] {
	case "new":
		if len(args) != 5 {
			return fmt.Errorf("usage: drive-type new <name> <speed> <capacity> <description>")
		}
		return c.CreateDriveType(config.DriveType{Name: args[1], Speed: args[2], Capacity: args[3], Description: args[4]})
	case "list":
		dts, err := c.ListDriveTypes()
		if err != nil {
			return err
		}
		return printResult(dts, jsonOut, func() {
			for _, dt := range dts {
				fmt.Printf("%-12s %-10s %-10s %s\n", dt.Name, dt.Speed, dt.Capacity, dt.Description)
			}
		})
	case "update":
		if len(args) != 5 {
			return fmt.Errorf("usage: drive-type update <name> <speed> <capacity> <description>")
		}
		return c.UpdateDriveType(args[1], config.DriveType{Name: args[1], Speed: args[2], Capacity: args[3], Description: args[4]})
	case "delete":
		if len(args) != 2 {
			return fmt.Errorf("usage: drive-type delete <name>")
		}
		return c.DeleteDriveType(args[1])
	default:
		return fmt.Errorf("unknown drive-type subcommand %q", args[0])
	}
}

func runTapeType(c *apiclient.Client, args []string, jsonOut bool) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: tape-type <new|list|update|delete> ...")
	}
	switch args[0] {
	case "new":
		if len(args) != 7 {
			return fmt.Errorf("usage: tape-type new <name> <capacity> <description> <barcode-family> <media-id> <volser-length>")
		}
		volserLength, err := strconv.Atoi(args[6])
		if err != nil {
			return err
		}
		return c.CreateTapeType(config.TapeType{Name: args[1], Capacity: args[2], Description: args[3], BarcodeFamily: args[4], MediaID: args[5], VolSerLength: volserLength})
	case "list":
		tts, err := c.ListTapeTypes()
		if err != nil {
			return err
		}
		return printResult(tts, jsonOut, func() {
			for _, tt := range tts {
				fmt.Printf("%-14s %-10s %-8s %-4s %-3d %s\n", tt.Name, tt.Capacity, tt.BarcodeFamily, tt.MediaID, tt.VolSerLength, tt.Description)
			}
		})
	case "update":
		if len(args) != 7 {
			return fmt.Errorf("usage: tape-type update <name> <capacity> <description> <barcode-family> <media-id> <volser-length>")
		}
		volserLength, err := strconv.Atoi(args[6])
		if err != nil {
			return err
		}
		return c.UpdateTapeType(args[1], config.TapeType{Name: args[1], Capacity: args[2], Description: args[3], BarcodeFamily: args[4], MediaID: args[5], VolSerLength: volserLength})
	case "delete":
		if len(args) != 2 {
			return fmt.Errorf("usage: tape-type delete <name>")
		}
		return c.DeleteTapeType(args[1])
	default:
		return fmt.Errorf("unknown tape-type subcommand %q", args[0])
	}
}

func runTapeSet(c *apiclient.Client, args []string, jsonOut bool) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: tape-set <new|list|update|delete|add-tapes|add-tape> ...")
	}
	switch args[0] {
	case "new":
		if len(args) != 5 {
			return fmt.Errorf("usage: tape-set new <name> <tape-type> <storage-folder> <initial-tape-count>")
		}
		count, err := strconv.Atoi(args[4])
		if err != nil {
			return err
		}
		res, err := c.CreateTapeSet(args[1], args[2], args[3], count)
		if err != nil {
			return err
		}
		return printResult(res, jsonOut, func() {
			fmt.Printf("created tape set %s with %d cartridge(s):\n", res.Name, len(res.Cartridges))
			for _, v := range res.Cartridges {
				fmt.Println(" ", v.Barcode)
			}
		})
	case "list":
		sets, err := c.ListTapeSets()
		if err != nil {
			return err
		}
		return printResult(sets, jsonOut, func() {
			for _, ts := range sets {
				fmt.Printf("%-16s %-12s %s\n", ts.Name, ts.TapeType, ts.StorageFolder)
			}
		})
	case "update":
		if len(args) != 4 {
			return fmt.Errorf("usage: tape-set update <name> <tape-type> <storage-folder>")
		}
		return c.UpdateTapeSet(args[1], config.TapeSetConfig{Name: args[1], TapeType: args[2], StorageFolder: args[3]})
	case "delete":
		if len(args) != 2 {
			return fmt.Errorf("usage: tape-set delete <name>")
		}
		return c.DeleteTapeSet(args[1])
	case "add-tapes":
		if len(args) != 3 {
			return fmt.Errorf("usage: tape-set add-tapes <name> <count>")
		}
		count, err := strconv.Atoi(args[2])
		if err != nil {
			return err
		}
		vols, err := c.AddTapeSetTapes(args[1], count)
		if err != nil {
			return err
		}
		return printResult(vols, jsonOut, func() {
			for _, v := range vols {
				fmt.Println(v.Barcode)
			}
		})
	case "add-tape":
		if len(args) != 3 {
			return fmt.Errorf("usage: tape-set add-tape <name> <barcode>")
		}
		vol, err := c.AddTapeSetTape(args[1], args[2])
		if err != nil {
			return err
		}
		return printResult(vol, jsonOut, func() {
			fmt.Println(vol.Barcode)
		})
	default:
		return fmt.Errorf("unknown tape-set subcommand %q", args[0])
	}
}

func runMagazine(c *apiclient.Client, args []string, jsonOut bool) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: magazine <new|list|update|delete> ...")
	}
	switch args[0] {
	case "new":
		if len(args) != 3 {
			return fmt.Errorf("usage: magazine new <id> <slots>")
		}
		slots, err := strconv.Atoi(args[2])
		if err != nil {
			return err
		}
		return c.CreateMagazine(config.MagazineConfig{ID: args[1], Slots: slots})
	case "list":
		mags, err := c.ListMagazines()
		if err != nil {
			return err
		}
		return printResult(mags, jsonOut, func() {
			for _, m := range mags {
				fmt.Printf("%-16s %d slots\n", m.ID, m.Slots)
			}
		})
	case "update":
		if len(args) != 3 {
			return fmt.Errorf("usage: magazine update <id> <slots>")
		}
		slots, err := strconv.Atoi(args[2])
		if err != nil {
			return err
		}
		return c.UpdateMagazine(args[1], config.MagazineConfig{ID: args[1], Slots: slots})
	case "delete":
		if len(args) != 2 {
			return fmt.Errorf("usage: magazine delete <id>")
		}
		return c.DeleteMagazine(args[1])
	default:
		return fmt.Errorf("unknown magazine subcommand %q", args[0])
	}
}

func runMailbox(c *apiclient.Client, args []string, jsonOut bool) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: mailbox <new|list|update|delete> ...")
	}
	switch args[0] {
	case "new":
		if len(args) != 3 {
			return fmt.Errorf("usage: mailbox new <id> <slots>")
		}
		slots, err := strconv.Atoi(args[2])
		if err != nil {
			return err
		}
		return c.CreateMailbox(config.MailboxConfig{ID: args[1], Slots: slots})
	case "list":
		mbs, err := c.ListMailboxes()
		if err != nil {
			return err
		}
		return printResult(mbs, jsonOut, func() {
			for _, m := range mbs {
				fmt.Printf("%-16s %d slots\n", m.ID, m.Slots)
			}
		})
	case "update":
		if len(args) != 3 {
			return fmt.Errorf("usage: mailbox update <id> <slots>")
		}
		slots, err := strconv.Atoi(args[2])
		if err != nil {
			return err
		}
		return c.UpdateMailbox(args[1], config.MailboxConfig{ID: args[1], Slots: slots})
	case "delete":
		if len(args) != 2 {
			return fmt.Errorf("usage: mailbox delete <id>")
		}
		return c.DeleteMailbox(args[1])
	default:
		return fmt.Errorf("unknown mailbox subcommand %q", args[0])
	}
}

func runDrive(c *apiclient.Client, args []string, jsonOut bool) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: drive <new|list|update|delete> ...")
	}
	switch args[0] {
	case "new":
		if len(args) != 1 && len(args) != 2 {
			return fmt.Errorf("usage: drive new [device-path]")
		}
		var devicePath string
		if len(args) == 2 {
			devicePath = args[1]
		}
		d, err := c.CreateDrive(devicePath)
		if err != nil {
			return err
		}
		return printResult(d, jsonOut, func() {
			fmt.Printf("drive %d %s\n", d.Index, d.DevicePath)
		})
	case "list":
		drives, err := c.ListDrives()
		if err != nil {
			return err
		}
		return printResult(drives, jsonOut, func() {
			for _, d := range drives {
				fmt.Printf("%-4d %s\n", d.Index, d.DevicePath)
			}
		})
	case "update":
		if len(args) != 3 {
			return fmt.Errorf("usage: drive update <index> <device-path>")
		}
		idx, err := strconv.Atoi(args[1])
		if err != nil {
			return err
		}
		return c.UpdateDrive(idx, args[2])
	case "delete":
		if len(args) != 2 {
			return fmt.Errorf("usage: drive delete <index>")
		}
		idx, err := strconv.Atoi(args[1])
		if err != nil {
			return err
		}
		return c.DeleteDrive(idx)
	default:
		return fmt.Errorf("unknown drive subcommand %q", args[0])
	}
}

func runOffsite(c *apiclient.Client, args []string, jsonOut bool) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: offsite <list|send|recall> ...")
	}
	switch args[0] {
	case "list":
		vs, err := c.OffsiteVolumes()
		if err != nil {
			return err
		}
		return printResult(vs, jsonOut, func() {
			for _, v := range vs {
				fmt.Printf("%-24s %12d/%-12d bytes\n", v.Barcode, v.WrittenBytes, v.CapacityBytes)
			}
		})
	case "send":
		if len(args) != 3 {
			return fmt.Errorf("usage: offsite send <slot|ioslot> <address>")
		}
		addr, err := strconv.Atoi(args[2])
		if err != nil {
			return err
		}
		vol, err := c.OffsiteSend(args[1], addr)
		if err != nil {
			return err
		}
		fmt.Printf("sent %s offsite\n", vol.Barcode)
		return nil
	case "recall":
		if len(args) != 4 {
			return fmt.Errorf("usage: offsite recall <barcode> <slot|ioslot> <address>")
		}
		addr, err := strconv.Atoi(args[3])
		if err != nil {
			return err
		}
		return c.OffsiteRecall(args[1], args[2], addr)
	default:
		return fmt.Errorf("unknown offsite subcommand %q", args[0])
	}
}

func runBackup(c *apiclient.Client, args []string, jsonOut bool) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: backup <download|list|download-stored|delete|schedule> ...")
	}
	switch args[0] {
	case "download":
		if len(args) != 2 {
			return fmt.Errorf("usage: backup download <output-file>")
		}
		data, filename, err := c.DownloadBackup()
		if err != nil {
			return err
		}
		if err := os.WriteFile(args[1], data, 0o640); err != nil {
			return err
		}
		fmt.Printf("wrote %s (%d bytes, server suggested name %q)\n", args[1], len(data), filename)
		return nil
	case "list":
		files, err := c.ListBackups()
		if err != nil {
			return err
		}
		return printResult(files, jsonOut, func() {
			for _, f := range files {
				fmt.Printf("%-40s %12d bytes  %s\n", f.Name, f.SizeBytes, f.CreatedAt.Format("2006-01-02T15:04:05Z"))
			}
		})
	case "download-stored":
		if len(args) != 3 {
			return fmt.Errorf("usage: backup download-stored <filename> <output-file>")
		}
		data, err := c.DownloadStoredBackup(args[1])
		if err != nil {
			return err
		}
		if err := os.WriteFile(args[2], data, 0o640); err != nil {
			return err
		}
		fmt.Printf("wrote %s (%d bytes)\n", args[2], len(data))
		return nil
	case "delete":
		if len(args) != 2 {
			return fmt.Errorf("usage: backup delete <filename>")
		}
		return c.DeleteBackup(args[1])
	case "schedule":
		if len(args) == 1 || (len(args) >= 2 && args[1] == "show") {
			sched, err := c.GetBackupSchedule()
			if err != nil {
				return err
			}
			return printResult(sched, true, func() {})
		}
		if args[1] != "set" {
			return fmt.Errorf("usage: backup schedule <show|set> [interval=<duration>] [retention=<n>]")
		}
		var interval *string
		var retention *int
		for _, kv := range args[2:] {
			parts := strings.SplitN(kv, "=", 2)
			if len(parts) != 2 {
				return fmt.Errorf("invalid key=value pair %q", kv)
			}
			switch parts[0] {
			case "interval":
				v := parts[1]
				interval = &v
			case "retention":
				n, err := strconv.Atoi(parts[1])
				if err != nil {
					return fmt.Errorf("invalid retention %q: %w", parts[1], err)
				}
				retention = &n
			default:
				return fmt.Errorf("unknown schedule field %q", parts[0])
			}
		}
		sched, err := c.UpdateBackupSchedule(interval, retention)
		if err != nil {
			return err
		}
		return printResult(sched, true, func() {})
	default:
		return fmt.Errorf("unknown backup subcommand %q", args[0])
	}
}

func runWizard(c *apiclient.Client, args []string, jsonOut bool) error {
	if len(args) == 0 || args[0] == "status" {
		ws, err := c.WizardState()
		if err != nil {
			return err
		}
		return printResult(ws, true, func() {})
	}
	return fmt.Errorf("usage: wizard status (full wizard interaction is via the web UI or REST API)")
}

func parseIntList(csv string) []int {
	if csv == "" {
		return nil
	}
	var out []int
	for _, s := range strings.Split(csv, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func parseStringList(csv string) []string {
	if csv == "" {
		return nil
	}
	var out []string
	for _, s := range strings.Split(csv, ",") {
		out = append(out, strings.TrimSpace(s))
	}
	return out
}

func runToken(c *apiclient.Client, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: token <new|revoke|list> [name] [role]")
	}
	switch args[0] {
	case "new":
		if len(args) != 3 {
			return fmt.Errorf("usage: token new <name> <admin|operator|viewer>")
		}
		raw, err := c.CreateToken(args[1], args[2])
		if err != nil {
			return err
		}
		fmt.Println(raw)
		return nil
	case "revoke":
		if len(args) != 2 {
			return fmt.Errorf("usage: token revoke <name>")
		}
		return c.RevokeToken(args[1])
	case "list":
		toks, err := c.ListTokens()
		if err != nil {
			return err
		}
		for _, t := range toks {
			fmt.Printf("%-20s %-10s created %s\n", t.Name, t.Role, t.CreatedAt.Format("2006-01-02T15:04:05Z"))
		}
		return nil
	default:
		return fmt.Errorf("unknown token subcommand %q", args[0])
	}
}

func runUser(c *apiclient.Client, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: user <new|list|delete|role|reset-password> ...")
	}
	switch args[0] {
	case "new":
		if len(args) != 4 {
			return fmt.Errorf("usage: user new <username> <admin|operator|viewer> <password>")
		}
		u, err := c.CreateUser(args[1], args[2], args[3])
		if err != nil {
			return err
		}
		fmt.Printf("created %s (%s)\n", u.Username, u.Role)
		return nil
	case "list":
		users, err := c.ListUsers()
		if err != nil {
			return err
		}
		for _, u := range users {
			flag := ""
			if u.MustChangePassword {
				flag = " (must change password)"
			}
			fmt.Printf("%-20s %-10s%s\n", u.Username, u.Role, flag)
		}
		return nil
	case "delete":
		if len(args) != 2 {
			return fmt.Errorf("usage: user delete <username>")
		}
		return c.DeleteUser(args[1])
	case "role":
		if len(args) != 3 {
			return fmt.Errorf("usage: user role <username> <admin|operator|viewer>")
		}
		return c.SetUserRole(args[1], args[2])
	case "reset-password":
		if len(args) != 3 {
			return fmt.Errorf("usage: user reset-password <username> <new-password>")
		}
		return c.ResetUserPassword(args[1], args[2])
	default:
		return fmt.Errorf("unknown user subcommand %q", args[0])
	}
}

func runSettings(c *apiclient.Client, args []string, jsonOut bool) error {
	if len(args) == 0 || args[0] == "get" {
		settings, err := c.GetSettings()
		if err != nil {
			return err
		}
		return printResult(settings, true, func() {})
	}
	if args[0] != "set" {
		return fmt.Errorf("usage: settings <get|set> [key=value ...]")
	}
	req := map[string]any{}
	for _, kv := range args[1:] {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid key=value pair %q", kv)
		}
		key, val := parts[0], parts[1]
		// Most UpdateSettingsRequest fields are plain strings, but a few
		// aren't - sending those as a bare string (e.g. {"snmp_enabled":
		// "true"}) fails server-side JSON unmarshaling into *bool/*int.
		switch key {
		case "snmp_enabled", "offsite_location":
			b, err := strconv.ParseBool(val)
			if err != nil {
				return fmt.Errorf("invalid boolean for %q: %w", key, err)
			}
			req[key] = b
		case "offsite_rotation_count":
			n, err := strconv.Atoi(val)
			if err != nil {
				return fmt.Errorf("invalid integer for %q: %w", key, err)
			}
			req[key] = n
		case "snmp_targets":
			return fmt.Errorf("snmp_targets is a list, not settable via key=value - use the web UI (Admin -> Settings) or a direct PUT /api/v1/settings call")
		default:
			req[key] = val
		}
	}
	out, err := c.UpdateSettings(req)
	if err != nil {
		return err
	}
	return printResult(out, true, func() {})
}

// setLatencyField applies one runLatency "set key=value" pair to ls, kept
// as an explicit switch (not reflection) since PUT /api/v1/settings/latency
// takes the whole struct, unlike runSettings' generic partial-map pattern.
func setLatencyField(ls *config.LatencySettings, key, val string) error {
	switch key {
	case "enabled":
		ls.Enabled = val == "true"
	case "drive_load":
		ls.DriveLoad = val
	case "drive_unload":
		ls.DriveUnload = val
	case "tape_positioning":
		ls.TapePositioning = val
	case "robot_move_tape":
		ls.RobotMoveTape = val
	case "robot_move_scan":
		ls.RobotMoveScan = val
	case "magazine_scan":
		ls.MagazineScan = val
	case "door_action":
		ls.DoorAction = val
	default:
		return fmt.Errorf("unknown latency field %q (expected one of: enabled, drive_load, drive_unload, tape_positioning, robot_move_tape, robot_move_scan, magazine_scan, door_action)", key)
	}
	return nil
}

func runLatency(c *apiclient.Client, args []string, jsonOut bool) error {
	if len(args) == 0 || args[0] == "get" {
		out, err := c.GetLatencySettings()
		if err != nil {
			return err
		}
		return printResult(out, true, func() {})
	}
	switch args[0] {
	case "set":
		cur, err := c.GetLatencySettings()
		if err != nil {
			return err
		}
		ls := cur.Settings
		for _, kv := range args[1:] {
			parts := strings.SplitN(kv, "=", 2)
			if len(parts) != 2 {
				return fmt.Errorf("invalid key=value pair %q", kv)
			}
			if err := setLatencyField(&ls, parts[0], parts[1]); err != nil {
				return err
			}
		}
		out, err := c.UpdateLatencySettings(ls)
		if err != nil {
			return err
		}
		return printResult(out, true, func() {})
	case "reset":
		// The one CLI place a "reset" immediately persists (unlike the
		// Admin UI's "Load defaults", which only prefills the form for
		// review) - there's no form/review step to skip on a CLI.
		cur, err := c.GetLatencySettings()
		if err != nil {
			return err
		}
		out, err := c.UpdateLatencySettings(cur.Defaults)
		if err != nil {
			return err
		}
		return printResult(out, true, func() {})
	default:
		return fmt.Errorf("usage: latency <get|set|reset> [key=value ...]")
	}
}

func setCleaningField(cs *config.CleaningSettings, key, val string) error {
	switch key {
	case "enabled":
		cs.Enabled = val == "true"
	case "mode":
		cs.Mode = val
	case "max_uses":
		n, err := strconv.Atoi(val)
		if err != nil {
			return fmt.Errorf("max_uses: %w", err)
		}
		cs.MaxUses = n
	case "mount_threshold":
		n, err := strconv.Atoi(val)
		if err != nil {
			return fmt.Errorf("mount_threshold: %w", err)
		}
		cs.MountThreshold = n
	case "duration":
		cs.Duration = val
	default:
		return fmt.Errorf("unknown cleaning field %q (expected one of: enabled, mode, max_uses, mount_threshold, duration)", key)
	}
	return nil
}

// runCleaning has no "trigger" subcommand: manual cleaning is triggered
// simply by loading a cleaning cartridge into a drive (the existing
// "load" subcommand), the same generic action used for any volume - see
// Library.Load's doc comment.
func runCleaning(c *apiclient.Client, args []string, jsonOut bool) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cleaning <settings|tape> ...")
	}
	switch args[0] {
	case "settings":
		return runCleaningSettings(c, args[1:])
	case "tape":
		return runCleaningTape(c, args[1:])
	default:
		return fmt.Errorf("usage: cleaning <settings|tape> ...")
	}
}

func runCleaningSettings(c *apiclient.Client, args []string) error {
	if len(args) == 0 || args[0] == "get" {
		out, err := c.GetCleaningSettings()
		if err != nil {
			return err
		}
		return printResult(out, true, func() {})
	}
	switch args[0] {
	case "set":
		cur, err := c.GetCleaningSettings()
		if err != nil {
			return err
		}
		cs := cur.Settings
		for _, kv := range args[1:] {
			parts := strings.SplitN(kv, "=", 2)
			if len(parts) != 2 {
				return fmt.Errorf("invalid key=value pair %q", kv)
			}
			if err := setCleaningField(&cs, parts[0], parts[1]); err != nil {
				return err
			}
		}
		out, err := c.UpdateCleaningSettings(cs)
		if err != nil {
			return err
		}
		return printResult(out, true, func() {})
	case "reset":
		// The one CLI place a "reset" immediately persists (see
		// runLatency's identical note) - there's no form/review step to
		// skip on a CLI.
		cur, err := c.GetCleaningSettings()
		if err != nil {
			return err
		}
		out, err := c.UpdateCleaningSettings(cur.Defaults)
		if err != nil {
			return err
		}
		return printResult(out, true, func() {})
	default:
		return fmt.Errorf("usage: cleaning settings <get|set|reset> [key=value ...]")
	}
}

func runCleaningTape(c *apiclient.Client, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cleaning tape <new|list>")
	}
	switch args[0] {
	case "new":
		out, err := c.CreateCleaningTape()
		if err != nil {
			return err
		}
		return printResult(out, true, func() {})
	case "list":
		out, err := c.ListCleaningTapes()
		if err != nil {
			return err
		}
		return printResult(out, true, func() {})
	default:
		return fmt.Errorf("usage: cleaning tape <new|list>")
	}
}

func runRoboticFault(c *apiclient.Client, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: robotic-fault <on|off> [kind] [message]")
	}
	switch args[0] {
	case "on":
		if len(args) < 2 {
			return fmt.Errorf("usage: robotic-fault on <kind> [message]")
		}
		message := ""
		if len(args) > 2 {
			message = strings.Join(args[2:], " ")
		}
		return c.RoboticFault(true, args[1], message)
	case "off":
		return c.RoboticFault(false, "", "")
	default:
		return fmt.Errorf("usage: robotic-fault <on|off> [kind] [message]")
	}
}

func printStatus(st library.Status) {
	if st.RoboticFault.Active {
		fmt.Printf("Robotic arm: FAULT (kind=%s)\n", st.RoboticFault.Kind)
	} else {
		fmt.Println("Robotic arm: ok")
	}
	fmt.Println("Drives:")
	for _, d := range st.Drives {
		switch {
		case d.Fault:
			fmt.Printf("  drive %-3d FAULT\n", d.Index)
		case d.Volume != nil:
			fmt.Printf("  drive %-3d loaded: %s\n", d.Index, d.Volume.Barcode)
		default:
			fmt.Printf("  drive %-3d empty\n", d.Index)
		}
	}
	fmt.Println("IO slots:")
	for _, io := range st.IOSlots {
		if io.Volume != nil {
			fmt.Printf("  slot %d %s\n", io.Address, io.Volume.Barcode)
		} else {
			fmt.Printf("  slot %d empty\n", io.Address)
		}
	}
	fmt.Println("Storage slots:")
	for _, s := range st.Slots {
		if s.Volume != nil {
			fmt.Printf("  slot %-3d %s\n", s.Address, s.Volume.Barcode)
		} else {
			fmt.Printf("  slot %-3d empty\n", s.Address)
		}
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `usage: gotochangerctl [--socket path | --url url --token tok] [--json] [--logical-library name] <subcommand> [args...]

subcommands:
  status
  events
  volumes
	outside
  load <slot|ioslot> <address> <drive>
  unload <drive> <slot|ioslot> <address>
  move <slot|ioslot> <address> <slot|ioslot> <address>
	outside-delete <barcode>
	io-door <mailbox-id> open [pin] | io-door <mailbox-id> close [actions-json]
	storage-door <magazine-id> open [pin] | storage-door <magazine-id> close [actions-json]
  fault <drive> <on|off>
  write-protect <barcode> <on|off>
  robotic-fault on <kind> [message]
  robotic-fault off
  token new|revoke|list [name] [role]
  user new|list|delete|role|reset-password ...
  settings get
	settings set <key>=<value> [key=value ...]
  latency get
	latency set <key>=<value> [key=value ...]
	latency reset
  logical-library new|list|show|update|delete ...
  drive-type new|list|update|delete ...
  tape-type new|list|update|delete ...
  tape-set new|list|update|delete|add-tapes|add-tape ...
  magazine new|list|update|delete ...
  mailbox new|list|update|delete ...
  drive new|list|update|delete ...
  unassigned
  offsite list|send|recall ...
  backup download <output-file>
  backup list
  backup download-stored <filename> <output-file>
  backup delete <filename>
  backup schedule show
  backup schedule set [interval=<duration>] [retention=<n>]
  restore <file>
  reset <confirm-name> [--delete-volumes]
  wizard status

note: ioslot addresses are global contiguous element addresses (after storage slots)
note: --logical-library scopes load/unload/move/status to one logical library`)
}

func printResult(v any, asJSON bool, human func()) error {
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	human()
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
