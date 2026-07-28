# CLI and REST API Reference

Every action available in the web UI is also available over the `gotochangerctl` CLI and the REST API - the
dashboard is a client of the same API, nothing more.

## gotochangerctl

By default, `gotochangerctl` talks to the trusted local Unix socket (`/run/gotochanger/gotochanger.sock`, no
token needed - every request over it is treated as Admin). Use `--url`/`--token` to drive a remote instance
instead, `--json` for machine-readable output, and `--logical-library=NAME` to scope status/load/unload/move
to one logical library.

```sh
gotochangerctl status                                        # arm/drive/slot/ioslot snapshot
gotochangerctl events                                         # recent event log
gotochangerctl volumes                                        # racked volumes (slots/ioslots/drives)
gotochangerctl outside                                        # outside-library ("loading dock") volumes
gotochangerctl load <slot|ioslot> <address|label> <drive>      # move a volume into a drive
gotochangerctl unload <drive> <slot|ioslot> <address|label>
gotochangerctl move <slot|ioslot> <addr> <slot|ioslot> <addr>
gotochangerctl outside-delete <barcode>                        # remove an outside-library cartridge
gotochangerctl io-door <mailbox-id> open [pin] | close [actions-json]
gotochangerctl storage-door <magazine-id> open [pin] | close [actions-json]
gotochangerctl fault <drive> <on|off>                          # simulate a drive fault
gotochangerctl write-protect <barcode> <on|off>
gotochangerctl robotic-fault on <kind> [message] | off         # simulate a robotic-arm fault
gotochangerctl token new|revoke|list [name] [role]             # API token management (admin/operator/viewer)
gotochangerctl user new|list|delete|role|reset-password ...    # local user accounts
gotochangerctl settings get | set <key>=<value> ...            # daemon-wide settings (snmp, offsite, ...)
gotochangerctl latency get | set <k>=<v>... | reset            # simulated latency knobs
gotochangerctl cleaning settings get|set|reset                 # cleaning tunables
gotochangerctl cleaning tape new|list                          # create/list cleaning cartridges
gotochangerctl logical-library new|list|show|update|delete ...
gotochangerctl drive-type new|list|update|delete ...
gotochangerctl tape-type new|list|update|delete ...
gotochangerctl tape-set new|list|update|delete|add-tapes|add-tape ...
gotochangerctl magazine new|list|update|delete ...
gotochangerctl mailbox new|list|update|delete ...
gotochangerctl drive new|list|update|delete ...
gotochangerctl unassigned                                      # drives/slots/ioslots not in any logical library
gotochangerctl offsite list|send|recall ...
gotochangerctl backup download|list|download-stored|delete|schedule show|schedule set ...
gotochangerctl restore <file>                                   # replaces state.db (incl. users/tokens!) + restarts
gotochangerctl reset <confirm-name> [--delete-volumes]            # factory-reset + restart
gotochangerctl wizard status                                      # setup-wizard state (the wizard itself is web/API only)
```

`<address|label>` arguments accept either a bare physical integer address or the human-facing
`"<ordinal>.<offset>"` label (e.g. `"2.3"` - slot 3 of the 2nd currently-existing magazine).

<div class="callout callout-note">
<svg class="icon" viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="9"/><path d="M12 8v4M12 16h.01"/></svg>
<div><code>outside-create</code>, <code>import</code>, <code>create-volume</code>, and <code>export</code> are
retired - creating a new cartridge is now always <code>tape-set add-tape</code> (it belongs to a tape set),
and moving media in/out of the library is always the <code>io-door</code>/<code>storage-door</code>
open/queue/close workflow described in
<a href="#dashboard-tour">Dashboard Tour</a>.</div>
</div>

## REST API

All actions are available over HTTP. The Unix socket is trusted (every request is treated as Admin, access
controlled by filesystem permissions); the TCP listener accepts either a browser session cookie or an API
token (`X-Api-Key` header or `Authorization: Bearer <token>`), each carrying one of the
`admin`/`operator`/`viewer` roles.

