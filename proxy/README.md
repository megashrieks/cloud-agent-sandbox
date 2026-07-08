# Sandbox MITM proxy

This image runs `mitmdump` as a regular HTTP proxy on port `8080` and loads `addons/inject.py` to inject git/API credentials only inside the proxy. Sandboxes should be configured to use this proxy and should never receive real tokens directly.

## Credential model

The orchestrator holds no git credentials. Tokens live only in the user-configured proxy container, typically as environment variables such as `GITHUB_TOKEN` and `GITLAB_TOKEN`, or as mounted secrets read by `inject.py`.

The default addon strips client-supplied credential headers and injects proxy-owned credentials for configured hosts:

- `github.com`: `Authorization: Basic base64("x-access-token:<token>")`
- `gitlab.com`: `Authorization: Bearer <token>` by default; comments show how to switch to `PRIVATE-TOKEN`.

Token values are never logged.

## Customizing hosts and tokens

Edit `addons/inject.py` in the `USER CONFIG` section only:

1. Add or change entries in `TOKENS`, usually reading from environment variables or mounted secret files.
2. Add or change entries in `HOST_RULES` for exact hostnames or wildcard patterns such as `*.gitlab.example.com`.
3. Choose an injection scheme per host: `basic`, `bearer`, or `header`.

For a self-hosted GitLab, add a token key to `TOKENS`, add the host to `HOST_RULES`, and choose either Bearer auth or a `PRIVATE-TOKEN` header.

## CA generation

The Dockerfile pins mitmproxy's config directory with:

```text
--set confdir=/home/mitmproxy/.mitmproxy
```

On startup, mitmproxy generates its CA certificate at:

```text
/home/mitmproxy/.mitmproxy/mitmproxy-ca-cert.pem
```

The orchestrator can start a proxy instance, extract that PEM file, and install it into sandboxes so TLS interception works for allowed git hosts.

## Pool-per-group topology

Run a separate proxy pool per sandbox group, tenant, or trust boundary. Each pool can have its own user-edited addon configuration and token set. Sandboxes in that group point `HTTPS_PROXY`/`HTTP_PROXY` at their assigned proxy instance or load-balanced pool.

## Closed-by-default networking

`ENFORCE_EGRESS_ALLOWLIST = True` by default. Any request whose host is not present in `HOST_RULES` receives `403 egress denied`. To relax this for development, set it to `False`; unmatched requests will pass through without injected credentials.