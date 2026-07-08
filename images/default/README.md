# Default sandbox image

This directory contains the curated default image for code sandboxes. It is intended for hardened Kubernetes pods that run untrusted LLM-authored code with a read-only root filesystem, a writable `/tmp` emptyDir, and a workspace volume mounted at `/workspace`.

## Installed tools

The image is based on `debian:stable-slim` and installs:

- `git`
- `curl`
- `ca-certificates`
- `openssh-client`
- `python3`
- `python3-pip`
- `nodejs` and `npm`
- `build-essential`
- `jq`
- `ripgrep`
- `less`
- `tar`
- `gzip`

## Runtime conventions

- Default user: `sandbox`
- UID: `1000`
- Working directory: `/workspace`
- `/workspace` is owned by UID `1000`
- The container entrypoint stays alive with `sleep infinity`; the orchestrator creates persistent shells and runs commands via exec.
- At runtime, pods should provide writable mounts only where needed, especially `/tmp` and `/workspace`. The root filesystem may be read-only.

## MITM CA injection

`entrypoint.sh` creates a writable combined CA bundle at:

```text
/tmp/ca-bundle.crt
```

The bundle is built from the OS CA bundle at `/etc/ssl/certs/ca-certificates.crt`. If the orchestrator mounts a CA certificate at `/etc/sandbox/ca.crt`, it is appended to that bundle. This avoids requiring root or `update-ca-certificates` on a read-only root filesystem.

The entrypoint also runs:

```sh
git config --global http.sslCAInfo /tmp/ca-bundle.crt
```

The pod spec must set these environment variables to `/tmp/ca-bundle.crt` so common tooling trusts the injected CA:

- `NODE_EXTRA_CA_CERTS=/tmp/ca-bundle.crt`
- `REQUESTS_CA_BUNDLE=/tmp/ca-bundle.crt`
- `SSL_CERT_FILE=/tmp/ca-bundle.crt`
- `GIT_SSL_CAINFO=/tmp/ca-bundle.crt`
- `CURL_CA_BUNDLE=/tmp/ca-bundle.crt`

If `/etc/sandbox/ca.crt` is absent, the entrypoint continues normally and does not fail.

## Supplying a custom image

Users may supply their own sandbox image if it follows these requirements:

1. Keep the main container process alive so the orchestrator can exec commands into it.
2. Do not require root at runtime; run as UID `1000` where possible.
3. Use `/workspace` as the working directory and writable workspace mount.
4. Support a writable `/tmp` for runtime-generated files.
5. Trust an orchestrator-provided certificate at `/etc/sandbox/ca.crt`, preferably by creating or using `/tmp/ca-bundle.crt` and honoring the CA environment variables listed above.

