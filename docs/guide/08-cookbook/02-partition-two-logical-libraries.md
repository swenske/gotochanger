# Partition one physical robot into two logical libraries

You have one gotochangerd instance with enough drives/magazines/mailboxes to serve two separate Bareos
Storage Daemons, and want each SD to see its own independent Autochanger without either one being able to
touch the other's media.

## Prerequisites

- At least 4 drives and 2 magazines (`mag1`, `mag2`) and 2 mailboxes (`mbx1`, `mbx2`) already configured - one
  magazine/mailbox pair and two drives per logical library, minimum.
- Admin access to gotochangerd.

## Steps

1. Check what's currently unassigned, to confirm the drive indices and magazine/mailbox IDs you'll use:
   ```sh
   gotochangerctl unassigned
   ```
2. Create the two logical libraries, assigning drives/magazines/mailboxes directly at creation time
   (`logical-library new <name> <drive-indices csv> <magazine-ids csv> <mailbox-ids csv>`):
   ```sh
   gotochangerctl logical-library new Library1 0,1 mag1 mbx1
   gotochangerctl logical-library new Library2 2,3 mag2 mbx2
   ```
   Equivalently via the REST API:
   ```sh
   curl -X POST --unix-socket /run/gotochanger/gotochanger.sock \
     http://localhost/api/v1/logical-libraries \
     -H 'Content-Type: application/json' \
     -d '{"name":"Library1","drives":[0,1],"magazines":["mag1"],"mailboxes":["mbx1"]}'
   ```
3. In the Admin web UI, open **Logical Libraries > Library1** and click **Bareos Config** - this generates a
   ready-to-paste Autochanger/Device block with the correct `Drive Index` values already filled in for
   Library1's two drives. Repeat for Library2. If you'd rather build the block by hand, `gotochangerctl
   logical-library show Library1` and `gotochangerctl drive list` return the same underlying data.
4. Paste each generated block into the corresponding Bareos Storage Daemon's config, giving each Autochanger
   its own `--logical-library` suffix on the `Changer Command` line:
   ```
   Changer Command = "/usr/bin/gotochanger-changer %c %o %S %a %d %V --logical-library=Library1"
   ```
   ```
   Changer Command = "/usr/bin/gotochanger-changer %c %o %S %a %d %V --logical-library=Library2"
   ```
5. Restart both Storage Daemons.

## Verify

```sh
gotochangerctl --logical-library=Library1 status
gotochangerctl --logical-library=Library2 status
```

Each should show only its own drives/slots/ioslots, with dense addresses starting from 1 (0 for drives)
within that scope - the physical addresses underneath are unaffected. Confirm cross-library isolation by
attempting a move that crosses the boundary from Library1 into a slot that belongs to `mag2`:

```sh
gotochangerctl --logical-library=Library1 move slot 1 slot <a-mag2-slot-address>
```

Expect an error rejecting the move because the destination isn't in `Library1` - this is exactly what keeps
the two Bareos SDs from touching each other's media while sharing one physical robot.
