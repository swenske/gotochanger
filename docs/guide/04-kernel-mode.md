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

## Reporting a real vendor/product SCSI identity

By default every kernel-mode device reports gotochanger's own identity (`GOTOCHNG`/`Virtual LTO-9`/
`Virtual Changer`, etc.) in its SCSI INQUIRY response - fine for Bareos and most third-party tools, which
don't care. A tool that whitelists specific real vendor/product strings needs the real thing instead, so
this is opt-in per catalog entry:

- **Drives**: set a Drive Type's **SCSI Identity** to *Realistic* - Admin > Drive Types' New/Edit dialog,
  `gotochangerctl drive-type new|update ... --scsi-identity realistic`, or `scsi_identity: realistic` via
  the API. Only defined for `LTO-8`/`LTO-9` generations today (reports as a real `IBM ULT3580-TD8`/`-TD9`)
  - any other generation stays on the default identity even with this set.
- **Changer**: set a logical library's **Changer Model** to *Realistic* - Admin > Logical Libraries'
  New/Edit dialog, `gotochangerctl logical-library new|update ... --changer-model realistic`, or
  `changer_model: realistic` via the API. Reports as an `STK SL150` - the same real device this project's
  own SMC-3 command layout was verified against (see the Oracle StorageTek SL150 SCSI Reference Guide,
  cited throughout `internal/scsi`). Only takes effect for a `gotochanger-tcmud` instance scoped to that
  logical library (`--logical-library`); an unscoped instance has no logical library to read this setting
  from and always reports the default identity.

Changing either setting takes effect the next time the affected `gotochanger-tcmud` instance (re)starts, not
live against an already-running device.

## Multi-partition tapes (kernel mode only)

A drive in kernel mode can format a mounted volume with two SSC partitions instead of one - the layout
LTFS itself needs (a small index partition plus a large data partition). Nothing in userspace/file mode or
the Admin UI creates a second partition; it's set purely via real SCSI commands (`MODE SELECT` staging a
partition count via the Medium Partition mode page, `FORMAT MEDIUM` applying it, `LOCATE(16)` or
`LOCATE(10)`'s CP bit switching between partitions). Each partition beyond the first is a fully independent
backing file next to the volume's own (`<path>.p1`), so data written to one partition never leaks into the
other. Only two partitions total are supported - enough for LTFS's own convention, not arbitrary
partitioning. The resulting partition count is visible read-only wherever the Admin UI/dashboard shows that
cartridge, as a "2P" badge on its card (hover the card for the full detail line).

## Cartridge memory (MAM) attributes (kernel mode only)

A drive in kernel mode answers real SCSI READ ATTRIBUTE/WRITE ATTRIBUTE commands against the mounted
volume's MAM (Medium Auxiliary Memory) - the small chip embedded in a real cartridge that stores its own
identity, capacity, and application-set metadata. Only a focused subset of the full T10 attribute table is
implemented: remaining/maximum capacity, TapeAlert flags, load count, and volume identifier (all read-only,
derived from state gotochanger already tracks), plus application vendor/name/version and a user medium text
label (read/write - genuinely persisted on the volume, surviving unmount/remount, settable only via a real
WRITE ATTRIBUTE command). Verify with `sg_read_attr`/`sg_write_attr` (sg3-utils) against the drive's
`/dev/sg*` device. Load count and the mutable attributes above are visible read-only in the Admin UI/
dashboard, in the hover tooltip on that cartridge's card.

## Real tape encryption (kernel mode only)

A drive in kernel mode implements real SCSI Security Protocol In/Out (SPIN/SPOUT) tape encryption - the same
"Tape Data Encryption" protocol real LTO encrypting drives and key-manager software (e.g. `stenc`) use.
Setting an AES-256 key and turning encryption on (via `stenc -e on -k <keyfile>`, or any T10-compliant key
manager) makes every subsequent `WRITE(6)` genuinely AES-256-GCM-encrypt its data before it touches the
backing file, and every `READ(6)` genuinely decrypt it back - not a protocol-only stub. The key is
**session-scoped**: it must be re-supplied after every drive load/unload or `gotochanger-tcmud` restart,
exactly like real hardware - there is nothing to configure in advance, and no key is ever persisted by
gotochanger itself. Reading previously-encrypted data with the wrong key (or no key at all) correctly fails
with a `Data Protect`/`Logical Unit Access Not Authorized` SCSI error rather than returning garbage.
Encryption is decided once, at the start of a fresh recording pass (BOT) - this project has no concept of
part-encrypted, part-plain data on one volume. Once set, the volume's encrypted flag is visible read-only
wherever the Admin UI/dashboard shows that cartridge, as an "Encrypted" badge on its card.

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
