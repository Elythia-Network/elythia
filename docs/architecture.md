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
| DI | NestJS (`@Injectable`, `di-symbols.ts`) | **手動配線** (`internal/server/router.go`) | フレームワーク非依存・起動経路の明示化 |
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
| `internal/server/router.go` | NestJS `MainModule`/`ServerModule` の配線 | DI 相当を手動で |

---

## 3. レイヤ詳細

### 3.1 `internal/api/` — HTTP ハンドラ（52 パッケージ / 503 ルート）

upstream `server/api/endpoints/` の endpoint 群を、ディレクトリ単位で実装。**upstream の 444 endpoint をすべて実装済み** (coverage 100.0%)。一次情報は `make apicompat` が生成する [api-compat.md](api-compat.md)。

ルート数は `router.go` が `/api/*` に静的登録する数 (`api.POST` 466 + `api.GET` 12 +
`api.Match(chartMethods, …)` 12 × 2 methods + catchall の `api.Any` 1 = 503)。実行時は
これに同梱プラグインのルートが乗る。`docs/api-compat.md` の 444 + 58 = 502 に catchall を
足した数と一致する。

`/api` の外は `s.echo` へ直接登録していて、**呼び出し**の数は 55
(`GET` 45 / `POST` 5 / `OPTIONS` 2 / `Any` 3)。`Any` は 1 呼び出しで全メソッドを
登録するので、ルート単位に展開すると増える。ほかに静的配信が 8 (`Static` 4 /
`File` 4)、`/debug/pprof` 配下が 8。

**Static / File / pprof は `if` の中**で、設定次第では登録されない。55 呼び出しの側にも条件付きのものが 4 件ある (`/url` / CSP report / `/vite/*` / `/embed_vite/*`)。

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

すべてのサービス生成・配線は `internal/server/router.go` の `setupRoutes()` で手動実行（NestJS 相当を関数呼び出しで構築）。DI フレームワークは使わない。repository は `setupRoutes()` から呼ぶ `buildRepos()` (`internal/server/wiring_repos.go`) で、core の service の一部は `buildCore()` (`internal/server/wiring_core.go`) で、連合の部品の一部は `buildFederation()` (`internal/server/wiring_fed.go`) で、queue の processor の一部は `buildQueue()` (`internal/server/wiring_queue.go`) で、stream の部品の一部は `buildStream()` (`internal/server/wiring_stream.go`) で作る (4.2)。

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
  ├→ Handler 生成 + ルート登録 (§3.1)。この途中 (プラグインの準備より前) で、queue の
  │   handler を registerQueueHandlers で 1 回だけ登録する (一覧は builtinQueueHandlers)
  └→ stream (bus・registry・manager と、ここから配る publisher は buildStream → wiring.stream
      で作り、別名を置く。pubsub の購読をここで始める。WebSocket のチャンネルは
      registerStreamChannels で 1 回だけ登録する。一覧は builtinStreamChannels) /
      WebSocket / 静的ファイル / middleware
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

**構築の途中で動く処理から見える配線も変えない。** `setupRoutes()` が終わる前から動く
goroutine は、`startConstructionWorker` で起動する worker 4 つ (`pollExpiryWorker`、
reaction の flush、retention の集計、利用者のキャッシュの invalidation)、pubsub の購読の
handler、`fedRuleHits` / `instanceTouchBuffer` / `sigCapBuffer` / `instanceCounterBuffer`
の `Start`、queue role の `deliveryHealth` / `inboxHealth` の `Start`、
`emojiInvalidationQueue` の `Run`、server / queue の stats publisher の `Start` がある。
そこから辿れる setter を、その起動より前へ移すと、構築中の動きが変わる。#3676 で前へ
移した setter のうち、これらから辿れるのは下の 1 本だけで、ほかの受け手 (note・
following・reaction・drive・連合・queue の processor・handler など) には届かない。例えば `notificationService.SetMainStreamPublisher` を前へ移すと、
`pollExpiryWorker` が構築中に作った通知が、packer (`SetPacker`、worker の起動より後) を
通らない形で `unreadNotification` として流れる (#3676 のレビューで実測)。なので、この
setter は依存を先に作っても、元の位置 (streaming の publisher の隣) に残している。

#3676 で、stream の pubsub と publisher (`streamPubSub` / `internalPubSub` /
`noteEventPub` / `relationReloadPublisher` / `mainStreamPublisher` / `drivePublisher` /
`reversiPublisher`) を `setupRoutes()` の先頭へ、chart の engine と hook
(`chartCharts` / `chartHooks`) を repository の直後へ移し、受け手には作った直後に渡す
ようにした (上の `notificationService.SetMainStreamPublisher` だけは元の位置)。ほかに、受け手より後で作っていた純粋な部品 (repository 16 個、
`featuredService`、`hashtagService`、`modLogService`、`instanceService` など) を
受け手より先に作るようにした (こちらは setter を呼ぶ位置は変えていない)。

