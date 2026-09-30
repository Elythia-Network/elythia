"""mk-go ↔ Mastodon: consent-respecting quote posts (FEP-044f, #3234).

Mastodon 4.5 以降は、引用に引用される側の承認を求める。mk-go の投稿を Mastodon
から引用でき、承認済みの引用として扱われることを、本物の Mastodon で確かめる。
"""

from __future__ import annotations

import uuid

from conftest import MASTODON_URL
from conftest_base import poll_until

MASTODON_ORIGIN = MASTODON_URL


def _note_url(mkgo, note_id: str) -> str:
    return f"{mkgo.base_url}/notes/{note_id}"


def _resolve(mastodon, mkgo, note_id: str) -> dict:
    return poll_until(
        lambda: mastodon.resolve_status(_note_url(mkgo, note_id)),
        timeout=90, interval=3, desc=f"Mastodon resolves mk-go note {note_id}",
    )


def test_public_note_is_quotable(mkgo, mastodon):
    """Stage 1: Mastodon reads our interactionPolicy as 'anyone may quote'."""
    note = mkgo.create_note(f"quotable public {uuid.uuid4()}")["createdNote"]
    status = _resolve(mastodon, mkgo, note["id"])
    approval = status["quote_approval"]
    assert approval["automatic"] == ["public"], approval
    assert approval["current_user"] == "automatic", approval


def test_home_note_is_quotable(mkgo, mastodon):
    note = mkgo.create_note(f"quotable home {uuid.uuid4()}", visibility="home")["createdNote"]
    status = _resolve(mastodon, mkgo, note["id"])
    assert status["quote_approval"]["automatic"] == ["public"], status["quote_approval"]


def test_quote_is_accepted(mkgo, mastodon):
    """Stage 2: the QuoteRequest is approved and Mastodon marks the quote accepted."""
    marker = uuid.uuid4().hex
    note = mkgo.create_note(f"to be quoted {marker}")["createdNote"]
    target = _resolve(mastodon, mkgo, note["id"])

    quoting = mastodon.quote(target["id"], f"quoting {marker}")

    def accepted():
        q = mastodon.status(quoting["id"]).get("quote")
        return q if q and q.get("state") == "accepted" else None

    quote = poll_until(accepted, timeout=90, interval=3, desc="Mastodon marks the quote accepted")
    assert quote["quoted_status"]["uri"] == _note_url(mkgo, note["id"])

    # mk-go は引用として受け取り、作者に通知する (承認の確認のために投稿を
    # 取り込まなかったので、Create が「新規」として処理される)。
    def notified():
        for n in mkgo.get_notifications(limit=30):
            if n.get("type") == "quote" and (n.get("note") or {}).get("renoteId") == note["id"]:
                return n
        return None

    poll_until(notified, timeout=90, interval=3, desc="mk-go notifies the author of the quote")


def test_quote_from_blocked_user_is_rejected(mkgo, mastodon):
    """A block on a visible note gets an explicit Reject, and Mastodon marks the quote rejected."""
    marker = uuid.uuid4().hex
    note = mkgo.create_note(f"not for blocked users {marker}")["createdNote"]
    target = _resolve(mastodon, mkgo, note["id"])

    alice = poll_until(
        lambda: mkgo.users_show("alice", "mastodon"),
        timeout=90, interval=3, desc="mk-go knows alice@mastodon",
    )
    mkgo._api("blocking/create", {"userId": alice["id"]})
    try:
        quoting = mastodon.quote(target["id"], f"quoting while blocked {marker}")

        def rejected():
            q = mastodon.status(quoting["id"]).get("quote")
            return q if q and q.get("state") == "rejected" else None

        poll_until(rejected, timeout=90, interval=3, desc="Mastodon marks the quote rejected")
    finally:
        mkgo._api("blocking/delete", {"userId": alice["id"]})


def _mkgo_ap_note(mkgo, note_id: str) -> dict:
    resp = mkgo.http.get(f"/notes/{note_id}", headers={"Accept": "application/activity+json"})
    resp.raise_for_status()
    return resp.json()


def test_quoting_a_mastodon_post_is_approved(mkgo, mastodon):
    """Stage 3: mk-go asks the quoted author for approval and republishes it."""
    marker = uuid.uuid4().hex
    status = mastodon.post("/api/v1/statuses", status=f"quote me {marker}")
    target = poll_until(
        lambda: mkgo.resolve_ap(status["uri"]).get("object"),
        timeout=90, interval=3, desc="mk-go resolves the Mastodon post",
    )
    quoting = mkgo.quote(target["id"], f"quoting mastodon {marker}")["createdNote"]
    quoting_uri = _note_url(mkgo, quoting["id"])

    # 引用される側 (Mastodon) では、QuoteRequest に答えた時点で承認済み。
    def accepted_on_mastodon():
        s = mastodon.resolve_status(quoting_uri)
        q = (s or {}).get("quote")
        return q if q and q.get("state") == "accepted" else None

    quote = poll_until(accepted_on_mastodon, timeout=90, interval=3, desc="Mastodon accepts the quote")
    assert quote["quoted_status"]["uri"] == status["uri"]

    # mk-go は返ってきた承認を quoteAuthorization として配る。
    def authorized():
        note = _mkgo_ap_note(mkgo, quoting["id"])
        auth = note.get("quoteAuthorization")
        return note if auth and auth.startswith(MASTODON_ORIGIN) else None

    note = poll_until(authorized, timeout=90, interval=3, desc="mk-go publishes the approval")
    assert note["quote"] == status["uri"]


def test_local_quote_is_approved_for_third_parties(mkgo, mkgo_second, mastodon):
    """Stage 3: a quote between two mk-go users carries mk-go's own approval."""
    marker = uuid.uuid4().hex
    original = mkgo.create_note(f"local original {marker}")["createdNote"]
    quoting = mkgo_second.quote(original["id"], f"local quote {marker}")["createdNote"]

    # 承認は投稿を作った後の配送の中で発行する (非同期) ので、付くまで待つ。
    ap = poll_until(
        lambda: (lambda n: n if n.get("quoteAuthorization") else None)(_mkgo_ap_note(mkgo, quoting["id"])),
        timeout=30, interval=1, desc="mk-go issues its own approval",
    )
    assert ap["quote"] == _note_url(mkgo, original["id"])
    assert ap["quoteAuthorization"].startswith(_note_url(mkgo, original["id"]) + "/quote-authorizations/")

    # 第三者の Mastodon は承認を取得して確かめ、承認済みの引用として扱う。
    def accepted():
        s = mastodon.resolve_status(_note_url(mkgo, quoting["id"]))
        q = (s or {}).get("quote")
        return q if q and q.get("state") == "accepted" else None

    quote = poll_until(accepted, timeout=90, interval=3, desc="Mastodon verifies mk-go's approval")
    assert quote["quoted_status"]["uri"] == _note_url(mkgo, original["id"])
