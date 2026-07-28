# Kernel Mode

By default gotochanger runs in **userspace/file mode**: loading a drive symlinks a plain file at the
configured `Archive Device` path, no root or kernel modules required. An optional **kernel mode** instead
exposes the same library as real SCSI devices (`/dev/sg*` for the changer/generic device, `/dev/nst*` for
tape drives) via TCMU/LIO (`target_core_user`), for tools that insist on talking to an actual SCSI medium
changer rather than a changer-script convention - no real tape hardware is involved either way, kernel mode
just adds a real *kernel device node* backed by the same plain files.

## When to enable it

Bareos itself never needs kernel mode - it talks to `gotochanger-changer` as a Changer Command script and
reads/writes plain files either way. Turn kernel mode on only when something else in your stack (a
third-party backup tool, a monitoring agent, a test harness) specifically requires a real `/dev/sg*`/`/dev/nst*`
device path rather than a script-driven changer convention. See
[Switch to kernel mode for a third-party SCSI tool](#switch-to-kernel-mode-for-a-third-party-scsi-tool) for a
full worked example.

## Enabling it

1. Install the separate `gotochanger-kernel` package (depends on `gotochanger` and `polkitd`). It needs root
   and the `target_core_user` kernel module - the package's postinst does a best-effort `modprobe`.
2. Turn it on by setting the **operational mode** to `kernel` (setup wizard, or Admin > Settings /
   `gotochangerctl settings set operational_mode=kernel`). gotochangerd's own reconciler then automatically
   starts/stops one `gotochanger-tcmud@<logical-library-name>.service` instance per logical library
   (`@default` if the library is unscoped) via polkit-authorized systemd calls - no manual `systemctl enable`
   step is required, though it's supported (the Admin UI's per-library "Kernel Mode Setup" dialog shows the
   equivalent manual `systemctl enable --now gotochanger-tcmud@<instance>` command for cases where automatic
   management isn't wanted).
3. Real devices then appear under `/dev/sg*`/`/dev/nst*`. Prefer the stable
   `/dev/tape/by-id/scsi-<NAA>[-nst]` symlinks over raw `/dev/sgN`/`/dev/nstN` numbers, which are **not**
   stable across a `gotochanger-tcmud` restart. Admin > Drives and the Bareos-Config generator button both
   show the actual current device paths.
4. Point the third-party tool's device configuration at those real device paths instead of the file-based
   ones - for Bareos specifically, no other config-file syntax change is needed either way.

Device paths are tracked in memory on the gotochangerd side (a running `gotochanger-tcmud` self-reports them
at startup via `POST /api/v1/kernel-mode/devices/{instance}`) and are lost on a gotochangerd restart until the
`gotochanger-tcmud` instance itself restarts.

## Scope and limitations

- **Drive bandwidth throttling only applies in kernel mode.** In the default userspace/file mode, a loaded
  drive is a symlink at the configured device path; the consuming application writes directly to that file
  and gotochangerd never sees the byte stream, so there's nothing to throttle. In kernel mode,
  `gotochanger-tcmud` sits directly in the SCSI I/O path and does throttle reads/writes to the assigned drive
  type's configured native speed.
- Backstore/WWN names are prefixed with the instance name (the logical library name, or `default`) to avoid
  kernel-level name collisions between concurrent `gotochanger-tcmud` instances.
- `gotochanger-tcmud` requires root and is not started or enabled automatically just by installing the
  package - only once operational mode is actually set to `kernel`.

See `gotochangerctl` status commands and `GET /api/v1/kernel-mode/status` / `GET /api/v1/kernel-mode/devices`
to check whether the kernel module and any running instances are currently available.
