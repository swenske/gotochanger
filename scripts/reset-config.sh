#!/usr/bin/env bash
set -euo pipefail

# Reset gotochanger to a fresh-install state: local machine by default, or a
# remote host via --host (mirrors scripts/redeploy.sh's local/remote split).
#
# This wipes:
# - /etc/gotochanger/config.yaml, users.json, tokens.json
# - /var/lib/gotochanger (the whole data dir: state.db, volumes, everything)
#
# and restores config.yaml from this repo's configs/gotochanger.yaml (the
# same file the .deb package installs) so the daemon has something to boot
# from. users.json/tokens.json are intentionally NOT recreated here - the
# daemon creates them itself on next start with a fresh, password-unset
# Admin account (LoadOrBootstrapUserStore/LoadOrBootstrapTokenStore).
#
# This script never touches the installed package - it does not install,
# reinstall, or upgrade the .deb. Use scripts/redeploy.sh for that.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
DEFAULT_CONFIG="$ROOT_DIR/configs/gotochanger.yaml"

HOST=""
SSH_USER=""
SSH_PORT=""
SERVICE="gotochanger"
NO_RESTART=0
ASSUME_YES=0

usage() {
  cat <<'EOF'
Usage: scripts/reset-config.sh [options]

Wipes gotochanger's config/state back to a fresh-install state, without
reinstalling the package. Defaults to the local machine.

Options:
  --host <hostname>      Remote host to reset (default: local machine)
  --user <username>      SSH username for remote reset
  --port <port>          SSH port for remote reset
  --service <name>       Systemd service name (default: gotochanger)
  --no-restart           Do not restart systemd service after reset
  -y, --yes              Skip the confirmation prompt
  -h, --help             Show this help

Examples:
  scripts/reset-config.sh
  scripts/reset-config.sh --host bareos-disk-sd-int-fr1.storage.core.vpgrp.io --user swenske
EOF
}

die() {
  echo "error: $*" >&2
  exit 1
}

log() {
  echo "==> $*"
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
    --no-restart)
      NO_RESTART=1
      shift
      ;;
    -y|--yes)
      ASSUME_YES=1
      shift
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

[[ -f "$DEFAULT_CONFIG" ]] || die "default config not found: $DEFAULT_CONFIG"

TARGET_DESC="the local machine"
[[ -z "$HOST" ]] || TARGET_DESC="$HOST"

if [[ "$ASSUME_YES" -eq 0 ]]; then
  cat <<EOF
This will WIPE ALL gotochanger data and config on $TARGET_DESC:
  - /etc/gotochanger/config.yaml, users.json, tokens.json
  - /var/lib/gotochanger (state.db, volumes, everything)

This cannot be undone. Take a backup first (Admin > Backup /
'gotochangerctl backup download') if this host holds data you need.
EOF
  read -r -p "Type 'yes' to continue: " confirm
  [[ "$confirm" == "yes" ]] || die "aborted"
fi

if [[ -z "$HOST" ]]; then
  log "Stopping local service: $SERVICE"
  sudo systemctl stop "$SERVICE"

  log "Wiping local config and data"
  sudo rm -f /etc/gotochanger/config.yaml /etc/gotochanger/users.json /etc/gotochanger/tokens.json
  sudo rm -rf /var/lib/gotochanger

  log "Recreating data directory"
  sudo mkdir -p /var/lib/gotochanger
  sudo chown gotochanger:gotochanger /var/lib/gotochanger
  sudo chmod 770 /var/lib/gotochanger

  log "Installing default config.yaml"
  sudo install -o gotochanger -g gotochanger -m 0640 "$DEFAULT_CONFIG" /etc/gotochanger/config.yaml

  if [[ "$NO_RESTART" -eq 0 ]]; then
    log "Restarting local service: $SERVICE"
    sudo systemctl restart "$SERVICE"
    sudo systemctl --no-pager --full status "$SERVICE" || true
  fi

  log "Local reset complete"
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

REMOTE_CONFIG="/tmp/gotochanger-config.yaml.$$"

log "Stopping remote service: $SERVICE on $SSH_TARGET"
ssh "${SSH_OPTS[@]}" "$SSH_TARGET" "sudo systemctl stop '$SERVICE'"

log "Copying default config.yaml to $SSH_TARGET:$REMOTE_CONFIG"
scp "${SSH_OPTS[@]}" "$DEFAULT_CONFIG" "$SSH_TARGET:$REMOTE_CONFIG"

log "Wiping remote config and data, reinstalling default config.yaml"
ssh "${SSH_OPTS[@]}" "$SSH_TARGET" "
  set -e
  sudo rm -f /etc/gotochanger/config.yaml /etc/gotochanger/users.json /etc/gotochanger/tokens.json
  sudo rm -rf /var/lib/gotochanger
  sudo mkdir -p /var/lib/gotochanger
  sudo chown gotochanger:gotochanger /var/lib/gotochanger
  sudo chmod 770 /var/lib/gotochanger
  sudo install -o gotochanger -g gotochanger -m 0640 '$REMOTE_CONFIG' /etc/gotochanger/config.yaml
  rm -f '$REMOTE_CONFIG'
"

if [[ "$NO_RESTART" -eq 0 ]]; then
  log "Restarting remote service: $SERVICE"
  ssh "${SSH_OPTS[@]}" "$SSH_TARGET" "sudo systemctl restart '$SERVICE' && sudo systemctl --no-pager --full status '$SERVICE' || true"
fi

log "Remote reset complete"
