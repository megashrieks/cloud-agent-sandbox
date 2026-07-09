# Default sandbox image

This directory contains the curated default image for code sandboxes. It is intended for hardened Kubernetes pods that run untrusted LLM-authored code with a read-only root filesystem, a writable `/tmp` emptyDir, and a workspace volume mounted at `/workspace`.

## Installed tools

The image is based on `debian:stable-slim` and installs a **polyglot** toolchain:

**Languages & runtimes**
- **Python**: `python3`, `python3-pip`, `python3-venv`
- **Node.js**: `nodejs`, `npm` (yarn/pnpm via `npm`/`corepack`)
- **Go**: installed to `/usr/local/go` (see `GO_VERSION` build arg)
- **Rust**: `rustup` + `cargo` (toolchain in `/opt/rust`)
- **Java**: `default-jdk` + **Maven** (`mvn`) + **Gradle** (`/opt/gradle`)
- **.NET**: `dotnet` SDK in `/usr/local/dotnet` (see `DOTNET_CHANNEL` build arg)
- **C/C++**: `build-essential`, `pkg-config`, `libssl-dev`

**Tooling**
- `git`, `gh` (GitHub CLI), `openssh-client`
- `curl`, `wget`, `ca-certificates`
- Data manipulation: `jq`, `ripgrep`, `grep`, `sed`, `gawk`, `findutils`, `coreutils`, `diffutils`, `patch`
- Archives: `tar`, `gzip`, `bzip2`, `xz-utils`, `zip`, `unzip`
- `less`

### Toolchain caches on a read-only root filesystem

The sandbox runs with a **read-only root FS** and **capped ephemeral storage**, so every
toolchain cache/home is redirected to the writable `/tmp` tmpfs via image `ENV` (and
pre-created by `entrypoint.sh`):

| Tool | Env var | Path |
|---|---|---|
| Go | `GOPATH` / `GOCACHE` / `GOMODCACHE` | `/tmp/go*` |
| Rust | `CARGO_HOME` (toolchain read-only in `/opt/rust`) | `/tmp/cargo` |
| Gradle | `GRADLE_USER_HOME` | `/tmp/gradle` |
| Maven | `HOME` → `~/.m2` | `/tmp/sandbox-home/.m2` |
| .NET | `DOTNET_CLI_HOME` / `NUGET_PACKAGES` | `/tmp/dotnet-home`, `/tmp/nuget` |
| pip | `PIP_CACHE_DIR` | `/tmp/pip-cache` |
| npm | `npm_config_cache` | `/tmp/npm-cache` |

> Heavy .NET / JVM restores can approach the default 2Gi ephemeral-storage limit. For
> large dependency trees, build under `/workspace` (the PVC) or raise
> `SANDBOX_EPHEMERAL_STORAGE_LIMIT` on the orchestrator.

### Package-registry network access

All egress is forced through the MITM proxy, which is **closed by default**. The proxy
allowlist (`proxy/addons/inject.py` → `PUBLIC_EGRESS_HOSTS`) permits the standard PUBLIC
registries so `pip`, `npm`, `go`, `cargo`, `mvn`/`gradle`, and `dotnet`/`nuget` can fetch
dependencies **without** any credential injection: PyPI, npm, proxy.golang.org/sum.golang.org,
crates.io, Maven Central, Gradle distributions, and NuGet. Broad object stores (GCS/S3/Azure
Blob) are intentionally *not* allowed (exfiltration risk). Private registries: add them to
`HOST_RULES` with a token instead.

> `gh` and `git` route their HTTPS traffic through the MITM proxy, which injects
> credentials. For `gh` API calls to work, the proxy addon's host rules must
> cover `api.github.com` (and the sandbox must trust the injected CA, see below).

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

