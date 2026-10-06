# アーキテクチャ

Elythia は Misskey (TypeScript/NestJS) のバックエンドを Go で書き換えたプロジェクトです。
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
| `optional` | JSON の null/absent を区別する Nullable 型 |
| `notehide` | per-viewer の hideNote ゲートを全 REST に一律適用（TS は各 pack 内 `hideNote`） |
| `userrelation` | viewer 視点の relation block (isFollowing 等) 解決 |
| `middleware` (**`internal/server/middleware`**。`internal/api/` 配下ではない) | 認証 / rate limit / WWW-Authenticate / Cache-Control |

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
| `note` の filter / `notesfilter` | QueryService (+ check-word-mute) | 可視性 / mute / block / word-mute の SQL filter |
| `wordmute` | check-word-mute.ts | 単語/正規表現ミュートのパース・LRU キャッシュ |
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

すべてのサービス生成・配線は `internal/server/router.go` の `setupRoutes()` で手動実行（NestJS 相当を関数呼び出しで構築）。DI フレームワークは使わない。

```
server.New() → setupRoutes()
  ├→ Repository 生成 (userRepo, noteRepo, … 30+)
  ├→ Core サービス生成 (noteCreateService, followingService, …)
  ├→ hook 注入 (§6)
  ├→ Handler 生成 + ルート登録 (§3.1)
  └→ WebSocket / 静的ファイル / middleware
```

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

マイグレーション（`migration/`、golang-migrate、現在 116 本）:

- TS Misskey の既存テーブルへは原則**追加のみ**。例外が 18 件あり、うち 14 件は Elythia が自分で作ったものの除去・初期化か upstream 追随 ([TS版からの移行](migration-from-ts.md#破壊的なマイグレーション))。Go 固有の追加列・テーブルは `IF NOT EXISTS`。
- drop-in テストで発見した補完列は専用マイグレーションで追加。
- down スクリプトは必須（data loss する場合は `-- data loss:` で明記する）。**ただしこれは今後の規約で、既存の down は守れていない** — 宣言があるのは 23 本だけで、宣言が無いまま `DROP TABLE` / `DROP COLUMN` する down が 51 本ある（[migration-from-ts.md](migration-from-ts.md#elythia-内での切り戻し)）。

```bash
make migrate-up      # 最新まで
make migrate-down    # 1 段ロールバック
make migrate-create  # 新規作成

# 全段ロールバック (破壊的。全テーブルが消える)
go run ./cmd/elythia migrate -direction down
```

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
├── internal/               # 全28ディレクトリ (`git ls-tree -d HEAD internal/ | wc -l`)
│   ├── cli/                # elythia のサブコマンド (serve / migrate / backfill <名前> / doctor / fsck / config-dump / healthcheck / dump-routes)
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
│   ├── frontendutil/       # 同梱フロントエンドの資産配信ヘルパー
│   ├── pgarray/            # database/sql 用の PostgreSQL 配列型
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
