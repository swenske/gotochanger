# Troubleshooting and FAQ

#### I forgot the Admin password.

There's no in-app recovery flow for the built-in account by design (sessions and credentials are kept
deliberately simple). Whoever manages the host running gotochangerd will need to create a new Admin user via
`gotochangerctl user new <name> admin <password>` against the trusted local socket, which bypasses web login
entirely.

#### Why don't I see an Admin button?

You're signed in as a Viewer. Viewer accounts see the dashboard and read-only Admin screens are hidden
entirely, not just disabled - ask an Admin to change your role if you need Operator or Admin access
(`gotochangerctl user role <username> operator`).

#### A Load/Unload/Move button (or command) gave me an error about "no destination available".

That means every eligible destination (an empty drive, slot, or I/O slot, depending on the action) is
currently occupied. Free one up - unload a drive, move a tape out of a slot - and try again.

#### Everything is rejected with a 409 / "robotic arm is in fault state".

The robotic arm has an active simulated fault. Clear it (`gotochangerctl robotic-fault off`, or the Robotic
Arm panel's **Clear fault** button) - door open/close still work while a fault is active, but Load/Unload/
Move are rejected library-wide until it's cleared. See
[Bareos resilience testing](#bareos-resilience-testing-fault-injection) if you triggered this deliberately.

#### My cartridge got a barcode I didn't expect.

Barcodes come entirely from the tape set's tape type format - check Admin > Tape Types for the tape set's
type to see its exact family/media-id/length, or Admin > Tape Sets to confirm which tape type the set
actually uses.

#### Operations feel instantaneous - is latency simulation on?

Check Admin > Latency > Enable. It's off by default on a fresh install; turning it on (with the "Load
defaults" prefill, or your own tuned values) is also what makes the drive's "active operation" light visibly
blink for more than an instant - see [Drive Indicator Lights](#drive-indicator-lights).

#### Does gotochanger create a real `/dev/sg*` or `/dev/nst*` device?

Only if you've explicitly enabled [Kernel Mode](#kernel-mode) (the separate `gotochanger-kernel` package,
`operational_mode=kernel`). By default gotochanger runs in userspace/file mode, where a loaded drive is a
plain symlink at a configured device path and Bareos never needs a kernel SCSI device for this integration -
it just calls a Changer Command script. Kernel mode exists only for third-party tools that insist on a real
device node; see [Switch to kernel mode](#switch-to-kernel-mode-for-a-third-party-scsi-tool).

#### A magazine/mailbox door won't open - "PIN required" or "invalid PIN".

That magazine or mailbox has a 4-digit PIN configured (Admin > Settings > PIN). Pass it as the optional `pin`
argument: `gotochangerctl storage-door <id> open <pin>` / `gotochangerctl io-door <id> open <pin>`. An empty
PIN in Admin > Settings clears the requirement entirely.

#### The Swagger UI at `/docs` doesn't show every endpoint this guide documents.

Correct, and known - the static OpenAPI spec currently documents the most commonly used routes but hasn't
been fully expanded to cover every admin/topology endpoint yet. Treat
[CLI Reference and REST API](#cli-and-rest-api-reference) as the authoritative list until that's addressed;
every route listed there works today even if Swagger doesn't render it yet.

#### Where do I configure Bareos itself to talk to gotochanger?

See [Bareos Integration](#bareos-integration) - Autochanger/Device resources, the Drive Index gotcha, and
scoping one Autochanger to one logical library - or generate a ready-to-paste config skeleton from Admin >
Logical Libraries > "Bareos Config".
