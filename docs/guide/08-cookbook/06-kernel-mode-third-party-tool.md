# Switch to kernel mode for a third-party SCSI tool

A monitoring agent, a test harness, or some other tool in your stack insists on talking to a real SCSI medium
changer (`/dev/sg*`) and tape drives (`/dev/nst*`) rather than a changer-script convention. Bareos itself never
needs this - only enable kernel mode if something else genuinely requires a real kernel device node.

## Prerequisites

- The `gotochanger-kernel` package installed (`sudo apt-get install gotochanger-kernel`).
- Root access on the host (kernel mode needs the `target_core_user` kernel module and configfs).
- An existing logical library (or the whole physical library, unscoped) you want to expose.

## Steps

1. Switch operational mode to `kernel`:
   ```sh
   gotochangerctl settings set operational_mode=kernel
   ```
2. gotochangerd's reconciler automatically starts one `gotochanger-tcmud@<logical-library-name>.service`
   instance per logical library (`gotochanger-tcmud@default.service` if the library is unscoped), via
   polkit-authorized systemd calls - no manual `systemctl enable` step is required. Confirm it's running:
   ```sh
   systemctl status 'gotochanger-tcmud@*.service'
   ```
3. Check which real devices came up:
   ```sh
   curl --unix-socket /run/gotochanger/gotochanger.sock http://localhost/api/v1/kernel-mode/status | jq .
   curl --unix-socket /run/gotochanger/gotochanger.sock http://localhost/api/v1/kernel-mode/devices | jq .
   ```
   The Admin UI's Drives page and each logical library's "Kernel Mode Setup" dialog show the same device
   paths, plus the equivalent manual `systemctl enable --now gotochanger-tcmud@<instance>` command if you'd
   rather manage the instance yourself instead of relying on the automatic reconciler.
4. Find the stable device symlinks - **prefer these over raw `/dev/sgN`/`/dev/nstN` numbers**, which are not
   stable across a `gotochanger-tcmud` restart:
   ```sh
   ls -l /dev/tape/by-id/
   ```
5. Point your third-party tool's device configuration at the `scsi-<NAA>` (changer) or `scsi-<NAA>-nst`
   (drive) symlink instead of the raw device number.

## Verify

```sh
sg_inq /dev/tape/by-id/scsi-<NAA>
mt -f /dev/tape/by-id/scsi-<NAA>-nst status
```

Expect a real SCSI INQUIRY response and tape-drive status output, backed by the same plain files gotochanger
already manages - loading a volume through `gotochangerctl load`/the dashboard should make it visible at that
same device path within a few seconds. If the third-party tool reports the device is gone after a
`gotochanger-tcmud` restart, check `/dev/tape/by-id/` again rather than a previously-noted `/dev/sgN` number -
that's exactly the instability the by-id symlinks exist to avoid.
