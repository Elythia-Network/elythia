"""mk-go 生まれの DB を受け取った TS が正常に動くか (#2379)。

`run-mkgo-born-test.sh` の stage 5 で実行する。この時点で instance A は
**TS-A** に切り替わっており、DB-A は **mk-go の migration が作ったもの**。

## 既存の swap test と何が違うか

    swap test : TS → mk-go → TS   DB を作ったのは TypeORM
    本 test   : mk-go → TS        DB を作ったのは mk-go の migration

後者では TS が **一度も触っていない DB** を受け取る。orchestrator が既に
「起動するか」「migration を再実行しないか」を見ているので、ここでは
**データを読めるか / 連合が続くか**を見る。

「mk-go で始めた人が Misskey に移れるか」に答えるのはこの経路だけ。移れることは
保証しない (#3191) ので、ここは測る対象 (落ちたときの手順は docs/dropin-e2e.md の
「復路は測る対象」)。
"""

from __future__ import annotations

import time

from conftest import A_DOMAIN, B_DOMAIN  # type: ignore[import-not-found]
from conftest_base import MisskeyLikeClient, poll_until  # type: ignore[import-not-found]
from test_swap_setup import BASELINE_NOTE_TEXT  # type: ignore[import-not-found]


def _tolerate(resp, label: str) -> None:
    assert resp.status_code < 500, (
        f"{label}: TS が 5xx を返した ({resp.status_code})。mk-go が作った schema / "
        f"データを TS が処理できていない。\n{resp.text[:400]}"
    )


def test_ts_accepts_mkgo_created_credentials(
    instance_a: MisskeyLikeClient, alice: dict
) -> None:
    """mk-go が作ったアカウントの認証情報を TS が受け付ける。

    パスワードハッシュ (bcrypt) と user / user_profile の shape が TS の期待と
    合っていないとここで落ちる。

    **ここで signin を叩き直さないこと。** `alice` fixture が既に
    create_admin (内部で signin へ fallback) を通しており、同じ session で
    もう一度叩くと mk-go / TS の signin rate limit に当たって 429 になる。
    fixture が確立した token が TS に対して有効であることを、認証必須の
    endpoint で確かめる方が意味がある。
    """
    assert instance_a.token, "fixture が token を確立していない"
    resp = instance_a.http.post("/api/i", json={"i": instance_a.token})
    assert resp.status_code == 200, (
        f"mk-go が作った token / アカウントを TS が受け付けない: "
        f"{resp.status_code} {resp.text[:300]}"
    )
    me = resp.json()
    assert me.get("username") == "alice", f"/api/i の結果が想定外: {me}"


def test_ts_can_read_mkgo_created_notes(
    instance_a: MisskeyLikeClient, alice: dict
) -> None:
    """mk-go が作った note を TS が pack できる。

    home timeline (`notes/timeline`) で見る。local-timeline は使わない。
    この setup では note を 4 件とも **bob が instance B から**投稿しており、
    instance A から見ると全て remote note なので、そもそも local timeline には
    1 件も入らない (`a:list:localTimeline` は作られない)。空が正しい。

    ## Redis fanout timeline の引き継ぎについて

    Misskey は timeline を DB だけでなく Redis にも持つ (FTT)。バックエンドを
    差し替えると、この Redis のデータも引き継げる必要がある。

    以前ここには「Redis の引き継ぎは未検証なので別途 issue 化する」と書いて
    あったが、実測の結果 **問題なく引き継げている**ことが分かったので取り下げた。
    根拠:

      - キーの名前空間が一致する。mk-go は `config.Redis.KeyPrefix()`、TS は
        ioredis の `keyPrefix: <host>:` で、どちらも `url` のホスト名に落ちる。
        実際に mk-go が書いたキーは `a:list:homeTimeline:<id>` で、TS が読む
        キーと同一
      - TS 起動後もキーは残る (purge も書き換えもされない)
      - `enableFanoutTimelineDbFallback` を **false** にして DB への逃げ場を
        消した状態で TS を起動しても、mk-go が Redis に積んだ 4 件が
        `notes/timeline` から返る。= TS は Redis 側を読んでいる

    最後の 1 つが決定的で、fallback を有効なままにすると Redis が読めなくても
    DB から同じ結果が返るため区別がつかない。この検証を常設テストにしていない
    のは、meta の書き換えが他のテストに影響するため。
    """
    tl = instance_a._api("notes/timeline", {"limit": 40})
    texts = [n.get("text") for n in tl]
    assert BASELINE_NOTE_TEXT in texts, (
        f"mk-go が作った baseline note が TS から見えない: {texts}"
    )


def test_ts_can_read_mkgo_created_follow(
    instance_a: MisskeyLikeClient, alice: dict
) -> None:
    """mk-go が作ったフォロー関係を TS が読める。"""
    resp = instance_a.http.post(
        "/api/users/following", json={"i": instance_a.token, "userId": alice["id"], "limit": 30}
    )
    _tolerate(resp, "users/following")
    assert resp.status_code == 200
    hosts = [(f.get("followee") or {}).get("host") for f in resp.json()]
    assert B_DOMAIN in hosts, f"mk-go が作った follow が TS から見えない: {hosts}"


def test_ts_actor_json_is_servable(instance_a: MisskeyLikeClient, alice: dict) -> None:
    """mk-go が作った user から TS が actor JSON を組み立てられる。

    mk-go は user_keypair_extra に Ed25519 鍵を持つが TS は知らない。RSA 鍵
    (user_keypair、upstream テーブル) が正しく残っていれば actor は出せる。
    """
    actor = instance_a.get_actor_ap_by_username("alice")
    assert actor.get("type") == "Person", f"actor JSON が想定外: {actor}"
    assert actor.get("publicKey", {}).get("publicKeyPem"), (
        "RSA publicKey が出ていない。mk-go が作った user_keypair を TS が読めていない"
    )
    # Ed25519 は mk-go 独自なので消えているのが正しい (機能喪失であって破壊ではない)。
    assert "assertionMethod" not in actor, "TS は assertionMethod を出さないはず"


def test_ts_federation_continues_after_takeover(
    instance_a: MisskeyLikeClient,
    instance_b: MisskeyLikeClient,
    alice: dict,
    bob: dict,
) -> None:
    """TS-A に引き継いだあとも B との連合が続く。

    mk-go が解決した remote user 行と、mk-go が発行した RSA 鍵を TS が使う。

    検証は reply で行う。setup が張る follow は **alice → bob の片方向だけ**
    なので、alice の public note には B 側の配送先が存在しない。reply なら
    reply 先の作者 = bob の inbox へ確実に配送され、通知として観測できる
    (`test_swap_roundtrip_verify.py` と同じ手法)。
    """
    tl = instance_a._api("notes/timeline", {"limit": 40})
    baseline = next((n for n in tl if n.get("text") == BASELINE_NOTE_TEXT), None)
    assert baseline is not None, "baseline note が無く reply できない"

    reply_text = f"mkgo-born-handover-reply-{int(time.time())}"
    instance_a._api("notes/create", {"text": reply_text, "replyId": baseline["id"]})

    def _arrived() -> bool:
        return any(
            (n.get("note") or {}).get("text") == reply_text
            for n in instance_b.get_notifications(limit=20)
        )

    assert poll_until(_arrived, timeout=120, desc="bob に TS-A からの reply が届く"), (
        "mk-go 生まれの DB を引き継いだ TS-A から連合配送ができていない"
    )
