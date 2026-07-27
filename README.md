# gotochanger

A fake SCSI tape autochanger (virtual library) simulator for testing backup
software against something that behaves like a real tape library — storage
slots, I/O ("mail slot") elements, multiple drives, volumes, robot moves,
and SNMP traps — without any real tape hardware.

It is meant as a drop-in, much more capable replacement for Bareos's
`disk-changer.in`, adding I/O slots, a REST API, a management web UI, and
SNMP notifications, while remaining compatible with existing
`Device Type = File` Bareos configurations.

## Components

| Binary                 | Purpose                                                                 |
|------------------------|--------------------------------------------------------------------------|
| `gotochangerd`         | The daemon: library state, REST API, embedded web UI, SNMP traps, setup wizard.        |
| `gotochanger-changer`  | Drop-in replacement for Bareos's `disk-changer.in` "Changer Command".     |
| `gotochangerctl`       | General purpose admin CLI (status, load/unload/move, volumes, tokens).   |

## Concepts

Modeled after SCSI Medium Changer (SMC) element types:

- **Storage slots** — home locations for cartridges (`slot:N`), addressed
  `1..slots`.
- **I/O slots** (`ioslot:N`) — "mail slots" used to physically load/pickup
  media through door-gated operator actions.
  Addressed contiguously right after the storage slots (e.g. slots 1..20
  followed by I/O slots 21..24) exactly like a real SCSI medium changer and
  mtx/mtx-changer's "N Slots (M Import/Export)" convention — not a
  separate address space starting back at 1. This matters for Bareos,
  which learns the *total* addressable element count from the changer's
  `slots` command and maps individual addresses to regular or
  import/export slots solely by which range they fall in. gotochangerctl
  and the web UI display I/O slot addresses with a trailing `@` (e.g.
  `21@`), the same convention used by real tape library tooling.
- **Mailboxes** — named, independently addressable groups of I/O slots
  (1-5 slots each), the I/O-slot equivalent of a magazine. I/O slots
  aren't configured as a flat count; they belong to a mailbox, and logical
  libraries assign whole mailboxes (by ID), not individual I/O slot
  addresses.
- **Magazines** — named, independently addressable groups of storage slots
  (5-20 slots, in increments of 5).
