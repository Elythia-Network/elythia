"""Shared helpers for mk-go federation tests.

mk-go is a Misskey-compatible Go backend, so the HTTP client used to drive
both mk-go and real Misskey instances is essentially the same. This module
exposes:

- `wait_for_health` / `poll_until` — polling helpers
- `MisskeyLikeClient` — Misskey-compatible API client used for both mk-go and
  Misskey. Exactly the same endpoints work on both sides because mk-go exposes
  the Misskey API surface.

Target-specific conftest.py files under tests/federation/<target>/ import
from this module and provide pytest fixtures.
"""

from __future__ import annotations

import time
from typing import Any

import httpx


def wait_for_health(url: str, path: str, timeout: int = 180, method: str = "GET") -> None:
    """Poll until an instance is ready. Accepts self-signed certs."""
    deadline = time.time() + timeout
    last_exc: Exception | None = None
    last_status: int | None = None
    while time.time() < deadline:
        try:
            if method == "POST":
                resp = httpx.post(f"{url}{path}", json={}, timeout=5, verify=False)
            else:
                resp = httpx.get(f"{url}{path}", timeout=5, verify=False)
            if resp.status_code == 200:
                return
            last_status = resp.status_code
        except Exception as exc:  # noqa: BLE001
            last_exc = exc
        time.sleep(2)
    msg = f"{url}{path} did not become healthy within {timeout}s"
    if last_status is not None:
        msg += f" (last status: {last_status})"
    if last_exc is not None:
        msg += f" (last: {last_exc})"
    raise TimeoutError(msg)


def poll_until(predicate, *, timeout: int = 60, interval: float = 3.0, desc: str = ""):
    """Poll predicate until it returns truthy or the timeout elapses.

    The defaults are tuned against Misskey's built-in rate limiter for
    `/api/ap/show` (30 requests per window), so that a suite-wide run does
    not exhaust the budget with one test.
    """
    deadline = time.time() + timeout
    last_exc: Exception | None = None
    while time.time() < deadline:
        try:
            result = predicate()
            if result:
                return result
        except Exception as exc:  # noqa: BLE001
            last_exc = exc
        time.sleep(interval)
    msg = f"Timed out: {desc}" if desc else "Poll timed out"
    if last_exc is not None:
        msg += f" (last: {last_exc})"
    raise TimeoutError(msg)


