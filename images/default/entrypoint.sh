#!/bin/sh
set -u

OS_CA_BUNDLE="/etc/ssl/certs/ca-certificates.crt"
SANDBOX_CA="/etc/sandbox/ca.crt"
COMBINED_CA_BUNDLE="/tmp/ca-bundle.crt"
RUNTIME_HOME="${HOME:-/tmp/sandbox-home}"

mkdir -p "$RUNTIME_HOME" 2>/dev/null || true

# The pod spec should point common TLS clients at COMBINED_CA_BUNDLE with:
# NODE_EXTRA_CA_CERTS, REQUESTS_CA_BUNDLE, SSL_CERT_FILE, GIT_SSL_CAINFO, CURL_CA_BUNDLE.
# Root filesystems are read-only at runtime, so build a user-writable bundle in /tmp.
if [ -r "$OS_CA_BUNDLE" ]; then
    cat "$OS_CA_BUNDLE" > "$COMBINED_CA_BUNDLE" 2>/dev/null || true
else
    : > "$COMBINED_CA_BUNDLE" 2>/dev/null || true
fi

if [ -r "$SANDBOX_CA" ]; then
    cat "$SANDBOX_CA" >> "$COMBINED_CA_BUNDLE" 2>/dev/null || true
fi

if [ -r "$COMBINED_CA_BUNDLE" ]; then
    HOME="$RUNTIME_HOME" git config --global http.sslCAInfo "$COMBINED_CA_BUNDLE" >/dev/null 2>&1 || true
fi

exec sleep infinity
