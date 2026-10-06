"""Differential endpoint tests (#2089): call the same endpoint on mk-go and TS,
diff the JSON responses, fail on value-level deviations.

Currently 35 endpoint comparisons (see docs/diff-e2e.md). Extend with more endpoints as
the harness matures; each confirmed deviation should become a #2078 sub-issue.

Run via the diff-runner container: `make diff-test`.
"""

from __future__ import annotations

import base64

from diff_core import DEFAULT_IGNORE_KEYS, diff_json, format_diffs

# meta carries a lot of operator-specific config (names, urls, versions, limits
# that depend on the instance config). Ignore those so the diff focuses on
# structural/value parity of the shared fields.
META_IGNORE = DEFAULT_IGNORE_KEYS | {
    # operator config strings / theming
    "name", "shortName", "description", "maintainerName", "maintainerEmail",
    "tosUrl", "privacyPolicyUrl", "inquiryUrl", "repositoryUrl", "feedbackUrl",
    "serverRules", "themeColor", "bannerUrl", "backgroundImageUrl", "logoImageUrl",
    "iconUrl", "infoImageUrl", "notFoundImageUrl", "serverErrorImageUrl",
    "defaultLightTheme", "defaultDarkTheme", "mascotImageUrl", "languages",
    # instance-specific addressing / state (host-derived URL, per-instance proxy account)
    "mediaProxy", "proxyAccountName", "proxyAccountId",
    # mk-go 独自の additive field。`version` は drop-in 互換のため互換 Misskey
    # バージョンを返す契約なので、mk-go の実装版は別 field にしている (#2274)。
    # TS 側に存在しないのが仕様。
    "mkGoVersion",
    # ビルドした revision (#2700)。/about-elythia が「Elythia 1.3.0 (abc1234)」として
    # 出す。TS 側に
    # 対応物が無い。docs/divergence.md に additive field として記載済み。
    # **埋め込みの無いビルドでは空文字**になるので、値ではなくキーごと無視する
    # (CI の diff harness は build-arg を渡さないので必ず空になる)。
    "mkGoCommit",
    # 新規登録の username の最小文字数 (#3015)。upstream には対応する設定が
    # 無い (`preservedUsernames` は名前を 1 つずつ列挙する仕組みなので
    # 「2 文字以下を全部」を表現できない)。登録フォームが判定に使うので公開
    # meta に出している。docs/divergence.md に additive field として記載済み。
    "minimumUsernameLength",
    # 分割アップロード (#2313) は mk-go 独自機能なので policies に TS 側の
    # 対応キーが無い。docs/divergence.md に additive field として記載済み。
    "canUseChunkedUpload", "chunkedUploadMaxConcurrentSessions",
    "chunkedUploadMaxPendingMb",
    # ロール単位の通知 opt-out (#2898) も mk-go 独自 policy。TS 側に対応キーが
    # 無い。docs/divergence.md に additive field として記載済み。
    "optOutNotificationTypes",
    # カスタム絵文字の登録申請 (#2934) も mk-go 独自 policy。upstream には申請
    # という概念自体が無いので TS 側にキーが無い。docs/divergence.md に
    # additive field として記載済み。
    "canRequestCustomEmojis",
    # 申請のロール別の期間上限 (#2958) と審査待ち上限 (#2977)。同上。
    "emojiApplicationMaxPerDay", "emojiApplicationMaxPerWeek",
    "emojiApplicationMaxPerMonth", "emojiApplicationMaxPending",
    # カスタム絵文字をアバターデコレーションにできるか (#2975) も mk-go 独自
    # policy。upstream の avatar decoration は管理者が登録した素材しか使えない
    # ので TS 側にキーが無い。docs/divergence.md に additive field として記載済み。
    "canUseEmojiAsAvatarDecoration",
    # IP から関連アカウントを引けるか (#3104) も mk-go 独自 policy。upstream の
    # admin/get-user-ips は requireAdmin で policy を持たないため TS 側にキーが
    # 無い。docs/divergence.md に additive field として記載済み。
    "canSearchIpHistory",
    # 本人退会と user/profile 行の物理削除を制御する (#3207) mk-go 独自
    # policy。upstream には対応キーが無い。docs/divergence.md に additive
    # field として記載済み。
    "canDeleteAccount", "canPurgeAccount",
    # 承認制の登録 (#2554 / #2555) は mk-go 独自機能なので TS 側にキーが無い。
    # meta 直下と features の両方に出る (frontend は features を feature
    # detection に使うため片方だけだと検出できない)。docs/divergence.md に
    # additive field として記載済み。
    "approvalRequiredForSignup",
    # 申請フォームの定義 (#2570)。承認制と同じく mk-go 独自で、申請ページが
    # 描画に使うので公開 meta に出す必要がある。
    "signupApplicationForm",
    # 新規登録を受け付けない (#3186) も mk-go 独自。登録可否そのものは本家の
    # disableRegistration / features.registration で表すので、比べる意味があるのは
    # そちらだけ。docs/divergence.md に additive field として記載済み。
    "registrationClosed",
    "globalTimeline", "localTimeline",
}


