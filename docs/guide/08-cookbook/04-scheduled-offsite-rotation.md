# Scheduled offsite rotation

You want a chosen number of full volumes automatically vaulted offsite on a recurring schedule, simulating
routine tape rotation without an operator manually clicking "Send to offsite" every day - plus the ability to
trigger a send/recall by hand (e.g. from a cron job or an external rotation script) when needed.

## Prerequisites

- Offsite vaulting enabled for this library (set during the setup wizard, or via Admin > Settings).
- At least one full storage-slot volume to rotate.

## Steps

### Enable and configure scheduled rotation

```sh
gotochangerctl settings set offsite_location=true offsite_rotation_count=2 offsite_rotation_interval=24h
```

gotochangerd re-reads `offsite_rotation_interval`/`offsite_rotation_count` from the live settings store every
few seconds, so this takes effect without a restart. Once the interval elapses, up to `offsite_rotation_count`
of the least-recently-created **full** volumes currently in storage slots are sent offsite automatically.

### Trigger a rotation manually (e.g. from cron)

```sh
gotochangerctl offsite list
gotochangerctl offsite send slot 3
```

Equivalently via curl:

```sh
curl -X POST --unix-socket /run/gotochanger/gotochanger.sock \
  http://localhost/api/v1/offsite/send \
  -H 'Content-Type: application/json' \
  -d '{"from_kind":"slot","from_address":3}'
```

A typical cron entry driving a nightly manual rotation instead of (or in addition to) the built-in scheduler:

```
0 2 * * * gotochangerctl offsite send slot 3 >> /var/log/gotochanger-offsite.log 2>&1
```

### Recall a volume

```sh
gotochangerctl offsite recall <barcode> slot 3
```

## Verify

```sh
gotochangerctl offsite list
gotochangerctl events | grep -i offsite
```

Expect the sent volume to disappear from storage-slot listings (`gotochangerctl status`) and appear in
`gotochangerctl offsite list`; the activity log (and an SNMP trap, if enabled - see [Monitoring](#monitoring))
records a `MEDIA.OFFSITE-SEND.SUCCESS`-style event for both the manual and scheduled paths. A scheduled
rotation that finds a candidate volume already moved by something else in the gap is skipped with a logged
error rather than treated as fatal - check the events log if fewer volumes were rotated than expected.
