"""Fixtures for mk-go ↔ Misskey federation tests."""

from __future__ import annotations

import os
import sys

import pytest

# `common/` is mounted alongside this directory inside the test-runner
# container (see tests/federation/compose.misskey.yml). Add it to sys.path
# so we can import the shared helpers.
_COMMON_DIR = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "common")
if _COMMON_DIR not in sys.path:
    sys.path.insert(0, _COMMON_DIR)

from conftest_base import MisskeyLikeClient, wait_for_health  # noqa: E402

MKGO_URL = os.environ.get("MKGO_URL", "https://mkgo")
MISSKEY_URL = os.environ.get("MISSKEY_URL", "https://misskey")
MKGO_DOMAIN = os.environ.get("MKGO_DOMAIN", "mkgo")
MISSKEY_DOMAIN = os.environ.get("MISSKEY_DOMAIN", "misskey")


@pytest.fixture(scope="session", autouse=True)
def wait_for_instances() -> None:
    """Block until both instances can accept API requests."""
    wait_for_health(MKGO_URL, "/healthz")
    wait_for_health(MISSKEY_URL, "/api/ping", method="POST")


@pytest.fixture(scope="session")
def mkgo() -> MisskeyLikeClient:
    return MisskeyLikeClient(MKGO_URL, MKGO_DOMAIN)


@pytest.fixture(scope="session")
def misskey() -> MisskeyLikeClient:
    return MisskeyLikeClient(MISSKEY_URL, MISSKEY_DOMAIN)


@pytest.fixture(scope="session")
def alice(mkgo: MisskeyLikeClient) -> dict:
    """First user on mk-go (= root)."""
    return mkgo.create_admin("alice", "password1234")


@pytest.fixture(scope="session")
def bob(misskey: MisskeyLikeClient) -> dict:
    """First user on Misskey (= root)."""
    return misskey.create_admin("bob", "password1234")


@pytest.fixture(scope="session", autouse=True)
def enable_federation(
    mkgo: MisskeyLikeClient,
    misskey: MisskeyLikeClient,
    alice: dict,
    bob: dict,
) -> None:
    """Turn federation on for both instances.

    `meta.federation` の既定値は **両実装とも `none`** で、その状態では
    webfinger / host-meta / nodeinfo の discovery が 403 になる。upstream は
    2025-08 の `TweakDefaultFederationSettings` migration で既定を `all` から
    `none` に変えており、mk-go もそれに追随している。

    つまり素の instance は連合しないのが正しい挙動なので、連合を検証する側が
    明示的に有効化する。root token が要るので alice / bob の作成後に走らせる。
    """
    for client in (mkgo, misskey):
        client._api("admin/update-meta", {"federation": "all"})
