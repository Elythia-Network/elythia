"""Phase 13-2 (#367) drop-in swap シナリオ: setup 段階。

`run-swap-test.sh` から `pytest test_swap_setup.py` の形で実行され、TS-A の
backend が動いている状態で alice/bob/follow/note を作る。後段の `pytest
test_swap_verify.py` は backend が mk-go に差し替わった後で同じ instance A
URL に対してこの state が残存していることを確認する。

ここで作る state は DB-A / Redis-A に persist される。docker compose stop で
backend だけ落として overlay で mk-go を立て直しても DB / Redis は無事。
"""

from __future__ import annotations

from conftest import A_DOMAIN, B_DOMAIN  # type: ignore[import-not-found]
from conftest_base import MisskeyLikeClient, poll_until  # type: ignore[import-not-found]

# verify 段で使う marker。run-swap-test.sh が同じ pytest コマンドを 2 回呼ぶので
# テストファイル間で値を共有する必要があり、定数として定義する。
BASELINE_NOTE_TEXT = "dropin-baseline-pre-swap"
HOME_NOTE_TEXT = "dropin-home-visibility-pre-swap"
FOLLOWERS_NOTE_TEXT = "dropin-followers-visibility-pre-swap"
SPECIFIED_NOTE_TEXT = "dropin-specified-visibility-pre-swap"
LIST_NAME = "dropin-buddies"
# #2629: 参照先を削除して dangling な renoteId を作るための組。
DANGLING_TARGET_TEXT = "dropin-dangling-target-pre-swap"
DANGLING_QUOTE_TEXT = "dropin-dangling-quote-pre-swap"


def test_setup_alice_follows_bob(
    instance_a: MisskeyLikeClient,
    instance_b: MisskeyLikeClient,
    alice: dict,
    bob: dict,
) -> None:
    """alice@a が bob@b を AP 経由で resolve して follow する。"""
    remote_bob = poll_until(
        lambda: instance_a.users_show("bob", host=B_DOMAIN),
        timeout=60,
        desc="alice resolves bob via AP",
    )
    try:
        instance_a.follow(remote_bob["id"])
    except RuntimeError as e:
        if "ALREADY_FOLLOWING" not in str(e):
            raise

    def _followed() -> bool:
        followers = instance_b._api("users/followers", {"userId": bob["id"]})
        for f in followers:
            follower = f.get("follower") or f
            host = follower.get("host") or follower.get("followerHost")
            if host == A_DOMAIN:
                return True
        return False

    assert poll_until(_followed, timeout=60, desc="bob registers alice as follower")

    # alice 側でもフォローが成立する (bob の Accept が届く) のを待つ。リモートへの
    # フォローは Accept まで申請中になるので (本家と同じ、#3491)、bob の followers に
    # 載っただけでは alice の home timeline にまだ積まれない。
    assert poll_until(
        lambda: (instance_a.users_show("bob", host=B_DOMAIN) or {}).get("isFollowing") is True,
        timeout=60,
        desc="alice's follow of bob is accepted",
    )


def test_setup_baseline_note(
    instance_a: MisskeyLikeClient,
    instance_b: MisskeyLikeClient,
    alice: dict,
    bob: dict,
) -> None:
    """bob が baseline note を投稿し alice の home に届くことを確認する。"""
    instance_b.create_note(BASELINE_NOTE_TEXT)

    def _arrived() -> bool:
        tl = instance_a._api("notes/timeline", {"limit": 40})
        return any(n.get("text") == BASELINE_NOTE_TEXT for n in tl)

    assert poll_until(_arrived, timeout=90, desc="alice receives baseline note")


def test_setup_home_visibility_note(
    instance_a: MisskeyLikeClient,
    instance_b: MisskeyLikeClient,
    alice: dict,
    bob: dict,
) -> None:
    """bob が home visibility ノートを投稿し alice の home timeline に届く。

    home visibility は AP `to: [followers]` + `cc: []` で配信される。public と
    比べて global timeline には現れないが follower (= alice) の home には届く。
    """
    instance_b.create_note(HOME_NOTE_TEXT, visibility="home")

    def _arrived() -> bool:
        tl = instance_a._api("notes/timeline", {"limit": 40})
        return any(n.get("text") == HOME_NOTE_TEXT for n in tl)

    assert poll_until(_arrived, timeout=90, desc="alice receives home-visibility note")


def test_setup_followers_visibility_note(
    instance_a: MisskeyLikeClient,
    instance_b: MisskeyLikeClient,
    alice: dict,
    bob: dict,
) -> None:
    """bob が followers visibility ノートを投稿し alice の home timeline に届く。

    followers visibility は AP `to: [followers]`、cc 無しで完全に follower 限定。
    """
    instance_b.create_note(FOLLOWERS_NOTE_TEXT, visibility="followers")

    def _arrived() -> bool:
        tl = instance_a._api("notes/timeline", {"limit": 40})
        return any(n.get("text") == FOLLOWERS_NOTE_TEXT for n in tl)

    assert poll_until(_arrived, timeout=90, desc="alice receives followers-visibility note")


