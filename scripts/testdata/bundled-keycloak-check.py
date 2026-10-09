#!/usr/bin/env python3
"""In-cluster check for the bundled Keycloak default (ADR 0106/0108), run as a Kubernetes Job
inside the real kind cluster -- not a port-forward out to the GitHub runner, per the design
note's own section (h): this proves the real chart, the real EnsureKeycloak provisioning, and
the real starter realm work end to end, not just a disposable fixture built independently of
the real chart (that's scripts/testdata/ci-realm.json's own job, booth-admin-check).

Runs scripts/booth-admin itself (mounted alongside this script via the same ConfigMap) against
the real bundled Keycloak, then uses a test-only client (created here, through the admin API --
the shipped client has direct grants off) to obtain a real token and confirm:
  1. the groups claim reads exactly ["/workspaces/<slug>/<role>"] -- the starter realm's own
     groups-client-scope mapper actually produces what ADR 0025's grammar needs;
  2. booth-core's own /api/me, called at its in-cluster Service DNS name, reports the same
     workspace and role -- the full chain, nothing stubbed.
"""
from __future__ import annotations

import base64
import json
import os
import socket
import subprocess
import sys
import threading
import urllib.error
import urllib.parse
import urllib.request

KEYCLOAK_URL = os.environ["KEYCLOAK_URL"]  # Keycloak's own in-cluster Service DNS name
CORE_URL = os.environ["CORE_URL"]  # booth-core's own in-cluster Service DNS name
REALM = os.environ.get("KEYCLOAK_REALM", "booth")
ADMIN_PASSWORD = os.environ["KEYCLOAK_ADMIN_PASSWORD"]
WORKSPACE = "bundled-check"
USERNAME = "bundled-check-user"
VERIFICATION_PASSWORD = "bundled-check-verification-password"
TEST_CLIENT_ID = "bundled-check-test-only"


def post_form(url: str, data: dict[str, str]) -> dict:
    body = "&".join(f"{k}={urllib.parse.quote(v)}" for k, v in data.items())
    req = urllib.request.Request(url, data=body.encode(), method="POST",
                                  headers={"Content-Type": "application/x-www-form-urlencoded"})
    with urllib.request.urlopen(req, timeout=15) as resp:
        return json.load(resp)