def test_meta_value_parity(mkgo, ts):
    mk = mkgo.json("meta", {"detail": True})
    tj = ts.json("meta", {"detail": True})
    diffs = diff_json(mk, tj, ignore_keys=META_IGNORE)
    assert not diffs, format_diffs(diffs)


def test_note_packing_parity(mkgo, ts):
    # Create an equivalent note on each instance and compare the packed object.
    text = "diff-harness parity probe"
    mk_note = mkgo.json("notes/create", {"text": text, "visibility": "public"})["createdNote"]
    ts_note = ts.json("notes/create", {"text": text, "visibility": "public"})["createdNote"]

    mk_show = mkgo.json("notes/show", {"noteId": mk_note["id"]})
    ts_show = ts.json("notes/show", {"noteId": ts_note["id"]})

    # user / author objects are entirely instance-specific (id/host/keys/dates).
    note_ignore = DEFAULT_IGNORE_KEYS | {"userId", "user", "renoteId", "replyId"}
    diffs = diff_json(mk_show, ts_show, ignore_keys=note_ignore)
    assert not diffs, format_diffs(diffs)


# user 固有値 (counts は fresh では 0 で一致するので無視しない)。
USER_IGNORE = DEFAULT_IGNORE_KEYS | {
    "avatarDecorations", "roles", "badgeRoles", "emojis", "instance",
    "avatarColor", "bannerColor", "followersCount", "followingCount",
    # notesCount は eventual-consistency (mk-go は note 作成で即時、TS は chart/job
    # で遅延更新) のため瞬間値が割れる timing-state。安定比較対象外。
    "notesCount",
    # onlineStatus は lastActiveDate 由来の timing-state (mk-go は activity で即
    # 'online'、TS は throttle で 'unknown')。安定した parity 比較対象ではない。
    "onlineStatus", "lastActiveDate",
    # (#2091 修正済: isAdmin/isModerator は self-view で populate されるように
    # なったため回帰 gate に戻した。ここで ignore しない。)
    # 分割アップロード (#2313) は mk-go 独自機能で TS 側に対応キーが無い。
    # users/show は policies を含むので META_IGNORE と同じ除外が要る。
    "canUseChunkedUpload", "chunkedUploadMaxConcurrentSessions",
    "chunkedUploadMaxPendingMb",
    # ロール単位の通知 opt-out (#2898)。同上。
    "optOutNotificationTypes",
    # カスタム絵文字の登録申請 (#2934)。同上。
    "canRequestCustomEmojis",
    # 申請のロール別の期間上限 (#2958) と審査待ち上限 (#2977)。同上。
    "emojiApplicationMaxPerDay", "emojiApplicationMaxPerWeek",
    "emojiApplicationMaxPerMonth", "emojiApplicationMaxPending",
    # カスタム絵文字をアバターデコレーションにできるか (#2975) も mk-go 独自
    # policy。upstream の avatar decoration は管理者が登録した素材しか使えない
    # ので TS 側にキーが無い。docs/divergence.md に additive field として記載済み。
    "canUseEmojiAsAvatarDecoration",
    # IP から関連アカウントを引けるか (#3104) も mk-go 独自 policy。upstream の
    # admin/get-user-ips は requireAdmin で policy を持たないため TS 側にキーが
    # 無い。docs/divergence.md に additive field として記載済み。
    "canSearchIpHistory",
    # 本人退会と user/profile 行の物理削除を制御する (#3207)。同上。
    "canDeleteAccount", "canPurgeAccount",
}


def test_user_packing_parity(mkgo, ts):
    mk = mkgo.json("users/show", {"username": "alice"})
    tj = ts.json("users/show", {"username": "alice"})
    diffs = diff_json(mk, tj, ignore_keys=USER_IGNORE)
    assert not diffs, format_diffs(diffs)


