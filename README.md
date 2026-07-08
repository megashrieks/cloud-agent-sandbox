# Sandbox Orchestrator

A Kubernetes-backed orchestrator that runs **untrusted, LLM-authored code** in isolated,
disposable sandboxes. An LLM agent drives sandboxes over **MCP** (streamable HTTP): it creates
(or resumes by session id) a sandbox, clones a repo, runs shell commands, edits files, and opens
pull requests, **without ever seeing real git credentials**.

Credentials are injected by a **MITM proxy** (mitmproxy) that the sandbox is forced to trust and
route through. The orchestrator itself holds **no credentials**, it does pure orchestration.

## Key properties

- **Isolation:** hardened container baseline + **gVisor** (default) or **Kata** (opt-in) via
  Kubernetes `RuntimeClass`.
- **Credential safety:** real tokens live only in the **user-configured** mitmproxy addon; the
  sandbox trusts an injected CA but never sees the token.
- **Closed network:** default-deny egress; sandboxes may only reach their assigned proxy.
- **Lifecycle:** running sandboxes reaped after **1h**, stopped sandboxes purged after **24h**.
- **Scaling:** configurable warm pool (min idle ready), max running, max stopped.
- **LLM-optimized tools:** `create_session`, sync + async `shell` (persistent, stateful),
  `read_file` / `write_file` / `str_replace` / `insert`.

## Layout

```
cmd/orchestrator      main entrypoint
internal/config       typed configuration
internal/session      session model + store
internal/runtime      K8s sandbox lifecycle (pods/PVCs, RuntimeClass, hardened spec)
internal/exec         K8s pods/exec: shell (sync/async) + file read/write
internal/mcp          streamable-HTTP MCP server + tools
internal/api          REST API (session create/get/stop/delete/list)
internal/pool         warm pool + scaling
internal/reaper       lifetime enforcement (1h/24h)
internal/proxy        mitmproxy pool management + session assignment
proxy/                mitmproxy image + user-editable addon (CA + token injection)
images/default        curated default sandbox image
deploy/               kind config, RuntimeClasses, NetworkPolicies, manifests
docs/                 architecture, threat model, operator guide
```

## Build

```bash
go build ./...
go test ./...
```

## Local development

See `docs/local-dev.md` for a kind-based cluster with gVisor and an end-to-end demo.