class MisskeyLikeClient:
    """Misskey-compatible API client.

    Used against both mk-go (which implements the Misskey API) and real
    Misskey. The two sides are distinguishable only by their `domain`
    attribute which is interpolated into webfinger / AP URIs.
    """

    def __init__(self, base_url: str, domain: str):
        self.base_url = base_url
        self.domain = domain
        self.http = httpx.Client(base_url=base_url, timeout=20, verify=False)
        self.token: str | None = None

    # ── Health ─────────────────────────────────────────────
    def ping(self) -> bool:
        try:
            resp = self.http.post("/api/ping", json={})
            return resp.status_code == 200
        except Exception:  # noqa: BLE001
            return False

    def healthz(self) -> bool:
        try:
            resp = self.http.get("/healthz")
            return resp.status_code == 200
        except Exception:  # noqa: BLE001
            return False

    # ── Account bootstrap ──────────────────────────────────
    def create_admin(self, username: str, password: str) -> dict:
        """Create the first (root) user, or sign in if it already exists.

        Works on fresh instances (initial setup path) and is idempotent
        against volumes that survived a previous test run.
        """
        resp = self.http.post(
            "/api/admin/accounts/create",
            json={"username": username, "password": password},
        )
        if resp.status_code == 200:
            data = resp.json()
            self.token = data.get("token")
            return data
        # Already initialised — fall back to signin for the same credentials.
        # mk-go returns 403 when rootUser already exists; upstream Misskey
        # returns 400 with an error code in the body. Either way, try signin.
        if resp.status_code in (400, 403, 409):
            return self.signin(username, password)
        resp.raise_for_status()
        return resp.json()

    def signin(self, username: str, password: str) -> dict:
        """Sign in as an existing user and cache the token."""
        resp = self.http.post(
            "/api/signin-flow",
            json={"username": username, "password": password},
        )
        resp.raise_for_status()
        data = resp.json()
        self.token = data.get("i") or data.get("token")
        return data

    # ── Low-level API call ─────────────────────────────────
    def _api(self, endpoint: str, body: dict | None = None) -> Any:
        payload: dict[str, Any] = dict(body or {})
        if self.token:
            payload["i"] = self.token
        resp = self.http.post(f"/api/{endpoint}", json=payload)
        if resp.status_code >= 400:
            raise RuntimeError(
                f"POST /api/{endpoint} failed ({resp.status_code}): {resp.text[:500]}"
            )
        return resp.json() if resp.content else {}

    # ── Notes ──────────────────────────────────────────────
    def create_note(self, text: str, visibility: str = "public", **kwargs) -> dict:
        return self._api("notes/create", {"text": text, "visibility": visibility, **kwargs})

    def get_note(self, note_id: str) -> dict:
        return self._api("notes/show", {"noteId": note_id})

    def delete_note(self, note_id: str) -> None:
        self._api("notes/delete", {"noteId": note_id})

    def local_timeline(self, limit: int = 20) -> list[dict]:
        return self._api("notes/local-timeline", {"limit": limit})

    def reply(self, reply_to_id: str, text: str) -> dict:
        return self._api("notes/create", {"text": text, "replyId": reply_to_id})

    def renote(self, note_id: str) -> dict:
        return self._api("notes/create", {"renoteId": note_id})

    def quote(self, note_id: str, text: str) -> dict:
        return self._api("notes/create", {"renoteId": note_id, "text": text})

    # ── Reactions ──────────────────────────────────────────
    def react(self, note_id: str, reaction: str) -> dict:
        return self._api("notes/reactions/create", {"noteId": note_id, "reaction": reaction})

    def unreact(self, note_id: str) -> dict:
        return self._api("notes/reactions/delete", {"noteId": note_id})

    def get_reactions(self, note_id: str) -> list[dict]:
        return self._api("notes/reactions", {"noteId": note_id})

    # ── Users & follows ────────────────────────────────────
    def users_show(self, username: str, host: str | None = None) -> dict:
        body: dict[str, Any] = {"username": username}
        if host:
            body["host"] = host
        return self._api("users/show", body)

    def follow(self, user_id: str) -> dict:
        return self._api("following/create", {"userId": user_id})

    def unfollow(self, user_id: str) -> dict:
        return self._api("following/delete", {"userId": user_id})

    def get_notifications(self, limit: int = 20) -> list[dict]:
        return self._api("i/notifications", {"limit": limit})

    # ── ActivityPub helpers ────────────────────────────────
    def resolve_ap(self, uri: str) -> dict:
        """Resolve an ActivityPub URI to a local object (authenticated)."""
        return self._api("ap/show", {"uri": uri})

    def webfinger(self, acct: str) -> dict:
        resp = self.http.get(
            "/.well-known/webfinger",
            params={"resource": f"acct:{acct}"},
        )
        resp.raise_for_status()
        return resp.json()

    def nodeinfo(self) -> dict:
        resp = self.http.get("/nodeinfo/2.1")
        resp.raise_for_status()
        return resp.json()

    def get_actor_ap_by_username(self, username: str) -> dict:
        """Resolve the AP actor JSON for a local username.

        Misskey uses numeric user IDs in its AP URLs (not the username), so
        we first resolve the id via `/api/users/show`, then GET the AP
        document from `/users/<id>`. This works for both mk-go (which uses
        the same URL shape) and real Misskey.
        """
        user = self._api("users/show", {"username": username})
        user_id = user["id"]
        resp = self.http.get(
            f"/users/{user_id}",
            headers={"Accept": "application/activity+json"},
        )
        resp.raise_for_status()
        return resp.json()
