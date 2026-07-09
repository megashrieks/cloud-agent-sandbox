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
# Orchestrator
docker build -t ghcr.io/megashrieks/sandbox-orchestrator:latest .
# MITM proxy
docker build -t ghcr.io/megashrieks/sandbox-mitmproxy:latest ./proxy
# Default sandbox image
docker build -t ghcr.io/megashrieks/sandbox-default:latest ./images/default

kind load docker-image ghcr.io/megashrieks/sandbox-orchestrator:latest --name sandbox-dev
kind load docker-image ghcr.io/megashrieks/sandbox-mitmproxy:latest --name sandbox-dev
kind load docker-image ghcr.io/megashrieks/sandbox-default:latest --name sandbox-dev
```

## 3. Configure tokens (proxy-only)

Real credentials live ONLY in the proxy. Create the token secret the mitmproxy Deployment reads:

```bash
kubectl create namespace sandboxes
kubectl -n sandboxes create secret generic mitmproxy-tokens \
  --from-literal=github-token=ghp_your_token_here
```

Customize injection/allowlist by editing `proxy/addons/inject.py` and rebuilding the proxy image.

## 4. Deploy

```bash
kubectl apply -f deploy/k8s/00-namespace.yaml
kubectl apply -f deploy/k8s/10-runtimeclasses.yaml     # skip if no gVisor/Kata
kubectl apply -f deploy/k8s/20-networkpolicy-sandbox.yaml
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

```bash
kubectl -n sandboxes port-forward svc/orchestrator 8080:8080 &

curl -s http://localhost:8080/healthz            # -> ok
curl -s -XPOST http://localhost:8080/sessions | tee /tmp/s.json   # -> {"id":"sbx-...", ...}
SID=$(jq -r .id /tmp/s.json)
curl -s http://localhost:8080/sessions/$SID       # inspect
```

## 6. End-to-end demo via MCP

Point any MCP client at the streamable-HTTP endpoint `http://localhost:8080/mcp`, then:

1. `create_session` → returns `session_id`.
2. `shell` `{session_id, command: "git clone https://github.com/<org>/<repo> /workspace/repo"}`
   The clone succeeds with credentials the sandbox never sees (injected by the proxy).
3. `read_file` / `str_replace` / `write_file` to make changes.
4. `shell` `{command: "cd /workspace/repo && git checkout -b change && git commit -am wip && git push -u origin change"}`.

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
