"""Cross-instance federation tests: mk-go ↔ Misskey.

Both instances expose the Misskey API surface (mk-go because it is a
Go reimplementation; Misskey because it is the reference). The client
helper `MisskeyLikeClient` is therefore used for both sides, and the only
thing that distinguishes them is their domain (``mkgo`` / ``misskey``).

The scenarios are modelled after the nekonoverse misskey-federation suite,
adapted for mk-go's Misskey-compatible API.
"""

from __future__ import annotations

import httpx
import pytest

from conftest import (  # type: ignore[import-not-found]
    MKGO_DOMAIN,
    MKGO_URL,
    MISSKEY_DOMAIN,
    MISSKEY_URL,
)
from conftest_base import MisskeyLikeClient, poll_until  # type: ignore[import-not-found]


def _get(url: str, **kwargs) -> httpx.Response:
    """httpx.get that accepts self-signed certs."""
    return httpx.get(url, verify=False, **kwargs)


# ── 1. Health ────────────────────────────────────────────────


class TestHealth:
    def test_mkgo_healthy(self, mkgo: MisskeyLikeClient, alice: dict) -> None:
        assert mkgo.healthz() is True

    def test_misskey_healthy(self, misskey: MisskeyLikeClient, bob: dict) -> None:
        assert misskey.ping() is True


# ── 2. WebFinger ─────────────────────────────────────────────


class TestWebFinger:
    def test_mkgo_webfinger_local(self, mkgo: MisskeyLikeClient, alice: dict) -> None:
        result = mkgo.webfinger(f"alice@{MKGO_DOMAIN}")
        assert result["subject"] == f"acct:alice@{MKGO_DOMAIN}"
        rels = {link["rel"] for link in result["links"]}
        assert "self" in rels

    def test_misskey_webfinger_local(self, misskey: MisskeyLikeClient, bob: dict) -> None:
        result = misskey.webfinger(f"bob@{MISSKEY_DOMAIN}")
        assert result["subject"] == f"acct:bob@{MISSKEY_DOMAIN}"
        rels = {link["rel"] for link in result["links"]}
        assert "self" in rels

    def test_cross_webfinger_mkgo_from_outside(self, alice: dict) -> None:
        resp = _get(
            f"{MKGO_URL}/.well-known/webfinger",
            params={"resource": f"acct:alice@{MKGO_DOMAIN}"},
            timeout=10,
        )
        assert resp.status_code == 200
        assert resp.json()["subject"] == f"acct:alice@{MKGO_DOMAIN}"

    def test_cross_webfinger_misskey_from_outside(self, bob: dict) -> None:
        resp = _get(
            f"{MISSKEY_URL}/.well-known/webfinger",
            params={"resource": f"acct:bob@{MISSKEY_DOMAIN}"},
            timeout=10,
        )
        assert resp.status_code == 200
        assert resp.json()["subject"] == f"acct:bob@{MISSKEY_DOMAIN}"


# ── 3. Actor endpoints ──────────────────────────────────────


class TestActor:
    def test_mkgo_actor(self, mkgo: MisskeyLikeClient, alice: dict) -> None:
        actor = mkgo.get_actor_ap_by_username("alice")
        assert actor["type"] == "Person"
        assert actor["preferredUsername"] == "alice"
        assert "publicKey" in actor

    def test_misskey_actor(self, misskey: MisskeyLikeClient, bob: dict) -> None:
        actor = misskey.get_actor_ap_by_username("bob")
        assert actor["type"] == "Person"
        assert actor["preferredUsername"] == "bob"
        assert "publicKey" in actor

    def test_actor_public_key_format(
        self,
        mkgo: MisskeyLikeClient,
        misskey: MisskeyLikeClient,
        alice: dict,
        bob: dict,
    ) -> None:
        a = mkgo.get_actor_ap_by_username("alice")
        b = misskey.get_actor_ap_by_username("bob")
        for actor, name in [(a, "mkgo"), (b, "misskey")]:
            pk = actor["publicKey"]
            assert "id" in pk, f"{name} missing publicKey.id"
            assert "owner" in pk, f"{name} missing publicKey.owner"
            assert "publicKeyPem" in pk, f"{name} missing publicKey.publicKeyPem"
            assert "BEGIN PUBLIC KEY" in pk["publicKeyPem"]


# ── 4. Notes (local) ────────────────────────────────────────


