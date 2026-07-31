# Runtime image for gotochangerd + the gotochanger-changer/gotochangerctl
# admin CLIs (non-kernel-mode only - see the gotochanger-kernel .deb for
# the TCMU/LIO backend, which needs host kernel access a container can't
# provide and isn't shipped as an image).
#
# Single-stage: goreleaser hands this Dockerfile an already cross-compiled
# binary via its own synthetic build context (see .goreleaser.yaml's
# "dockers:" section) - there's nothing to compile here.
#
# debian:trixie-slim, not scratch/musl/Alpine: go-sqlite3 is cgo, so the
# binary is dynamically linked against glibc - matches this project's own
# .deb, which also targets trixie.
FROM debian:trixie-slim

# ca-certificates: gotochangerctl/gotochanger-changer's --url flag can
# point at a remote gotochangerd behind TLS (e.g. a reverse proxy); the
# daemon itself only ever serves plain HTTP, but the admin CLIs shipped in
# this image for that use case need a trust store to verify such a
# remote. debian-slim omits ca-certificates by default.
#
# Fixed numeric UID/GID 999 (Debian's system-ID ceiling, unused by
# trixie-slim's own base packages) instead of the .deb postinstall's
# dynamic "adduser --system" allocation - reproducible across container
# recreations/volume mounts, where a dynamically assigned UID could shift.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid 999 gotochanger \
    && useradd --system --uid 999 --gid gotochanger \
         --home-dir /var/lib/gotochanger --no-create-home \
         --shell /usr/sbin/nologin gotochanger \
    && mkdir -p /var/lib/gotochanger /run/gotochanger /etc/gotochanger

COPY gotochangerd /usr/sbin/gotochangerd
COPY gotochanger-changer /usr/bin/gotochanger-changer
COPY gotochangerctl /usr/bin/gotochangerctl
COPY configs/gotochanger.yaml /etc/gotochanger/config.yaml

# Mirrors systemd/gotochanger.service's own StateDirectoryMode=0770 /
# RuntimeDirectoryMode=0750 / config file mode 0640.
RUN chmod 0755 /usr/sbin/gotochangerd /usr/bin/gotochanger-changer /usr/bin/gotochangerctl \
    && chown gotochanger:gotochanger /var/lib/gotochanger /run/gotochanger /etc/gotochanger/config.yaml \
    && chmod 0770 /var/lib/gotochanger \
    && chmod 0750 /run/gotochanger \
    && chmod 0640 /etc/gotochanger/config.yaml

USER gotochanger:gotochanger

EXPOSE 8480
VOLUME ["/var/lib/gotochanger"]

# GET /api/v1/auth/state is unauthenticated by design (how a client
# discovers whether it needs to log in) - the lightest available liveness
# probe. Plain bash + /dev/tcp instead of curl/wget: debian-slim ships
# bash by default (Priority: required), so this needs no extra package.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD bash -c 'exec 3<>/dev/tcp/127.0.0.1/8480 && printf "GET /api/v1/auth/state HTTP/1.0\r\nHost: localhost\r\n\r\n" >&3 && head -n 1 <&3 | grep -q " 200 "'

# gotochangerd already handles SIGTERM directly (signal.NotifyContext in
# cmd/gotochangerd/main.go) - exec-form ENTRYPOINT makes it PID 1, so
# "docker stop" reaches it with no init/tini needed.
ENTRYPOINT ["/usr/sbin/gotochangerd"]
CMD ["-config", "/etc/gotochanger/config.yaml"]