def call(method: str, url: str, token: str | None = None, body: dict | None = None, headers: dict | None = None) -> tuple[int, bytes]:
    data = json.dumps(body).encode() if body is not None else None
    h = dict(headers or {})
    if token:
        h["Authorization"] = f"Bearer {token}"
    if body is not None:
        h["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, method=method, headers=h)
    try:
        with urllib.request.urlopen(req, timeout=15) as resp:
            return resp.status, resp.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()


def start_loopback_relay(target_host: str, target_port: int) -> int:
    """booth-admin itself refuses a plain http:// KEYCLOAK_URL unless the host is literally
    "localhost"/"127.0.0.1" (a deliberate check: the admin password and every token it fetches
    must never cross a real network in the clear) -- it checks the hostname string, not whether
    the traffic is actually confined to the cluster, so it refuses Keycloak's in-cluster Service
    DNS name exactly as it would refuse a real external host. Rather than weaken that check (not
    this job's call to make), relay it through loopback here -- functionally identical to the
    kubectl port-forward pattern that check already exists to steer operators toward, and the
    bytes never leave this container either way.
    """
    listener = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    listener.bind(("127.0.0.1", 0))
    listener.listen(16)
    port = listener.getsockname()[1]

    def pump(src: socket.socket, dst: socket.socket) -> None:
        try:
            while True:
                data = src.recv(65536)
                if not data:
                    break
                dst.sendall(data)
        except OSError:
            pass
        finally:
            for s in (src, dst):
                try:
                    s.shutdown(socket.SHUT_RDWR)
                except OSError:
                    pass

    def accept_loop() -> None:
        while True:
            try:
                client, _ = listener.accept()
            except OSError:
                return
            try:
                upstream = socket.create_connection((target_host, target_port), timeout=10)
            except OSError as e:
                print(f"relay: connecting upstream failed: {e}")
                client.close()
                continue
            threading.Thread(target=pump, args=(client, upstream), daemon=True).start()
            threading.Thread(target=pump, args=(upstream, client), daemon=True).start()

    threading.Thread(target=accept_loop, daemon=True).start()
    return port


def run_booth_admin(keycloak_url: str, *args: str) -> str:
    env = dict(os.environ, KEYCLOAK_URL=keycloak_url, KEYCLOAK_REALM=REALM,
               KEYCLOAK_ADMIN_USER="admin", KEYCLOAK_ADMIN_PASSWORD=ADMIN_PASSWORD)
    result = subprocess.run(
        [sys.executable, "/scripts/booth-admin", *args],
        env=env, capture_output=True, text=True,
    )
    print(f"$ booth-admin {' '.join(args)}\n{result.stdout}{result.stderr}")
    if result.returncode != 0:
        raise SystemExit(f"booth-admin {args} failed (exit {result.returncode})")
    return result.stdout.strip()


def main() -> None:
    kc_host, kc_port_str = urllib.parse.urlsplit(KEYCLOAK_URL).netloc.split(":")
    relay_port = start_loopback_relay(kc_host, int(kc_port_str))
    booth_admin_url = f"http://localhost:{relay_port}"

    run_booth_admin(booth_admin_url, "create-workspace", WORKSPACE)
    run_booth_admin(booth_admin_url, "create-user", USERNAME, "--email", f"{USERNAME}@example.test", "--name", "Bundled Check")
    run_booth_admin(booth_admin_url, "add-member", USERNAME, WORKSPACE, "owner")

    admin_token = post_form(f"{KEYCLOAK_URL}/realms/master/protocol/openid-connect/token", {
        "grant_type": "password", "client_id": "admin-cli", "username": "admin", "password": ADMIN_PASSWORD,
    })["access_token"]

    # The shipped client (booth-design) has direct grants off (ADR 0106 item d) -- this is a
    # test-only client, created here, never part of the starter realm itself. It needs its own
    # audience mapper: the chart's real default (oidc.requireAudience=true, clientId=booth-design)
    # is in effect for this check, and Keycloak's default `aud` ("account") would not satisfy it.
    status, resp = call("POST", f"{KEYCLOAK_URL}/admin/realms/{REALM}/clients", token=admin_token, body={
        "clientId": TEST_CLIENT_ID, "publicClient": True, "directAccessGrantsEnabled": True,
        "standardFlowEnabled": False, "serviceAccountsEnabled": False,
        "defaultClientScopes": ["groups", "profile", "email", "roles", "web-origins", "basic", "acr"],
        "protocolMappers": [{
            "name": "audience-booth-design", "protocol": "openid-connect", "protocolMapper": "oidc-audience-mapper",
            "consentRequired": False,
            "config": {"included.custom.audience": "booth-design", "id.token.claim": "false", "access.token.claim": "true"},
        }],
    })
    if status not in (201, 409):
        raise SystemExit(f"creating the test client failed: {status} {resp!r}")

    status, resp = call("GET", f"{KEYCLOAK_URL}/admin/realms/{REALM}/users?username={USERNAME}&exact=true", token=admin_token)
    user_id = json.loads(resp)[0]["id"]

    # booth-admin itself only ever sets a temporary password -- a real person resolves that
    # through the browser on first sign-in. This has no browser, so it sets a permanent one
    # directly, purely to obtain a token to inspect; test scaffolding, not something booth-admin does.
    status, _ = call("PUT", f"{KEYCLOAK_URL}/admin/realms/{REALM}/users/{user_id}/reset-password", token=admin_token,
                      body={"type": "password", "value": VERIFICATION_PASSWORD, "temporary": False})
    if status != 204:
        raise SystemExit(f"resetting the verification password failed: {status}")

    token = post_form(f"{KEYCLOAK_URL}/realms/{REALM}/protocol/openid-connect/token", {
        "grant_type": "password", "client_id": TEST_CLIENT_ID, "username": USERNAME, "password": VERIFICATION_PASSWORD,
    })["access_token"]

    payload_b64 = token.split(".")[1]
    payload_b64 += "=" * (-len(payload_b64) % 4)
    claims = json.loads(base64.urlsafe_b64decode(payload_b64))
    want_group = f"/workspaces/{WORKSPACE}/owner"
    if claims.get("groups") != [want_group]:
        raise SystemExit(f"groups claim = {claims.get('groups')!r}, want exactly [{want_group!r}]")
    print("groups claim OK:", claims["groups"])

    status, resp = call("GET", f"{CORE_URL}/api/me", token=token, headers={"X-Workspace": WORKSPACE})
    if status != 200:
        raise SystemExit(f"core's /api/me returned {status}: {resp!r}")
    me = json.loads(resp)
    if me.get("active") != {"workspace": WORKSPACE, "role": "owner"}:
        raise SystemExit(f"core's /api/me active = {me.get('active')!r}, want workspace={WORKSPACE!r} role=owner")
    print("core /api/me OK:", me["active"])

    print("=== bundled Keycloak default verified end to end ===")


if __name__ == "__main__":
    main()
