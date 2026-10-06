"""mk-go ↔ Mastodon: consent-respecting quote posts (FEP-044f, #3234).

Mastodon 4.5 以降は、引用に引用される側の承認を求める。mk-go の投稿を Mastodon
から引用でき、承認済みの引用として扱われることを、本物の Mastodon で確かめる。
"""

from __future__ import annotations

import time
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


# ブロック中の相手からの QuoteRequest に Reject を返すこと (段階 2) は e2e にしない。
# Mastodon は、どちらの向きでもブロックを知った時点で引用そのものを作らせない
# (`StatusPolicy#quote?` = `show?` && `!blocking_author?`)。mk-go のブロックは Block として
# Mastodon へ届くので、このシナリオは「Block が届く前に引用を作れたとき」しか成り立たず、
# CI で Block が先に届いて落ちた。Reject の経路はユニットテスト
# (`TestQuoteRequest_Rejects` など) で押さえ、ブロックの e2e は取り消し
# (`test_blocking_revokes_our_approval`) で見る。


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

    # mk-go は返ってきた承認を quoteAuthorization として配る。**Mastodon の検索で
    # 引用する投稿を取らせるのは、承認が返った後にする** — 取得と QuoteRequest の
    # 処理が同時に走ると、Mastodon は引用先が結び付く前の記録で照合して黙って捨てる
    # (Mastodon 側の競合。docs/divergence/federation.md §3-6)。
    def authorized():
        note = _mkgo_ap_note(mkgo, quoting["id"])
        auth = note.get("quoteAuthorization")
        return note if auth and auth.startswith(MASTODON_ORIGIN) else None

    note = poll_until(authorized, timeout=90, interval=3, desc="mk-go publishes the approval")
    assert note["quote"] == status["uri"]

    # 引用される側 (Mastodon) では、QuoteRequest に答えた時点で承認済み。
    def accepted_on_mastodon():
        s = mastodon.resolve_status(quoting_uri)
        q = (s or {}).get("quote")
        return q if q and q.get("state") == "accepted" else None

    quote = poll_until(accepted_on_mastodon, timeout=90, interval=3, desc="Mastodon accepts the quote")
    assert quote["quoted_status"]["uri"] == status["uri"]


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


def test_mastodon_revocation_withdraws_our_approval(mkgo, mastodon):
    """Stage 4: the quoted author revokes on Mastodon; mk-go drops quoteAuthorization."""
    marker = uuid.uuid4().hex
    status = mastodon.post("/api/v1/statuses", status=f"quote then revoke {marker}")
    target = poll_until(
        lambda: mkgo.resolve_ap(status["uri"]).get("object"),
        timeout=90, interval=3, desc="mk-go resolves the Mastodon post",
    )
    quoting = mkgo.quote(target["id"], f"quoting to be revoked {marker}")["createdNote"]
    quoted_at = time.monotonic()
    quoting_uri = _note_url(mkgo, quoting["id"])

    # 承認が返るまで Mastodon に取得させない (上のテストと同じ理由)。
    poll_until(
        lambda: _mkgo_ap_note(mkgo, quoting["id"]).get("quoteAuthorization"),
        timeout=90, interval=3, desc="mk-go publishes the approval",
    )

    def accepted_on_mastodon():
        s = mastodon.resolve_status(quoting_uri)
        q = (s or {}).get("quote")
        return s if q and q.get("state") == "accepted" else None

    quoting_status = poll_until(accepted_on_mastodon, timeout=90, interval=3, desc="Mastodon accepts the quote")

    mastodon.post(f"/api/v1/statuses/{status['id']}/quotes/{quoting_status['id']}/revoke")

    def withdrawn():
        note = _mkgo_ap_note(mkgo, quoting["id"])
        return note if "quoteAuthorization" not in note else None

    note = poll_until(withdrawn, timeout=90, interval=3, desc="mk-go withdraws the approval")
    assert "quote" not in note
    assert note["_misskey_quote"] == status["uri"]

    # 保留中の QuoteRequest を送り直す仕組み (#3238) は、承認・取り消しの後には
    # 送らない。送ると Mastodon は状態を見ずに承認し直す (取り消しが元に戻る)。
    # 最初の送り直しの時刻を過ぎても、Mastodon 側で取り消されたままか。送り直しは
    # 作成の 1 分後以降に毎分の定期処理が拾い、そこから配送の queue を通るので、
    # 定期処理 2 回分の余裕を見る。
    elapsed = time.monotonic() - quoted_at
    time.sleep(max(0.0, 180 - elapsed))
    # 取り消された引用は、Mastodon の API では quote ごと出ないか revoked になる
    # (mk-go の引用は承認を外した Update で legacy に戻るので、承認済みでない限り出ない)。
    quote = mastodon.status(quoting_status["id"]).get("quote")
    assert quote is None or quote.get("state") != "accepted", quote


def test_blocking_revokes_our_approval(mkgo_second, mastodon):
    """Stage 4: an mk-go author blocking the quoter revokes the approval on Mastodon."""
    marker = uuid.uuid4().hex
    note = mkgo_second.create_note(f"to be quoted then blocked {marker}")["createdNote"]
    target = _resolve(mastodon, mkgo_second, note["id"])
    quoting = mastodon.quote(target["id"], f"quoting before block {marker}")

    def state():
        q = mastodon.status(quoting["id"]).get("quote") or {}
        return q.get("state")

    poll_until(lambda: state() == "accepted", timeout=90, interval=3, desc="Mastodon marks the quote accepted")
    alice = poll_until(
        lambda: mkgo_second.users_show("alice", "mastodon"),
        timeout=90, interval=3, desc="mk-go knows alice@mastodon",
    )
    mkgo_second._api("blocking/create", {"userId": alice["id"]})
    try:
        poll_until(lambda: state() == "revoked", timeout=90, interval=3, desc="Mastodon revokes the quote")
    finally:
        mkgo_second._api("blocking/delete", {"userId": alice["id"]})
