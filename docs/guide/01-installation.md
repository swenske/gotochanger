# Installation

gotochanger ships as two Debian binary packages built from the same source tree - `gotochanger` (the daemon,
the Bareos changer shim, and the admin CLI) and the optional `gotochanger-kernel` add-on (see
[Kernel Mode](#kernel-mode)) - plus a Docker image covering the `gotochanger` package's contents (no
`gotochanger-kernel` image exists yet, see [Run with Docker](#run-with-docker) below for why). All paths
below produce the same binaries; pick whichever fits your environment.

## Install from a `.deb` package

Pre-built `.deb` packages for Debian 13 (trixie) (`gotochanger` and the optional
`gotochanger-kernel` add-on) are published to an apt repository after
every release:

```sh
sudo install -d -m 0755 /etc/apt/keyrings
curl -fsSL https://apt.sw-servers.net/apt-sw-servers.net.gpg.asc | sudo gpg --dearmor -o /etc/apt/keyrings/gotochanger.gpg
echo "deb [signed-by=/etc/apt/keyrings/gotochanger.gpg] https://apt.sw-servers.net/gotochanger trixie main" \
  | sudo tee /etc/apt/sources.list.d/gotochanger.list
sudo apt-get update
sudo apt-get install gotochanger
# optional, only if this deployment needs real /dev/sg*/dev/nst* devices:
sudo apt-get install gotochanger-kernel
```

Every tagged release also publishes both `.deb`s (and plain binary tarballs) as
[GitHub Releases](https://github.com/swenske/gotochanger/releases) assets - installable directly with
`sudo apt install ./gotochanger_<version>_amd64.deb` without configuring a repository at all.

## Build from source

```sh
make build            # binaries land in ./bin
make test             # go test ./...
sudo make install DESTDIR=/some/root   # same install layout debian/rules uses
```

Dependencies are vendored (`vendor/`), so this never needs network access. `make build` also regenerates the
embedded User Guide from `docs/guide/**.md` first (`make guide`) - see
[CLI Reference and REST API](#cli-and-rest-api-reference) if you're editing the guide itself and want to
preview it with `make site` before publishing.

## Build the Debian package directly

```sh
dpkg-buildpackage -us -uc -b
```

Produces the same two binary packages (`gotochanger`, `gotochanger-kernel`) from a plain git checkout, no
network access required.

## Run with Docker

A Docker image covering the `gotochanger` package's contents (`gotochangerd`, `gotochanger-changer`,
`gotochangerctl`) is published to Docker Hub after every release:

```sh
docker pull swenske/gotochanger:latest
docker run -d --name gotochanger \
  -p 8480:8480 \
  -v gotochanger-data:/var/lib/gotochanger \
  swenske/gotochanger:latest
```

`-v gotochanger-data:/var/lib/gotochanger` persists `state.db` (topology, users, tokens, volumes - everything
except `data_dir`/`listen`, which come from the config file baked into the image) across container restarts.
The web UI is then reachable at `http://<host>:8480/` - continue with
[First Run and Setup Wizard](#first-run-and-setup-wizard).

Only `linux/amd64` is published, matching every other release artifact. There is no `gotochanger-kernel`
image: `gotochanger-tcmud` needs real host kernel/TCMU access, and `gotochangerd`'s kernel-mode reconciler
manages it via `systemctl` talking to a real host systemd/polkit - neither is available inside a plain
container, so kernel mode currently requires a `.deb` install (see [Kernel Mode](#kernel-mode)).

To read the one-time bootstrap admin API token (same as the systemd/journalctl flow below, just via
`docker logs`):

```sh
docker logs gotochanger 2>&1 | grep 'bootstrap API token'
```

The admin CLI is included in the image for one-off commands. Against the same container's trusted Unix
socket:

```sh
docker exec gotochanger gotochangerctl status
```

Or, from anywhere, against a remote gotochangerd (`--url`/`--token`, see
[CLI Reference and REST API](#cli-and-rest-api-reference)):

```sh
docker run --rm --entrypoint gotochangerctl swenske/gotochanger \
  --url http://<host>:8480 --token <api-token> status
```

## First start

```sh
sudo systemctl status gotochanger
journalctl -u gotochanger | grep 'bootstrap API token'   # save this - it's an admin-scoped token, shown once
```

Then open the web UI at `http://<host>:8480/` (or whatever `listen.http` is set to in
`/etc/gotochanger/config.yaml`) and continue with [First Run and Setup Wizard](#first-run-and-setup-wizard).
