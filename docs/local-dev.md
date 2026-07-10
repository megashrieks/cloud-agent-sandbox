# Local development

This guide brings up the full stack on a local **kind** cluster and walks an end-to-end demo:
create a sandbox → clone a repo through the credential-injecting proxy → edit → push.

> Prerequisites: Docker, `kind`, `kubectl`, and Go 1.25+. For real isolation and the closed-network
> guarantee you also need a NetworkPolicy-enforcing CNI and the gVisor runtime (see below).

## 1. Create the cluster

```bash
kind create cluster --config deploy/kind/kind-config.yaml
```

kind's default CNI (kindnet) does **not** enforce NetworkPolicy. Install Calico so the sandbox
egress lockdown is real:

```bash
kubectl apply -f https://raw.githubusercontent.com/projectcalico/calico/v3.28.0/manifests/calico.yaml
kubectl -n kube-system rollout status daemonset/calico-node --timeout=180s
```

### gVisor (isolation runtime)

Running gVisor inside kind requires the `runsc` shim in the node image. For pure local iteration
you can skip it and fall back to the hardened baseline by setting `SANDBOX_RUNTIME_CLASS=""` on the
orchestrator Deployment (edit `deploy/k8s/50-orchestrator.yaml`). **Do not run genuinely hostile
code without gVisor/Kata.** For real isolation, use a cluster whose nodes have the runsc containerd
runtime registered as handler `runsc`, then keep `SANDBOX_RUNTIME_CLASS=gvisor`.

## 2. Build & load images

```bash
# For local kind development, build the bare image names the manifests reference
# (imagePullPolicy is IfNotPresent, so kind uses the loaded local images).
# To push to a remote registry instead, set a prefix, e.g. REGISTRY=ghcr.io/<your-user>/
REGISTRY=

# Orchestrator
docker build -t ${REGISTRY}sandbox-orchestrator:latest .
# MITM proxy
docker build -t ${REGISTRY}sandbox-mitmproxy:latest ./proxy
# Default sandbox image
docker build -t ${REGISTRY}sandbox-default:latest ./images/default

kind load docker-image ${REGISTRY}sandbox-orchestrator:latest --name sandbox-dev
kind load docker-image ${REGISTRY}sandbox-mitmproxy:latest --name sandbox-dev
kind load docker-image ${REGISTRY}sandbox-default:latest --name sandbox-dev
```

If you use a `REGISTRY` prefix, update the image references in `deploy/k8s/40-mitmproxy.yaml`,
`deploy/k8s/50-orchestrator.yaml` (including the `SANDBOX_DEFAULT_IMAGE` env), or override the
default sandbox image at runtime with `SANDBOX_DEFAULT_IMAGE`.

## 3. Configure tokens (proxy-only)

Real credentials live ONLY in the proxy. Create the token secret the mitmproxy Deployment reads:

```bash
kubectl create namespace sandboxes
kubectl -n sandboxes create secret generic mitmproxy-tokens \
  --from-literal=github-token=ghp_your_token_here
```

Customize injection/allowlist by editing `proxy/addons/inject.py` and rebuilding the proxy image.

## 4. Deploy

The orchestrator refuses to start without an API key. Create it (any strong random string) alongside
the new proxy egress policy:

```bash
kubectl apply -f deploy/k8s/00-namespace.yaml
kubectl -n sandboxes create secret generic orchestrator-auth \
  --from-literal=api-key=$(openssl rand -hex 32)

kubectl apply -f deploy/k8s/10-runtimeclasses.yaml     # skip if no gVisor/Kata
kubectl apply -f deploy/k8s/20-networkpolicy-sandbox.yaml
kubectl apply -f deploy/k8s/21-networkpolicy-mitmproxy.yaml
kubectl apply -f deploy/k8s/30-orchestrator-rbac.yaml
kubectl apply -f deploy/k8s/50-orchestrator.yaml       # provisions the shared CA on startup
kubectl -n sandboxes rollout status deploy/orchestrator
kubectl apply -f deploy/k8s/40-mitmproxy.yaml          # mounts the shared CA secret
kubectl -n sandboxes rollout status deploy/mitmproxy
```

> Ordering: the orchestrator's `EnsureCASecret` creates the shared `mitmproxy-ca` Secret on
> startup, which the mitmproxy pods mount. Deploy the orchestrator first; mitmproxy pods will start
> once the secret exists (Kubernetes retries the volume mount automatically).

## 5. Port-forward and smoke-test

Every REST and MCP request needs the API key. Load it into a variable first:

```bash
kubectl -n sandboxes port-forward svc/orchestrator 8080:8080 &
KEY=$(kubectl -n sandboxes get secret orchestrator-auth -o jsonpath='{.data.api-key}' | base64 -d)
AUTH="Authorization: Bearer $KEY"

curl -s http://localhost:8080/healthz            # -> ok (open, no auth)
curl -s -H "$AUTH" http://localhost:8080/sessions        # -> [] (401 without the key)
```

## 6. End-to-end demo via MCP

Point any MCP client at the streamable-HTTP endpoint `http://localhost:8080/mcp` and set two
headers on every request: `Authorization: Bearer $KEY` and `X-Session-Id: <your-session-id>` (any
stable string; the sandbox is created on first use and reused afterwards). Then:

1. `shell` `{command: "git clone https://github.com/<org>/<repo> /workspace/repo"}`
   The sandbox is auto-provisioned and the clone succeeds with credentials it never sees (injected
   by the proxy). Call `create_sandbox` first only if you want a specific `image`.
2. `str_replace_based_edit_tool` (`view` / `create` / `str_replace` / `insert`) to make changes.
3. `shell` `{command: "cd /workspace/repo && git checkout -b change && git commit -am wip && git push -u origin change"}`.
4. `clear_session` when done to destroy the sandbox and workspace.

The sandbox trusts the injected CA, all egress is forced through the proxy, and the proxy injects
the real token for github.com only. Everything else is blocked by the NetworkPolicy.

## Teardown

```bash
kind delete cluster --name sandbox-dev
```

## Troubleshooting

- **mitmproxy pod stuck ContainerCreating**: the `mitmproxy-ca` secret doesn't exist yet. Ensure
  the orchestrator started successfully (it provisions it).
- **git clone fails with TLS errors**: the CA wasn't injected; confirm `SANDBOX_*` proxy env and
  `/tmp/ca-bundle.crt` exist in the sandbox (`shell` `{command:"env | grep -i ca"}`).
- **egress not blocked**: your CNI isn't enforcing NetworkPolicy; install Calico/Cilium.
