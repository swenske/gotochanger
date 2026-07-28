# Installation

gotochanger ships as two Debian binary packages built from the same source tree - `gotochanger` (the daemon,
the Bareos changer shim, and the admin CLI) and the optional `gotochanger-kernel` add-on (see
[Kernel Mode](#kernel-mode)). All three installation paths below produce the same binaries; pick whichever
fits your environment.

## Install from a `.deb` package

If your organization publishes gotochanger to an apt repository, installation is the usual:

```sh
sudo install -d -m 0755 /etc/apt/keyrings
curl -fsSL <your-repo-signing-key-url> | sudo gpg --dearmor -o /etc/apt/keyrings/gotochanger.gpg
echo "deb [signed-by=/etc/apt/keyrings/gotochanger.gpg] <your-repo-url> trixie main" \
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

## First start

```sh
sudo systemctl status gotochanger
journalctl -u gotochanger | grep 'bootstrap API token'   # save this - it's an admin-scoped token, shown once
```

Then open the web UI at `http://<host>:8480/` (or whatever `listen.http` is set to in
`/etc/gotochanger/config.yaml`) and continue with [First Run and Setup Wizard](#first-run-and-setup-wizard).