| | #3676 の前 (`b9db7e49b`) | #3676 の後 | #3684 の後 | #3686 の後 | #3688 の後 | #3690 の後 | 今 (#3692 の後) |
|---|---|---|---|---|---|---|---|
| setter で依存を入れる辺 | 877 | 877 | 879 | 879 | 872 | 872 | 872 |
| うち、受け手より後で作った依存を入れるもの | 192 (受け手 55 個) | 107 (受け手 41 個) | 104 (受け手 41 個) | 103 (受け手 40 個) | 96 (受け手 40 個) | 96 (受け手 40 個) | 96 (受け手 40 個) |
| うち、循環しているもの | 40 | 40 | 40 | 39 | 35 | 35 | 34 |
| うち、それ以外 | 152 | 67 | 64 | 64 | 61 | 61 | 62 |
| setter の呼び出しが受け手から離れているもの (下の数え方) | 359 | 307 | 308 | 392 | 415 | 426 | 444 |

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

数え方 (`setupRoutes()` の構文木。道具はコミットしていない):

- 節点: `setupRoutes()` の中で `:=` か `var` で定義した名前 (関数リテラルの中と、
  `if` / `for` / `switch` の初期化文の一時変数を除く) と、`s.<フィールド>`。定義の位置は
  最初の定義の行 (`s.<フィールド>` は `setupRoutes()` の中で最初に代入した行、無ければ 0)
- 辺: `X.SetYyy(...)` の呼び出しで、受け手 `X` が節点で、引数 (関数リテラルの中を含む)
  に `X` 以外の節点 `B` が現れるもの。1 回の呼び出しに 2 つの節点が現れれば 2 辺
- 後から入れる辺: `B` の定義の行が `X` の定義の行より後のもの
- 循環: 依存の図 (`A := f(... B ...)` の右辺と、setter の引数から張る `A → B`) で、
  `B` から `X` へ辿り着けるもの。図は `setupRoutes()` の中だけで張り、`buildRepos()`・
  `buildCore()`・`buildFederation()`・`buildQueue()`・`buildStream()` の中の構築の依存は
  辿らない (別名 `x := coreSvcs.y` の依存は `coreSvcs` だけ)
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
#3690 で queue の processor の一部を、#3692 で stream の部品の一部を移した。今のフィールドは
`repos`・`core`・`fed`・`queue`・`stream` の 5 つ。

```
setupRoutes()
  w := &wiring{repos: s.buildRepos()}   // wiring_repos.go
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
  - `repos` はリテラルのキー (`w := &wiring{repos: s.buildRepos()}`)。`w` を定義する文で
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
  - **後の領域の `build…()` も、先に作った領域を `w.core.x` のようにコンテナから読まず、
    `coreSvcs` のような変数を引数で受ける。** コンテナのフィールドから読むと、その
    `build…()` を代入より前に置いてもコンパイルが通り、ゼロ値を受け取る (別名と同じ穴)。
    今 `w.core`・`w.fed`・`w.queue`・`w.stream` を読む箇所は無い (#3686 のレビュー 2 周目。
    `buildFederation()` と `buildQueue()` は `coreSvcs` を引数で受ける)
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
| `if` の中で作る (3 回) | `pollExpiryWorker` (queue role だけ、構築の途中で動く worker)、sensitive media の detector (meta を読んだ分岐の中) |
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

**残りの領域は、次の順で同じ形に移す。** handler と
ルート (ルートはファイルを分ける)。上で残した core の service・連合の部品・queue の
processor・stream の部品は、依存する領域を移すときに一緒に扱う。どれも `wiring` にフィールドを 1 つ足し、
`build…` の関数で作り、`setupRoutes()` では別名を置く。repository は `w` から、それより後の
領域は `coreSvcs` / `fedSvcs` / `queueSvcs` / `streamSvcs` のような変数から受け取る。後の領域ほど、構築の途中で動く処理
(4.1) と shutdown hook を登録するものが多いので、それらを動かさずに残す範囲を
PR ごとに決める。

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
