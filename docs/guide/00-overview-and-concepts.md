# Welcome to gotochanger

<p class="lead">A virtual tape library you can operate, break, and repair without touching real hardware.</p>

gotochanger simulates a SCSI tape autochanger &mdash; storage slots, import/export (mail) slots, robotics, and
multiple tape drives &mdash; behind a REST API and a web dashboard. It's most often used as a drop-in
`Changer Command` target for **Bareos**, replacing a bare disk-staging setup with something that behaves like
a real tape library: slots fill up, cartridges have real barcodes, drives can fault, and operations take
realistic amounts of time if you turn latency simulation on.

This guide covers everything a day-to-day operator or administrator needs: the dashboard, the concepts behind
slots/drives/tape sets, common step-by-step workflows, installation, the first-run setup wizard, Bareos
integration, kernel mode, the Admin section, monitoring, the CLI and REST API, and a cookbook of full
end-to-end scenarios.

<div class="callout callout-tip">
<svg class="icon" viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="9"/><path d="M12 8v.01M11 11h1v5h1"/></svg>
<div>By default, everything in gotochanger is simulated in userspace: no real kernel SCSI device is created,
just plain files. Loads, unloads, faults, and even drive timing are all software state you can freely
experiment with. An optional <a href="#kernel-mode">kernel mode</a> can additionally expose the same library
as real <code>/dev/sg*</code>/<code>/dev/nst*</code> devices, for tools that insist on a real SCSI medium
changer - see the Kernel Mode section for when that's actually needed.</div>
</div>

## Getting Started

### First run: bootstrap and sign in

The first time you open gotochanger, you'll be asked to set a password for the built-in **Admin** account
&mdash; there's no default password to guess. After that, sign in with username `Admin` and the password you
just set. Sessions are cookie-based and held in memory by the daemon, so signing everyone out is as simple as
restarting the service.