def test_note_with_reaction_parity(mkgo, ts):
    # Create a note and react with a unicode emoji on each, compare the packed
    # reactions map / counts.
    for c in (mkgo, ts):
        note = c.json("notes/create", {"text": "react probe", "visibility": "public"})["createdNote"]
        c.json("notes/reactions/create", {"noteId": note["id"], "reaction": "\U0001F44D"})
        c._probe_note_id = note["id"]

    mk_show = mkgo.json("notes/show", {"noteId": mkgo._probe_note_id})
    ts_show = ts.json("notes/show", {"noteId": ts._probe_note_id})

    note_ignore = DEFAULT_IGNORE_KEYS | {"userId", "user", "renoteId", "replyId"}
    diffs = diff_json(mk_show, ts_show, ignore_keys=note_ignore)
    assert not diffs, format_diffs(diffs)


NOTE_IGNORE = DEFAULT_IGNORE_KEYS | {"userId", "user", "renoteId", "replyId"}


def test_note_with_reply_parity(mkgo, ts):
    # parent note -> reply -> show the reply, compare the packed reply (incl. the
    # embedded parent under `reply`).
    for c in (mkgo, ts):
        parent = c.json("notes/create", {"text": "parent", "visibility": "public"})["createdNote"]
        reply = c.json("notes/create", {"text": "child reply", "replyId": parent["id"], "visibility": "public"})["createdNote"]
        c._probe_note_id = reply["id"]
    mk_show = mkgo.json("notes/show", {"noteId": mkgo._probe_note_id})
    ts_show = ts.json("notes/show", {"noteId": ts._probe_note_id})
    # the embedded `reply` object is itself a note (instance-specific ids/user).
    diffs = diff_json(mk_show, ts_show, ignore_keys=NOTE_IGNORE, ignore_paths={"$.reply"})
    assert not diffs, format_diffs(diffs)


def test_note_with_renote_parity(mkgo, ts):
    # pure renote (no text) of a parent; compare the packed renote object.
    for c in (mkgo, ts):
        parent = c.json("notes/create", {"text": "renote target", "visibility": "public"})["createdNote"]
        renote = c.json("notes/create", {"renoteId": parent["id"], "visibility": "public"})["createdNote"]
        c._probe_note_id = renote["id"]
    mk_show = mkgo.json("notes/show", {"noteId": mkgo._probe_note_id})
    ts_show = ts.json("notes/show", {"noteId": ts._probe_note_id})
    diffs = diff_json(mk_show, ts_show, ignore_keys=NOTE_IGNORE, ignore_paths={"$.renote"})
    assert not diffs, format_diffs(diffs)


def test_note_hashtags_parity(mkgo, ts):
    # text -> tags extraction (hashtags). mentions carry user ids (instance-
    # specific) so we ignore `mentions` and compare `tags`.
    text = "hello #harness #ParityCheck world"
    mk_note = mkgo.json("notes/create", {"text": text, "visibility": "public"})["createdNote"]
    ts_note = ts.json("notes/create", {"text": text, "visibility": "public"})["createdNote"]
    mk_show = mkgo.json("notes/show", {"noteId": mk_note["id"]})
    ts_show = ts.json("notes/show", {"noteId": ts_note["id"]})
    diffs = diff_json(mk_show, ts_show, ignore_keys=NOTE_IGNORE | {"mentions"})
    assert not diffs, format_diffs(diffs)


def test_note_state_parity(mkgo, ts):
    for c in (mkgo, ts):
        note = c.json("notes/create", {"text": "state probe", "visibility": "public"})["createdNote"]
        c._probe_note_id = note["id"]
    mk = mkgo.json("notes/state", {"noteId": mkgo._probe_note_id})
    tj = ts.json("notes/state", {"noteId": ts._probe_note_id})
    diffs = diff_json(mk, tj)
    assert not diffs, format_diffs(diffs)


# /api/i (authoritative MeDetailed). 自己固有 / role 依存 / mk-go 独自の additive を
# 吸収する。**版ずれ由来の除外は 1 つも無い** — #2303 で 3 件 (app192IconUrl /
# app512IconUrl / singleUserMode) を整理して以降、下の各項目はすべて版に依存しない
# 理由で入っている。TS image を上げたときにここを読み直す運用は続けること
# (docs/upstream-catch-up.md の checklist)。
I_IGNORE = META_IGNORE | {
    "policies",  # role policy 依存 (instance role 設定で変わる)
    "avatarId", "bannerId", "achievements", "loggedInDays", "signupReason",
    "twoFactorEnabled", "usePasswordLessLogin", "securityKeys",
    "notesCount", "twoFactorBackupCodesStock", "lastActiveDate", "onlineStatus",
    "isAdmin", "isModerator",  # role 依存 (root fallback) は users/show 側で gate 済
    # room / clientData は mk-go の **意図的な** pass-through extra (upstream は削除済
    # だが mk-go は client 固有 JSON 保存機能として read/write/test 付きで保持、#2094 で
    # intentional と判断)。upstream に無い field なので harness では永続無視する。
    "room", "clientData",
    # (#2094 修正済: moderationNote は moderator viewer に emit するようになったため
    # ここで ignore しない = 回帰 gate に戻した。)
}


