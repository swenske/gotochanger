#!/bin/sh
# nfpm postinstall for the "gotochanger" package - translated 1:1 from
# debian/gotochanger.postinst (service account creation, config file
# permissions, welcome message), plus explicit systemd enable/start/
# restart logic that debhelper's dh_installsystemd would otherwise inject
# via the "#DEBHELPER#" token. nfpm has no equivalent token, so this is
# spelled out directly with plain systemctl calls (a deliberate, minor
# simplification vs. dh_installsystemd's deb-systemd-helper/policy-rc.d
# indirection - that indirection only matters inside build chroots, not
# on a real install/upgrade/remove).
set -e

GTC_USER=gotochanger
GTC_GROUP=gotochanger
GTC_HOME=/var/lib/gotochanger

case "$1" in
  configure)
    if ! getent group "$GTC_GROUP" >/dev/null; then
      addgroup --system "$GTC_GROUP"
    fi
    if ! getent passwd "$GTC_USER" >/dev/null; then
      adduser --system --ingroup "$GTC_GROUP" --home "$GTC_HOME" \
        --no-create-home --disabled-password --disabled-login \
        --shell /usr/sbin/nologin \
        --gecos "gotochanger virtual autochanger service" "$GTC_USER"
    fi

    mkdir -p "$GTC_HOME"
    chown "$GTC_USER:$GTC_GROUP" "$GTC_HOME"
    chmod 0770 "$GTC_HOME"

    mkdir -p /etc/gotochanger
    chown "$GTC_USER:$GTC_GROUP" /etc/gotochanger
    chmod 0750 /etc/gotochanger
    if [ -f /etc/gotochanger/config.yaml ]; then
      chown "$GTC_USER:$GTC_GROUP" /etc/gotochanger/config.yaml
      chmod 0640 /etc/gotochanger/config.yaml
    fi

    if [ -d /run/systemd/system ]; then
      systemctl daemon-reload >/dev/null 2>&1 || true
      if [ -z "$2" ]; then
        # Fresh install ($2, the old version, is empty on a first
        # configure) - enable and start, matching what dh_installsystemd
        # would have done for a unit with [Install] WantedBy=.
        systemctl enable gotochanger.service >/dev/null 2>&1 || true
        systemctl start gotochanger.service >/dev/null 2>&1 || true
      else
        # Upgrade - restart only if it was already running.
        systemctl try-restart gotochanger.service >/dev/null 2>&1 || true
      fi
    fi

    cat <<'EOF'

gotochanger installed.

  * Config file:     /etc/gotochanger/config.yaml
  * Service account:  gotochanger:gotochanger
  * Trusted socket:  /run/gotochanger/gotochanger.sock (group gotochanger)
  * Web UI:          http://0.0.0.0:8480/

Open the web UI to set the built-in Admin account's password (required on
first visit), then use its Admin section to create Operator/Viewer users
and scoped API tokens for automation. A one-time admin-scoped API token is
also generated for scripting and printed once to the journal:

  journalctl -u gotochanger | grep 'bootstrap API token'

To allow another service account (e.g. bareos) to use gotochanger-changer
against the trusted local socket and read/write volume files, add it to
the gotochanger group:

  adduser bareos gotochanger

EOF
    ;;
esac

exit 0
