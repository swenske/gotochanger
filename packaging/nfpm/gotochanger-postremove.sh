#!/bin/sh
# nfpm postremove for the "gotochanger" package - translated 1:1 from
# debian/gotochanger.postrm (purge cleanup), plus explicit systemd
# stop/disable that dh_installsystemd would otherwise inject automatically.
set -e

case "$1" in
  remove)
    if [ -d /run/systemd/system ]; then
      systemctl stop gotochanger.service >/dev/null 2>&1 || true
    fi
    ;;
  purge)
    if [ -d /run/systemd/system ]; then
      systemctl disable gotochanger.service >/dev/null 2>&1 || true
      systemctl daemon-reload >/dev/null 2>&1 || true
    fi
    rm -rf /etc/gotochanger
    rm -rf /var/lib/gotochanger
    if getent passwd gotochanger >/dev/null; then
      deluser --system gotochanger || true
    fi
    if getent group gotochanger >/dev/null; then
      delgroup --system gotochanger || true
    fi
    ;;
esac

exit 0