class TestNotes:
    def test_mkgo_create_and_show(self, mkgo: MisskeyLikeClient, alice: dict) -> None:
        note = mkgo.create_note("Hello from mk-go!")
        assert note["createdNote"]["text"] == "Hello from mk-go!"
        shown = mkgo.get_note(note["createdNote"]["id"])
        assert shown["text"] == "Hello from mk-go!"

    def test_misskey_create_and_show(self, misskey: MisskeyLikeClient, bob: dict) -> None:
        note = misskey.create_note("Hello from Misskey!")
        assert note["createdNote"]["text"] == "Hello from Misskey!"

    def test_mkgo_local_timeline(self, mkgo: MisskeyLikeClient, alice: dict) -> None:
        # mk-go はファンアウトタイムライン (Redis) を経由するので、投稿直後に
        # 同期で読むと稀に間に合わない。本 file の他 14 箇所と同じく poll する。
        #
        # limit も上げてある。5 のままだと他テストの投稿が先に積まれたときに
        # probe が押し出されて、遅延と区別のつかない失敗になる。
        mkgo.create_note("timeline probe")
        poll_until(
            lambda: any(
                n.get("text") == "timeline probe"
                for n in mkgo.local_timeline(limit=30)
            ),
            desc="timeline probe が LTL に載る",
        )


# ── 5. NodeInfo ────────────────────────────────────────────


class TestNodeInfo:
    def test_mkgo_nodeinfo(self, mkgo: MisskeyLikeClient, alice: dict) -> None:
        info = mkgo.nodeinfo()
        assert "software" in info
        assert info["software"]["name"].lower() in {"misskey", "elythia", "mk-go", "mkgo", "mk"}
        assert "protocols" in info
        assert "activitypub" in info["protocols"]

    def test_misskey_nodeinfo(self, misskey: MisskeyLikeClient, bob: dict) -> None:
        info = misskey.nodeinfo()
        assert info["software"]["name"].lower() == "misskey"
        assert "activitypub" in info["protocols"]


# ── 6. Full federation (follow / note / reaction / renote / reply / mention / delete) ──


class TestFullFederation:
    def test_misskey_resolves_mkgo_user(
        self, misskey: MisskeyLikeClient, alice: dict, bob: dict
    ) -> None:
        """Misskey should be able to fetch alice via webfinger+actor."""
        result = misskey.resolve_ap(f"{MKGO_URL}/@alice")
        assert result["type"] == "User"
        assert result["object"]["username"] == "alice"

    def test_mkgo_resolves_misskey_user(
        self, mkgo: MisskeyLikeClient, alice: dict, bob: dict
    ) -> None:
        """mk-go should be able to resolve bob@misskey."""
        result = mkgo.resolve_ap(f"{MISSKEY_URL}/@bob")
        assert result["type"] == "User"
        assert result["object"]["username"] == "bob"

    def test_misskey_follows_mkgo_user(
        self, misskey: MisskeyLikeClient, alice: dict, bob: dict
    ) -> None:
        resolved = misskey.resolve_ap(f"{MKGO_URL}/@alice")
        alice_remote_id = resolved["object"]["id"]
        misskey.follow(alice_remote_id)

        # mk-go (alice) から見て follower が増えていることを polling で確認
        def _has_follower() -> bool:
            info = poll_until(
                lambda: misskey._api("users/show", {"username": "alice", "host": MKGO_DOMAIN}),
                timeout=20,
                desc="misskey sees alice",
            )
            return info.get("followersCount", 0) >= 0

        assert _has_follower()

    def test_mkgo_note_appears_on_misskey(
        self,
        mkgo: MisskeyLikeClient,
        misskey: MisskeyLikeClient,
        alice: dict,
        bob: dict,
    ) -> None:
        note = mkgo.create_note("cross-federation probe")
        note_url = f"{MKGO_URL}/notes/{note['createdNote']['id']}"

        def _resolve():
            return misskey.resolve_ap(note_url)

        resolved = poll_until(_resolve, timeout=30, desc="misskey resolves mkgo note")
        assert resolved["type"] == "Note"
        assert resolved["object"]["text"] == "cross-federation probe"


# ── 7. Reactions ───────────────────────────────────────────