- **Drives** (`drive:N`) — data transfer elements. Loading a drive symlinks
  its configured device path (Bareos's "Archive Device") to the volume's
  backing file, exactly like `disk-changer.in` did.
- **Volumes** are plain files under `<data_dir>/volumes/`, growable up to a
  configured capacity; once a loaded volume's file reaches that capacity it
  is marked full and made read-only, simulating end-of-tape.
- **Logical libraries** — a named partition of the physical library (like a
  Dell ML3), each with its own subset of drives/magazines/mailboxes and a
  color for the dashboard. `Load`/`Unload`/`Move` reject any element
  outside the logical library named in the `X-Logical-Library` header (or
  `gotochanger-changer --logical-library=NAME` / `gotochangerctl
  --logical-library`); an element can belong to at most one logical
  library at a time.
- **Tape sets** — named groups of cartridges by tape/media type (LTOx,
  DDSx, DLTxxxx — tracked separately from drive hardware types), each
  stored under its own folder on disk.

Library topology (a VTL name, drive/tape types, magazines, mailboxes,
drive devices, logical libraries, tape sets) lives in a SQLite database at
`<data_dir>/state.db`, populated by the setup wizard or the Admin API —
**not** in `config.yaml`, which only holds service-level settings
(data dir, listeners, SNMP, poll interval, log level). A fresh install has
no drives, magazines, or mailboxes configured at all until the wizard runs.

## Quick start

```sh
sudo apt install ./gotochanger_0.2.0_amd64.deb
sudo systemctl status gotochanger
journalctl -u gotochanger | grep 'bootstrap API token'   # save this token!
```

Open the web UI at `http://127.0.0.1:8480/`. On first visit you'll be asked
to set a password for the built-in **Admin** account (username `Admin`),
then — since a fresh install starts with nothing configured — you'll be
guided through the setup wizard to name your VTL and create your drives,
magazines, mailboxes, an optional offsite location, tape sets (optionally
auto-generating N labeled cartridges per set), at least one logical
library, and a latency profile. Finishing the wizard applies the new
topology to the running daemon immediately, no restart needed.
From there, use the **Admin** section to manage users/tokens, the drive/tape
type catalogs, tape sets, magazines, mailboxes, and logical libraries; or
drive everything from the CLI:

```sh
# gotochangerctl talks to the trusted local Unix socket by default, no token needed
# example below assumes 20 storage slots and 4 I/O slots addressed as 21..24
gotochangerctl status
gotochangerctl outside-create Vol0001 BC0001 10GiB # create a tape outside the library
gotochangerctl io-door open                         # open mail slot door
gotochangerctl io-door close '[{"action":"load","address":21,"label":"Vol0001"}]'
gotochangerctl move ioslot 21 slot 1               # robot move inside the library
gotochangerctl load slot 1 0                        # load slot 1 into drive 0
gotochangerctl unload 0 slot 1                      # unload drive 0 back to slot 1
gotochangerctl events
```

## Users, roles and API tokens

The web UI is protected by username/password sessions; the REST API also
accepts scoped API tokens (for scripts/automation) or is reachable
unauthenticated over the trusted local Unix socket (used by
`gotochanger-changer`/`gotochangerctl` for Bareos integration).

Three roles, in increasing order of privilege:

| Role       | Can do                                                              |
|------------|----------------------------------------------------------------------|
| `viewer`   | Read-only: status, events, volumes list                              |
| `operator` | Everything a viewer can, plus load/unload/move/door operations/outside tape create-delete/drive fault |
| `admin`    | Everything, plus the Admin section: users, tokens, settings          |

The built-in `Admin` account must have its password set on first visit to
the web UI (enforced password policy: 12+ characters, at least 3 of
lowercase/uppercase/digit/special character, not the username, not a common
password; accounts lock out for 15 minutes after 5 failed attempts).

From the **Admin** section (admin role required) you can:

- **Users**: create accounts (username + role + initial password, which
  must be changed at next login), change roles, reset passwords, delete
  accounts (the last remaining admin cannot be demoted or deleted).
- **API Tokens**: create/revoke tokens scoped to a role, used by scripts
  via the `X-Api-Key` header or `Authorization: Bearer <token>`.
- **Drive Types** / **Tape Types**: manage the suggested catalogs (add,
  update, remove) — LTO-8, LTO-9, DDS, DLT, and an Unlimited
  capacity/performance model are seeded on first start.
- **Tape Sets**: create/edit/delete named groups of cartridges by tape
  type, each with its own storage folder.
- **Magazines**: create/edit/delete slot groups (5-20 slots, in increments
  of 5). Changes hot-apply immediately — no restart needed. Deleting a
  magazine that still has volumes in its slots is refused.
- **Mailboxes**: create/edit/delete I/O slot groups (1-5 slots each),
  the I/O-slot equivalent of a magazine. Changes hot-apply immediately.
  Deleting a mailbox that still has volumes in its slots is refused.
- **Logical Libraries**: create/edit/delete named partitions of the
  library, assigning drives/magazines/mailboxes and a display color. An
  element can only belong to one logical library at a time (rejected with
  409 otherwise); unassigned drives/slots/mailboxes are listed in the same
  screen for reassignment.
- **Settings**: view/change the parts of configuration that can safely
  apply without a restart (default volume capacity, barcodes, poll
  interval, log level, SNMP, latency profile, offsite rotation schedule).
  Only `data_dir`, `tokens_file`, and `listen` require editing
  `/etc/gotochanger/config.yaml` plus `systemctl restart gotochanger`.

Equivalent CLI commands (`gotochangerctl`, talking to the trusted socket by
default so no token/login is needed locally):

```sh
gotochangerctl user new op1 operator 'Op3rat0r$InitialPass1'
gotochangerctl user list
gotochangerctl user role op1 viewer
gotochangerctl user reset-password op1 'N3wPassword$Here'
gotochangerctl token new ci-bot operator
gotochangerctl token list
gotochangerctl settings get
gotochangerctl settings set poll_interval=10s log_level=debug

gotochangerctl drive-type new LTO-10 500MB/s 24TB "LTO-10 tape drive"
gotochangerctl tape-type list
gotochangerctl tape-set new Archive2026 LTO-9 /var/lib/gotochanger/tapesets/Archive2026
gotochangerctl magazine new Magazine3 10
gotochangerctl mailbox new Mailbox2 3
gotochangerctl logical-library new Library2 "" "" ""   # empty CSVs = no elements yet
gotochangerctl logical-library update Library2 '#FF8800' 2,3 Magazine3 Mailbox2
gotochangerctl unassigned
gotochangerctl offsite send slot 4
gotochangerctl offsite recall Vol0004 slot 4
gotochangerctl wizard status
```

## Bareos integration

Point Bareos's Device resource at the changer shim exactly like
`disk-changer.in`, and set `Device Type = File` with `Archive Device`
matching the corresponding entry in `library.drive_devices` in
`/etc/gotochanger/config.yaml`:

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

`Drive Index` defaults to `0` for any Device resource that doesn't set it
explicitly - with only one drive that's harmless, but with two or more it
means every drive silently collapses to "drive 0" from Bareos's point of
view (regardless of the Device's `Name`/`Archive Device`), so it's
required as soon as an Autochanger has more than one Device. Set it to
the drive's 0-based position within *this* Autochanger's own `Device =`
list, not gotochangerd's own drive index (they happen to match here only
because both start at 0 and are contiguous). The Admin UI's Logical
Libraries "Bareos Config" button generates this correctly, including
`Drive Index`, for whatever drives are actually assigned to that logical
library.

Add the `bareos` system user to the `gotochanger` group so it can reach the
trusted local socket and read/write volume files:

```sh
sudo adduser bareos gotochanger
```

Supported changer commands (matching `disk-changer.in`): `load`, `unload`,
`list`, `listall`, `slots`, `loaded`, `transfer`. Extra commands usable by
hand (never invoked by Bareos itself): `outside`, `outside-create`,
`outside-delete`, `io-door`, `storage-door`, `ioslots`, `offsite-send`,
`offsite-recall`.

### Scoping an Autochanger to one logical library

If the physical library is partitioned into multiple logical libraries,
add a static `--logical-library=NAME` flag to that Autochanger resource's
`Changer Command` line (Bareos has no substitution variable for this, so
it's a fixed per-Autochanger suffix — the Autochanger is already
permanently bound to one logical library):

```
Changer Command = "/usr/bin/gotochanger-changer %c %o %S %a %d %V --logical-library=Library1"
```

With this set, `load`/`unload`/`move` (and their `X-Logical-Library`
REST/CLI equivalents) are rejected with an error if the addressed slot,
I/O slot, or drive doesn't belong to `Library1` — this is what keeps two
Bareos Autochangers sharing one physical gotochanger instance from
touching each other's media.

## REST API

All actions are available over HTTP. The Unix socket
(`/run/gotochanger/gotochanger.sock`) is trusted (every request is treated
as an admin, access controlled by filesystem permissions); the TCP
listener accepts either a browser session cookie or an API token
(`X-Api-Key` header or `Authorization: Bearer <token>`), each carrying one
of the `admin`/`operator`/`viewer` roles described above.

| Method | Path                          | Role      | Purpose                          |
|--------|-------------------------------|-----------|-----------------------------------|
| POST   | `/api/v1/auth/bootstrap`       | none      | Set the initial Admin password    |
| POST   | `/api/v1/auth/login`           | none      | Log in, start a session           |
| POST   | `/api/v1/auth/logout`          | viewer+   | Log out                           |
| GET    | `/api/v1/auth/state`           | none      | Am I logged in? Is bootstrap needed?|
| POST   | `/api/v1/auth/change-password` | viewer+   | Change your own password          |
| GET    | `/api/v1/status`               | viewer+   | Full library snapshot             |
| GET    | `/api/v1/events`                | viewer+   | Recent activity log               |
| GET    | `/api/v1/volumes`               | viewer+   | List all volumes                  |
| GET    | `/api/v1/outside`               | viewer+   | List outside-library tapes        |
| POST   | `/api/v1/outside`               | operator+ | Create outside-library tape       |
| DELETE | `/api/v1/outside/{label}`       | operator+ | Delete outside-library tape       |
| POST   | `/api/v1/load`                  | operator+ | Load a slot/ioslot into a drive   |
| POST   | `/api/v1/unload`                | operator+ | Unload a drive to a slot/ioslot   |
| POST   | `/api/v1/move`                  | operator+ | Move between slot/ioslot elements |
| POST   | `/api/v1/doors/io/open`         | operator+ | Open I/O door                     |
| POST   | `/api/v1/doors/io/close`        | operator+ | Close I/O door and process actions|
| POST   | `/api/v1/doors/storage/open`    | operator+ | Open storage door                 |
| POST   | `/api/v1/doors/storage/close`   | operator+ | Close storage door and process actions|
| POST   | `/api/v1/drives/{index}/fault`  | operator+ | Inject/clear a simulated drive fault|
| GET/POST/DELETE | `/api/v1/users`        | admin     | Manage user accounts              |
| GET/POST/DELETE | `/api/v1/tokens`       | admin     | Manage scoped API tokens          |
| GET/PUT | `/api/v1/settings`             | admin     | View/update application settings  |
| GET    | `/api/v1/wizard`               | none      | Current setup wizard state         |
| POST   | `/api/v1/wizard`               | none      | Submit one wizard step (persists immediately) |
| POST   | `/api/v1/wizard/complete`      | none      | Finish the wizard, hot-apply topology |
| POST   | `/api/v1/wizard/reset`         | none      | Reset wizard progress              |
| GET    | `/api/v1/wizard/options`       | none      | Catalogs + current state for the wizard UI |
| GET/POST/PUT/DELETE | `/api/v1/logical-libraries[/{name}]` | admin | Manage logical library partitions |
| GET    | `/api/v1/unassigned`           | admin     | Drives/slots/mailboxes in no logical library |
| GET/POST/PUT/DELETE | `/api/v1/drive-types[/{name}]` | admin | Manage the drive-type catalog     |
| GET/POST/PUT/DELETE | `/api/v1/tape-types[/{name}]`  | admin | Manage the tape/media-type catalog|
| GET/POST/PUT/DELETE | `/api/v1/tape-sets[/{name}]`   | admin | Manage tape sets (type + storage folder) |
| GET/POST/PUT/DELETE | `/api/v1/magazines[/{id}]`     | admin | Manage magazines (hot-applies)    |
| GET/POST/PUT/DELETE | `/api/v1/mailboxes[/{id}]`     | admin | Manage mailboxes (hot-applies)    |
| GET    | `/api/v1/offsite`               | viewer+   | List volumes in the offsite vault  |
| POST   | `/api/v1/offsite/send`          | operator+ | Send a volume to the offsite vault |
| POST   | `/api/v1/offsite/recall`        | operator+ | Recall a volume from the offsite vault |

`Load`/`Unload`/`Move` (and `GET /api/v1/status`) additionally accept an
`X-Logical-Library: NAME` header to scope the operation to one logical
library — see "Scoping an Autochanger to one logical library" above.

### Interactive API docs (Swagger UI)

A full OpenAPI 3.0 spec is served at `/api/v1/openapi.json` and rendered
as an interactive, embedded Swagger UI at **`/docs`** (linked from the web
UI's nav bar). No internet access is required to view it: the Swagger UI
assets are vendored and embedded in the binary. Use the "Authorize" button
with an API token, or just stay logged into the web UI in the same browser
(the session cookie is sent automatically for same-origin requests).

## SNMP traps

Disabled by default. Enable in `/etc/gotochanger/config.yaml`:

```yaml
snmp:
  enabled: true
  enterprise_oid: "1.3.6.1.4.1.55555.1"   # replace with a real IANA PEN if desired
  targets:
    - host: 192.0.2.10
      port: 162
      community: public
```

Every state-changing action emits:

- one activity-log event exposed via `/api/v1/events`;
- one SNMPv2c trap (when SNMP is enabled).

This includes both success and failure outcomes for robotics/media actions,
authentication actions (login/logout/bootstrap/password change), and
configuration actions (users/tokens/settings).

Events now use a structured code taxonomy inspired by enterprise tape-library
MIB conventions, for example:

- `ROBOTICS.LOAD.SUCCESS`
- `ROBOTICS.MOVE.FAILURE`
- `DRIVE.FAULT.SET.SUCCESS`
- `AUTH.LOGIN.FAILURE`
- `CONFIG.SETTINGS.UPDATE.SUCCESS`

Each event also carries `category`, `severity`, `outcome`, `operation`, and
`detail` fields so operators and NMS rules can classify behavior without
parsing free-text messages.

A dynamic MIB is served by the daemon at `/api/v1/snmp/mib` (viewer+ auth)
and is also linked from the web UI's Admin -> Settings -> SNMP traps panel.
The endpoint renders the MIB using the currently configured
`snmp.enterprise_oid`, so receivers decode the exact OIDs currently emitted.

### Event code quick reference

| Domain | Code prefix | Typical severity |
|--------|-------------|------------------|
| Robotics/media movement | `ROBOTICS.*`, `MEDIA.*` | `information` / `warning` |
| Drive state | `DRIVE.*` | `information` / `error` |
| Authentication | `AUTH.*` | `information` / `error` |
| Configuration/admin | `CONFIG.*` | `configuration` / `error` |
| Internal daemon failures | `SYSTEM.*` | `error` |

Outcome suffix convention:

- `*.SUCCESS` for completed actions
- `*.FAILURE` for rejected/failed actions
- `*.WARNING` for non-fatal alerts (for example simulated end-of-tape)

## Building from source

```sh
make build          # binaries in ./bin
make test
make install DESTDIR=/some/root   # used by debian/rules
```

## Building the Debian package

```sh
dpkg-buildpackage -us -uc -b
```

## Quick redeploy helper

For faster inner-loop testing, use the helper script:

```sh
scripts/redeploy.sh
```

It builds a fresh `.deb`, installs it with `dpkg -i --force-confold`, then
restarts the `gotochanger` service.

Remote redeploy example:

```sh
scripts/redeploy.sh --host bareos-sd01.example.com --user <ssh-user>
```

Dependencies are vendored (`vendor/`), so the build does not require
network access.

## Scope and limitations

gotochanger is a userspace simulation: it does **not** create real kernel
SCSI generic devices (`/dev/sg*`, `/dev/nst*`). Bareos (and any tool driven
through `gotochanger-changer`/the REST API) never needs one — Bareos calls
a "Changer Command" script and reads/writes plain files, exactly as with
`disk-changer.in`. A future phase could expose true kernel SCSI devices via
`target_core_user` (TCMU)/LIO, backed by this same daemon, for tools that
insist on talking to a real SCSI medium changer/tape device; that is
intentionally out of scope for this initial version given the root
privileges, kernel module and custom SSC/SMC command emulation it would
require.

**Drive bandwidth throttling is not implemented.** A loaded drive is a
symlink at the configured device path; Bareos writes directly to that
file and gotochanger never sees the byte stream, so there's nothing to
throttle without a real interception layer (FUSE, or the same TCMU/LIO
kernel-device work described above). Revisit alongside kernel-device
support if this is ever needed.
