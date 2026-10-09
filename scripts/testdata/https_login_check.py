#!/usr/bin/env python3
"""Real-browser HTTPS login check (ADR 0108), run against a real kind cluster with a real
Traefik Ingress and core's own self-signed certificate -- not a port-forward, not a test-only
client. Proves the actual thing the earlier NodePort spike (booth-core PR #5, commit 3187046)
found broken: PKCE S256 needs a secure context, which https now provides, through a real
Ingress, using the real shipped "booth-design" realm client (never a test-only one -- the
coordinator's explicit condition on this check).

Drives the stand-in shell (scripts/testdata/https-login-shell/, deployed by the CI job onto a
second Ingress path on the same host/certificate -- booth-core ships no frontend of its own)
through a real login as a real user (created beforehand via scripts/booth-admin, over the
ordinary port-forward path -- not this script's job), and asserts:
  1. the real Keycloak login page's own asset requests (/resources/*) all return 200 -- the
     condition added in the "part two" review, proving CSS/JS actually load through the
     Ingress, not just that the HTML document itself does;
  2. the login completes and the shell's callback successfully exchanges the code for a token
     using the SHIPPED client;
  3. core's own /api/me, called with that access token (not the id_token), reports the
     expected membership -- the real audience-mapper defect PR A's review caught would show up
     here as a 401, not as a client-side crash.
"""
import json
import os
import socket
import ssl
import sys
import urllib.error
import urllib.request

from playwright.sync_api import sync_playwright

HOST = os.environ.get("INGRESS_HOST", "booth-ci.test")

# Chromium's --host-resolver-rules (below) only applies inside the browser's own process --
# the plain HTTP checks in this script need the same "resolve HOST to the local node" rule
# for themselves, the equivalent of the operator's own /etc/hosts entry.
_real_getaddrinfo = socket.getaddrinfo


def _patched_getaddrinfo(host, *args, **kwargs):
    if host == HOST:
        host = "127.0.0.1"
    return _real_getaddrinfo(host, *args, **kwargs)


socket.getaddrinfo = _patched_getaddrinfo
SHELL_URL = f"https://{HOST}/ci-shell/"
USERNAME = os.environ["TEST_USERNAME"]
PASSWORD = os.environ["TEST_PASSWORD"]
WORKSPACE = os.environ["TEST_WORKSPACE"]
ROLE = os.environ.get("TEST_ROLE", "owner")

# Self-signed, same reasoning as the browser's ignore_https_errors below: these plain HTTP
# checks prove routing, not certificate trust.
_SSL_CTX = ssl.create_default_context()
_SSL_CTX.check_hostname = False
_SSL_CTX.verify_mode = ssl.CERT_NONE

_KEYCLOAK_MARKERS = (b"keycloak", b"kc-page-title", b"kc-form-login", b"kc-feedback-text")


def fail(msg: str) -> None:
    print(f"RESULT: FAILED - {msg}")
    sys.exit(1)


def _request(method: str, path: str, data: bytes | None = None, headers: dict | None = None) -> tuple[int, bytes]:
    req = urllib.request.Request(f"https://{HOST}{path}", data=data, method=method, headers=headers or {})
    try:
        with urllib.request.urlopen(req, context=_SSL_CTX, timeout=10) as resp:
            return resp.status, resp.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()


def _looks_like_keycloak(body: bytes) -> bool:
    lower = body.lower()
    return any(marker in lower for marker in _KEYCLOAK_MARKERS)


def check_admin_and_master_realm_not_exposed() -> None:
    """ADR 0108's review: the admin console and master realm must be unreachable through the
    Ingress at all -- not routed to Keycloak, whatever core's own default backend happens to
    answer with (a 404, the shell, anything) for these paths."""
    for path in ("/admin/", "/admin/master/console/", "/realms/master/"):
        status, body = _request("GET", path)
        if _looks_like_keycloak(body):
            fail(f"{path} looks like it was served by Keycloak (status {status}) -- it must never reach Keycloak through the Ingress")
        print(f"{path}: not Keycloak (status {status})")

    # Whatever grant Keycloak would or wouldn't honor here is irrelevant -- this path isn't
    # routed to Keycloak at all, so no real token response can come back regardless.
    status, body = _request(
        "POST", "/realms/master/protocol/openid-connect/token",
        data=b"grant_type=client_credentials&client_id=admin-cli",
        headers={"Content-Type": "application/x-www-form-urlencoded"},
    )
    looks_like_token_response = False
    try:
        parsed = json.loads(body)
        looks_like_token_response = isinstance(parsed, dict) and ("access_token" in parsed or "token_type" in parsed)
    except ValueError:
        pass
    if looks_like_token_response or _looks_like_keycloak(body):
        fail(f"/realms/master/protocol/openid-connect/token returned what looks like a real Keycloak response (status {status}): {body[:200]!r}")
    print(f"/realms/master/protocol/openid-connect/token: not a Keycloak token response (status {status})")


