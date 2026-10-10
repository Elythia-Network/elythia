"""mk-go ↔ Mastodon: followers-only posts reach mk-go followers (#3498).

Mastodon はフォロワー限定の投稿を配るとき、`Collection-Synchronization` ヘッダーを
付けて署名する。受信の worker がこのヘッダーを持たずに署名を組み立て直していたので、
フォロワー限定の投稿だけが捨てられていた。公開と未収載の投稿にはこのヘッダーが
付かないので、公開の投稿のテストでは気付けなかった。
"""

from __future__ import annotations

import uuid

from conftest import MASTODON_DOMAIN
from conftest_base import poll_until


def _follow_alice(mkgo) -> str:
    """Follow alice@mastodon from carol and wait until Mastodon accepts."""
    alice = poll_until(
        lambda: mkgo.users_show("alice", MASTODON_DOMAIN),
        timeout=90, interval=3, desc="mk-go resolves alice@mastodon",
    )
    if not alice.get("isFollowing"):
        if not alice.get("hasPendingFollowRequestFromYou"):
            mkgo.follow(alice["id"])

        def following():
            u = mkgo.users_show("alice", MASTODON_DOMAIN)
            return u if u.get("isFollowing") else None

        # リモートへのフォローは、Mastodon の Accept が届くまで申請中 (#3491)。
        poll_until(following, timeout=90, interval=3, desc="Mastodon accepts carol's follow")
    return alice["id"]


def test_followers_only_post_reaches_followers(mkgo, mastodon):
    alice_id = _follow_alice(mkgo)
    marker = uuid.uuid4().hex
    status = mastodon.post("/api/v1/statuses", status=f"followers only {marker}", visibility="private")

    def delivered():
        for note in mkgo._api("notes/timeline", {"limit": 30}):
            if note.get("uri") == status["uri"]:
                return note
        return None

    note = poll_until(delivered, timeout=90, interval=3, desc="the followers-only post reaches carol's home timeline")
    assert note["visibility"] == "followers", note
    assert note["userId"] == alice_id
    assert marker in (note.get("text") or "")