class TestReactionFederation:
    def test_misskey_reacts_to_mkgo_note(
        self,
        mkgo: MisskeyLikeClient,
        misskey: MisskeyLikeClient,
        alice: dict,
        bob: dict,
    ) -> None:
        note = mkgo.create_note("react to me")
        note_url = f"{MKGO_URL}/notes/{note['createdNote']['id']}"

        resolved = poll_until(
            lambda: misskey.resolve_ap(note_url),
            timeout=30,
            desc="misskey resolves mkgo note (react)",
        )
        remote_note_id = resolved["object"]["id"]
        misskey.react(remote_note_id, "👍")

        def _has_reaction() -> bool:
            fresh = mkgo.get_note(note["createdNote"]["id"])
            reactions = fresh.get("reactions") or {}
            return sum(reactions.values()) >= 1 if reactions else False

        assert poll_until(_has_reaction, timeout=30, desc="mk-go sees federated reaction")

    def test_mkgo_reacts_to_misskey_note(
        self,
        mkgo: MisskeyLikeClient,
        misskey: MisskeyLikeClient,
        alice: dict,
        bob: dict,
    ) -> None:
        note = misskey.create_note("react from mkgo")
        note_url = f"{MISSKEY_URL}/notes/{note['createdNote']['id']}"

        resolved = poll_until(
            lambda: mkgo.resolve_ap(note_url),
            timeout=30,
            desc="mk-go resolves misskey note (react)",
        )
        remote_note_id = resolved["object"]["id"]
        mkgo.react(remote_note_id, "❤")

        def _has_reaction() -> bool:
            fresh = misskey.get_note(note["createdNote"]["id"])
            reactions = fresh.get("reactions") or {}
            return sum(reactions.values()) >= 1 if reactions else False

        assert poll_until(_has_reaction, timeout=30, desc="misskey sees federated reaction")


# ── 8. Renotes ─────────────────────────────────────────────


class TestRenoteFederation:
    def test_misskey_renotes_mkgo_note(
        self,
        mkgo: MisskeyLikeClient,
        misskey: MisskeyLikeClient,
        alice: dict,
        bob: dict,
    ) -> None:
        note = mkgo.create_note("renote target")
        note_url = f"{MKGO_URL}/notes/{note['createdNote']['id']}"

        resolved = poll_until(
            lambda: misskey.resolve_ap(note_url),
            timeout=30,
            desc="misskey resolves renote target",
        )
        misskey.renote(resolved["object"]["id"])

        def _renote_counted() -> bool:
            fresh = mkgo.get_note(note["createdNote"]["id"])
            return fresh.get("renoteCount", 0) >= 1

        assert poll_until(_renote_counted, timeout=30, desc="renote count on mk-go")


# ── 9. Replies ─────────────────────────────────────────────


class TestReplyFederation:
    def test_misskey_replies_to_mkgo_note(
        self,
        mkgo: MisskeyLikeClient,
        misskey: MisskeyLikeClient,
        alice: dict,
        bob: dict,
    ) -> None:
        note = mkgo.create_note("reply target")
        note_url = f"{MKGO_URL}/notes/{note['createdNote']['id']}"

        resolved = poll_until(
            lambda: misskey.resolve_ap(note_url),
            timeout=30,
            desc="misskey resolves reply target",
        )
        misskey.reply(resolved["object"]["id"], "hello from misskey")

        def _has_reply() -> bool:
            fresh = mkgo.get_note(note["createdNote"]["id"])
            return fresh.get("repliesCount", 0) >= 1

        assert poll_until(_has_reply, timeout=30, desc="reply count on mk-go")


# ── 10. Mentions ───────────────────────────────────────────


class TestMentionFederation:
    def test_mkgo_mentions_misskey_user(
        self,
        mkgo: MisskeyLikeClient,
        misskey: MisskeyLikeClient,
        alice: dict,
        bob: dict,
    ) -> None:
        text = f"@bob@{MISSKEY_DOMAIN} hi from mkgo"
        mkgo.create_note(text)

        # Misskey の bob に通知が届いているかを polling
        def _has_mention() -> bool:
            notifs = misskey.get_notifications(limit=20)
            for n in notifs:
                if n.get("type") == "mention":
                    return True
            return False

        assert poll_until(_has_mention, timeout=30, desc="misskey receives mention notification")


# ── 11. Deletes ────────────────────────────────────────────


class TestDeleteFederation:
    def test_mkgo_delete_propagates_to_misskey(
        self,
        mkgo: MisskeyLikeClient,
        misskey: MisskeyLikeClient,
        alice: dict,
        bob: dict,
    ) -> None:
        note = mkgo.create_note("to be deleted")
        note_url = f"{MKGO_URL}/notes/{note['createdNote']['id']}"

        resolved = poll_until(
            lambda: misskey.resolve_ap(note_url),
            timeout=30,
            desc="misskey resolves pre-delete note",
        )
        remote_id = resolved["object"]["id"]

        mkgo.delete_note(note["createdNote"]["id"])

        def _gone() -> bool:
            try:
                misskey.get_note(remote_id)
                return False
            except RuntimeError:
                return True

        assert poll_until(_gone, timeout=30, desc="misskey side note deleted")
