# Monitor gotochanger via SNMP

You want an existing NMS (Nagios, Zabbix, PRTG, or similar) to receive SNMPv2c traps for library events -
robotic/drive faults, media movement, authentication, configuration changes - and to decode them using
gotochanger's own MIB.

## Prerequisites

- An SNMP trap receiver reachable from the gotochangerd host.
- Admin access to gotochangerd.

## Steps

1. Enable SNMP and point it at your receiver:
   ```sh
   gotochangerctl settings set snmp_enabled=true snmp_enterprise_oid=1.3.6.1.4.1.55555.1
   ```
   Targets (`host:port:community`, one or more) aren't expressible via the CLI's simple `key=value` form -
   set them from Admin > Settings > "SNMP traps", or directly:
   ```sh
   curl -X PUT --unix-socket /run/gotochanger/gotochanger.sock \
     http://localhost/api/v1/settings \
     -H 'Content-Type: application/json' \
     -d '{"snmp_targets":[{"host":"nms.example.com","port":162,"community":"public"}]}'
   ```
2. Download the MIB matching your configured enterprise OID:
   ```sh
   curl --unix-socket /run/gotochanger/gotochanger.sock \
     http://localhost/api/v1/snmp/mib -o gotochanger.mib
   ```
   (Or click the same link from Admin > Settings > SNMP traps in the web UI.)
3. Load `gotochanger.mib` into your NMS's MIB browser/trap decoder.
4. Trigger an event to confirm delivery - a drive fault is the simplest:
   ```sh
   gotochangerctl fault 0 on
   gotochangerctl fault 0 off
   ```

## Verify

On your NMS, expect one decoded trap for the fault-set event and one for the fault-clear event, each carrying
the `sysUpTime`, `snmpTrapOID`, a human-readable message, and structured `code`/`category`/`severity`/
`outcome`/`operation` varbinds plus a `key=value; ...` detail string - not just an opaque numeric OID. If the
trap arrives but decodes as an unknown OID, re-download the MIB: it's rendered against your *current*
`snmp.enterprise_oid`, so an MIB downloaded before changing that setting will no longer match. Cross-check
against `gotochangerctl events`, which shows the exact same event that produced the trap, to confirm nothing
was dropped in transit.