def test_setup_specified_visibility_note(
    instance_a: MisskeyLikeClient,
    instance_b: MisskeyLikeClient,
    alice: dict,
    bob: dict,
) -> None:
    """bob が alice 宛 specified (DM) ノートを投稿し alice 側に届く。

    specified visibility は AP `to: [<alice URI>]` で direct message 相当。
    home timeline には載らないので /api/notes/mentions で確認する。
    """
    remote_alice = poll_until(
        lambda: instance_b.users_show("alice", host=A_DOMAIN),
        timeout=60,
        desc="bob resolves alice via AP",
    )
    instance_b.create_note(
        SPECIFIED_NOTE_TEXT,
        visibility="specified",
        visibleUserIds=[remote_alice["id"]],
    )

    def _arrived() -> bool:
        # specified note は home timeline には現れない。/api/notes/mentions に
        # `visibility="specified"` を指定して DM だけ引く (mk-go の仕様: 既定
        # では non-specified mention のみ返るため)。
        mentions = instance_a._api(
            "notes/mentions", {"limit": 40, "visibility": "specified"}
        )
        return any(n.get("text") == SPECIFIED_NOTE_TEXT for n in mentions)

    assert poll_until(_arrived, timeout=90, desc="alice receives specified-visibility DM")


def test_setup_user_list_with_remote_member(
    instance_a: MisskeyLikeClient,
    instance_b: MisskeyLikeClient,
    alice: dict,
    bob: dict,
) -> None:
    """alice が user list を作って bob (remote) を member に追加する。

    User list の membership は user_list_membership テーブルに保存される。
    切替後に list 自体と member 構成が引き継がれることを後段で検証する。
    """
    remote_bob = poll_until(
        lambda: instance_a.users_show("bob", host=B_DOMAIN),
        timeout=60,
        desc="alice resolves bob",
    )

    # 既存の同名 list があれば再利用 (再実行性)
    existing = instance_a._api("users/lists/list")
    list_id = None
    for lst in existing:
        if lst.get("name") == LIST_NAME:
            list_id = lst["id"]
            break
    if list_id is None:
        created = instance_a._api("users/lists/create", {"name": LIST_NAME})
        list_id = created["id"]

    try:
        instance_a._api("users/lists/push", {"listId": list_id, "userId": remote_bob["id"]})
    except RuntimeError as e:
        # 既に member の場合は許容
        if "ALREADY_ADDED" not in str(e) and "Already" not in str(e):
            raise

    # 直後に show して list 自体が存在することを確認 (member 構成の確認は
    # 後段で user-list-timeline 経由で行う。Misskey TS の `users/lists/show` は
    # `userIds: string[]` を返すが、mk-go の同 endpoint は raw UserList を
    # 返すだけで userIds を含まない既知の API gap がある — drop-in test では
    # この差分の影響を避けるため間接検証に倒す)。
    shown = instance_a._api("users/lists/show", {"listId": list_id})
    assert shown.get("id") == list_id, "user list lookup failed right after creation"


def test_setup_dangling_renote_target(
    instance_a: MisskeyLikeClient,
    alice: dict,
) -> None:
    """引用先 / リノート先が削除された行を DB-A に残す (#2629)。

    upstream Misskey は 2025.8.0 (migration 1753868431598 /
    misskey-dev#16332「ノートの脱CASCADE削除」) で note の自己参照 FK を削除した。
    以後、元ノートを削除しても、それを参照するリノート / 引用の renoteId は
    **そのまま残る** (frontend が「削除されたノート」として描画する正常な状態)。

    mk-go 側の migration が 000001 でこの FK を張ると、`ADD CONSTRAINT` が既存行を
    全件検証するためこの行に当たって 23503 で失敗し、golang-migrate は version 1 で
    dirty のまま停止する。**運用実績のある TS インスタンスほど踏む**ので、swap 前に
    その状態を作って mk-go 側が引き継げることを検証する。

    この setup が無いと空に近い DB しか渡らず、原理的に検出できない。
    """
    target = instance_a.create_note(DANGLING_TARGET_TEXT)["createdNote"]
    target_id = target["id"]

    # pure renote (text 無し) と quote (text あり) の両方を作る。後段の
    # 000081 は「本文が無い孤児」だけを消すので、両方あると削除範囲も確かめられる。
    pure = instance_a._api("notes/create", {"renoteId": target_id})["createdNote"]
    quote = instance_a.create_note(DANGLING_QUOTE_TEXT, renoteId=target_id)["createdNote"]

    instance_a.delete_note(target_id)

    # 参照元は残り、renoteId も保持されている (upstream に FK が無いため)。
    for note_id, label in ((pure["id"], "pure renote"), (quote["id"], "quote")):
        shown = instance_a._api("notes/show", {"noteId": note_id})
        assert shown.get("renoteId") == target_id, (
            f"{label} should keep renoteId after the target was deleted "
            "(upstream 2025.8.0+ has no self-referencing FK)"
        )