If this is a brand-new install with no topology configured yet (no drives, magazines, or mailboxes), you'll be
dropped straight into the [Setup Wizard](#first-run-and-setup-wizard) instead of the dashboard.

### Roles

Every user account and API token has exactly one role, checked on every request:

- **Viewer** &mdash; read-only. Can see the dashboard and Admin screens but can't load/unload tapes, open
  doors, or change anything.
- **Operator** &mdash; everything a Viewer can do, plus day-to-day operation: load/unload/move tapes, open and
  close doors, raise/clear drive and robotic-arm faults, download a manual backup.
- **Admin** &mdash; everything, including all of Admin > Library Topology, user/token management, scheduled
  backups, and restore.

The dashboard adapts to your role automatically &mdash; action buttons (Load, Unload, Move, Raise fault, ...)
only appear if you're an Operator or Admin, and the Admin nav button itself is hidden entirely for a Viewer.

## Dashboard Tour

The dashboard is a set of independent panels, each mirroring one physical part of the library. Every panel can
be **dragged by its `::` handle** to reorder it, and **collapsed** with the button in its header &mdash; both
the order and which panels are collapsed are remembered per-browser (saved to local storage), so your layout
survives a reload. The toolbar above the panels has two more options: *Show library colors* (tints each
element by which Logical Library it belongs to) and *Hide unassigned* (hide elements that aren't part of any
Logical Library yet), plus a *Collapse all* shortcut.

The dashboard polls `GET /api/v1/status` every 4 seconds and re-renders automatically &mdash; you never need to
manually refresh the page to see a change made from another tab, another operator, or the API.

### Outside Library Tapes

Cartridges that exist as real files on disk but aren't in any slot, drive, or the offsite vault right now
&mdash; think of it as the loading dock. This is also where new cartridges are born: the **Create tape**
button walks you through picking a Tape Set and either auto-generating the next barcode in sequence or typing
one in by hand. From here, tapes get loaded into a drive, a storage slot, or an I/O slot.

### Offsite Vault

Volumes that have been sent offsite &mdash; simulating tape rotation to a physical vault. **Send to offsite**
picks a full storage slot to vault; each vaulted volume gets a **Recall** button to bring it back into an
empty storage slot. See [Offsite Vaulting](#offsite-vaulting) for the concept and how scheduled rotation
works, and the [Scheduled offsite rotation](#scheduled-offsite-rotation) cookbook scenario
for a worked example with the CLI/API.

### Robotic Arm

There's exactly one robotic arm in the physical library, shared by every Logical Library. This panel shows
its status &mdash; idle, moving, or in a simulated fault &mdash; with an indicator light using the same
convention as the drives (see [Drive Indicator Lights](#drive-indicator-lights)). Operators can **Raise
fault** (choosing a realistic failure mode: blocked arm, mispositioned cartridge, pickup/drop failure,
movement jam, or other) to test how their backup software reacts, then **Clear fault** once done. A raised
fault rejects Load/Unload/Move (but not door open/close) library-wide until cleared &mdash; see the
[Bareos resilience testing](#bareos-resilience-testing-fault-injection) cookbook scenario for a full
walkthrough with events and SNMP traps.

### Drives

Every physical tape drive, each with an indicator light (see below), the cartridge currently loaded (if any)
rendered as a real-looking barcode label, and how full it is. Loaded drives get an **Unload to...** button;
every drive gets a **Raise fault / Clear fault** toggle to simulate a hardware problem on that specific drive.

### Drive Indicator Lights

Each drive (and the robotic arm) shows a small colored light modeled on a real tape drive's front panel, so
the dashboard reads at a glance instead of requiring you to parse text on every card.

<div class="led-legend">
<div class="led-legend-row"><span class="led led-fault"></span><strong>Amber, pulsing</strong><span class="desc">Fault - the drive (or the robotic arm) is in a simulated fault state. Takes priority over everything else.</span></div>
<div class="led-legend-row"><span class="led led-writing"></span><strong>Red, pulsing</strong><span class="desc">Writing - the loaded cartridge's backing file just grew between two dashboard polls, meaning something is actually writing data to it right now (e.g. a real attached backup job).</span></div>
<div class="led-legend-row"><span class="led led-busy"></span><strong>Green, blinking</strong><span class="desc">Active operation - this browser tab has a Load/Unload/Move in flight for this drive (or the arm generally, for the Robotic Arm panel).</span></div>
<div class="led-legend-row"><span class="led led-idle"></span><strong>Green, steady/dim</strong><span class="desc">Ready - a cartridge is loaded and idle.</span></div>
<div class="led-legend-row"><span class="led led-off"></span><strong>Dark</strong><span class="desc">Empty - no cartridge loaded, nothing happening.</span></div>
</div>

Priority order when more than one condition is true, highest first: **Fault > Writing > Active operation >
Ready > Empty**. A faulted drive always shows amber even mid-operation, for instance.

<div class="callout callout-warn">
<svg class="icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3l10 18H2L12 3z"/><path d="M12 10v4M12 17.5h.01"/></svg>
<div><strong>The "Writing" light needs a real write, not just a Load.</strong> gotochanger doesn't simulate the
byte stream of a backup job &mdash; a loaded drive (in userspace/file mode) is just a symlink at a device path,
and nothing writes to it unless something external actually does (typically a real attached Bareos SD job, or
a manual test write). So clicking Load and watching the drive sit at steady green with no red flash is
completely normal, not a bug &mdash; it just means nothing has written any bytes to that cartridge yet.</div>
</div>

Two more details worth knowing if you're timing things precisely: the "busy" light only reflects an operation
*this browser tab* started &mdash; there's no way to see another tab's or another operator's in-flight action
as "busy" (it just means an operation is happening somewhere; a status refresh from any tab will briefly pause
until it finishes, since only one physical robotic arm exists). And the "writing" light, once triggered, stays
lit for a few seconds after the write is detected (longer than one poll interval) so a brief burst of activity
remains visible instead of flickering for a single frame.

### I/O Slots (mail slots)

Import/export elements, grouped into **Mailboxes**. Each mailbox group has its own door: click **Open mail
slot** to start staging tapes in or out, then use each slot's **Load** (bring an outside tape in) or **Pickup**
(queue a tape to come out) button, and finally **Close mail slot** to commit every queued action at once
&mdash; just like closing a real mail slot door triggers the robot to do the actual moves.

### Storage Slots

The main body of the library, grouped into **Magazines**. Each occupied slot can **Move** its cartridge to an
empty drive, another slot, or an I/O slot. Open a magazine's storage door to get **Bulk load...** (load several
outside tapes into empty slots in one dialog) and **Move all to outside...** (queue several loaded slots for
pickup at once) &mdash; handy for seeding a fresh magazine or clearing one out.

Below the panels, the **Activity Log** dock (bottom of the screen, also collapsible) shows a running feed of
everything that's happened &mdash; loads, unloads, faults, configuration changes &mdash; each tagged with a
status pill (success/failure/warning/...), useful for spotting what an automated job or another operator just
did.

## Core Concepts

gotochanger's data model mirrors a real tape library fairly literally. If you've operated a physical
autochanger or an LTO library before, most of this will already be familiar.

### Magazines & Storage Slots

A **Magazine** is a named group of 5-20 storage slots (in increments of 5) &mdash; the removable cartridge
racks a real library holds internally. Storage slots are what hold the bulk of your tape inventory between
backup jobs. Slots are addressed contiguously across the whole physical library (all magazines' slots first,
in creation order), which only matters if you're scripting against the raw API &mdash; the dashboard always
shows you slots grouped back into their magazine, and each slot also gets a human-facing, magazine-relative
label like `2.3` (slot 3 of the 2nd currently-existing magazine).

### Mailboxes & I/O Slots

A **Mailbox** is the import/export equivalent of a magazine &mdash; a named group of 1-5 I/O (mail) slots. I/O
slots share one contiguous address space with storage slots (storage slots first, then I/O slots), matching
how real SCSI medium changers and Bareos itself report a combined "N slots (M import/export)" total. This is
the normal door operators use to bring new cartridges in or send full ones out without opening the whole
library.

### Tape Drives

Each physical tape drive (a "Data Transfer Element") can hold at most one cartridge at a time. A drive can be
individually put into a simulated fault state to test failure handling, independent of every other drive and
of the robotic arm. By default a loaded drive is a plain symlink at a configured device path (userspace/file
mode); [Kernel Mode](#kernel-mode) can instead back it with a real `/dev/nst*` device.

### Tape Sets & Barcodes

Every cartridge belongs to exactly one **Tape Set** &mdash; a named group of cartridges that share a **Tape
Type** (the media family: LTO, DLT, SDLT, DDS/DAT, AIT/SAIT, IBM 3592, or a non-physical "generic" type) and a
storage folder on disk. A cartridge's **barcode** is its one and only identifier &mdash; there's no separate
"volume label" concept &mdash; and it's always unique across the entire library, not just within its own tape
set. Barcodes auto-generate in sequence per tape type (skipping past any already in use), or you can type one
in by hand as long as it matches the tape type's format.

| Family | Shape | Example | Notes |
|---|---|---|---|
| LTO | 6-digit volser + 2-char media id | `000001L8` | Media id like `L8`/`L9` per LTO generation; real published vendor format. |
| DLT | 6-digit volser + 0-1 char media id | `0000034` | 7 characters total for DLT-IV; real published vendor format. |
| SDLT | 6-digit volser + 1-2 char media id | `000007S2` | Real published vendor format. |
| DDS / AIT / 3592 | 6-digit volser + 2-char media id | `000001D6` | No official external barcode standard for these - gotochanger's own convention, for consistency with LTO/SDLT. |
| Generic | Configurable length, no media id | `00000001` | Used by the built-in "Unlimited"-capacity type, for non-physical/test tapes. |
| Cleaning | Fixed 5-digit sequence + `CLN` suffix | `00001CLN` | Not admin-configurable; see [Managing cleaning tapes](#managing-cleaning-tapes). |

Barcodes render as real Code 39 bars throughout the dashboard (the same symbology real tape libraries and
barcode scanners use) rather than as plain text &mdash; purely cosmetic, but it makes a slot full of cartridges
look like an actual tape library.

### Logical Libraries

A **Logical Library** partitions the physical library into an independent slice &mdash; its own subset of
drives, magazines, and mailboxes &mdash; the same way a Dell ML3 or similar real library can be split into two
logical autochangers sharing one chassis. Each drive/magazine/mailbox can belong to at most one Logical Library
at a time (an element already assigned to one can't be added to another). This is what lets one gotochangerd
instance stand in for two completely separate Bareos Autochanger resources - see
[Partitioning one physical robot into two logical libraries](#partition-one-physical-robot-into-two-logical-libraries)
for a full worked example. Elements not yet assigned to any Logical Library show up under Admin > Logical
Libraries > Unassigned.

### Offsite Vaulting

Simulates rotating full tapes to a physical offsite vault: sending a volume moves it out of its storage slot
into the vault; recalling brings it back into an empty slot. This can also happen on a schedule (Admin >
Settings > Offsite rotation) &mdash; a chosen number of full volumes get sent offsite automatically at a
configured interval, simulating routine tape rotation without a human doing it by hand every time. See
[Scheduled offsite rotation](#scheduled-offsite-rotation) for the CLI/cron-driven version.
