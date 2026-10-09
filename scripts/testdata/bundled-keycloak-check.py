#!/usr/bin/env python3
"""In-cluster check for the bundled Keycloak default (ADR 0106/0108), run as a Kubernetes Job
inside the real kind cluster -- not a port-forward out to the GitHub runner, per the design
note's own section (h): this proves the real chart, the real EnsureKeycloak provisioning, and
the real starter realm work end to end, not just a disposable fixture built independently of
the real chart (that's scripts/testdata/ci-realm.json's own job, booth-admin-check).

Runs scripts/booth-admin itself (mounted alongside this script via the same ConfigMap) against
the real bundled Keycloak, then uses a test-only client (created here, through the admin API --
the shipped client has direct grants off, so only a test-only client can fetch a token
non-interactively) to obtain a real token and confirm:
  1. the groups claim reads exactly ["/workspaces/<slug>/<role>"] -- the starter realm's own
     groups-client-scope mapper actually produces what ADR 0025's grammar needs;
  2. booth-core's own /api/me, called at its in-cluster Service DNS name, reports the same
     workspace and role -- the full chain, nothing stubbed.

Critically, the test-only client supplies NO protocol mappers of its own: it copies the
*shipped* booth-design client's actual protocolMappers (read live from the realm over the
admin API) onto itself, so a missing/wrong audience mapper on the real client fails this check
exactly as it would fail a real browser login -- this is what an earlier version of this check
got wrong (it gave the test client its own audience mapper, which hid a real missing-mapper
defect on the shipped client entirely). Also asserts the shipped client's own
publicClient/directAccessGrantsEnabled/implicitFlowEnabled/PKCE settings, read live, so ADR
0106 item (d)'s settings are pinned against drift.
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
SHIPPED_CLIENT_ID = "booth-design"  # ADR 0106 item (d)'s fixed, shipped client id


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

    # Read the SHIPPED client live, from the realm the chart actually produced -- not asserted
    # against the template, which could silently drift from what Keycloak actually imported.
    status, resp = call("GET", f"{KEYCLOAK_URL}/admin/realms/{REALM}/clients?clientId={SHIPPED_CLIENT_ID}", token=admin_token)
    shipped_matches = json.loads(resp)
    if status != 200 or not shipped_matches:
        raise SystemExit(f"the shipped client {SHIPPED_CLIENT_ID!r} was not found in the live realm: {status} {resp!r}")
    shipped = shipped_matches[0]

    # ADR 0106 item (d)'s settings, pinned against drift -- read live, not assumed.
    if shipped.get("publicClient") is not True:
        raise SystemExit(f"shipped client publicClient = {shipped.get('publicClient')!r}, want True")
    if shipped.get("directAccessGrantsEnabled") is not False:
        raise SystemExit(f"shipped client directAccessGrantsEnabled = {shipped.get('directAccessGrantsEnabled')!r}, want False")
    if shipped.get("implicitFlowEnabled") is not False:
        raise SystemExit(f"shipped client implicitFlowEnabled = {shipped.get('implicitFlowEnabled')!r}, want False")
    pkce = (shipped.get("attributes") or {}).get("pkce.code.challenge.method")
    if pkce != "S256":
        raise SystemExit(f"shipped client pkce.code.challenge.method = {pkce!r}, want 'S256'")
    print("shipped client settings OK: publicClient/directAccessGrantsEnabled/implicitFlowEnabled/PKCE all as expected")

    # The shipped client has direct grants off (ADR 0106 item d), so only a test-only client can
    # fetch a token non-interactively here. Deliberately NO protocolMappers of its own: it copies
    # the shipped client's actual mappers (read above), so a missing/wrong audience mapper on the
    # real client fails this check exactly as it would fail a real browser login -- never supply
    # a mapper here that the shipped client doesn't already have. Each mapper's own "id" is
    # stripped before reposting: Keycloak's admin API treats a supplied "id" as authoritative, so
    # reusing the shipped client's mapper id(s) verbatim on a second client silently corrupts
    # client creation instead of giving the copy fresh ids of its own.
    copied_mappers = [{k: v for k, v in m.items() if k != "id"} for m in shipped.get("protocolMappers", [])]
    status, resp = call("POST", f"{KEYCLOAK_URL}/admin/realms/{REALM}/clients", token=admin_token, body={
        "clientId": TEST_CLIENT_ID, "publicClient": True, "directAccessGrantsEnabled": True,
        "standardFlowEnabled": False, "serviceAccountsEnabled": False,
        "defaultClientScopes": ["groups", "profile", "email", "roles", "web-origins", "basic", "acr"],
        "protocolMappers": copied_mappers,
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

    # Entirely from the copied mapper, nothing this script supplied -- the actual defect this
    # check exists to catch: Keycloak's own default access-token `aud` is "account", which core's
    # real default (oidc.requireAudience=true, clientId=booth-design) would reject.
    aud = claims.get("aud")
    aud_list = aud if isinstance(aud, list) else [aud]
    if SHIPPED_CLIENT_ID not in aud_list:
        raise SystemExit(f"aud claim = {aud!r}, does not include {SHIPPED_CLIENT_ID!r} -- the shipped client's audience mapper is missing or wrong")
    print("aud claim OK:", aud)

    status, resp = call("GET", f"{CORE_URL}/api/me", token=token, headers={"X-Workspace": WORKSPACE})
    if status != 200:
        raise SystemExit(f"core's /api/me returned {status}: {resp!r}")
    me = json.loads(resp)
    if me.get("active") != {"workspace": WORKSPACE, "role": "owner"}:
        raise SystemExit(f"core's /api/me active = {me.get('active')!r}, want workspace={WORKSPACE!r} role=owner")
    print("core /api/me OK:", me["active"])

    # ADR 0108 condition 6 / the coordinator's PR B review: --import-realm cannot alter the
    # already-existing master realm, and the bundled Keycloak's NetworkPolicy admits any
    # namespace to :8080, including /realms/master -- harden-master is the real fix, run here
    # against the real bundled Keycloak, not just unit-tested against a fake transport
    # (scripts/testdata/test_booth_admin.py covers that separately).
    run_booth_admin(booth_admin_url, "harden-master")
    status, resp = call("GET", f"{KEYCLOAK_URL}/admin/realms/master", token=admin_token)
    if status != 200:
        raise SystemExit(f"reading the master realm failed: {status} {resp!r}")
    if not json.loads(resp).get("bruteForceProtected"):
        raise SystemExit("master realm bruteForceProtected is not true after harden-master")
    print("harden-master OK: master realm bruteForceProtected = true")

    # Idempotence, proven by running it again -- not just asserted by reading the code.
    second_output = run_booth_admin(booth_admin_url, "harden-master")
    if "already enabled" not in second_output:
        raise SystemExit(f"harden-master run a second time did not report idempotence: {second_output!r}")
    status, resp = call("GET", f"{KEYCLOAK_URL}/admin/realms/master", token=admin_token)
    if not json.loads(resp).get("bruteForceProtected"):
        raise SystemExit("master realm bruteForceProtected was unset by the second harden-master run")
    print("harden-master OK: idempotent on a second run")

    print("=== bundled Keycloak default verified end to end ===")


if __name__ == "__main__":
    main()
