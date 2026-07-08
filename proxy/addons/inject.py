"""
Default mitmproxy addon for sandbox git credential injection.

This file is intentionally user-editable. The orchestrator should not hold real
Git credentials; only this proxy process/container should receive them, usually
through environment variables or a mounted secret that this file reads.
"""

from __future__ import annotations

import base64
import fnmatch
import json
import os
import threading
import time
import urllib.request
from datetime import datetime, timezone
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

# --- GitHub App authentication (alternative to a static GITHUB_TOKEN) ---------
#
# A GitHub App does NOT have a static PAT. Instead the App has a private key that
# this proxy uses to sign a short-lived JWT and exchange it for an *installation
# access token* (valid ~1h). The token is cached here and auto-refreshed before
# it expires. This keeps all credential material inside the proxy.
#
# To use App auth, provide these (env vars / mounted secret) and leave
# GITHUB_TOKEN unset:
#   GITHUB_APP_ID              - the App's numeric id (Settings > Developer > Apps)
#   GITHUB_APP_PRIVATE_KEY     - the App private key PEM (inline), OR
#   GITHUB_APP_PRIVATE_KEY_FILE- path to the mounted .pem file
#   GITHUB_APP_INSTALLATION_ID - (optional) the installation id; if omitted the
#                                first installation of the App is auto-discovered.
#
# When GITHUB_APP_ID is set, the "github.com" token_key resolves to a freshly
# minted installation token instead of GITHUB_TOKEN, so the existing github.com
# and api.github.com HOST_RULES work unchanged.
GITHUB_APP_ID = os.getenv("GITHUB_APP_ID")
GITHUB_APP_INSTALLATION_ID = os.getenv("GITHUB_APP_INSTALLATION_ID")


def _load_app_private_key() -> str | None:
    inline = os.getenv("GITHUB_APP_PRIVATE_KEY")
    if inline:
        return inline
    path = os.getenv("GITHUB_APP_PRIVATE_KEY_FILE")
    if path and os.path.exists(path):
        with open(path, "r", encoding="utf-8") as fh:
            return fh.read()
    return None

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
    # GitHub REST/GraphQL API, used by the `gh` CLI. Same token as git-over-HTTPS,
    # injected as a bearer token so `gh auth status`, `gh pr create`, etc. work
    # without the sandbox ever seeing the credential.
    "api.github.com": {
        "token_key": "github.com",
        "scheme": "bearer",
    },
    # `gh` and git may also reach these GitHub hosts (archive/asset/LFS/upload
    # endpoints). Uncomment and add them to the allowlist if your workflow needs
    # authenticated access:
    # "codeload.github.com": {"token_key": "github.com", "scheme": "basic", "basic_username": "x-access-token"},
    # "uploads.github.com": {"token_key": "github.com", "scheme": "bearer"},
    # "objects.githubusercontent.com": {"token_key": "github.com", "scheme": "bearer"},
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


def _b64url(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode("ascii")


def _parse_expiry(iso8601: str) -> float:
    # GitHub returns e.g. "2026-07-08T21:39:00Z".
    try:
        dt = datetime.strptime(iso8601, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc)
        return dt.timestamp()
    except (ValueError, TypeError):
        return time.time() + 3300.0


class GitHubAppTokenProvider:
    """Mints and caches GitHub App installation access tokens.

    Signs a JWT with the App private key (RS256) and exchanges it for an
    installation token, refreshing shortly before the ~1h expiry. The exchange
    call goes directly to the GitHub API (bypassing any proxy env), and the real
    private key never leaves this process.
    """

    def __init__(self, app_id: str, private_key_pem: str, installation_id: str | None,
                 api_base: str = "https://api.github.com") -> None:
        # Imported lazily so the addon still loads when App auth is unused.
        from cryptography.hazmat.primitives import hashes, serialization
        from cryptography.hazmat.primitives.asymmetric import padding

        self._app_id = str(app_id)
        self._installation_id = installation_id
        self._api_base = api_base.rstrip("/")
        self._key = serialization.load_pem_private_key(private_key_pem.encode("utf-8"), password=None)
        self._hashes = hashes
        self._padding = padding
        self._lock = threading.Lock()
        self._cached: str | None = None
        self._expiry = 0.0

    def token(self) -> str | None:
        now = time.time()
        with self._lock:
            if self._cached and now < self._expiry - 300:
                return self._cached
            try:
                inst_id = self._installation_id or self._discover_installation()
                tok, exp = self._mint(inst_id)
            except Exception as exc:  # noqa: BLE001 - surface, never crash the proxy
                ctx.log.warn(f"github app token refresh failed: {exc}")
                return self._cached
            self._installation_id, self._cached, self._expiry = inst_id, tok, exp
            ctx.log.info("minted github app installation token")
            return tok

    def _jwt(self) -> str:
        now = int(time.time())
        header = {"alg": "RS256", "typ": "JWT"}
        payload = {"iat": now - 60, "exp": now + 540, "iss": self._app_id}
        signing_input = _b64url(json.dumps(header).encode()) + "." + _b64url(json.dumps(payload).encode())
        signature = self._key.sign(
            signing_input.encode("ascii"),
            self._padding.PKCS1v15(),
            self._hashes.SHA256(),
        )
        return signing_input + "." + _b64url(signature)

    def _api(self, method: str, path: str, body: bytes | None = None) -> Any:
        req = urllib.request.Request(
            self._api_base + path,
            method=method,
            data=body,
            headers={
                "Authorization": f"Bearer {self._jwt()}",
                "Accept": "application/vnd.github+json",
                "User-Agent": "sandbox-mitmproxy",
                "X-GitHub-Api-Version": "2022-11-28",
            },
        )
        # Never route the App API call through an upstream proxy.
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        with opener.open(req, timeout=15) as resp:
            return json.loads(resp.read().decode("utf-8"))

    def _discover_installation(self) -> str:
        data = self._api("GET", "/app/installations")
        if not data:
            raise RuntimeError("app has no installations; install it on the target repo/org")
        return str(data[0]["id"])

    def _mint(self, installation_id: str) -> tuple[str, float]:
        data = self._api("POST", f"/app/installations/{installation_id}/access_tokens", body=b"")
        return str(data["token"]), _parse_expiry(str(data.get("expires_at", "")))


def _init_app_provider() -> GitHubAppTokenProvider | None:
    if not GITHUB_APP_ID:
        return None
    pem = _load_app_private_key()
    if not pem:
        ctx.log.warn("GITHUB_APP_ID is set but no private key was provided; App auth disabled")
        return None
    try:
        provider = GitHubAppTokenProvider(GITHUB_APP_ID, pem, GITHUB_APP_INSTALLATION_ID)
        ctx.log.info(f"github app auth enabled (app_id={GITHUB_APP_ID})")
        return provider
    except Exception as exc:  # noqa: BLE001
        ctx.log.warn(f"failed to initialize github app auth: {exc}")
        return None


_app_provider = _init_app_provider()


def _resolve_token(token_key: str) -> str | None:
    # GitHub credentials come from the App provider when configured; otherwise
    # from the static TOKENS table. The api.github.com rule reuses the
    # "github.com" token_key, so both git and gh get App-minted tokens.
    if _app_provider is not None and token_key == "github.com":
        return _app_provider.token()
    return TOKENS.get(token_key)


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
    token = _resolve_token(token_key)
    if not token:
        ctx.log.warn(f"matched host rule {host_pattern!r}, but token {token_key!r} is not configured")
        return

    _inject(flow, str(host_pattern), rule, token)