def test_i_meDetailed_parity(mkgo, ts):
    mk = mkgo.json("i", {})
    tj = ts.json("i", {})
    diffs = diff_json(mk, tj, ignore_keys=I_IGNORE)
    assert not diffs, format_diffs(diffs)


# 独自 packer を持つ entity (clip / user list / channel / antenna)。
ENTITY_IGNORE = DEFAULT_IGNORE_KEYS | {"userId", "user"}


def test_clip_packing_parity(mkgo, ts):
    for c in (mkgo, ts):
        clip = c.json("clips/create", {"name": "harness clip", "isPublic": True, "description": "d"})
        c._probe = clip["id"]
    mk = mkgo.json("clips/show", {"clipId": mkgo._probe})
    tj = ts.json("clips/show", {"clipId": ts._probe})
    diffs = diff_json(mk, tj, ignore_keys=ENTITY_IGNORE)
    assert not diffs, format_diffs(diffs)


def test_userlist_packing_parity(mkgo, ts):
    for c in (mkgo, ts):
        lst = c.json("users/lists/create", {"name": "harness list"})
        c._probe = lst["id"]
    mk = mkgo.json("users/lists/show", {"listId": mkgo._probe})
    tj = ts.json("users/lists/show", {"listId": ts._probe})
    diffs = diff_json(mk, tj, ignore_keys=ENTITY_IGNORE)
    assert not diffs, format_diffs(diffs)


def test_channel_packing_parity(mkgo, ts):
    for c in (mkgo, ts):
        ch = c.json("channels/create", {"name": "harness channel", "description": "ch desc"})
        c._probe = ch["id"]
    mk = mkgo.json("channels/show", {"channelId": mkgo._probe})
    tj = ts.json("channels/show", {"channelId": ts._probe})
    diffs = diff_json(mk, tj, ignore_keys=ENTITY_IGNORE)
    assert not diffs, format_diffs(diffs)


def test_antenna_packing_parity(mkgo, ts):
    params = {
        "name": "harness antenna", "src": "all", "keywords": [["hello"]],
        "excludeKeywords": [], "users": [], "caseSensitive": False,
        "withReplies": False, "withFile": False, "notify": False,
    }
    for c in (mkgo, ts):
        a = c.json("antennas/create", params)
        c._probe = a["id"]
    mk = mkgo.json("antennas/show", {"antennaId": mkgo._probe})
    tj = ts.json("antennas/show", {"antennaId": ts._probe})
    diffs = diff_json(mk, tj, ignore_keys=ENTITY_IGNORE)
    assert not diffs, format_diffs(diffs)


# 1x1 transparent PNG for drive upload parity.
_PNG_1x1 = base64.b64decode(
    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII="
)


def _upload(c, name="probe.png", *, payload: bytes | None = None):
    """Upload one file, optionally with distinct bytes.

    Drive は md5 で dedup するので、同じ内容を上げ直しても行は増えない。
    cursor ページングを試すときは payload を変えること (#2765)。
    """
    resp = c.session.post(
        f"{c.base}/api/drive/files/create",
        files={"file": (name, payload if payload is not None else _PNG_1x1, "image/png")},
        data={"i": c.token},
        timeout=30,
    )
    resp.raise_for_status()
    return resp.json()


def test_drivefile_packing_parity(mkgo, ts):
    for c in (mkgo, ts):
        f = _upload(c)
        c._probe = f["id"]
    mk = mkgo.json("drive/files/show", {"fileId": mkgo._probe})
    tj = ts.json("drive/files/show", {"fileId": ts._probe})
    # blurhash / properties は画像処理 impl 依存で割れうるので除外。md5/size/type/name は
    # 同一コンテンツなので比較対象に残す。
    file_ignore = DEFAULT_IGNORE_KEYS | {"userId", "user", "folderId", "folder", "blurhash", "properties"}
    diffs = diff_json(mk, tj, ignore_keys=file_ignore)
    assert not diffs, format_diffs(diffs)


def test_rich_profile_parity(mkgo, ts):
    update = {
        "description": "hello bio", "location": "Tokyo", "birthday": "2000-01-01",
        "lang": "ja", "fields": [{"name": "site", "value": "https://example.com"}],
    }
    for c in (mkgo, ts):
        c.json("i/update", update)
    mk = mkgo.json("users/show", {"username": "alice"})
    tj = ts.json("users/show", {"username": "alice"})
    diffs = diff_json(mk, tj, ignore_keys=USER_IGNORE)
    assert not diffs, format_diffs(diffs)


