# Threat model & hardening

This system runs **deliberately untrusted, potentially malicious** code authored by an LLM. The
security design assumes the code inside a sandbox is hostile and will actively try to (a) escape
to the host/cluster, (b) steal credentials, and (c) exfiltrate data.

## Trust boundaries

```
 trusted                              | untrusted
-------------------------------------- | ----------------------------------
 Orchestrator (Go)                     | Sandbox pod (runs LLM/user code)
 Manager, exec plumbing                |
 mitmproxy addon (holds tokens)        |  <-- talks to proxy, never sees token
 Kubernetes control plane              |
```

The orchestrator and the proxy are trusted. The sandbox is not. **Nothing the sandbox can do is
allowed to compromise the trusted side or reveal a credential.**

## 1. Container isolation (escape prevention)

Applied to every sandbox pod (`internal/runtime/kubernetes.go`):

- **Isolation runtime via RuntimeClass**: `gvisor` (default) or `kata` (opt-in). gVisor runs a
  user-space kernel so untrusted syscalls do not hit the host kernel directly; Kata gives each
  sandbox its own VM kernel. Configured by `SANDBOX_RUNTIME_CLASS`.
- **Non-root**: `runAsNonRoot: true`, `runAsUser: 1000`; no root inside the container.
- **No privilege escalation**: `allowPrivilegeEscalation: false`, `privileged: false`,
  `no-new-privileges` via the restricted PodSecurity standard.
- **Drop ALL capabilities**: `capabilities.drop: ["ALL"]`.
- **Read-only root filesystem**: only `/tmp` (emptyDir) and `/workspace` (PVC) are writable.
- **Seccomp**: `RuntimeDefault` profile.
- **No service-account token**: `automountServiceAccountToken: false`, so the sandbox has no
  Kubernetes API credentials.
- **Resource limits**: CPU/memory limits bound denial-of-service.
- **No docker socket**: the sandbox has no access to any container runtime socket, so it cannot
  spawn privileged containers.
- **PodSecurity**: the `sandboxes` namespace enforces the `restricted` Pod Security Standard.

## 2. Network containment (exfiltration prevention)

`deploy/k8s/20-networkpolicy-sandbox.yaml` applies a **default-deny** NetworkPolicy to every pod
labelled `app=sandbox`:

- **Ingress: denied entirely.** Nothing can connect into a sandbox.
- **Egress: denied except** DNS (to kube-dns) and TCP to the `app=mitmproxy` pool on 8080.
- The sandbox therefore cannot reach the internet, the cluster API, cloud metadata endpoints,
  or other pods directly. Only the proxy is reachable.

> Requires a NetworkPolicy-enforcing CNI (Calico/Cilium). kind's default kindnet does **not**
> enforce policy. See `docs/local-dev.md`.

## 3. Credential isolation

- The **orchestrator holds no tokens** and has no credential logic. It only injects a proxy
  endpoint and the proxy's CA cert into sandboxes.
- **Real tokens live only in the user-configured mitmproxy addon** (`proxy/addons/inject.py`),
  loaded from env/secret. The proxy strips any sandbox-supplied `Authorization` and injects the
  real credential server-side, so the token is never present in the sandbox.
- The sandbox trusts an injected **CA cert only** (`/etc/sandbox/ca.crt` → `/tmp/ca-bundle.crt`),
  which grants it nothing.
- The proxy addon enforces a **closed-by-default egress allowlist**: requests to hosts without a
  configured rule are rejected with 403.

## 4. Shared-CA design

All proxy replicas mount one shared CA Secret (`mitmproxy-ca`), so a sandbox can talk to any
replica behind the Service and still trust it. The orchestrator provisions this CA once
(`proxy.EnsureCASecret`) and reads the cert from the same Secret to inject into sandboxes. The CA
**private key never leaves the proxy tier**.

## Residual risks / notes

- **Exfiltration via allowed hosts:** even with injected auth, an approved upstream (e.g. a git
  push to an attacker-controlled repo) is a data path. The allowlist + the user's choice of
  injected scopes are the real boundary. Keep tokens least-privilege.
- **gVisor/Kata availability:** on clusters without these runtimes, set `SANDBOX_RUNTIME_CLASS=""`
  to fall back to the hardened baseline only, which is weaker isolation; do not run truly hostile
  code there.
- **Multi-tenant auth** for the MCP/REST surface (who may create/reach which session) is a layer
  to add on top; today any caller of the orchestrator can address any session.
