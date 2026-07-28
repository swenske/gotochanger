# Managing cleaning tapes

You want gotochanger to simulate drive cleaning realistically: cartridges that wear out after a fixed number
of cleaning cycles, and a mount-count threshold that decides when a drive actually needs cleaning.

## Prerequisites

- Admin access to gotochangerd.

## Steps

### Configure thresholds

```sh
gotochangerctl cleaning settings get
gotochangerctl cleaning settings set enabled=true mode=backup_robot max_uses=20 mount_threshold=50 duration=2m
```

Two modes are available:

- `backup_software` - cleaning cartridges sit in a magazine assigned to a logical library, so your backup
  software (e.g. Bareos) decides when to mount/unmount them; gotochangerd only tracks usage/expiry.
- `backup_robot` - cartridges sit in a magazine **not** assigned to any logical library (invisible to Bareos);
  gotochangerd's own background sweep finds idle drives at/over `mount_threshold` mounts-since-last-cleaning
  and runs the cycle itself, auto-ejecting back to origin when done.

### Create a cleaning cartridge

```sh
gotochangerctl cleaning tape new
gotochangerctl cleaning tape list
```

Up to 5 cleaning cartridges can exist at once, barcoded with a fixed `NNNNNCLN` format (e.g. `00001CLN`) - not
admin-configurable, unlike regular tape sets.

### Run a cleaning cycle manually (`backup_software` mode)

Loading a cleaning cartridge into a drive like any other volume is enough - gotochangerd detects it's a
cleaning tape and branches into the cleaning path automatically:

```sh
gotochangerctl load slot <cleaning-tape-slot> 0
```

## Verify

```sh
gotochangerctl cleaning tape list
gotochangerctl events | grep -i cleaning
```

Expect `CLEANING.CYCLE-START.SUCCESS` then `CLEANING.CYCLE-SUCCESS` events around the load, the drive's
`MountsSinceCleaning` counter reset to 0 for that drive, and the cartridge's own usage count incremented by
one. Once a cartridge's usage count reaches `max_uses`, its state moves to `expired` and it can no longer be
auto-selected or manually loaded (`gotochangerctl load` returns an error) - replace it:

```sh
gotochangerctl cleaning tape new
```

and, if the expired cartridge was one of 5 already at the pool limit, note that a `CLEANING.TAPE-CREATE.FAILURE`
event (pool full) means you need to physically account for/remove the expired one from its magazine before
creating a replacement.
