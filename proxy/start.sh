#!/bin/sh
# Entrypoint for the sandbox MITM proxy.
#
# Runs mitmdump directly (bypassing the base image's root-only usermod
# entrypoint) as a non-root user. The shared CA is mounted read-only at /ca;
# we copy it into a writable confdir so mitmproxy can load the existing CA
# (and write any runtime files it needs) without touching the read-only mount.
set -eu

CONFDIR="${MITMPROXY_CONFDIR:-/home/mitmproxy/.mitmproxy}"
mkdir -p "$CONFDIR"

if [ -d /ca ]; then
    for f in /ca/*; do
        [ -e "$f" ] || continue
        cp "$f" "$CONFDIR/$(basename "$f")"
    done
fi

exec mitmdump \
    --listen-port 8080 \
    -s /addons/inject.py \
    --set confdir="$CONFDIR"
