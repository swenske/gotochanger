#!/usr/bin/env bash
set -euo pipefail

# Quick redeploy helper for gotochanger.
#
# Default behavior:
# 1) Build a Debian package with dpkg-buildpackage.
# 2) Pick the newest ../gotochanger_*.deb artifact.
# 3) Install it locally with dpkg -i.
# 4) Restart and show status for the systemd service.
#
# Remote mode (--host):
# - Copies the .deb to /tmp on the target over scp.
# - Installs with sudo dpkg -i --force-confold --force-confmiss to avoid
#   interactive conffile prompts and to recreate any conffile (e.g.
#   config.yaml) that's missing from disk.
# - Restarts and shows status for the target service.
#
# --kernel additionally builds/installs the optional gotochanger-kernel
# package (gotochanger-tcmud + its systemd unit) - opt-in, since it needs
# root and real kernel TCMU/LIO support, unlike the base package. Its
# postinst loads the kernel module immediately (not just at next boot),
# but nothing in it is started/enabled automatically - kernel mode itself
# stays a separate, explicit "systemctl enable --now gotochanger-tcmud@..."
# step, same as documented in the package's own postinst message.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

HOST=""
SSH_USER=""
SSH_PORT=""
SERVICE="gotochanger"
SKIP_BUILD=0
NO_RESTART=0
DEB_PATH=""
WITH_KERNEL=0
KERNEL_DEB_PATH=""

usage() {
  cat <<'EOF'
Usage: scripts/redeploy.sh [options]

Options:
  --host <hostname>      Remote host to deploy to (default: local machine)
  --user <username>      SSH username for remote deploy
  --port <port>          SSH port for remote deploy
  --service <name>       Systemd service name (default: gotochanger)
  --deb <path>           Use an existing .deb file instead of building
  --skip-build           Skip build step and use newest ../gotochanger_*.deb
  --no-restart           Do not restart systemd service after install
  --kernel               Also install the optional gotochanger-kernel
                          package (gotochanger-tcmud + its systemd unit).
                          Not enabled/started - see its postinst message.
  --kernel-deb <path>    Use an existing gotochanger-kernel .deb instead
                          of the newest ../gotochanger-kernel_*.deb
                          (implies --kernel)
  -h, --help             Show this help

Examples:
  scripts/redeploy.sh
  scripts/redeploy.sh --host bareos-disk-sd-int-fr1.storage.core.vpgrp.io --user swenske
  scripts/redeploy.sh --deb ../gotochanger_0.1.2_amd64.deb --host myhost --user admin
  scripts/redeploy.sh --kernel
EOF
}

die() {
  echo "error: $*" >&2
  exit 1
}

log() {
  echo "==> $*"
}