def test_locked_follow_request_parity(mkgo, ts):
    # bob locks his account, alice follows -> pending follow request. Compares the
    # relation block (hasPendingFollowRequestFromYou etc.) on users/show(bob).
    for c in (mkgo, ts):
        bob = _create_second_user(c, "boblocked")
        c.call("i/update", {"isLocked": True}, token=bob["token"])
        c.json("following/create", {"userId": bob["id"]})
        c._probe = bob["id"]
    mk = mkgo.json("users/show", {"userId": mkgo._probe})
    tj = ts.json("users/show", {"userId": ts._probe})
    diffs = diff_json(mk, tj, ignore_keys=USER_IGNORE)
    assert not diffs, format_diffs(diffs)


def _create_second_user(admin, username):
    # admin (alice, root) creates a 2nd account via the authed admin path.
    resp = admin.call("admin/accounts/create", {"username": username, "password": "test-password-1234"})
    resp.raise_for_status()
    return resp.json()


def test_user_relation_parity(mkgo, ts):
    # alice follows bob on each instance, then compares users/show(bob) — the
    # relation block (isFollowing/isFollowed/...) is computed, a likely diff site.
    for c in (mkgo, ts):
        bob = _create_second_user(c, "bob")
        c.json("following/create", {"userId": bob["id"]})
        c._probe = bob["id"]
    mk = mkgo.json("users/show", {"userId": mkgo._probe})
    tj = ts.json("users/show", {"userId": ts._probe})
    # (#2097 修正済: follower には followedMessage=null を emit するようになったため
    # 回帰 gate に戻した = ここで ignore しない。)
    diffs = diff_json(mk, tj, ignore_keys=USER_IGNORE)
    assert not diffs, format_diffs(diffs)


def test_drivefolder_packing_parity(mkgo, ts):
    for c in (mkgo, ts):
        folder = c.json("drive/folders/create", {"name": "harness folder"})
        c._probe = folder["id"]
    mk = mkgo.json("drive/folders/show", {"folderId": mkgo._probe})
    tj = ts.json("drive/folders/show", {"folderId": ts._probe})
    diffs = diff_json(mk, tj, ignore_keys=DEFAULT_IGNORE_KEYS | {"parentId"})
    assert not diffs, format_diffs(diffs)


def test_oauth_app_packing_parity(mkgo, ts):
    for c in (mkgo, ts):
        app = c.json("app/create", {"name": "harness app", "description": "d", "permission": ["read:account"]})
        c._probe = app["id"]
    mk = mkgo.json("app/show", {"appId": mkgo._probe})
    tj = ts.json("app/show", {"appId": ts._probe})
    diffs = diff_json(mk, tj, ignore_keys=DEFAULT_IGNORE_KEYS | {"userId"})
    assert not diffs, format_diffs(diffs)


def test_page_packing_parity(mkgo, ts):
    page_params = {
        "title": "harness page", "name": "harness-page", "content": [], "variables": [],
        "script": "", "eyeCatchingImageId": None, "font": "sans-serif",
        "alignCenter": False, "hideTitleWhenPinned": False,
    }
    for c in (mkgo, ts):
        p = c.json("pages/create", page_params)
        c._probe = p["id"]
    mk = mkgo.json("pages/show", {"pageId": mkgo._probe})
    tj = ts.json("pages/show", {"pageId": ts._probe})
    # visibility は drop-in #367 由来の意図的な mk-go extra (upstream page に無い)。
    page_ignore = DEFAULT_IGNORE_KEYS | {"userId", "user", "visibility"}
    diffs = diff_json(mk, tj, ignore_keys=page_ignore)
    assert not diffs, format_diffs(diffs)


def test_announcement_packing_parity(mkgo, ts):
    for c in (mkgo, ts):
        c.json("admin/announcements/create", {"title": "harness ann", "text": "body", "imageUrl": None})
    mk = mkgo.json("announcements", {})
    tj = ts.json("announcements", {})
    # (#2101 修正済: user-facing から admin field forExistingUsers/isActive を除去した
    # ため回帰 gate に戻した = ここで ignore しない。)
    diffs = diff_json(mk, tj, ignore_keys=DEFAULT_IGNORE_KEYS)
    assert not diffs, format_diffs(diffs)


