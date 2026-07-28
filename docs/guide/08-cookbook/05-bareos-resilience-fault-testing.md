# Bareos resilience testing: fault injection

You want to verify your Bareos configuration (retries, alerting, failover) actually reacts correctly when a
drive or the robotic arm fails mid-job - without waiting for real hardware to break. gotochanger can inject
either kind of failure on demand and clear it just as easily.

## Prerequisites

- A running backup job (or one ready to run) against a gotochanger-backed Autochanger.
- Operator access (drive/robotic faults are an Operator-level action, not Admin-only).

## Steps

### Inject a drive fault

```sh
gotochangerctl fault 0 on
```

Equivalently:

```sh
curl -X POST --unix-socket /run/gotochanger/gotochanger.sock \
  http://localhost/api/v1/drives/0/fault \
  -H 'Content-Type: application/json' \
  -d '{"fault":true}'
```

Kick off (or let continue) a Bareos job using drive 0. Expect Bareos to report a mount/drive error - exactly
as if a real drive had failed - and your alerting to fire on it.

Clear it once you've confirmed the failure handling:

```sh
gotochangerctl fault 0 off
```

### Inject a robotic-arm fault

There's only one shared robotic arm, so this affects every drive/logical library at once - useful for testing
a full "changer offline" scenario:

```sh
gotochangerctl robotic-fault on blocked_arm "simulated jam for DR test"
```

Valid `<kind>` values: `blocked_arm`, `mispositioned_cartridge`, `pickup_failure`, `drop_failure`,
`movement_jam`, `other`. Equivalently:

```sh
curl -X POST --unix-socket /run/gotochanger/gotochanger.sock \
  http://localhost/api/v1/robotics/fault \
  -H 'Content-Type: application/json' \
  -d '{"active":true,"kind":"blocked_arm","message":"simulated jam for DR test"}'
```

Every Load/Unload/Move now fails library-wide (door open/close still works) until cleared:

```sh
gotochangerctl robotic-fault off
```

## Verify

```sh
gotochangerctl events
```

Expect `DRIVE.FAULT.SET.SUCCESS` and `ROBOTICS.FAULT.SET.SUCCESS`-style events (and, if SNMP is enabled, a
matching trap for each - see [Monitoring](#monitoring)) around the times you injected each fault, followed by
`*.CLEAR.SUCCESS` events once cleared. On the dashboard, the affected drive (or the Robotic Arm panel) shows
the amber pulsing fault light for the whole window - see
[Drive Indicator Lights](#drive-indicator-lights) for the full light legend. Cross-check against Bareos's own
job log/`bconsole status storage` output to confirm it actually surfaced the failure to an operator instead
of silently retrying forever.
