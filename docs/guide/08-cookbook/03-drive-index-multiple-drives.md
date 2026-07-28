# Configure multiple drives without the Drive Index trap

You're adding a second (or third) drive to an existing Autochanger and want Bareos to actually address each
one independently, instead of every Device silently reporting as "drive 0".

## Prerequisites

- An Autochanger resource with 2+ Device resources already defined in `bareos-sd.d/`.
- 2+ drives already created in gotochanger (`gotochangerctl drive list`).

## Steps

1. List gotochanger's own drives, to confirm their indices and device paths:
   ```sh
   gotochangerctl drive list
   ```
   Expected output (one line per drive):
   ```
   0    /var/lib/gotochanger/drives/drive0
   1    /var/lib/gotochanger/drives/drive1
   ```
2. In the Bareos Autochanger's config, add an explicit `Drive Index` to **every** Device resource, set to
   that Device's 0-based position within *this Autochanger's own* `Device =` list - not gotochangerd's drive
   index (they only happen to match when both start at 0 and are contiguous, which is the common but not
   universal case):
   ```
   Autochanger {
     Name = FakeML3
     Device = Drive0, Drive1
     Changer Command = "/usr/bin/gotochanger-changer %c %o %S %a %d %V"
   }
   Device {
     Name = Drive0
     Drive Index = 0
     ...
   }
   Device {
     Name = Drive1
     Drive Index = 1
     ...
   }
   ```
3. Restart `bareos-sd`.

## Verify

```sh
gotochanger-changer /dev/null loaded 0
gotochanger-changer /dev/null loaded 1
```

Load a different volume into each drive and confirm each Device resource reports the correct one:

```sh
gotochangerctl load slot 1 0
gotochangerctl load slot 2 1
```

```sh
bconsole <<'EOF'
status storage=FakeML3
EOF
```

Expect Drive0/Drive1 to each show their own distinct volume, not both showing the same one. If both Devices
report identical status regardless of which drive actually has the tape, `Drive Index` is missing (or
identical) on one of them - that's the trap: it silently defaults to `0` for any Device that doesn't set it,
so a missing second `Drive Index` makes Bareos treat both Devices as the same physical drive.
