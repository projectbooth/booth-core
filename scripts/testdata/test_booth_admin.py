#!/usr/bin/env python3
"""Unit tests for scripts/booth-admin's set-password and harden-master subcommands
(ADR 0106 condition 7 / ADR 0108 condition 6). Pure stdlib (unittest + unittest.mock),
matching the script's own "no pip install needed" discipline -- no real Keycloak, every
Admin REST API call is faked via urllib.request.urlopen.

Run directly: python3 scripts/testdata/test_booth_admin.py
"""
import importlib.machinery
import importlib.util
import io
import json
import sys
import unittest
import unittest.mock
import urllib.request
from pathlib import Path

# scripts/booth-admin has no .py extension (it's an executable CLI, not a library), so
# importlib needs an explicit loader to recognize it as Python source.
_SCRIPT_PATH = Path(__file__).resolve().parent.parent / "booth-admin"
_loader = importlib.machinery.SourceFileLoader("booth_admin", str(_SCRIPT_PATH))
_spec = importlib.util.spec_from_file_location("booth_admin", _SCRIPT_PATH, loader=_loader)
booth_admin = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(booth_admin)


class FakeResponse:
    """Enough of urllib's response object for booth-admin's own usage: a context manager
    whose .read() returns pre-baked bytes."""

    def __init__(self, body):
        self._body = json.dumps(body).encode("utf-8") if body is not None else b""

    def read(self):
        return self._body

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False


MASTER_REALM = {
    "realm": "master",
    "enabled": True,
    "sslRequired": "external",
    "bruteForceProtected": False,
    "someOtherField": "must survive a round trip untouched",
}


class FakeKeycloak:
    """Routes urlopen(req) calls to canned responses by (method, path), and records every
    PUT/POST body it was actually asked to send -- so a test can assert not just "it printed
    the right thing" but "it sent exactly this body to exactly this path"."""

    def __init__(self, users=None, master_realm=None):
        self.users = users or {}  # username -> user dict (must include "id")
        self.master_realm = dict(master_realm or MASTER_REALM)
        self.sent = []  # list of (method, path, body)

    def __call__(self, req, *a, **kw):
        method = req.get_method()
        path = req.full_url.split("://", 1)[1].split("/", 1)[1]
        path = "/" + path

        if method == "POST" and path == "/realms/master/protocol/openid-connect/token":
            # application/x-www-form-urlencoded, not JSON -- booth-admin never inspects this
            # module's response beyond access_token, so there's nothing to assert on the body.
            return FakeResponse({"access_token": "fake-admin-token"})

        body = json.loads(req.data.decode("utf-8")) if req.data else None

        if method == "GET" and path.startswith("/admin/realms/booth/users?"):
            username = path.split("username=", 1)[1].split("&", 1)[0]
            user = self.users.get(username)
            return FakeResponse([user] if user else [])

        if method == "PUT" and path.endswith("/reset-password"):
            self.sent.append((method, path, body))
            return FakeResponse(None)

        if method == "GET" and path == "/admin/realms/master":
            return FakeResponse(self.master_realm)

        if method == "PUT" and path == "/admin/realms/master":
            self.sent.append((method, path, body))
            return FakeResponse(None)

        raise AssertionError(f"unexpected request: {method} {path}")


def run_booth_admin(argv, fake, env=None):
    """Runs booth-admin's real main() against a faked transport, capturing stdout."""
    env = dict(env or {})
    env.setdefault("KEYCLOAK_URL", "https://keycloak.example.test")
    env.setdefault("KEYCLOAK_ADMIN_USER", "admin")
    env.setdefault("KEYCLOAK_ADMIN_PASSWORD", "admin-password")
    out = io.StringIO()
    with unittest.mock.patch.object(urllib.request, "urlopen", side_effect=fake), \
         unittest.mock.patch.dict("os.environ", env, clear=False), \
         unittest.mock.patch("sys.stdout", out):
        exit_code = booth_admin.main(argv)
    return exit_code, out.getvalue()


class TestSetPassword(unittest.TestCase):
    def test_resets_an_existing_user_and_prints_the_password_once(self):
        fake = FakeKeycloak(users={"alice": {"id": "u-alice", "username": "alice"}})
        code, out = run_booth_admin(["set-password", "alice"], fake)
        self.assertEqual(code, 0)
        self.assertIn("'alice': password reset", out)
        self.assertIn("Temporary password (shown once", out)

        self.assertEqual(len(fake.sent), 1)
        method, path, body = fake.sent[0]
        self.assertEqual((method, path), ("PUT", "/admin/realms/booth/users/u-alice/reset-password"))
        self.assertEqual(body["type"], "password")
        self.assertTrue(body["temporary"], "must require a change at next login, not set a permanent password")
        self.assertTrue(len(body["value"]) >= 20, "temporary password looks too short/weak")

    def test_unknown_user_is_an_error_and_sends_no_request(self):
        fake = FakeKeycloak(users={})
        code, out = run_booth_admin(["set-password", "nobody"], fake)
        self.assertEqual(code, 1)
        self.assertEqual(fake.sent, [])

    def test_dry_run_does_not_reveal_a_password_that_was_never_actually_set(self):
        fake = FakeKeycloak(users={"alice": {"id": "u-alice", "username": "alice"}})
        code, out = run_booth_admin(["set-password", "alice", "--dry-run"], fake)
        self.assertEqual(code, 0)
        self.assertIn("would be reset", out)
        self.assertNotIn("Temporary password", out)
        # _request intercepts PUT in dry-run before any request is sent.
        self.assertEqual(fake.sent, [])


class TestHardenMaster(unittest.TestCase):
    def test_enables_brute_force_protection_and_preserves_every_other_field(self):
        fake = FakeKeycloak(master_realm=dict(MASTER_REALM, bruteForceProtected=False))
        code, out = run_booth_admin(["harden-master"], fake)
        self.assertEqual(code, 0)
        self.assertIn("brute-force protection enabled", out)

        self.assertEqual(len(fake.sent), 1)
        method, path, body = fake.sent[0]
        self.assertEqual((method, path), ("PUT", "/admin/realms/master"))
        self.assertTrue(body["bruteForceProtected"])
        # Never a partial body -- a PUT with only bruteForceProtected would be a full
        # replace on the server, wiping every other master-realm setting.
        self.assertEqual(body["someOtherField"], "must survive a round trip untouched")
        self.assertEqual(body["sslRequired"], "external")

    def test_idempotent_when_already_enabled(self):
        fake = FakeKeycloak(master_realm=dict(MASTER_REALM, bruteForceProtected=True))
        code, out = run_booth_admin(["harden-master"], fake)
        self.assertEqual(code, 0)
        self.assertIn("already enabled, no change", out)
        self.assertEqual(fake.sent, [], "an already-protected master realm must not be PUT again")

    def test_dry_run_makes_no_change(self):
        fake = FakeKeycloak(master_realm=dict(MASTER_REALM, bruteForceProtected=False))
        code, out = run_booth_admin(["harden-master", "--dry-run"], fake)
        self.assertEqual(code, 0)
        self.assertIn("would be enabled", out)
        self.assertEqual(fake.sent, [])


if __name__ == "__main__":
    sys.exit(unittest.main())
