"""
Default mitmproxy addon for sandbox git credential injection.

This file is intentionally user-editable. The orchestrator should not hold real
Git credentials; only this proxy process/container should receive them, usually
through environment variables or a mounted secret that this file reads.
"""

from __future__ import annotations

import base64
import fnmatch
import os
from typing import Any

from mitmproxy import ctx, http

# =============================================================================
# USER CONFIG: edit this section to customize hosts, token sources, and injection
# =============================================================================

# Closed-by-default egress posture. Requests to hosts not listed in HOST_RULES are
# denied with HTTP 403. Set to False only if your sandbox network policy allows it.
ENFORCE_EGRESS_ALLOWLIST = True

# Token lookup table. Values are loaded from environment variables by default so
# secrets can be supplied with `docker run -e ...`, Compose/Kubernetes secrets, or
# another secret-mount mechanism. You may also replace a value with code that reads
# a mounted file, e.g. Path("/run/secrets/github_token").read_text().strip().
#
# To add a self-hosted GitLab, add a token key here and reference it from
# HOST_RULES below:
#   "gitlab.internal.example.com": os.getenv("GITLAB_INTERNAL_TOKEN"),
TOKENS: dict[str, str | None] = {
    "github.com": os.getenv("GITHUB_TOKEN"),
    "gitlab.com": os.getenv("GITLAB_TOKEN"),
}

# Per-host injection rules. Keys are exact hostnames or fnmatch wildcards such as
# "*.gitlab.internal.example.com". Values describe how to transform the token into
# request credentials.
#
# Supported schemes:
#   basic  -> Authorization: Basic base64("<username>:<token>")
#             GitHub git-over-HTTPS commonly uses username "x-access-token".
#   bearer -> Authorization: Bearer <token>
#   header -> <header_name>: <token> (for example GitLab PRIVATE-TOKEN)
#
# Only edit TOKENS and HOST_RULES for normal customization.
HOST_RULES: dict[str, dict[str, Any]] = {
    "github.com": {
        "token_key": "github.com",
        "scheme": "basic",
        "basic_username": "x-access-token",
    },
    # GitLab accepts OAuth/PAT tokens as Bearer for many APIs and git-over-HTTPS.
    # If your GitLab deployment requires PRIVATE-TOKEN instead, change this rule to:
    #   "scheme": "header", "header_name": "PRIVATE-TOKEN"
    "gitlab.com": {
        "token_key": "gitlab.com",
        "scheme": "bearer",
    },
    # Example self-hosted GitLab. Uncomment and add a TOKENS entry above.
    # "gitlab.internal.example.com": {
    #     "token_key": "gitlab.internal.example.com",
    #     "scheme": "header",
    #     "header_name": "PRIVATE-TOKEN",
    # },
}

# Client-supplied credential headers are removed before injecting proxy-owned
# credentials so untrusted sandbox code cannot smuggle its own Authorization value.
STRIP_CLIENT_CREDENTIAL_HEADERS = (
    "Authorization",
    "Proxy-Authorization",
    "PRIVATE-TOKEN",
)

# =============================================================================
# Addon implementation: normal users should not need to edit below this line.
# =============================================================================


def _basic_auth(username: str, token: str) -> str:
    raw = f"{username}:{token}".encode("utf-8")
    return "Basic " + base64.b64encode(raw).decode("ascii")


def _find_rule(host: str) -> tuple[str, dict[str, Any]] | tuple[None, None]:
    normalized = host.lower().rstrip(".")
    for pattern, rule in HOST_RULES.items():
        pattern_normalized = pattern.lower().rstrip(".")
        if normalized == pattern_normalized or fnmatch.fnmatch(normalized, pattern_normalized):
            return pattern, rule
    return None, None


def _strip_client_credentials(flow: http.HTTPFlow) -> None:
    for header_name in STRIP_CLIENT_CREDENTIAL_HEADERS:
        if header_name in flow.request.headers:
            del flow.request.headers[header_name]


def _deny(flow: http.HTTPFlow, host: str) -> None:
    flow.response = http.Response.make(
        403,
        b"egress denied by sandbox MITM proxy allowlist\n",
        {"content-type": "text/plain; charset=utf-8"},
    )
    ctx.log.warn(f"egress denied: host={host!r} is not present in HOST_RULES")


def _inject(flow: http.HTTPFlow, host_pattern: str, rule: dict[str, Any], token: str) -> None:
    scheme = str(rule.get("scheme", "")).lower()

    if scheme == "basic":
        username = str(rule.get("basic_username", "x-access-token"))
        flow.request.headers["Authorization"] = _basic_auth(username, token)
    elif scheme == "bearer":
        flow.request.headers["Authorization"] = f"Bearer {token}"
    elif scheme == "header":
        header_name = str(rule.get("header_name", "Authorization"))
        flow.request.headers[header_name] = token
    else:
        ctx.log.warn(f"host rule {host_pattern!r} has unsupported scheme {scheme!r}; no token injected")
        return

    ctx.log.info(f"matched host rule {host_pattern!r}; injected {scheme!r} credentials")


def request(flow: http.HTTPFlow) -> None:
    """mitmproxy request hook for HTTP and intercepted HTTPS requests."""
    host = flow.request.pretty_host
    host_pattern, rule = _find_rule(host)

    if rule is None:
        if ENFORCE_EGRESS_ALLOWLIST:
            _deny(flow, host)
        # To relax networking, set ENFORCE_EGRESS_ALLOWLIST = False. Unmatched
        # requests will then pass through without proxy-injected credentials.
        return

    _strip_client_credentials(flow)

    token_key = str(rule.get("token_key", host_pattern))
    token = TOKENS.get(token_key)
    if not token:
        ctx.log.warn(f"matched host rule {host_pattern!r}, but token {token_key!r} is not configured")
        return

    _inject(flow, str(host_pattern), rule, token)