def test_emoji_packing_parity(mkgo, ts):
    for c in (mkgo, ts):
        f = _upload(c, "emoji.png")
        c.json("admin/emoji/add", {"fileId": f["id"], "name": "harness_emoji", "category": "test", "aliases": ["he"]})
    mk = mkgo.json("emojis", {})
    tj = ts.json("emojis", {})
    diffs = diff_json(mk, tj, ignore_keys=DEFAULT_IGNORE_KEYS)
    assert not diffs, format_diffs(diffs)


def test_flash_parity(mkgo, ts):
    params = {"title": "harness flash", "summary": "s", "script": "<: 'hi'", "permissions": []}
    for c in (mkgo, ts):
        f = c.json("flash/create", params)
        c._probe = f["id"]
    mk = mkgo.json("flash/show", {"flashId": mkgo._probe})
    tj = ts.json("flash/show", {"flashId": ts._probe})
    diffs = diff_json(mk, tj, ignore_keys=DEFAULT_IGNORE_KEYS | {"userId", "user"})
    assert not diffs, format_diffs(diffs)


def test_favorites_parity(mkgo, ts):
    for c in (mkgo, ts):
        note = c.json("notes/create", {"text": "fav me", "visibility": "public"})["createdNote"]
        c.json("notes/favorites/create", {"noteId": note["id"]})
    mk = mkgo.json("i/favorites", {})
    tj = ts.json("i/favorites", {})
    diffs = diff_json(mk, tj, ignore_keys=DEFAULT_IGNORE_KEYS | {"noteId", "note"})
    assert not diffs, format_diffs(diffs)


def test_mute_list_parity(mkgo, ts):
    for c in (mkgo, ts):
        bob = _create_second_user(c, "bobmute")
        c.json("mute/create", {"userId": bob["id"]})
    mk = mkgo.json("mute/list", {})
    tj = ts.json("mute/list", {})
    diffs = diff_json(mk, tj, ignore_keys=DEFAULT_IGNORE_KEYS | {"muteeId", "mutee"})
    assert not diffs, format_diffs(diffs)


TIMELINE_IGNORE = DEFAULT_IGNORE_KEYS | {"userId", "user", "renoteId", "replyId"}


def test_home_timeline_parity(mkgo, ts):
    # create a fresh note, then read the home timeline (limit=1). The note must
    # be fanned out to the creator's own home timeline and packed identically.
    for c in (mkgo, ts):
        c.json("notes/create", {"text": "home timeline probe", "visibility": "public"})
    mk = mkgo.json("notes/timeline", {"limit": 1})
    tj = ts.json("notes/timeline", {"limit": 1})
    diffs = diff_json(mk, tj, ignore_keys=TIMELINE_IGNORE)
    assert not diffs, format_diffs(diffs)


def test_local_timeline_parity(mkgo, ts):
    for c in (mkgo, ts):
        c.json("notes/create", {"text": "local timeline probe", "visibility": "public"})
    mk = mkgo.json("notes/local-timeline", {"limit": 1})
    tj = ts.json("notes/local-timeline", {"limit": 1})
    diffs = diff_json(mk, tj, ignore_keys=TIMELINE_IGNORE)
    assert not diffs, format_diffs(diffs)


def test_user_notes_timeline_parity(mkgo, ts):
    for c in (mkgo, ts):
        c.json("notes/create", {"text": "user notes probe", "visibility": "public"})
        c._probe = c.json("i", {})["id"]
    mk = mkgo.json("users/notes", {"userId": mkgo._probe, "limit": 1})
    tj = ts.json("users/notes", {"userId": ts._probe, "limit": 1})
    diffs = diff_json(mk, tj, ignore_keys=TIMELINE_IGNORE)
    assert not diffs, format_diffs(diffs)


def test_home_timeline_followee_parity(mkgo, ts):
    # alice follows bob; bob posts; bob's note must be fanned out to alice's home
    # timeline (cross-user fanout — the core of FanoutTimeline).
    for c in (mkgo, ts):
        bob = _create_second_user(c, "bobtl")
        c.json("following/create", {"userId": bob["id"]})
        c.call("notes/create", {"text": "followee fanout probe", "visibility": "public"}, token=bob["token"])
    mk = mkgo.json("notes/timeline", {"limit": 1})
    tj = ts.json("notes/timeline", {"limit": 1})
    diffs = diff_json(mk, tj, ignore_keys=TIMELINE_IGNORE)
    assert not diffs, format_diffs(diffs)


