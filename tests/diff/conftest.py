"""Fixtures for the mk-go <-> Misskey-TS differential e2e harness (#2089).

Two independent instances are reached via env URLs (set by tests/diff/compose.yml):
  MKGO_URL (mk-go)  /  TS_URL (misskey/misskey)。版数は tests/diff/compose.yml が持つ

Each test seeds equivalent state on both, calls the same endpoint, and asserts
the JSON responses match modulo instance noise (see diff_core).
"""

from __future__ import annotations

import os
import time

import pytest
import requests

MKGO_URL = os.environ.get("MKGO_URL", "http://mkgo:3000")
TS_URL = os.environ.get("TS_URL", "http://ts:3000")


class Client:
    """Minimal Misskey API client: POST /api/<endpoint> with an optional token."""

    def __init__(self, base_url: str, label: str) -> None:
        self.base = base_url.rstrip("/")
        self.label = label
        self.token: str | None = None
        self.session = requests.Session()

    def call(self, endpoint: str, body: dict | None = None, *, token: str | None = None) -> requests.Response:
        payload = dict(body or {})
        tok = token if token is not None else self.token
        if tok:
            payload["i"] = tok
        return self.session.post(f"{self.base}/api/{endpoint}", json=payload, timeout=30)

    def json(self, endpoint: str, body: dict | None = None, *, token: str | None = None) -> dict:
        resp = self.call(endpoint, body, token=token)
        resp.raise_for_status()
        return resp.json() if resp.content else {}

    def ensure_admin(self, username: str, password: str) -> str:
        """Create the first (admin) account and store its token.

        Uses Misskey's bootstrap path (admin/accounts/create needs no auth while
        the instance has no users). The harness is therefore one-shot per fresh
        `make diff-up`: re-running against an instance that already has users
        fails here with a clear hint to recreate the stack (`make diff-down &&
        make diff-up`). This keeps seeding simple and version-independent.
        """
        resp = self.call("admin/accounts/create", {"username": username, "password": password})
        if resp.status_code == 200:
            self.token = resp.json()["token"]
            return self.token
        raise RuntimeError(
            f"{self.label}: bootstrap admin create failed (status {resp.status_code}). "
            "The instance likely already has users — recreate the stack: "
            "`make diff-down && make diff-up`."
        )
        return self.token


def _wait_healthy(url: str) -> None:
    deadline = time.time() + 120
    last = None
    while time.time() < deadline:
        try:
            # /api/ping は JSON body 必須 (空 POST は 400)。
            r = requests.post(f"{url}/api/ping", json={}, timeout=5)
            if r.status_code == 200:
                return
            last = r.status_code
        except requests.RequestException as e:  # noqa: PERF203
            last = e
        time.sleep(2)
    raise RuntimeError(f"instance {url} not healthy (last={last})")


@pytest.fixture(scope="session", autouse=True)
def _wait_for_instances() -> None:
    _wait_healthy(MKGO_URL)
    _wait_healthy(TS_URL)


@pytest.fixture(scope="session")
def mkgo() -> Client:
    c = Client(MKGO_URL, "mkgo")
    c.ensure_admin("alice", "test-password-1234")
    return c


@pytest.fixture(scope="session")
def ts() -> Client:
    c = Client(TS_URL, "ts")
    c.ensure_admin("alice", "test-password-1234")
    return c
