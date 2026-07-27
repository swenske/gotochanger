#!/bin/sh
# nfpm postinstall for the "gotochanger-kernel" package - translated 1:1
# from debian/gotochanger-kernel.postinst. Deliberately does NOT
# enable/start gotochanger-tcmud@.service: it's a template unit with no
# concrete instance, and which instances should run is decided at runtime
# by gotochangerd's own kernel-mode reconciler (or an admin's explicit
# "systemctl enable --now gotochanger-tcmud@<name>"), not by this postinst.
set -e

case "$1" in
  configure)
    # /etc/modules-load.d/gotochanger-kernel.conf (installed by this
    # package) makes target_core_mod/target_core_user/tcm_loop load
    # automatically at the *next* boot - that alone would leave kernel
    # mode unusable until a reboot on a system that's already running.
    # Load them right now too, best-effort: a failure here (e.g. this
    # kernel wasn't built with target_core_user) must not abort the
    # package install, since the modules-load.d entry still makes a
    # future boot work once a suitable kernel is running.
    for mod in target_core_mod target_core_user tcm_loop; do
      modprobe "$mod" 2>/dev/null || echo "gotochanger-kernel: could not load kernel module '$mod' now - it will load at next boot instead (see /etc/modules-load.d/gotochanger-kernel.conf), or run 'modprobe $mod' by hand" >&2
    done

    # Make the freshly-installed gotochanger-tcmud@.service template and
    # the polkit rule visible to systemd/polkitd. The polkit rule itself
    # needs no reload - polkitd watches its rules.d directory.
    if [ -d /run/systemd/system ]; then
      systemctl daemon-reload >/dev/null 2>&1 || true
    fi

    cat <<'EOF'

gotochanger-kernel installed.

gotochangerd will now automatically start/stop gotochanger-tcmud@<name>
instances to match your logical libraries whenever the deployment's
operational mode is "kernel" (set via the setup wizard or Admin API) - no
manual systemctl step needed. One instance per logical library, or a
single @default instance covering the whole physical library when none
are configured yet.

Admin > Logical Libraries' "Kernel Mode Setup" button in the gotochanger
web UI still prints the exact systemctl command, useful for manual control
or to see what's expected to be running.

EOF
    ;;
esac

exit 0
