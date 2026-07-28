# Administration

The Admin section is organized into three groups, in the order you'll typically need them: who can get in,
what the library is made of, and how it behaves day to day. Every route under Admin is Admin-only - there is
no Operator-reachable exception, since these actions change shared topology, credentials, or the database
itself.

## Access Control

**Users** - create additional accounts beyond the built-in `Admin`, each with one of the three roles
(Viewer/Operator/Admin). Passwords are hashed with PBKDF2-HMAC-SHA256; a locked-out account (5 failed logins)
unlocks itself automatically after 15 minutes. The last remaining Admin account can never be deleted or
demoted, so you can't accidentally lock yourself out entirely.

**API Tokens** - role-scoped tokens for scripts/automation to authenticate with, via either an `X-Api-Key`
header or an `Authorization: Bearer` token. Managing tokens is Admin-only, but a token itself can be scoped
down to Viewer or Operator - see
[Set up a scoped operator API token for a CI script](#set-up-a-scoped-operator-api-token-for-a-ci-script) for a full example.
Only a token's SHA-256 hash is ever stored; a token's raw value is shown exactly once, at creation time.

The bootstrap install also auto-generates a single admin-scoped token on first run, logged once to
`journalctl -u gotochanger` (`grep 'bootstrap API token'`) - useful for scripting an initial setup before any
user account exists.

## Library Topology

Everything that defines the physical (and logical) shape of the library. All of it hot-applies immediately -
no daemon restart, ever - when you add, edit, or remove something here.

- **Drive Types** - the catalog of drive hardware models offered during setup and used by Logical Libraries.
- **Tape Types** - the catalog of media families (LTO/DLT/SDLT/DDS/AIT/3592/generic), each defining its own
  barcode format (see the barcode reference table in
  [Overview and Concepts](#tape-sets--barcodes) - unless noted otherwise in that table, `New tape type`
  chooses the format for you).
- **Tape Sets** - groups of cartridges by tape type, each stored under its own folder on disk. **New tape
  set** creates the set and its first batch of cartridges in one step; **Add tapes** on an existing set tops
  it up later (auto-generated or a manually-typed barcode).
- **Drives** - the physical drive device list. Removing anything other than the highest-indexed drive shifts
  every later drive's index, so double-check which drive you mean to remove if there's more than one - this
  is exactly the kind of shift that produces the [Drive Index trap](#bareos-integration) on the Bareos side
  if the corresponding `Device` resources aren't updated to match.
- **Magazines** / **Mailboxes** - storage-slot and I/O-slot groups, respectively. Either can optionally
  require a 4-digit PIN to open its door (Admin > Settings > PIN) - an empty PIN clears the requirement.
- **Logical Libraries** - create/edit partitions of drives, magazines, and mailboxes; the Unassigned list at
  the bottom shows anything not yet claimed by one. The **Bareos Config** button on each logical library
  generates a ready-to-paste Autochanger/Device block, including the correct `Drive Index` per drive.

## Operations

- **Latency** - seven independently-tunable simulated delays (drive load/unload, tape positioning, robotic
  arm movement for tape moves vs. magazine scans, post-close magazine scanning, door open/close) applied
  library-wide, editable live with a "Load defaults" prefill (`gotochangerctl latency get|set|reset`). Turning
  this on is what makes the busy indicator light actually visible for more than an instant.
- **Cleaning tapes** - thresholds (mount count before a drive needs cleaning), maximum uses per cartridge, and
  cleaning duration are all configured here (`gotochangerctl cleaning settings get|set|reset`). See
  [Managing cleaning tapes](#managing-cleaning-tapes) for the full lifecycle and a worked example.
- **Settings** - default volume capacity, offsite rotation schedule, daemon-level knobs (log level, capacity
  poll interval), and SNMP trap configuration - see [Monitoring](#monitoring).
- **Backup** - a backup is a full snapshot of the database (`VACUUM INTO`): topology, every setting above,
  current slot/drive/volume state, and - since user accounts and API tokens live in that same database - the
  stored credential hashes too. Because a backup file therefore carries password hashes, every action here is
  Admin-only: taking and downloading a manual backup, scheduling recurring backups (interval + retention),
  browsing previously stored ones, and restoring.
  **Restoring replaces the entire database** (atomically, then restarts the service) and genuinely overwrites
  user accounts and tokens along with topology - see
  [Backup and restore for disaster recovery](#backup-and-restore-for-disaster-recovery) for the full
  procedure and what to check afterward.
- **Factory reset** - `gotochangerctl reset <confirm-name>` (or the Admin UI equivalent) wipes the database
  back to empty defaults: no topology, no users/tokens (bootstrap required again), wizard not completed -
  exactly like a fresh install. It requires typing the VTL's current name as confirmation, and takes a
  safety-net backup automatically before wiping anything. Optional `--delete-volumes` also deletes every
  cartridge's backing file on disk. Like restore, this restarts the service.
