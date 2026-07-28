# Set up a scoped operator API token for a CI script

An external CI job needs to drive day-to-day operations (load/unload/move, inject a fault for a resilience
test, read status) against gotochangerd, but should never be able to touch Users, Tokens, Settings, or Backup
- exactly what the `operator` role is for.

## Prerequisites

- Admin access to gotochangerd.
- The CI system's secret-storage mechanism, to hold the token once issued.

## Steps

1. Create a scoped token - the raw value is only ever shown once, right here:
   ```sh
   gotochangerctl token new ci-pipeline operator
   ```
   Equivalently via curl:
   ```sh
   curl -X POST --unix-socket /run/gotochanger/gotochanger.sock \
     http://localhost/api/v1/tokens \
     -H 'Content-Type: application/json' \
     -d '{"name":"ci-pipeline","role":"operator"}'
   ```
2. Store the returned value in your CI system's secret store (e.g. a GitHub Actions repository secret,
   `GOTOCHANGER_TOKEN`) - it will never be displayed again; only its SHA-256 hash is kept server-side.
3. Use it from the CI script against the TCP listener (the trusted Unix socket isn't reachable from outside
   the host, and always behaves as Admin anyway, which defeats the point of scoping):
   ```sh
   curl -H "X-Api-Key: $GOTOCHANGER_TOKEN" http://gotochanger-host:8480/api/v1/status
   ```
   or with `gotochangerctl --url http://gotochanger-host:8480 --token "$GOTOCHANGER_TOKEN" status`.

## Verify

```sh
gotochangerctl token list
```

Expect `ci-pipeline` listed with role `operator`. Confirm the scope is actually enforced - an Admin-only route
should be rejected:

```sh
curl -s -o /dev/null -w '%{http_code}\n' -H "X-Api-Key: $GOTOCHANGER_TOKEN" \
  http://gotochanger-host:8480/api/v1/users
```

Expect `403`, not `200` - proving the token can't manage users/tokens/settings/backups even though it can
freely load/unload/move volumes and inject faults. When the CI script is decommissioned, revoke it rather
than leaving it live:

```sh
gotochangerctl token revoke ci-pipeline
```
