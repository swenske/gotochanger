# Monitoring

Every state-changing action in gotochanger emits two things: one activity-log event (visible via
`GET /api/v1/events`, the dashboard's Activity Log dock, and `gotochangerctl events`), and - when SNMP is
enabled - one SNMPv2c trap. This includes both success and failure outcomes for robotics/media actions,
authentication actions (login/logout/bootstrap/password change), and configuration actions
(users/tokens/settings/scheduled backups).

## Event codes

Events use a structured code taxonomy, for example:

- `ROBOTICS.LOAD.SUCCESS`
- `ROBOTICS.MOVE.FAILURE`
- `DRIVE.FAULT.SET.SUCCESS`
- `AUTH.LOGIN.FAILURE`
- `CONFIG.SETTINGS.UPDATE.SUCCESS`

Each event also carries `category`, `severity`, `outcome`, and `operation` fields (plus a free-form `detail`
string) so operators and NMS rules can classify behavior without parsing free text.

| Domain | Code prefix | Typical severity |
|---|---|---|
| Robotics/media movement | `ROBOTICS.*`, `MEDIA.*` | information / warning |
| Drive state | `DRIVE.*` | information / error |
| Authentication | `AUTH.*` | information / error |
| Configuration/admin | `CONFIG.*` | configuration / error |
| Cleaning cycles | `CLEANING.*` | information / warning |
| Internal daemon failures | `SYSTEM.*` | error |

Outcome suffix convention: `*.SUCCESS` for completed actions, `*.FAILURE` for rejected/failed actions,
`*.WARNING` for non-fatal alerts (for example simulated end-of-tape).

## SNMP traps

Disabled by default. Like the rest of the daemon's configuration, SNMP settings live in the database and are
edited live, with no config file and no restart required:

- Web UI: Admin > Settings > "SNMP traps" panel (Enabled, Enterprise OID, Agent address, and Targets - one
  per line, `host:port:community`).
- CLI, for the scalar fields:

  ```sh
  gotochangerctl settings set snmp_enabled=true snmp_enterprise_oid=1.3.6.1.4.1.55555.1
  ```

  Targets are a list, which the CLI's simple `key=value` form can't express - set them via the web UI, or
  with a direct `PUT /api/v1/settings` call (`snmp_targets`, an array of `{host, port, community}`).

## The dynamic MIB

A MIB matching your *currently configured* enterprise OID is served at `GET /api/v1/snmp/mib` (any
authenticated Viewer+ role) and linked from Admin > Settings > SNMP traps. The endpoint rewrites the PEN and
root object-identifier lines of a bundled MIB template to match the live `snmp.enterprise_oid` setting, so
whatever you load into your NMS always decodes the exact OIDs this instance actually emits - even after
changing the enterprise OID. See
[Monitor gotochanger via SNMP](#monitor-gotochanger-via-snmp) for a full worked example: receiving a trap,
downloading the MIB, and decoding one in a real monitoring tool.

<div class="callout callout-note">
<svg class="icon" viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="9"/><path d="M12 8v4M12 16h.01"/></svg>
<div>The REST API's OpenAPI spec (served at <code>/api/v1/openapi.json</code>, rendered at <code>/docs</code>)
currently documents the most commonly used routes but not every admin/topology endpoint listed in
<a href="#cli-and-rest-api-reference">CLI Reference and REST API</a> - treat the CLI reference and this guide
as the source of truth for anything not yet in Swagger.</div>
</div>

## Prometheus metrics

Disabled by default, like SNMP. Enable it from Admin > Settings > "Prometheus" (Enable checkbox, current
status, and a "Download Grafana dashboard" button), or from the CLI:

```sh
gotochangerctl prometheus enable
```

Once enabled, `GET /metrics` serves metrics in the standard Prometheus text exposition format.

<div class="callout callout-warning">
<svg class="icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3l9 16H3l9-16z"/><path d="M12 9v4M12 16h.01"/></svg>
<div><code>/metrics</code> is <strong>intentionally unauthenticated</strong> - matching standard Prometheus
scrape practice, and reachable even on the authenticated TCP listener with no session cookie or API token.
Restrict network access to it (firewall, reverse proxy, or a scrape-only security group) if this daemon is
reachable beyond trusted monitoring infrastructure: it exposes slot/volume/tape-set naming and library
topology to anyone who can reach it, even though it never exposes credentials.</div>
</div>

Example Prometheus scrape config:

```yaml
scrape_configs:
  - job_name: 'gotochanger'
    static_configs:
      - targets: ['localhost:8480']  # adjust host:port to your listen address
```

Metrics are recorded regardless of transport: both the TCP API and the trusted Unix socket (used by
`gotochanger-changer`/`gotochangerctl`/`gotochanger-tcmud` - i.e. how Bareos actually drives this daemon)
share the same routing, so `gotochanger_operations_total`/`gotochanger_operation_duration_seconds` reflect
real Bareos-driven activity, not just direct API calls.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `gotochanger_slots_total` | gauge | | Total storage slots |
| `gotochanger_slots_free` | gauge | | Free storage slots |
| `gotochanger_slots_occupied` | gauge | | Occupied storage slots |
| `gotochanger_readers_total` | gauge | | Total tape drives |
| `gotochanger_readers_idle` | gauge | | Drives loaded but not currently reading/writing |
| `gotochanger_readers_active` | gauge | | Drives currently reading or writing |
| `gotochanger_readers_free` | gauge | | Drives with no volume loaded |
| `gotochanger_readers_error` | gauge | | Drives in a simulated fault state |
| `gotochanger_volumes_total` | gauge | | Total tape volumes known to the library |
| `gotochanger_volumes_by_status` | gauge | `status` (`in_slot`, `in_ioslot`, `in_drive`, `outside`, `offsite`) | Volumes by current location |
| `gotochanger_magazines_total` | gauge | | Total storage magazines |
| `gotochanger_capacity_utilization_percent` | gauge | | Occupied storage slots as a percentage of total |
| `gotochanger_queue_depth` | gauge | | 1 if the single robotic arm is currently busy, 0 if idle - this simulator has one arm and no operation queue |
| `gotochanger_uptime_seconds` | gauge | | Seconds since the daemon started |
| `gotochanger_last_backup_timestamp` | gauge | | Unix timestamp of the last configuration backup (Admin > Backup, a `state.db` snapshot) - absent if none has ever been taken. Not a Bareos backup-job signal; this daemon only models the changer/library, not Bareos jobs |
| `gotochanger_operations_total` | counter | `operation_type` (`load`, `unload`, `move`, `door_open`, `door_close`, `offsite_send`, `offsite_recall`) | Total library operations executed, whether they succeeded or failed |
| `gotochanger_operation_duration_seconds` | histogram | `operation_type` | Library operation latency in seconds |
| `gotochanger_errors_total` | counter | `error_type` (`bad_request`, `unauthorized`, `forbidden`, `not_found`, `conflict`, `internal`, `other`) | Total request errors, bucketed by HTTP status class |

A ready-to-import Grafana dashboard (Overview, Storage Capacity, Reader Status, Tape Inventory, Operations
Timeline, and System Health rows, with threshold-based coloring on capacity/error-rate panels) is available
from Admin > Settings > Prometheus > "Download Grafana dashboard", or `gotochangerctl prometheus dashboard
gotochanger-dashboard.json`. See
[Monitor gotochanger via Prometheus and Grafana](#monitor-gotochanger-via-prometheus-and-grafana) for a full
worked example: enabling the exporter, scraping it, and importing the dashboard.
