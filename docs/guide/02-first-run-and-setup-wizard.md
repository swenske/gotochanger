# First Run and Setup Wizard

A fresh install has no drives, magazines, or mailboxes configured at all - the daemon starts completely empty
and walks you through bootstrap, then an 8-step wizard, before handing you the dashboard.

## Bootstrap

The very first request to the web UI (or `POST /api/v1/auth/bootstrap`) sets the password for the built-in
`Admin` account - there's no default password to guess, and this only works once (`GET /api/v1/auth/state`
reports whether bootstrap is still required). After that, sign in normally; sessions are cookie-based and
held in memory only, so restarting `gotochangerd` signs everyone out.

## The wizard

You can always go back with **Previous** without losing what you've already entered, and everything you
submit is saved immediately to the database (not just at the end) - closing the browser mid-wizard never
loses earlier steps. `gotochangerctl wizard status` reports the current step from the CLI; the wizard itself
is only driven through the web UI/REST API (`GET/POST /api/v1/wizard`, `POST /api/v1/wizard/complete`,
`POST /api/v1/wizard/reset`, `GET /api/v1/wizard/options`).

1. **Operational Mode** - name your virtual tape library (the VTL name shown throughout the UI) and pick the
   operational mode: `changer` (default userspace/file mode) or `kernel` (see [Kernel Mode](#kernel-mode)).
2. **Drives** - choose how many physical drives to create and which drive type each one is, from the
   drive-type catalog.
3. **Magazines** - define one or more magazines (storage slot groups, 5-20 slots each).
4. **Mailboxes** - define one or more mailboxes (I/O slot groups, 1-5 slots each) - optional, a library can
   run with no I/O slots at all.
5. **Offsite Location** - a simple on/off toggle for whether offsite vaulting is available at all; the
   rotation schedule itself is configured later, in Admin > Settings.
6. **Tape Sets** - create at least one tape set (tape type + storage folder + how many cartridges to
   generate up front). The cartridge-count field is consumed once, at step 8's completion.
7. **Logical Libraries** - optionally partition your drives/magazines/mailboxes into one or more Logical
   Libraries; you can also leave everything unassigned and do this later from Admin.
8. **Latency Simulation** - a single checkbox: enable realistic timing delays or not. The actual delay values
   always start at sensible factory defaults and are tuned afterwards from Admin > Latency, never from the
   wizard itself.

`POST /api/v1/wizard/complete` hot-applies everything to the running daemon immediately - no restart, and the
dashboard reflects your new topology the instant you land on it. It also generates each tape set's pending
cartridges and reconciles any kernel-mode instances if operational mode is `kernel`.

<div class="callout callout-tip">
<svg class="icon" viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="9"/><path d="M12 8v.01M11 11h1v5h1"/></svg>
<div>The wizard is resumable across a daemon restart: every step's data is written to the database as you
submit it, so a restart mid-wizard picks up exactly where you left off instead of starting over.</div>
</div>
