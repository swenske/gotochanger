# Migrate from disk-changer.in

You have an existing Bareos Storage Daemon using `disk-changer.in` as its Changer Command, with one or more
`Device = File` resources pointed at plain files. gotochanger stays compatible with that same Device
configuration, so the migration only touches the Autochanger's `Changer Command` line and how the archive
device paths get populated - Bareos's own catalog (volume names, pools, retention) is untouched.

## Prerequisites

- The existing `disk-changer.in`-based Autochanger's Device count and current `Archive Device` paths (e.g.
  `/etc/bareos/scripts/disk-changer.conf` or the Device resources themselves).
- gotochanger installed (see [Installation](#installation)) but not yet configured (fresh wizard state).
- `bareos` added to the `gotochanger` group so it can reach the trusted socket and the volume files:
  `sudo adduser bareos gotochanger`.

## Steps

1. Note how many drives and how many total storage slots the existing setup uses - `disk-changer.in`'s own
   config typically has this as a slot count and a directory of volume files.
2. Run through the [setup wizard](#first-run-and-setup-wizard), creating one magazine (call its ID `mag1`)
   sized to match your existing slot count (round up to the nearest multiple of 5), one drive per existing
   Device resource, and one tape set for your existing media. Skip mailboxes/logical libraries for now if you
   just want the fastest path back to a working Autochanger - you can add them later without disrupting
   Bareos.
3. For each existing Device resource, edit only the `Changer Command` line:
   ```
   Changer Command = "/usr/bin/gotochanger-changer %c %o %S %a %d %V"
   ```
   Leave `Archive Device`, `Media Type`, `Device Type = File`, `AutomaticMount`, and `RemovableMedia`
   untouched.
4. Register each existing volume as an outside-library tape under the tape set you just created, using its
   real existing barcode, then bring it straight into a storage slot through that magazine's storage door -
   this is the same open/queue/close pattern the dashboard's "Bulk load..." button uses:
   ```sh
   gotochangerctl tape-set add-tape <tape-set-name> <existing-barcode>
   gotochangerctl storage-door mag1 open
   gotochangerctl storage-door mag1 close '[{"action":"load","address":1,"barcode":"<existing-barcode>"}]'
   ```
   Repeat the last line (with the next free storage-slot address) for each existing volume.
5. Restart `bareos-sd`, then verify Bareos still sees the expected slot/drive count:
   ```sh
   gotochanger-changer /dev/null slots
   gotochanger-changer /dev/null listall
   ```

## Verify

```sh
bconsole <<'EOF'
status storage=FakeML3 slots
EOF
```

Expect the same total slot count Bareos reported before the migration, and `gotochangerctl status` to show
the same barcodes racked in storage slots your old `disk-changer.in` setup had. Run a small backup/restore
job against one volume to confirm read/write still works end-to-end before decommissioning the old script.
