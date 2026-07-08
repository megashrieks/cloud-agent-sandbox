# Architecture

The sandbox orchestrator runs untrusted, LLM-authored code in isolated, disposable Kubernetes
sandboxes and lets an LLM drive them over MCP, without ever exposing real git credentials.

## Components

| Package | Role |
|---|---|
| `internal/config` | Typed configuration (env-overridable). Scaling limits, lifetimes, runtime, proxy, image. |
| `internal/session` | Session model + state machine (creating→running→stopped→dead) and the `Store` (in-memory). |
| `internal/kube` | Shared Kubernetes client (clientset + REST config). |
| `internal/runtime` | `Runtime` over Kubernetes: hardened pod spec, PVC, RuntimeClass, create/stop/resume/purge. |
| `internal/exec` | `Executor` over the `pods/exec` streaming subresource: sync/async shell + file read/write. |
| `internal/proxy` | Shared-CA generation, CA Secret, and the `ProxyAssigner` (endpoint + CA for injection). |
| `internal/pool` | Warm pool of pre-created ready sandboxes for fast startup. |
| `internal/manager` | Integration core. Ties store+runtime+pool+proxy into the full session lifecycle. |
| `internal/reaper` | Enforces lifetimes: stop running >1h, purge stopped >24h. |
| `internal/api` | REST API for session lifecycle. |
| `internal/mcp` | Streamable-HTTP MCP server exposing LLM tools. |
| `cmd/orchestrator` | Wires everything and serves `/healthz`, `/sessions`, `/mcp`. |
| `proxy/` | mitmproxy image + user-editable addon (holds tokens, injects credentials, egress allowlist). |
| `images/default/` | Curated default sandbox image (git/node/python/build tools) + CA-trust entrypoint. |
| `deploy/` | kind config, RuntimeClasses, NetworkPolicies, RBAC, deployments. |

## Request flow (create + use a sandbox)

```
LLM ──(MCP: create_session)──► Manager.Create
        │                         ├─ enforce MaxRunning
        │                         ├─ ProxyAssigner.Assign  → endpoint + shared CA
        │                         ├─ Pool.Acquire (warm) OR Runtime.Create (cold)
        │                         │     └─ hardened Pod: gVisor, non-root, ro-rootfs,
        │                         │        drop caps, HTTPS_PROXY + CA injected
        │                         ├─ WaitReady
        │                         └─ persist session (running)
LLM ──(MCP: shell/read/write/…)─► validate session (Require) → Executor via pods/exec
Sandbox ──egress──► mitmproxy (NetworkPolicy allows only this) → injects token → github/gitlab
Reaper ──► Stop running >1h ; Purge stopped >24h
```

## Session lifecycle

- **create_session** mints a session id (the only tool that does). Sandbox = Pod (+ workspace PVC).
- **Stopped** = Pod deleted, PVC + metadata retained → resumable within 24h (`Manager.Resume`).
- **Dead** = purged (Pod + PVC deleted).
- Every non-create tool calls `Manager.Require`; unknown/expired/not-running sessions return
  `invalid`.

## Credential model

The orchestrator holds **no tokens**. Real credentials live only in the user-configured mitmproxy
addon. The orchestrator injects only a proxy endpoint and the shared proxy CA cert. See
`docs/threat-model.md` and `proxy/README.md`.

## Configuration (selected env vars)

| Env | Default | Meaning |
|---|---|---|
| `SANDBOX_LISTEN_ADDR` | `:8080` | HTTP/MCP listen address |
| `SANDBOX_NAMESPACE` | `sandboxes` | Namespace for sandbox pods |
| `SANDBOX_RUNTIME_CLASS` | `gvisor` | Isolation runtime (`""` = hardened baseline only) |
| `SANDBOX_DEFAULT_IMAGE` | curated default | Image when a session omits one |
| `SANDBOX_MIN_IDLE_READY` | `2` | Warm pool target |
| `SANDBOX_MAX_RUNNING` | `50` | Hard cap on running sandboxes |
| `SANDBOX_MAX_STOPPED` | `100` | Hard cap on retained stopped sandboxes |
| `SANDBOX_RUNNING_TTL` | `1h` | Idle running sandbox reap age |
| `SANDBOX_STOPPED_TTL` | `24h` | Stopped sandbox purge age |