| Method | Path | Role | Purpose |
|---|---|---|---|
| POST | `/api/v1/auth/bootstrap` | none | Set the initial Admin password |
| POST | `/api/v1/auth/login` | none | Log in, start a session |
| POST | `/api/v1/auth/logout` | viewer+ | Log out |
| GET | `/api/v1/auth/state` | none | Am I logged in? Is bootstrap needed? |
| POST | `/api/v1/auth/change-password` | viewer+ | Change your own password |
| GET | `/api/v1/status` | viewer+ | Full library snapshot |
| GET | `/api/v1/events` | viewer+ | Recent activity log |
| GET | `/api/v1/stream` | viewer+ | Live event stream (Server-Sent Events) |
| GET | `/api/v1/volumes` | viewer+ | List all volumes |
| GET | `/api/v1/outside` | viewer+ | List outside-library tapes |
| DELETE | `/api/v1/outside/{barcode}` | operator+ | Delete an outside-library tape |
| GET / POST | `/api/v1/cleaning/tapes` | viewer+ / operator+ | List / create cleaning tapes |
| POST | `/api/v1/load` | operator+ | Load a slot/ioslot into a drive |
| POST | `/api/v1/unload` | operator+ | Unload a drive to a slot/ioslot |
| POST | `/api/v1/move` | operator+ | Move between slot/ioslot elements |
| POST | `/api/v1/doors/io/{id}/open` \| `/close` | operator+ | Open/close an I/O (mailbox) door |
| POST | `/api/v1/doors/storage/{id}/open` \| `/close` | operator+ | Open/close a storage (magazine) door |
| POST | `/api/v1/drives/{index}/fault` | operator+ | Inject/clear a simulated fault on one drive |
| POST | `/api/v1/robotics/fault` | operator+ | Inject/clear a simulated fault on the shared robotic arm |
| POST | `/api/v1/volumes/{barcode}/write-protect` | operator+ | Set/clear a volume's write-protect flag |
| GET | `/api/v1/offsite` | viewer+ | List volumes in the offsite vault |
| POST | `/api/v1/offsite/send` \| `/recall` | operator+ | Send/recall a volume to/from the offsite vault |
| GET/POST/DELETE | `/api/v1/users` | admin | Manage user accounts |
| GET/POST/DELETE | `/api/v1/tokens` | admin | Manage scoped API tokens |
| GET/PUT | `/api/v1/settings` | admin | View/update application settings |
| GET/PUT | `/api/v1/settings/latency` | admin | View/update latency simulation settings |
| GET/PUT | `/api/v1/settings/cleaning` | admin | View/update cleaning thresholds |
| GET/PUT | `/api/v1/settings/pin` | admin | View/update the magazine/mailbox door PIN |
| GET/POST/PUT/DELETE | `/api/v1/logical-libraries[/{name}]` | admin | Manage logical library partitions |
| GET | `/api/v1/unassigned` | admin | Drives/slots/mailboxes in no logical library |
| GET/POST/PUT/DELETE | `/api/v1/drive-types[/{name}]` | admin | Manage the drive-type catalog |
| GET/POST/PUT/DELETE | `/api/v1/drives[/{index}]` | admin | Manage drives (hot-applies) |
| GET/POST/PUT/DELETE | `/api/v1/tape-types[/{name}]` | admin | Manage the tape/media-type catalog |
| GET/POST/PUT/DELETE | `/api/v1/tape-sets[/{name}]` | admin | Manage tape sets (type + storage folder) |
| GET | `/api/v1/fs/browse` | admin | Browse server-side folders (tape-set storage picker) |
| GET/POST/PUT/DELETE | `/api/v1/magazines[/{id}]` | admin | Manage magazines (hot-applies) |
| GET/POST/PUT/DELETE | `/api/v1/mailboxes[/{id}]` | admin | Manage mailboxes (hot-applies) |
| GET | `/api/v1/backup/download` | admin | Download an on-demand backup of `state.db` |
| GET/PUT | `/api/v1/backup/schedule` | admin | View/update the recurring backup schedule |
| GET | `/api/v1/backups` | admin | List stored (scheduled) backups |
| GET | `/api/v1/backups/{filename}/download` | admin | Download a stored backup |
| DELETE | `/api/v1/backups/{filename}` | admin | Delete a stored backup |
| POST | `/api/v1/restore` | admin | Restore `state.db` from a backup file (restarts) |
| POST | `/api/v1/reset` | admin | Factory reset (name-confirmed, restarts) |
| GET/POST | `/api/v1/wizard` | admin | Current step / submit one wizard step |
| POST | `/api/v1/wizard/complete` \| `/reset` | admin | Finish, or reset progress of, the setup wizard |
| GET | `/api/v1/wizard/options` | admin | Catalogs + current state for the wizard UI |
| GET | `/api/v1/kernel-mode/status` | viewer+ | Whether `gotochanger-kernel`/the kernel module are available |
| GET | `/api/v1/kernel-mode/devices` | viewer+ | Real device paths self-reported by running `gotochanger-tcmud` |
| GET | `/api/v1/snmp/mib` | viewer+ | Download the dynamic MIB (see [Monitoring](#monitoring)) |
| GET | `/api/v1/openapi.json` | none | OpenAPI 3.0 spec backing the Swagger UI at `/docs` |

`Load`/`Unload`/`Move` (and `GET /api/v1/status`) additionally accept an `X-Logical-Library: NAME` header to
scope the operation to one logical library.

A minimal curl example, using the trusted socket's HTTP-over-Unix-socket form:

```sh
curl --unix-socket /run/gotochanger/gotochanger.sock http://localhost/api/v1/status | jq .
```

Or against the TCP listener with a token:

```sh
curl -H "X-Api-Key: $GOTOCHANGER_TOKEN" http://localhost:8480/api/v1/status | jq .
```

For interactive exploration with full request/response schemas, use the Swagger UI at `/docs` (backed by
`/api/v1/openapi.json`) - see the note in [Monitoring](#monitoring) about its current route coverage.