check_build_deps() {
  local missing=()

  command -v dpkg-buildpackage >/dev/null 2>&1 || missing+=("dpkg-dev")
  command -v dh >/dev/null 2>&1 || missing+=("debhelper")
  command -v go >/dev/null 2>&1 || missing+=("golang-go")
  command -v fakeroot >/dev/null 2>&1 || missing+=("fakeroot")

  if [[ ${#missing[@]} -gt 0 ]]; then
    cat >&2 <<EOF
error: missing build dependencies: ${missing[*]}

Install them with:
  sudo apt update
  sudo apt install -y dpkg-dev debhelper golang-go fakeroot

Or skip building and deploy an existing package with:
  scripts/redeploy.sh --skip-build
  scripts/redeploy.sh --deb ../gotochanger_<version>_<arch>.deb
EOF
    exit 1
  fi
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --host)
      [[ $# -ge 2 ]] || die "--host requires a value"
      HOST="$2"
      shift 2
      ;;
    --user)
      [[ $# -ge 2 ]] || die "--user requires a value"
      SSH_USER="$2"
      shift 2
      ;;
    --port)
      [[ $# -ge 2 ]] || die "--port requires a value"
      SSH_PORT="$2"
      shift 2
      ;;
    --service)
      [[ $# -ge 2 ]] || die "--service requires a value"
      SERVICE="$2"
      shift 2
      ;;
    --deb)
      [[ $# -ge 2 ]] || die "--deb requires a path"
      DEB_PATH="$2"
      shift 2
      ;;
    --skip-build)
      SKIP_BUILD=1
      shift
      ;;
    --no-restart)
      NO_RESTART=1
      shift
      ;;
    --kernel)
      WITH_KERNEL=1
      shift
      ;;
    --kernel-deb)
      [[ $# -ge 2 ]] || die "--kernel-deb requires a path"
      KERNEL_DEB_PATH="$2"
      WITH_KERNEL=1
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "unknown argument: $1"
      ;;
  esac
done

if [[ -n "$DEB_PATH" && "$SKIP_BUILD" -eq 1 ]]; then
  die "--deb and --skip-build cannot be used together"
fi

if [[ -z "$DEB_PATH" ]]; then
  if [[ "$SKIP_BUILD" -eq 0 ]]; then
    check_build_deps
    log "Building Debian package"
    (
      cd "$ROOT_DIR"
      dpkg-buildpackage -us -uc -b
    )
  else
    log "Skipping build (using newest existing package)"
  fi

  DEB_PATH="$(ls -1t "$ROOT_DIR"/../gotochanger_*.deb 2>/dev/null | head -n1 || true)"
  [[ -n "$DEB_PATH" ]] || die "no package found at ../gotochanger_*.deb"
fi

[[ -f "$DEB_PATH" ]] || die "package not found: $DEB_PATH"
DEB_PATH="$(readlink -f "$DEB_PATH")"
BASENAME="$(basename "$DEB_PATH")"

if [[ "$WITH_KERNEL" -eq 1 && -z "$KERNEL_DEB_PATH" ]]; then
  # No separate build step needed here - gotochanger-kernel is a second
  # binary package from the same source, so the dpkg-buildpackage call
  # above (unless --skip-build/--deb bypassed it) already produced both
  # .deb files in one pass.
  KERNEL_DEB_PATH="$(ls -1t "$ROOT_DIR"/../gotochanger-kernel_*.deb 2>/dev/null | head -n1 || true)"
  [[ -n "$KERNEL_DEB_PATH" ]] || die "--kernel given but no package found at ../gotochanger-kernel_*.deb (build without --skip-build/--deb first, or pass --kernel-deb <path>)"
fi
if [[ "$WITH_KERNEL" -eq 1 ]]; then
  [[ -f "$KERNEL_DEB_PATH" ]] || die "gotochanger-kernel package not found: $KERNEL_DEB_PATH"
  KERNEL_DEB_PATH="$(readlink -f "$KERNEL_DEB_PATH")"
  KERNEL_BASENAME="$(basename "$KERNEL_DEB_PATH")"
fi

if [[ -z "$HOST" ]]; then
  log "Installing locally: $DEB_PATH"
  sudo dpkg -i --force-confold --force-confmiss "$DEB_PATH"

  if [[ "$WITH_KERNEL" -eq 1 ]]; then
    log "Installing locally: $KERNEL_DEB_PATH"
    sudo dpkg -i --force-confold --force-confmiss "$KERNEL_DEB_PATH"
  fi

  if [[ "$NO_RESTART" -eq 0 ]]; then
    log "Restarting local service: $SERVICE"
    sudo systemctl restart "$SERVICE"
    sudo systemctl --no-pager --full status "$SERVICE" || true
  fi

  log "Local redeploy complete"
  exit 0
fi

SSH_TARGET="$HOST"
if [[ -n "$SSH_USER" ]]; then
  SSH_TARGET="$SSH_USER@$HOST"
fi

SSH_OPTS=()
if [[ -n "$SSH_PORT" ]]; then
  SSH_OPTS+=("-p" "$SSH_PORT")
fi

REMOTE_DEB="/tmp/$BASENAME"

log "Copying package to $SSH_TARGET:$REMOTE_DEB"
scp "${SSH_OPTS[@]}" "$DEB_PATH" "$SSH_TARGET:$REMOTE_DEB"

log "Installing package on $SSH_TARGET"
ssh "${SSH_OPTS[@]}" "$SSH_TARGET" "sudo dpkg -i --force-confold --force-confmiss '$REMOTE_DEB'"

REMOTE_KERNEL_DEB=""
if [[ "$WITH_KERNEL" -eq 1 ]]; then
  REMOTE_KERNEL_DEB="/tmp/$KERNEL_BASENAME"
  log "Copying package to $SSH_TARGET:$REMOTE_KERNEL_DEB"
  scp "${SSH_OPTS[@]}" "$KERNEL_DEB_PATH" "$SSH_TARGET:$REMOTE_KERNEL_DEB"

  log "Installing package on $SSH_TARGET"
  ssh "${SSH_OPTS[@]}" "$SSH_TARGET" "sudo dpkg -i --force-confold --force-confmiss '$REMOTE_KERNEL_DEB'"
fi

if [[ "$NO_RESTART" -eq 0 ]]; then
  log "Restarting remote service: $SERVICE"
  ssh "${SSH_OPTS[@]}" "$SSH_TARGET" "sudo systemctl restart '$SERVICE' && sudo systemctl --no-pager --full status '$SERVICE' || true"
fi

log "Cleaning up remote package"
ssh "${SSH_OPTS[@]}" "$SSH_TARGET" "rm -f '$REMOTE_DEB' '$REMOTE_KERNEL_DEB'"

log "Remote redeploy complete"