def test_note_with_poll_parity(mkgo, ts):
    poll = {"choices": ["alpha", "beta", "gamma"], "multiple": False}
    mk_note = mkgo.json("notes/create", {"text": "poll probe", "visibility": "public", "poll": poll})["createdNote"]
    ts_note = ts.json("notes/create", {"text": "poll probe", "visibility": "public", "poll": poll})["createdNote"]

    mk_show = mkgo.json("notes/show", {"noteId": mk_note["id"]})
    ts_show = ts.json("notes/show", {"noteId": ts_note["id"]})

    # compare just the packed poll object (the rest is covered by note packing).
    note_ignore = DEFAULT_IGNORE_KEYS | {"userId", "user", "renoteId", "replyId"}
    diffs = diff_json(mk_show.get("poll"), ts_show.get("poll"), ignore_keys=note_ignore, path="$.poll")
    assert not diffs, format_diffs(diffs)


# --- sinceId 単独指定のページング (#2765) ---
#
# **upstream の `makePaginationQuery` は sinceId / sinceDate 単独のときだけ ASC で
# 返す** (`sinceId && !untilId` の分岐)。mk-go は `paginationOrder` で同じ規則を持つ
# (#2713)。frontend の paginator は「もっと新しいものを読む」(`fetchNewer`) でこの形を
# 投げるので、向きが逆だとページが飛ぶ。
#
# **本家 backend e2e には既に 2 本ある** —
# `packages/backend/test/e2e/timelines.ts` が `users/notes` の
# sinceId 単独 (ASC) と sinceId+untilId (DESC) を `deepStrictEqual` でリテラル配列に
# 固定していて、これは mk-go に対しても実行されている (exclude にも
# known-divergences にも入っていない。`describe.each` の FTT on/off で計 4 実行)。
#
# **守られていたのは `users/notes` だけ。** `clips.ts` も `users/clips` /
# `clips/notes` に sinceId を投げるが、3 箇所とも `res.sort(compareBy(s => s.id))`
# で**両辺を並べ替えてから**比較しており、集合しか見ていない (順序回帰は落ちない)。
#
# **無かったのは mk-go 側で管理するゲート**で、`tests/` には 1 本も無い。
#
# 各テストは diff (mk-go と TS が一致するか) に加えて **向きそのものを直接
# assert する**。diff だけだと「両方 DESC」でも通ってしまい、TS 側の実装に
# 依存してしまうため。


def _seed_notes(c, prefix: str, count: int) -> list[str]:
    """Create `count` public notes in order and return their ids (oldest first).

    count は必須。既定値を持たせると「候補 <= limit」の呼び出しを書きやすくなり、
    そこでは ASC と DESC が同じ行を返してしまう (下の各テストのコメントを参照)。
    """
    ids = []
    for i in range(count):
        note = c.json("notes/create", {"text": f"{prefix} {i}", "visibility": "public"})["createdNote"]
        ids.append(note["id"])
    return ids


def _assert_page(rows, key: str, want: list[str], label: str) -> None:
    """Assert the page is exactly `want`, in that order."""
    got = [r[key] for r in rows]
    assert got == want, f"{label}: sinceId 単独は昇順で最古から返すこと (got={got}, want={want})"


def test_home_timeline_since_id_parity(mkgo, ts):
    # notes/timeline に sinceId を投げると、Redis に ID が揃っていても DB へ
    # 倒れる (upstream の shouldFallbackToDb が sinceId 非空で常に真、#2720)。
    # つまりこの経路は、**DB fallback が有効な限り** FTT の有無に関わらず SQL の
    # 並び順で決まる。`meta.enableFanoutTimelineDbFallback` を off にすると
    # 空が返る (#2762、docs/divergence/operations.md §5.6)。既定は on なのでこのテストは
    # 通るが、off の環境では `got=[]` で落ちる (silent pass にはならない)。
    #
    # **候補 4 件に対して limit 2 で読む。** 候補 <= limit だと `ORDER BY id ASC`
    # と `ORDER BY id DESC` が同じ行を返してしまい、「SQL は DESC のまま Go 側で
    # slice を reverse する」実装を素通ししてしまう。それは順序は合うが**返す行の
    # 集合が違う** (最古 n 件ではなく最新 n 件) ので、ページに穴が空く。
    # 実運用のリクエストもこの形で、paginator の fetchNewer は limit 30 を投げる。
    prefix = "since home probe"
    pages = {}
    for c, label in ((mkgo, "mk-go"), (ts, "TS")):
        ids = _seed_notes(c, prefix, 5)
        page = c.json("notes/timeline", {"sinceId": ids[0], "limit": 2})
        _assert_page(page, "text", [f"{prefix} 1", f"{prefix} 2"], f"{label} notes/timeline")
        pages[label] = page

    diffs = diff_json(pages["mk-go"], pages["TS"], ignore_keys=TIMELINE_IGNORE)
    assert not diffs, format_diffs(diffs)


