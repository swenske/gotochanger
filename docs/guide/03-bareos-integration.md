# Bareos Integration

Point Bareos's Device resource at the changer shim exactly like `disk-changer.in`, and set `Device Type =
File` with `Archive Device` matching the corresponding entry in `library.drive_devices`.

## Autochanger and Device resources

```
Autochanger {
  Name = FakeML3
  Device = Drive0, Drive1
  Changer Device = /dev/null              # unused, kept for compatibility
  Changer Command = "/usr/bin/gotochanger-changer %c %o %S %a %d %V"
}

Device {
  Name = Drive0
  Drive Index = 0                         # required with 2+ drives - see below
  Media Type = File
  Archive Device = /var/lib/gotochanger/drives/drive0
  Device Type = File
  AutomaticMount = yes
  RemovableMedia = yes
  AutoChanger = yes
}

Device {
  Name = Drive1
  Drive Index = 1
  Media Type = File
  Archive Device = /var/lib/gotochanger/drives/drive1
  Device Type = File
  AutomaticMount = yes
  RemovableMedia = yes
  AutoChanger = yes
}
```

Add the `bareos` system user to the `gotochanger` group so it can reach the trusted local socket and read/
write volume files:

```sh
sudo adduser bareos gotochanger
```

Supported changer commands (matching `disk-changer.in`): `load`, `unload`, `list`, `listall`, `slots`,
`loaded`, `transfer`. Extra commands usable by hand (never invoked by Bareos itself): `outside`,
`outside-delete`, `io-door`, `storage-door`, `ioslots`, `offsite-send`, `offsite-recall`.

<div class="callout callout-warn">
<svg class="icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3l10 18H2L12 3z"/><path d="M12 10v4M12 17.5h.01"/></svg>
<div><strong>The Drive Index trap.</strong> <code>Drive Index</code> defaults to <code>0</code> for any
Device resource that doesn't set it explicitly - with only one drive that's harmless, but with two or more it
means every drive silently collapses to "drive 0" from Bareos's point of view (regardless of the Device's
<code>Name</code>/<code>Archive Device</code>), so it's required as soon as an Autochanger has more than one
Device. Set it to the drive's 0-based position within <em>this</em> Autochanger's own <code>Device =</code>
list, not gotochangerd's own drive index (they happen to match here only because both start at 0 and are
contiguous). See the
<a href="#configure-multiple-drives-without-the-drive-index-trap">Configure multiple drives</a> cookbook
scenario for a worked example, including how to catch this if it's already happened to you.</div>
</div>

The Admin UI's Logical Libraries "Bareos Config" button generates this block correctly, including
`Drive Index`, for whatever drives are actually assigned to that logical library - the fastest way to get a
correct config skeleton without hand-counting indices.

## Scoping an Autochanger to one logical library

If the physical library is partitioned into multiple logical libraries, add a static
`--logical-library=NAME` flag to that Autochanger resource's `Changer Command` line (Bareos has no
substitution variable for this, so it's a fixed per-Autochanger suffix - the Autochanger is already
permanently bound to one logical library):

```
Changer Command = "/usr/bin/gotochanger-changer %c %o %S %a %d %V --logical-library=Library1"
```

With this set, `load`/`unload`/`move` (and their `X-Logical-Library` REST/CLI equivalents) are rejected with
an error if the addressed slot, I/O slot, or drive doesn't belong to `Library1` - this is what keeps two
Bareos Autochangers sharing one physical gotochanger instance from touching each other's media. See
[Partition one physical robot into two logical libraries](#partition-one-physical-robot-into-two-logical-libraries)
for a full worked example with two separate Bareos Storage Daemons.

## Migrating from `disk-changer.in`

gotochanger is designed as a drop-in replacement: it stays compatible with existing `Device Type = File`
Bareos configurations, so an existing `disk-changer.in`-based Autochanger can usually switch over by changing
only the `Changer Command` line - Device/Media Type/Archive Device stay the same. See
[Migrate from disk-changer.in](#migrate-from-disk-changerin) for the full step-by-step migration, including
how to verify Bareos still sees the same volumes in the same slots afterward.
