# API互換性状況

対象バージョン: **Misskey 2026.9.1** (mk-go 1.4.0)
最終更新: 2026-09-23

本ドキュメントは互換性調査 (#107, #124) と、Playwright Phase 1-4 で発見・修正した drift backlog の**履歴**を集約したもの。

> **現在の一次情報はここではない。** 互換性の現況は次を参照すること。本ドキュメントは
> 「どの調査でどこを直したか」を辿るための記録として残している。
>
> | 知りたいこと | 参照先 |
> |---|---|
> | endpoint の実装状況 | [`api-compat.md`](api-compat.md) (`make apicompat` で自動生成) |
> | 意図的な差分の一覧 | [`divergence.md`](divergence.md) |
> | 本家 e2e に対する適合状況 | [`upstream-backend-e2e.md`](upstream-backend-e2e.md) |
> | entity shape の drift | [`shape-drift.md`](shape-drift.md) |

## 概要

- **upstream catch-up**: **2026.9.1 まで追従完了**。2026.3.2 → 2026.5.1 → 2026.5.4 → 2026.6.0 → 2026.7.0 → 2026.9.0 → 2026.9.1 と段階的に追従した (**2026.8.0 に stable は無い**)。各 release 差分は [`docs/update/`](update/) を参照 (`<yyyymm><nn>diff.md`。`nn` は**対象 upstream release の patch 番号**で日付ではない。backend に変更が無い release は doc を作らないので番号は飛ぶ。同じディレクトリに `<yyyymmdd>-<issue>-triage.md` 形式の triage note も同居する)
- **本家 backend e2e**: Misskey 本家の `test/e2e/**` をテスト本体無改変で mk-go に向けて実行する基盤を整備し、**25 ファイル 1256 テストが全通過**。PR ごとに CI で回る。『通らないことが正しい』23 件は根拠付きで expected-failure として登録している ([`upstream-backend-e2e.md`](upstream-backend-e2e.md))
- **Playwright e2e**: 298 spec ファイルを PR ごとに実行。Misskey TS backend に対しては upstream 追従時に実行し、spec が mk-go の挙動に引きずられていないかを検証する
- **drift backlog**: Phase 1-4 の spec 整備中に発見した 40+ 件の drop-in 互換 drift は fix 済

## エンドポイントカバー率 (Playwright Phase 1-4 時点)

以下は Playwright spec による round-trip 検証の到達範囲を、Phase 1-4 完了時点で
まとめたもの。**endpoint の実装状況そのものは [`api-compat.md`](api-compat.md) が
一次情報**で、そちらは `make apicompat` が upstream の endpoint 一覧と突き合わせて
自動生成する。

当時の数値は router.go 登録の 448 endpoint のうち 242 endpoint (54.3%)。残りは
smoke 範囲外 (WebSocket / 複雑 mutation / federation delivery / cron / push server)
または smoke 化困難な mutation。現在は本家 backend e2e が別軸で 1256 テストを
回しているため、Playwright だけで cover 率を語る意味は薄れている。

| カテゴリ | 主要 endpoint 群 | 状態 |
|---|---|---|
| admin/* | user / role / meta / queue / abuse-report / system-webhook / avatar-decorations / ad / invite / announcements / relays / captcha / drive read / stats | ✅ Phase 4 verified |
| notes/* | create / delete / show / visibility / search / search-by-tag | ✅ Phase 1-2 verified |
| users/* | profile / follow / mute / block / list / search / featured / reactions | ✅ Phase 2 verified |
| i/* | favorites / pin / follow_requests / registry / revoke_token / webhooks / 2FA TOTP / 2FA Passkey | ✅ Phase 1-4 verified |
| drive/* | create / find / find-by-hash / folders / mutate | ✅ Phase 2 verified |
| reactions/* | create / delete / replace / different / users_reactions | ✅ Phase 2 verified |
| chat/* | DM / room / messaging_notification / extra | ✅ Phase 2 / 4 verified (mk-go 独自設計部分含む) |
| notifications/* | reaction / mention / follow / reply / renote / follow_request / mark_all_as_read / control | ✅ Phase 2-3 verified |
| timeline/* | home / local / global / hybrid | ✅ Phase 2 verified |
| emoji/* | custom_emoji_reaction / lifecycle / bulk / import_zip | ✅ Phase 2 verified |
| auth/* | signup / signin / signin-invalid / 2FA TOTP / 2FA Passkey | ✅ Phase 1 verified |
| federation/* / ap/* | shape | ✅ Phase 3 verified |
| channels/* | list / search / featured / followed / show / timeline | ✅ Phase 4 verified |
| hashtags/* | search / tags / list | ✅ Phase 4 verified |
| roles/* | role-policy 系 | ✅ Phase 4 verified |
| pages/* / gallery/* / clips/* / flash/* / reversi/* | content 系 | ✅ Phase 2 verified |
| antennas/* / search/* | shape | ✅ Phase 3 verified |
| その他 (charts, miauth, app, sw, utility, …) | shape | ✅ Phase 3-4 verified |

未実装エンドポイントの応答は **method で分かれる**。GET は
`404 UNKNOWN_API_ENDPOINT`、それ以外は `200 {}`。後者を 404 にしないのは、
Misskey 公式フロントの一部ページが未登録エンドポイントの 404 で例外を投げる
ため。**例外はプラグイン同士の通信の受け口** (`/api/plugin/<名前>/_peer`) で、
method に関わらず 404 を返す (#2822。200 だと送信側が「受け口が無い」と
「プラグインが空の応答を返した」を区別できない。詳細は
[peer プロトコル](plugin-peer-protocol.md))。いずれも warn ログ
(`unimplemented API endpoint`) が出る。なお **upstream endpoint の未実装は現在ゼロ (coverage 100.0%、444/444)**。

## 対応済みの互換性修正

### 第1次調査 (#107) サブissue

| # | 内容 | 状態 |
|---|---|---|
| #109 | AP: MFM→HTML変換、attachment、context拡充 | 完了 |
| #110 | NoteEntity/UserDetailed レスポンス完全化 | 完了 |
| #111 | パスワードリセット、MiAuth gen-token、App API | 完了 |
| #112 | Timelineフィルタリング、signin-flow captcha | 完了 |
| #113 | 欠損テーブル7種 + 補助エンドポイント12+ | 完了 |

### 第2次調査 (#124) サブissue

| # | 内容 | 状態 |
|---|---|---|
| #125 | WebSocket 9チャンネル追加 + タイムラインフィルタパラメータ | 完了 |
| #126 | AP Inbox Block/Flag/Move/Add/Remove | 完了 |
| #127 | AP Accept完全実装 + Question(投票)受信 + EmojiReactバリアント | 完了 |
| #128 | users/lists update + update-membership | 完了 |
| #129 | trustProxyサポート | 完了 |
| #130 | エラーレスポンス標準化 (UUID統一) | 完了 |
| #131 | DBスキーマ欠損カラム追加 | 完了 |
| #132 | WebSocketプロトコル改善 (OAuth2スコープ, pong応答) | 完了 |
| #133 | dbSlaves (リードレプリカ)サポート | 完了 |
| #134 | chart tables, social auth, AP拡張, Sentry | 完了 |

### Playwright Phase 1-4 由来の drift backlog (#798 ~ #944)

Playwright spec を両 backend で走らせる中で観測した「TS と mk-go で挙動が違う」 drift を 40+ 件 fix。代表的なもの (一部 PR バンドル含む):

#### auth / signup / signin
| # | 内容 |
|---|---|
| #798 | signup duplicate username の status code (400 DUPLICATED_USERNAME に統一) |
| #800 | username 長さ制限 (20 char max に統一) |
| #802 | error response body shape (Misskey misc 形式に統一) |

#### note / timeline
| # | 内容 |
|---|---|
| #799 | notes/show で visibility 違反時の挙動 (200 で stub note を返す) |
| #874 | timeline endpoint で user mute filter を追加 (#892 / #894 で perf 最適化) |
| #876 | users/lists/list の N+1 query を batch fetch に最適化 |
| #877 | notes/search の external search backend 互換: `fulltextSearch.provider: "none"` を opt-in で追加し upstream TS strict-mode (400 UNAVAILABLE) に揃える経路を提供 (mk-go 既定は SQL LIKE fallback で従来通り動く) |
| #878 | users/search-by-username-and-host の suspend filter |

#### drive
| # | 内容 |
|---|---|
| #812 | drive/files/create userId / user / folder shape |
| #818 | drive/files/find / find-by-hash の packMany self path drift |
| #845 | drive/folders/show detail mode (parent / counts) |
| #977 | drive/folders 系の `NO_SUCH_FOLDER` UUID を endpoint 別に分割 (create `53326628-...` / show `d74ab9eb-...` / update `f7974dac-...` / delete `1069098f-...`) + `folders/update` の parent 不在を `NO_SUCH_PARENT_FOLDER` (`ce104e3a-...`) で区別 |

#### chat
| # | 内容 |
|---|---|
| #851 / #855 / #860 | chat packMessage / packRoom の null 省略・field set drift |
| #864 | reaction emoji の variation selector 正規化 |

#### users / following / blocking
| # | 内容 |
|---|---|
| #870 | blocking/create / delete return shape (UserDetailed を返す) |
| #871 | users/lists/create response shape (createdAt / userIds / isPublic 含む) |
| #872 | blocked → following/create reject status (400) |
| #984 | users/relation を stub から実装に切替。viewer ↔ target の follow / follow-request / block / mute / renote-mute を 5 repo (`following` / `follow_request` / `blocking` / `muting` / `renote_muting`) から実 DB 状態として返す |
| #970 | `/api/users/show` で viewer===target のとき MeDetailed 拡張 field を merge (upstream `pack(user, me)` 互換)。`entity.AsMeDetailed` helper で pre-built UserDetailed を promote |
| #988 | `canChat` 二重 drift 解消: PackUserLite が role policy `chatAvailability === "available"` 由来に切替 (旧 chatScope 比較を撤回) + Me handler の hardcode true override も撤去。新規 `entity.CanChatLookup` interface で entity ↔ role.Service を decouple |

#### settings / token
| # | 内容 |
|---|---|
| #883 | i/regenerate-token return shape (204 No Content) |
| #884 | 旧 API token invalidation (cache 経由でも即時 reject) |
| #885 | i/update-email error status 標準化 |
| #910 | app-issued access token を auth middleware で dual lookup (raw → hash) |
| #913 | i/revoke-token も dual lookup + cache invalidation 化 |
| #985 | `entity.PackMeDetailed` に `emailNotificationTypes` / `mutingNotificationTypes` / `notificationRecieveConfig` を追加 (i/update 経路で frontend `$i` state が settings/email 等の toggle 後に正しく反映される) |
| #971 | `/api/i` Me handler を `PackMeDetailed` ベースに refactor。JSON round-trip で 11 self-view field + notification 3 field の重複コード 25 行を削除、MeDetailed packer 更新時に自動追従 |

#### admin
| # | 内容 |
|---|---|
| #888 | admin/show-user shape (roles / policies / signins / roleAssigns / isHibernated / lastActiveDate) |
| #889 | admin/roles/create paramDef strict (13 field required) |
| #896 / #900 / #901 / #931 / #932 | pq.StringArray drift (avatar-decorations / system-webhook / flash / antenna / emoji bulk の Updates(map) で空 string[] が NULL 化される問題) |
| #907 | i/registry/set で domain field の handling drift |
| #918 | sw/show-registration を 204 No Content に統一 |
| #915 | federation/show-instance を 204 No Content に統一 |
| #925 | hashtags/* paramDef strictness drift |
| #926 | channels の Playwright LCD を strict 化 |
| #929 | admin/queue + admin/abuse-report の paramDef strictness / idempotency drift |
| #936 | i/webhooks/update の status code drift (204) |
| #937 | i/webhooks/test の paramDef drift (type required + webhookEventTypes enum) |
| #939 | i/webhooks/{create,update} の on array enum check |

#### UDS production 由来 bug
| # | 内容 |
|---|---|
| #940 | 遅延配送 remote note の createdAt drift (AP `published` を採用 + clock skew/floor guard) |
| #941 | カスタム絵文字 (リアクション / picker) のアニメ pass-through (mediaproxy で gif/apng を resize しない) |
| #942 | URL summary 文字化け (Shift_JIS / EUC-JP / ISO-2022-JP の自動正規化) |
| #943 | リモートユーザー counts (notesCount / followersCount / followingCount) を origin から fetch する mk-go 独自拡張 (#945 で LRU cache 化) |

## ActivityPub互換性

### 対応済みActivity

| Activity | 送信 | 受信 |
|---|---|---|
| Create (Note) | o | o |
| Delete (Note) | o | o |
| Update (Note/Person) | o | o |
| Follow | o | o |
| Accept (Follow) | o | o |
| Reject (Follow) | o | o |
| Undo (Follow/Like/Announce/Block) | o | o |
| Like (Reaction) | o | o |
| Announce (Renote) | o | o |
| Block | o | o |
| Flag (Report) | - | o |
| Move (Account Migration) | - | o |
| Add/Remove (Pin) | - | o |

### AP Person

MFM→HTML変換、`featured`、`attachment`(プロフィールフィールド)、`tag`(絵文字タグ)、`image`(バナー)は#109で対応済み。

### AP Note

`content`のHTML化、`attachment`(ファイル)、拡張`@context`(Misskey独自vocabulary)は#109で対応済み。Question(投票)オブジェクトの受信は#127で対応済み。

### AP variant handling

upstream / 他実装が出してくる variant に対するロバスト性:

- **`published` の parse + 異常値 fallback** (#940): RFC3339 / RFC3339Nano、未来 5min skew / 過去 10 年で fallback
- **`actor` が embedded object のケースを救済** (#999、upstream #17340)
- **`alsoKnownAs` の array / string 双方受け入れ** (#1000、upstream #17275)
- **存在しない Actor の Delete を ignore** (#1001、upstream #17294)
- **リレー由来 Announce の正しい処理** (#1002、upstream #17308)
- **ブロック中インスタンスの inbox job 蓄積**は verify-in-worker (#565) の構造上起きない (#1003、upstream #17336)

詳細な状態は [federation.md](federation.md) の「AP variant handling」を参照。

## WebSocket/Streaming

### チャンネルカバー率

**upstream の 18 チャンネルをすべて実装**し、mk-go 独自の `notifications` を
加えて 19。#125 で欠損 9 チャンネルを追加した。

`notifications` は upstream に無い (upstream は `main` チャンネルで通知を流す)
ので、これに依存するクライアントは Misskey TS では動かない。

プロトコル改善 (#132):
- OAuth2スコープに基づくチャンネルアクセス制御 (チャンネル単位の判定が実際に
  効くようになったのは 1.2.1 より後。それ以前は接続にスコープが載っておらず
  素通ししていた)
- `pong`応答の実装
- パラメータバリデーション強化

## mk-go 独自の挙動 / 拡張

upstream Misskey TS と異なる、mk-go ならではの実装:

### RemoteStatsFetcher (#943)

Misskey TS の `users/show` は **自インスタンスで観測した範囲** のみで notesCount / followersCount / followingCount を集計するため、リモートユーザーの数値が実体より小さく表示される。mk-go は user.Host が non-local の場合、 origin instance の `/api/users/show` を https POST で叩いて公開 counts を取得し、上書き表示する。

- 1 時間 TTL の LRU cache (size cap 10000、`hashicorp/golang-lru/v2`、#945)
- SSRF guard: `safehttp.NewSSRFSafeTransport` 経由 (private IP / metadata service block)
- host validation: URL injection (`/`, `?`, `#`, `@`, ` ` 混入) を url.Parse で reject
- 失敗時は silent fallback で local 観測値を維持
- フォロー一覧 / フォロワー一覧 endpoint は **自インスタンス上に存在する関係のみ** (= 数値だけ remote、一覧は local の非対称設計)

### mediaproxy のアニメ pass-through (#941)

`?emoji` / `?avatar` / `?preview` mode で gif / apng / vnd.mozilla.apng を **decode → resize しない** で raw bytes pass-through する。Go std の `image.Decode` は 1 frame しか返さないため resize 経路に乗せると静止画化されるのを回避。`?static` / `?badge` mode は明示的な静止画要求なので従来通り decode。

### URL preview の charset 自動正規化 (#942)

`internal/core/urlpreview/fetcher.go` で `golang.org/x/net/html/charset.NewReader` 経由で response の Content-Type charset と HTML `<meta charset>` を自動判定して UTF-8 に正規化。Shift_JIS / EUC-JP / ISO-2022-JP の日本語ページで title / description が文字化けしない。

### inbox processor の verify-in-worker 化 (#565)

upstream は HTTP handler の中で AP signature verify を同期実行するため、悪意ある unsigned activity が手前で増えると HTTP 受信スループットが低下する。mk-go は HTTP handler は body + signature header だけを payload に詰めて 202 即返し、signature verify / host block / instance touch / chart hook を inbox worker (queue processor) 側で実行することで HTTP 受信 rps が **TS の 2.6-2.8x** (queue-bench で確認)。

### mkq driver (#571 audit / #2985)

job queue driver の既定を asynq から mkq (BullMQ-compatible Go ライブラリ) に変更 (#571)。当時 queue-bench (`tests/queue-bench/`) で BullMQ / asynq / mkq を 3-way 比較した結果、**送信 rps は mkq が最良**だった (drain time は asynq のほうが速い計測もある。[queue-bench.md](queue-bench.md) の表を参照)。

その後 **#2985 で asynq driver 自体を削除**したので、選択肢は mkq だけになっている。`jobQueueDriver: asynq` を明示した設定は起動エラーになる。

## エラーレスポンス

#130でエラーレスポンスを標準化:
- 同一エラーコードに対するUUIDを統一
- ヘルパー関数を統合

## 設定ファイル互換性

TS版の`.config/default.yml`をそのまま使用可能。以下の設定もGo版で対応済み:
- 基本接続 (DB, Redis, URL, port)
- trustProxy (#129)
- dbSlaves (#133)
- 各種Redis分離設定
- jobQueueDriver (`mkq` のみ。#571 で既定に、#2985 で唯一の driver に)
- allowedPrivateNetworks (SSRF allowlist、開発時の self-loop 許可用)

## DB構造

### テーブル

Go側のマイグレーション (000001〜) はTS版テーブルに対して原則追加のみだが、例外が 16 件ある ([migration-from-ts.md](migration-from-ts.md#破壊的なマイグレーション))。TS版のマイグレーションで作成される全テーブルは維持される。

**mk-go 固有のテーブル (upstream に対応するものが無い) は 18 件:**

> [divergence.md](divergence.md) §2-1 は同じものを **21** と数えている。差は 3 件で、
> あちらは `note_unread` (upstream DB には legacy として残るが 2026.7.0 の `models/` に
> entity が無く参照 0 件。mk-go はこれを実用している) と bookkeeping 2 件
> (`migrations` / `schema_migrations`) を加える。CI の
> `TestDivergenceDoc_TableCountMatchesSchema` がその定義で固定している。矛盾ではなく
> 母集団の取り方の違い。

| テーブル | 用途 | migration |
|---|---|---|
| `antenna_note_unread` / `channel_note_unread` | アンテナ / チャンネルの未読管理 | `000037` |
| `user_keypair_extra` / `user_publickey_extra` | Ed25519 鍵の保持 (FEP-521a Multikey) | `000049` / `000050` |
| `chunked_upload_session` | 分割アップロードのセッション (#2313) | `000069` |
| `relay_observed_user` | リレー経由で観測したユーザー | `000071` |
| `instance_secret` | インスタンス単位の秘密値 | `000072` |
| `instance_signature_capability` | 相手インスタンスの署名方式 capability | `000073` |
| `signup_application` | 登録申請 | `000075` |
| `emoji_application` | カスタム絵文字の登録申請 (#2934) | `000086` |
| `user_suspension_origin` | 凍結の由来 (local / remote、#2973) | `000087` |
| `emoji_application_quota_reset` | 絵文字申請枠の手動リセットの記録 (#2962) | `000089` |
| `ip_lookup_log` | IP 照会そのものの監査記録 (#3106) | `000096` |
| `instance_gone_suspension` | goneSuspended になった時刻 (#3067) | `000099` |
| `federation_rule` | 連合のルール (#3090) | `000100` |
| `note_quote_authorization` | 引用の承認 (FEP-044f、#3234) | `000101` |
| `note_quote_request` | 引用の承認を求めた記録 (FEP-044f、#3234) | `000102` |
| `bubble_game_versus_record` | バブルゲームの対戦の記録 (#3232) | `000105` |

mk-go の migration が作るテーブルは 121。上記 18 件と golang-migrate 台帳の
`schema_migrations` を除く **102 はすべて upstream にも存在する** (TypeORM 台帳の
`migrations` を含む)。

mk-go は schema を自前の migration で 0 から作るので、**「mk-go の migration が
作る = mk-go が足した」ではない**。`password_reset_request` / `signin` /
`channel_favorite` / `channel_muting` / `clip_favorite` / `user_list_favorite` /
`retention_aggregation` / `system_account` / `used_username` /
`note_thread_muting` はいずれも upstream のテーブルで、drop-in の可否には影響
しない。


drop-in テスト (#367) で発見した補完カラム:
- `note.pageCount` (= `note_id` 配下の page 数キャッシュ) を `migration/000039_dropin_compat.up.sql` で追加
- `note.renoteChannelId` を同 migration で追加

### 既知のカラム差分

| テーブル | 状況 |
|---|---|
| `user_profile` | followedMessage, lang, publicReactionsは#131で追加済み |
| `note` | appId (App連携識別)、score (ノートスコア) はGo版では未使用 |
| `abuse_user_report` | resolvedAsのサイズ差 (Go: varchar(16), TS: varchar(128)) |

## 既知の制限

- **Identicon の外見** — TS版と生成アルゴリズムが異なるため、アイコン未設定ユーザーの表示が異なる
- **chat/* は upstream + 拡張** — upstream の 25 endpoint は**すべて同じパスで実装済み**。
  これに加えて yojo-art/cherrypick (federated chat) 由来の 15 endpoint
  (`chat/messages/create` / `chat/rooms/joined` / `chat/rooms/members/ban` 等) を
  additive に足している。upstream のクライアントから見て欠けているものは無い
- **search backend** — `fulltextSearch.provider` で挙動切替: 既定の `sqlLike` (= `lower(text) LIKE` による部分一致。**ILIKE ではない** — pg_bigm の GIN index `gin (lower(text) gin_bigm_ops)` は LIKE しか加速せず、ILIKE だと拡張を入れても index が効かないため。Meilisearch 不要、軽量 deploy 向け) / `meilisearch` (要 host 設定) / `sqlPgroonga` (要 PGroonga 拡張) / `none` (= upstream TS strict-mode 互換、400 UNAVAILABLE で reject、#877)
- **promo は作成・既読化できるが表示されない (#2781)** — `admin/promo/create` と
  `promo/read` は upstream と同じパス・同じ error id で実装済みで (#2784)、
  DB 行も正しく増える。しかし
  **`promo_note` を読んで利用者へ提示する経路が upstream にも無い**
  (2026.7.0 の backend で `promoNote` / `promoRead` を参照するのは endpoint 2 本と
  DI / model 定義だけ。`grep -rlni` で 8 ファイル、内訳は
  [`upstream-catch-up.md`](upstream-catch-up.md) の「submodule bump 後に必須」)。
  **frontend の menu 項目も無い** — `_promote()` 関数
  (`packages/frontend/src/utility/get-note-menu.ts:279`) と `promote` locale
  (`locales/*.yml:559`) は残っているが、どちらも参照ゼロ。menu 項目は upstream
  #14554 (2024-09) でコメントアウトごと削除済み。到達するのは API を
  直接叩く場合とサードパーティクライアントだけで、そこでは 204 が返るので
  **成功したように見えて誰にも表示されない**。
  mk-go はこの状態を忠実に再現している (endpoint を消すと 444/444 の
  カバレッジが崩れ、drop-in 切替でも挙動が変わるため)。表示経路を足すなら
  upstream に無い additive 拡張になる。
  ただし入力側だけは mk-go に意図的乖離がある — `admin/promo/create` は public
  以外の note を reject する (`docs/divergence.md` §7)
- **upstream 2026.9.1 まで追従済** — `#947` (2026.3.2 → 2026.5.1) / `#1164` (2026.5.1 → 2026.5.4、LD-Signature 初期実装 + 2026.5.4 hardening 含む) を経て 2026.6.0 → 2026.7.0 → 2026.9.0 → 2026.9.1 まで完了。各 release 差分は [`docs/update/`](update/) を参照 (`<yyyymm><nn>diff.md`。`nn` は**対象 upstream release の patch 番号**で日付ではない。backend に変更が無い release は doc を作らないので番号は飛ぶ。同じディレクトリに `<yyyymmdd>-<issue>-triage.md` 形式の triage note も同居する)

詳細は[TS版からの移行ガイド](migration-from-ts.md)の「既知の制限」セクションも参照。

## 関連 issue / tracker

- #107 — 第1次互換性調査 (API/DB/挙動の3軸)
- #124 — 第2次互換性調査 (6軸: +WebSocket, エラー, 設定)
- #744 — Playwright e2e Phase 1-4 tracker (54.3% endpoint coverage 達成)
- #947 — Misskey TS 2026.3.2 → 2026.5.1 への upstream 追従 tracker (closed)
- #1164 — Misskey TS 2026.5.1 → 2026.5.4 への upstream 追従 tracker (LD-Signature 初期実装含む、1 PR 集約)
- #949 — ドキュメント更新親 issue (本 doc を含む)