def test_user_notes_since_id_parity(mkgo, ts):
    # users/notes は fanout を通らず ListByUserIDFiltered に直行する。
    # timeline 系とは別の SQL 経路。本家 e2e の timelines.ts も同じ endpoint を
    # 見ているが、あちらは mk-go 単体の assert で値レベルの突き合わせはしない。
    prefix = "since usernotes probe"
    pages = {}
    for c, label in ((mkgo, "mk-go"), (ts, "TS")):
        ids = _seed_notes(c, prefix, 5)
        page = c.json("users/notes", {"userId": c.json("i", {})["id"], "sinceId": ids[0], "limit": 2})
        _assert_page(page, "text", [f"{prefix} 1", f"{prefix} 2"], f"{label} users/notes")
        pages[label] = page

    diffs = diff_json(pages["mk-go"], pages["TS"], ignore_keys=TIMELINE_IGNORE)
    assert not diffs, format_diffs(diffs)


def test_drive_folders_since_id_parity(mkgo, ts):
    # #2713 / PR #2764 が実際に直した経路 (internal/repository/drive_folder.go の
    # ListByUser)。note 系は元から ASC だったので、そこだけ見ても #2713 の回帰は
    # 捕まらない。
    names = [f"since folder probe {i}" for i in range(5)]
    pages = {}
    for c, label in ((mkgo, "mk-go"), (ts, "TS")):
        ids = [c.json("drive/folders/create", {"name": n})["id"] for n in names]
        page = c.json("drive/folders", {"sinceId": ids[0], "limit": 2})
        _assert_page(page, "name", names[1:3], f"{label} drive/folders")
        pages[label] = page

    diffs = diff_json(pages["mk-go"], pages["TS"], ignore_keys=DEFAULT_IGNORE_KEYS | {"parentId"})
    assert not diffs, format_diffs(diffs)


def test_drive_files_since_id_parity(mkgo, ts):
    # drive/files は、このテストを足した時点では **mock からは順序回帰が
    # 見えなかった** 経路 (`MockDriveFileRepository.ListByUser` が sinceID 単独の
    # ASC を実装していなかった)。#2766 で揃えたので今は mock でも見えるが、
    # mock は production の SQL を実行しないので実 API 側のゲートは残す。
    #
    # `sort` を渡さないのは意図的。production も upstream も sort 指定時は
    # paginationOrder を通らず固定 order を使う (`drive_file.go` の switch)。
    # frontend の MkDrive も `-createdAt` のとき sort を送らないので実利用と一致する。
    # **中身を 1 バイトずつ変える。** drive は md5 で dedup するので
    # (`internal/core/drive/drive_service.go` の Force=false 経路。upstream も同じ)、
    # 同じ内容を何度上げても行は 1 つしか増えず、cursor が効かない
    # (実測: ids が全部同じになり sinceId で 0 件になった)。PNG は IEND 以降の
    # バイトを無視するので、末尾に足せば画像としては有効なまま md5 が変わる。
    names = [f"since-file-{i}.png" for i in range(4)]
    pages = {}
    for c, label in ((mkgo, "mk-go"), (ts, "TS")):
        ids = [_upload(c, n, payload=_PNG_1x1 + bytes([i])) for i, n in enumerate(names)]
        ids = [f["id"] for f in ids]
        page = c.json("drive/files", {"sinceId": ids[0], "limit": 2})
        _assert_page(page, "name", names[1:3], f"{label} drive/files")
        pages[label] = page

    file_ignore = DEFAULT_IGNORE_KEYS | {"userId", "user", "folderId", "folder", "blurhash", "properties"}
    diffs = diff_json(pages["mk-go"], pages["TS"], ignore_keys=file_ignore)
    assert not diffs, format_diffs(diffs)


def test_admin_announcements_since_id_parity(mkgo, ts):
    # note / drive 以外の repository (internal/repository/announcement.go)。
    titles = [f"since ann probe {i}" for i in range(5)]
    pages = {}
    for c, label in ((mkgo, "mk-go"), (ts, "TS")):
        ids = [c.json("admin/announcements/create",
                      {"title": t, "text": "body", "imageUrl": None})["id"] for t in titles]
        page = c.json("admin/announcements/list", {"sinceId": ids[0], "limit": 2})
        _assert_page(page, "title", titles[1:3], f"{label} admin/announcements/list")
        pages[label] = page

    diffs = diff_json(pages["mk-go"], pages["TS"], ignore_keys=DEFAULT_IGNORE_KEYS)
    assert not diffs, format_diffs(diffs)
