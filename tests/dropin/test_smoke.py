"""Smoke tests for Phase 13-1 (#365): verify TS-A ↔ TS-B federation baseline.

These tests are the foundation that Phase 13-2 (mk swap) and Phase 13-3
(feature matrix) build on. If these fail, nothing else in the drop-in suite
can be trusted, so keep the scope deliberately small: health, federation
follow, and one-hop note delivery.
"""

from __future__ import annotations

from conftest import A_DOMAIN, B_DOMAIN  # type: ignore[import-not-found]
from conftest_base import MisskeyLikeClient, poll_until  # type: ignore[import-not-found]


class TestHealth:
    """Both instances return a 200 from /api/ping."""

    def test_instance_a_healthy(self, instance_a: MisskeyLikeClient, alice: dict) -> None:
        assert instance_a.ping() is True

    def test_instance_b_healthy(self, instance_b: MisskeyLikeClient, bob: dict) -> None:
        assert instance_b.ping() is True


class TestWebFinger:
    """Each instance responds to its own webfinger and to cross-host lookups."""

    def test_a_webfinger_local(self, instance_a: MisskeyLikeClient, alice: dict) -> None:
        result = instance_a.webfinger(f"alice@{A_DOMAIN}")
        assert result["subject"] == f"acct:alice@{A_DOMAIN}"

    def test_b_webfinger_local(self, instance_b: MisskeyLikeClient, bob: dict) -> None:
        result = instance_b.webfinger(f"bob@{B_DOMAIN}")
        assert result["subject"] == f"acct:bob@{B_DOMAIN}"


class TestFederationFollow:
    """
    Verify that instance A can follow a remote user on instance B via
    ActivityPub. This is the minimal handshake that proves federation is
    reachable under the self-signed TLS setup.
    """

    def test_alice_follows_bob(
        self,
        instance_a: MisskeyLikeClient,
        instance_b: MisskeyLikeClient,
        alice: dict,
        bob: dict,
    ) -> None:
        # Instance A resolves bob@b via AP.
        remote_bob = poll_until(
            lambda: instance_a.users_show("bob", host=B_DOMAIN),
            timeout=60,
            desc="alice sees bob via AP",
        )
        assert remote_bob["username"] == "bob"
        assert remote_bob["host"] == B_DOMAIN

        try:
            instance_a.follow(remote_bob["id"])
        except RuntimeError as e:
            # 前回 run の follow が残っていても許容 (docker compose down で
            # volume ごと消すのが通常運用だが、手元検証を繰り返すと状態が
            # 残りうる)。
            if "ALREADY_FOLLOWING" not in str(e):
                raise

        # Instance B should register the follow within its inbox delivery window.
        # `users/followers` response shape は Misskey バージョンで揺れるので、
        # 複数キーをフォールバック的に見る。
        def _followed() -> bool:
            followers = instance_b._api("users/followers", {"userId": bob["id"]})
            for f in followers:
                follower = f.get("follower") or f
                host = follower.get("host") or follower.get("followerHost")
                if host == A_DOMAIN:
                    return True
            return False

        assert poll_until(_followed, timeout=60, desc="bob sees alice as follower")

        # alice 側でもフォローが成立する (bob の Accept が届く) のを待つ。リモートへの
        # フォローは Accept まで申請中になるので (本家と同じ、#3491)、bob の followers
        # に載っただけでは alice の home timeline にまだ積まれない。
        assert poll_until(
            lambda: (instance_a.users_show("bob", host=B_DOMAIN) or {}).get("isFollowing") is True,
            timeout=60,
            desc="alice's follow of bob is accepted",
        )


class TestFederationNoteDelivery:
    """
    After the follow handshake above, a note from bob should federate into
    alice's home timeline within a reasonable delivery window.
    """

    def test_bob_note_reaches_alice(
        self,
        instance_a: MisskeyLikeClient,
        instance_b: MisskeyLikeClient,
        alice: dict,
        bob: dict,
    ) -> None:
        # Ensure alice follows bob (idempotent because of the test above).
        remote_bob = instance_a.users_show("bob", host=B_DOMAIN)
        try:
            instance_a.follow(remote_bob["id"])
        except RuntimeError as e:
            # ALREADY_FOLLOWING のみ許容。それ以外 (network error, rate limit
            # 等) はそのまま raise して原因を露呈させる (Devin #366 #2)。
            if "ALREADY_FOLLOWING" not in str(e):
                raise

        marker = "dropin-smoke-" + bob["id"][-6:]
        bob_note = instance_b.create_note(marker)

        def _arrived() -> bool:
            tl = instance_a._api("notes/timeline", {"limit": 40})
            return any(n.get("text") == marker for n in tl)

        assert poll_until(_arrived, timeout=90, desc="alice sees bob's note")
        assert bob_note["createdNote"]["text"] == marker
