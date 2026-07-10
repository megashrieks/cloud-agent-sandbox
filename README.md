# Cloud Agent Sandbox

A Kubernetes-backed orchestrator that gives **cloud coding agents (LLMs)** a safe, disposable place
to run **untrusted, agent-authored code** (clone a repo, install tooling, run builds and tests,
edit files, and open pull requests) **without ever exposing real git credentials to the code it
runs**.

The agent drives sandboxes over **MCP** (streamable HTTP). Every request carries an `X-Session-Id`
header that names the sandbox; the orchestrator get-or-creates it on first use and reuses it on
later calls, so the agent just keeps talking to the same session id. Anything the code does over the
network that needs authentication (e.g. `git push`, `gh`, API calls to GitHub/GitLab) is
transparently authenticated by a **MITM proxy sidecar** that injects credentials in flight. The
sandbox trusts the proxy's CA but **never sees the token**, and the orchestrator itself holds no
credentials. It does pure orchestration.

## Why

Cloud agents increasingly need to *execute* the code they write, not just generate it. Doing that
safely means two hard problems at once:

1. **Untrusted execution.** The code may be buggy or actively malicious. It must not be able to
   break out of its sandbox or reach anything it shouldn't.
2. **Credential safety.** The agent needs to push branches and open PRs, but handing a real token
   to arbitrary code is a credential-leak waiting to happen.

This project solves both: strong per-sandbox isolation for (1), and a credential-injecting proxy so
the token lives *outside* the sandbox for (2).

## How it works

```
        MCP (streamable HTTP)
Agent ──────────────────────▶ Orchestrator ──▶ Kubernetes
                                   │              ├─ sandbox pod (untrusted code, no creds)
                                   │              │     │ egress-only ──▶ MITM proxy
                                   └──────────────┴─────┘     (injects git credentials)
```

- The **orchestrator** exposes MCP tools, manages sandbox lifecycle, a warm pool, and lifetime
  cleanup. It never holds credentials.
- Each **sandbox** is a hardened pod running an image the agent chooses. Its only network egress is
  to its assigned proxy (default-deny `NetworkPolicy`).
- The **MITM proxy** is the only component with credentials. A user-editable addon decides which
  hosts to authenticate and how (e.g. mint a GitHub App installation token and inject it). All other
  egress is passed through untouched, so nothing is blocked, only authenticated where configured.

## Key properties

- **Isolation:** hardened container baseline + optional **gVisor** / **Kata** via Kubernetes
  `RuntimeClass`; dropped capabilities, seccomp, no service-account token, egress-only networking.
- **Credential safety:** real tokens live only in the user-configured proxy addon; the sandbox
  trusts an injected CA but never sees the token.
- **Bring-your-own image:** the agent picks any container image (`alpine`, `python`, a language
  toolchain, etc.) and installs whatever it needs at runtime (as root, writable root fs by default).
- **Open, authenticated egress:** the proxy authenticates configured hosts and passes everything
  else through, with no allowlist to maintain.
- **Lifecycle:** running sandboxes reaped after **1h** (or 10m idle), stopped sandboxes purged after
  **24h**; a self-healing sweep reconciles against Kubernetes so restarts never leak orphans.
- **Scaling:** configurable warm pool (min idle ready), max running, max stopped; the proxy fleet
  autoscales (~1 proxy per 100 sandboxes, scale-to-zero when idle).
- **Agent-optimized tools:** header-driven sessions (`X-Session-Id`), sync/async `shell`
  (persistent, stateful) with `shell_poll` / `shell_wait` / `shell_stop`, a single
  `str_replace_based_edit_tool` (view / create / str_replace / insert), plus `create_sandbox` and
  `clear_session`.
- **Authenticated control plane:** the REST and MCP surfaces require a shared key
  (`Authorization: Bearer <SANDBOX_API_KEY>`); the orchestrator refuses to start without one.

## MCP tools

Identity travels in request headers, not tool arguments: `X-Session-Id` (required) selects the
sandbox, and optional `X-Org-Id` / `X-User-Id` are recorded for attribution. A sandbox is
auto-created the first time a session id is seen.

| Tool | Purpose |
|------|---------|
| `create_sandbox` | Provision or re-provision this session's sandbox with a specific `image`, `use_kata`, `run_as_root`, or `writable_root`. Optional: sandboxes are auto-created on first use. |
| `clear_session` | Delete this session's sandbox and workspace; a later request with the same id starts fresh. |
| `shell` | Run a command synchronously in the persistent sandbox shell. |
| `shell_async` | Start a long-running command in the background; returns a job id. |
| `shell_poll` | Fetch current status/output of an async job. |
| `shell_wait` | Block until an async job finishes (or a timeout). |
| `shell_stop` | Terminate a running async job. |
| `str_replace_based_edit_tool` | File operations: `view` (with `view_range`), `create`, `str_replace`, `insert`. |

## Layout

```
cmd/orchestrator      main entrypoint
internal/config       typed configuration
internal/session      session model + in-memory store
internal/runtime      K8s sandbox lifecycle (pods/PVCs, RuntimeClass, hardened spec)
internal/exec         K8s exec: shell (sync/async) + file editing
internal/mcp          streamable-HTTP MCP server + tools
internal/api          REST API (session create/get/stop/delete/list)
internal/pool         warm pool + scaling
internal/reaper       lifetime enforcement + orphan reconcile
internal/proxy        mitmproxy pool management, assignment + autoscaling
internal/netpolicy    egress NetworkPolicy management
proxy/                mitmproxy image + user-editable addon (CA + credential injection)
images/default        curated default sandbox image
deploy/               kind config, RuntimeClasses, NetworkPolicies, seccomp, manifests
docs/                 architecture, threat model, operator guide
```

## Build

```bash
go build ./...
go test ./...
```

## Local development

See [`docs/local-dev.md`](docs/local-dev.md) for a kind-based cluster with gVisor and an
end-to-end demo. For deeper background, see the [architecture](docs/architecture.md) and
[threat model](docs/threat-model.md).
