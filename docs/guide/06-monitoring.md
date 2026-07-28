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
