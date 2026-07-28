# Backup and restore for disaster recovery

The host running gotochangerd was rebuilt (or its disk was lost) and you need to restore topology, volume
state, and settings from a previously downloaded backup - onto either the same host or a freshly installed
one.

## Prerequisites

- A previously downloaded backup file (see "Taking a backup" below if you don't have one yet).
- Admin access to the *new* gotochangerd instance (a fresh install, bootstrapped with a temporary Admin
  password).

<div class="callout callout-warn">
<svg class="icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3l10 18H2L12 3z"/><path d="M12 10v4M12 17.5h.01"/></svg>
<div><strong>A backup is a full snapshot of the whole database, not just topology.</strong> It contains every
user account and API token's credential hash alongside slots/drives/tape sets/settings. Restoring
<strong>replaces the entire database</strong>, including users and tokens - the temporary Admin password you
just bootstrapped on the new install will stop working the moment the restore completes, replaced by whatever
accounts existed at backup time.</div>
</div>

## Taking a backup (before disaster strikes)

```sh
gotochangerctl backup download ./gotochanger-backup.db
```

Or schedule recurring backups so you always have a recent one:

```sh
gotochangerctl backup schedule set interval=24h retention=7
```

## Restoring onto the rebuilt host

1. Install gotochanger fresh (see [Installation](#installation)) and bootstrap a temporary Admin password -
   this account only exists long enough to perform the restore.
2. Restore the backup:
   ```sh
   gotochangerctl restore ./gotochanger-backup.db
   ```
   Equivalently via curl:
   ```sh
   curl -X POST --unix-socket /run/gotochanger/gotochanger.sock \
     http://localhost/api/v1/restore \
     --data-binary @./gotochanger-backup.db
   ```
3. The daemon validates the file (SQLite header + expected schema), atomically swaps it in, and restarts.
   Expect a brief unreachable window.
4. Sign in again - **using the credentials that existed at backup time**, not the temporary bootstrap
   password from step 1.

## Verify

```sh
gotochangerctl status
gotochangerctl user list
gotochangerctl volumes
```

Expect the same topology (magazines, mailboxes, drives, logical libraries), the same tape sets/volumes, and
the same user accounts/roles that existed when the backup was taken - not the temporary bootstrap account
from step 1, which no longer exists post-restore. Re-issue any API tokens your automation depends on if you
aren't certain they survived in the backup, since a token's raw value is never recoverable - only its hash is
stored, matching what was true before the disaster.
