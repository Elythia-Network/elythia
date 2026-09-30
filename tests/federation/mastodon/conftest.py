"""Fixtures for mk-go ↔ Mastodon federation tests (#3234)."""

from __future__ import annotations

import os
import sys

import httpx
import pytest

_COMMON_DIR = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "common")
if _COMMON_DIR not in sys.path:
    sys.path.insert(0, _COMMON_DIR)

from conftest_base import MisskeyLikeClient, wait_for_health  # noqa: E402

MKGO_URL = os.environ.get("MKGO_URL", "https://mkgo")
MASTODON_URL = os.environ.get("MASTODON_URL", "https://mastodon")
MKGO_DOMAIN = os.environ.get("MKGO_DOMAIN", "mkgo")
MASTODON_DOMAIN = os.environ.get("MASTODON_DOMAIN", "mastodon")
MASTODON_TOKEN_FILE = os.environ.get("MASTODON_TOKEN_FILE", "/shared/mastodon_token")


class MastodonClient:
    """Minimal Mastodon REST client for the e2e."""

    def __init__(self, base_url: str, token: str):
        self.http = httpx.Client(
            base_url=base_url, timeout=30, verify=False,
            headers={"Authorization": f"Bearer {token}"},
        )

    def _check(self, resp: httpx.Response):
        if resp.status_code >= 400:
            raise RuntimeError(f"{resp.request.method} {resp.request.url} failed ({resp.status_code}): {resp.text[:500]}")
        return resp.json()

    def get(self, path: str, **params):
        return self._check(self.http.get(path, params=params))

    def post(self, path: str, **body):
        return self._check(self.http.post(path, json=body))

    def resolve_status(self, url: str) -> dict | None:
        """Fetch a remote post through search (resolve=true), or None."""
        res = self.get("/api/v2/search", q=url, resolve="true", type="statuses")
        statuses = res.get("statuses") or []
        return statuses[0] if statuses else None

    def status(self, status_id: str) -> dict:
        return self.get(f"/api/v1/statuses/{status_id}")

    def quote(self, status_id: str, text: str) -> dict:
        return self.post("/api/v1/statuses", status=text, quoted_status_id=status_id)


@pytest.fixture(scope="session", autouse=True)
def wait_for_instances() -> None:
    wait_for_health(MKGO_URL, "/healthz")
    wait_for_health(MASTODON_URL, "/health")


@pytest.fixture(scope="session")
def mkgo() -> MisskeyLikeClient:
    client = MisskeyLikeClient(MKGO_URL, MKGO_DOMAIN)
    client.create_admin("carol", "password1234")
    # meta.federation の既定は none なので、連合を検証する側が有効化する。
    client._api("admin/update-meta", {"federation": "all"})
    return client


@pytest.fixture(scope="session")
def mastodon() -> MastodonClient:
    with open(MASTODON_TOKEN_FILE) as f:
        token = f.read().strip()
    return MastodonClient(MASTODON_URL, token)


@pytest.fixture(scope="session")
def mkgo_second(mkgo) -> MisskeyLikeClient:
    """A second mk-go user (dave), for quotes between local users."""
    client = MisskeyLikeClient(MKGO_URL, MKGO_DOMAIN)
    try:
        data = mkgo._api("admin/accounts/create", {"username": "dave", "password": "password1234"})
        client.token = data.get("token")
    except RuntimeError:
        # 前回の実行で作った利用者が残っている (volume を消さずに回し直したとき)。
        client.signin("dave", "password1234")
    return client