def check_discovery_issuer_matches_ingress_host() -> None:
    status, body = _request("GET", "/realms/booth/.well-known/openid-configuration")
    if status != 200:
        fail(f"/realms/booth/.well-known/openid-configuration returned {status}, not 200")
    doc = json.loads(body)
    want = f"https://{HOST}/realms/booth"
    if doc.get("issuer") != want:
        fail(f"discovery issuer = {doc.get('issuer')!r}, want exactly {want!r}")
    print(f"discovery issuer OK: {doc['issuer']}")


def main() -> None:
    check_admin_and_master_realm_not_exposed()
    check_discovery_issuer_matches_ingress_host()

    with sync_playwright() as p:
        # --host-resolver-rules is the browser-scoped equivalent of the operator's own
        # /etc/hosts entry (the design note's own "resolves ingress.host the same way a real
        # operator would") -- avoids needing root to edit the CI runner's system-wide hosts
        # file for one test's hostname.
        #
        # ignore_https_errors, not a trusted CA: this check proves the secure-context/Ingress
        # mechanics work over https, not that this specific self-signed CA chains correctly --
        # a separate, well-understood browser behavior, not what ADR 0108's spike was about.
        browser = p.chromium.launch(args=[f"--host-resolver-rules=MAP {HOST} 127.0.0.1"])
        page = browser.new_page(ignore_https_errors=True)
        console_lines: list[str] = []
        page.on("console", lambda msg: console_lines.append(msg.text))
        asset_requests: list[tuple[str, int]] = []
        page.on("response", lambda resp: asset_requests.append((resp.url, resp.status)) if "/resources/" in resp.url else None)

        print(f"navigating to {SHELL_URL} ...")
        page.goto(SHELL_URL)
        page.click("#login")
        page.wait_for_url("**/realms/booth/protocol/openid-connect/auth*", timeout=15000)
        print(f"landed on the real Keycloak login page: {page.url}")

        page.wait_for_selector("#username", timeout=15000)
        page.wait_for_timeout(500)  # let the theme's own async asset loads finish

        print(f"login-page asset requests observed: {len(asset_requests)}")
        for url, status in asset_requests:
            print(f"  {status} {url}")
        if not asset_requests:
            fail("no /resources/* asset requests observed at all -- the login page's own CSS/JS never loaded")
        failed_assets = [(u, s) for u, s in asset_requests if s >= 400]
        if failed_assets:
            fail(f"some login-page assets failed to load: {failed_assets}")

        page.fill("#username", USERNAME)
        page.fill("#password", PASSWORD)
        page.click("#kc-login")
        page.wait_for_url("**/ci-shell/callback.html*", timeout=15000)
        page.wait_for_timeout(1000)

        status_text = page.inner_text("#status")
        print("final status text:", status_text)
        print("console log:", console_lines)

        if not status_text.startswith("LOGIN_OK:"):
            fail(f"login did not complete: {status_text}")

        me = json.loads(status_text[len("LOGIN_OK: /api/me = "):])
        want = {"workspace": WORKSPACE, "role": ROLE}
        if {"workspace": WORKSPACE, "role": ROLE} not in [
            {"workspace": m["workspace"], "role": m["role"]} for m in me.get("memberships", [])
        ]:
            fail(f"core's /api/me memberships = {me.get('memberships')!r}, want {want!r} present")

        browser.close()
        print("RESULT: PASSED - real shipped-client HTTPS login through a real Ingress; core accepted the access token")


if __name__ == "__main__":
    main()
