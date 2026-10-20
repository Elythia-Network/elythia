# アーキテクチャ

Elythia は Misskey (TypeScript/NestJS) から生まれ、バックエンドを Go で再構築した ActivityPub サーバーです。
本ドキュメントは各レイヤ・パッケージが**どのような責務を持ち、Misskey-TS のどこに対応するか**を
詳述します。upstream の参照実装は `make upstream-fetch` が取得する `.cache/misskey/<版>/packages/backend/`（版は `UPSTREAM_MISSKEY_VERSION`、#3378）。

## 0. 設計思想

- **wire 互換が最優先**: REST API 応答 shape・エラーコード・ActivityPub の wire format を upstream と一致させる。
- **Go らしい再構成**: TS のパターンをそのまま移植せず、明示的 interface・エラー値・構造体埋め込みで書き直す。
- **互換を機械的に守る**: golden/shapetest・drift detector・値レベル diff harness・drop-in e2e で乖離を CI 検出する（§8）。
- **1.0 以降の方針**: 完全互換 backend から「互換を保ちつつ frontend (Misskey TS の fork。#3379 で本体の `frontend/` へ取り込んだ) も独自進化させる Misskey ファミリー fork」へ。API 拡張は additive-only、ActivityPub は硬く互換維持、REST は自フロント主導でケースバイケース。

---

## 1. レイヤ構成

```
┌─────────────────────────────────────────────┐
│  api (HTTP ハンドラ)            Echo v4       │  ← server/api/endpoints/* + server/*Service
├─────────────────────────────────────────────┤
│  core (ビジネスロジック)        ドメインサービス │  ← core/*Service.ts
├─────────────────────────────────────────────┤
│  repository (データアクセス)    GORM interface  │  ← TypeORM repository (TS は明示層なし)
├─────────────────────────────────────────────┤
│  model (DB エンティティ)        GORM 構造体     │  ← models/ (TypeORM entity)
└─────────────────────────────────────────────┘

補助レイヤ:
  entity       … model → JSON レスポンス変換       ← core/entities/*EntityService
  activitypub  … AP の型/署名/レンダラ/解決         ← core/activitypub/*
  queue        … 非同期ジョブ (mkq)                 ← queue/processors/*
  stream       … WebSocket チャンネル               ← server/api/stream/channels/*
```

**依存方向**: `api → core → repository → model` の一方向のみ。逆向き依存は禁止。`entity` は変換専用でドメインロジックを持たない。

### Elythia と Misskey-TS の構造的相違（重要）

| 観点 | Misskey-TS | Elythia | 理由 |
|---|---|---|---|
| DI | NestJS (`@Injectable`, `di-symbols.ts`) | **手動配線** (`internal/server/router.go` の `setupRoutes()` と、そこから呼ぶ `wiring_*.go` / `routes_*.go` の関数) | フレームワーク非依存・起動経路の明示化 |
| データアクセス | サービスが TypeORM repository を直接注入 | **明示的な `repository` interface 層** | mock 注入による単体テスト容易性 |
| pack | `*EntityService.pack()` は DI されたサービス | **`entity.PackX()` は純関数** | 変換とドメインロジックの分離 |
| クロスカット | `GlobalEventService` (event emitter) + 直接注入 | **明示的 hook interface** (`SetFanoutHook` 等) | import cycle 回避（§6） |
| ジョブキュー | BullMQ | **mkq** (BullMQ wire 互換) | Redis ストリーム互換 + Go ネイティブ |
| HTTP 署名 | `@misskey-dev/node-http-message-signatures` | **自前実装** (`internal/activitypub/signature.go`) | 依存削減・RSA/Ed25519 両対応 |

---

## 2. Misskey-TS との対応関係（全体マップ）

| Elythia | Misskey-TS | 備考 |
|---|---|---|
| `internal/api/*` | `server/api/endpoints/*` | endpoint 群（admin, notes, users, i, drive, …） |
| `internal/api/{inbox,wellknown,nodeinfo,proxy,streaming}` | `server/{ActivityPub,WellKnown,Nodeinfo,FileServer,Streaming}ServerService` | endpoints でなくサーバ層 |
| `internal/core/*` | `core/*Service.ts` | ドメインサービス（§3.2 に対応表） |
| `internal/entity/*` | `core/entities/*EntityService` | pack 関数 ↔ EntityService.pack |
| `internal/repository/*` | （TypeORM repository を各サービスが直接利用） | Elythia のみ明示層 |
| `internal/model/*` | `models/` (TypeORM entity) | テーブル 1:1 |
| `internal/activitypub/*` + `internal/core/federation/*` | `core/activitypub/*` | 型/署名/レンダラ/解決/inbox |
| `internal/queue/processors/*` | `queue/processors/*ProcessorService` | 非同期ジョブ |
| `internal/stream/channels/*` | `server/api/stream/channels/*` | WS チャンネル |
| `internal/misc/id` | `core/IdService` | ID 生成 (aidx 既定) |
| `internal/safehttp` | `core/HttpRequestService`(の SSRF guard) | private-network 拒否 transport |
| `internal/config` + `internal/cli` | `config.ts` + `boot/` | 設定・起動 |
| `internal/server/router.go` (+ `wiring_*.go` / `routes_*.go`) | NestJS `MainModule`/`ServerModule` の配線 | DI 相当を手動で。/api のルートの登録は `routes_<領域>.go` (§4.2) |

---

## 3. レイヤ詳細

### 3.1 `internal/api/` — HTTP ハンドラ（60 パッケージ / 553 ルート）

upstream `server/api/endpoints/` の endpoint 群を、ディレクトリ単位で実装。**upstream の 444 endpoint をすべて実装済み** (coverage 100.0%)。一次情報は `make apicompat` が生成する [api-compat.md](api-compat.md)。

ルート数は `internal/server` が `/api/*` に静的登録する数 (`api.POST` 514 + `api.GET` 14 +
`api.Match(chartMethods, …)` 12 × 2 methods + catchall の `api.Any` 1 = 553。test mode でだけ
登録する `/api/reset-db` を含む)。登録は、catchall と `/api/reset-db` が `router.go` の
`setupRoutes()`、ほかは領域ごとの `routes_<領域>.go` にある (#3697、4.2)。実行時は
これに同梱プラグインのルートが乗る。`docs/api-compat.md` の 444 + 108 = 552 に catchall を
足した数と一致する。パッケージ数は、`git ls-files internal/api` のうちテストでない `.go` を持つ
ディレクトリの数 (`apierr` のような handler でないものも含む)。

前に書いていた「52 パッケージ / 503 ルート」(`api.POST` 466 + `api.GET` 12 + 12 × 2 + 1、
api-compat.md の 444 + 58) は、ルート数の段落を最後に直した 2026-08-20 (`7d430d318`) の時点では
正しかった (その版の `router.go` と api-compat.md で数え直して一致)。数え方は同じで、その後に
足したルートとパッケージが反映されていなかった。#3697 で数え直した (ルートは `internal/server` の
非テストの `.go` の構文木で、`api` への `POST` / `GET` / `Match` / `Any` の呼び出しを数えた)。

`/api` の外は `s.echo` へ直接登録していて、`setupRoutes()` の本文の登録の式文は 64
(`GET` 50 / `POST` 5 / `OPTIONS` 2 / `Any` 1 / `File` 4 / `Static` 2。4.2 の「/api のルートの
登録」の前後の数の表と同じ数え方で、`if` の中も数える)。`Any` は 1 呼び出しで全メソッドを登録するので、
ルート単位に展開すると増える。ほかに `/debug/pprof` 配下が 8 (`pprofGroup` への登録)。
`setupRoutes()` から呼ぶ関数の中の登録 (`registerFrontendAssets` の `/vite/*` と `/embed_vite/*`、
`wireMetricsEndpoint` の `/metrics`、`serveStaticAssetDir` の `/twemoji/*` と `/fluent-emoji/*`) は、
この数に入れていない。

**`if` の中にあり、設定次第では登録されないもの**は、64 のうち 7 (`Static` 2・`File` 4・CSP report
の `POST`) と、pprof の 8。上の関数の中の登録のうち、`serveStaticAssetDir` の 2 つも
`os.Stat` の `if` の中で呼ぶので、置き場所が無ければ登録されない。

**数え方で値が変わるので、引用するときは何を数えたかも書くこと。**

**endpoint 群**（`server/api/endpoints/<同名>` に対応）:

| パッケージ | 対応エンドポイント / 機能 |
|---|---|
| `admin` | `admin/*` — accounts, roles, emoji, relays, queue (pause/resume), announcements, ad, moderation |
| `notes` | `notes/*` — CRUD, timeline (home/local/global/hybrid), reactions, polls, translate, thread-muting |
| `users` | `users/*` — show(bulk可), followers/following, search, reactions, relation, lists |
| `i` | `i/*` — 自アカウント update / notifications / webhooks / registry / 2fa / move |
| `drive` | `drive/*` — files (upload/list/find), folders |
| `following` `blocking` `mute` `renotemute` | フォロー/ブロック/ミュート/リノートミュート |
| `antennas` `clips` `channels` `userlists` | アンテナ / クリップ / チャンネル / リスト |
| `notifications` | `notifications/*` — list, grouped, mark-all-read |
| `roles` | `roles/*` — role 一覧/notes、policy |
| `gallery` `pages` `flash` | ギャラリー / ページ / Play (AiScript) |
| `emojis` `hashtags` `federation` | カスタム絵文字 / ハッシュタグ / 連合情報 |
| `auth` `oauth` `signin` `signup` `resetpassword` | MiAuth / OAuth2 / ログイン (TOTP/passkey/captcha) / 登録 |
| `app` `webhooks` `invite` `meta` | アプリ / Webhook / 招待 / meta (通報は `users` と `admin` に分かれて入る) |
| `reversi` `chat` `bubblegame` | リバーシ / チャット / バブルゲーム（§9 参照） |
| `ap` | `ap/show`, `ap/get` — リモート AP オブジェクト解決 |
| `charts` | `charts/*` — 12 chart engine（別登録、`buildChartBundle`） |
| `sw` | `sw/*` — Service Worker 登録 |

**サーバ層**（`server/*ServerService` に対応、endpoint ではない）:

| パッケージ | 対応 | 機能 |
|---|---|---|
| `inbox` | `ActivityPubServerService` (inbox) | SharedInbox + 個別 inbox。HTTP handler は 202 即返し、verify は worker (§7) |
| `wellknown` | `WellKnownServerService` | `.well-known/webfinger` / `nodeinfo` / `host-meta` |
| `nodeinfo` | `NodeinfoServerService` | NodeInfo 2.0/2.1 |
| `proxy` | `FileServerService` | メディアプロキシ (`/proxy/*`)、リモート画像キャッシュ |
| `streaming` | `StreamingApiServerService` | WebSocket 接続のアップグレード（§実体は `internal/stream`） |

**Elythia 固有のヘルパー**（endpoint ではない内部パッケージ）:

| パッケージ | 役割 |
|---|---|
| `apierr` | Misskey 互換のエラーコード/UUID 定義 |
| `pagination` | sinceId/untilId/sinceDate/untilDate の正規化 |
| `paramschema` | 本家の`paramDef`(引数のJSON Schema)で、本家のajvと同じ判定・同じ最初のエラー・同じdefaultの挿入をする検証器(#3673)。handlerの判定にはまだ使っていない。本番では`internal/server/paramshadow`が検証だけを走らせて、handlerと判定が分かれた件数を数える(#3677) |
| `optional` | JSON の null/absent を区別する Nullable 型 |
| `notehide` | per-viewer の hideNote ゲートを全 REST に一律適用（TS は各 pack 内 `hideNote`） |
| `userrelation` | viewer 視点の relation block (isFollowing 等) 解決 |
| `middleware` (**`internal/server/middleware`**。`internal/api/` 配下ではない) | 認証 / rate limit / WWW-Authenticate / Cache-Control |
| `paramshadow` (**`internal/server/paramshadow`**) | 本家の`paramDef`での検証を、応答を変えずにhandlerの前で走らせ、判定が分かれた件数をログに出す(#3677、[param-shadow.md](param-shadow.md)) |

### 3.2 `internal/core/` — ビジネスロジック（≈50 パッケージ）

`core/*Service.ts` に対応。主要対応表:

| Elythia core | Misskey-TS core | 責務 |
|---|---|---|
| `note` | NoteCreate/NoteDelete/NoteDraft/NotePining Service | ノート作成・削除・下書き・ピン |
| `user` | UserService / AccountUpdate / UserSuspend / DeleteAccount | ユーザー CRUD・凍結・削除 |
| `signup` | SignupService | 登録（禁止ワード検証等） |
| `following` | UserFollowingService | フォロー/承認 (autoAcceptFollowed, carefulBot) |
| `blocking` `muting` | UserBlocking / UserMuting / UserRenoteMuting Service | ブロック/ミュート/リノートミュート |
| `reaction` | ReactionService / ReactionsBufferingService | リアクション付与・削除・buffer flush |
| `timeline` | FanoutTimeline / FanoutTimelineEndpoint Service | Redis fanout 配信 + 取得 |
| `note` の filter / `notesfilter` | QueryService (+ frontend の checkNoteWordMute) | 可視性 / mute / block / word-mute の SQL filter |
| `wordmute` | frontend の utility/check-word-mute.ts と use-note.ts の checkNoteWordMute | ハードミュートの判定 (サーバーで掛ける。本家は frontend だけ)・規則のパースと LRU キャッシュ |
| `federation` | core/activitypub/* | AP 受信処理・解決・配信（§3.6） |
| `relay` | RelayService | リレー購読・配信 |
| `instance` | FederatedInstance / FetchInstanceMetadata Service | リモートインスタンス情報 |
| `drive` | DriveService / InternalStorage / S3 / ImageProcessing | ファイル保存 (Local/S3)・画像変換 |
| `mediaproxy` | （FileServerService 内のプロキシ） | リモート画像キャッシュ、AVIF/HEIC decode |
| `urlpreview` | UrlPreviewService | OG/Twitter/oEmbed プレビュー（Shift_JIS 等正規化） |
| `notification` | NotificationService | 通知生成・stream/push 配信（2秒既読 guard） |
| `webpush` | PushNotificationService | Web Push (VAPID) |
| `webhook` | UserWebhook / SystemWebhook / WebhookTest Service | Webhook 発火 |
| `chart` | core/chart/* | 12 chart engine（notes/users/drive/federation/instance/…） |
| `search` | SearchService | 全文検索 (Meilisearch / SQL fallback) |
| `twofactor` | UserAuthService / WebAuthnService | TOTP / WebAuthn(passkey) |
| `email` `captcha` | EmailService / CaptchaService | メール送信 / CAPTCHA 検証 |
| `channel` | ChannelFollowing / ChannelMuting Service (+ CRUD) | チャンネル |
| `clip` `antenna` `page` `flash` | Clip / Antenna / Page / Flash Service | クリップ/アンテナ/ページ/Play |
| `announcement` `abuse` `achievement` | Announcement / AbuseReport(+Notification) / Achievement Service | お知らせ/通報/実績 |
| `avatardecoration` `featured` `hashtag` | AvatarDecoration / Featured / Hashtag Service | アバター装飾/注目/ハッシュタグ |
| `emojiimport` | CustomEmojiService (import) | 絵文字インポート |
| `role` | RoleService | ロール・ポリシー解決 |
| `moderationlog` `moderatoractivity` | ModerationLog Service / CheckModeratorsActivity | モデレーションログ/モデレータ活動 |
| `move` | AccountMoveService | アカウント移行 (Move/alsoKnownAs) |
| `transfer` | Export*/Import* Processor | データ export/import |
| `poll` | PollService | 投票 |
| `chat` `reversi` | ChatService / ReversiService | §9（vanilla + 連合拡張） |
| `systemaccount` | SystemAccountService | instance.actor 等のシステムアカウント |
| `serverstats` `retention` | （server-stats daemon）/ AggregateRetention | サーバ統計 / 継続率 |
| `cache` `event` | CacheService / GlobalEventService | キャッシュ / イベント発火 |

### 3.3 `internal/entity/` — レスポンス DTO

`core/entities/*EntityService.pack()` に対応。ただし Elythia は**純関数 `PackX()`**（DI なし・ドメインロジックなし）。

| Elythia | Misskey-TS |
|---|---|
| `PackNote` / `PackNotes` (note.go) | NoteEntityService |
| `PackUserLite` / `PackUserDetailed` (user系) | UserEntityService |
| `PackNotification` | NotificationEntityService |
| `PackDriveFile` / `PackDriveFolder` | DriveFile/DriveFolder EntityService |
| `note_reaction` 系 | NoteReactionEntityService |
| `role.go` / `clip.go` / `announcement.go` / `page.go` / … | Role/Clip/Announcement/Page … EntityService |
| `note_field_resolver.go` | （pack 内の field 解決を batch 化した Elythia 集約） |
| `emoji_resolver.go` / `instance_resolver.go` / `mediaurl.go` | 絵文字/instance/メディア URL の解決ヘルパー |

### 3.4 `internal/repository/` — データアクセス（79 ファイル）

エンティティ毎に `interface + GORM 実装 + コンストラクタ`。**Misskey-TS には明示層が無く**、各サービスが TypeORM repository を直接注入する。Elythia は mock 注入で単体テストを成立させるためにこの層を設けている。

```go
type NoteRepository interface {
    FindByID(id string) (*model.Note, error)
    Create(note *model.Note) error
    ListGlobalTimeline(limit int, sinceID, untilID string, f model.TimelineDBFilter) ([]*model.Note, error)
    ListPublicNotes(f model.PublicNotesFilter, limit int, sinceID, untilID string) ([]*model.Note, error) // upstream notes.ts
    // ...
}
func NewNoteRepository(db *gorm.DB) NoteRepository { return &noteRepository{db: db} }
```

可視性の SQL push-down（`applyViewerVisibility`）は upstream `QueryService.generateVisibilityQuery` に対応。

### 3.5 `internal/model/` — DB モデル（66 ファイル）

Misskey-TS の `models/`（TypeORM entity）とテーブル 1:1 対応の GORM 構造体。列名・型・default を upstream に合わせる。enum・jsonb の扱いも互換。

### 3.6 `internal/activitypub/` + `internal/core/federation/` — ActivityPub

upstream `core/activitypub/*` に対応。型/署名/レンダラは `activitypub` パッケージ、受信処理・解決・配信ロジックは `core/federation` に分かれる。

| Elythia | Misskey-TS | 責務 |
|---|---|---|
| `activitypub/types.go` | `activitypub/type.ts` | ActivityStreams 型定義 |
| `activitypub/renderer.go` | `ApRendererService` | Go model → AP JSON-LD (Person/Note/Activity) |
| `activitypub/signature.go` | `ApRequestService`(+署名 lib) | HTTP Signatures (RSA-SHA256 / Ed25519) |
| `activitypub/client.go` | `ApRequestService` | 署名付き fetch（SSRF-safe transport） |
| `activitypub/jsonld.go` + `ld/` | `JsonLdService` | `@context` 構築・URDNA2015 正規化・LD-Signature |
| `activitypub/keypair.go` | `UserKeypairService` | RSA/Ed25519 鍵生成・解析 |
| `activitypub/multikey.go` | （FEP-521a Multikey, Ed25519 公開鍵公開） | assertionMethod |
| `activitypub/public_key_cache.go` | `ApDbResolverService`(key cache) | 公開鍵キャッシュ |
| `activitypub/webfinger.go` | `WebfingerService` | WebFinger 解決 |
| `activitypub/inbox_admission.go` | （ApInboxService の入口 gate） | forbidden directive 検査・freeze |
| `activitypub/mfm/` | `ApMfmService` / `MfmService` | MFM ↔ HTML |
| `core/federation/processor.go` | `ApInboxService` | inbox activity の処理 (Follow/Undo/Like/Announce/…) |
| `core/federation/resolver.go` | `ApResolver/ApDbResolver/RemoteUserResolve Service` | リモートオブジェクト解決・キャッシュ |
| `core/federation` の deliver | `ApDeliverManagerService` + queue/deliver | 配信（fan-out, retry） |
| `core/federation` の deriveVisibility | `ApAudienceService` | to/cc → visibility 判定 |

### 3.7 `internal/queue/` — ジョブキュー

driver は `mkq`（BullMQ wire 互換）のみ。legacy の `asynq` は #2985 で削除し、`jobQueueDriver: asynq` は起動エラーになる。`processors/` 配下:

| Elythia processor | Misskey-TS processor | 内容 |
|---|---|---|
| `deliver` | DeliverProcessorService | AP 配信（retry/backoff） |
| `inbox` | InboxProcessorService | AP 受信（verify-in-worker） |
| `follow` `unfollow` `block` `unblock` | RelationshipProcessorService | 関係操作ジョブ |
| `webhook` | User/System WebhookDeliverProcessorService | Webhook 配送 |
| `webpush` | （PushNotificationService 経由） | Web Push |
| `emoji_import` | ImportCustomEmojisProcessorService | 絵文字 import |
| `transfer` | Export*/Import* ProcessorService | データ export/import |
| `reaction_flush` | BakeBufferedReactionsProcessorService | リアクション buffer → DB |
| `clean_remote_notes` | CleanRemoteNotesProcessorService | リモートノート GC |
| `check_expired_mutings` | CheckExpiredMutingsProcessorService | 期限切れミュート解除 |
| `check_moderators_activity` | CheckModeratorsActivityProcessorService | モデレータ活動監視 |
| `retention` | AggregateRetentionProcessorService | 継続率集計 |
| `chart` | Tick/Resync/Clean ChartsProcessorService | チャート集計 |
| `delete_account` | DeleteAccountProcessorService | アカウント削除 |
| `post_scheduled_note` (+ `scheduled_note_lock`) | PostScheduledNoteProcessorService | 予約投稿 |
| `instance_refresh` | FetchInstanceMetadata 系 | instance メタ更新 |
| `clean` | Clean/CleanCharts ProcessorService | 定期クリーンアップ |

組み込みの job 種別と、それを受ける processor のメソッドの対応は、
`internal/server/wiring_queue.go` の `builtinQueueHandlers()` の一覧にだけある (#3690、4.2)。
プラグインの job 種別は、プラグインの配線 (`plugin_wiring.go`・`plugin_notifications.go`) が登録する。

### 3.8 `internal/stream/` — WebSocket

`server/api/stream/channels/*` に対応。`channels/` 配下:

| Elythia | Misskey-TS |
|---|---|
| timeline (home/local/hybrid/global) | home-/local-/hybrid-/global-timeline |
| main / notifications | main |
| chat_room / chat_user | chat-room / chat-user |
| reversi_game | reversi-game / reversi |
| drive | drive |
| role-timeline / hashtag / antenna / user-list | role-timeline / hashtag / antenna / user-list |

dispatcher が shareable channel の共有・pong ack を upstream `Connection.ts` 互換で扱う。

組み込みのチャンネル名と、それを作る factory と credentialed (匿名の接続に許すか) の対応は、
`internal/server/wiring_stream.go` の `builtinStreamChannels()` の一覧にだけある (#3692、4.2)。

### 3.9 その他

| Elythia | Misskey-TS | 役割 |
|---|---|---|
| `internal/misc/id` | `core/IdService` | ID 生成 (aidx 既定 / aid/meid/ulid/objectid) |
| `internal/safehttp` | `core/HttpRequestService` の private-network guard | SSRF-safe transport（urlpreview/mediaproxy/federation で共用） |
| `internal/misc/notesummary` | （push 本文生成） | 通知/Web Push の本文要約 |
| `internal/config` + `internal/cli` | `config.ts` + `boot/` | Viper 設定解決・起動 |
| `internal/entitycompat` | （golden 突合の自前基盤） | autogen 型に対する shape 検証（§8） |

---

## 4. DI / 配線

すべてのサービス生成・配線は `internal/server/router.go` の `setupRoutes()` で手動実行（NestJS 相当を関数呼び出しで構築）。DI フレームワークは使わない。repository は `setupRoutes()` から呼ぶ `buildRepos()` (`internal/server/wiring_repos.go`) で、core の service の一部は `buildCore()` (`internal/server/wiring_core.go`) で、連合の部品の一部は `buildFederation()` (`internal/server/wiring_fed.go`) で、queue の processor の一部は `buildQueue()` (`internal/server/wiring_queue.go`) で、stream の部品の一部は `buildStream()` (`internal/server/wiring_stream.go`) で、API の handler の一部は `buildHandlers()` (`internal/server/wiring_handlers.go`) で作る。`/api` のルートは、領域ごとのファイル (`internal/server/routes_<領域>.go`) の `register…Routes()` が登録し、`setupRoutes()` はそれを 1 回ずつ呼ぶ (4.2)。停止が要る部品は、名前を付けて一覧 `lifecycle` に登録し、世代を止めるときに登録した順に止める。構築では登録するだけで、起動を持つ部品は`StartBackground`が一覧の順に起動する(4.3、#3703)。

```
server.New() → setupRoutes()
  ├→ stream の pubsub と、stream の節より前の受け手へ渡す publisher の生成 (購読はまだ始めない)
  ├→ Repository 生成 (buildRepos → wiring.repos。setupRoutes では userRepo, noteRepo, … の別名を置く)
  ├→ chart の hook と、作るだけで副作用の無い部品の生成
  ├→ Core サービス生成 (作るだけのものは buildCore → wiring.core で作り、別名を置く。
  │   setter はそれぞれ元の位置で呼ぶ。副作用や後の領域への依存があるものはその場で作る)
  ├→ 連合の部品の生成 (作るだけのものは buildFederation → wiring.fed で作り、別名を置く。
  │   setter と、outbound client に依存するもの (apClient、resolver など) は元の位置)
  ├→ queue の processor の生成 (作るだけのものは buildQueue → wiring.queue で作り、別名を置く。
  │   setter と、outbound client や setupRoutes で作る部品に依存するものは元の位置)
  ├→ hook 注入 (§5)
  ├→ Handler 生成 (§3.1)。作るだけの handler は buildHandlers → wiring.handlers
  │   で作り、usersHandler の位置で別名を置く。setter は元の位置。この途中
  │   (プラグインの準備より前) で、queue の handler を registerQueueHandlers で 1 回だけ
  │   登録する (一覧は builtinQueueHandlers)
  ├→ stream (bus・registry・manager と、ここから配る publisher は buildStream → wiring.stream
  │   で作り、別名を置く。pubsub の購読をここで始める。WebSocket のチャンネルは
  │   registerStreamChannels で 1 回だけ登録する。一覧は builtinStreamChannels) /
  │   WebSocket / middleware
  ├→ /api のルートの登録。handler をすべて作った後、プラグインの準備より前で、
  │   routes_<領域>.go の register…Routes を 1 回ずつ呼ぶ (24 個)。/api の外のルート
  │   (ActivityPub・SSR・well-known など) は s.echo へ元の位置で登録する
  └→ プラグインの準備 (失敗したらここで return) / /api の catchall / 静的ファイル /
      SPA の catchall

Server.StartBackground() (Supervisor が構築の後に呼ぶ。4.3)
  ├→ 一覧の部品のうち起動を持つものを、一覧の順に起動する (#3703)
  ├→ queue の worker・autoscale・scheduler (queue role のとき)
  └→ chart の保存の loop
```

### 4.1 作る順と、後から入れる依存 (#3676)

**依存は受け手より先に作り、受け手を作った直後に渡す。** setter で後から入れるのは、
循環しているものと、作るときに副作用があって今の位置から動かせないものに限る。
`setupRoutes()` を領域ごとの関数に分けるとき (#3666 の段階 2)、受け手より後で作る
依存は関数をまたぐ配線になるため。

**動かしてよいのは、作るだけで DB にも Redis にも外部にも触らず、goroutine も起動せず、
shutdown hook も登録しないものだけ。** shutdown hook は登録順 (FIFO) で止まり、
Redis への命令や DB の読み書きの順も起動の手順の一部なので、それらを伴う文は動かさない。
例外は chart の hook と一緒に前へ移した meta のフラグの写し (`metaRepo.Fetch()`、
cachedMeta の最初の読み出しなので DB を読みうる) で、元の位置との間に DB の文が無いので、
PostgreSQL の文の順は変わらない。
pubsub の購読 (`Subscribe`) も、今の位置 (stream の節) から動かさない。早めると、
handler ができる前に届いた event が、まだ配線していない service を触る。

**構築の途中で動く処理から見える配線も変えない。** #3705の後、`setupRoutes()`が終わる前から
動いて配線を辿るのは、pubsubの購読のhandlerだけになった(ほかに`urlPreviewFetcher`のLRUの
期限切れのgoroutineが立つが、配線は辿らない)。そこから辿れるsetterを、その起動より前へ移すと、
構築中の動きが変わる。一覧の部品(workerの4つ、`fedRuleHits`、バッファ3つ、`deliveryHealth`/
`inboxHealth`、`emojiInvalidationQueue`、statsのpublisher)は、構築では起動せず、
`StartBackground`(構築の後)で起動する(4.3。`emojiInvalidationQueue`は#3705、ほかは#3703)。

#3703の前は、それらも構築の途中で起動していた。#3676で前へ移したsetterのうち、それらから
辿れたのは`notificationService.SetMainStreamPublisher`の1本だけで、ほかの受け手(note・
following・reaction・drive・連合・queueのprocessor・handlerなど)には届かなかった。このsetterを
前へ移すと、`pollExpiryWorker`が構築中に作った通知が、packer(`SetPacker`、workerの起動より後)を
通らない形で`unreadNotification`として流れた(#3676のレビューで実測)。なので、このsetterは
依存を先に作っても、元の位置(streamingのpublisherの隣)に残した。#3703で`pollExpiryWorker`の
起動が構築の後になったので、この流れは起きなくなったが、setterの位置は変えていない。

#3676 で、stream の pubsub と publisher (`streamPubSub` / `internalPubSub` /
`noteEventPub` / `relationReloadPublisher` / `mainStreamPublisher` / `drivePublisher` /
`reversiPublisher`) を `setupRoutes()` の先頭へ、chart の engine と hook
(`chartCharts` / `chartHooks`) を repository の直後へ移し、受け手には作った直後に渡す
ようにした (上の `notificationService.SetMainStreamPublisher` だけは元の位置)。ほかに、受け手より後で作っていた純粋な部品 (repository 16 個、
`featuredService`、`hashtagService`、`modLogService`、`instanceService` など) を
受け手より先に作るようにした (こちらは setter を呼ぶ位置は変えていない)。

| | #3676 の前 (`b9db7e49b`) | #3676 の後 | #3684 の後 | #3686 の後 | #3688 の後 | #3690 の後 | #3692 の後 | #3694 の後 | #3697 の後 | #3699 の後 | 今(#3705の後) |
|---|---|---|---|---|---|---|---|---|---|---|---|
| setter で依存を入れる辺 | 877 | 877 | 879 | 879 | 872 | 872 | 872 | 872 | 872 | 872 | 872 |
| うち、受け手より後で作った依存を入れるもの | 192 (受け手 55 個) | 107 (受け手 41 個) | 104 (受け手 41 個) | 103 (受け手 40 個) | 96 (受け手 40 個) | 96 (受け手 40 個) | 96 (受け手 40 個) | 96 (受け手 40 個) | 96 (受け手 40 個) | 96 (受け手 40 個) | 96(受け手40個) |
| うち、循環しているもの | 40 | 40 | 40 | 39 | 35 | 35 | 34 | 34 | 34 | 34 | 34 |
| うち、それ以外 | 152 | 67 | 64 | 64 | 61 | 61 | 62 | 62 | 62 | 62 | 62 |
| setter の呼び出しが受け手から離れているもの (下の数え方) | 359 | 307 | 308 | 392 | 415 | 426 | 444 | 660 | 660 | 660 | 659 |

#3684 の後の列は、#3684 の後の `router.go` を同じ道具で数えた。#3676 の後に入った変更で、setter の
辺と呼び出しが離れている辺が 2 つずつ増えている (#3684 の直前の `692710223` で
879 / 107 / 309)。#3684 で、後から入れる辺は 3 つ減った。repository の別名を
`setupRoutes()` の先頭に置いたことによる (下の「まだ手を付けていない」の項)。呼び出しが
離れている辺は 1 つ減った。`noteCreateService.SetChannelHook(...)` の受け手・依存と
呼び出しの間で定義していた `noteUnreadRepo` を、先頭の別名へ移したため (配線は同じ)。

#3686 で core の service 46 個を `buildCore()` へ移し、その別名を `setupRoutes()` の
`featuredService` を作っていた位置に置いた (4.2)。setter を呼ぶ位置は変えていない。
後から入れる辺は 1 つ減った。`apFetcher.SetSigner(sysAcctSvc)` の依存
`sysAcctSvc` が、受け手より前で定義されるようになったため。循環が 1 つ減り、それ以外が
同じなのは、`noteQueryService.SetEphemeralReader(ephemeralStore)` の分類が循環から
それ以外へ変わったため。これは道具の数え方による。道具は名前で辿り、`ephemeralStore` の
右辺のクロージャの中の `err` を、`setupRoutes()` の中のすべての `err` と同じ節点として
辿っていた。別名にしたので、右辺は `coreSvcs.ephemeralStore` だけになった。呼び出しが
離れている辺は 84 増えた。受け手の定義が前へ移り、setter の呼び出しは元の位置に
残したので、間に定義が挟まるようになったため。これは、setter を元の位置に残すという
方針の結果である。段階 3 以降で setter を領域の関数へ寄せるときの目安になる。

#3688 で連合の部品 13 個を `buildFederation()` へ移し、その別名を `setupRoutes()` の
`inboxSoftwareChecker` を作っていた位置に置いた (4.2)。setter を呼ぶ位置は変えていない。
setter で依存を入れる辺は 7 つ減った。同じ種類を 2〜3 回作っていたものを 1 つにまとめ、
setter の引数が構築の式から変数 1 つに変わったため (`iHandler` と `adminHandler` の
`NewUserModerationDeliveryHook(deliverService, apRenderer, userRepo)` で 2 つずつ、
`inboxProcessor` と `apHandler` の `NewInboxSuspendedChecker(metaRepo, instanceRepo)` で
1 つずつ、`blockingService.SetQuoteRevoker` の中の `NewQuoteRequestDeliveryHook` で 1 つ)。
後から入れる辺も 7 つ減った。6 つは、依存の定義が受け手より前へ移ったため
(`iHandler.SetProfileUpdateHook`・`iHandler.SetPinDeliveryHook`・
`federationProcessor.SetInboundFollowAcceptor`・`federationProcessor.SetRelayActorChecker`・
`federationProcessor.SetRelayMarker`・`deliverProcessor.SetDeliveryBreaker`)。残る 1 つは
`blockingService.SetQuoteRevoker` で、後から入れる依存が `deliverService` と `apRenderer` の
2 つから `quoteRequestDeliveryHook` の 1 つになった。循環が 4 つ減り、それ以外が 3 つ
減ったのは、上の分に加えて `followingService.SetFederationHook(followingDeliveryHook)` の
分類が循環からそれ以外へ変わったため。#3686 の `ephemeralStore` と同じく道具の数え方に
よる (別名 `followingDeliveryHook := fedSvcs.followingDeliveryHook` の依存は `fedSvcs`
だけ)。`relaySvc` も `buildFederation()` へ移したので、`noteDeliveryHook` などの
`SetRelayBroadcaster(relaySvc)` は後から入れる辺にならない。呼び出しが離れている辺は
23 増えた。#3686 と同じく、受け手と依存の定義が前へ移り、setter の呼び出しは元の位置に
残したため。

#3690 で queue の processor 15 個を `buildQueue()` へ移し、その別名を `setupRoutes()` の
`importProcessor` を作っていた位置に置いた (4.2)。setter を呼ぶ位置は変えていない。
setter の辺、後から入れる辺、循環、それ以外は、どれも数が変わらない。後から入れる 96 辺は、
行番号を除けば受け手・依存・setter が前と同じ (道具の一覧を行番号を消して突き合わせた)。
移した processor の setter の依存 (`relationReloadPublisher`・`pageRepo`・`webPushCache`・
`notificationService` など) が、どれも `buildQueue()` を呼ぶ位置より前で定義されているため。
呼び出しが離れている辺は 11 増えた。受け手の定義が前へ移り、setter の呼び出しは元の位置に
残したため (`cleanProcessor.SetCursorStore` の 2 辺、`checkExpiredMutingsProcessor` の 1 辺、
`cleanGenericProcessor` の 2 辺、`deleteAccountProcessor` の 3 辺、`postScheduledNoteProcessor`
の 3 辺)。handler の登録 (`s.queueServer.Handle`) は setter ではないので、どの行にも数えない。

#3692 で stream の部品 7 個 (registry、manager、revoke publisher、note・notification・
broadcast・chat の publisher。bus も作るが manager に渡すだけ) を `buildStream()` へ移し、
その別名を `setupRoutes()` の `streamBus` を作っていた位置 (stream の節の先頭) に置いた (4.2)。
setter を呼ぶ位置は変えていない。setter の辺と後から入れる辺は数が変わらず、後から入れる
96 辺は行番号を除けば前と同じ。循環が 1 つ減り、それ以外が 1 つ増えたのは、
`iHandler.SetStreamRevoker(streamRevokePublisher)` の分類が循環からそれ以外へ変わったため。
#3686 の `ephemeralStore` と同じく道具の数え方による (別名
`streamRevokePublisher := streamSvcs.streamRevokePublisher` の依存は `streamSvcs` だけで、
右辺の `streamManager` を辿らなくなった)。呼び出しが離れている辺は 18 増えた。受け手か依存の
定義が前へ移り、setter の呼び出しは元の位置に残したため (`notePublisher` の 5 辺、
`notificationPublisher` の 11 辺、`iHandler.SetStreamRevoker` と `oauthHandler.SetStreamRevoker`
の 1 辺ずつ)。2 つの handler の setter の引数でそれぞれ作っていた
`NewUserListStreamPublisher` を先頭の 1 つにまとめたが、setter の辺の数は変わらない (依存が
`streamPubSub` から `userListStreamPublisher` に変わっただけ)。チャンネルの登録
(`Register` / `RegisterCredentialed`) は setter ではないので、どの行にも数えない。

`buildStream()` を `setupRoutes()` の先頭で呼ぶ形も、同じ道具で比べた。先頭で呼ぶと、
manager と note・notification の publisher (どれも setter の受け手) まで先頭へ移り、
後から入れる辺は 96 から 117 に増える (27 辺増えて 6 辺減る。増えるのは
`streamManager` の setter の 12 辺、`notePublisher` の 5 辺、`notificationPublisher` の 10 辺)。
なので、#3676 で先頭に置いた 4 つの publisher は先頭に残し、`buildStream()` は stream の節で
呼んだ (4.2)。

#3694 で API の handler 30 個を `buildHandlers()` へ移し、その別名を `setupRoutes()` の
`usersHandler` を作っていた位置に置いた (4.2)。setter を呼ぶ位置は変えていない。setter の辺、
後から入れる辺、循環、それ以外は、どれも数が変わらない。後から入れる 96 辺は、行番号を除けば
受け手・依存・setter・分類が前と同じ (道具の一覧を行番号を消して突き合わせた)。移す handler を、
`usersHandler` の位置へ前倒ししても後から入れる辺が増えないものに限ったため (4.2)。呼び出しが
離れている辺は 216 増えた。受け手の定義が前へ移り、setter の呼び出しは元の位置に残したため
(受け手が移した handler の辺が 213。多いのは `usersHandler` の 29、`iHandler` の 26、
`apHandler` の 20。依存が移した handler の辺が 3。`sharedUserPacker.SetDetailExtras` と
`SetDetailExtrasMany` の `usersHandler`、`followingService.SetMeUpdatedPublisher` の
`iHandler`)。ルートの登録は setter ではないので、どの行にも数えない。

#3697 で `/api` のルートの登録 539 本を `routes_<領域>.go` の 24 個の関数へ移した (4.2)。
setter も構築も動かしていないので、どの行も数が変わらない。後から入れる 96 辺と、呼び出しが
離れている 660 辺は、どちらも行番号を除けば前と同じ (道具の一覧を行番号を消して突き合わせた)。
節点は 364 から 361 に減った。ルートの行だけが使っていた `chartMethods`・`chatRead`・
`chatWrite` の定義を登録の関数へ移したためで、どれも setter の引数に現れないので辺は変わらない。

#3699 で停止の hook を名前付きの一覧の登録に置き換えた (4.3)。登録の位置も順も変えていないので、
どの行も数が変わらない。後から入れる 96 辺と、呼び出しが離れている 660 辺は、どちらも行番号を除けば
前と同じ。節点も 361 のまま。

#3701 で、`setupRoutes()` の中の、値から作れる 8 つの部品 (起動を伴う 7 つと `metadataRefresher`) の
起動と登録を、値を 1 つ渡す登録 (`s.startPartWithClose(ctx, name, part)` など) に置き換えた (4.3。
もう 1 つのプラグインの store は `setupRoutes()` の外の `dbBackedStorage` で登録する)。起動の位置も
登録の順も変えていないので、どの行も数が変わらない。後から入れる
96 辺と、呼び出しが離れている 660 辺は、どちらも行番号を除けば前と同じ。節点も 361 のまま。

#3703で、構築の途中で起動していた12個の部品の起動を`StartBackground`へ移した(4.3)。`setupRoutes()`
の中で変えたのは`fedRuleHits`の1か所(`fedRuleHits.Start(hitsCtx)`と`s.addPart(...)`の2行を、
`s.addPartWithStart(...)`の1行にした)だけで、setterも構築の順も動かしていない。後から入れる96辺と、
呼び出しが離れている660辺は、どちらも行番号を除けば前と同じ。節点も361のまま。

#3705で、`emojiInvalidationQueue`の`Run`の起動を`StartBackground`へ移した(4.3)。`setupRoutes()`の
中で変えたのは、`go`文と`s.addPart(...)`のclosure(局所変数`emojiInvalidationCtx`・
`stopEmojiInvalidation`・`emojiInvalidationDone`を使う)を、`s.startPartWithStop(...)`の1行にした
ことだけで、setterも構築の順も動かしていない。節点は消した3つの局所変数の分だけ減り、358になった。
後から入れる96辺は、行番号を除けば前と同じ。呼び出しが離れている辺は660から659になった。減ったのは
`emojiRepo.SetInvalidationHook(emojiInvalidationQueue.Enqueue)`の辺で、`emojiInvalidationQueue`を
作ってから呼ぶまでの間に定義していた節点が、消した3つだけだったため。

数え方 (`setupRoutes()` の構文木。道具はコミットしていない):

- 節点: `setupRoutes()` の中で `:=` か `var` で定義した名前 (関数リテラルの中と、
  `if` / `for` / `switch` の初期化文の一時変数を除く) と、`s.<フィールド>`。定義の位置は
  最初の定義の行 (`s.<フィールド>` は `setupRoutes()` の中で最初に代入した行、無ければ 0)
- 辺: `X.SetYyy(...)` の呼び出しで、受け手 `X` が節点で、引数 (関数リテラルの中を含む)
  に `X` 以外の節点 `B` が現れるもの。1 回の呼び出しに 2 つの節点が現れれば 2 辺
- 後から入れる辺: `B` の定義の行が `X` の定義の行より後のもの
- 循環: 依存の図 (`A := f(... B ...)` の右辺と、setter の引数から張る `A → B`) で、
  `B` から `X` へ辿り着けるもの。図は `setupRoutes()` の中だけで張り、`buildRepos()`・
  `buildCore()`・`buildFederation()`・`buildQueue()`・`buildStream()`・`buildHandlers()` の中の
  構築の依存は辿らない (別名 `x := coreSvcs.y` の依存は `coreSvcs` だけ)
- 「後から入れる辺」は**定義の順だけを見ていて、setter を呼ぶ位置は見ていない**。依存の
  定義だけを前へ移した辺 (setter の呼び出しは元の位置) も、元の位置に残した
  `notificationService.SetMainStreamPublisher` も、後から入れる辺には数えない。呼ぶ位置は
  別に、「`X` と `B` の両方ができた後、別の節点を 1 つでも定義してから setter を呼んでいる
  辺」として数えた (表の最後の行)。段階 2 で関数に分けるときに節をまたぐ配線の目安になる
- setter を通らない注入は数えていない。フィールドへの直接の代入で、受け手より後の
  依存を入れているのは `chartHooks.InstanceCounter = instanceCounterBuffer` の 1 つ
  (goroutine と shutdown hook を持つバッファを元の位置に残したため)

#3676 で残した 107 辺 (内訳の数は #3676 の時点の値):

- **循環している 40 辺。** 通知 (`notificationHook` / `notificationPublisher`)、フォロー・
  ブロック・ミュート、連合の配送 (`apRenderer` / `quoteOutbox` / `noteDeliveryHook`)、
  アカウントの移行 (`accountMover`)、ephemeral (`ephemeralMaterializer`)、
  `sharedUserPacker` ⇄ `usersHandler`、`followingService` ⇄ `iHandler` など。core が
  api の handler に依存している箇所 (`sharedUserPacker.SetDetailExtras(usersHandler)`、
  `followingService.SetMeUpdatedPublisher(iHandler)`) が循環を api まで広げている。
  解くには interface の切り出しが要り、作る順の入れ替えでは消えない
- **作るときに DB や外部に触るか、その結果から作るもの 7 辺。** `fedRuleService` (作るときに
  DB からルールを読む)、`searchService` (Meilisearch を使う設定では、作るときに index の
  設定を書き込む)、`sensCfg` / `nsfwDetector` (meta を DB から読んだ分岐の中で作る)
- **pubsub の publish と購読を対で置いているもの 2 辺。** `roleService` と `cachedMeta`
  の invalidation hook (`rolesUpdatedSender`、`nodeinfoHandler`)。hook と `Subscribe`
  を同じ場所に置いて、片方だけ消える抜けを防いでいる
- **まだ手を付けていない 58 辺。** 作る順を入れ替えれば消せるが、#3676 では動かさな
  かったもの。依存が別の service・hook・連合の部品で、それ自身も多くの部品に依存する
  もの (`timelineFanoutHook`、`notificationHook`、`deliverService`、
  `ephemeralMaterializer`、`chatService` など) が多いが、作るだけで副作用の無いもの
  (`apURLs`、`imgProcessor`、`deliveryHealth` など) や、shutdown hook を登録する部品・
  queue role の分岐・作れなかったときは配線しない分岐と同じ節に置いているもの
  (`metadataFetcher`、`deliveryBreaker`、`webauthnSvc` など)、プラグインの節でまとめて
  扱う値 (`registeredPlugins` / `peered`) も含む。段階 2 で領域ごとの関数に分けるときに
  扱う。今は 55 辺。#3684 で repository の別名を `setupRoutes()` の先頭に置いたので、
  repository を依存とする 3 辺 (`quoteAuthorizationRepo`・`sigCapRepo`・
  `userSecurityKeyRepo`) が消えた (4.2)

### 4.2 組み立てのコンテナと、領域ごとの関数 (#3684)

**`setupRoutes()` で作る部品は、領域ごとの関数で作り、コンテナの構造体 `wiring`
(`internal/server/wiring.go`) の領域ごとのフィールドに入れる。** #3666 の段階 2。
#3684 で repository を、#3686 で core の service の一部を、#3688 で連合の部品の一部を、
#3690 で queue の processor の一部を、#3692 で stream の部品の一部を、#3694 で API の handler の
一部を移した。部品の領域のフィールドは `repos`・`core`・`fed`・`queue`・`stream`・`handlers` の 6 つ。
#3699 で、停止が要る部品の一覧を指す `lifecycle` を足した (4.3。部品は作らず、`Server` が持つ一覧を指すだけ。
今は誰も読まない。段階 (6) で、作り直す範囲を絞るときに使う)。
#3697 で `/api` のルートの登録を領域ごとのファイルの関数へ分けた (部品は作らないので、
フィールドは増やしていない。下の「/api のルートの登録」)。

```
setupRoutes()
  w := &wiring{repos: s.buildRepos(), lifecycle: &s.lifecycle}
  // ↑ repos は buildRepos (wiring_repos.go) が作る。lifecycle は Server の一覧を指すだけ (4.3)
  noteRepo := w.repos.note              // 別名。以降の行はこの名前を使う
  …
```

- **`setupRoutes()` では、先頭で今までと同じ名前の別名を置き、以降の行は書き換えない。**
  wiring-check など、`setupRoutes()` の行を引数まで含めて照合するゲートが多いため
  (`docs/gates.md`)。別名の型は `repos` のフィールドの型から引けるので、
  `collectRoutes` の型の解決も変わらない
- **`buildRepos()` は `*Server` のメソッドにした。** 中で今までと同じ行
  (`userRelationRepo := repository.NewUserRelationRepository(s.db)` など) で作り、
  最後に `repos` を組む。ゲートが照合する `s.db` を含む行をそのまま残すためで、使うのは
  `s.db` / `s.relationCache` / `s.userRepo` だけ
- **1 つの種類は 1 つのフィールドにし、使う側すべてで共有する。** 状態を持たない
  repository (db のハンドルと relationCache だけを持つもの) は、共有しても使う側ごとに
  作るのと動きが同じ。キャッシュ付きのもの (`cachedMeta`・`emoji`・`cachedAntenna`、
  `s.userRepo` の `user`) は状態を持ち、invalidation の hook と他のプロセスからの購読が
  そのインスタンスに繋がっているので、別の値に置き換えない
- **例外として、同じ種類をその場で作っている所が 3 か所ある。** `NewCleanProcessor` の
  `repository.NewUserPendingRepository(s.db)` (#3690 から `buildQueue()` の中)、`SetIPLookupLogPruner` の
  `repository.NewIPLookupLogRepository(s.db)`、`coreabuse.NewInAppNotifier` の
  `repository.NewAbuseReportRepository(s.db)`。ゲート
  (`TestCleanProcessorReceivesThePendingPruner`・`TestIPLookupLogRetentionIsWired`・
  `TestAbuseReportNotifierIsWired`) が、nil を渡せないよう構築の式そのものを照合して
  いるため。どれも状態を持たないので、2 つあっても動きは同じ
- **照合する行が `build…` の関数の中へ移ったゲートは、`setupRoutes()` がその値を受け取る
  別名の行と、コンテナのフィールドへ入れる行も照合する。** 構築の式でも setter の呼び出し
  でも同じ。照合した行だけを見ると、`setupRoutes()` の別名を nil や別の式に書き換えても
  緑のまま通る。`TestRelationSingleStatementReadersAreWired` は、構築の行に加えて
  `userRelation: userRelationRepo`、`repos: s.buildRepos()`、
  `userRelationRepo := w.repos.userRelation` も照合する (#3684 のレビューで、別名を nil に
  する変異が通ったため)。後の領域 (core 以降は setter の照合が大半) を移すときも同じ。
  守られているかを機械で見る検査は無い
- **コンテナへは、別名がコンパイル時に順序を守れる形で入れ、照合もその形で行う。**
  別名を、コンテナへ値を入れるより前に書けてしまう形だと、別名はゼロ値 (nil) を黙って
  受け取り、文字列の照合はすべて残ったまま緑になる (#3686 のレビューで実測)。今の形は
  2 つある
  - `repos` はリテラルのキー (`w := &wiring{repos: s.buildRepos(), …}`)。`w` を定義する文で
    入れるので、それより前に `w.repos.x` を書くとコンパイルが通らない。照合はキー
    `repos: s.buildRepos()` で行い、リテラル全体は見ない (後でフィールドを足しても
    書き直さずに済む)
  - `core` は変数を経由した代入 (`coreSvcs := s.buildCore(w, idGen)`、
    `w.core = coreSvcs`、別名は `x := coreSvcs.x`)。`buildCore()` が `w.repos` を読むので
    リテラルには足せない。`w.core = …` から直接別名を取ると、別名を代入より前に書いても
    コンパイルが通るので、別名は変数から取る。照合はこの 3 つの行で行う
  - `fed` も `core` と同じ形 (`fedSvcs := s.buildFederation(w, coreSvcs, idGen)`、
    `w.fed = fedSvcs`、別名は `x := fedSvcs.x`)
  - `queue` も同じ形 (`queueSvcs := s.buildQueue(w, coreSvcs, idGen)`、
    `w.queue = queueSvcs`、別名は `x := queueSvcs.x`)
  - `stream` も同じ形 (`streamSvcs := buildStream(streamPubSub, idGen)`、
    `w.stream = streamSvcs`、別名は `x := streamSvcs.x`)。`buildStream()` は `Server` の
    フィールドを読まないので、メソッドではなく普通の関数にした。stream の pubsub は
    `setupRoutes()` の先頭で作った変数 `streamPubSub` を引数で受ける
  - `handlers` も同じ形 (`hSvcs := s.buildHandlers(w, coreSvcs, fedSvcs, idGen)`、
    `w.handlers = hSvcs`、別名は `x := hSvcs.x`)
  - **後の領域の `build…()` も、先に作った領域を `w.core.x` のようにコンテナから読まず、
    `coreSvcs` のような変数を引数で受ける。** コンテナのフィールドから読むと、その
    `build…()` を代入より前に置いてもコンパイルが通り、ゼロ値を受け取る (別名と同じ穴)。
    今 `w.core`・`w.fed`・`w.queue`・`w.stream`・`w.handlers` を読む箇所は無い (#3686 の
    レビュー 2 周目。`buildFederation()` と `buildQueue()` は `coreSvcs` を、`buildHandlers()` は
    `coreSvcs` と `fedSvcs` を引数で受ける。`w.handlers` については、
    `TestSetupRoutesAliasesEveryBuiltHandler` がパッケージ全体で `<x>.handlers.<y>` の読み出しを
    落とす)
- **関数に移してよいのは、作るだけで副作用の無いものだけ (4.1 と同じ)。** 作るときに
  DB に触る文 (`EnsureInitial`、`NormalizePrivateKeysToPKCS8`、`RecomputeFollowCounts`)
  は `setupRoutes()` に残し、互いの順も変えていない。repository のコンストラクタは
  どれも構造体を作るだけなので、`buildRepos()` がこれらより先に走っても DB の文の順は
  変わらない

| | #3684 の前 (`692710223`) | 後 |
|---|---|---|
| `repository.New…(` の呼び出し | 122 | 102 |
| 呼び出している種類 | 99 | 99 |
| 2 回以上呼んでいる種類 | 14 | 3 (上の例外) |
| setter で後から入れる辺 (4.1 の数え方) | 107 | 104 |

数え方: `setupRoutes()` (#3684 の後は `buildRepos()` も合わせる) の本文に現れる
`repository\.New[A-Za-z]+\(` を数えた。`NewCachedMetaRepositoryWithTTL(repository.NewMetaRepository(s.db), …)`
の内側の呼び出しも 1 回に数え、repository でない `NewEmojiInvalidationQueue` なども
含む。panic の文字列の中の `repository.NewNoteRepository` は `(` が続かないので数えない。

#### core の service (#3686)

```
setupRoutes()
  …                                     // repository の別名、EnsureInitial、chart の hook
  coreSvcs := s.buildCore(w, idGen)     // wiring_core.go
  w.core = coreSvcs
  roleService := coreSvcs.role          // 別名 (変数から取る)
  …
  roleService.SetUserRepo(userRepo)     // setter は元の位置のまま
```

- **`buildCore()` は `*Server` のメソッドで、repository は `w` から受け取る。** 中の行は
  `setupRoutes()` にあった行をそのまま移し、作る順も元のまま。repository を
  `setupRoutes()` と同じ名前の別名で受け直すのは、ゲートが構築の式を引数まで含めて照合する
  ため。使う `Server` のフィールドは `s.redis` / `s.config` / `s.queueClient` / `s.db`
- **移すのは、作るだけのものに限る。** 作るときに DB にも Redis にも外部にも触らず、
  ログを出さず、goroutine・timer・shutdown hook・グローバルの書き換え・hook の登録の
  どれもせず、依存が repository と先に作った core だけのもの。コンストラクタの本体
  (中で呼ぶ関数を含む) を 1 つずつ読んで確かめた
- **setter は 1 つも動かさない。** 構築の途中で動く処理 (4.1) から見える配線を変えない
  ため。`buildCore()` を呼ぶ位置は、移したものの中で最初に作っていた `featuredService` の
  位置。移した構築は副作用を持たないので、DB と Redis の文の順は変わらない
- **`wiring` へは変数を経由した代入で入れ、別名は変数から取る** (上の規則)。ゲートは
  この形を照合する (`TestAbuseReportNotifierIsWired`)
- **フィールド名は、今までのローカル変数の名前から `Service` / `Svc` を落としたもの。**
  型はコンストラクタが返す具体的な型のまま (`collectRoutes` の型の解決と、interface を
  満たすかどうかを変えないため)

照合する行が `buildCore()` の中にだけあるゲートは、`TestAbuseReportNotifierIsWired` の
`coreabuse.NewInAppNotifier(...)` の 1 本だった (確かめ方: `internal/entitycompat` /
`internal/server` / `tools` の `_test.go` の文字列リテラルを、`wiring_core.go` と
`internal/server` のほかのファイルに当てた)。ほかのゲートは setter の行を照合して
おり、setter は動かしていない。このゲートは、上の規則のとおり
`abuseInAppNotifier: abuseInAppNotifier`、`coreSvcs := s.buildCore(w, idGen)`、
`w.core = coreSvcs`、`abuseInAppNotifier := coreSvcs.abuseInAppNotifier` も照合する。

**`setupRoutes()` に残したもの** (`core*.New…` の 46 回の呼び出し、`corefederation` を除く):

| 理由 | 例 |
|---|---|
| setter の引数や、別の部品の構築の引数の中で作る (20 回) | `corechannel.NewNoteCreateHook(channelService)`、`coretwofactor.NewRedisPendingGuard(...)` など。setter の行そのものなので動かさない |
| `if` の中で作る (3 回) | `pollExpiryWorker` (queue role だけで登録する worker。#3703の前は構築の途中で動いていた)、sensitive media の detector (meta を読んだ分岐の中) |
| 作るときに副作用がある | `urlPreviewFetcher` (中の LRU cache が goroutine を起動する)、`proxyService` (transport を作るときに、設定によってはログを出す)、`exporter` (引数の `s.outboundClient()` が共有の transport を初めて作りうる)、`searchService` (4.1)、`reactionCountWriter` と `captchaSvc` (meta を読んだ値で作る)、`webauthnSvc` (作れなかったときの分岐がある) |
| `Start` と shutdown hook を対で持つ | `instanceTouchBuffer`・`sigCapBuffer`・`instanceCounterBuffer`、`metadataRefresher` |
| stream・連合・`setupRoutes()` で作る部品に依存する | `abuseCreatedNotifier`・`reversiService`・`bubbleVersusService`・`modActivitySvc` (stream)、`relaySvc`・`accountMover`・`ephemeralMaterializer`・`metadataFetcher` (連合)、`emojiImporter` (`emojiDecorationResolver`) |
| setter の依存が後の領域で、先に作ると後から入れる辺が増える | `chatService` (stream・連合・`sharedUserPacker`)、`pluginAccountSvc` (`adminHandler`)。2 つとも移すと、後から入れる辺が 104 から 107 に増えた |
| ゲートが同じファイルを要求する | `webPushCache` (`TestPushSubscriptionCacheIsShared` が、生成と配送・無効化への受け渡しを同じファイルに置くことを要求する) |

| | #3686 の前 (`a997a195c`) | 後 |
|---|---|---|
| `setupRoutes()` の `core*.New…` の呼び出し | 120 (うち `corefederation` 28) | 74 (うち `corefederation` 28) |
| `buildCore()` の `core*.New…` の呼び出し | — | 46 |
| `setupRoutes()` で `core*.New…` を呼ぶ package | 46 | 30 |
| `core` のフィールド (`buildCore()` で作る部品) | — | 46 |
| setter で後から入れる辺 (4.1 の数え方) | 104 | 103 |

数え方: `setupRoutes()` と `buildCore()` の本文に `grep -oE '\bcore[a-z]+\.New[A-Z]'` を
当てた。`coremodlog.New(` は `New` の後が大文字でないので数えない。`buildCore()` の
`coretransfer.NewFollowingServiceAdapter(...)` (`importer` の引数) は数える。なので
フィールドの数と呼び出しの数は、どちらも 46 だが中身は 1 つずつ違う。

`relaySvc` は #3688 で `buildFederation()` へ移した (下の連合の部品の節)。

#### 連合の部品 (#3688)

```
setupRoutes()
  …                                                // core の別名
  fedSvcs := s.buildFederation(w, coreSvcs, idGen) // wiring_fed.go
  w.fed = fedSvcs
  apRenderer := fedSvcs.apRenderer                 // 別名 (変数から取る)
  …
  apRenderer.SetIDGenerator(idGen)                 // setter は元の位置のまま
```

- **`buildFederation()` は `*Server` のメソッドで、repository は `w` から、core の
  service は `buildCore()` の戻り値 `coreSvcs` から受け取る。** 中の行は `setupRoutes()` に
  あった行をそのまま移し、作る順も元のまま。使う `Server` のフィールドは `s.config` /
  `s.queueClient` / `s.redis` / `s.db`。`coreSvcs` から使うのは `sysAcct` だけ (`relaySvc`
  の引数)
- **移す条件は core と同じ。** 作るときに DB にも Redis にも外部にも触らず、ログを出さず、
  goroutine・timer・shutdown hook・グローバルの書き換え・hook の登録のどれもせず、依存が
  repository・core・先に作った連合の部品・設定だけのもの。コンストラクタの本体を 1 つずつ
  読んで確かめた。移したのは 13 個: `inboxSoftwareChecker`、`apURLs`、`apRenderer`、
  `deliverService`、`relaySvc` (core/relay。配送に依存するので core から外していた)、
  配送の hook 7 個 (`noteDeliveryHook`、`followingDeliveryHook`、
  `quoteRequestDeliveryHook`、`noteDeleteHook`、`userModerationDeliveryHook`、
  `profileUpdateHook`、`pinDeliveryHook`)、`deliveryBreaker`
- **setter は 1 つも動かさない。** `federationProcessor` と `federationResolver` への
  setter を含め、呼ぶ位置も順も元のまま。`buildFederation()` を呼ぶ位置は、移したものの
  中で最初に作っていた `inboxSoftwareChecker` の位置 (core の別名より後の `dbHealth` の直後)。移した構築は
  副作用を持たないので、DB と Redis の文の順は変わらない
- **同じ種類を 2 回以上作っていたもののうち、状態を持たない 3 種類を 1 つにまとめた。**
  `NewInboxSuspendedChecker` (3 回。resolver と `remoteUserResolver` が共有していたもの、
  inbox の processor、ap/show)、`NewQuoteRequestDeliveryHook` (2 回。QuoteRequest の
  handler、`QuoteRevoker`)、`NewUserModerationDeliveryHook` (2 回。i/delete-account、
  admin)。どれもフィールドをコンストラクタでしか書かず、setter も無く、メソッドは
  フィールドを読むだけなので、1 つを共有しても使う側ごとに作るのと動きが同じ。setter の
  引数が構築の式から別名に変わったが、setter の位置は変えていない
- **`wiring` へは変数を経由した代入で入れ、別名は変数から取る** (上の規則)。別名を
  `buildFederation()` の呼び出しより前に書くと、`buildFederation()` を `buildCore()` より
  前に置くと、どちらもコンパイルが通らない (変異で確かめた)

照合する行が `buildFederation()` の中にだけあるゲートは無かった (確かめ方: core と同じく、
tracked な `_test.go` すべてと `tools/apicompat/main.go` の文字列リテラルのうち、#3688 の前の
`router.go` に現れ、後の `router.go` と `internal/server` のほかのファイル (`wiring_fed.go`
を除く) に現れないものを探した。0 件)。連合の部品を照合しているゲートは setter の行
(`apRenderer.SetIDGenerator(idGen)` など) を照合しており、setter は動かしていない。
なので、ゲートは変えていない。

**`setupRoutes()` に残したもの** (`activitypub` / `corefederation` / `fedrule` /
`deliveryhealth` の `New…` の呼び出し 25 回):

| 理由 | 例 |
|---|---|
| outbound の HTTP client に依存する | `apClient` (`apHTTPClient := s.outboundClient(...)` が共有の transport を初めて作りうる)、`webfingerClient`。それに依存する `apFetcher`・`federationResolver`・`federationProcessor`・`remoteUserResolver`、`federationResolver.FetchNoteForVerification` を受け取る `quoteRequestHandler` と、それに依存する `quoteOutbox` |
| 作るときに副作用がある | `remoteStatsFetcher` (作るときに transport を作る)、`fedRuleService` (作るときに DB からルールを読む、4.1) |
| `Start` と shutdown hook を対で持つ | `fedRuleHits`、`deliveryHealth`・`inboxHealth` (queue role の分岐で `Start`。`NewStore` / `NewStoreForDirection` を含む) |
| setter の引数や、別の部品の構築の引数の中で作る | `NewUserMentionResolver`・`NewPollDeliveryHook`・`NewQuoteRevoker`・`NewReactionDeliveryHook`・`NewBlockingDeliveryHook`・`NewSuspendedChecker`・`NewLDSignatureVerifier`・`NewAccountCreatedAtFiller`・`fedrule.NewManager`、`s.peerDeps` の中の `activitypub.NewPublicKeyCache` |

`NewSuspendedChecker` (配送の停止) は `NewInboxSuspendedChecker` と同じ型だが、読む設定が
違う (`deliverSuspendedSoftware`) ので別の値のまま。`setupRoutes()` で作る core の service
のうち連合に依存する `accountMover`・`ephemeralMaterializer`・`metadataFetcher` は、
`federationResolver` か `apFetcher` に依存するので残した。

| | #3688 の前 (`8db95c0e2`) | 後 |
|---|---|---|
| `setupRoutes()` の `corefederation.New…` の呼び出し | 28 | 15 |
| `setupRoutes()` の `activitypub.New…` の呼び出し | 5 | 3 |
| `setupRoutes()` の `fedrule.New…` / `deliveryhealth.New…` の呼び出し | 3 / 5 | 3 / 4 |
| `setupRoutes()` の `core*.New…` の呼び出し (`corefederation` を含む) | 74 | 60 |
| `buildFederation()` の `New…` の呼び出し | — | 14 (`corefederation` 9、`activitypub` 2、`deliveryhealth` 1、`corerelay` 1、`repository` 1) |
| `fed` のフィールド (`buildFederation()` で作る部品) | — | 13 |
| setter で後から入れる辺 (4.1 の数え方) | 103 | 96 |

数え方: `setupRoutes()` と `buildFederation()` の本文 (`func` の行から、行頭の `}` まで)
に `grep -oE '\b<package>\.New[A-Z]'` を当てた。`corefederation` の 28 から 15 への差の
13 は、移した 7 回 (`NewInboxSuspendedChecker`・`NewDeliverService`・
`NewNoteDeliveryHook`・`NewFollowingDeliveryHook`・`NewNoteDeleteDeliveryHook`・
`NewProfileUpdateDeliveryHook`・`NewPinDeliveryHook`) と、まとめて無くなった 6 回
(`NewInboxSuspendedChecker` 2、`NewQuoteRequestDeliveryHook` 2、
`NewUserModerationDeliveryHook` 2)。`buildFederation()` の `corefederation` の 9 は、
移した 7 種類に、まとめた `NewQuoteRequestDeliveryHook` と
`NewUserModerationDeliveryHook` を 1 回ずつ足したもの。

#### queue の processor (#3690)

```
setupRoutes()
  …                                               // 連合の別名
  exportProcessor := processors.NewExportProcessor(exporter)
  queueSvcs := s.buildQueue(w, coreSvcs, idGen)   // wiring_queue.go
  w.queue = queueSvcs
  cleanGenericProcessor := queueSvcs.cleanGenericProcessor  // 別名 (変数から取る)
  …
  cleanGenericProcessor.SetIPLookupLogPruner(…)   // setter は元の位置のまま
  …
  s.registerQueueHandlers(builtinQueueHandlers(queueHandlerProcs{  // handler の登録は 1 回
    clean: cleanGenericProcessor,
    …
  }))
```

- **`buildQueue()` は `*Server` のメソッドで、repository は `w` から、core の service は
  `buildCore()` の戻り値 `coreSvcs` から受け取る。** 中の行は `setupRoutes()` にあった行を
  そのまま移し、作る順も元のまま。使う `Server` のフィールドは `s.config` / `s.db`。
  連合の部品は使わないので、`fedSvcs` は受け取らない
- **移す条件は core・連合と同じ。** コンストラクタの本体を 1 つずつ読み、構造体を作るだけ
  なのを確かめた。移したのは 15 個: `importProcessor`、`cleanProcessor` (meta を job の
  たびに読む関数 `cleanCfgFn` ごと)、`orphanUserProcessor`、`orphanAttachmentProcessor`
  (cutoff 用の ID の generator ごと。作るときは乱数を読むだけ)、
  `checkExpiredMutingsProcessor`、`cleanGenericProcessor`、`chunkedUploadGCProcessor`、
  `objectStorageProcessor`、`deleteAccountProcessor`、`unfollowProcessor`・
  `followProcessor`・`blockProcessor`・`unblockProcessor`、`retentionProc`、
  `postScheduledNoteProcessor`。meta を読む関数は job のたびに呼ばれるもので、作るときには
  呼ばない
- **setter は 1 つも動かさない。** `buildQueue()` を呼ぶ位置は、移したものの中で最初に
  作っていた `importProcessor` の位置 (`exportProcessor` の直後)。移した構築は副作用を
  持たないので、DB と Redis の文の順は変わらない。起動時に 1 回だけ流す retention の集計
  (`startConstructionWorker`、queue role だけ) も、位置も条件も元のまま
- **`wiring` へは変数を経由した代入で入れ、別名は変数から取る** (上の規則)。
  `setupRoutes()` で使わなくなった別名 (`roleAssignmentRepo`・`importer`) は消した

**handler の登録は、job 種別と processor のメソッドの一覧 (`builtinQueueHandlers()`) に
まとめ、`setupRoutes()` で 1 回だけ登録する (`registerQueueHandlers()`)。** 前は
`s.queueServer.Handle(...)` が `setupRoutes()` の 30 か所に散らばっていた。

- **登録の場所を変えても動きは変わらない。** mkqdriver の `Server.Handle` は job 種別を
  キーに map へ入れるだけで、map を読むのは `Start` が写しを取るときだけ (`Start` の後の
  `Handle` は panic する)。`Start` は `New` の後に呼ぶ。30 個の job 種別は
  `internal/queue/tasks.go` の `TaskType…` の定数 30 個と 1 対 1 で、重複は無い。登録は前も
  今も役割 (web / queue) を問わず常に行う。メンテナンスで世代を作り直すときも、`New` の中で
  登録するのは同じ
- **登録の位置は、最後の登録 (moderator の活動) があった位置。** processor への setter は
  すべてこれより前にある。`setupRoutes()` はプラグインの準備に失敗すると途中で return
  するが、前の 30 か所はすべてその return より前にあり、今の 1 か所も前にある。
  プラグインの登録 (`plugin_wiring.go`・`plugin_notifications.go`。job 種別の接頭辞が別) は
  これより後のままで、変えていない
- **登録する processor は、setter を呼ぶのと同じ `setupRoutes()` の名前から渡す
  (`queueHandlerProcs`)。** 一覧の欄名は job 種別に合わせ (`clean: cleanGenericProcessor`
  など)、別名や `queueProcs` の欄名と重ならないようにした。ゲートが `name: name` の形で
  照合するときに、別の行を拾わないため
- `Handle` の引数の中で作っていた `NewResendQuoteRequestsProcessor` と
  `NewCheckModeratorsActivityProcessor` は、元の位置で変数に入れるようにした

照合する行が `buildQueue()` の中にだけ移ったゲートは、
`TestCleanProcessorReceivesThePendingPruner` の `NewCleanProcessor` の 1 本だった (確かめ方:
#3688 と同じく、tracked な `_test.go` すべてと `tools/apicompat/main.go` の文字列リテラルの
うち、#3690 の前の `router.go` に現れ、後の `router.go` と `internal/server` のほかのファイル
(`wiring_queue.go` を除く) に現れないものを探した。4 件で、ほかの 3 件は関係の無いテストの
`/api/retention`・`reader`・`role_assignment`)。このゲートは AST で `internal/server` の全体を
読むので、移しても通る。上の規則のとおり、`cleanGenericProcessor: cleanGenericProcessor`、
`queueSvcs := s.buildQueue(w, coreSvcs, idGen)`、`w.queue = queueSvcs`、
`cleanGenericProcessor := queueSvcs.cleanGenericProcessor` と、一覧へ渡す
`clean: cleanGenericProcessor` も照合する。ほかのゲートは setter の行を照合しており、setter は
動かしていない。一覧には `internal/server/wiring_queue_test.go` のテストを付けた (一覧が
`internal/queue` の `TaskType…` の定数 (パッケージの tracked な非テストのファイルすべてから
読み、値が文字列リテラルでなければ落とす) をすべて 1 回ずつ含むこと、job 種別ごとのメソッド、
登録が一覧のとおりであること、`setupRoutes()` が登録を本文の直下の文として 1 回だけ、
どの return よりも前で呼び (関数リテラル・`go` 文・`if` の中での呼び出しは落とす)、
関数リテラルの中を含めて `s.queueServer.Handle` を直接呼ばないこと、`queueHandlerProcs` の
欄の値がどれも `setupRoutes()` の本文で `:=` で 1 回だけ定義し、`=` で書き換えていない変数で、
欄の書き忘れが無いこと、パッケージの中で queue の server の `Handle` を呼ぶ関数が
`registerQueueHandlers()` とプラグインの登録の 3 つ (`setupPlugins()`・
`registerPluginNotifications()`・`(*pluginJobs).Handle`) に限られること。許す関数の一覧には、
使われていない項目を落とす検査も付けた)。`setupRoutes()` は `router.go` を名指しせず、
`git ls-files` で列挙したファイルから探す。

**`setupRoutes()` に残したもの** (`processors.New…` の 14 回の呼び出し):

| 理由 | 例 |
|---|---|
| outbound の HTTP client を使う | `webPushProcessor` (`webPushSender` の `s.outboundClient(...)`)、`webhookProcessor` |
| `setupRoutes()` で作る部品に依存する | `exportProcessor` (`exporter`)、`emojiImportProcessor` (`emojiImporter`)、`resendQuoteRequestsProcessor` (`quoteOutbox`)、`deliverProcessor` (`apClient`)、`inboxProcessor` (`federationProcessor`)、`flushProcessor` (`reactionCountWriter`)、`chartProcessor` (chart の engine `chartCharts`)、`instanceRefreshProc` (`metadataFetcher`)、`checkModeratorsActivityProcessor` (`modActivitySvc`。stream の publisher に依存する) |
| setter の引数の中で作る | `NewRepoSigningKeySource`、`NewRedisInboxReplayGuard`、`NewRedisScheduledNoteLock` |

| | #3690 の前 (`53475bb41`) | 後 |
|---|---|---|
| `setupRoutes()` の `processors.New…` の呼び出し | 29 | 14 |
| `buildQueue()` の `processors.New…` の呼び出し | — | 15 |
| `queue` のフィールド (`buildQueue()` で作る processor) | — | 15 |
| `setupRoutes()` の `s.queueServer.Handle(` | 30 | 0 |
| `builtinQueueHandlers()` の項目 (job 種別) | — | 30 (processor は 26 個) |
| `setupRoutes()` の行数 | 4,899 | 4,823 |
| setter で後から入れる辺 (4.1 の数え方) | 96 | 96 |

数え方: `setupRoutes()` と `buildQueue()` の本文 (`func` の行から、行頭の `}` まで) に
`grep -oE '\bprocessors\.New[A-Z]'` を当てた。`Handle` は同じ本文の
`grep -c 'queueServer\.Handle('`、一覧の項目は `builtinQueueHandlers()` の本文の
`grep -c '{queue\.TaskType'`、processor の数は `queueHandlerProcs` のフィールドの行数、
行数は同じ本文の `wc -l`。

#### stream の部品 (#3692)

```
setupRoutes()
  streamPubSub := event.NewPubSubService(...)        // 先頭 (#3676)
  noteEventPub := stream.NewNoteEventPublisher(...)  // 受け手へ早く渡す 5 つは先頭のまま
  …
  // Streaming の節
  streamSvcs := buildStream(streamPubSub, idGen)     // wiring_stream.go
  w.stream = streamSvcs
  streamManager := streamSvcs.streamManager          // 別名 (変数から取る)
  …
  s.streamManager = streamManager                    // 代入・setter・購読は元の位置のまま
  streamManager.SetNotificationReader(…)
  …
  chatRoomFactory := channels.NewChatRoomFactory(chatService)
  registerStreamChannels(streamRegistry, builtinStreamChannels(streamChannelDeps{  // 登録は 1 回
    chatRoom: chatRoomFactory,
    …
  }))
  streamingHandler := streaming.NewHandler(streamManager)  // /streaming はこの後
```

- **`buildStream()` は普通の関数で、stream の pubsub (`streamPubSub`) と ID の generator を
  引数で受ける。** `Server` のフィールドは読まない。中の行は `setupRoutes()` にあった行を
  そのまま移し、作る順も元のまま
- **移す条件は core・連合・queue と同じ。** コンストラクタの本体を 1 つずつ読み、構造体を
  作るだけなのを確かめた。Redis に繋ぐのは pubsub の最初の `Subscribe` で、作るときには
  繋がない。移したのは 8 つの構築: bus (`NewEventPubSubBus`。manager に渡すだけなので
  フィールドは持たない)、registry (`NewRegistry`。空の map を 2 つ作るだけ)、manager
  (`NewManager`。接続の map を作り、registry と bus の参照を持つだけで、goroutine も購読も
  始めない)、revoke publisher (`NewStreamRevokePublisher`。pubsub と manager の参照を
  持つだけ)、note・notification・broadcast・chat の publisher。フィールドは 7 つ
- **setter は 1 つも動かさない。** 購読 (`Subscribe…`。server role の分岐の中)、
  `OnStreamRevoke`、`s.streamManager` への代入も、位置も順も元のまま。`buildStream()` を
  呼ぶ位置は、移したものの中で最初に作っていた `streamBus` の位置 (stream の節の先頭)。
  移した構築は副作用を持たないので、DB と Redis の文の順は変わらない
- **#3676 で `setupRoutes()` の先頭に置いた 4 つの publisher (note event・drive・reversi・
  main stream) は、先頭に残した。** どれも stream の節より前にある受け手 (core の service・
  連合の processor・handler) へ渡す依存で、stream の節より前に要る。`buildStream()` を先頭で呼ぶ形も比べたが、manager と note・notification
  の publisher (setter の受け手) まで先頭へ移り、後から入れる辺が 96 から 117 に増えた
  (4.1)。stream の節で呼べば 96 のまま。`relationReloadPublisher` は `internal/server` の
  adapter で、これも先頭のまま
- **2 つの handler の setter の引数で別々に作っていた `NewUserListStreamPublisher` を、
  先頭の 1 つ (`userListStreamPublisher`) にまとめた。** フィールドは pubsub だけで、
  コンストラクタでしか書かず、setter も無い。メソッドはそれを読んで publish するだけなので、
  1 つを共有しても使う側ごとに作るのと動きが同じ。先頭に置いたのは、受け手の
  `usersHandler` が stream の節より前にあるため
- **`wiring` へは変数を経由した代入で入れ、別名は変数から取る** (上の規則)。別名を
  `buildStream()` の呼び出しより前に書くとコンパイルが通らない

**チャンネルの登録は、チャンネル名・factory・credentialed の一覧 (`builtinStreamChannels()`)
にまとめ、`setupRoutes()` で 1 回だけ登録する (`registerStreamChannels()`)。** 前は
`streamRegistry.Register(...)` / `RegisterCredentialed(...)` が `setupRoutes()` の 22 行に
散らばっていた。

- **登録の場所を変えても動きは変わらない。** `Registry.Register` / `RegisterCredentialed` は
  mutex を取って map に入れるだけで、map を引くのは dispatcher が接続を受けたとき
  (`Lookup` / `RequiresCredential`) だけ。manager は registry の参照を持つだけで、作るときにも
  購読の処理でも引かない。`internal/server` の中で registry を引く経路はほかに無く、
  プラグインはチャンネルを登録しない。21 個の名前はすべて別々で、上書きは起きない。
  registry は世代ごとに `buildStream()` で作るので、メンテナンスで世代を作り直すときも、
  その世代の `setupRoutes()` の中で登録するのは同じ
- **登録の位置は、最後の登録 (chatUser) があった位置。** /streaming の handler
  (`streaming.NewHandler`) を作るより前で、`setupRoutes()` の途中の return (プラグインの
  準備の失敗) より前でもある。サーバーが接続を受けるのは `setupRoutes()` が終わった後
- **factory が依存する service と publisher は、setter を呼ぶのと同じ `setupRoutes()` の名前から
  渡す (`streamChannelDeps`)。** reversi・バブルゲームの対戦・chat の service、serverStats・
  queueStats の publisher、antenna の repository、role の service。chatRoom には、起動時の
  検査 (`streamChannels.chatRoom.service`) が `HasService` を見るのと同じ `chatRoomFactory` を
  渡す。欄名は別名と重ならないようにした (`chat: chatService` など)
- **queueStats の分岐は一覧の中で表す。** `setupRoutes()` は、queueInspector があるときだけ
  publisher を作って `Start` する (server role の条件と shutdown hook はそのまま)。
  publisher は `var queueStatsPub *stream.QueueStatsPublisher` で宣言し、
  `if s.queueInspector != nil {` の中で 1 回だけ代入する。一覧は publisher が nil なら
  fallback の factory (`NewQueueStats`) を選ぶ。前の `else` の枝は登録だけだったので無くなった
  (#3676 の道具で並べた副作用のある文の列は、この `ELSE` の 1 行だけが減り、ほかは同じ)

照合する行が `buildStream()` の中にだけ移ったゲートは、`TestStreamRevokeIsWired` の
`stream.NewStreamRevokePublisher(streamPubSub, streamManager)` の 1 本だった (確かめ方: #3690 と
同じく、tracked な `_test.go` すべてと `tools/apicompat/main.go` の文字列リテラルのうち、
#3692 の前の `router.go` に現れ、後の `router.go` と `internal/server` のほかのファイル
(`wiring_stream.go` を除く) に現れないものを探した。登録を移す前で 1 件。登録を移した後に
増えた 11 件は、足したテスト (`wiring_stream_test.go`) の文字列と、`internal/stream/channels`
と `tests/e2e-federation` のテストのチャンネル名で、どれも `internal/server` の行を照合しない)。上の規則のとおり、
`streamRevokePublisher: streamRevokePublisher`、`streamManager: streamManager`、
`streamSvcs := buildStream(streamPubSub, idGen)`、`w.stream = streamSvcs`、
`streamRevokePublisher := streamSvcs.streamRevokePublisher`、
`streamManager := streamSvcs.streamManager` も照合する。
`TestDivergenceDoc_StreamChannelsMatchRegistry` は `streamRegistry.Register(...)` の正規表現で
チャンネル名を読んでいたので、`builtinStreamChannels()` の項目の名前を構文木から読む形に
直した (名前が文字列リテラルでない項目と、関数が 1 つでないときは落とす。一覧の外の直接の
登録も、今までの正規表現で拾う)。一覧には `internal/server/wiring_stream_test.go` のテストを
付けた (一覧がチャンネル名を 1 回ずつ含み、credentialed が前と同じであること、名前ごとの
factory の関数 (queueStats は publisher の有無の両方)、登録が一覧のとおりで印も付くこと、
chatRoom に起動時の検査と同じ factory を登録すること (一覧が `d.chatRoom.New` を使うことと、
`setupRoutes()` が `chatRoom` の欄に検査と同じ変数を渡すことの両方)、実際に作ったサーバーの
manager が引く registry (`stream.Manager.Registry()`。このテストのために足した) に一覧の
チャンネルがすべて同じ関数の factory と credentialed で載っていること (queueInspector の有無の両方。
factory は関数名で比べ、どのインスタンスに束ねたかは上の構文木のテストが見る。
registry は `buildStream()` で作り、登録は `setupRoutes()` の別名に対して行うので、別名を
別の registry に書き換えると構文木のテストは通ったまま、どの connect も黙って無視される)、
`internal/stream/channels` の
チャンネルの実装 (`stream.Channel` / `stream.ChannelFactory` / `*…Factory` を返す exported な
`New…`) をどれも一覧が使うこと (使わない許可は `NewServerStats` だけで、使われていない項目も
落とす)、`setupRoutes()` が登録を本文の直下の文として 1 回だけ、/streaming の handler を作る
より前とどの return よりも前で呼び、関数リテラルの中を含めて `Register` /
`RegisterCredentialed` を直接呼ばないこと、`streamChannelDeps` の欄の値がどれも
`setupRoutes()` の本文の直下で 1 回だけ定義し、`=` で書き換えていない変数で (queueStats だけは
上の形に限って許す)、欄の書き忘れが無いこと、パッケージの中で stream の registry に登録する
関数が `registerStreamChannels()` に限られること)。

**`setupRoutes()` に残したもの** (`stream.New…` の 9 回の呼び出し):

| 理由 | 例 |
|---|---|
| stream の節より前にある受け手へ渡す | `noteEventPub`・`drivePublisher`・`reversiPublisher`・`mainStreamPublisher` (#3676 で先頭に置いたもの。受け手は core の service・連合の processor・handler)、`userListStreamPublisher` (上でまとめたもの。stream の節より前の受け手は `usersHandler`) |
| `Start` と shutdown hook を対で持つ | `serverStatsPub`、`queueStatsPub` (queueInspector の分岐の中) |
| 別の部品の構築の引数の中で作る | `NewAdminStreamPublisher` (`coreabuse.NewCreatedNotifier` の引数。ゲートが式ごと照合する)、`NewBubbleVersusPublisher` (`corebubbleversus.NewService` の引数) |

| | #3692 の前 (`f2d14ede1`) | 後 |
|---|---|---|
| `setupRoutes()` の `stream.New…` の呼び出し | 18 | 9 |
| `buildStream()` の `stream.New…` の呼び出し | — | 8 |
| `stream` のフィールド (`buildStream()` で作る部品) | — | 7 |
| `setupRoutes()` の `streamRegistry.Register…` | 22 | 0 |
| `builtinStreamChannels()` の項目 (チャンネル) | — | 21 (credentialed は 12) |
| `setupRoutes()` の行数 | 4,823 | 4,838 |
| setter で後から入れる辺 (4.1 の数え方) | 96 | 96 |

数え方: `setupRoutes()` と `buildStream()` の本文 (`func` の行から、行頭の `}` まで) に
`grep -oE '\bstream\.New[A-Z]'` を当てた。18 から 9 への差の 9 は、移した 8 回と、まとめて
無くなった `NewUserListStreamPublisher` の 1 回。登録は `setupRoutes()` の本文の
`grep -c 'streamRegistry\.Register'`、一覧の項目は `builtinStreamChannels()` の本文で
`{"` で始まる行の数 (credentialed はそのうち `credentialed},` で終わる行)、フィールドは
`streamParts` のフィールドの行数、行数は `setupRoutes()` の本文の `wc -l`。行数が増えたのは、
`streamChannelDeps` のリテラルと、移した理由を書いたコメントのため。

#### API の handler (#3694)

```
setupRoutes()
  …                                                      // meta・notes などの handler とルート
  hSvcs := s.buildHandlers(w, coreSvcs, fedSvcs, idGen)  // wiring_handlers.go
  w.handlers = hSvcs
  usersHandler := hSvcs.users                            // 別名 (変数から取る)
  …
  // Users endpoints
  usersHandler.SetUserListEventPublisher(…)              // setter とルートの登録は元の位置のまま
  …
  api.POST("/get-avatar-decorations", avatarDecorationsHandler.Get)
```

(#3694 の時点の形。ルートの登録は #3697 で `routes_<領域>.go` の関数へ移った。下の
「/api のルートの登録」)

- **`buildHandlers()` は `*Server` のメソッドで、repository は `w` から、core の service と
  連合の部品は `buildCore()` / `buildFederation()` の戻り値 `coreSvcs` / `fedSvcs` から
  受け取る。** 中の行は `setupRoutes()` にあった行をそのまま移し、作る順も元のまま。使う
  `Server` のフィールドは `s.db` / `s.config` / `s.echo`。`s.echo` は endpoints の handler の
  closure の中で読むだけで、作るときには読まない (呼ばれたときにルートを数える)。連合の部品
  から使うのは `apRenderer` (ap/show など) と `apURLs` (WebFinger)
- **移す条件は core・連合・queue・stream と同じ。** handler のコンストラクタの本体を 1 つずつ
  読み、引数を構造体に入れるだけなのを確かめた (nodeinfo は空の cache の map も作る)。
  移したのは 30 個: `usersHandler`、`iHandler`、`webhookHandler`、`notificationsHandler`、
  `hashtagsHandler`、`galleryHandler`、`blockingHandler`、`muteHandler`、`renoteMuteHandler`、
  `driveHandler`、`apHandler`、`wellknownHandler`、`nodeinfoHandler`、`federationHandler`、
  `channelsHandler`、`antennasHandler`、`clipsHandler`、`pagesHandler`、`flashHandler`、
  `followingHandler`、`rolesHandler`、`userListHandler`、`authHandler`、`appHandler`、
  `bubbleGameHandler`、`endpointsHandler`、`avatarDecorationsHandler`、`promoHandler`、
  `inviteHandler`、`echoTestHandler`
- **setter もルートの登録も 1 つも動かさない。** `buildHandlers()` を呼ぶ位置は、移した
  handler の中で最初に作っていた `usersHandler` の位置。移した構築は副作用を持たないので、
  DB と Redis の文の順は変わらない。handler へ依存を入れる setter (`setupRoutes()` で
  `…Handler.Set…` を呼ぶ 385 行) は、位置も順も元のまま
- **移すのは、`usersHandler` の位置へ前倒ししても、setter で後から入れる辺 (4.1) が増えない
  handler に限る。** 前倒しすると、その handler の setter の依存のうち、`usersHandler` の位置
  から元の位置までの間で定義していたものが、受け手より後で作った依存になる。呼ぶ位置は、
  移せる handler が最も多くなる位置を、#3676 の道具で handler を作っていた位置ごとに数えて
  選んだ (`usersHandler` の位置で 30 個。1 つ前の `notesHandler` の位置では 24 個、最初の
  `metaHandler` の位置では 16 個)
- **`/api/test` の echo の handler (`apitest.NewEchoHandler()`) と、test mode の
  `/api/reset-db` の handler (`apitest.NewHandler(...)`) は、型は同じだが別の値のまま。**
  echo の方はフィールドを持たず (`&Handler{}`)、`Test` もフィールドを読まない。reset-db の方は
  test mode の `if` の中で DB・Redis・meta を持って作る。test mode でないときは作らないので、
  1 つにまとめると「test mode でなくても reset-db の依存を持つ値ができる」か「echo を test mode
  の分岐に入れる」のどちらかになり、元と同じにならない。echo の方だけを `buildHandlers()` へ
  移した。`apibubblegame` の `NewHandler` (バブルゲームの記録) と `NewVersusHandler` (対戦) は
  別の型で、同じ handler を 2 回作ってはいない。記録の方だけを移した (対戦の方は
  `setupRoutes()` で作る `bubbleVersusService` に依存する)
- **`wiring` へは変数を経由した代入で入れ、別名は変数から取る** (上の規則)。別名を
  `buildHandlers()` の呼び出しより前に書くと、`buildHandlers()` を `buildCore()` や
  `buildFederation()` より前に置くと、どちらもコンパイルが通らない。`setupRoutes()` で
  使わなくなった別名 (`promoReadRepo`・`authSessionRepo`・`bubbleGameRepo`・`pageService`・
  `flashService`) は消した

照合する行が `buildHandlers()` の中にだけ移ったゲートは、`TestAvatarDecorationRoleSetsAreWired`
の `avatardecorations.NewHandler(s.db, roleService.ExistingRoleIDSet, roleService.PublicRoleIDSet)`
の 1 本だった (確かめ方: #3692 と同じく、tracked な `_test.go` すべてと
`tools/apicompat/main.go` の文字列リテラルのうち、#3694 の前の `router.go` に現れ、後の
`router.go` と `internal/server` のほかのファイル (`wiring_handlers.go` を除く) に現れないものを
探した。11 件で、ほかの 10 件はテストの import 宣言と `collectRoutes` の fixture の import の
パス 9 件と、関係の無いテストの `Existing`。どれも `internal/server` の行を照合しない)。上の規則のとおり、
`avatarDecorations: avatarDecorationsHandler`、`hSvcs := s.buildHandlers(w, coreSvcs, fedSvcs, idGen)`、
`w.handlers = hSvcs`、`avatarDecorationsHandler := hSvcs.avatarDecorations` と、ルートの登録
`api.POST("/get-avatar-decorations", avatarDecorationsHandler.Get)` も照合する (ルートの登録は、
#3697 から `routes_emojis.go` の `registerEmojisRoutes` と、`setupRoutes()` からの呼び出しまで
照合する)。
`internal/server/wiring_handlers_test.go` のテストも付けた (`buildHandlers()` が `apiHandlers` の
欄をすべて埋めること。method value は nil のレシーバでも作れるので、欄を書き忘れると最初の
リクエストまで気付かない。`setupRoutes()` が本文の直下で `hSvcs := s.buildHandlers(...)` と
`w.handlers = hSvcs` をこの順に 1 回ずつ書き、欄ごとに `x := hSvcs.<欄>` を 1 回だけ、
`w.handlers = hSvcs` より後に書くこと。別名と `hSvcs` を関数リテラルの中を含めて `=` で入れ替えず、
`:=` で定義し直さないこと。`hSvcs` の欄を書き換えず (`hSvcs.<欄> = …`)、`&hSvcs` を取らないこと。
欄の書き換えは、構築・別名・代入の行の文字列がどれも残るので、上の文字列で照合するゲートでは
捕まらない。パッケージのどこでも `<x>.handlers.<y>` の形でコンテナから読み戻さない
こと)。

ルートから handler の型を引くゲート (`collectRoutes`、docs/gates.md) は、別名
(`usersHandler := hSvcs.users`) から `buildHandlers()` の戻り値の構造体のフィールドの型までを
辿る。この形は 9 段の再帰が要り、辿る深さの上限 (`maxResolveDepth`) が 8 のままだと、/api の
route の解決が 280 本に落ちた (`TestAPIRouteHandlersResolve` の下限は 500)。上限を 16 に上げ、
`TestCollectRoutesResolvesHandlerTypes` の fixture にこの形を足した (上限を 8 に戻すと落ちる)。
別名を通しても、ルートはどれも前と同じ package の同じメソッドへ解決する。

**`setupRoutes()` に残したもの** (`internal/api` の `New…` の 27 回の呼び出し):

| 理由 | 例 |
|---|---|
| `buildHandlers()` を呼ぶ位置より前で作る | `metaHandler`・`resetHandler`・`emojisHandler` (どれも作るだけで、依存も先の領域だけ。位置の選び方は上)、`notesHandler` (`searchService` にも依存する。4.1)、`signupHandler`・`signinHandler` (setter の依存 `captchaSvc` は outbound client を使う)、`urlHandler` (`urlPreviewFetcher` は作るときに goroutine を起動する)、`statsHandler` (chart の engine)、`testHandler` (test mode の `if` の中) |
| `setupRoutes()` で作る部品に依存する | `proxyHandler` (`proxyService`)、`oauthHandler` (`chunkedUploadCapability`・`proxyAccountResolver`・`s.outboundOpts`)、`inboxHandler` (`federationResolver`・`federationProcessor`。outbound client に依存する)、`chartsHandler` (chart の engine)、`emojiApplicationHandler` (`emojiApplicationService`)、`backupHandler` (`backupVerifier`。`webauthnSvc` の有無で分岐して作り、作れなければ panic する)、`swHandler` (`webPushCache`。`TestPushSubscriptionCacheIsShared` が、キャッシュの生成と `apisw.NewHandler` への受け渡しを同じファイルに置くことを要求する)、`bubbleVersusHandler` (`bubbleVersusService`。stream の publisher に依存する) |
| stream の部品に依存する | `streamingHandler` (`streamManager`。`buildStream()` は stream の節で呼ぶ) |
| `usersHandler` の位置で作ると、setter で後から入れる辺が増える | `adminHandler` (`avatarDecorationResolver`・`streamRevokePublisher`・`emojiApplicationRepo`・`broadcastPublisher` の 4 辺)、`announcementHandler` (`broadcastPublisher`)、`reversiHandler` (`reversiService`)、`chatHandler` (`chatService`) |
| outbound の HTTP client を使う | `fetchExternalHandler`・`fetchRSSHandler` (`s.outboundClient(...)` が共有の transport を初めて作りうる) |
| setter の引数の中で作る | `drive.NewURLUploader`、`oauth.NewRedisStore`、`apiadmin.NewEmojiImageFetcher` |

| | #3694 の前 (`75226e7a4`) | 後 |
|---|---|---|
| `setupRoutes()` の `internal/api` の `New…` の呼び出し | 57 | 27 |
| `buildHandlers()` の `internal/api` の `New…` の呼び出し | — | 30 |
| `handlers` のフィールド (`buildHandlers()` で作る handler) | — | 30 |
| `setupRoutes()` の `…Handler.Set…` の行 | 387 (setter の呼び出しは 385) | 387 (同じ 385) |
| setter を呼ぶ handler (受け手) | 40 | 40 |
| `setupRoutes()` の行数 | 4,838 | 4,843 |
| setter で後から入れる辺 (4.1 の数え方) | 96 | 96 |

数え方: `internal/api` の `New…` は、`setupRoutes()` と `buildHandlers()` の構文木で、
`internal/api/` の下を import した名前の `New` で始まる関数の呼び出しを数えた (setter や別の
構築の引数の中の呼び出しも 1 回に数える)。`…Handler.Set…` の行は `setupRoutes()` の本文
(`func` の行から、行頭の `}` まで) の `grep -cE 'Handler\.Set[A-Z]'` で、うち 2 行はコメント
(`apHandler.SetExternalRedirectPolicy`) とルートの登録 (`bubbleVersusHandler.SetPublic`) なので、
setter の呼び出しは行頭が `<名前>Handler.Set` の 385 行。受け手はその 385 行の `<名前>Handler`
の種類。行の中身と順は前後で同じ (行の列を突き合わせた)。行数は同じ本文の `wc -l`。5 行増えたのは、
`hSvcs` の 2 行・別名の 30 行・理由を書いたコメント 9 行・空行 1 行 (計 42 行) を足し、構築の
30 行・使わなくなった別名 5 行・`buildHandlers()` へ移したコメント 2 行 (計 37 行) を消したため。

#### /api のルートの登録 (#3697)

```
setupRoutes()
  api := apiRoutes{s.echo.Group("/api")}
  api.Use(…)                                     // group の middleware (元の位置)
  …                                              // handler の構築と setter (元の位置のまま)
  registerMetaRoutes(api, metaHandler, statsHandler, endpointsHandler, echoTestHandler)
  …                                              // routes_<領域>.go の 24 個を 1 回ずつ
  registerFetchRoutes(api, fetchExternalHandler, fetchRSSHandler)
  …                                              // プラグインの準備 (失敗したら return)
  api.Any("/*", apiCatchall)                     // catchall は元の位置
```

- **領域ごとのファイル `routes_<領域>.go` に、`func register<領域>Routes(api apiRoutes, …)` を
  1 つ置く。** 受けるのは `apiRoutes` で、`*echo.Group` では受けない。`*echo.Group` で受けると、
  その中の登録に `withParamShadow` と `requireObjectBody` が付かず、
  `TestAPIRegistrationsGoThroughAPIRoutes` の走査からも外れる (docs/gates.md)。`Server` の
  フィールドを使わないので、メソッドではなく普通の関数にした
- **分け方は、`/api` の 1 つ目の区切りを単位にし、同じ handler が受ける小さな区切りは同じ
  ファイルにまとめた。** 例: `promo/read` は notes、`pinned-users` と `get-online-users-count`
  は users、`verify-email` は i、`retention` は charts、`page-push` は pages、`v2/admin/emoji/list`
  は admin。関係の近い区切りも 1 つにした: 利用者どうしの関係 (`following`・`blocking`・
  `mute`・`renote-mute`)、アカウントの作成と認証とアプリの認可 (`signup*`・`signin*`・
  パスワードの再設定・`invite`・`auth`・`miauth`・`app`・`my`)、通知と push の購読
  (`notifications`・`sw`)、連合 (`federation`・`v1/instance/peers`・`ap`)、ゲーム
  (`reversi`・`bubble-game`)、外部の取得 (`fetch-rss`・`fetch-external-resources`)、
  サーバーの情報 (`meta`・`ping`・`stats`・`server-info`・`endpoints`・`endpoint`・`test`)、
  絵文字とアバターデコレーション。`admin` (144 本) は 2 つ目の区切りで分けず 1 つにした。
  handler はほぼ `adminHandler` 1 つで、分けても引数が減らないため
- **ルートの行は移す前と同じ文字列にし、関数の引数の名前を `setupRoutes()` の変数の名前に
  揃える。`setupRoutes()` は同じ名前の変数をそのまま渡す。** ルートの行を文字列で照合する
  ゲート (`assertWired` など) を書き換えずに済み、名前が同じ値を指すことはテストで見る (下)。
  引数は多くて 7 個 (auth と i) なので、構造体にはしていない
- **ルートの行だけが使う値は、関数の中で作る。** `chartMethods` (`parseGetRoutes` が method の
  一覧の変数を同じファイルの中でだけ引くため。docs/gates.md) と、`chatRead` / `chatWrite`
  (`RequireChatAvailability` を返す closure を作るだけ)
- **呼ぶ位置は 1 か所。** handler をすべて作った後で、プラグインの準備 (`registeredPlugins` の節)
  の直前。すべての `api.Use` より後 (echo の group は `Use` より前に登録したルートに middleware を
  付けない)、`setupRoutes()` のどの `return` よりも前 (プラグインの準備に失敗した経路でも、元と
  同じく /api のルートを登録してから止まる) になる。並びは、移す前に各ファイルの最初のルートが
  あった順にした

| ファイル | 関数 | 1 つ目の区切り (本数) | 登録の文 | 引数 (`api` の後) |
|---|---|---|---|---|
| `routes_meta.go` | `registerMetaRoutes` | `server-info` 2・`meta` 1・`ping` 1・`stats` 1・`endpoints` 1・`endpoint` 1・`test` 1 | 8 | `metaHandler`、`statsHandler`、`endpointsHandler`、`echoTestHandler` |
| `routes_auth.go` | `registerAuthRoutes` | `signup-application` 4・`auth` 4・`invite` 4・`miauth` 2・`app` 2・`signup` 1・`signup-pending` 1・`username` 1・`signin` 1・`signin-flow` 1・`signin-with-passkey` 1・`request-reset-password` 1・`reset-password` 1・`my` 1・`email-address` 1 | 26 | `signupHandler`、`signinHandler`、`resetHandler`、`authHandler`、`appHandler`、`inviteHandler`、`roleService` |
| `routes_emojis.go` | `registerEmojisRoutes` | `emoji-application` 3・`emojis` 2・`emoji` 2・`get-avatar-decorations` 1・`export-custom-emojis` 1 | 9 | `emojisHandler`、`emojiApplicationHandler`、`avatarDecorationsHandler`、`roleService` |
| `routes_notes.go` | `registerNotesRoutes` | `notes` 38・`promo` 1 | 39 | `notesHandler`、`promoHandler` |
| `routes_users.go` | `registerUsersRoutes` | `users` 33・`get-online-users-count` 2・`pinned-users` 1 | 36 | `usersHandler`、`userListHandler`、`roleService` |
| `routes_i.go` | `registerIRoutes` | `i` 59・`verify-email` 1 | 60 | `iHandler`、`webhookHandler`、`notificationsHandler`、`pagesHandler`、`flashHandler`、`announcementHandler`、`roleService` |
| `routes_notifications.go` | `registerNotificationsRoutes` | `notifications` 4・`sw` 4 | 8 | `notificationsHandler`、`swHandler` |
| `routes_hashtags.go` | `registerHashtagsRoutes` | `hashtags` 6 | 6 | `hashtagsHandler` |
| `routes_gallery.go` | `registerGalleryRoutes` | `gallery` 9 | 9 | `galleryHandler` |
| `routes_relations.go` | `registerRelationsRoutes` | `following` 12・`blocking` 3・`mute` 3・`renote-mute` 3 | 21 | `blockingHandler`、`muteHandler`、`renoteMuteHandler`、`followingHandler` |
| `routes_drive.go` | `registerDriveRoutes` | `drive` 24 | 24 | `driveHandler`、`chatHandler` |
| `routes_federation.go` | `registerFederationRoutes` | `federation` 9・`ap` 2・`v1` 1 | 12 | `federationHandler`、`apHandler`、`roleService` |
| `routes_channels.go` | `registerChannelsRoutes` | `channels` 16 | 16 | `channelsHandler`、`roleService` |
| `routes_antennas.go` | `registerAntennasRoutes` | `antennas` 7 | 7 | `antennasHandler` |
| `routes_clips.go` | `registerClipsRoutes` | `clips` 11 | 11 | `clipsHandler` |
| `routes_pages.go` | `registerPagesRoutes` | `pages` 7・`page-push` 1 | 8 | `pagesHandler` |
| `routes_flash.go` | `registerFlashRoutes` | `flash` 10 | 10 | `flashHandler` |
| `routes_charts.go` | `registerChartsRoutes` | `charts` 12・`retention` 2 | 14 | `chartsHandler` |
| `routes_roles.go` | `registerRolesRoutes` | `roles` 5 | 5 | `rolesHandler` |
| `routes_announcements.go` | `registerAnnouncementsRoutes` | `announcements` 2 | 2 | `announcementHandler` |
| `routes_admin.go` | `registerAdminRoutes` | `admin` 144・`v2` 1 | 145 | `adminHandler`、`announcementHandler`、`backupHandler`、`roleService`、`backupGuard` |
| `routes_games.go` | `registerGamesRoutes` | `bubble-game` 13・`reversi` 7 | 20 | `reversiHandler`、`bubbleGameHandler`、`bubbleVersusHandler` |
| `routes_chat.go` | `registerChatRoutes` | `chat` 40 | 40 | `chatHandler`、`roleService` |
| `routes_fetch.go` | `registerFetchRoutes` | `fetch-rss` 2・`fetch-external-resources` 1 | 3 | `fetchExternalHandler`、`fetchRSSHandler` |

数え方: 「登録の文」は、関数の本文の `api.POST` / `api.GET` / `api.Match` の呼び出しの数
(`api.Match` は 2 つの method を登録するが 1 に数える)。区切りは、その呼び出しの path の
最初の `/` と次の `/` の間。行の並びは表の上からが `setupRoutes()` で呼ぶ順。

**登録が値を受け取る時点が変わる。** ルートの行の引数 (handler の method value、
`middleware.RequireRolePolicy(roleService, …)` のような呼び出し) は、登録した時点で評価される。
移す前は各領域の handler を作った直後に登録していたので、元の位置から呼ぶ位置までの間に変わる
値を受け取っていると、登録が受け取る値も変わる。移した 539 文について、次を道具で確かめた
(`setupRoutes()` を `go/types` で読む。道具はコミットしていない)。

- 行に現れる `setupRoutes()` の変数は、どれも `setupRoutes()` で 1 回だけ定義し、`=`・`++`・
  `&` で書き換えていない。書き換えのある変数を使うルートは `/avatar/@:acct` の `localHost`
  だけで、/api の外なので移していない
- handler の method value の受け手はすべてポインタで、値の受け手のメソッドや、埋め込みを通して
  引くメソッドは無い。method value は作った時点で受け手を写すので、値の受け手だと、その間の
  setter が入れた依存を登録が持たなくなる。ポインタなら setter の結果は呼ばれたときに見える
- 行の中の呼び出しは middleware の constructor (`RequireAuth`・`RequireScope`・
  `RequireNotMoved`・`RequireSecure`・`RequireModerator`・`RequireAdmin`・`RequireRolePolicy`・
  `RequireRolePolicyPublic`・`RejectAppToken`・`AdmitAppUser`、chat の `RequireChatAvailability`)
  だけ。どれも引数を閉じ込めた closure を返すだけで、ログ・metrics・Redis・`s.*` への代入を
  持たない (本体を読んで確かめた)。`roleService` も `backupGuard` も書き換えていない
- /api の行には、handler を関数リテラルで書いたものが無い

**echo の登録の順は変わる。** `Echo.Routes()` は map から作るので順を持たない。
`Echo.OnAddRouteHandler` で登録の順に並べると、前後で同じ位置にある登録は、test mode でない
構成で 667 のうち 61、test mode と pprof を有効にした構成で 676 のうち 69 だった。順が効くのは
同じ method と path を 2 回登録したとき (後の登録が勝つ) だけで、2 回登録しているのは
`api.Use` が足す `echo_route_not_found` の `/api` と `/api/*` だけ (順は前後で同じ)。catchall
(`/api/*` の `Any`) とプラグインのルートは、前と同じく /api のほかのルートより後に登録する。

**残したもの**

| 登録 | 理由 |
|---|---|
| test mode の `/api/reset-db` | handler を test mode の `if` の中で作る。`if` ごと元の位置に残した (`apiRoutesOutsideRegistrars` に、どの経路で登録するかを書いた) |
| catchall の `api.Any("/*", apiCatchall)` | プラグインのルートより後に置く (読み手のため。echo は静的な path を wildcard より優先するので、順には依存しない)。`apiRoutes` を通らない登録なので、`apiRegistrationAllowed` でファイル (`router.go`) まで決めてある |
| /api の外の `s.echo` の 64 文 (ActivityPub・SSR・well-known・oauth・nodeinfo・inbox・streaming・`/files`・`/proxy`・`/url`・`/backup-download`・静的配信・SPA) | `apiRoutes` を通らない別の層で、この段の関数の形 (`api apiRoutes` を受ける) に当てはまらない。`/notes/:id` や `/@:acct` のように `s` と `ssrMeta` などを捕まえる closure、構築の呼び出しを行の中に持つもの (`filesHandler(…)`、`newTwemojiBadgeHandler(…)`) があり、評価の時刻の確認がこの段より重い。静的配信と SPA の catchall はプラグインの準備の `return` より後にある |
| `/debug/pprof` の 8 文 | `s.config.EnablePprof` の `if` の中で、/api の外 |

**同じ動きであることの確かめ方。** `develop` (`b3d75729a`) とこの段の binary で、
`elythia dump-routes` の出力、起動時の Redis の命令の列と起動ログ、PostgreSQL の文の集合が
一致する。#3676 の道具で並べた副作用のある文の列、setter の辺の一覧 (行番号を除く)、
呼び出しが離れている辺の一覧も一致する。加えて、起動したサーバーで `OnAddRouteHandler` から
ルートごとの method・path・名前・handler の関数名・middleware の関数名の並びを書き出し、前後で
集合として一致することを確かめた (上の 2 つの構成で 667 と 676)。middleware の関数名は引数を
含まないので、各ルートの行の引数の式も構文木から文字列で書き出し、`api`・`s.echo`・pprof の
613 文で前後が一致することを確かめた。`collectRoutes` の解決 (589 本、/api の 528 本) も前後で
同じ。

**ゲートとテスト**

- `internal/server/routes_registrars_test.go`
  - `TestSetupRoutesCallsEveryRouteRegistrarOnce`: 登録の関数ごとに、`setupRoutes()` の本文の
    直下の式文としての呼び出しが 1 つだけで、ほかから呼ばない。呼び出しはすべての `api.Use`
    より後、どの `return` と `setupPlugins` よりも前。引数は関数の引数と同じ名前の識別子で、
    `setupRoutes()` の本文の直下で `:=` で 1 回だけ定義し、関数リテラルの中を含めて書き換えず、
    アドレスも取らない
  - `TestRouteRegistrarsOnlyRegisterRoutes`: 本文の直下は `api` の `POST` / `GET` / `Match` の
    式文と `:=` だけ (`if` の中の登録、`api.Use`、`api.Group` を落とす)。引数を書き換えない
  - `TestAPIRoutesAreRegisteredByRegistrars`: パッケージのどこでも、`api` への `POST` / `GET` /
    `Match` は登録の関数の中に限る。外は `apiRoutesOutsideRegistrars` (`/api/reset-db` だけ。
    使われていない項目も落とす)
  - 登録の関数の形も見る: `apiRoutes` を受ける関数は、`register…Routes` という名前の、
    `routes_` で始まるファイルの、最初の引数を `api` と名付けた普通の関数に限る。名指しの
    4 つ (notes・admin・i・chat) とファイルを要求し、20 個未満なら落とす。最初の引数の名前を
    `api` に限るので、perm / secure / oauth / cacheSec などの `api.` を正規表現で見る抽出から
    ルートが外れることも無い (docs/gates.md の既知の限界)
- `TestAPIRegistrationsGoThroughAPIRoutes` (`param_shadow_test.go`) は、許可した登録をファイルごとに
  照合するようにした。catchall を `routes_<領域>.go` へ移すと落ちる
- ルートの行を文字列で照合していたゲートは、行に出る名前が関数の引数になったので、
  `assertRouteRegisteredBy(t, "<ファイル>", "<関数>", <行>, …)` (`internal/entitycompat`) で照合する。
  行がそのファイルのその関数の本文の直下にあり、`setupRoutes()` がその関数を本文の直下で 1 回だけ、
  引数と同じ名前の変数を渡して呼ぶことを見る。対象は `TestDriveUsageRouteIsRegistered`、
  `TestIPAccountSearchRouteIsRegistered`、`TestIPRelatedAccountsRouteIsRegistered`、
  `TestIPLookupLogRouteIsRegistered`、`TestAvatarDecorationRoleSetsAreWired` の 5 つ。前の
  `assertWired` のままだと、`setupRoutes()` で別名を作って渡す・引数の名前を変えて行の名前へ
  入れ直す・`if` の中で呼ぶ・ルートを別の関数へ移す、の 5 つの変異がすべて通った
- 照合するリテラルが移ったゲートの探し方: tracked な `_test.go` すべてと
  `tools/apicompat/main.go` の文字列リテラル (と、その各行) のうち、#3697 の前の `router.go` に
  現れ、後の `router.go` と `routes_*.go` 以外の `internal/server` のファイルに現れないもの。
  568 件で、ほとんどは endpoint の path や scope の文字列。`internal/server` のソースを読む
  テストのファイルに絞り、`setupRoutes()` の変数の名前を含む行を照合しているものが上の 5 つ。
  ほかは middleware の中身 (`RequireRolePolicy(` や scope) を見るもので、名前が指す値は上の
  `TestSetupRoutesCallsEveryRouteRegistrarOnce` が見る
- `collectRoutes` (docs/gates.md) は、関数の引数で受けた handler の型を引くので、変更は要らなかった

| | #3697 の前 (`b3d75729a`) | 後 |
|---|---|---|
| `setupRoutes()` の行数 | 4,843 | 4,069 |
| `setupRoutes()` のルートの登録の文 | 613 (`api` 541・`s.echo` 64・pprof 8) | 74 (`api` 2・`s.echo` 64・pprof 8) |
| 同じ (前の段の数え方: 行頭の `api.` / `s.echo.` / `pprofGroup.` の `GET` などの行) | 605 (541・56・8) | 66 (2・56・8) |
| `routes_<領域>.go` のファイル (登録の関数) | — | 24 (539 文) |
| `setupRoutes()` の節点 (4.1 の数え方) | 364 | 361 |

数え方: 行数は `setupRoutes()` の本文 (`func` の行から、行頭の `}` まで) の `wc -l`。774 行
減ったのは、ルートの登録の文 590 行 (複数行の文がある)・それに付いていたコメント 200 行・空行 19 行・
登録の関数へ移した 3 つの定義の 3 行 (計 812 行) を消し、呼び出しの 24 行・理由のコメント 13 行・
空行 1 行 (計 38 行) を足したため。登録の文は構文木で、`api`・`s.echo`・`pprofGroup` への
`GET`・`POST`・`PUT`・`DELETE`・`PATCH`・`OPTIONS`・`HEAD`・`Match`・`Any`・`Add`・`File`・`Static`
の呼び出しの式文を数えた (`if` の中も数える)。前の段の数え方は `GET|POST|PUT|DELETE|Match|Any` の
行頭だけを数えるので、`OPTIONS`・`File`・`Static` の 8 文を数えない。

**#3666 の段階 (2) はここまで。** 次は段階 (3)、部品に起動・停止の口を持たせる段 (#3661・#3662 で
先に直した内容を土台にする)。上で残した core の service・連合の部品・queue の processor・
stream の部品・handler と、/api の外のルートの登録は、依存する領域の起動・停止を扱うときに
一緒に扱う。部品を移すときは、どれも `wiring` のフィールドに入れ、`build…` の関数で作り、
`setupRoutes()` では別名を置く。repository は `w` から、それより後の領域は
`coreSvcs` / `fedSvcs` / `queueSvcs` / `streamSvcs` / `hSvcs` のような変数から受け取る。後の
領域ほど、構築の途中で動く処理 (4.1) と shutdown hook を登録するものが多いので、それらを
動かさずに残す範囲を PR ごとに決める。

### 4.3 部品の起動と停止の一覧 (#3699、#3701、#3703)

**停止が要る部品は、名前を付けて 1 つの一覧 `lifecycle` (`internal/server/lifecycle_parts.go`)
に登録する。** #3666 の段階 (3)。前は `registerShutdownHook(func(context.Context))`
の無名の closure の並びで、どの部品の停止かがテストからも実行時からも見えなかった。#3699 で
すべての呼び出しを名前付きの登録に置き換え、`registerShutdownHook` と `shutdownHooks` を消した。
無名で登録する口は残していない。#3701 で、起動と停止が部品自身のメソッドであるものは、値を 1 つ
渡すだけで起動と停止の登録を済ませる形にした。#3703で、一覧の項目に起動を持たせ、構築では登録だけを
行い、起動は`StartBackground`が一覧の順にまとめて行う形にした(下の「起動」)。#3705で、構築の途中で
`go`文で回していた最後の部品`emojiInvalidationQueue`に`Start`と`Stop`を持たせ、値から作る登録
(`startPartWithStop`)で一覧に入れた。#3707で、部品の停止のpanicを部品ごとに回収し、残りの停止を
続ける形にした(下の「停止のpanic」)。#3709で、`StartBackground`と`StartBackgroundForTest`が同じ
1度きりの起動を通り、停止を始めた後は何も起動しない形にした(下の「2回呼んだとき」と「停止の後の起動」)。
#3711で、世代の停止(`shutdown`)と構築の後始末(`cleanupConstruction`)で直書きしていた手順を、名前付きの
段の表(`shutdownSteps`・`cleanupSteps`)にし、段ごとにpanicを回収して残りの段を続ける形にした(下の
「世代の停止」と「停止のpanic」)。

- 項目は名前・止める値 (`part`)・起動 (`func()`。起動するものが無い部品はnil)・停止
  (`func(context.Context)`) を持つ。`part` は停止が働きかける値 (バッファ・publisher・service・
  プラグインの store など) で、本番のコードは読まない。名前どおりの
  値を止めたかをテストが確かめるために持つ
- 一覧は `Server.lifecycle` が値で持つ (テストが組む `Server` のリテラルでも、ゼロ値が空の一覧として
  使える)。登録はどれも `Server` のメソッド (下の表の 8 つ) を通し、起動と停止も `s.lifecycle` から回す
  (`paramShadowMiddleware` と `dbBackedStorage` は `w` を受け取らない)。コンテナの
  `wiring.lifecycle` は同じ一覧を指すが、**今は誰も読まない** (消してもテストは落ちない)。
  段階 (6) で、メンテナンスの作り直しの範囲を部品の単位に絞るときに使う
- **構築では起動しない(#3703)。** 登録は部品と起動・停止を一覧に足すだけで、起動は
  `StartBackground`が一覧の順に呼ぶ(下の「起動」)。`StartBackground`自身が起動するもの(queueの
  worker・autoscale・scheduler・chartの保存のloop)は、下の一覧の外の停止の段(`queue`・`chart`)と
  対になっていて、一覧には入れていない
- **名前は、部品を持つ変数の名前にする** (`setupRoutes()` の局所変数か、`Server` のフィールド)。
  例外は 3 つで、起動時に 1 回だけ走る retention の集計は `retentionAggregateAtStartup`
  (変数 `retentionSvc` は集計の service で、止めるのは起動時の 1 回の実行だけ)、param shadow の
  記録は `paramShadowRecorder` (`param_shadow.go` の変数名は `rec` で、部品が分からない)、プラグインの
  store は `pluginStorage:<プラグイン名>` (プラグインごとに 1 つ開く) にした
- **名前が空・停止が nil・名前の重複は、構築の失敗にする** (`constructionError`)。その場で panic
  しないのは、プラグインの名前のように実行時の値から作る名前があるため。重複した部品も一覧に
  入れるので、構築に失敗した世代の後始末で、それも止まる

**登録の形 (#3701)。** 部品が持つメソッドの形ごとに、小さな interface で受ける関数を分けた。
1 つの関数で `any` を受けて形を実行時に振り分けるのではなく、形ごとに分けたのは、形の合わない値を
渡すとコンパイルで落ちるようにするため。停止は渡した値のメソッドから作るので、`part` と停止が
別の値を指すことは起きない。

| 登録 | 受ける形 | 起動 | 停止 | 部品 |
|---|---|---|---|---|
| `s.startPartWithClose(ctx, name, part)` | `Start(ctx)` + `Close()` | `StartBackground`で`Start(ctx)`(ctxは登録で渡したもの) | `Close()`。停止の ctx は捨てる | `instanceTouchBuffer`・`sigCapBuffer`・`instanceCounterBuffer` |
| `s.startPartWithStop(ctx, name, part)` | `Start(ctx)` + `Stop(ctx)` | `StartBackground`で`Start(ctx)`(ctxは登録で渡したもの) | 停止の ctx を渡して `Stop(ctx)` | `deliveryHealth`・`inboxHealth`・`emojiInvalidationQueue` |
| `s.startPartWithoutCtx(name, part)` | `Start()` + `Stop()` | `StartBackground`で`Start()` | `Stop()`。停止の ctx は捨てる | `serverStatsPub`・`queueStatsPub` |
| `s.addCloser(name, part)` | `Close(ctx)` | しない | 停止の ctx を渡して `Close(ctx)` | `metadataRefresher` |
| `s.addErrCloser(name, part)` | `Close() error` | しない | `Close()`。エラーと停止の ctx は捨てる (エラーを捨てるのは前の hook と同じ) | `pluginStorage:<名前>` (`setupRoutes()` の外の `dbBackedStorage` で登録する) |
| `s.addPartWithStart(name, part, start, stop)` | 問わない (起動と停止を closure で渡す) | `StartBackground`で渡した`start` | 渡した `stop` | `fedRuleHits` |
| `s.startConstructionWorker(name, run)` | 値を持たない (関数を渡す) | `StartBackground`で`run`をgoroutineで回す | ctx を切り、`run` が返るのを待つ。起動していなければ待たずに返す | 下の worker の 4 つ |
| `s.addPart(name, part, stop)` | 問わない (停止を closure で渡す) | しない (起動するものが無い) | 渡した `stop` | `hashtagService`・`paramShadowRecorder` |

名前の`start…`は、#3703より前の「登録したその場で起動する」の名残で、今は「`StartBackground`で
起動する部品を登録する」の意味。`startConstructionWorker`も構築の途中では起動しない。名前を
変えなかったのは、この段の差分を起動の移動だけにするため(テストの`valueShapes`の表や、この節と
4.1の記録が名前で登録を指している)。

**closureのまま残した7つ**は、停止が部品のメソッド1つでは書けないもの(#3705で
`emojiInvalidationQueue`が値から作る登録へ移り、8つから7つになった)。

| 名前 | closure が要る理由 | `part` |
|---|---|---|
| `fedRuleHits` | `Start` に渡した ctx を切って止める。`HitStore` に停止のメソッドは無い。`addPartWithStart`で起動も渡す | `*fedrule.HitStore` |
| `hashtagService` | `Shutdown(ctx)` がエラーを返し、時間切れを警告に出す | `*hashtag.Service` |
| `paramShadowRecorder` | 停止は `Flush()` で、停止のための口ではない | `*paramshadow.Recorder` |
| `pollExpiryWorker`・`reactionFlushWorker`・`retentionAggregateAtStartup`・`userCacheLink` | `startConstructionWorker(name, run)` が関数を goroutine で回し、ctx を切って返るのを待つ。止める値が無い | nil |

この7つの`part`は、本番で読まない値のまま残した(nilにしなかった)。理由は2つで、型の表の
テスト (下) が別の種類の値を渡す取り違えを落とせることと、`hashtagService` は値を引いて止まったこと
を観測しているため (`TestNewServer_ShutdownStopsTheNamedParts`)。

**値から作れる部品を `addPart` で登録していないかは、テストで検査する。** closure で書くと、`part` と
停止が別の値を指しうる形に戻るため。一覧の項目は、値から作る登録を経たかの印 (`fromValue`) を持つ。
`TestNewServer_ShutdownStopsThePartsInTheirOrder` が 7 つの組で、`addPart` で登録した部品のうち、
`part` が上の 5 つの形のどれかを満たすものを落とす。理由付きの許可リスト (`addPartShapeAllowlist`。
理由の欄には、なぜ closure のまま残すかを書く) に載っているものは落とさず、使われていない項目は落とす。
許可リストは今は空(closureの7つは、どれも形を満たさない)なので、使われていない項目の検査は
空振りする。代わりに、検査がclosureの3つ(`fedRuleHits`・`hashtagService`・`paramShadowRecorder`)を
見たことと、値から作る10個に印が付いたことを名指しで確かめる。
形の判定 (テストの `valueShape`) は、`TestValueShape_ClassifiesThePartTypes` が型の名前を並べて固定する。
値から作る10個の部品の型は9つ(`deliveryHealth`と`inboxHealth`は同じ型)で、どれも形を満たす。
closureの3つの型と値の無いworkerは満たさない。部品の型にメソッドを足して形が変わると、ここが落ちる。
判定が知る形の一覧 (`valueShapes`) は、`TestValueShapes_CoverEveryRegistrationFromValue` が
`lifecycle_parts.go` を構文木で読み、`addFromValue` を呼ぶ登録の関数と、その値の引数の interface の型を
列挙して突き合わせる (1 つも拾えなければ落とす)。登録の形を足して判定に足し忘れると、ここが落ちる。
ただし読むのは `lifecycle_parts.go` の `Server` のメソッドだけで、別のファイルに置いた登録と、
型パラメータを持つ自由関数 (メソッドは型パラメータを持てない) で書いた登録は拾えない。
組をまたぐ検査 (許可リストの使われていない項目と下限) は、7 つの組が全部走ったときだけ行う
(`-run` で組を絞ったときは、関係の無い失敗を出さないように省く)。

**実行時には拒まない** (`addPart` は形を見ない)。拒むと、部品の型にメソッドを足しただけで本番の構築が
失敗し、案内どおりに値から作る登録へ直すと、closure の停止 (`hashtagService` の時間切れの警告など) が
失われるため (#3701 のレビュー)。

**同じ型どうしの取り違えを書く余地が無くなった範囲。** #3699 では値と停止を別々に渡すので、
`s.addPart("deliveryHealth", deliveryHealth, func(ctx context.Context) { inboxHealth.Stop(ctx) })` の
ような、値と停止で別の部品を指す誤りを書けた。同じ型どうし (`deliveryHealth` と `inboxHealth`) では、
型の表のテストでも落とせなかった。#3701 の後は次のとおり。

- **値から作る10個(#3705の後)は、値と停止が別の部品を指す誤りを書けない。** 停止は渡した値そのものから作る。
  起動も同じ値に対して呼ぶので、値と起動が別の部品を指す誤りも書けない
- **同じ値を 2 回渡す誤りは、テストが落とす。** 例えば `s.startPartWithStop(ctx, "inboxHealth", deliveryHealth)`
  と書くと、値が起動と停止を決めるので、`inboxHealth` は起動も停止もされない。同じ型なので型の表では
  落ちないが、`TestNewServer_ShutdownStopsThePartsInTheirOrder` が 7 つの組で、nil でない `part` どうしが
  互いに異なる値であること (同一性) を見て落とす
- **名前と値の入れ替えは落とせない。** `deliveryHealth` の名前に `inboxHealth` を、`inboxHealth` の名前に
  `deliveryHealth` を渡す形は、同じ型で値も 2 つとも異なるので、どのテストも落ちない (使い捨ての変異で、
  `internal/server` のテストがどれも落ちないことを確かめた)。名前は呼び出し元が渡す文字列で、値からは
  作れないため。ただし両方とも起動され、両方とも止まるので、起動されない・止まらない部品は出ない
  (変わるのは、一覧の名前と値の対応と、その 2 つの間の起動と停止の順だけ)。別の型の値を渡す誤り
  (`sigCapBuffer` の名前に `instanceTouchBuffer` を渡すなど) は、型の表のテストが落とす
- **closureの7つ(#3705の後)は#3699と同じ。** 値と停止の対応は保証しない。`fedRuleHits`・
  `hashtagService`・`paramShadowRecorder`は型が互いに違うので、別の部品の値を渡す誤りは
  型の表のテストが落とす。停止の closure が別の部品を止める誤りは、観測している `hashtagService` の
  ほかは落とせない

起動と停止の順と、登録の条件は次のとおり(上から順に起動し、上から順に止める。どちらも登録した順と同じ)。
「起動」が○の13個は`StartBackground`が起動し、構築だけでは動かない(#3703。`emojiInvalidationQueue`は#3705)。

| 順 | 名前 | 登録 | 起動 | 止め方 | 登録の条件 |
|---|---|---|---|---|---|
| 1 | `pollExpiryWorker` | `startConstructionWorker` | ○ | ctx を切り、`Run` が返るのを待つ | queue role |
| 2 | `fedRuleHits` | `addPartWithStart` | ○ | `Start` に渡した ctx を切る | 常に |
| 3 | `metadataRefresher` | `addCloser` | | `Close(ctx)`。新しい取り直しを止め、走っているものを短く待つ | 常に |
| 4 | `instanceTouchBuffer` | `startPartWithClose` | ○ | `Close()`。残りを flush する | 常に |
| 5 | `sigCapBuffer` | `startPartWithClose` | ○ | `Close()`。残りを flush する | 常に |
| 6 | `reactionFlushWorker` | `startConstructionWorker` | ○ | 1 と同じ | 常に |
| 7 | `instanceCounterBuffer` | `startPartWithClose` | ○ | `Close()`。残りを flush する | 常に |
| 8 | `deliveryHealth` | `startPartWithStop` | ○ | `Stop(ctx)` | queue role |
| 9 | `inboxHealth` | `startPartWithStop` | ○ | `Stop(ctx)` | queue role |
| 10 | `hashtagService` | `addPart` | | `Shutdown(ctx)`。in-flight の集計を待つ (#727) | 常に |
| 11 | `retentionAggregateAtStartup` | `startConstructionWorker` | ○ | 1 と同じ | queue role |
| 12 | `paramShadowRecorder` | `addPart` | | `Flush()`。最後の要約を書く | `disableParamShadow` でなく、本家の paramDef を読めたとき |
| 13 | `emojiInvalidationQueue` | `startPartWithStop` | ○ | `Stop(ctx)`。ctxを切り、`Run`が溜まった分を送って返るのを待つ | 常に |
| 14 | `userCacheLink` | `startConstructionWorker` | ○ | 1 と同じ | 常に |
| 15 | `serverStatsPub` | `startPartWithoutCtx` | ○ | `Stop()` | server role |
| 16 | `queueStatsPub` | `startPartWithoutCtx` | ○ | `Stop()` | server role で、queueInspector があるとき |
| 17〜 | `pluginStorage:<名前>` | `addErrCloser` | | `Close()` (DB のプールを閉じる) | 有効なプラグインごと (`newServer` が DB の store を開くとき。本番の経路)。プラグインの順 |

「常に」は role と設定に関係なく登録するもの。両方の role (既定) では 1〜16 がすべて、server role
では 2〜7・10・12〜16、queue role では 1〜14 が並ぶ。`deliveryBreaker` は停止を持たないので、
有無で並びは変わらない。

**起動(#3703)。** `StartBackground`は次の順に起動する。本番の経路(Supervisor)は「構築 →
`StartBackground` → リクエストを回す」の順なので、リクエストを受けるときに動いている部品の集合は
#3703の前と変わらない。

1. **一覧の部品のうち起動を持つものを、一覧の順に起動する**(`lifecycle.startAll`)
2. queueのworkerを起動し、autoscaleとschedulerを起動する(queue roleのとき。前と同じ)
3. chartの保存のloopを起動する(両方のrole。前と同じ)

- **一覧をqueueより先に起動する理由。** queueのworkerのジョブが、バッファ(`instanceTouchBuffer`・
  `sigCapBuffer`・`instanceCounterBuffer`)、配送の健全性(`deliveryHealth`・`inboxHealth`)、連合の
  ルールの記録(`fedRuleHits`)へ書く。書き手より先に、溜めたものを流す側を動かす。schedulerの
  cronのジョブも同じworkerが処理する。#3703の前は構築の途中で起動していたので、queue・
  autoscale・scheduler・chartより先という相対的な順は変わらない。停止はqueueを先に止めてから一覧を
  止めるので(下の手順2と4)、起動と停止はqueueを挟んで対になる
- **chartとの前後。** 一覧の部品はchartの保存のloopに頼らない(chartのhookはメモリの集計へ書くだけで、
  loopの有無に関係なく書ける)。前と同じくchartを最後にした
- **起動のctx。** 前と同じく、登録で渡した`context.Background()`(`fedRuleHits`は自分でcancelするctx)を
  使う。`StartBackground`はctxを受け取らず、部品は停止のメソッド(`Close`・`Stop`・ctxのcancel)で
  止める。停止で切れるctxを渡すと、バッファの`runLoop`が`Close`の最後のflushより先にctxで抜けるなど、
  止まり方が変わる
- **起動の失敗。** 部品の起動はどれもgoroutineを立てるだけで、エラーを返さない。panicしたときは
  `startAll`が回収してエラー(`errLifecycleStart`)にし、後の部品を起動せずに返す。`StartBackground`は
  queueもchartも起動せずにそのエラーを返す。Supervisorはそれを受けて世代を`StopBackground`で止め、
  DBのプールを閉じる(`startGenerationLocked`。最初の世代は`Start`がエラーを返し、`serve`が
  `Shutdown`する)。#3703の前は構築の中で起動していたので、panicは構築ごとプロセスを落としていた
- **2回呼んだとき。** `StartBackground`と`StartBackgroundForTest`は同じ`startOnce`を通る(#3709)。
  どちらを先に呼んでも、先に呼んだ方だけが起動し、後から呼んだ方(どちらでも)は何も起動せずに先の
  結果を返す。`startAll`も1回目だけ起動し、`stopAll`を始めた後は何も起動しない(止める人の居ない
  goroutineを残さないため)。本番の経路で`StartBackground`を呼ぶのは、最初の世代を`Supervisor.Start`が、
  作り直した世代を`startGenerationLocked`が呼ぶ2か所で、同じ世代に2回は呼ばない(`Start`は
  listenより前に呼び、メンテナンスの監視は`Start`の中のlistenの後で始まる)
- `StartBackgroundForTest`(e2e-federationが使う)は、`StartBackground`と同じ起動の関数を、違いを
  引数(`backgroundStart`)で渡して通る。違いは2つで、roleを見ずにqueueのworker・autoscale・schedulerを
  起動することと、chartを起動しないこと(どちらも前と同じ)。一覧をqueueより先に起動するのも同じ。
  先に`StartBackgroundForTest`を呼んだ世代では、後から`StartBackground`を呼んでもchartは起動しない
- **停止の後の起動(#3709)。** `Server`は起動と停止の順を守るmutex(`runMu`)と、停止を始めた印
  (`stopped`)を持つ。停止(`shutdown`)は何かを止めるより先に、mutexを取って印を立てる。構築の後始末
  (`cleanupConstruction`)も同じ印を立てる。起動はmutexを握ったまま走り、印が立っていれば何も
  起動せずに`errGenerationStopped`を返す(1度きりの起動の結果なので、後の呼び出しも同じエラーを返す)。
  起動の途中で停止が始まったときは、停止は起動が済むのを待ってから印を立て、その起動が起動したもの
  をすべて止める。起動の間はmutexを握ったままにした。起動は部品のgoroutineを立てるほか、cronの登録
  (`registerSchedulerJobs`の11個の登録の関数)と撤去(`PruneUnregistered`)でRedisを
  `context.Background()`のまま呼ぶので、その分だけ長くかかりうる
  - **本番の経路に足す待ちは無い。** Supervisorは`StartBackground`と`StopBackground`をどちらも`genMu`の
    下で呼ぶので、固まった起動は#3709の前から停止を止めていた(`Supervisor.Shutdown`は
    `lockBefore(&s.genMu, b.drainUntil)`で見切って進む)。走っている起動を`runMu`で待つのは、
    `Server.StopBackground`を直接呼ぶもの(テスト)だけで、その待ちは停止のctxを見ない
  - **待ち合い(デッドロック)にならない。** 停止は印を立てる間だけ`runMu`を握り、部品やqueueを止めて
    待つ間は握らない。起動が`runMu`を握ったまま取るのは、一覧のmutex(`stopAll`は印を立てる間だけ握る)、
    chartの管理のmutex、queueのdriverの中のmutexで、停止の側が`runMu`を握ったままそれらを待つことは無い

**起動していない部品を止めても安全であること。** 構築に失敗した世代(`cleanupConstruction`)と、
`StartBackground`に失敗した世代(Supervisorの`StopBackground`)では、起動していない部品も止める。
部品ごとに次のとおり。

| 部品 | 起動していないときの停止 |
|---|---|
| `instanceTouchBuffer`・`sigCapBuffer`・`instanceCounterBuffer` | `Close()`は`started`の印が無ければ、loopを待たずに残りを同期でflushして返る(`doneCh`を待たない) |
| `deliveryHealth`・`inboxHealth` | `Stop(ctx)`は`cancel`がnilなら何もせずに返る |
| `serverStatsPub`・`queueStatsPub` | `Stop()`は`started`がfalseなら何もせずに返る |
| `emojiInvalidationQueue` | `Stop(ctx)`は起動していなければ`Run`を待たずに、溜まった分を同期で送って返る(#3705。バッファの`Close()`と同じ扱い)。送るのは締め切り(`drainTimeout`、2秒)と停止のctxの期限の早いほうまでで、1件ごとに送る前に見る。送信中の1件はpublishの側のtimeout(2秒)まで待つので、最悪で約4秒かかる(`router.go`のコメントの「最悪約4秒」と同じ) |
| `fedRuleHits` | 停止はctxをcancelするだけ。起動していなければ待つgoroutineも無い |
| worker の 4 つ(`startConstructionWorker`) | 停止はctxをcancelし、起動していなければ`done`を待たずに返る。`done`は起動しないと閉じないので、待つと停止のctxの期限まで固まる(後始末は`context.Background()`で止めるので、永久に返らない) |
| 起動を持たない部品(`metadataRefresher`・`hashtagService`・`paramShadowRecorder`・`pluginStorage:<名前>`) | 起動するものが無い。停止は#3703の前と同じ |

**意図して変えた動き(#3703)。**

- `elythia dump-routes`と、構築だけして起動しない使い方(テストの`newServer`など)では、上の12個が
  動かない。`dump-routes`の起動の間のRedisの命令の列とPostgreSQLの文の集合から、`queueStatsPub`の
  1回目のtick(deliverとinboxのqueueの状態を読んで`stream:queueStats`へpublishする)、
  `retentionAggregateAtStartup`(登録ユーザーと最近のユーザーを読み、その日の行をINSERTする)、
  `pollExpiryWorker`の1回目の走査(`poll`のSELECT)が消えた
- 本番の経路では、12個の起動が構築中のほかの副作用(起動時のDBの文、Redisの購読、ログ)より後になる。
  `serve`のログでは、`retentionAggregateAtStartup`が書くgormのログが、構築の途中のログ(param shadow
  の行の前)から、構築の最後のログ(frontendの警告)の後へ動いた。`pollExpiryWorker`の最初の走査も
  構築の後になったので、構築の途中で作ったpollEndedの通知が配線の途中のnotificationServiceを
  通ることは無くなった(`router.go`の`notificationService.SetMainStreamPublisher`の行のコメント)
- 構築の失敗の後始末では、起動していない部品を止める(上の表)
- 部品の起動のpanicは、前はプロセスごと落としていた。今は`StartBackground`のエラーになり、
  Supervisorが世代を止める。メンテナンスを抜けるときの作り直しでは、ほかの失敗と同じく
  `ProcResumeFailed`のまま、間隔を延ばしながら作り直しを繰り返す(`maintenance.go`の`resume`)
- プラグインの`ctx.Go`で保留したworkは、今までどおり`newServer`の最後(`pluginGoGate.open()`)で
  動き始めるので、12個の部品より先に動き始める順になった。プラグインのworkが一覧の部品へ書いても
  困らない。書き込みの口はどれも待たずに返り(バッファは溜めるだけ、`userCacheLink`の
  `Enqueue`は溜めて起こすだけ、`fedRuleHits`の`Record`は溢れたら捨てる、健全性はメモリの集計へ
  足すだけ)、溜めた分は部品の起動の後に流れる。queueへ積むジョブも、workerの起動を待って処理される

**意図して変えた動き(#3705)。**

- `elythia dump-routes`と構築だけの使い方では、`emojiInvalidationQueue`の`Run`のgoroutineが立たなく
  なった。`dump-routes`では構築の間に積まれる無効化が無く、起動の間のRedisの命令の列は変わらなかった
  (下の確かめ方)
- 本番の経路では、`Run`の起動が構築の後(`StartBackground`。一覧の中の位置は変えていない)になった。
  hook(`emojiRepo.SetInvalidationHook`)は前と同じ位置で繋ぐので、構築から`StartBackground`までに
  積まれた無効化は溜めておき、起動の後に送る
- 起動していない世代を止めるとき(構築や`StartBackground`の失敗の後始末)は、溜まった分を同期で送る
  (上の表)。#3705の前は構築の途中で`Run`が動いていたので、`Run`が送ってから返るのを待っていた
- 2回目の`Start`・`Stop`と、止めた後の`Start`は何もしない(部品の型が自分で守る。一覧の`startAll`も
  止め始めた後は起動しない)

**意図して変えた動き(#3707)。**

- 部品の停止のpanicは、前は呼び出し元を抜けてプロセスごと落としていた。後ろの部品の停止、chartの保存、
  queueのdriverと購読を閉じる処理は飛び、SIGTERMの停止でも、メンテナンスに入るときの停止でも、DBを
  閉じる前に落ちた。今は記録して残りを止め、`StopBackground`のエラーになる(下の「停止のpanic」)
- panicしない限り、停止の順も`StopBackground`の戻り値も変わらない

**意図して変えた動き(#3711)。**

- 一覧の外の停止の段(streaming、queue、箱、chart、遅れたqueueの後始末、queueのdriver、購読、echo)と、
  構築の後始末の段のpanicは、前は`StopBackground`(または`cleanupConstruction`)を抜けて、後の手順を飛ばして
  プロセスごと落としていた。今は記録して後の段を続け、`StopBackground`のエラーになる(下の「段のpanic」)
- panicしない限り、停止の順も`StopBackground`の戻り値も変わらない
- 一覧の部品の停止だけがpanicし、echoのエラーが無いときの戻り値は、前は`stopAll`のエラーを
  `errors.Join`でもう1段包んでいたが、今は`stopAll`のエラーそのものになる。`errors.Is`の結果と`Error()`の
  文字列は変わらない

**意図して変えた動き(#3709)。** どちらも本番の経路では起きない(Supervisorは`genMu`の下で、1つの
世代に`StartBackground`を1回だけ呼び、止めた世代を再び起動しない)。

- `StopBackground`の後(または構築の後始末の後)に`StartBackground`を呼ぶと、前は一覧だけを起動せず、
  queueのworker・autoscale・schedulerとchartは止めた世代のまま起動して成功を返した。起動したworkerを
  止める人は居らず、queueのdriverは閉じた後だった。今は何も起動せず、`errGenerationStopped`を返す
- `StartBackground`と`StartBackgroundForTest`を両方呼ぶと、前はqueueの`Start`を2回呼んだ
  (`StartBackgroundForTest`は`startOnce`を通らなかった)。今は先に呼んだ方だけが起動する
- 起動と停止を並行に呼んだとき、前は停止が止め終わった後に起動が走りえた。今は上の「停止の後の起動」の
  とおり、起動したものはすべて止まる

**Redisの購読は動かしていない。** streamの節で始める購読(`internalPubSub.Subscribe`の
`metaUpdated`など4つと、`cachebus`・`relationcache`の`Connect…`/`Subscribe…`)は、#3703の後も
構築の途中で始める。一覧に乗せなかった理由は3つ。

- 購読を閉じるのは一覧ではなく、queueのdriverの後の一覧の外の段(下の手順8の`pubsub`)で、pubsubごと閉じる。
  起動だけを一覧に乗せると、起動と停止が別の仕組みに分かれる
- 購読の多くは、他の部品への配線と1つの関数(`cachebus.ConnectUsers`など)で行っていて、購読だけを
  後で始めるには、それらの関数の形から変える必要がある
- 購読を始める時刻を動かすと、構築から`StartBackground`までの間に他のプロセスが送った通知
  (キャッシュの無効化、`metaUpdated`)を受け取らなくなる。受けるかどうかは、通知ごとに確かめる必要がある

一覧に乗せるかは、段階(6)でメンテナンスの作り直しの範囲を決めるときに、別に決める。

**世代の停止 (`shutdown`) は、一覧を次の位置で回す。** 順を決めている理由は #3662 と #3661。
停止は名前付きの段の表(`shutdownSteps`。#3711)を上から順に回す。段の名前は次の各行の先頭に書く。
表の前に、起動しない印(`stopped`)を立てる(#3709。上の「停止の後の起動」)。印は段の表の外に置くので、
段のpanicで飛ぶことは無い。

1. `streaming`: streamingの接続を1012で閉じる(別の上限付き。`streamCloseContext`)
2. `queue`: queueのworkerとschedulerを止める(`stopQueue`)。**一覧より先に止める**(#3662)。workerの
   ジョブはバッファとchartへ書くので、先にバッファを閉じると、止まるまでに終わったジョブの書き込みが
   flushされずに消える
3. `background`: 投げっぱなしのgoroutineの箱(`background`)を回収する(#3661)。queueの後に置くのは、
   受信の処理が止まるまで箱へ足され続けるため。一覧より前に置くのは、箱のgoroutineが書くバッファと
   chartの分をflushと保存に含めるため。queueが上限内に止まらなかったときは、ここでは何もせず、6で回収する
4. `lifecycle`: **一覧を登録した順(FIFO)に、停止のctxを渡して止める。** 後に登録した部品の停止が、
   先に登録した部品の閉じるものを使う組は無い(#3662で全件を確認)
5. `chart`: chartの管理のloopを止める(`chartMgmt.Stop`。最後の保存を含む)
6. `lateQueue`: queueが上限内に止まらなかったときは、ここで止まるのを待ち、箱を回収してからchartを
   もう一度保存する。止まっていれば何もしない
7. `queueDriver`: queueのdriverを閉じる
8. `pubsub`: この世代のRedisの購読(`internalPubSub` / `streamPubSub`)を閉じる
9. `echo`: echoをShutdownする

queueが止まったかどうか(2の結果)は、表を作る関数の中の変数に置き、3と6がそれを読む。

構築に失敗したときの後始末(`cleanupConstruction`)は、起動しない印を立て(#3709)、段の表(`cleanupSteps`)を
`context.Background()`で回す。段は`pluginGoGate`(プラグインの保留のworkを捨てる)、`lifecycle`(一覧を
同じ順に止める)、`background`(箱を1秒まで待つ)、`releaseParts`(一覧を空にする)、`queueDriver`(driverを
閉じてnilに戻す)、`streaming`(接続を1秒まで待って閉じる)、`pubsub`(購読を閉じる)の順。

**停止のpanic(#3707)。** 一覧の部品の停止は、どれも呼び出し元のgoroutineで同期に走る。`stopAll`は
部品ごとに停止をrecoverで包み、panicしたら部品の名前・panicの値・スタックを`slog.Error`で1件記録して、
次の部品の停止へ進む。panicした部品ごとに、名前を含み`errLifecycleStop`を包むエラーを作り、
`errors.Join`でまとめて返す(panicが無ければnil)。起動の側の`startAll`(#3703)と違い、後の部品も止める。
止めないと、その部品が流すもの(バッファのflushなど)が失われるため。

- `lifecycle`の段は`stopAll`のエラーを段のエラーとして返す。`shutdown`は、panicがあっても後の段
  (上の5〜9)を走らせ、段のエラーをまとめて返す(下の「段のpanic」)。panicが無ければechoのエラーを
  そのまま返す(#3707の前と同じ値)。`StopBackground`はそれを返す
- `cleanupConstruction`は、エラーを返す口が無いので、`stopAll`の記録だけにする(重ねて記録しない)。
  後の段(箱、queueのdriver、streaming、購読)は続く
- 停止のctxは、panicした部品の後もそのまま後の部品へ渡す。panicした部品が使った時間は、後の部品の
  時間から引かれる
- panicした部品は止め直さない。停止の途中でpanicした部品は、途中まで止まったまま残りうる(goroutineが
  残れば、DBを閉じた後に失敗する)。`StopBackground`は1回目だけ働く(`shutdownOnce`)ので、呼び直しても
  1回目のエラーを返すだけになる
- 停止がロックを握ったまま(`defer`で外さずに)panicすると、そのロックを待つ後の部品の停止や手順が
  止まりうる。#3707の前はプロセスごと落ち、立ち上げ直しは運用の側(composeの`restart`など)に
  任されていた。今はたとえばメンテナンスに入るとき(`genMu`を握ったまま止める`stopGeneration`)が、
  期限無しに`ProcDraining`のまま止まりうる。SIGTERMの停止は、`Supervisor.Shutdown`の期限と
  `lockBefore`で打ち切られる。今の部品の停止では、ロックを握ったままpanicしうる区間(バッファの
  flushのDBへの書き込み、配送の健全性の集計の読み出しなど)は、どれもロックを`defer`で外すので、
  この組み合わせは無い(#3707で各部品の停止を読んで確かめた)
- 回収するのは停止を呼んだgoroutineのpanicだけで、停止が立てたgoroutineの中のpanicは回収しない
- 一覧の外の停止の手順(streamingの`CloseAll`、`stopQueue`、`background.Stop`、`chartMgmt.Stop`、
  `queueDriver.Close`、pubsubの`Close`、echoの`Shutdown`)のpanicは、#3707の時点では回収せず、一覧に
  入れる段階で扱うとしていた(#3666)。#3711で、一覧には入れないまま(下の「一覧の外の段」)、段ごとに
  回収する形にした(下の「段のpanic」)

**段のpanic(#3711)。** 停止の段は、どれも呼び出し元のgoroutineで同期に走る。段を回す関数
(`runShutdownSteps`。`internal/server/shutdown_steps.go`)は、段ごとにrecoverで包み、panicしたら段の名前・
panicの値・スタックを`slog.Error`で1件記録して(`stopAll`と同じ形)、次の段へ進む。

- panicした段ごとに、名前を含み`errShutdownStep`を包むエラーを作る。段が返したエラー(`lifecycle`の段の
  `stopAll`のエラー、`echo`の段の`Shutdown`のエラー)と合わせて、段の順に`errors.Join`でまとめて返す。
  失敗した段が1つだけなら、そのエラーを包まずに返す。panicが無い停止の戻り値(echoのエラーか、nil)は、
  #3711の前と同じ値になる
- `cleanupConstruction`は、段のエラーを捨てる。段のpanicは`runShutdownSteps`の記録だけ、部品の停止の
  panicは`stopAll`の記録だけにする(重ねて記録しない)
- 停止のctxは、panicした段の後もそのまま後の段へ渡す。panicした段は止め直さない。途中までしか
  済んでいないことがある(たとえば`queueDriver`の段で`Close`がpanicすると、後始末の側ではdriverを
  nilに戻さない)
- **queueの段がpanicしたとき。** `stopQueue`が値を返していないので、workerが止まったかは分からない。
  止まらなかったものとして扱う。`background`の段は何もせず、`lateQueue`の段が一覧とchartの後で箱を
  回収し、chartをもう一度保存する。止まったものとして扱うと、止まっていないworkerが箱へ足した分を
  捨てうるため。待つchannelが無いので、`lateQueue`の段はqueueを待たない(nilのchannelを待つと永久に
  止まる)。`stopQueue`が裏で立てるgoroutine(autoscale・scheduler・workerの停止)の中のpanicは、
  一覧の部品の停止と同じく回収しない(プロセスごと落ちる)
- 停止がロックを握ったままpanicしたときに後の停止が止まりうること(上の部品の停止の項)は、段にも
  同じことが言える

Supervisorの側は変えていない。停止のエラーの使われ方は次のとおり。

- `stopGenerationLocked`は、今までどおり`StopBackground`のエラーをDBを閉じるエラーと合わせて
  `StopReport.Err`に載せ、その後でDBを閉じる。メンテナンスに入るとき(`maintenance.go`の`enter`)は、
  エラーがあっても`ProcDrained`にし、報告の`Error`にその文字列を載せるだけで、メンテナンスを抜ける
  ときの作り直し(`resume`)は止めたときのエラーを見ない
- `Supervisor.Shutdown`は、期限内に終わったときだけエラーに含めて返す。期限が切れて背景処理の停止を
  呼んだ後に返るときは`ctx.Err()`だけを返し、panicは`stopAll`と`runShutdownSteps`の記録にだけ残る。`serve`は返ったエラーを
  `server shutdown error`として記録する。
  Redisを閉じずに残すのは期限切れ・取り消しのエラーを含むときだけ(`keepRedis`)で、終了コードは
  このエラーで変わらない
- `startGenerationLocked`(`StartBackground`に失敗した世代)は、今までどおり停止のエラーを捨てる

**一覧の外の段(上の1〜3・5〜9)は、一覧に入れていない。** 動きを変えずには入れられないため。
#3711で段の表にした後も、一覧とは別の表に置いている。

| 段 | 入れない理由 |
|---|---|
| `streaming` (`CloseAll`) | `shutdown` では queue より前 (一覧より前) に、`cleanupConstruction` では driver の後に置いていて、2 つの経路で位置が違う。待ちの上限も一覧と別 (`streamCloseContext`) |
| `queue` (`stopQueue`) | 止まったかどうかを返し、それで箱の回収と chart の再保存の位置 (3 と 6) が変わる。待ちは裏で続く |
| `background` / `lateQueue` (`background.Stop`) | queue が止まったかどうかで、一覧の前か 6 の後かが変わる。後始末では一覧の後 |
| `chart` (`chartMgmt.Stop`) | 起動するのは `StartBackground` で、構築の途中ではない。後始末では止めない (起動していない) |
| `queueDriver` (`queueDriver.Close`) | エラーを返し、6 で止まらなかった worker を待った後に閉じる。後始末では nil に戻す |
| `pubsub` (pubsub の `Close`) | 購読は stream の節で始めるが、閉じるのは driver の後。一覧に入れると、一覧より後に止める 5〜7 より先に閉じることになる |
| `echo` (echo の `Shutdown`) | 戻り値が `StopBackground` の戻り値になる(ほかの段が失敗していれば、そのエラーと`errors.Join`でまとめる。#3707、#3711) |

**テストが見ている範囲は次のとおり** (`internal/server/lifecycle_parts_test.go`・
`lifecycle_parts_value_test.go`・`lifecycle_start_test.go`・`lifecycle_startonce_test.go`・`lifecycle_stop_test.go`・`lifecycle_steps_test.go`。多くは実際に`newServer`で作ったサーバーで見る)。部品を足す・動かすときは、テストの表とこの節の表を一緒に直す。

- **名前の並びと登録の条件。** `TestNewServer_ShutdownStopsThePartsInTheirOrder` が、role (両方・
  server・queue) と設定 (queueInspector の有無、`disableParamShadow`、DB の store を持つプラグインの
  有無) の 7 つの組で、一覧の名前の並びを固定する。`StopBackground` が一覧の停止の関数を、その順に、
  停止の ctx を渡して全部呼ぶことも見る
- **起動の順と回数(#3703)。** 同じテストが7つの組で、構築の後に一覧の起動の印(`lifecycle.started`)が
  立っていないことと、`StartBackground`を2回呼んでも、起動を持つ部品(表`partsStartedByStartBackground`の13個のうち、その組で
  登録されるもの)が一覧の順に1回ずつ起動されることを見る。起動を持つ部品の集合が表とずれても落ちる
- **構築だけでは部品が動かないこと(#3703)。** 一覧の起動を差し替えて見る上の検査は、一覧を通らずに構築の
  途中で起動する書き方を落とせない。`TestNewServer_ConstructionStartsNoPart`が両方のroleで、部品が回す
  goroutine(スタックの関数名で数える。`emojiInvalidationQueue`の`Run`は#3705で足した)とretentionの
  集計が書く行を見て、構築の後には増えず、`StartBackground`の後に増えることを確かめる
- **起動の失敗と、起動していない部品の停止(#3703)。** `TestSupervisor_FailedStartStopsEveryPart`が、
  Supervisorの作り直し(`startGeneration`)で一覧の中ほどの部品の起動をpanicさせ、エラーが返ること、
  一覧の全部の部品(起動していないworkerを含む)がその順に止まること、停止が`DrainTimeout`まで待たずに
  返ること、世代のDBの接続とRedisの購読が残らないことを見る。`TestLifecycle_StartAllRecoversAPanickingStart`・
  `TestLifecycle_StartAllStartsNothingAfterStopAll`・`TestServer_ConstructionWorkerRunsOnlyOnceStarted`が、
  panicの回収、止め始めた後の起動、workerの起動と停止を一覧の単位で見る
- **queueとの前後(#3703)。** `TestServer_StartBackgroundStartsThePartsBeforeTheQueue`が、
  `StartBackground`と`StartBackgroundForTest`のどちらも、一覧の部品を一覧の順に、queueのworkerの
  `Start`より先に起動することを見る(queueの起動を記録する差し替えのdriverを使う)。
  `TestServer_FailedPartStartDoesNotStartTheQueue`が、部品の起動が失敗したら後の部品もqueueも
  起動しないことを見る
- **値の型。** 同じテストが、各部品の `part` の動的な型を表 (`lifecyclePartTypes`) と照合する。
  名前が別の種類の値を持つ取り違え (`serverStatsPub` に `hashtagService` を渡すなど) は落ちるが、
  **同じ型の部品どうしの名前の取り違えは落とせない**。`deliveryHealth` と `inboxHealth` はどちらも
  `*deliveryhealth.Service` で、`startConstructionWorker` の 4 つはどれも nil。値と停止の取り違えは、
  値から作る10個では書けない(上の「同じ型どうしの取り違えを書く余地が無くなった範囲」)
- **値の同一性と、登録の経路。** 同じテストが、nil でない `part` どうしが互いに異なる値であることと、
  形を満たす値が `addPart` を経ていないこと (上の許可リストの検査) を見る。値は同一性で比べるので
  ポインタに限り、**ポインタでない値を持つ部品は、それだけで落ちる** (値の型の部品を足すときは、
  比べ方から見直す)。2 つの検査は違反の一覧を返す関数 (`sharedPartValues`・`addPartShapeViolations`) で、
  `TestPartChecks_ReportViolations` が手で組んだ一覧で違反を報告すること (正の対照) を固定する
- **値から作る登録。** `TestServer_RegistersAPartFromItsValue` が 5 つの登録それぞれについて、次を見る。
  登録では起動せず、`startAll`が1回だけ、一覧の順に、登録で渡したctxで起動すること(#3703)。`part` が渡した値そのもので
  あること。止めるとその値のメソッドが 1 回呼ばれ、停止の ctx を `Stop(ctx)`・`Close(ctx)` には渡し、
  ほかでは捨てること。`Close() error` のエラーを一覧のエラーにしないこと。同じ型の部品を 2 つ登録し、
  それぞれが自分の値を止めることも見る
- **名前どおりの値を止めたこと。** `TestNewServer_ShutdownStopsTheNamedParts` が、`StartBackground`で起動してから
  (#3703)、`StopBackground` の後に、次の部品が実際に止まったことを観測する。`instanceTouchBuffer`・`sigCapBuffer`・
  `instanceCounterBuffer` (閉じた後の書き込みは、溜めずにその場で DB へ書く)、`hashtagService`
  (書き込み中の worker を表の lock で止めておくと、`Shutdown(ctx)` が期限まで待って警告を出す)、
  `pluginStorage:<名前>` (閉じた後は DB のプールに Ping できない)。停止が ctx を無視する退行が
  入ると hashtag の worker を待ち続けるので、`StopBackground` を 10 秒で打ち切って落とす
- **それ以外の部品** (`pollExpiryWorker`・`fedRuleHits`・`metadataRefresher`・`reactionFlushWorker`・
  `deliveryHealth`・`inboxHealth`・`retentionAggregateAtStartup`・`paramShadowRecorder`・
  `emojiInvalidationQueue`・`userCacheLink`・`serverStatsPub`・`queueStatsPub`) は、その名前の停止の
  関数が呼ばれることまでしか見ていない。停止の関数が名前どおりの値を止めているかは、外から観測する
  口が無く、テストしていない (`paramShadowRecorder` の停止が要約を書くことは
  `TestParamShadowRunsAfterTheRouteMiddleware` が別に見る)。`emojiInvalidationQueue`の`Start`/`Stop`
  そのものの動き(起動の前に積んだ分を起動の後に送る、起動していない`Stop`が`Run`を待たずに溜まった
  分を締め切りか停止のctxの期限まで送る、2回目の`Start`/`Stop`と止めた後の`Start`は何もしない)は、
  `internal/repository`の`TestEmojiInvalidationQueue_*`が見る(#3705)
- 後始末が一覧を順に回し、起動していないworkerを待って固まらないことは
  `TestNewServer_FailedConstructionStopsTheRegisteredParts`、重複で構築が
  失敗することは `TestNewServer_DuplicatePartFailsTheConstruction` が見る。`addPart` が部品に値から
  作った印を付けず、形を満たす値も拒まないことは `TestServer_AddPartDoesNotMarkThePartAsFromValue` が見る
- **停止のpanic(#3707)。** `TestLifecycle_StopAllRecoversAPanickingStop`が、2つの部品の停止をpanicさせ、
  後の部品も同じctxで順に止まること、エラーがpanicした部品ごとに1つで、どれも`errLifecycleStop`を包み
  名前とpanicの値を含むこと、部品の名前・panicの値・スタックが記録されることを見る。
  `TestServer_StopBackgroundGoesOnAfterAPanickingStop`が、一覧の中ほどの部品の停止がpanicしても、
  後の部品の停止とqueueのdriverを閉じる処理が走り、Redisの購読が閉じることと、`StopBackground`の
  エラーが部品の名前を含むことを見る。`TestServer_StopBackgroundJoinsThePanicWithTheEchoError`が、
  echoの`Shutdown`のエラーが残ることと、panicが無ければechoのエラーをそのまま返すことを、
  `TestServer_StopBackgroundWithoutAPanic`がpanicの無い停止がnilを返すことを見る。
  `TestServer_FailedConstructionCleanupGoesOnAfterAPanickingStop`が、構築の後始末が最後まで走り、
  記録を重ねないことを見る。`TestSupervisor_StopGenerationGoesOnAfterAPanickingStop`が、`newServer`で
  作った世代の2つの部品の停止をpanicさせ、Supervisorが全部の部品をその順に止め、`StopReport.Err`に
  載せ、世代のDBの接続とRedisの購読を残さず、次の世代を作れることを見る
- **1度きりの起動と、停止の後の起動(#3709)。** `lifecycle_startonce_test.go`が、queueのworkerと
  schedulerの`Start`/`Shutdown`を記録し、autoscaleの`Resize`を数える差し替えのdriverと、chartの
  管理の`Start`が起動中に失敗することで、起動したものを見る。`TestServer_StartAfterStopStartsNothing`が、
  `StopBackground`の後の`StartBackground`と`StartBackgroundForTest`が何も起動せずに
  `errGenerationStopped`を返すことを、`TestServer_StartAfterFailedConstructionStartsNothing`が構築の
  後始末の後も同じことを見る。`TestServer_StartBackgroundAndForTestShareTheOnce`が、2つの起動を
  どちらの順で呼んでもqueueの`Start`が1回で、後の呼び出しが先の結果(失敗なら同じエラーの値)を返す
  ことを、`TestServer_StartBackgroundForTestIgnoresTheRole`が引数で分けた2つの違い(roleとchart)を見る。
  `TestServer_ConcurrentStartAndStopLeaveNothingRunning`が、起動と停止を並行に300回呼び、起動した
  部品・worker・schedulerがその後に止まり、autoscaleとchartが残らず、固まらないことを見る(`-race`で
  `autoscale`のフィールドの競合も見る)。組ごとに先に譲る側を替え、起動が先の組と停止が先の組の両方が
  起きたことも確かめる(片方だけでは、もう片方の順の検査が空振りする)
- **停止の段(#3711)。** `lifecycle_steps_test.go`が見る。`TestServer_ShutdownStepsAreInTheirOrder`が、
  `shutdownSteps`と`cleanupSteps`の段の名前と順を固定する。`TestRunShutdownSteps_RecoversAPanickingStep`が、
  2つの段をpanicさせ、1つの段にエラーを返させて、後の段も同じctxで順に走ること、エラーが段の順に3つで、
  panicしたものは`errShutdownStep`を包み名前とpanicの値を含むこと、段の名前・panicの値・スタックが記録
  されることを見る。`TestRunShutdownSteps_ReturnsASingleErrorAsIs`が、失敗した段が1つならそのエラーを
  包まずに返すことを見る。`TestServer_StopBackgroundGoesOnAfterAPanickingStep`が、`queueDriver`の段と
  一覧の部品をpanicさせ、後の段(購読を閉じる、echoの`Shutdown`)が走り、`StopBackground`のエラーが段の
  名前・部品の名前・echoのエラーを含むことを見る。`TestServer_ShutdownAfterAPanickingQueueStep`が、
  `queue`の段をpanicさせても後の段が固まらず、箱の回収が一覧の後(`lateQueue`の段)になることを、
  panicしない組を対照にして見る。`TestServer_FailedConstructionCleanupGoesOnAfterAPanickingStep`が、
  構築の後始末の`queueDriver`の段がpanicしても後の段が走り、panicが1回だけ記録されることを見る

#3699 の前後で同じ動きであることは、次のように確かめた (道具はコミットしていない)。

- 停止の順: #3699 の前の `registerShutdownHook` に、登録した呼び出し元の行を記録する使い捨ての
  変更を当て、8 つの組 (上のテストの 7 つの組に、両方の role で queueInspector が無い組を足したもの。
  プラグインの組はどちらも 2 つのプラグインで) で、`StopBackground` が hook を呼んだ順を記録した。行と名前の対応表で名前に
  直すと、どの組も #3699 の後の名前の並びと一致した。停止にかかった時間も、どの組も 10ms 未満で
  前後に差は無かった
- `elythia dump-routes` の出力、起動の間の Redis の命令の列と起動ログ、PostgreSQL の文の集合は、
  前後で一致した
- 4.1 の道具の数は変わらない。#3676 の seq の道具は `registerShutdownHook` を副作用の文として
  拾うので、後の `router.go` の `s.addPart("<名前>", <値>, ` を `s.registerShutdownHook(` に、
  `startConstructionWorker` の名前の引数を消して戻すと、前と一致した

#3701 の前後で同じ動きであることは、次のように確かめた (道具はコミットしていない)。

- 起動と停止の順: 前後の木の両方に同じ使い捨ての変更を当てた。値から作る 9 つの部品と `fedRuleHits` の
  `Start` / `Stop` / `Close` の先頭で、受け手のポインタと受けた ctx の型を記録する。同じ記録に、`newServer`
  に渡す Redis の client の命令 (go-redis の hook) と gorm の SQL (logger) と、`StopBackground` が呼んだ
  一覧の停止の名前を足す。ポインタは構築の後に、一覧の `part` で名前に直した。#3699 と同じ 8 つの組で、
  構築の goroutine の列 (起動と、その前後の SQL・Redis の命令の相対的な位置を含む) と、停止の列
  (停止の名前、各部品の `Stop` / `Close` と受けた ctx の型) は前後で一致した。構築の途中で起動した
  worker の SQL と Redis の命令は、時刻で混ざり方が変わるので、並べ替えた集合で比べて一致した。
  前の木を 2 回回して、比べ方が前の木どうしで揺れないことも確かめた
- `elythia dump-routes` の出力、起動の間の Redis の命令の列と起動ログ、PostgreSQL の文の集合は、
  前後で一致した
- 4.1 の道具の数は変わらない。#3676 の seq の道具は `X.Start(...)` を副作用の文として拾い、値から作る
  登録は拾わない。後の `router.go` の `s.startPartWithClose(<ctx>, "<名前>", X)` と
  `s.startPartWithStop(<ctx>, "<名前>", X)` を `X.Start(<ctx>)` に、`s.startPartWithoutCtx("<名前>", X)` を
  `X.Start()` に戻すと (7 行)、前と一致した

#3703の前後で、意図した変化(上の「意図して変えた動き」)のほかは同じであることは、次のように
確かめた(道具はコミットしていない)。

- 起動と停止の順: 前後の木の両方に、#3701と同じ使い捨ての記録(値から作る9つの部品と`fedRuleHits`の
  `Start`/`Stop`/`Close`の先頭)と、`startConstructionWorker`がgoroutineを立てる位置の記録を当てた。
  #3699と同じ8つの組で、`newServer`・`StartBackground`・`StopBackground`のそれぞれの間に起動した部品と
  停止の列を記録した。前の木で構築の間に起動した部品(組により7〜12個)は、後の木では構築の間に1つも
  起動せず、`StartBackground`の間に同じ順で起動した。停止の列(一覧の停止の名前と、各部品の`Stop`/`Close`
  と受けたctxの型)は前後で一致した。どちらの木も2回回して揺れないことを確かめた
- `elythia dump-routes`: 出力(ルート)と起動ログは一致した。Redisの命令の列とPostgreSQLの文の集合の差は、
  上の「意図して変えた動き」に書いた`queueStatsPub`・`retentionAggregateAtStartup`・`pollExpiryWorker`の
  分だけだった(前の木どうしでは差が無い)
- `serve`: 隔離したPostgreSQLとRedisに向けて、前後のバイナリをUNIXソケットで25秒ずつ2回動かし、
  SIGTERMで止めた。分の境目をまたがないよう、毎分2秒に始めた。PostgreSQLの文の集合と、Redisの
  命令とキーの組の集合は、前後の4通りの組み合わせのどれでも一致した(値まで含めた集合は、ジョブの
  時刻・msgpackのキーの順・worker名で、前の木どうしでも揺れる)。起動ログは、
  `retentionAggregateAtStartup`のgormのログの位置が動いた(上の「意図して変えた動き」)ほかは一致した
- 4.1の道具の数は変わらない(4.1)。#3676のseqの道具の列は、`fedRuleHits.Start(hitsCtx)`の行が
  `s.addPartWithStart(...)`に変わった1行だけが違う

#3705の前後で、意図した変化(上の「意図して変えた動き(#3705)」)のほかは同じであることは、次のように
確かめた(道具はコミットしていない)。

- 起動と停止の順: #3703と同じ使い捨ての記録を前後の木に当て、`emojiInvalidationQueue`の起動(`Run`に
  渡すctxの型)と停止(受けたctxの型)の記録を足した(前の木は`setupRoutes`の`go`文とclosure、後の木は
  `Start`/`Stop`)。#3699と同じ8つの組で、前の木で構築の間に起動したのは`emojiInvalidationQueue`だけ、
  後の木では構築の間に起動した部品は無く、`StartBackground`の間の起動の列は、前の木の列に
  `emojiInvalidationQueue`を一覧の位置に足したものと一致した(`Run`に渡すctxの型は前後とも
  `*context.cancelCtx`)。停止の列は前後で一致した。どちらの木も2回回して揺れないことを確かめた
- `elythia dump-routes`: 出力(ルート)、起動ログ、Redisの命令の列、PostgreSQLの文の集合は、前後で一致した
- `serve`: #3703と同じ方法(前後のバイナリを25秒ずつ2回)で、PostgreSQLの文の集合、Redisの命令とキーの
  組の集合、起動ログは、前後の4通りの組み合わせのどれでも一致した
- 4.1の道具: 節点は361から358に、呼び出しが離れている辺は660から659になった(4.1)。#3676のseqの道具の
  列は、消した`go`文の1つだけが違う(`addPart`と`startPartWithStop`はどちらも拾わない)

#3707の前後で、panicしない限り同じであることは、次のように確かめた(道具はコミットしていない)。

- 起動と停止の順: #3705と同じ使い捨ての記録(`emojiInvalidationQueue`の`Start`/`Stop`を含む)を前後の木に
  当て、#3699と同じ8つの組で、`newServer`・`StartBackground`・`StopBackground`のそれぞれの間の起動と
  停止の列を記録した。8つの組のどれも、前後で一致した。どちらの木も2回回して揺れないことを確かめた
- `elythia dump-routes`: 出力(ルート)、起動ログ、Redisの命令の列、PostgreSQLの文の集合は、前後で一致した
- `serve`: #3703と同じ方法(前後のバイナリを25秒ずつ)で、PostgreSQLの文の集合と、Redisの命令とキーの
  組の集合は、前の木の2回と後の木の2回の4通りの組み合わせのどれでも一致した。起動ログの差は、gormの
  ログに出る呼び出し元のファイルのパス(ビルドした木の置き場所)だけだった。前の木の最初の1回は、
  空のDBへの初回の起動(`meta`と`instance_secret`の行を作る)なので、比べる対象から外した
- 4.1の道具は、`router.go`を変えていないので数が変わらない

#3709の前後で、本番の経路(構築 → `StartBackground` → `StopBackground`を1回ずつ)の動きが同じであることは、
次のように確かめた(道具はコミットしていない)。意図して変えた動き(上の「意図して変えた動き(#3709)」)は、
上のテストが見る。

- 起動と停止の順: #3707と同じ使い捨ての記録を前後の木に当て、#3699と同じ8つの組で、`newServer`・
  `StartBackground`・`StopBackground`のそれぞれの間の起動と停止の列を記録した。8つの組のどれも、前後で
  一致した。どちらの木も2回回して揺れないことを確かめた
- `elythia dump-routes`: 出力(ルート)、起動ログ、Redisの命令の列、PostgreSQLの文の集合は、前後で一致した
- `serve`: #3703と同じ方法(前後のバイナリを25秒ずつ2回。前の木の初回の起動を1回先に流し、比べる対象から
  外した)で、PostgreSQLの文の集合と、Redisの命令とキーの組の集合は、前の木の2回と後の木の2回の4通りの
  組み合わせのどれでも一致した。起動ログの差は、#3707と同じくgormのログに出る呼び出し元のファイルの
  パス(ビルドした木の置き場所)だけだった
- 4.1の道具は、`router.go`を変えていないので数が変わらない

#3711の前後で、panicしない限り同じであることは、次のように確かめた(道具はコミットしていない)。

- 起動と停止の順: #3709と同じ使い捨ての記録に、一覧の外の停止の手順の記録(streamingの`CloseAll`、
  `stopQueue`、箱の`Stop`/`StopWithin`、chartの`Stop`/`SaveAll`、pubsubの`Close`の先頭と、差し替えた
  queueのdriverの`Close`)を足して前後の木に当て、#3699と同じ8つの組で、`newServer`・`StartBackground`・
  `StopBackground`のそれぞれの間の起動と停止の列を記録した。8つの組のどれも、前後で一致した。どちらの
  木も2回回して揺れないことを確かめた。echoの`Shutdown`は外のパッケージなので記録していない
- `elythia dump-routes`: 出力(ルート)、起動ログ、Redisの命令の列、PostgreSQLの文の集合は、前後で一致した
- `serve`: #3709と同じ方法で、PostgreSQLの文の集合と、Redisの命令とキーの組の集合は、前の木の2回と後の木の
  2回の4通りの組み合わせのどれでも一致した。起動と停止のログの差は、#3707と同じくgormのログに出る
  呼び出し元のファイルのパス(ビルドした木の置き場所)だけだった
- 4.1の道具は、`router.go`を変えていないので数が変わらない

コメントの「shutdown hook」は、この一覧の部品の停止を指す。

## 5. フック注入パターン

import cycle を避けつつクロスカットを注入する Elythia 固有パターン（TS は NestJS DI + GlobalEventService）。例: `noteCreateService`:

```
noteCreateService
  ├→ SetFanoutHook(timelineFanoutHook)     ← タイムライン配信
  ├→ SetFederationHook(noteDeliveryHook)    ← AP 配信
  ├→ SetNotificationHook(notificationHook)  ← 通知生成（CreateWithPush で push を guard 経由）
  ├→ SetWebhookHook(noteCreateWebhookHook)  ← Webhook
  ├→ SetChartHook(chartHooks)               ← 統計
  └→ SetIndexHook(noteIndexHook)            ← 検索 index
```

hook: Fanout / Federation / Notification / Webhook / Chart / Index / BlockingChecker。

## 6. Redis 分離

用途別に複数 Redis 接続を論理分離（設定で同一ホストでも別クライアント扱い）。

| 用途 | 設定キー | 既定 |
|---|---|---|
| 汎用キャッシュ | `redis` | 必須 |
| PubSub | `redisForPubsub` | `redis` fallback |
| ジョブキュー | `redisForJobQueue` | `redis` fallback |
| タイムライン | `redisForTimelines` | `redis` fallback |
| リアクション | `redisForReactions` | `redis` fallback |

## 7. parity 品質ゲート（互換性の moat）

wire 互換を機械的に守る多層防御。upstream は **official `misskey/misskey` Docker image** を使うため、`frontend/`（自フロント fork）の改造とは独立して機能する。

| 仕組み | 内容 | docs |
|---|---|---|
| golden / shapetest | misskey-js autogen 型に対する応答 shape 突合（`internal/entitycompat`） | shape-drift.md |
| drift detector | CanSeeNote ↔ SQL push-down 等のロジック整合検出 | shape-drift.md |
| 値レベル diff harness | TS インスタンス ↔ Elythia の応答を値単位で diff（`make diff-test`） | diff-e2e.md |
| drop-in e2e | 実 Misskey TS ↔ Elythia 切替の連合/フロント互換（PR ごと。frontend e2e のみ nightly） | dropin-e2e.md / dropin-frontend-e2e.md |
| playwright | 298 spec を Elythia backend で（PR ごと、4 シャード）。TS backend は手動 | — |
| inbound/outbound 連合 | Fedibird-like mock との Ed25519 双方向 verify | federation.md |

CI（`ci.yml`）は build / 4-shard test（パッケージ毎 90% カバレッジ強制）/ lint を必須化。

## 8. Elythia 独自・cherrypick 拡張

upstream に無い、または cherrypick 由来の加算機能（wire 互換を壊さない additive 拡張）。
下表は概観で、**全項目の網羅カタログは [divergence.md](divergence.md)** を参照。

| 機能 | 系統 | 備考 |
|---|---|---|
| password hash の Argon2id 受理 | cherrypick | signin 成功後に bcrypt へ段階移行。[divergence/security.md](divergence/security.md) |
| federated chat | vanilla chat shape + yojo-art/cherrypick 連合 | 1-on-1 DM を `Create+Note(_misskey_talk:true)` で AP 配送 |
| federated reversi | vanilla reversi + cherrypick 連合対戦 | crc32 等は packed schema 外（連合 verify 用） |
| Ed25519 / Multikey (FEP-521a) | 連合拡張 | RSA に加え Ed25519 署名 |
| `_misskey_*` AP 拡張 | Misskey 系共通 | renderPerson 等で常時出力 |

> 注: chat / reversi は vanilla Misskey 2026.x にも存在する。Elythia が持つのは「vanilla 基盤 + cherrypick 由来の連合拡張」であり、vanilla misskey-js golden で厳密 gate するのは不適切（拡張 field を許容）。

## 9. 設定・マイグレーション

設定解決:

```
.config/default.yml → Viper → Source 構造体(生値) → resolve() → Config 構造体(解決済) → 各サービス
```

`MK_` プレフィックスの環境変数でオーバーライド可（例 `MK_DB_HOST`）。詳細は [configuration.md](configuration.md)。

マイグレーション（`migration/`、golang-migrate、現在 133 本）:

- TS Misskey の既存テーブルへは原則**追加のみ**。例外が 20 件あり、うち 15 件は Elythia が自分で作ったものの除去・初期化か upstream 追随 ([TS版からの移行](migration-from-ts.md#破壊的なマイグレーション))。Go 固有の追加列・テーブルは `IF NOT EXISTS`。
- drop-in テストで発見した補完列は専用マイグレーションで追加。
- down スクリプトは必須（data loss する場合は `-- data loss:` で明記する）。**ただしこれは今後の規約で、既存の down は守れていない** — 宣言があるのは 29 本だけで、宣言が無いまま `DROP TABLE` / `DROP COLUMN` する down が 51 本ある（[migration-from-ts.md](migration-from-ts.md#elythia-内での切り戻し)）。

```bash
make migrate-up      # 最新まで
make migrate-down    # 1 段ロールバック (本体の系列)
make migrate-create  # 新規作成

# 全段ロールバック (破壊的。全テーブルが消える)。down は -track が必須
go run ./cmd/elythia migrate -direction down -track core
```

fork が独自の migration を足すときは、本体とは別の系列 `migration/local/` (管理表 `schema_migrations_local`) に置く。`elythia migrate -direction up` は本体の後にそれを流す ([fork の独自 migration](fork-migrations.md)、#3428)。

## 技術スタックとディレクトリ構成 (旧 CLAUDE.md Section 1 / 2)

CLAUDE.md の Section 1 / 2 にあった表とツリーを、#3248 でここへ移した。**記述は当時のまま**で、文中の「Section N」は当時の CLAUDE.md の節を指す。最新の技術スタックは CLAUDE.md の Section 1 を見ること (例: testcontainers は今は Redis 用で、PostgreSQL は外部のものを使う)。

### 技術スタック

#### コア

| Component | Library | 用途 |
|-----------|---------|------|
| 言語 | **Go 1.27** | `go.mod`でバージョン管理 |
| Webフレームワーク | **Echo v4** (`labstack/echo/v4`) | HTTPルーティング、ミドルウェア、WebSocket |
| ORM | **GORM** (`gorm.io/gorm`) | PostgreSQLアクセス |
| Migration | **golang-migrate** (`golang-migrate/migrate/v4`) | SQLベースのマイグレーション |
| Config | **Viper** (`spf13/viper`) | YAML + 環境変数オーバーライド |
| Logging | **slog** (標準ライブラリ) | 構造化ロギング |

#### インフラ

| Component | Library | 用途 |
|-----------|---------|------|
| PostgreSQL Driver | **pgx/v5** (`jackc/pgx/v5`) | PostgreSQL接続 |
| Redis | **go-redis v9** (`redis/go-redis/v9`) | キャッシュ、PubSub |
| Job Queue | **mkq** (`elythia-network/mkq`) | BullMQ wire互換のRedisジョブキュー。**唯一のdriver** (legacyの`asynq`は#2985で削除) |
| Search | **meilisearch-go** | Meilisearch連携 |
| Object Storage | **aws-sdk-go-v2/s3** | S3互換ストレージ |

#### 連合 / ActivityPub

- **HTTP Signatures**: 自前実装（`internal/activitypub/`）
- **JSON-LD**: `piprate/json-gold` (LD-Signature の canonicalize。`internal/activitypub/ld/`)
- **ActivityStreams Types**: カスタム構造体

#### 認証

- **bcrypt** (`golang.org/x/crypto/bcrypt`) - パスワードハッシュ
- **pquerna/otp** - TOTP（2FA）
- **go-webauthn/webauthn** - パスキー / セキュリティキー（2FA、`signin-with-passkey`）

#### テスト

- **testing** (標準) + **testify** (`stretchr/testify`)
- **testcontainers-go** - 実PostgreSQL/Redisを使った統合テスト
- 単体テストでは`internal/testutil/`のモックを使用

### ディレクトリ構成

```
/
├── cmd/
│   └── elythia/            # 実行バイナリ。internal/cli を呼ぶだけ
├── internal/               # 全31ディレクトリ (`git ls-tree -d HEAD internal/ | wc -l`)
│   ├── cli/                # elythia のサブコマンド (serve / migrate / backfill <名前> / backup <名前> / maintenance <enter|exit|wait|status> / doctor / fsck / config-dump / healthcheck / dump-routes)
│   ├── backup/             # DB のバックアップ (`elythia backup`、#3457)。保存先 (S3 互換 / ディレクトリ)、世代の置き方、pg_dump での取得
│   ├── config/             # 設定ローダー（Misskey YAML互換）
│   ├── db/                 # GORM の PostgreSQL 接続配線
│   ├── server/             # HTTPサーバーのセットアップ、ルーティング、ミドルウェア
│   ├── api/                # APIハンドラ（エンドポイント単位でサブディレクトリ）
│   │   ├── admin/          # admin/* 管理API
│   │   ├── ap/             # ap/* ActivityPub解決API
│   │   ├── auth/           # auth/* 認証API
│   │   ├── notes/          # notes/* ノート関連API
│   │   ├── users/          # users/* ユーザー関連API
│   │   ├── i/              # i/* 自アカウントAPI
│   │   ├── drive/          # drive/* ファイル管理API
│   │   ├── federation/     # federation/* 連合情報API
│   │   └── ...             # その他エンドポイント群
│   ├── core/               # ビジネスロジック層（サービス）
│   ├── activitypub/        # ActivityPub実装（Inbox、Deliver、Renderer、Resolver、HTTP署名、LD-Signature）
│   ├── model/              # DBモデル（GORM、Misskeyエンティティ対応）
│   ├── repository/         # データアクセス層
│   ├── queue/              # ジョブキュー（mkq）とプロセッサ
│   ├── stream/             # WebSocketストリーミング（チャンネル実装）
│   ├── entity/             # レスポンス用DTO（シリアライゼーション）
│   ├── entitycompat/       # 静的な shape drift 検出と doc gate（Section 8 / docs/shape-drift.md）
│   ├── pluginspec/         # 公開プラグインAPIの面を抽出（entitycompat が使う）
│   ├── pluginstore/        # プラグインごとの専用 PostgreSQL schema (#2481)
│   ├── safehttp/           # 外向きHTTPの共通ヘルパー（SSRFガード等）
│   ├── charttick/          # チャートの絶対時刻を再導出する TickFunc 群
│   ├── effectivepolicy/    # ロールポリシーの host schema (本番の解決とプラグイン検証で共有)
│   ├── l10n/               # サーバーが送るメール文面のロケール解決
│   ├── safemath/           # 固定幅へ寄せるときに飽和させる算術ヘルパー
│   ├── maintenance/        # SQL migration として書けない後始末バッチ（`elythia backfill <名前>` で手動で回す）
│   ├── maintmode/          # メンテナンスモード (全プロセスが DB を手放す)。Redis の状態・各プロセスの報告・pg_stat_activity で held を決める判定役・メンテナンス中の HTTP 応答 (#3463)
│   ├── frontendutil/       # 同梱フロントエンドの資産配信ヘルパー
│   ├── pgarray/            # database/sql 用の PostgreSQL 配列型
│   ├── pgactivity/         # pg_stat_activity で DB に繋いでいる接続を数えて切る。バックアップの戻しとメンテナンスモードが使う (#3463)
│   ├── bgwork/             # 世代ごとに、投げっぱなしで起動する goroutine を数える箱。世代の停止で上限付きで待ち、待ちの 3/4 で ctx を切る (#3661)
│   ├── sentry/             # sentry-go の配線
│   ├── redislog/           # go-redis の内部ロガーを slog へ流す配線
│   ├── misc/               # ユーティリティ（ID生成 等。既定は`aidx`、Section 6 参照）
│   └── testutil/           # テスト用ヘルパー（testcontainers、モック）
├── plugin/                 # プラグインが import する公開パッケージ（docs/plugins/）
├── plugins/                # プラグイン本体。gitignore 済で同梱するものだけ例外指定
├── tools/                  # parity ゲート / コード生成のCLI群（apicompat、shapediff、pluginbuild 等）
├── migration/              # golang-migrate用SQLファイル（`NNNNNN_name.up.sql` / `.down.sql`）
├── tests/                  # Go の e2e（`tests/e2e` / `tests/e2e-federation`）と、Go 以外の検証基盤（playwright / diff / dropin / bench / upstream-e2e 等）
├── frontend/               # 同梱フロントエンド。fork した Misskey TS を取り込んだ pnpm workspace（#3379）
├── deploy/                 # デプロイ用の補助資材（UDS 構成、pg_bigm 入り postgres image）
├── .config/                # 設定ファイル（Misskey互換YAML）
│   ├── default.yml.example # ローカル開発用テンプレート (track 対象)
│   ├── docker.yml.example  # Docker Compose用テンプレート (track 対象)
│   ├── default.yml         # operator-local (gitignored)
│   └── docker.yml          # operator-local (gitignored)
├── docs/                   # プロジェクトドキュメント
├── Makefile
├── Dockerfile
├── docker-compose.yml      # **`name:` が無い**。単体で使うと本番 project `mk` に合流する
└── go.mod                  # Moduleパス: github.com/elythia-network/elythia
```

`built/` と `drive-files/` は gitignored な生成物 / ローカルストレージ。

レイヤ責務：
- **api** → **core** → **repository** → **model** の順に依存。逆向きの依存は禁止。
- **entity**はレスポンス変換専用。ドメインロジックを入れない。
- **activitypub**は`core`から呼び出され、連合処理を担う。
