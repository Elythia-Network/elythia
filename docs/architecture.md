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
| `paramschema` | 本家の`paramDef`(引数のJSON Schema)で、本家のajvと同じ判定・同じ最初のエラー・同じdefaultの挿入をする検証器(#3673)。Elythiaが本家の外で受ける値は`elythia_overlays.json`に宣言し、`Effective`が本家の`paramDef`に足すことしかできない形で重ねる(#3865)。`internal/server/paramshadow`がhandlerの前で走らせる。本家かElythiaの宣言のあるendpointの全部(テストのモードでだけ登録する`reset-db`を除く)では拒否に使い(#3742から#3888で広げ、#3890で宣言から決める形にした)、それ以外の本家のendpoint(テストのモードでだけ登録する`reset-db`だけ)では、検証だけを走らせてhandlerと判定が分かれた件数を数える(#3677) |
| `optional` | JSON の null/absent を区別する Nullable 型 |
| `notehide` | per-viewer の hideNote ゲートを全 REST に一律適用（TS は各 pack 内 `hideNote`） |
| `userrelation` | viewer 視点の relation block (isFollowing 等) 解決 |
| `middleware` (**`internal/server/middleware`**。`internal/api/` 配下ではない) | 認証 / rate limit / WWW-Authenticate / Cache-Control |
| `paramshadow` (**`internal/server/paramshadow`**) | 本家の`paramDef`での検証をhandlerの前で走らせる。本家に揃えたendpoint(`reset-db`を除く、本家かElythiaの宣言のあるendpointの全部。#3890)では本家と同じ400で拒否し(#3742)、それ以外(本家のendpointでは`reset-db`だけ)では応答を変えずに、判定が分かれた件数をログに出す(#3677、[param-shadow.md](param-shadow.md)) |

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

`core/entities/*EntityService.pack()` に対応。Elythia では `entity.Packer` のメソッド `PackX()` で、ドメインロジックは持たない。pack が読む依存 (role 由来の値・アバターの飾り・メディアのプロキシなど) は Packer が持つ (#3788)。組み立て (`internal/server`) で 1 組の依存から部品へ渡す Packer を作る。投稿を包む部品 (`PackNote` / `PackNotes` / `PackNoteWithInstance` と `NoteFieldResolver`) は渡された Packer を使う (#3790)。利用者の詳細を包む部品 (`PackUserDetailed` / `PackMeDetailed` / `PackUserForFollowStreamEvent` と `userpack.Packer`) も同じく渡された Packer を使う (#3792)。`PackUserLite` を直接呼ぶ部品も同じ (#3794)。ドライブのファイル・ページ・クリップ・お知らせ・通知を包む部品も同じで、通知に埋め込む投稿と利用者も渡された Packer で包む (#3796)。メディアのプロキシ URL と `IdenticonURL` を使う側も同じで、`/avatar`・`/emoji`・フィード・reversi のアイコンは組み立てた Packer をそのまま使う (#3798)。既定の Packer へ委ねるパッケージ関数は無くなり、既定の Packer と `Set*` も本番から消した (#3800)。メディアのプロキシ (`MediaURLContext`) は `entity.NewPacker` の必須の第 1 引数で、残りの 7 つの依存は `PackerDeps` の欄で渡す (#3834)。`PackerDeps` の 7 つの欄の配線は `entitycompat` の `TestEntityPackerDepsAreWired` が見る。組み立てた Packer に 8 つの依存がすべて入っていることは `internal/server` の `TestBuiltHandlersHaveNoNilDependencies` が見る。

| Elythia | Misskey-TS |
|---|---|
| `(*Packer).PackNote` / `PackNotes` (note.go) | NoteEntityService |
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
| `core/federation/processor.go` / `processor_*.go` | `ApInboxService` | inbox activity の処理 (Follow/Undo/Like/Announce/…) |
| `core/federation/resolver.go` / `resolver_*.go` | `ApResolver/ApDbResolver/RemoteUserResolve Service` | リモートオブジェクト解決・キャッシュ |
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
  │   で作り、usersHandler の位置で別名を置く。setter は元の位置 (#3817 で構築子の
  │   必須の引数にした依存は buildHandlers で渡す)。buildHandlers の直前で
  │   buildStream を呼ぶ (#3827。streamRevokePublisher を除く別名・setter・購読は下の stream の節)。この途中
  │   (プラグインの準備より前) で、queue の handler を registerQueueHandlers で 1 回だけ
  │   登録する (一覧は builtinQueueHandlers)
  ├→ stream (bus・registry・manager と、ここから配る publisher は buildStream で作り
  │   (#3827 から buildHandlers の直前で呼ぶ)、ここで wiring.stream に入れて別名を置く。
  │   pubsub の購読をここで始める。WebSocket のチャンネルは
  │   registerStreamChannels で 1 回だけ登録する。一覧は builtinStreamChannels。
  │   #3901 から、別名より後の購読・チャンネルの登録・/streaming は節 wireStreaming) /
  │   WebSocket / middleware
  ├→ /api のルートの登録。handler をすべて作った後、プラグインの準備より前で、
  │   routes_<領域>.go の register…Routes を 1 回ずつ呼ぶ (24 個)。/api の外のルート
  │   (ActivityPub・SSR・well-known など) は s.echo へ元の位置で登録する
  └→ プラグインの準備 (失敗したらここで return) / /api の catchall / 静的ファイル /
      SPA の catchall (静的ファイルと SPA の catchall は #3901 から節 wireFrontendRoutes)

#3901 から、`setupRoutes()` の連続した範囲のいくつかは `wire…` の節にある (4.2 の
「setupRoutes の節」)。節は元の範囲の位置で呼ぶので、上の順は変わらない。

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
ようにした (上の `notificationService.SetMainStreamPublisher` だけは元の位置)。
`reversiPublisher` は、#3827 で Packer を構築子の引数で受けるようにしたので、`entityPacker` の
直後へ移した (使う側はどれもそれより後にある)。ほかに、受け手より後で作っていた純粋な部品 (repository 16 個、
`featuredService`、`hashtagService`、`modLogService`、`instanceService` など) を
受け手より先に作るようにした (こちらは setter を呼ぶ位置は変えていない)。

| | #3676 の前 (`b9db7e49b`) | #3676 の後 | #3684 の後 | #3686 の後 | #3688 の後 | #3690 の後 | #3692 の後 | #3694 の後 | #3697 の後 | #3699 の後 | #3705〜#3729 の後 |
|---|---|---|---|---|---|---|---|---|---|---|---|
| setter で依存を入れる辺 | 877 | 877 | 879 | 879 | 872 | 872 | 872 | 872 | 872 | 872 | 872 |
| うち、受け手より後で作った依存を入れるもの | 192 (受け手 55 個) | 107 (受け手 41 個) | 104 (受け手 41 個) | 103 (受け手 40 個) | 96 (受け手 40 個) | 96 (受け手 40 個) | 96 (受け手 40 個) | 96 (受け手 40 個) | 96 (受け手 40 個) | 96 (受け手 40 個) | 96(受け手40個) |
| うち、循環しているもの | 40 | 40 | 40 | 39 | 35 | 35 | 34 | 34 | 34 | 34 | 34 |
| うち、それ以外 | 152 | 67 | 64 | 64 | 61 | 61 | 62 | 62 | 62 | 62 | 62 |
| setter の呼び出しが受け手から離れているもの (下の数え方) | 359 | 307 | 308 | 392 | 415 | 426 | 444 | 660 | 660 | 660 | 659 |

#3732 の後の数は、4.4 の末尾にある (instance actor の秘密鍵を 1 つの変数にまとめたので、setter で依存を入れる辺が 870、呼び出しが離れている辺が 657 になった。後から入れる辺は 96 のまま)。

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
呼んだ (4.2)。#3827 で `buildStream()` の呼び出しだけを `buildHandlers()` の直前へ動かしたが、
別名 (`streamRevokePublisher` を除く) は stream の節に残したので、この比べ方の前提 (manager と publisher の別名の位置) は変わらない。

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
起動と登録を、値を 1 つ渡す登録 (`s.startPartWithClose(ctx, name, part)` など。#3715で`add…`の
名前に改めた) に置き換えた (4.3。もう 1 つのプラグインの store は `setupRoutes()` の外の
`dbBackedStorage` で登録する)。起動の位置も登録の順も変えていないので、どの行も数が変わらない。後から入れる
96 辺と、呼び出しが離れている 660 辺は、どちらも行番号を除けば前と同じ。節点も 361 のまま。

#3703で、構築の途中で起動していた12個の部品の起動を`StartBackground`へ移した(4.3)。`setupRoutes()`
の中で変えたのは`fedRuleHits`の1か所(`fedRuleHits.Start(hitsCtx)`と`s.addPart(...)`の2行を、
`s.addPartWithStart(...)`の1行にした)だけで、setterも構築の順も動かしていない。後から入れる96辺と、
呼び出しが離れている660辺は、どちらも行番号を除けば前と同じ。節点も361のまま。

#3705で、`emojiInvalidationQueue`の`Run`の起動を`StartBackground`へ移した(4.3)。`setupRoutes()`の
中で変えたのは、`go`文と`s.addPart(...)`のclosure(局所変数`emojiInvalidationCtx`・
`stopEmojiInvalidation`・`emojiInvalidationDone`を使う)を、`s.startPartWithStop(...)`(今の
`addStartStopper`)の1行にしたことだけで、setterも構築の順も動かしていない。節点は消した3つの局所変数の分だけ減り、358になった。
後から入れる96辺は、行番号を除けば前と同じ。呼び出しが離れている辺は660から659になった。減ったのは
`emojiRepo.SetInvalidationHook(emojiInvalidationQueue.Enqueue)`の辺で、`emojiInvalidationQueue`を
作ってから呼ぶまでの間に定義していた節点が、消した3つだけだったため。

#3717で、`fedRuleHits`・`hashtagService`の登録を値から作る登録にした(4.3)。`setupRoutes()`の中で
変えたのは、`fedRuleHits`のブロック(局所変数`hitsCtx`・`stopHits`と`s.addPartWithStart(...)`)と
`hashtagService`の`s.addPart(...)`のclosureを、それぞれ1行の登録にしたことだけで、setterも構築の順も
動かしていない。節点は消した2つの局所変数の分だけ減り、356になった。後から入れる96辺は行番号を除けば
前と同じで、呼び出しが離れている辺は659のまま。

#3827で、`buildStream()`の呼び出し(`streamSvcs := buildStream(...)`)を、stream の節から
`buildHandlers()`の直前へ動かした(4.2)。`i`と`oauth`のhandlerが、`buildStream()`で作る
`streamRevokePublisher`を構築子の引数で受けるため。動かしたのはこの1行だけで、`w.stream`への
代入、別名(`streamManager := streamSvcs.streamManager`など)、setter、購読(`Subscribe…`)は
stream の節に残した。下の数え方では、別名の依存は`streamSvcs`だけで、`streamSvcs`をsetterの
引数に書いている行は無い。なので、定義の行が前へ移る節点は`streamSvcs`だけで、どのsetterの
受け手と依存の定義の行も変わらず、後から入れる辺と呼び出しが離れている辺は変わらない(道具では
数え直していない。どの辺にも`streamSvcs`が現れないことから言える)。`buildStream()`の中の
構築子は、どれも値を作って返すだけ(下の「stream の部品 (#3692)」)。後の段で`streamRevokePublisher`の
別名だけを呼び出しの直後へ移した(`i`と`oauth`の構築子へ渡すため)。依存の定義が前へ移るだけなので、
後から入れる辺は増えない。同じ#3827の後の段で、
`reversiPublisher`の定義を`entityPacker`の直後へ後ろへ移した。`reversiPublisher`を受け手とする
setter(`SetEntityPacker`)は構築子の引数にして消し、依存とするsetter
(`federationProcessor.SetReversiStreamPublisher`・`reversiHandler.SetStreamPublisher`)の受け手は
どちらも`entityPacker`より後で定義するので、後から入れる辺は増えない。

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
#3699 で停止が要る部品の一覧を指す `lifecycle` を、#3721 でキャッシュの一覧を指す `caches` を足した
(4.3 / 4.4。部品は作らず、`Server` が持つ一覧を指すだけ)。段階 (6) で作り直す範囲を絞るときに使う予定
だったが、#3802 で狭めないと決め、誰も読まないまま残っていたので #3808 で消した (一覧そのものは `Server` に残る)。
#3697 で `/api` のルートの登録を領域ごとのファイルの関数へ分けた (部品は作らないので、
フィールドは増やしていない。下の「/api のルートの登録」)。

```
setupRoutes()
  w := &wiring{repos: s.buildRepos()}
  // ↑ repos は buildRepos (wiring_repos.go) が作る
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
  - `core` は変数を経由した代入 (`coreSvcs := s.buildCore(w, idGen, roleService, streamPubSub, entityPacker, sharedUserPacker)`、
    `w.core = coreSvcs`、別名は `x := coreSvcs.x`。引数の`roleService`は#3825から`buildRole()`で作る。`streamPubSub`は先頭で作るstreamのpubsub。2つのpackerは`buildRole()`の後に作る)。`buildCore()` が `w.repos` を読むので
    リテラルには足せない。`w.core = …` から直接別名を取ると、別名を代入より前に書いても
    コンパイルが通るので、別名は変数から取る。照合はこの 3 つの行で行う
  - `fed` も `core` と同じ形 (`fedSvcs := s.buildFederation(w, coreSvcs, idGen)`、
    `w.fed = fedSvcs`、別名は `x := fedSvcs.x`)
  - `queue` も同じ形 (`queueSvcs := s.buildQueue(w, coreSvcs, idGen)`、
    `w.queue = queueSvcs`、別名は `x := queueSvcs.x`)
  - `stream` も同じ形 (`streamSvcs := buildStream(streamPubSub, idGen, entityPacker, …)`、
    `w.stream = streamSvcs`、別名は `x := streamSvcs.x`。`…` は #3827 で足した、manager の
    4 つの lookup)。`buildStream()` は `Server` の
    フィールドを読まないので、メソッドではなく普通の関数にした。stream の pubsub は
    `setupRoutes()` の先頭で作った変数 `streamPubSub` を引数で受ける。#3827 から
    `buildStream()` は `buildHandlers()` の直前で呼び、`w.stream` への代入と別名は stream の節に
    置く (`streamRevokePublisher` の別名だけは呼び出しの直後。呼び出しより前に別名を書けない
    ことは変わらない)
  - `handlers` も同じ形 (`hSvcs := s.buildHandlers(w, coreSvcs, fedSvcs, idGen, …)`、
    `w.handlers = hSvcs`、別名は `x := hSvcs.x`。`…` は #3817 と #3827 で足した、handler の
    構築子へ渡す値。#3827 の 2 つは `twoFAPendingGuard` と `streamRevokePublisher`)
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
  roleService := s.buildRole(w, idGen)  // wiring_core.go (#3825)
  …                                     // entityPacker と sharedUserPacker (#3825)
  coreSvcs := s.buildCore(w, idGen, roleService, streamPubSub, entityPacker, sharedUserPacker)
  w.core = coreSvcs
  featuredService := coreSvcs.featured  // 別名 (変数から取る)
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
  (中で呼ぶ関数を含む) を 1 つずつ読んで確かめた。#3825 から、`buildCore()` が引数で
  受ける値 (`roleService`・stream の pubsub `streamPubSub`・`entityPacker`・`sharedUserPacker`) にも依存してよい。stream の
  publisher は、その pubsub から作るもの (`clipService` に渡す `NewClipStreamPublisher`) に
  限る (作っても購読も接続もしない)
- **setter は 1 つも動かさない。** 構築の途中で動く処理 (4.1) から見える配線を変えない
  ため (#3825 で構築子の必須の引数にした依存は、setter ではなく `buildCore()` の中で構築子に
  渡す。下の「handlerの依存を構築子の必須の引数にする」の段(3))。`buildCore()` を呼ぶ位置は、移したものの中で最初に作っていた `featuredService` の
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
`abuseInAppNotifier: abuseInAppNotifier`、`coreSvcs := s.buildCore(w, idGen, roleService, streamPubSub, entityPacker, sharedUserPacker)`、
`w.core = coreSvcs`、`abuseInAppNotifier := coreSvcs.abuseInAppNotifier` も照合する。#3825で
`roleService`を`buildCore()`の引数にしたので、`roleService := s.buildRole(w, idGen)`も照合する。

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
  の引数) だった。#3833 から `instance` も使う (`deliverService` の host のブロックの判定)
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
  呼ばない。#3809 で `archiveAnnouncementsProc` を足して 16 個になり、#3829 で
  `deleteAccountProcessor` と `postScheduledNoteProcessor` を `setupRoutes()` へ戻して 14 個
  (下の「handlerの依存を構築子の必須の引数にする」の段(5))
- **setter は 1 つも動かさない。** `buildQueue()` を呼ぶ位置は、移したものの中で最初に
  作っていた `importProcessor` の位置 (`exportProcessor` の直後)。移した構築は副作用を
  持たないので、DB と Redis の文の順は変わらない。起動時に 1 回だけ流す retention の集計
  (`startConstructionWorker`。今の`addWorker`。queue role だけ) も、位置も条件も元のまま
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

**`setupRoutes()` に残したもの** (`processors.New…` の 14 回の呼び出し。#3829 で 2 回戻して 16 回):

| 理由 | 例 |
|---|---|
| outbound の HTTP client を使う | `webPushProcessor` (`webPushSender` の `s.outboundClient(...)`)、`webhookProcessor` |
| `setupRoutes()` で作る部品に依存する | `exportProcessor` (`exporter`)、`emojiImportProcessor` (`emojiImporter`)、`resendQuoteRequestsProcessor` (`quoteOutbox`)、`deliverProcessor` (`apClient`)、`inboxProcessor` (`federationProcessor`)、`flushProcessor` (`reactionCountWriter`)、`chartProcessor` (chart の engine `chartCharts`)、`instanceRefreshProc` (`metadataFetcher`)、`checkModeratorsActivityProcessor` (`modActivitySvc`。stream の publisher に依存する) |
| setter の引数の中で作る | `NewRepoSigningKeySource`、`NewRedisInboxReplayGuard`、`NewRedisScheduledNoteLock` (#3829 から 3 つとも構築子の引数の中) |
| `buildQueue()` に無い値 (Web Push の購読キャッシュ・Redis の lock) を受ける (#3829 で戻した) | `deleteAccountProcessor` (`webPushCache`)、`postScheduledNoteProcessor` (`NewRedisScheduledNoteLock`) |

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
  streamSvcs := buildStream(streamPubSub, idGen, entityPacker, …)  // wiring_stream.go。#3827 から buildHandlers の直前
  streamRevokePublisher := streamSvcs.streamRevokePublisher        // i と oauth の構築子へ渡す (#3827)
  hSvcs := s.buildHandlers(…)
  …
  // Streaming の節
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
  そのまま移し、作る順も元のまま。#3827 から、構築子の必須の引数にした `entityPacker` (note・
  notification の publisher) と manager の 4 つの lookup (ハードミュート・ミュートとブロックの
  snapshot・role の policy・`ugcVisibilityForVisitor`) も引数で受ける。どれも `setupRoutes()` が
  呼び出しより前で作る (lookup の adapter と closure は、呼び出しの引数の中で作る)
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
  移した構築は副作用を持たないので、DB と Redis の文の順は変わらない。#3827 で、呼び出し
  (`streamSvcs := buildStream(...)`) だけを `buildHandlers()` の直前へ動かした。`w.stream` への
  代入と別名 (`streamRevokePublisher` の別名を除く) は stream の節に残した (4.1)
- **#3676 で `setupRoutes()` の先頭に置いた 4 つの publisher (note event・drive・reversi・
  main stream) は、先頭に残した。** (reversi は #3827 で `entityPacker` の直後へ移した。4.1) どれも stream の節より前にある受け手 (core の service・
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
`streamSvcs := buildStream(streamPubSub, idGen)` (#3827 からは足した引数まで含めた呼び出し)、`w.stream = streamSvcs`、
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

**`setupRoutes()` に残したもの** (`stream.New…` の 9 回の呼び出し。#3809 で 1 回足して 10 回、#3825 で `NewClipStreamPublisher` を `buildCore()` へ移して 9 回):

| 理由 | 例 |
|---|---|
| stream の節より前にある受け手へ渡す | `noteEventPub`・`drivePublisher`・`reversiPublisher`・`mainStreamPublisher` (#3676 で先頭に置いたもの。受け手は core の service・連合の processor・handler。`reversiPublisher` は #3827 から `entityPacker` の直後で作る)、`userListStreamPublisher` (上でまとめたもの。stream の節より前の受け手は `usersHandler`) |
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
  受け取る。** 中の行は `setupRoutes()` にあった行をそのまま移し、作る順も元のまま (#3694 の
  時点。#3817 で構築子の引数を足した。下の「handlerの依存を構築子の必須の引数にする」)。使う
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
  `…Handler.Set…` を呼ぶ 385 行) は、位置も順も元のまま (どちらも #3694 の時点。#3817 で
  一部の setter を構築子の引数に移した)
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
| stream の部品に依存する | `streamingHandler` (`streamManager`。#3694 の時点では `buildStream()` を stream の節で呼んでいた。#3827 から呼び出しは `buildHandlers()` の直前だが、別名 `streamManager` は stream の節で置く) |
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

#### handlerの依存を構築子の必須の引数にする(#3817)

**`buildHandlers()`で作るhandlerのうち、入れ忘れると黙って劣化する依存は、setterではなく構築子の
必須の引数で受ける。** #3812の段(1)。以前は`setupRoutes()`でsetterを呼んで後から入れ、入れ忘れを
起動時の検査(`recordCriticalWiring`の表)とwiring-checkの文字列の照合で拾っていた。引数にすると、
渡し忘れはコンパイルで落ちる。**ただし引数にもnilは渡せる。** 以前の表と照合はnilを渡す形も
落としていたので、それは`TestBuiltHandlersHaveNoNilDependencies`(`internal/server`)が見る(下)。

- **置き換えたのは、表の行かwiring-checkの照合で見ていた依存。** 1段目は小さいhandlerの16個
  (webhooks・notifications・gallery・mute・renote-mute・federation・flash・following・user-lists・
  bubble-game・invite・blocking・auth・hashtags・pages・drive)で、表の34行と、wiring-checkが照合して
  いたnotificationsとinviteのsetter 5本(通報と申請の受付の通知のlookup・未対応の件数・権限の
  再確認、inviteのモデレーターの判定)。2段目は投稿を返すhandlerの6個(users・channels・
  antennas・clips・roles・ap)で、表の30行と、wiring-checkが照合していた
  `usersHandler.SetUserRelationReader`。表にもwiring-checkにも載っていないsetter
  (`blockingHandler.SetModeratorChecker`・`rolesHandler.SetMuteBlockRepos`など)は、setterのまま
  元の位置に残した
- **`buildHandlers()`は、置き換えに要る値を引数で受ける。** `setupRoutes()`の局所変数の
  `noteHider`・`entityPacker`・`sharedUserPacker`・`metaRepo`・`abuseNotifStates`・
  `signupApplicationNotifLookup`と、`abuseReportRepoForNotif.CountUnresolved`。どれも
  `buildHandlers()`を呼ぶ位置より前で作ってあるので、呼ぶ位置は変えていない。`Server`の
  フィールドは`s.db`・`s.config`・`s.echo`に加えて`s.background`も読むようになった。repository (`muting`・`blocking`・
  `channelMuting`・`userRelation`) は`w.repos`から、apの`inboxSoftwareChecker`は`fedSvcs`から
  受ける。ugcVisibilityForVisitorの毎回読むlookup (`func() string { return
  metaUGCVisibility(metaRepo) }`) は、users・channels・clipsのそれぞれに、元のsetterと同じ式で
  作って渡す。`CountUnresolved`などのmethod valueを
  `buildHandlers()`の中で`w.repos`から作らないのは、ゼロ値の`wiring`で組み立てる
  `TestBuildHandlersFillsEveryField`が、nilのinterfaceからmethod valueを作るところでpanicするため
- **置き換えたsetterと、起動時の検査に使っていた`Has…`は消した。** 構築子は引数を構造体に入れる
  だけで、副作用は足していない。テストは構築子で依存を渡す。テストの補助関数が作ったhandlerに
  後から入れていたテストは、同じパッケージのテストに限り、フィールドへ直接代入する
- **構築子が引数を捨てても、ビルドは通る。** 起動時の検査はhandlerのフィールドを見ていたので、
  この形も拾っていた。代わりに、各パッケージの`TestNewHandlerKeepsDependencies`が、渡した依存を
  handlerが持つことを確かめる
- **nilを渡す形は、組み立てたサーバーで見る。** `TestBuiltHandlersHaveNoNilDependencies`は
  `New`でサーバーを組み立て(test mode・`MK_ONLY_SERVER`)、`buildHandlers()`が作ったhandlerの
  うち`requiredDepHandlers`に並べたもの(2段目の後で22個。#3827で`i`を足して23個)について、nilになりうるフィールド(ポインタ・interface・func・map・slice・chan)が
  どれもnilでないことを確かめる。interfaceに型付きのnilのポインタを入れた形もnilとみなす。
  setterで入れるフィールドも、`setupRoutes()`を通った後なので同じ規則で見る。組み立てた
  handlerは`Server`のフィールド`builtHandlers`に持たせる(本番では読まない)。nilでよい
  フィールドは、どの経路で実際にnilになるかを書いた許可リスト(`handlerNilAllowed`)に載せ、
  使われない項目があれば落とす。#3817の時点では空(#3823で3つ、#3825で2つ足した。下の段(2)・段(3))。このテストを書く途中で、`setupRoutes()`が
  `clipsHandler.SetNoteRepo`を呼んでおらず本番でもnilのままだったことが分かり
  (clips/notesのrenoteの入れ子のmute/block検査が飛んでいた)、#3818で配線した。
  抽出が空振りしても緑にならないよう、置き換えた依存のフィールドを名指しで要求する
  (`requiredDepChecks`)。対象外の欄(wellknown・nodeinfoなど)は見ない。`i`のhandlerは#3827で
  対象に足した(下の段(4))
- **`entityPacker`の行も外した。** この行は`nil`ではなく`HasMediaURL()`を見ていて、mediaURLを
  持つことは型では保証できない。ただ、どのhandlerも`setupRoutes()`の同じ`entityPacker`を受け取り、
  その値そのものは表の`entityPacker.mediaURL`の行が見ている。段(8)でmediaURLを位置引数にするまで、
  `entityPacker.mediaURL`の行と、setterで受ける部品の`entityPacker`の行は残す(`entityPacker.mediaURL`の
  行は段(8)で外した)
- **pubsubの購読のhandlerから辿れる配線は変わらない(4.1)。** 依存を入れる時点が、元のsetterの
  位置(followingのlist packer・authのbackground・inviteのpolicyなどはstreamの節より後)から
  `buildHandlers()`の位置へ前へ移った。購読のhandler(`metaUpdated`・`federationRulesUpdated`・
  `rolesUpdated`・`antennaUpdated`と、`cachebus`・`corerelationcache`の`Connect…`)が触るのは、
  meta・ロール・アンテナ・関係・認証・利用者などのキャッシュと、metaとnodeinfoのhandlerで、
  置き換えたhandlerには届かない。置き換えたhandlerはHTTPのリクエストからしか呼ばれず、
  リクエストは構築の後にしか来ない
- **`buildHandlers()`より後で作る値に依存するものは、setterのまま。** `usersHandler.SetAbuseReportCreatedNotifier`
  (`abuseCreatedNotifier`を`buildHandlers()`より後で作る)と、`i`のhandlerの依存(`streamRevoker`が
  `buildStream()`の`streamRevokePublisher`)。`i`のhandlerは、streamの組み立てを前へ動かす段(4)
  (#3827)で置き換えた(下)

| | #3817の前(`16a2d79186`) | 1段目の後 | 2段目の後 |
|---|---|---|---|
| `criticalWiringCount` | 209 | 175 | 145 |
| `setupRoutes()`の本文で、行頭が`<名前>Handler.Set`の行 | 437 | 398 | 367 |
| そのsetterを呼ぶhandler(受け手) | 42 | 41 | 41 |

数え方: `criticalWiringCount`は`internal/server/wiring_check.go`の定数(表の実数と一致することは
`TestCriticalWiringCountMatchesTable`が見る)。setterの行は`setupRoutes()`の本文(`func`の行から、
行頭の`}`まで)で、行頭の空白の後が`<名前>Handler.Set`の行。受け手はその`<名前>Handler`の種類で、
1段目で減ったのは`bubbleGameHandler`(setterが`SetEntityPacker`だけだった)。

##### 段(2): `setupRoutes()`の本文で作るhandlerと部品(#3823)

**`buildHandlers()`の外(`setupRoutes()`の本文)で作るhandlerと部品も、同じ形にした。** #3812の
段(2)。置き換えたのは、表の行で見ていた依存だけ。

- **1段目は認証・管理の7構築子・22行。** adminのhandler(9行: `background`・`entityPacker`・
  `detailExtras`・`listPacker`・`suspensionOriginRepo`・`userTokenInvalidator`・
  `userStreamRevoker`・`ipLookupAudit`・`emojiImageFetcher`)、signup(3行)、signin(3行)、
  resetpassword(2行)、oauthの`authInvalidator`(1行)、`emojiapplication.Service`(2行)、
  `abuse.CreatedNotifier`(2行)
- **2段目はその他の部品の14構築子・19行。** chatのhandler(3行: `entityPacker`・`fileRepo`・
  `lites`)と`chat.Service`(3行: `entityPacker`・`lites`・`blockingRepo`)、reversiのhandler(2行)、
  bubble-gameの`VersusHandler`と`stream.BubbleVersusPublisher`、announcementsのhandlerと
  `announcement.Creator`、urlのhandler、`ssrMetaHandler`、`drive.URLUploader`、
  `webhook.SignupHook`(ここまで各1行で、どれも`entityPacker`)、`move.Service`
  (`SetCarryOverRepos`の4つの値を1行と数える)、`federation.RemoteStatsFetcher`
  (`hostAllowed`。可変長の`opts`の前に置いた)、`federation.RemoteUserResolver`(`softwareBlocker`)
- **`move.Service`の`blockQueue`はsetterのまま。** 表の述語(`HasCarryOverRepos`)は
  `SetCarryOverRepos`の4つに加えて`SetBlockQueue`で入れる`blockQueue`も見ていた。`blockQueue`は
  置き換えの対象ではないが、`accountMover`を`routeParts`に入れたので、nilのまま組み立てが終わると
  `TestBuiltHandlersHaveNoNilDependencies`が落とす(`requiredDepChecks`にも名指しで入れた)。
  `remoteUserResolver.SetHostBlockChecker`も表に無いので、setterのまま残した
- **`signup.formTokens`は残した。** 鍵かnonceの置き場が作れないときは機能を切る設計で、#3812で
  残すと決めた行。表の行もsetterも元のまま
- **段(4)へ回したもの(#3827で置き換えた。下の段(4))。** `oauth.streamRevoker`(構築子より後の`buildStream()`で作る値)、
  `reversiPublisher.entityPacker`(構築子が`entityPacker`より前にあり、置き換えるには作る位置を
  動かす必要がある)、`feed.ugcVisibility`・`peer.blocker`(setterではなく構造体リテラルで、
  置き換えるには構築子を新しく作る必要がある)
- **作る順は変えていない。** どの部品も、置き換えた依存は構築子より前で作ってあった。依存を入れる
  時点が、元のsetterの位置から構築子の位置へ前へ移っただけで、構築子から最後に置き換えたsetterまでの
  間に、`Subscribe`・`Connect…`・`Start`・goroutineの起動は無い(行を突き合わせて確かめた)。
  pubsubの購読のhandlerから辿れる配線は変わらない(4.1)
- **外部パッケージのテストは、`export_test.go`の`…ForTest`で後から入れる。** 同じパッケージの
  テストはフィールドへ直接代入するが(#3817と同じ)、admin・signup・signin・`core/chat`・
  `core/federation`のテストの多くは外部パッケージ(`admin_test`など)にある。補助関数が作った
  部品に後から入れていた箇所(adminは`SetEmojiImageFetcher`だけで60か所。テストで
  `h.SetEmojiImageFetcherForTest(`を呼ぶ行を数えた)を、構築子の引数へ書き換える代わりに、
  テストのビルドでだけ見える入れ口を置いた。本番のコードからは呼べない。別のパッケージの
  テスト(`api/chat`から`chat.Service`を作るものなど)は`export_test.go`が見えないので、
  構築子で渡す
- **nilを渡す形は、`TestBuiltHandlersHaveNoNilDependencies`を広げて見る。** `setupRoutes()`の
  本文で作る部品を、`Server`のフィールド`builtRouteParts`(型は`routeParts`、
  `internal/server/route_parts.go`)に持たせる。代入は全部のsetterを呼んだ後
  (`recordCriticalWiring`の直前)。テストは`routeParts`の欄を全部見るので、欄を足せばそのまま
  対象になる。許可リスト(`handlerNilAllowed`)に入れたのは3つ。adminの`captchaVerifierFactory`と
  `clock`は、どちらもテストだけが入れるseam(`SetCaptchaVerifierFactory`・`SetClockForTest`)で、
  `setupRoutes()`は入れないので本番でも常にnil。`chatService.messageObserver`は、通知を受け取る
  プラグインが居るとき(`newPluginNotificationDispatcher`がnilでないとき)だけ入れるもので、テストの
  組み立てはプラグインを登録しない。issueで挙げていたadminのqueueまわり・captcha・WebAuthnは、
  テストの組み立て(test mode・miniredis)では全部入るので、載せると「使われない項目」として落ちる
- **依存の中まで見るものは`nestedDepChecks`に書く。** `iplookuplog.Service`は`repo`と`idGen`の
  どちらかがnilだと黙って記録しない。以前の表の述語(`Wired()`)は両方を見ていたので、
  `admin.ipLookupAudit`の`repo`・`idGen`も見る。`Wired()`は使う所が無くなったので消した
- **wiring-checkの照合も直した。** `TestIPLookupAuditIsWired`から`adminHandler.SetIPLookupAudit`の
  項目を外し(読む側の`SetIPLookupLogRepo`は残す)、`TestAbuseReportNotifierIsWired`は構築子の
  呼び出しを、`TestEmojiApplicationIsWired`(`internal/server`)は`emojiapplication.NewService`の
  最後の引数が`repository.NewEmojiApplicationQuotaResetRepository(s.db)`であることと、
  `apiadmin.NewHandler`の最後の引数が`apiadmin.NewEmojiImageFetcher(...)`であることを照合する。
  `TestRemoteStatsGateUsesFailClosedPredicate`は、`NewRemoteStatsFetcher`の呼び出しに
  `instanceService.CanFetchOptionalRemoteData`を渡していることを照合する(同じ型で判定を反転させた
  関数も渡せるので、型では防げない)

| | #3823の前(`8b832bd5ec`) | 1段目の後 | 2段目の後 |
|---|---|---|---|
| `criticalWiringCount` | 145 | 123 | 104 |
| `setupRoutes()`の本文で、行頭が`<名前>Handler.Set`の行 | 368 | 350 | 342 |
| そのsetterを呼ぶhandler(受け手) | 41 | 41 | 40 |
| `setupRoutes()`の本文で、行頭が`<変数>.Set`の行 | 721 | 699 | 680 |
| その受け手 | 119 | 119 | 112 |

数え方: 上の表と同じ。下の2行は、行頭の空白の後が`<変数>.Set`に大文字が続く行と、その`<変数>`の
種類(handler以外の部品も数える)。#3823の前の368行は、#3817の2段目の後の367行に、#3818で足した
`clipsHandler.SetNoteRepo`の1行を足したもの。2段目で減った受け手は、置き換えたsetterしか
呼んでいなかった`urlHandler`・`bubbleVersusPublisher`・`announcementCreator`・`ssrMeta`・
`driveURLUploader`・`webhookSignupHook`・`remoteStatsFetcher`の7つ(うちhandlerは`urlHandler`)。

##### 段(3): `buildCore()`で作るcoreの部品(#3825)

**`buildCore()`で作るcoreの部品も、同じ形にした。** #3812の段(3)。置き換えたのは、表の行で
見ていた依存だけ。

- **1段目で、`roleService`と2つのpackerを`buildCore()`より前で作るようにした。** 部品の
  構築子の一部は`entityPacker`か`sharedUserPacker`を受け取るが、2つのpackerは`buildCore()`の
  中で作る`roleService`に依存していた。そこで`roleService`を`buildRole()`
  (`internal/server/wiring_core.go`)で作り、`setupRoutes()`の順を、`buildRole()` →
  `entityPacker`の節 → `listRelationRepos`と`sharedUserPacker`の節 → `buildCore()`にした。
  `buildCore()`は`roleService`を引数で受け、`core`の`role`にそのまま入れる。setterは動かして
  いない。移した節のうち構築のときに走るDBの文は、`proxyRemoteFiles`の写しの`metaRepo.Fetch()`
  だけで、元の位置との間には`buildCore()`(作るだけでDBにもRedisにも触らない)しか無いので、
  DBの文の順も購読の位置も変わらない
- **2段目はpackerの要らない8構築子・15行。** blocking(4行: `unfollower`・
  `followRequestCanceller`・`userListRepo`・`roomInvitationRemover`)、reaction(3行:
  `userRolesProvider`・`mediaSilenceChecker`・`blockingChecker`)、signup(2行。
  `ticketConsumption`と`applicationSettlement`で、引数は`db`・`ticketRepo`・
  `signupApplicationRepo`の3つ)、clip(2行: `rolePolicyProvider`・`streamPublisher`)、
  antennaの`rolePolicyProvider`、pollの`blockingChecker`、moderationlogとnotificationの
  `background`(ここまで各1行)
- **2段目で、`buildCore()`の中のblockingの構築をreactionより前へ移した。** reactionの
  `blockingChecker`に渡すため。blockingが使う値(repository・`followingService`・`chatRepo`)は、
  どれもその前で揃う。clipの`streamPublisher`は、`setupRoutes()`の先頭で作る`streamPubSub`を
  `buildCore()`の引数で受けて、その場で`stream.NewClipStreamPublisher(streamPubSub)`で作る
  (作っても購読も接続もしない)
- **3段目はpackerの要る7構築子・17行。** noteCreate(6行: `entityPacker`・`background`・
  `blockingRepo`・`metaRepo`・`silencingProvider`・`driveFileRepo`)、following(4行: `entityPacker`・
  `silencedHostChecker`・`silentFollowRepo`・`userPacker`)、drive(3行: `entityPacker`・`roleChecker`・
  `moderationLog`。`moderationLog`は`modLog`と`modLogUsers`の2つの引数)、userの`entityPacker`、
  webhookのnote・reactionの`entityPacker`とfollowingの`userPacker`(ここまで各1行)。
  `buildCore()`は`entityPacker`と`sharedUserPacker`も引数で受ける
- **`internal/cli/backfill`(`elythia backfill emoji-system-file`)は、driveのserviceに
  `entityPacker`・`roleChecker`・`moderationLog`を渡さない(明示的にnil)。** 呼ぶのは
  `CopyToSystemFile`と`DeleteSystemFile`だけで、複製は利用者の無いsystem fileなので、ロールの
  policyの検査もstreamへのpublish(ここでPackerを使う)も通らない。#3825より前もsetterを
  呼ばず、どれもnilのまま動いていた
- **依存を入れる時点が、元のsetterの位置から`buildCore()`の位置へ前へ移っただけ。**
  2段目の元のsetterはどれもstreamの節(最初の`Subscribe`)より前にあり、`buildCore()`から元の
  setterまでの間に、`Subscribe`・`Connect…`・`Start`・goroutineの起動は無い(行を突き合わせて
  確かめた)。3段目の`driveService.SetModerationLog`だけは`Subscribe`より後にあったが、購読の
  handler(`metaUpdated`・`federationRulesUpdated`・`rolesUpdated`・`antennaUpdated`と、
  `cachebus`・`corerelationcache`の`Connect…`)が触るのはmeta・連合のルール・ロール・アンテナ・
  関係・インスタンスの停止・relay・認証・利用者・アバターデコレーション・絵文字のキャッシュと、
  metaとnodeinfoのhandler・captcha・リアクションのバッファで、driveのserviceには届かない。
  pubsubの購読のhandlerから辿れる配線は変わらない(4.1)
- **外部パッケージのテストは、`export_test.go`の`…ForTest`で後から入れる。** 補助関数が作った
  serviceに後から入れていたテスト(`core/blocking`・`core/reaction`・`core/clip`・`core/poll`と、
  3段目の`core/note`・`core/following`・`core/drive`・`core/user`の外部パッケージのテスト)に
  置いた。テストの中で構築子を呼んでいる箇所と、別のパッケージの
  テストは構築子で渡す。同じパッケージのテストはフィールドへ直接代入する(#3817と同じ)
- **置き換えなかったもの。** 循環なので残す`following.blockingChecker`・
  `following.meUpdatedPublisher`・`userPacker.detailExtras`・`userpack.detailExtrasMany`(#3812で
  残すと決めた行)、既に型で必須の`sharedUserPacker.entityPacker`、段(8)で扱う
  `entityPacker.mediaURL`。表の行もsetterも元のまま
- **nilを渡す形は、`TestBuiltHandlersHaveNoNilDependencies`を広げて見る。** `buildCore()`で作る
  部品のうち置き換えたものを、`Server`のフィールド`builtCore`(型は`coreParts`、
  `internal/server/core_parts.go`)に持たせる。`core`の欄を全部は並べない。欄の名前は
  `setupRoutes()`の別名(`blockingService`など)にした。`core`のフィールド名
  (`blocking`・`signup`など)は`apiHandlers`・`routeParts`の欄とぶつかり、許可リストの
  key (`<欄>.<フィールド>`)が区別できないため。テストは3つの入れ物の欄の名前が重ならないことも
  確かめる。許可リストに足したのは、clipの`pendingUpdated`(依存ではなく状態で、最初の
  `publishUpdated`が作る)と`afterFunc`(テストだけが差し替えるseamで、nilのとき
  `time.AfterFunc`を使う)の2つと、3段目のdriveの`sensitiveDetector`(`nsfwDetectorUrl`を
  設定しないときは`setupRoutes()`がnilを入れる既定の構成で、テストの組み立ても設定しない)

| | #3825の前(`5b7e06b1bf`) | 2段目の後 | 3段目の後 |
|---|---|---|---|
| `criticalWiringCount` | 104 | 89 | 72 |
| `setupRoutes()`の本文で、行頭が`<変数>.Set`の行 | 680 | 664 | 647 |
| その受け手 | 112 | 111 | 110 |

数え方: 上の表と同じ。減った受け手は、置き換えたsetterしか呼んでいなかった`modLogService`
(2段目)と`webhookFollowingHook`(3段目)。行頭が`<名前>Handler.Set`の行(342)と、その受け手(40)は変わらない
(handlerは置き換えていない)。

##### 段(4): streamの部品と、前の段から回した行(#3827)

**streamの部品と、段(1)・段(2)から回した行も、同じ形にした。** #3812の段(4)。置き換えたのは、
表の行で見ていた依存だけ。

- **1段目で、`buildStream()`の呼び出しを`buildHandlers()`の直前へ動かした。** `i`と`oauth`の
  handlerの`streamRevoker`は、`buildHandlers()`(と`oauth`の構築子)より後の`buildStream()`で作る
  `streamRevokePublisher`を受けていた。`buildStream()`が使うのは先頭で作る`streamPubSub`と`idGen`だけ
  (1段目の時点。3段目で`entityPacker`と4つのlookupも受けるようにした)で、
  中の構築子はどれも値を作って返すだけ(Redisに繋がず、購読もgoroutineも始めない。上の
  「stream の部品 (#3692)」)。動かしたのは呼び出しの1行だけで、`w.stream`への代入・別名・
  `s.streamManager`への代入・setter・購読(`Subscribe…`)はstreamの節に残した。動かした区間に
  `streamSvcs`を読み書きする行は無い(呼び出しより前には書けない)ので、購読の位置も、購読の
  handlerから辿れる配線も変わらない(4.1)
- **2段目は`i`のhandlerの10行と`oauth`の`streamRevoker`(2構築子・11行)。** `i`は`noteHider`・
  `entityPacker`・`roleProvider`・`totpReplayGuard`・`twoFAPendingGuard`・`authInvalidator`・
  `accessTokenRepo`・`streamRevoker`・`followRequestBulkAccepter`・`background`。`buildHandlers()`は
  `twoFAPendingGuard`と`streamRevokePublisher`も引数で受ける(`hSvcs := s.buildHandlers(w, coreSvcs,
  fedSvcs, idGen, noteHider, entityPacker, sharedUserPacker, metaRepo, abuseNotifStates,
  abuseReportRepoForNotif.CountUnresolved, signupApplicationNotifLookup, twoFAPendingGuard,
  streamRevokePublisher)`)。2FAのpending guard(`coretwofactor.NewRedisPendingGuard(s.redis.Default)`)は
  `setupRoutes()`で作って渡す。`buildHandlers()`の中で`s.redis`を読むと、Redisの無い`Server`で
  組み立てる`TestBuildHandlersFillsEveryField`がpanicするため。guardの構築子はRedisに触らない。
  `streamRevokePublisher`の別名(`streamRevokePublisher := streamSvcs.streamRevokePublisher`)は、
  `i`と`oauth`が構築子で受けるので、`buildStream()`の呼び出しの直後へ移した(ほかの別名はstreamの節)。
  `buildHandlers()`は`Server`のフィールドの`s.auth`も読むようになった
- **3段目はstreamの部品の5構築子・8行。** `stream.NewManager`の4行(`hardMuteLookup`・
  `muteBlockLookup`・`policyProvider`・`ugcVisibilityLookup`)、`NewNotePublisher`・
  `NewNotificationPublisher`の`entityPacker`、`NewServerStatsPublisher`の`serverStatsEnabled`
  (判定の関数)、`NewReversiGamePublisher`の`entityPacker`(ここまで各1行)。`buildStream()`は
  `entityPacker`と4つのlookupも引数で受ける(`streamSvcs := buildStream(streamPubSub, idGen,
  entityPacker, &hardMuteLookupAdapter{…}, &muteBlockSnapshotAdapter{…}, roleService, func()
  string { return metaUGCVisibility(metaRepo) })`)。lookupのadapterとclosureは、元のsetterと
  同じ式で呼び出しの引数の中で作る。`reversiPublisher`は`entityPacker`より前(`setupRoutes()`の
  先頭、#3676)で作っていたので、`entityPacker`の直後へ移した。使う側(連合のprocessor・reversiの
  serviceとhandler)はどれもそれより後にある。serverStatsのpublisherはstreamの節の元の位置で、
  判定を構築子に渡す
- **4段目は`feed`と`peer`(新しい構築子2つ・2行)。** どちらもsetterではなく構造体リテラルで
  組んでいたので、構築子`newFeedHandler`(`internal/server/feed.go`)と`newPluginPeerDeps`
  (`internal/server/plugin_peer.go`)を作り、本番の組み立てだけで使う。置き換えた行は
  `feed.ugcVisibility`と`peer.blocker`だが、構築子はリテラルに書いていた欄をすべて引数で受ける。
  テストが差し替えるseam(`feed`の`now`、`peer`の`urlFor`)は受けない。パッケージ内のテストは、
  要る欄だけを書いたリテラルのまま残した(`pluginPeerDeps`のリテラルは#3827の前で42個あり、多くは
  1〜3欄しか書かない。`HasBlocker`のテストの2個を消して40個。`internal/server`の`_test.go`に
  `grep -o 'pluginPeerDeps{'`を当てて数えた)。構築子が引数を捨てないことは`TestNewFeedHandlerKeepsDependencies`と
  `TestNewPluginPeerDepsKeepsDependencies`が見る。作る位置は元のリテラルの位置のままで、
  リテラルを構築子の呼び出しに替えただけなので、動きは変わらない
- **依存を入れる時点が、元のsetterの位置から構築子の位置へ前へ移っただけ。** 元の`i`のsetterは
  `buildHandlers()`の後のAccount endpointsの節と、streamの節(`SetStreamRevoker`。購読の後)にあった。
  購読のhandler(`metaUpdated`・`federationRulesUpdated`・`rolesUpdated`・`antennaUpdated`と、
  `cachebus`・`corerelationcache`の`Connect…`)が触るのはキャッシュとmeta・nodeinfoのhandlerで、
  `i`と`oauth`のhandlerには届かない。どちらもHTTPのリクエストからしか呼ばれない。3段目の
  managerの4つのsetterとnote・notificationのpublisherの`SetEntityPacker`はstreamの節にあり、
  managerのsetterは購読(`SubscribeWordMuteReload`など)の前、`notePublisher.SetEntityPacker`は
  購読の後だった。購読のhandlerのうちmanagerが持つもの(word muteとrelationの取り直し、
  broadcast、失効)は、接続ごとのsnapshotの取り直しと転送・切断で、どれもmanagerを作った後に
  始まる。構築子へ移すと、その時点で4つのlookupは入っているので、購読の前にlookupが揃って
  いることは変わらない。publisherはどれも購読のhandlerから呼ばれない(呼ぶのはcoreのservice
  と連合のprocessorで、リクエストと受信のときだけ)。`reversiPublisher`の`SetEntityPacker`は
  `buildCore()`の後(streamの節より前)にあった。serverStatsの判定は、`StartBackground`で始まる
  tick(4.3)と、接続を受けた後の`requestLog`(`Log`)からしか読まない。どちらも構築の後で、
  構築子と元のsetterは同じ節で隣り合っていた
- **wiring-checkの照合も直した(4段目)。** `TestPluginPeerRateLimiterIsWired`と
  `TestPluginPeerEnqueuerIsWired`は、構造体リテラルの欄(`limiter: newPeerRateLimiter()`・
  `enqueuer: s.queueClient`)の照合をやめ、`s.peerDeps = newPluginPeerDeps(...)`の呼び出しを
  引数まで含めて照合する(引数の順を取り違える形も落とす)
- **wiring-checkの照合も直した(3段目)。** `TestStreamRevokeIsWired`は`buildStream()`の呼び出しを、
  足した引数まで含めて照合する。`TestServerStatsGateIsWired`は、setterの照合をやめ、
  `stream.NewServerStatsPublisher`に渡す判定の中身を照合する(判定を反転させた関数も同じ型で
  渡せるので、型でもnilの検査でも捕まらない)
- **wiring-checkの照合も直した(2段目)。** `TestTwoFAPendingGuardIsWired`(`internal/entitycompat`)は、
  setterの照合をやめ、Redisで実装したguardを作る行と、それが`buildHandlers()`の呼び出しを通って
  `i.NewHandler(...)`へ渡る行を照合する(同じ型の数えないguardを渡す形は、型でもnilの検査でも
  捕まらない)。`emoji_decoration_cache_wiring_test.go`の`buildHandlers()`の呼び出しの照合と、
  `TestBuildHandlersFillsEveryField`の呼び出しを新しい署名に直した
- **外部パッケージのテストは、構築子で渡す。** `i`・`oauth`・`stream`のテストはどれも同じ
  パッケージにあるので、補助関数が作った部品に後から入れていた箇所はフィールドへ直接代入する
  (#3817と同じ)。ほかのパッケージから構築子を呼ぶテスト(`core/timeline`のベンチの
  `NewNotePublisher`、`internal/server`の`NewManager`)はnilを渡す。`export_test.go`の`…ForTest`は
  足していない。`TestNewHandlerKeepsDependencies`を`i`に足し、`oauth`には`streamRevoker`を足した。
  `stream`には、5つの構築子が引数を持たせることを見るテスト(`constructor_test.go`)を足した
- **nilを渡す形は、`TestBuiltHandlersHaveNoNilDependencies`を広げて見る。** `requiredDepHandlers`に
  `i`を足した。`oauth`は段(2)から`routeParts`にある。テストの組み立て(test mode・miniredis)では、
  `i`のnilになりうるフィールドはどれもnilでなかったので、許可リストには足していない。3段目で
  streamの部品5つ(`streamManager`・`notePublisher`・`notificationPublisher`・`serverStatsPub`・
  `reversiPublisher`)を`routeParts`に足した。欄の名前は`setupRoutes()`の別名(reversiの欄は
  handlerなので`reversiPublisher`)。許可リストに足したのは`notePublisher.cachedBody`の1つで、
  依存ではなく状態(直前にpackした投稿のJSON)。最初に投稿を流すとき(`packNote`)に作るので、
  組み立てた直後はnil。4段目で`feed`と`peer`を`routeParts`に足し、許可リストに`feed.now`と
  `peer.urlFor`を足した。どちらもテストだけが入れるseamで、構築子は入れず、nilのとき既定
  (`time.Now`・httpsのURL)を使う

| | #3827の前(`19f512bf20`) | 2段目の後 | 3段目の後 | 4段目の後 |
|---|---|---|---|---|
| `criticalWiringCount` | 72 | 61 | 53 | 51 |
| `setupRoutes()`の本文で、行頭が`<名前>Handler.Set`の行 | 342 | 331 | 331 | 331 |
| そのsetterを呼ぶhandler(受け手) | 40 | 39 | 39 | 39 |
| `setupRoutes()`の本文で、行頭が`<変数>.Set`の行 | 647 | 636 | 628 | 628 |
| その受け手 | 110 | 109 | 107 | 107 |

数え方: 上の表と同じ。1段目は呼び出しを動かしただけで、どの数も変わらない。2段目で減った受け手は、
置き換えたsetterしか呼んでいなかった`oauthHandler`。3段目で減った受け手は`reversiPublisher`と
`serverStatsPub`。4段目はsetterを置き換えていない(構造体リテラルを構築子に替えた)ので、setterの
行は変わらない。

##### 段(5): queueのprocessor(#3829)

**queueのprocessorも、同じ形にした。** #3812の段(5)。置き換えたのは、表の行で見ていた依存だけ。

- **1段目で、`deleteAccountProcessor`と`postScheduledNoteProcessor`を`buildQueue()`から出し、
  `setupRoutes()`で作るようにした。** 置き換える依存のうち、Web Pushの購読キャッシュ
  (`webPushCache`)とRedisのlock(`processors.NewRedisScheduledNoteLock(s.redis.Default, …)`)は、
  `queueProcs`の規則(依存はrepository・core・設定・DB・ID生成器に限る。上の「queue の processor
  (#3690)」)の外にあるため。作る位置は元のsetterの位置で、deleteAccountは`SetUserRepo`の直前、
  postScheduledNoteは`SetLock`の直前。構築子は構造体を作るだけで、`buildQueue()`の呼び出しから
  新しい位置までの間に、2つを使う行は無い(`setupRoutes()`の本文をgrepで確かめた)。handlerの
  登録(`queueHandlerProcs`)はどちらよりも後にある。`queueProcs`の欄と`setupRoutes()`の別名を消し、
  setterは元の位置のまま残した。構築は副作用を持たないので、DBとRedisの文の順は変わらない
- **2段目は`deleteAccount`と`postScheduledNote`(2構築子・3行)。** `NewDeleteAccountProcessor`は
  `pageRepo`と`pushCache`を、`NewPostScheduledNoteProcessor`はRedisのlockを受ける
  (`processors.NewDeleteAccountProcessor(noteRepo, driveFileRepo, followingRepo, pageRepo,
  webPushCache)`、`processors.NewPostScheduledNoteProcessor(noteDraftRepo, userRepo,
  noteCreateService, processors.NewRedisScheduledNoteLock(s.redis.Default, s.config.Redis.KeyPrefix(),
  0))`)。lockは`setupRoutes()`で作る。構築子は構造体を作るだけで、Redisに触らない。
  `deleteAccountProcessor.SetUserRepo`と`postScheduledNoteProcessor.SetNotifier`は表に無いので、
  setterのまま残した
- **依存を入れる時点は変わらない。** 1段目で、構築子を元のsetterの位置(の直前)へ動かしてある
- **置き換えたsetterと、起動時の検査に使っていた`Has…`は消した。** 外部パッケージ
  (`processors_test`)の`delete_account_test.go`は構築子で渡す(`processors.NewDeleteAccountProcessor(`を
  呼ぶ行は25から23。`delete_account_test.go`に`grep -c`を当てた。`HasPageRepo`と`HasPushSubscriptionCache`だけを見ていた2か所を消した)。
  同じパッケージの`post_scheduled_note_test.go`はフィールドへ直接代入する(#3817と同じ)。
  `wiring_predicate_test.go`から`HasLock`のテストを消した。構築子が引数を捨てないことは、
  `internal/queue/processors/constructor_test.go`が見る
- **wiring-checkの照合も直した。** `TestPushSubscriptionCacheIsShared`(`internal/entitycompat`)は、
  `deleteAccountProcessor.SetPushSubscriptionCache`の引数の照合をやめ、`processors.NewDeleteAccountProcessor`の
  最後の引数を照合する(購読キャッシュをもう1つ作って渡す形は、型でもnilの検査でも捕まらない)。
  補助関数の単体テスト`TestPushSubscriptionCacheProblemsRequireOneFile`の合成のソースも直した
- **nilを渡す形は、`TestBuiltHandlersHaveNoNilDependencies`を広げて見る。** `routeParts`に
  `deleteAccountProcessor`と`postScheduledNoteProcessor`を足し、`requiredDepChecks`に置き換えた3つの
  欄を足した。テストの組み立て(test mode・miniredis)では、2つのnilになりうるフィールドはどれも
  nilでなかったので、許可リストには足していない
- **3段目は`deliver`(1構築子・4行)。** `NewDeliverProcessor`は`deliveryGate`・`keySource`・
  `suspendedChecker`・`metadataRefresher`と統計の判定`statsEnabled`を受ける
  (`deliverProcessor := processors.NewDeliverProcessor(apClient, instanceService,
  processors.NewRepoSigningKeySource(keypairRepo, keypairExtraRepo),
  corefederation.NewSuspendedChecker(metaRepo, instanceRepo), metadataRefresher,
  corefollowing.MetaInstanceStatsGate(metaRepo))`)。渡す値はどれも元の`NewDeliverProcessor`の
  呼び出しより前で作ってあり、引数の中で作る3つ(署名鍵の解決元・配送停止の判定・統計の判定)は
  構造体かclosureを作るだけで、DBにもRedisにも触らない。`SetChartHook`・`SetResponseHook`・
  `SetRedis`・`SetSignatureCapabilityRecorder`と、queueロールでだけ呼ぶ`SetDeliveryTelemetry`・
  `SetDeliveryBreaker`は表に無いので、setterのまま残した
- **依存を入れる時点が、元のsetterの位置から構築子の位置へ前へ移っただけ。** 構築子から最後に
  置き換えたsetter(`SetMetadataRefresher`)までの間は、同じprocessorの残したsetterだけで、
  `Subscribe`・`Connect…`・`Start`・goroutineの起動は無い。processorを呼ぶのはqueueのworkerで、
  workerは組み立ての後に始まる
- **外部パッケージのテストは構築子で渡す。** `NewDeliverProcessor`を呼ぶテスト(87か所。
  `processors`の86と`core/cachebus`の1。`_test.go`に`grep -c 'NewDeliverProcessor('`を当て、
  `internal/server/wiring_queue_test.go`のコメントの1行を除いた)は、置き換えたsetterを直後で呼んでいたものは引数へ移し、
  それ以外はnilを渡す。同じパッケージの`deliver_keysource_test.go`なども構築子で渡す。
  `wiring_predicate_test.go`から`deliver`の`Has…`のテストを消した。構築子が引数を捨てないことは
  `constructor_test.go`が見る
- **wiring-checkの照合も直した。** `TestInstanceStatsGateIsWired`の
  `deliverProcessor.SetMetadataRefresher(metadataRefresher, corefollowing.MetaInstanceStatsGate(metaRepo))`は、
  構築子の呼び出しを引数まで照合する形に直して残した(判定を反転させた関数も同じ型で渡せる)。
  配送停止の判定(`NewSuspendedChecker`)と受信の拒否の判定(`NewInboxSuspendedChecker`、
  `inboxSoftwareChecker`)は同じ型`*corefederation.SuspendedChecker`を返し、取り違えは型でも
  nilの検査でも捕まらないので、`TestQueueProcessorSoftwareCheckersAreWired`
  (`internal/entitycompat/software_suspension_wiring_test.go`)を足して、同じ呼び出しを照合する。
  `make wiring-check`の`-run`にも足した
- **`routeParts`に`deliverProcessor`を足した。** `requiredDepChecks`に置き換えた5つの欄
  (`statsEnabled`を含む)を足した。許可リストに足したのは`deliverProcessor.telemetry`と
  `deliverProcessor.breaker`の2つ。`setupRoutes()`はqueueロール(`s.role.RunsQueue()`)でだけ
  配線し、テストの組み立ては`MK_ONLY_SERVER=1`でqueueロールを持たない
- **4段目は`inbox`(1構築子・5行)。** `NewInboxProcessor`は`verifier`・`hostBlocker`・
  `softwareBlocker`・`metadataRefresher`・`replayGuard`を受ける
  (`inboxProcessor := processors.NewInboxProcessor(federationProcessor, federationResolver,
  instanceService, inboxSoftwareChecker, metadataRefresher,
  processors.NewRedisInboxReplayGuard(s.redis.Default))`)。replay guardは元のsetterと同じ式で、
  構築子の引数の中で作る。guardの構築子はRedisに触らない。`s.redis.Default`(`*redis.Client`)がnilのときも、
  `redis.UniversalClient`で受けた値はnilでないので、中のclientがnilのguardを返す。interfaceの
  引数としてはnilでない。これは元の`SetInboxReplayGuard`と同じで、以前の
  `HasInboxReplayGuard`も見分けていなかった。`SetChartHook`・`SetRelayActorChecker`・
  `SetInstanceTracker`・`SetSignatureCapabilityRecorder`・`SetLDSignatureVerifier`と、queueロールで
  だけ呼ぶ`SetDeliveryTelemetry`は表に無いので、setterのまま残した。`api/inbox`のhandlerと
  `core/federation`のresolverの`HasHostBlockChecker`・`HasSoftwareSuspensionChecker`は段(7)で扱うので
  残した
- **依存を入れる時点は、元のsetterの位置から構築子の位置へ前へ移っただけ。** 構築子から最後に
  置き換えたsetter(`SetInboxReplayGuard`)までの間は、同じprocessorの残したsetterだけ。
  processorを呼ぶのはqueueのworkerで、workerは組み立ての後に始まる
- **外部パッケージのテストは構築子で渡す。** `NewInboxProcessor`を呼ぶテスト(70か所。`_test.go`に
  `grep -c 'NewInboxProcessor('`を当てた。`inbox_bench_test.go`を含む)は、置き換えたsetterを直後で
  呼んでいたものは引数へ移し、それ以外はnilを渡す。`wiring_predicate_test.go`は、`Has…`のテストが
  無くなったので消した(使っていた`stubKeySource`は`deliver_keysource_test.go`にある)
- **wiring-checkの照合も足した。** `TestQueueProcessorSoftwareCheckersAreWired`に、`NewInboxProcessor`の
  呼び出しの照合と、`internal/server`の中で`inboxSoftwareChecker`へ代入する文の右辺が、どれも
  `NewInboxSuspendedChecker(...)`(`buildFederation()`)か`fedSvcs.inboxSoftwareChecker`(別名)である
  ことの検査を足した(3段目の配送の照合と合わせて、2つの判定の取り違えを落とす)。別名は`router.go`と
  `wiring_handlers.go`にあり、ファイルを名指ししないよう(#3672)パッケージ全体の代入を見る。どちらの
  形も1つ以上あることを下限にした
- **`routeParts`に`inboxProcessor`を足した。** `requiredDepChecks`に置き換えた5つの欄を足した。
  許可リストに足したのは`inboxProcessor.telemetry`の1つで、理由は`deliverProcessor.telemetry`と同じ

| | #3829の前(`7ec218bbfd`) | 1段目の後 | 2段目の後 | 3段目の後 | 4段目の後 |
|---|---|---|---|---|---|
| `criticalWiringCount` | 51 | 51 | 48 | 44 | 39 |
| `setupRoutes()`の`processors.New…`の呼び出し | 14 | 16 | 16 | 16 | 16 |
| `buildQueue()`の`processors.New…`の呼び出し | 16 | 14 | 14 | 14 | 14 |
| `setupRoutes()`の本文で、行頭が`<変数>.Set`の行 | 628 | 628 | 625 | 621 | 616 |
| その受け手 | 107 | 107 | 107 | 107 | 107 |

数え方: `criticalWiringCount`と`<変数>.Set`の行は上の表と同じ。`processors.New…`の呼び出しは、
上の「queue の processor (#3690)」の表と同じく、本文に`grep -oE '\bprocessors\.New[A-Z]'`を当てた。
2段目から4段目で受け手が減らないのは、置き換えたprocessorのどれにも表に無いsetterが残っているため。
`processors.New…`の数には、構築子の引数の中で作る`NewRepoSigningKeySource`なども入る。

##### 段(6): notesのhandler(#3831)

**notesのhandlerも、同じ形にした。** #3812の段(6)。置き換えたのは、表の行で見ていた依存だけ。

- **1段目で、`noteHider`を`notesHandler`より前で作るようにした。** `notes.NewHandler`の構築子で
  受けるため。`notehide.NewHider(followingRepo, …)`は値を作るだけで、DBにもRedisにも触らない。
  使う`followingRepo`と`metaRepo`は、どちらも`setupRoutes()`の先頭で作ってある。
  `notesHandler.SetNoteHider`は元の位置のまま残した
- **2段目は投稿の見え方を決める4行。** `notes.NewHandler`は、元の9個の引数に続けて`noteHider`・
  `entityPacker`・`policyProvider`・`ugcVisibility`(ugcVisibilityForVisitorを毎回引く関数)を受ける
  (`notes.NewHandler(…, idGen, noteHider, entityPacker, roleService, func() string { return
  metaUGCVisibility(metaRepo) })`)。関数は元の`SetUGCVisibilityLookup`と同じ式。どれも
  `notesHandler`より前で作ってある(`noteHider`は1段目で動かした)
- **依存を入れる時点が、元のsetterの位置から構築子の位置へ前へ移っただけ。** 一番後ろにあった
  `SetEntityPacker`は`buildHandlers()`の呼び出しの後にあり、間には`buildStream()`がある。
  `buildStream()`と`buildHandlers()`は`notesHandler`を受け取らないので、notesのhandlerに触れる
  行は無い。notesのhandlerはHTTPのリクエストからしか呼ばれず、リクエストは構築の後にしか来ない
- **固定値の`ugcVisibility`の欄は消し、毎回引く関数(`ugcVisibilityFn`)の1本にした。** 固定値を
  入れていたのは`SetUGCVisibility`(Deprecated)を呼ぶテストの2か所だけで、どちらも
  `h.ugcVisibilityFn = func() string { return "none" }`に置き換えた。関数がnilのときは空文字
  (`all`と同じ扱い)を返すので、挙動は変わらない
- **置き換えたsetter(`SetUGCVisibility`を含む5本)と、起動時の検査に使っていた`Has…`の4本は消した。**
  使われなくなった`noteHiderMissingEffect`(`wiring_check.go`)も消した。テストの`NewHandler`の
  呼び出し(66か所。`internal/api/notes`の`_test.go`に`grep -c 'NewHandler('`を当てた)は、
  置き換えた引数にnilを補う補助関数`newHandlerNilDeps`(`constructor_test.go`)に寄せた。
  置き換えたsetterを呼んでいたテスト(21か所)は、フィールドへ直接代入する。`entity_packer_test.go`
  (`HasEntityPacker`のテスト)と、`wiring_predicate_test.go`の`HasPolicyProvider`・`HasUGCVisibility`の
  テストは消した。構築子が引数を捨てないことは`TestNewHandlerKeepsDependencies`が見る
- **nilを渡す形は、`TestBuiltHandlersHaveNoNilDependencies`を広げて見る。** `routeParts`に`notes`
  (notesのhandler)を足し、`requiredDepChecks`に置き換えた4つの欄を足した。setterで入れる欄も
  同じ規則で見る。テストの組み立て(test mode・miniredis)でnilだったのは`timelineCache`の1つで、
  許可リストに足した。`enableTimelineCache`(`MK_ENABLETIMELINECACHE`)を有効にしたときだけ
  `EnableTimelineJSONCache`で作り、既定は無効
- **3段目は可視性のfilterに使うrepositoryの7行。** `notes.NewHandler`は、2段目の13個に続けて
  `driveFileRepo`・`metaRepo`・`blockingRepo`・`mutingRepo`・`renoteMutingRepo`・
  `channelMutingRepo`・`userFollowingRepo`を受ける(`setupRoutes()`では`followingRepo`を渡す)。
  どれも`setupRoutes()`の先頭で作ってあり、元のsetterは構築子の直後の数行にあったので、依存を
  入れる時点はほぼ変わらない(間にあるのは同じhandlerの残したsetterだけ)
- **置き換えたsetter 7本と`Has…`の7本は消した。** テストの補助関数`newHandlerNilDeps`は、
  置き換えた引数に続けてnilを補う。置き換えたsetterを呼んでいたテスト(73か所)は、フィールドへ
  直接代入する。`wiring_predicate_test.go`(`Has…`のテスト)と、`setters_test.go`の
  `SetDriveFileRepo`のテストは消した。`TestNewHandlerKeepsDependencies`に7つを足した。repositoryは
  どれも別のinterfaceなので、引数の取り違えはほぼ型で落ちる。同じmockが2つのinterfaceを満たす
  形は、`assert.Same`で捕まえる
- **`requiredDepChecks`に置き換えた7つの欄を足した。** 許可リストは2段目から変わらない
  (`notes.timelineCache`の1つ)

| | #3831の前(`988d002543`) | 1段目の後 | 2段目の後 | 3段目の後 |
|---|---|---|---|---|
| `criticalWiringCount` | 39 | 39 | 35 | 28 |
| `setupRoutes()`の本文で、行頭が`notesHandler.Set`の行 | 33 | 33 | 29 | 22 |
| `setupRoutes()`の本文で、行頭が`<名前>Handler.Set`の行 | 331 | 331 | 327 | 320 |
| そのsetterを呼ぶhandler(受け手) | 39 | 39 | 39 | 39 |
| `setupRoutes()`の本文で、行頭が`<変数>.Set`の行 | 616 | 616 | 612 | 605 |
| その受け手 | 107 | 107 | 107 | 107 |

数え方: 段(1)・段(4)の表と同じ。`notesHandler.Set`の行は、同じ本文で行頭の空白の後が
`notesHandler.Set`の行。2段目と3段目で受け手が減らないのは、notesのhandlerに表に無いsetter
(`SetPiningRepo`・`SetTranslator`など)が残っているため。

##### 段(7): 連合(#3833)

**連合の部品も、同じ形にした。** #3812の段(7)。置き換えたのは、表の行で見ていた依存だけ。

- **1段目は、テストの組み立てを補助関数に寄せただけ。** `internal/core/federation`のテストの
  `NewResolver`の呼び出し(338か所。`federation_test`の316と`package federation`の22)と
  `NewProcessor`の呼び出し(38か所。すべて`federation_test`)を、今の構築子をそのまま包む
  `NewResolverNilDeps`・`NewProcessorNilDeps`(`nil_deps_test.go`)へ機械的に寄せた。後の段で
  構築子の引数が増えても、直すのは補助関数の中だけで済む。`package federation`の`_test.go`に
  置いたので、外部テスト(`federation_test`)からも呼べる
- **2段目は`instance.hostBoundNodeinfoFetcher`(1構築子・1行)。** nilではなく、渡した取得元が
  redirectの飛び先を返す取得(`FetchJSONWithFinalURL`)を持つかを見ていた行で、引数の型で受ける
  ようにした。`coreinstance.NewFetchMetadataService`は`HTTPFetcher`に代えて
  `HostBoundHTTPFetcher`(`HTTPFetcher`と`FetchJSONWithFinalURL`)を受け、optional interfaceで
  拾っていた`hostBoundJSONFetcher`と、持たない実装で`FetchJSON`へ落ちるfallbackは消した。本番の
  `apFetcher`(`*corefederation.APFetcher`)は元から実装している。テストの偽の取得元のうち持って
  いなかった2つ(`panicFetcher`・`handshakeFetcher`)には、`FetchJSON`を呼んで最終URLを空文字で
  返すメソッドを足した(最終URLが空なら検査をskipするので、元のfallbackと同じ動き)。構築子が
  引数を捨てないことは`internal/core/instance/constructor_test.go`が見る
- **`routeParts`に`metadataFetcher`を足した。** `requiredDepChecks`に`metadataFetcher.fetcher`を
  足した。テストの組み立てでnilになるフィールドは無かった
- **3段目は配送の`DeliverService`と、ノートの削除を配る`NoteDeleteDeliveryHook`(2構築子・3行)。**
  `NewDeliverService`は`hostBlocker`を、`NewNoteDeleteDeliveryHook`は`userRepo`と`noteRepo`を
  受ける。どちらも`buildFederation()`で作り、`instanceService`(`coreSvcs.instance`)と
  repositoryの別名を渡す。`buildFederation()`は`buildCore()`の後に呼ぶので、どれも揃っている
- **依存を入れる時点が、元のsetterの位置(`setupRoutes()`の`buildFederation()`より後)から
  構築子の位置へ前へ移った。** 間で配送する行は無い(配送はHTTPのリクエストと受信のworkerから
  起きる)。前へ移って変わりうるのは、配送先が減る(ブロック済みのhostを弾く)か増える(DMの
  宛先とrenoteした相手に届く)方向だけ。`deliverService.SetKeypairExtraRepo`・
  `SetPublickeyExtraRepo`は表に無いので、setterのまま残した
- **置き換えたsetterと`Has…`は消した。** `NewDeliverService`を呼ぶテスト(16か所。
  `internal/core/federation`の`_test.go`に`grep -c 'NewDeliverService('`を当てた)はnilを渡す。
  `SetHostBlockChecker`を呼んでいた6か所(`deliver_service_test.go`と`default_port_gate_test.go`)は、
  判定を構築子へ渡す補助関数`newDeliverServiceWithBlocker`に寄せた。`note_delete_delivery_hook_test.go`
  (外部テスト)の`SetUserRepo`・`SetNoteRepo`の20か所は、残りの引数を同じにして構築子で作り直す
  `WithRepos`(`export_test.go`)に置き換えた。`wiring_predicate_test.go`から配送の`HasHostBlockChecker`の
  テストを消した。構築子が引数を捨てないことは`internal/core/federation/constructor_test.go`が見る
- **`routeParts`に`deliverService`と`noteDeleteHook`を足した。** `requiredDepChecks`に置き換えた
  3つの欄を足した。テストの組み立てでnilだったのは`deliverService.syncDeliverHook`の1つで、
  許可リストに足した。連合のe2e(`tests/e2e-federation`)だけが`Server.SetSyncDeliverHookForTest`で
  入れるテスト用の口で、`setupRoutes()`は入れない
- **4段目は`api/inbox`のhandler(1構築子・3行)。** `inbox.NewHandler`は`expectedHost`・`enqueuer`・
  `hostBlocker`を受ける(`inbox.NewHandler(federationResolver, federationProcessor, s.config.Host,
  s.queueClient, instanceService)`)。元のsetterは構築子の直後の数行にあり、間にあるのは同じ
  handlerの残したsetterだけ。handlerはHTTPのリクエストからしか呼ばれない。`enqueuer`がnilの
  ときの同期の経路は、テストが使うので残した。`SetInstanceTracker`・
  `SetSignatureCapabilityRecorder`・`SetChartHook`は表に無いので、setterのまま残した
- **置き換えたsetterと`Has…`は消した。** テスト(すべて`package inbox`)の`NewHandler`の呼び出し
  3か所は空文字とnilを渡し、置き換えたsetterの18か所はフィールドへ直接代入する。
  `wiring_predicate_test.go`(`Has…`のテスト)は消し、構築子が引数を捨てないことは
  `constructor_test.go`が見る
- **`routeParts`に`inboxHandler`を足した。** `requiredDepChecks`に`enqueuer`と`hostBlocker`を足した。
  `expectedHost`は文字列でnilにならないので、空でないことを見る`requiredNonEmptyStrings`を足して
  名指しした。テストの組み立てでnilになるフィールドは無かった。表を空にすると検査が何も見なく
  なるので、空でないことを下限として要求する
- **自ホストの名前は、wiring-checkで照合する。** `expectedHost`には`s.config.Host`を渡す。以前は
  `SetExpectedHost`の名前が値の意味を示していたが、位置引数では呼び出し側から見えず、
  `s.config.URL`などの別の文字列を渡しても型と空でないことの検査を通る。
  `TestFederationHostValuesAreWired`(`internal/entitycompat`)が構築子の呼び出しを引数まで照合する。
  `make wiring-check`の`-run`にも足した
- **5段目は`core/federation`のprocessor(1構築子・3行)。** `corefederation.NewProcessor`は
  `localBaseURL`・`blockingService`・`background`を受ける(`…, userRepo, noteRepo, s.config.URL,
  blockingService, s.background)`)。`blockingService`(`coreSvcs.blocking`)は`buildCore()`で作り、
  元の`SetBlockingService`はreversiの節(`setupRoutes()`の後ろの方)にあった。依存を入れる時点が
  前へ移ったが、processorを呼ぶのはinboxのhandler(HTTPのリクエスト)とqueueのworkerだけで、
  どちらも組み立ての後に始まる。前へ移って変わりうるのは、受信したBlockを処理し、reversiの
  Inviteにブロック判定が掛かる方向だけ。残った`federationProcessor.Set…`の19行(`setupRoutes()`の
  本文で行頭が`federationProcessor.Set`の行)は表に無いので、setterのまま残した
- **テストの組み立ては1段目の補助関数の中だけを直した。** `NewProcessorNilDeps`は空文字とnilを
  渡す。置き換えたsetterを呼んでいたテスト(`SetLocalBaseURL`の35か所、`SetBlockingService`の
  4か所、`SetBackground`の1か所。すべて外部テストの`federation_test`)は、作った後でフィールドへ
  代入できないので、`nil_deps_test.go`の`SetLocalBaseURLForTest`・`SetBlockingServiceForTest`・
  `SetBackgroundForTest`に置き換えた(`_test.go`にあるので本番のコードからは呼べない。
  `quote_request_dispatch_test.go`はテストの途中で入れて、空のときと入れたときの両方を見る)。
  パッケージ外の`NewProcessor`の呼び出し(`internal/api/inbox`の3か所と`internal/queue/processors`の
  2か所)は、空文字とnilを渡す。構築子が引数を捨てないことは`constructor_test.go`が見る
- **`routeParts`に`federationProcessor`を足した。** `requiredDepChecks`に`blockingService`と
  `background`を、`requiredNonEmptyStrings`に`localBaseURL`を足した。テストの組み立てでnilになる
  フィールドは無かった
- **自ホストのURLも、wiring-checkで照合する。** `localBaseURL`には`s.config.URL`を渡す。4段目の
  `expectedHost`と同じく、`s.config.Host`などを渡しても型と空でないことの検査を通るので、
  `TestFederationHostValuesAreWired`に`NewProcessor`の呼び出しの照合を足した
- **6段目は`core/federation`のresolver(1構築子・8行)。** `corefederation.NewResolver`は
  `hostBlocker`・`softwareBlocker`・`silencedChecker`・`rolePolicyProvider`・`suspensionOriginRepo`・
  `background`を受ける(`…, idGen, instanceService, inboxSoftwareChecker, instanceService, roleService,
  suspensionOriginRepo, s.background)`)。禁止語とセンシティブワードの2行は、nilではなく
  `hostBlocker`が`ProhibitedWords()`・`SensitiveWords()`を持つかを見ていた行で、引数の型で受ける
  ようにした。`hostBlocker`は`HostBlockChecker`に代えて`ResolverHostBlocker`(`HostBlockChecker`・
  `ProhibitedWordsProvider`・`SensitiveWordsProvider`)を受け、optional interfaceで拾うfallbackと
  `SetProhibitedWordsProvider`・`SetSensitiveWordsProvider`を消した。本番の`instanceService`は3つとも
  実装している。`hostBlocker`がnilなら(テスト)、今と同じく3つの判定をどれもskipする
- **渡す値はどれも`NewResolver`の呼び出しより前で作ってある。** `suspensionOriginRepo`は
  `setupRoutes()`の先頭、`roleService`は`buildRole()`、`instanceService`は`buildCore()`、
  `inboxSoftwareChecker`は`buildFederation()`。依存を入れる時点が元のsetterの位置から前へ移ったが、
  間にあるのは構築とsetterだけで、resolverを呼ぶのは組み立ての後に始まるHTTPのリクエストと
  queueのworkerだけ。前へ移って変わりうるのは、取り込みの判定が掛かる方向だけ。残った
  `federationResolver.Set…`の19行は表に無いので、setterのまま残した
- **テストの組み立ては1段目の補助関数の中だけを直した。** `NewResolverNilDeps`は6つのnilを渡す。
  置き換えたsetterを呼んでいた外部テストは、processorと同じく`nil_deps_test.go`の…ForTestに
  置き換えた(`SetHostBlockerForTest`・`SetSoftwareBlockerForTest`・`SetSilencedHostCheckerForTest`・
  `SetRolePolicyProviderForTest`・`SetSuspensionOriginRepoForTest`・`SetBackgroundForTest`)。
  禁止語とセンシティブワードのテストは、偽のhostBlocker(`stubHostBlocker`)に語を持たせる形に
  直し、`hostBlocker`から読むことは`NewResolver`へ渡す形で見る。テストの偽のhostBlocker
  (`stubHostBlocker`・`stubRemoteUserHostBlocker`)には、語を返す(既定はnil)メソッドを足した。
  `HasSensitiveWordsSource`だけを見ていたテストと、`wiring_predicate_test.go`(resolverの`Has…`の
  テスト)は消した。パッケージ外の`NewResolver`の呼び出し(`internal/api/inbox`の3か所と
  `internal/queue/processors`の2か所)はnilを渡す。構築子が引数を捨てないことは
  `constructor_test.go`が見る
- **`routeParts`に`federationResolver`を足した。** `requiredDepChecks`に置き換えた6つの欄を足した。
  テストの組み立てでnilだったのは`federationResolver.keyFetchFailures`の1つで、許可リストに足した。
  依存ではなく、公開鍵の取り直しに失敗した時刻の状態で、最初の失敗を記録するとき
  (`markKeyFetchFailed`)に作る
- **受信の拒否の判定は、wiring-checkで照合する。** `softwareBlocker`には受信の拒否の判定
  (`inboxSoftwareChecker`、`NewInboxSuspendedChecker`)を渡す。配送停止の判定(`NewSuspendedChecker`)と
  同じ型なので、取り違えは型でもnilの検査でも捕まらない。段(5)の
  `TestQueueProcessorSoftwareCheckersAreWired`に、`NewResolver`の呼び出しの照合を足した

| | #3833の前(`ea8d65fae2`) | 1段目の後 | 2段目の後 | 3段目の後 | 4段目の後 | 5段目の後 | 6段目の後 |
|---|---|---|---|---|---|---|---|
| `criticalWiringCount` | 28 | 28 | 27 | 24 | 21 | 18 | 10 |
| `setupRoutes()`の本文で、行頭が`<変数>.Set`の行 | 605 | 605 | 605 | 602 | 599 | 596 | 590 |
| その受け手 | 107 | 107 | 107 | 106 | 106 | 106 | 106 |

数え方: 段(1)・段(2)の表と同じ。2段目は表の行を外しただけで、setterは元から無い。3段目で
減った受け手は、置き換えたsetterしか呼んでいなかった`noteDeleteHook`。6段目で減った6行は
resolverの`SetBackground`・`SetHostBlockChecker`・`SetSoftwareSuspensionChecker`・
`SetSilencedHostChecker`・`SetRolePolicyProvider`・`SetSuspensionOriginRepo`(禁止語と
センシティブワードは元からsetterを呼んでいなかった)。

残った10行は段(7)の対象ではない。内訳は次のとおり(`recordCriticalWiring`の表の名前)。

- setterのまま残す5行: 循環の4行(`userPacker.detailExtras`・`following.meUpdatedPublisher`・
  `userpack.detailExtrasMany`・`following.blockingChecker`)と`signup.formTokens`
- 既に型で必須の4行: `webPushNote.entityPacker`・`sharedUserPacker.entityPacker`・
  `webPushUser.entityPacker`・`streamChannels.chatRoom.service`。部品そのものは構築子の引数で受け、
  受け取ったPackerがメディアのプロキシを持つか(chatRoomはサービスがnilでないか)を見ている
- 段(8)で扱う1行: `entityPacker.mediaURL`

##### 段(8): `entity.NewPacker`のmediaURL(#3834)

**entityのPackerのメディアのプロキシも、構築子の必須の引数にした。** #3812の段(8)で、最後の段。

- **`entity.NewPacker(mediaURL *MediaURLContext, deps PackerDeps)`にし、`PackerDeps`から`MediaURL`を
  外した。** 本番の呼び出しは`setupRoutes()`の1か所(`entity.NewPacker(mediaURLCtx, entityPackerDeps)`)。
  `mediaURLCtx`は`entityPackerDeps`と同じく`NewPacker`の前で作ってあり、依存を入れる時点は
  変わらない。無いとリモートのメディアのURLを生のまま出し、閲覧者のIPがリモートへ漏れる。この
  説明は`Packer`の`mediaURL`のフィールドのコメントへ移した
- **テストの呼び出しは機械的に置き換えた。** `_test.go`に
  `git grep -E 'entity\.NewPacker\(|[^.]NewPacker\(PackerDeps'`を当てた73か所(58ファイル)。
  `PackerDeps{}`を渡していた43か所と、`MediaURL`以外の欄だけを渡していた2か所は第1引数にnilを
  渡す。`MediaURL:`を渡していた28か所は、その値を第1引数へ移した(`MediaURL:`だけの27か所は
  機械的に、ほかの欄と一緒に渡していた`internal/entity/packer_test.go`の1か所は手で)。置き換えの
  前後で件数を突き合わせた。`meself.NewPacker`は別の構築子なので対象外
- **`HasMediaURL`は残した。** Packerを構築子で受ける部品(`webPushNote`・`webPushUser`と
  `sharedUserPacker`)の`HasEntityPacker`がこれを使い、表の3行が見る
- **`entityPacker.mediaURL`の行を表から外した**(`criticalWiringCount`は10から9)。組み立てた
  Packerに`mediaURL`が入っていることは、`TestBuiltHandlersHaveNoNilDependencies`の
  `nestedDepChecks`に`notes.entityPacker`を足して見る。どのhandlerも`setupRoutes()`の同じ
  `entityPacker`を受け取るので、代表にnotesのhandlerのPackerを辿る。本番は`PackerDeps`の7つの欄も
  すべて埋めるので、あわせて8つのフィールドを見る。テストの組み立てでnilのものは無かった。
  `NewPacker`の第1引数にnilを渡すと、表に残る`…entityPacker`の3行が起動時に落とす。notesの
  handlerにだけmediaURLの無いPackerを渡すと、`nestedDepChecks`が落とす。`Packer`に依存の欄を足した
  ときに一覧が追いつかないまま緑にならないよう、`Packer`のnilになりうる型(ポインタ・interface・
  func・map・スライス・chan)の欄の数と一覧の数(8)も突き合わせる。mutexのような依存でない欄は数えない
- **代表にnotesのPackerを見る前提も縛る。** 同じテストの`assertOnePacker`が、`apiHandlers`・
  `routeParts`・`coreParts`の各部品の直下にある`*entity.Packer`の欄が、どれもnotesのhandlerと同じ
  Packerを指すことを見る。handlerの中に入っていて直下にPackerを持たない部品のうち、Packerを包む
  共有の`userpack.Packer`(`sharedUserPacker`)・Web Pushの`NoteRepoPacker`と`UserRepoPacker`・
  `entity.NoteFieldResolver`は、`routeParts`に持たせて直下で見る。部品の中の部品までreflectで
  辿る形も試したが、組み立ての後に動き出すgoroutineが書き換える状態まで読み、`-race`が競合として
  落とした。抽出が空振りしても緑にならないよう、件数ではなく、Packerを欄に持つべき型を6つ名指しで
  要求する(`requiredPackerHolders`。`<型>.<欄>`の形で、notes・blockingのhandlerの2つと上の4部品の
  6つ)。別に作ったPackerを1つの部品にだけ渡すと、その経路だけ閲覧者のIPがリモートへ漏れるか、
  roles・canChat・isSilencedが黙って既定値になる。以前の表の行も変数`entityPacker`しか見ていな
  かったので、これは捕まらなかった。照合するのは`*entity.Packer`型の欄だけで、Packerを包む部品
  (`userpack.Packer`・`NoteFieldResolver`など)を作り直して1つのhandlerにだけ渡す誤りは、包む部品の
  型の欄やinterfaceの欄に現れるので捕まらない(既知の限界。#3834の前から捕まっていなかった)
- **`entitycompat`の`TestEntityPackerDepsAreWired`から`MediaURL: mediaURLCtx`の行を外し、構築子の
  呼び出しの照合に置き換えた。** `PackerDeps`の欄の数と表の行の数(7)を突き合わせる。nilの検査では、
  作り直した`*MediaURLContext`を渡す誤りが捕まらない。`mediaURLCtx`には後から、管理画面の
  `proxyRemoteFiles`を毎回読むlookupと自前のメディアの判定(#2315)を付けているので、作り直すと
  その2つが黙って外れる。`entityPacker := entity.NewPacker(mediaURLCtx, entityPackerDeps)`を
  照合する(`entityPackerDeps`を実際に渡していることもここで縛る)。構築子が`mediaURL`を捨てない
  ことは`internal/entity`の`TestNewPacker_KeepsMediaURL`が見る

| | #3834の前(`d646f6efee`) | 段(8)の後 |
|---|---|---|
| `criticalWiringCount` | 10 | 9 |
| `setupRoutes()`の本文で、行頭が`<変数>.Set`の行 | 590 | 590 |
| その受け手 | 106 | 106 |

数え方: 段(1)・段(2)の表と同じ。段(8)はsetterを置き換えていない(`MediaURL`は`PackerDeps`の欄で
渡していた)ので、setterの行は変わらない。

**`criticalWiringCount`は、#3812の見込みの9に達した。** 残る9行は、#3812で置き換えないと決めた
行と、既に型で必須の行だけ(`recordCriticalWiring`の表の名前。表の行数は
`TestCriticalWiringCountMatchesTable`が定数と突き合わせる)。

- setterのまま残す5行: 循環の4行(`userPacker.detailExtras`・`following.meUpdatedPublisher`・
  `userpack.detailExtrasMany`・`following.blockingChecker`)と`signup.formTokens`
- 既に型で必須の4行: `webPushNote.entityPacker`・`sharedUserPacker.entityPacker`・
  `webPushUser.entityPacker`・`streamChannels.chatRoom.service`。部品そのものは構築子の引数で受け、
  受け取ったPackerがメディアのプロキシを持つか(chatRoomはサービスがnilでないか)を見ている

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
| `routes_admin.go` | `registerAdminRoutes` | `admin` 144・`v2` 1 | 145 | `adminHandler`、`announcementHandler`、`backupHandler`、`roleService` |
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
  持たない (本体を読んで確かめた)。`roleService` も `backupGuard` も書き換えていない。
  (後に #3878 で、再認証を引数の検証より後に走らせるため、`backupGuard` を route から外し、
  再認証は `adminbackup` の handler の中で行うようになった。`registerAdminRoutes` も
  `backupGuard` を受け取らない)
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
| /api の外の `s.echo` の 64 文 (ActivityPub・SSR・well-known・oauth・nodeinfo・inbox・streaming・`/files`・`/proxy`・`/url`・`/backup-download`・静的配信・SPA) | `apiRoutes` を通らない別の層で、この段の関数の形 (`api apiRoutes` を受ける) に当てはまらない。`/notes/:id` や `/@:acct` のように `s` と `ssrMeta` などを捕まえる closure、構築の呼び出しを行の中に持つもの (`filesHandler(…)`、`newTwemojiBadgeHandler(…)`。後者は #3776 で変数に取り出した) があり、評価の時刻の確認がこの段より重い。静的配信と SPA の catchall はプラグインの準備の `return` より後にある |
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
    `setupRoutes()` の本文の直下で `:=` か値付きの `var` で 1 回だけ定義し、関数リテラルの中を含めて書き換えず、
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

#### setupRoutes の節 (#3901)

**`setupRoutes()` の中の連続した範囲を、名前が `wire…` の `*Server` のメソッド (節) へそのまま
移す。** 呼ぶ位置は元の範囲の位置で、中の文とその順は変えない。段階 (2) で作るだけの部品は
`build…` へ移り、`setupRoutes()` に残ったのは主に setter の呼び出しと、副作用があって動かせない
文だった。setter を受け手の領域ごとに寄せ直すと、4.1 の「setter は元の位置で呼ぶ」を覆すことに
なるので、範囲を切り出すだけにした。

```
setupRoutes()
  …
  s.wireCacheInvalidation(cacheInvalidationDeps{   // wiring_cache_invalidation.go
      idGen:          idGen,
      internalPubSub: internalPubSub,
      …                                            // 範囲の中で使う、範囲より前の名前すべて
  })
  …

func (s *Server) wireCacheInvalidation(d cacheInvalidationDeps) {
  idGen := d.idGen                                 // setupRoutes と同じ名前で受け直す
  …
  …                                                // 元の範囲の文 (そのまま)
}
```

- **範囲より前の名前は、`…Deps` の構造体で受けて、本文の先頭で同じ名前に受け直す。** 移した文は
  1 文字も変わらないので、`assertWired` などの照合はそのまま効く。引数で受けないのは、範囲が
  使う名前が多いため (#3901 で測った。キャッシュの無効化の範囲で 31 個、ほかの候補の範囲で 19〜70 個)
- **節は範囲ごとに 1 つで、型は `wireX` に対して `xDeps`。** 範囲で作って範囲より後で使う名前は、
  最後の文の `return a, b` で返し、`setupRoutes()` が `a, b := s.wireX(…)` で同じ名前に受ける
- **渡す側の形は、`internal/server` の `TestSetupRoutesSectionsReceiveSameNamedVariables` が見る。**
  フィールドをすべて `f: f` の形で 1 回ずつ書く (書き忘れたフィールドはゼロ値が黙って渡り、別の
  名前の変数を渡すと照合する文の名前と指す値がずれる)。渡す名前は `setupRoutes()` の引数か、本文の
  直下で `:=` か値付きの `var` で 1 回だけ定義した名前で、呼び出しより後と関数リテラルの中で書き換えず、アドレスも
  取らない (節は呼び出しの時点の値を受け取るため)。節を呼ぶのは `setupRoutes()` の本文の直下の
  1 か所だけ
- **`setupRoutes()` の本文の直下の文を見るテストは、節を展開した本文を読む。** `internal/server` の
  `assembledSetupRoutes` は、節の呼び出しを節の本文 (受け直しと最後の `return` を除く) に置き換えた
  `setupRoutes()` を返す。受け渡しが同じ名前なので、移す前の 1 つの関数と同じ文を同じ順に持つ。
  文がファイルをまたぐので、順序は行ではなく本文の添字で比べる (今使っているのは stream の
  チャンネルの登録を見る 3 つ)
- **`:=` の右辺で作り方を見るゲートは、受け直し `x := d.x` を数えない。** `internal/entitycompat` の
  `sectionDepsAliases` が受け直しの文を返す (今使っているのは `TestCaptchaReloadIsWired`)
- **範囲の中で `:=` で受け直すだけの `err` は渡さない。** `wireAdmin` の範囲は `setupRoutes()` の
  `err` を `backupVerifier, err := …` で書き換えるが、読む前に必ず代入し、範囲より後も `:=` で
  代入してから読む。渡すと「呼び出しより後で書き換える」に当たるので、節の中で新しく宣言させた
- **範囲に `setupRoutes()` の `return`・`defer`・ラベルを含めない。** 節へ移すと、返る先や走る時刻が
  変わる。これはゲートでは見ておらず、切り出すときに確かめる

| 節 | ファイル | 移した範囲 | 受け取る名前 | 返す名前 |
|---|---|---|---|---|
| `wireCacheInvalidation` | `wiring_cache_invalidation.go` | cross-worker のキャッシュの無効化と、キャッシュの一覧への登録 (`urlPreview` より前の分) | 31 | 0 |
| `wireFrontendRoutes` | `wiring_frontend_routes.go` | /api の外のフロントエンドの配信 (アセット・twemoji・identicon・manifest・Service Worker・埋め込みページ) と SPA の catchall。プラグインの準備の `return` より後 | 13 | 0 |
| `wireAdmin` | `wiring_admin.go` | 管理画面の API の handler と配線 (通報の webhook・メール、ロール、絵文字の申請、連合など) と、DB のバックアップの管理画面の handler・再認証・`/backup-download` | 69 | 4 (`adminHandler`・`emojiApplicationService`・`emojiApplicationHandler`・`backupHandler`) |
| `wireStreaming` | `wiring_streaming.go` | ストリーミングの接続の管理 (lookup と、server role で始める購読)、server と queue の統計の publisher、既存の service への publisher の注入、reversi・バブルゲームの対戦・chat の service とチャンネルの登録、`/streaming` の登録 | 64 | 7 (`abuseCreatedNotifier`・`serverStatsPub`・`reversiService`・`bubbleVersusPublisher`・`bubbleVersusService`・`chatService`・`chatRoomFactory`) |

数え方: 受け取る名前は `…Deps` のフィールドの数、返す名前は `return` の値の数。

**同じ動きであることの確かめ方。** 節を足すたびに、`develop` とその段の binary で、`elythia dump-routes`
の出力、その起動の間の Redis の命令の列 (`MONITOR`) と起動ログが一致することを確かめた。どれも
使い捨ての PostgreSQL と Redis に向けて流した。

### 4.3 部品の起動と停止の一覧 (#3699、#3701、#3703)

**停止が要る部品は、名前を付けて 1 つの一覧 `lifecycle` (`internal/server/lifecycle_parts.go`)
に登録する。** #3666 の段階 (3)。前は `registerShutdownHook(func(context.Context))`
の無名の closure の並びで、どの部品の停止かがテストからも実行時からも見えなかった。#3699 で
すべての呼び出しを名前付きの登録に置き換え、`registerShutdownHook` と `shutdownHooks` を消した。
無名で登録する口は残していない。#3701 で、起動と停止が部品自身のメソッドであるものは、値を 1 つ
渡すだけで起動と停止の登録を済ませる形にした。#3703で、一覧の項目に起動を持たせ、構築では登録だけを
行い、起動は`StartBackground`が一覧の順にまとめて行う形にした(下の「起動」)。#3705で、構築の途中で
`go`文で回していた最後の部品`emojiInvalidationQueue`に`Start`と`Stop`を持たせ、値から作る登録
(`startPartWithStop`。今の`addStartStopper`)で一覧に入れた。#3707で、部品の停止のpanicを部品ごとに
回収し、残りの停止を続ける形にした(下の「停止のpanic」)。#3709で、`StartBackground`と`StartBackgroundForTest`が同じ
1度きりの起動を通り、停止を始めた後は何も起動しない形にした(下の「2回呼んだとき」と「停止の後の起動」)。
#3711で、世代の停止(`shutdown`)と構築の後始末(`cleanupConstruction`)で直書きしていた手順を、名前付きの
段の表(`shutdownSteps`・`cleanupSteps`)にし、段ごとにpanicを回収して残りの段を続ける形にした(下の
「世代の停止」と「停止のpanic」)。#3713で、起動がqueueのworkerを起動したことを世代に記録し(`queueStarted`)、
停止がroleにかかわらずそのworkerとschedulerを止める形にした(下の`StartBackgroundForTest`の項と、
「世代の停止」の手順2)。#3715で、登録の関数の名前を`add…`に揃えた(下の「登録の形」)。#3717で、値を持つ
残りの3つの部品(`fedRuleHits`・`hashtagService`・`paramShadowRecorder`)に停止の口を足して値から作る登録へ
移し、closureで停止を渡す登録(`addPart`・`addPartWithStart`)を本番のコードから消した(下の「登録の形」と
「closureのまま残した4つ」)。

- 項目は名前・止める値 (`part`)・起動 (`func()`。起動するものが無い部品はnil)・停止
  (`func(context.Context)`) を持つ。`part` は停止が働きかける値 (バッファ・publisher・service・
  プラグインの store など) で、本番のコードは読まない。名前どおりの
  値を止めたかをテストが確かめるために持つ
- 一覧は `Server.lifecycle` が値で持つ (テストが組む `Server` のリテラルでも、ゼロ値が空の一覧として
  使える)。登録はどれも `Server` のメソッド (下の表の6つ) を通し、起動と停止も `s.lifecycle` から回す
  (`paramShadowMiddleware` と `dbBackedStorage` は `w` を受け取らない)。コンテナにも同じ一覧を指す
  `wiring.lifecycle` があったが、誰も読まないまま残っていたので #3808 で消した (4.2)
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
| `s.addStartCloser(ctx, name, part)` | `Start(ctx)` + `Close()` | `StartBackground`で`Start(ctx)`(ctxは登録で渡したもの) | `Close()`。停止の ctx は捨てる | `instanceTouchBuffer`・`sigCapBuffer`・`instanceCounterBuffer` |
| `s.addStartStopper(ctx, name, part)` | `Start(ctx)` + `Stop(ctx)` | `StartBackground`で`Start(ctx)`(ctxは登録で渡したもの) | 停止の ctx を渡して `Stop(ctx)` | `fedRuleHits`・`deliveryHealth`・`inboxHealth`・`emojiInvalidationQueue` |
| `s.addPlainStartStopper(name, part)` | `Start()` + `Stop()` | `StartBackground`で`Start()` | `Stop()`。停止の ctx は捨てる | `serverStatsPub`・`queueStatsPub` |
| `s.addCloser(name, part)` | `Close(ctx)` | しない | 停止の ctx を渡して `Close(ctx)` | `metadataRefresher`・`hashtagService`・`paramShadowRecorder` |
| `s.addErrCloser(name, part)` | `Close() error` | しない | `Close()`。エラーと停止の ctx は捨てる (エラーを捨てるのは前の hook と同じ) | `pluginStorage:<名前>` (`setupRoutes()` の外の `dbBackedStorage` で登録する) |
| `s.addWorker(name, run)` | 値を持たない (関数を渡す) | `StartBackground`で`run`をgoroutineで回す | ctx を切り、`run` が返るのを待つ。起動していなければ待たずに返す | 下の worker の 4 つ |

起動と停止をclosureで渡す登録(`s.addPart(name, part, stop)`・`s.addPartWithStart(name, part, start, stop)`)は、
#3717で本番のコードから消した。テストが偽の部品を足す口として、同じ形のまま`lifecycle_parts_test.go`へ
移した。本番でclosureの停止を渡せるのは、値を持たない`addWorker`だけになった。

#3715で、登録の関数の名前を、部品の値が満たすinterfaceの名前に揃えて`add…`に改めた
(`startPartWithClose`→`addStartCloser`、`startPartWithStop`→`addStartStopper`、
`startPartWithoutCtx`→`addPlainStartStopper`、`startConstructionWorker`→`addWorker`)。前の名前は
#3703より前の「登録したその場で起動する」の名残で、構築の途中では起動しない今の動きと合わなかった。
部品の名前、登録の順、起動と停止の順は変えていない。各段の記録(この節と4.1・4.2)は、当時の名前のまま
残している。

**closureのまま残した4つ**は、値を持たないworker(#3705で`emojiInvalidationQueue`が値から作る登録へ
移り8つから7つに、#3717で値を持つ3つが移り4つになった)。

| 名前 | closure が要る理由 | `part` |
|---|---|---|
| `pollExpiryWorker`・`reactionFlushWorker`・`retentionAggregateAtStartup`・`userCacheLink` | `addWorker(name, run)` が関数を goroutine で回し、ctx を切って返るのを待つ。止める値が無い | nil |

#3717で移した3つは、前のclosureの中身を部品のメソッドへ移した。止め方は変えていない。

| 名前 | 足した口 | 中身(前のclosureと同じ) |
|---|---|---|
| `fedRuleHits` | `HitStore.Stop(ctx)` | `Start`の中で作ったctxを切る。goroutineは待たない。前は組み立てが自分でcancelするctxを`Start`に渡し、それを切っていた |
| `hashtagService` | `Service.Close(ctx)` | `Shutdown(ctx)`を呼び、エラーなら前と同じ文面(`hashtag service shutdown timed out`)と属性(`err`)で警告を出す |
| `paramShadowRecorder` | `Recorder.Close(ctx)` | `Flush()`を呼ぶ。ctxは使わない |

- `HitStore`は、`Start`の中で作ったctxのcancelをmutexの下で持つ。`Stop`はmutexの下で停止の印を立てて
  cancelを取り出し、mutexの外で呼ぶ。`Start`より先に呼んでも、2回呼んでも、nilの`HitStore`でも安全で、
  `Start`と並行に呼んでも、どちらが先でもloopは残らない。`Start`を2回呼んだときは、`Stop`が両方のloopを止める
- `Stop`の後の`Start`はloopを立てない。前は切れたctxでloopを立て、すぐに抜けていた(溜まった記録があると、
  切れたctxで1回書こうとして失敗することはありえた)。一覧の`startAll`は停止を始めた後には起動しないので、
  本番の経路ではどちらも起きない

**値を持つ部品が、値から作る登録を経ていることは、テストで検査する。** closureで書くと、`part`と
停止が別の値を指しうる形に戻るため。#3717で本番からclosureの登録を消したが、一覧へ直接足す
`lifecycle.add`・`addWithStart`は値と停止を別々に受けるので、型だけでは防げない。一覧の項目は、
値から作る登録を経たかの印(`fromValue`)を持つ。`TestNewServer_ShutdownStopsThePartsInTheirOrder`が
7つの組で、`part`がnilでない部品のうち印の無いものを落とす(`partsNotFromValue`)。違反の文には、
形の判定(テストの`valueShape`)で、その値を受ける登録の名前を添える。形を満たさない値も落とす
(停止の口を足して、値から作る登録へ移す)。
違反が0件なのが正常な検査なので、下限を置く。検査が値を持つ部品を見たことを、名前の一覧
(`valuePartsInspected`。両方のroleの組の12個と、プラグインの組の2つのstoreで14個)で、どれかの組で
見たことを確かめる。一覧に無い名前で値を持つ部品が出ても落とす(下限が古くならないように)。
組をまたぐこの検査は、7つの組が全部走ったときだけ行う
(`-run`で組を絞ったときは、関係の無い失敗を出さないように省く)。
#3717の前は、`addPart`で登録した部品のうち形を満たすものだけを落とす検査で、理由付きの許可リスト
(`addPartShapeAllowlist`。#3717の時点で空)を持っていた。形を満たさない値をclosureで登録しても落ちなかった。
形の判定は、`TestValueShape_ClassifiesThePartTypes`が型の名前を並べて固定する。
値から作る13個の部品の型は12(`deliveryHealth`と`inboxHealth`は同じ型)で、どれも形を満たす。
値の無いworkerは満たさない。部品の型にメソッドを足して形が変わると、ここが落ちる。
判定が知る形の一覧 (`valueShapes`) は、`TestValueShapes_CoverEveryRegistrationFromValue` が
`lifecycle_parts.go` を構文木で読み、`addFromValue` を呼ぶ登録の関数と、その値の引数の interface の型を
列挙して突き合わせる (1 つも拾えなければ落とす)。登録の形を足して判定に足し忘れると、ここが落ちる。
ただし読むのは `lifecycle_parts.go` の `Server` のメソッドだけで、別のファイルに置いた登録と、
型パラメータを持つ自由関数 (メソッドは型パラメータを持てない) で書いた登録は拾えない。

**実行時には拒まない**(`lifecycle.add`は印を見ない)。本番で`lifecycle.add`・`addWithStart`を呼ぶのは、
値から作る登録(`addFromValue`)と`addWorker`だけで、値を持つ部品が直接通っていないことは上のテストが見る。
#3701では、`addPart`が形を見て拒むと、部品の型にメソッドを足しただけで本番の構築が失敗し、案内どおりに
値から作る登録へ直すと、closureの停止(`hashtagService`の時間切れの警告など)が失われるため、拒まなかった
(#3701のレビュー)。#3717でその停止を部品のメソッドへ移したので、この懸念は無くなった。

**同じ型どうしの取り違えを書く余地が無くなった範囲。** #3699 では値と停止を別々に渡すので、
`s.addPart("deliveryHealth", deliveryHealth, func(ctx context.Context) { inboxHealth.Stop(ctx) })` の
ような、値と停止で別の部品を指す誤りを書けた。同じ型どうし (`deliveryHealth` と `inboxHealth`) では、
型の表のテストでも落とせなかった。#3701 の後は次のとおり。

- **値から作る13個(#3717の後。#3705の後は10個)は、値と停止が別の部品を指す誤りを書けない。** 停止は渡した値そのものから作る。
  起動も同じ値に対して呼ぶので、値と起動が別の部品を指す誤りも書けない
- **同じ値を 2 回渡す誤りは、テストが落とす。** 例えば `s.addStartStopper(ctx, "inboxHealth", deliveryHealth)`
  と書くと、値が起動と停止を決めるので、`inboxHealth` は起動も停止もされない。同じ型なので型の表では
  落ちないが、`TestNewServer_ShutdownStopsThePartsInTheirOrder` が 7 つの組で、nil でない `part` どうしが
  互いに異なる値であること (同一性) を見て落とす
- **名前と値の入れ替えは落とせない。** `deliveryHealth` の名前に `inboxHealth` を、`inboxHealth` の名前に
  `deliveryHealth` を渡す形は、同じ型で値も 2 つとも異なるので、どのテストも落ちない (使い捨ての変異で、
  `internal/server` のテストがどれも落ちないことを確かめた)。名前は呼び出し元が渡す文字列で、値からは
  作れないため。ただし両方とも起動され、両方とも止まるので、起動されない・止まらない部品は出ない
  (変わるのは、一覧の名前と値の対応と、その 2 つの間の起動と停止の順だけ)。別の型の値を渡す誤り
  (`sigCapBuffer` の名前に `instanceTouchBuffer` を渡すなど) は、型の表のテストが落とす
- **closureで停止を渡すのは、値を持たないworkerの4つだけになった(#3717)。** 値が無いので、値と停止の
  取り違えは起きない。名前と`run`の対応は、#3699と同じく型でも値でも保証しない。#3717の前は、値を持つ
  `fedRuleHits`・`hashtagService`・`paramShadowRecorder`もclosureで、停止のclosureが別の部品を止める誤りは、
  観測している`hashtagService`のほかは落とせなかった

起動と停止の順と、登録の条件は次のとおり(上から順に起動し、上から順に止める。どちらも登録した順と同じ)。
「起動」が○の13個は`StartBackground`が起動し、構築だけでは動かない(#3703。`emojiInvalidationQueue`は#3705)。

| 順 | 名前 | 登録 | 起動 | 止め方 | 登録の条件 |
|---|---|---|---|---|---|
| 1 | `pollExpiryWorker` | `addWorker` | ○ | ctx を切り、`Run` が返るのを待つ | queue role |
| 2 | `fedRuleHits` | `addStartStopper` | ○ | `Stop(ctx)`。`Start`の中で作ったctxを切る(待たない) | 常に |
| 3 | `metadataRefresher` | `addCloser` | | `Close(ctx)`。新しい取り直しを止め、走っているものを短く待つ | 常に |
| 4 | `instanceTouchBuffer` | `addStartCloser` | ○ | `Close()`。残りを flush する | 常に |
| 5 | `sigCapBuffer` | `addStartCloser` | ○ | `Close()`。残りを flush する | 常に |
| 6 | `reactionFlushWorker` | `addWorker` | ○ | 1 と同じ | 常に |
| 7 | `instanceCounterBuffer` | `addStartCloser` | ○ | `Close()`。残りを flush する | 常に |
| 8 | `deliveryHealth` | `addStartStopper` | ○ | `Stop(ctx)` | queue role |
| 9 | `inboxHealth` | `addStartStopper` | ○ | `Stop(ctx)` | queue role |
| 10 | `hashtagService` | `addCloser` | | `Close(ctx)`。`Shutdown(ctx)`で in-flight の集計を待ち (#727)、時間切れなら警告を出す | 常に |
| 11 | `retentionAggregateAtStartup` | `addWorker` | ○ | 1 と同じ | queue role |
| 12 | `paramShadowRecorder` | `addCloser` | | `Close(ctx)`。`Flush()`で最後の要約を書く | 本家の paramDef を読めたとき (#3742 から、`disableParamShadow` でも登録する) |
| 13 | `emojiInvalidationQueue` | `addStartStopper` | ○ | `Stop(ctx)`。ctxを切り、`Run`が溜まった分を送って返るのを待つ | 常に |
| 14 | `userCacheLink` | `addWorker` | ○ | 1 と同じ | 常に |
| 15 | `serverStatsPub` | `addPlainStartStopper` | ○ | `Stop()` | server role |
| 16 | `queueStatsPub` | `addPlainStartStopper` | ○ | `Stop()` | server role で、queueInspector があるとき |
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
- **起動のctx。** 前と同じく、登録で渡した`context.Background()`を使う(`fedRuleHits`は#3717の前は
  組み立てが自分でcancelするctxを渡していた。今は`context.Background()`を渡し、切れるctxは`Start`の中で
  作る)。`StartBackground`はctxを受け取らず、部品は停止のメソッド(`Close`・`Stop`・ctxのcancel)で
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
  先に`StartBackgroundForTest`を呼んだ世代では、後から`StartBackground`を呼んでもchartは起動しない。
  起動したworker・autoscale・schedulerは、roleにかかわらず`StopBackground`が止める(#3713。下の
  「世代の停止」の手順2)。#3713の前は、server role(`MK_ONLY_SERVER`)の世代で起動したworkerと
  schedulerを止めなかった
- **停止の後の起動(#3709)。** `Server`は起動と停止の順を守るmutex(`runMu`)と、停止を始めた印
  (`stopped`)を持つ。停止(`shutdown`)は何かを止めるより先に、mutexを取って印を立てる。構築の後始末
  (`cleanupConstruction`)も同じ印を立てる。起動はmutexを握ったまま走り、印が立っていれば何も
  起動せずに`errGenerationStopped`を返す(1度きりの起動の結果なので、後の呼び出しも同じエラーを返す)。
  起動の途中で停止が始まったときは、停止は起動が済むのを待ってから印を立て、その起動が起動したもの
  をすべて止める。起動の間はmutexを握ったままにした。起動は部品のgoroutineを立てるほか、cronの登録
  (`registerSchedulerJobs`の12個の登録の関数)と撤去(`PruneUnregistered`)でRedisを
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
| `fedRuleHits` | `Stop(ctx)`は`Start`の中で作ったctxをcancelするだけ。起動していなければcancelするものも待つgoroutineも無く、停止の印を立てるだけ(#3717。後の`Start`はloopを立てない) |
| worker の 4 つ(`addWorker`) | 停止はctxをcancelし、起動していなければ`done`を待たずに返る。`done`は起動しないと閉じないので、待つと停止のctxの期限まで固まる(後始末は`context.Background()`で止めるので、永久に返らない) |
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
  通ることは無くなった(`wiring_streaming.go`の`notificationService.SetMainStreamPublisher`の行のコメント。#3901より前は`router.go`)
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

**意図して変えた動き(#3713)。** 本番の経路では起きない(本番は`StartBackgroundForTest`を呼ばず、
`StartBackground`はroleがqueueを回すときだけworkerを起動するので、`queueStarted`が立つ世代では、
roleの判定もすでに真になっている)。

- server role(`MK_ONLY_SERVER`)の世代で`StartBackgroundForTest`を呼ぶと、前は起動したworkerと
  schedulerを`StopBackground`が止めず、autoscaleが無ければ`stopQueue`は待たずに返った。今は
  workerとschedulerの`Shutdown`を1回ずつ呼び、止まるのを待つ。e2e-federationは既定のrole(両方)で
  呼ぶので、変わらない
- 起動していない世代と、roleがqueueを回す世代の停止は、前と同じ

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

一覧には乗せない。段階(6)でメンテナンスの作り直しの範囲を決めるときに検討する予定だったが、#3802で範囲を狭めないと
決めた(世代ごとに全部を作り直すので、一覧に乗せる理由が無い)。

**世代の停止 (`shutdown`) は、一覧を次の位置で回す。** 順を決めている理由は #3662 と #3661。
停止は名前付きの段の表(`shutdownSteps`。#3711)を上から順に回す。段の名前は次の各行の先頭に書く。
表の前に、起動しない印(`stopped`)を立てる(#3709。上の「停止の後の起動」)。印は段の表の外に置くので、
段のpanicで飛ぶことは無い。

1. `streaming`: streamingの接続を1012で閉じる(別の上限付き。`streamCloseContext`)
2. `queue`: queueのworkerとschedulerを止める(`stopQueue`)。**一覧より先に止める**(#3662)。workerの
   ジョブはバッファとchartへ書くので、先にバッファを閉じると、止まるまでに終わったジョブの書き込みが
   flushされずに消える。autoscaleは起動していれば止める。workerとschedulerの`Shutdown`を呼ぶのは、
   roleがqueueを回すとき(起動していない世代でも呼ぶ。#3713の前と同じ)と、起動がworkerを起動したと
   記録したとき(`queueStarted`。#3713)。`queueStarted`はworkerの`Start`が成功した直後に立てるので、
   続くautoscaleの起動が失敗した世代でもworkerを止める。どちらでもなく、autoscaleも無ければ、
   待たずに止まったものとして返る(期限の切れたctxで、止めるものの無いqueueを「止まらなかった」と
   取り違えないため)
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
同じ順に止める)、`background`(箱を1秒まで待つ)、`releaseParts`(一覧と、4.4のキャッシュの一覧を空にする。キャッシュの一覧は#3721から)、`queueDriver`(driverを
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
  `TestLifecycle_StartAllStartsNothingAfterStopAll`・`TestServer_WorkerRunsOnlyOnceStarted`が、
  panicの回収、止め始めた後の起動、workerの起動と停止を一覧の単位で見る
- **queueとの前後(#3703)。** `TestServer_StartBackgroundStartsThePartsBeforeTheQueue`が、
  `StartBackground`と`StartBackgroundForTest`のどちらも、一覧の部品を一覧の順に、queueのworkerの
  `Start`より先に起動することを見る(queueの起動を記録する差し替えのdriverを使う)。
  `TestServer_FailedPartStartDoesNotStartTheQueue`が、部品の起動が失敗したら後の部品もqueueも
  起動しないことを見る
- **値の型。** 同じテストが、各部品の `part` の動的な型を表 (`lifecyclePartTypes`) と照合する。
  名前が別の種類の値を持つ取り違え (`serverStatsPub` に `hashtagService` を渡すなど) は落ちるが、
  **同じ型の部品どうしの名前の取り違えは落とせない**。`deliveryHealth` と `inboxHealth` はどちらも
  `*deliveryhealth.Service` で、`addWorker` の 4 つはどれも nil。値と停止の取り違えは、
  値から作る13個では書けない(上の「同じ型どうしの取り違えを書く余地が無くなった範囲」)
- **値の同一性と、登録の経路。** 同じテストが、nil でない `part` どうしが互いに異なる値であることと、
  nilでない`part`を持つ部品がすべて値から作る登録を経ていること(上の`partsNotFromValue`の検査。#3717)を
  見る。値は同一性で比べるのでポインタに限り、**ポインタでない値を持つ部品は、それだけで落ちる** (値の型の部品を足すときは、
  比べ方から見直す)。2 つの検査は違反の一覧を返す関数 (`sharedPartValues`・`partsNotFromValue`) で、
  `TestPartChecks_ReportViolations` が手で組んだ一覧で違反を報告すること (正の対照) と、守った一覧では
  報告しないことを固定する
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
  `internal/repository`の`TestEmojiInvalidationQueue_*`が見る(#3705)。#3717で足した停止の口そのものの
  動きは、各パッケージの単体テストが見る。`internal/core/fedrule`の`TestHitStore_Stop*`・
  `TestHitStore_StartAfterStopStartsNothing`・`TestHitStore_ConcurrentStartAndStop`が、`Stop`がloopを
  終わらせ、その後の記録を書かないこと、`Start`を2回呼んだときも両方を止めること、`Stop`の後の`Start`が
  loopを立てないこと、`Start`と並行に呼んでもloopが残らないこと(`-race`)、nilとRedisの無い`HitStore`で
  何もしないことを見る。`internal/core/hashtag`の`TestService_Close_*`が、書き込み中のworkerがctxより
  長いと警告を出し、ctx内に終われば待って何も出さないことを、`internal/server/paramshadow`の
  `TestRecorderCloseFlushes`が、`Close`が要約を書くことを見る
- 後始末が一覧を順に回し、起動していないworkerを待って固まらないことは
  `TestNewServer_FailedConstructionStopsTheRegisteredParts`、重複で構築が
  失敗することは `TestNewServer_DuplicatePartFailsTheConstruction` が見る。値と停止を別々に受ける
  `lifecycle.add`(テストの`addPart`が呼ぶ)が部品に値から作った印を付けず、形を満たす値も拒まないことは
  `TestServer_AddPartDoesNotMarkThePartAsFromValue` が見る
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
  ことを、`TestServer_StartBackgroundForTestIgnoresTheRole`が引数で分けた2つの違い(roleとchart)と、
  server roleでどちらの起動の後も`StopBackground`が起動したものだけを止めること(#3713)を見る。
  `TestServer_ConcurrentStartAndStopLeaveNothingRunning`が、起動と停止を並行に300回呼び、起動した
  部品・worker・schedulerがその後に止まり、autoscaleとchartが残らず、固まらないことを見る(`-race`で
  `autoscale`のフィールドの競合も見る)。組ごとに先に譲る側を替え、起動が先の組と停止が先の組の両方が
  起きたことも確かめる(片方だけでは、もう片方の順の検査が空振りする)。両方のroleの組に加えて、server roleで
  `StartBackgroundForTest`を呼ぶ組も回し、停止が`queueStarted`を読む経路を`-race`で見る(#3713。両方の
  roleではroleの判定で短絡し、`queueStarted`を読まない)
- **起動したqueueの停止(#3713)。** `TestServer_StopBackgroundStopsTheQueueByRoleOrStart`が、
  `StopBackground`がworkerとschedulerの`Shutdown`を呼ぶ回数を、server roleで`StartBackgroundForTest`を
  呼んだ世代(1回ずつ)、server roleでworkerの起動の後にautoscaleの起動が失敗した世代(1回ずつ)、
  server roleでworkerの起動が失敗した世代(呼ばない)、起動していないserver roleの世代(呼ばない)、
  起動していない両方とqueueのroleの世代(1回ずつ。#3713の前と同じ)、`StartBackground`で起動した両方のroleの世代(1回ずつ)で固定し、autoscaleが
  残らないことを見る。`TestServer_StopQueueOfANeverStartedServerRoleDoesNotWait`が、起動していない
  server roleの世代の`stopQueue`が、期限の切れたctxでも待たずに止まったものとして返り、workerと
  schedulerの`Shutdown`を呼ばないことを見る
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

#3713の前後で、本番の経路の動きが同じであることは、次のように確かめた(道具はコミットしていない)。
意図して変えた動き(上の「意図して変えた動き(#3713)」)は、上のテストが見る。

- 起動と停止の順: #3711と同じ使い捨ての記録に、queueのworkerとschedulerの`Start`/`Shutdown`
  (`internal/queue`の`Server`と`Scheduler`)の記録を足して前後の木に当て、#3699と同じ8つの組で、
  `newServer`・`StartBackground`・`StopBackground`のそれぞれの間の起動と停止の列を記録した。8つの組の
  どれも、前後で一致した。どちらの木も2回回して揺れないことを確かめた。同じ8つの組を
  `StartBackgroundForTest`で起動したときの差は、server roleの2つの組(queueInspectorの有無)で、停止に
  schedulerとworkerの`Shutdown`が足されたことだけだった
- `elythia dump-routes`: 出力(ルート)、起動ログ、Redisの命令の列、PostgreSQLの文の集合は、前後で一致した
- `serve`: #3709と同じ方法で、PostgreSQLの文の集合と、Redisの命令とキーの組の集合は、前の木の2回と後の木の
  2回の4通りの組み合わせのどれでも一致した。起動と停止のログの差は、#3707と同じくgormのログに出る
  呼び出し元のファイルのパス(ビルドした木の置き場所)だけだった
- 4.1の道具は、`router.go`を変えていないので数が変わらない

#3715の前後で同じであることは、次のように確かめた(道具はコミットしていない)。

- Goのソース: 後の木の変えた8つのファイルで、新しい4つの名前(と、名前を含む2つのテストの名前)を前の
  名前へ置き換えて戻すと、7つのファイルは前の木と完全に一致し、`lifecycle_parts.go`はコメントの行だけが
  違った。コメント以外のソースは名前の置き換えだけなので、どの組でも動きは変わらない
- `elythia dump-routes`: 出力(ルート)と起動ログは前後で一致した。前の木を2回流して揺れないことも
  確かめた
- 部品の名前と、起動と停止の順は、`TestNewServer_ShutdownStopsThePartsInTheirOrder`の7つの組の表を
  変えずに通った。workerのgoroutineをスタックの関数名で数える`TestNewServer_ConstructionStartsNoPart`の
  印(`server.(*Server).addWorker.func`)は、前の名前に戻すと落ちる
- 4.1の道具の数は変わらない。後から入れる辺は96、呼び出しが離れている辺は659で、前後で同じだった

#3717の前後で同じであることは、次のように確かめた(道具はコミットしていない)。

- 起動と停止の順: #3713と同じ使い捨ての記録に、`hashtagService`の`Shutdown`と`paramShadowRecorder`の
  `Flush`の記録を足して前後の木に当て、#3699と同じ8つの組で、`newServer`・`StartBackground`・
  `StopBackground`のそれぞれの間の起動と停止の列を記録した。8つの組のどれも、差は`fedRuleHits`の
  `Start`が受けたctxの型(前は組み立てが作った`*context.cancelCtx`、後は登録で渡す
  `context.backgroundCtx`。切れるctxは`Start`の中で作る)だけだった。停止の列(一覧の停止の名前と、
  各部品の停止のメソッドと受けたctxの型)は前後で一致した。どちらの木も2回回して揺れないことを確かめた
- `elythia dump-routes`: 出力(ルート)、起動ログ、Redisの命令の列、PostgreSQLの文の集合は、前後で一致した。
  前の木を2回流して揺れないことも確かめた
- `serve`: #3709と同じ方法で、PostgreSQLの文の集合と、Redisの命令とキーの組の集合は、前の木の2回と後の木の
  2回の4通りの組み合わせのどれでも一致した。起動と停止のログの差は、#3707と同じくgormのログに出る
  呼び出し元のファイルのパス(ビルドした木の置き場所)だけだった。前に1回流してから測った(最初の1回は
  `meta`などの初期化の文を出すため)
- 4.1の道具: 節点は358から356になった。後から入れる96辺は行番号を除けば前と同じで、呼び出しが離れている
  辺は659のまま(4.1)

コメントの「shutdown hook」は、この一覧の部品の停止を指す。

### 4.4 キャッシュの一覧 (#3721、#3723、#3725、#3727、#3729、#3732、#3736、#3738、#3776、#3777)

**世代のキャッシュは、そのプロセスが持つ分を全部捨てる口 (`DropLocal()`) を持たせ、名前と由来と
一緒に `Server` の一覧 (`cacheList`、`internal/server/caches.go`) へ登録する。** #3666 の段階 4。
段階 (6) でメンテナンスの作り直しの範囲を狭めるときに、捨てるべきキャッシュをコードの上で列挙
できるようにするためだった。#3802 で作り直しが短い (組み立てと起動で中央値 58ms・最大 71ms。空の DB・プラグイン無し・負荷無しの
隔離した環境で 10 回測った値) と分かり、範囲は狭めないと
決めたので、本番から `dropAll` は呼ばない。一覧は、世代のキャッシュの登録漏れを落とす検査 (下の #3777) の
土台として残す。

```
setupRoutes()
  s.wireCacheInvalidation(…)   // #3901 から、urlPreview より前の登録はこの節 (wiring_cache_invalidation.go)
    s.addCache("cachedMeta", cacheFromDB, cachedMeta)
    s.addCache("metaResponse", cacheFromDB, metaHandler)
    s.addCache("nodeinfo", cacheFromDB, nodeinfoHandler)
    s.addCache("captcha", cacheFromDB, reloadOnDrop(reloadCaptcha))   // #3777。捨てずに読み直す
    s.addCache("reactionBuffering", cacheFromDB, reloadOnDrop(reloadReactionBuffering))
    …
    s.addCache("userCache", cacheFromDB, s.userCache)       // 3 つは newServer で作る (#3723)
    s.addCache("relationCache", cacheFromDB, s.relationCache)
    s.addCache("authTokens", cacheFromDB, s.auth)
    s.addCache("roles", cacheFromDB, roleService)           // #3725。userCache の後
    …
    s.addCache("emojis", cacheFromDB, emojiRepo)            // #3727
    s.addCache("emojiDecorations", cacheFromDB, emojiDecorationResolver)  // emojis の後
    s.addCache("avatarDecorations", cacheFromDB, avatarDecorationResolver)
    s.addCache("instances", cacheFromDB, instanceService)   // #3729
    s.addCache("relays", cacheFromDB, relaySvc)
    s.addCache("fedRules", cacheFromDB, fedRuleService)
    s.addCache("deliverEd25519", cacheFromDB, deliverService)        // #3732
    s.addCache("federationPublicKeys", cacheFromDB, federationResolver)
    s.addCache("deliverSigningKeys", cacheFromDB, deliverProcessor)
    s.addCache("instanceActorSigner", cacheFromDB, instanceSigner)   // userCache の後
    …
  s.addCache("urlPreview", cacheFromExternal, urlPreviewFetcher)   // #3776。peerDeps の直後
  …
  s.addCache("twemojiBadge", cacheFromFile, twemojiBadge)
```

- **`DropLocal()` は他のプロセスに知らせない。** publish も、無効化の hook (`SetInvalidationHook`)
  も呼ばない。誰に捨てさせるかは呼ぶ側が決める。今の捨てる口は型ごとに意味がばらばらで
  (`CachedEmojiRepository.Invalidate()` は一覧だけを捨てる、`RelationCache.InvalidateAll()` は他の
  プロセスにも通知する)、そのままでは 1 つの口として扱えない。既存のメソッドと精密な無効化
  (`metaUpdated` の購読など) は置き換えずに残す
- **名前を `Flush` にしない。** `paramshadow.Recorder.Flush()` やリアクションのバッファ (`corereaction` の
  `bufferedWriter.Flush`) では、`Flush` は
  「溜めた分を書き出す」という逆の意味で使われている
- **由来** (`cacheOrigin`) は DB・設定・外部・ファイルの 4 つ。どの元が変わったときに捨てるべきかを
  表す
- **一覧は登録した順に捨てる** (`cacheList.dropAll`)。ほかのキャッシュから組み立てるものは、元の後に
  登録する。meta の系統では meta 行を先に捨てる。逆だと、間に来たリクエストが古い meta 行から
  `/api/meta` や `/nodeinfo` を組み直して積む (`nodeinfo.Handler.InvalidateCache` の doc と同じ理由)
- **名前が空・由来が不明・値が nil (型付きの nil を含む)・名前の重複・同じ値の 2 回目の登録は、
  構築の失敗にする** (`constructionError`)。不正な登録は一覧に入れない。比較できない値 (型が
  比較できても、interface のフィールドに slice を持つ struct など) は、同じ値かを見られないので
  2 回目の検査を飛ばす (今の値はどれもポインタ)。構築に失敗した世代の
  後始末 (`releaseParts`、4.3) は、この一覧も空にする
- **本番の経路からは `dropAll` を呼ばない。** 段階 (6) で使う予定だったが、#3802 で範囲を狭めないと
  決めた。`wiring` の `caches` も一覧を指すだけで誰も読まなかったので、#3808 で消した (4.2)

移した系統と、残りの系統 (#3666 の段階 4)。

| 系統 | 一覧の名前 | issue |
|---|---|---|
| meta | `cachedMeta`・`metaResponse`・`nodeinfo` | #3721 |
| 利用者と関係 | `userCache` (`CachedUserRepository`)・`relationCache` (`RelationCache`)・`authTokens` (`AuthMiddleware` の tokenCache) | #3723 |
| ロール | `roles` (`role.Service`。利用者のロール・ロールの一覧と、プラグインの policy の結果) | #3725 |
| 絵文字とデコレーション | `emojis` (`CachedEmojiRepository`。ローカルの一覧と (name, host) の entry)・`emojiDecorations` (`avatardecoration.EmojiResolver`)・`avatarDecorations` (`avatardecoration.Resolver`。catalog) | #3727 |
| 連合 (その 1) | `instances` (`instance.Service`。ホストの停止の判定と応答の健全)・`relays` (`relay.Service`。承認済みの relay の一覧)・`fedRules` (`fedrule.Service`。連合のルールのスナップショット) | #3729 |
| 連合 (その 2) | `deliverEd25519` (`DeliverService`。相手の Ed25519 の対応と送る側の Ed25519 の鍵)・`federationPublicKeys` (`Resolver`。相手の公開鍵と、Ed25519 の鍵が無いことの記憶)・`deliverSigningKeys` (`DeliverProcessor` の解析済みの署名鍵)・`instanceActorSigner` (instance actor の秘密鍵) | #3732 |
| 投稿・タイムライン・通知 | `antennas` (`CachedAntennaRepository`。有効なアンテナの全件)・`pushSubscriptions` (`webpush.SubscriptionCache` のメモリの層)・`featuredNotes` (`notes.FeaturedRankingCache`。注目ノートの全体の順位)・`notePublisher` (`stream.NotePublisher`。直前に pack したノート)・`timelineJSON` (`notes.TimelineJSONCache`。opt-in のタイムラインの最初の頁) | #3736 |
| 管理画面と統計 | `driveUsage` (`driveusage.Service`。ドライブの使用量の集計)・`dbHealth` (`dbhealth.Service`。DB の統計)・`statsCounts` (`stats.Handler`。`/api/stats` の instance と note_reaction の総数)・`ipLogWindow` (`iplog.Service`。書いた (利用者, IP) の印) | #3738 |
| 外部とファイル | `urlPreview` (`urlpreview.Fetcher`。URL ごとのプレビュー)・`remoteStats` (`federation.RemoteStatsFetcher`。リモート利用者の統計の件数と作成日時)・`fetchRSS` (`fetchrss.Handler`。URL ごとの pack 済みの本文)・`pluginPeers` (`nodeInfoPeerLister`。相手の nodeinfo にあるプラグインの名前)・`twemojiBadge` (`twemojiBadgeHandler`。名前ごとの焼いた PNG) | #3776 |
| 起動時の写し(段階4の最後) | `captcha`(captchaのproviderの組)・`reactionBuffering`(リアクションのバッファの切り替え)。どちらも捨てずにmetaから読み直す項目(`reloadOnDrop`) | #3777 |

- 利用者と関係の系統の 3 つは、どれもこのプロセスだけで全部を捨てる処理を元から中に持っていて
  (`invalidateAllLocal`・`apply({All: true})`・`tokenCache.invalidateAll`)、`DropLocal()` はそれを
  呼ぶ。全体を捨てた時刻 (`allInvalidatedAt`) を持つので、捨てる前に読み始めた結果は積まない。
  `RelationCache.InvalidateAll()` は他のプロセスにも通知するので、一覧からは呼ばない
- この 3 つの間に捨てる順の制約は無い。tokenCache は利用者を DB から読み (native token は
  `FindByToken`、app token は `FindByHashOrToken` の Preload)、利用者のキャッシュを通らない
- ロールの `DropLocal()` は、`rolesUpdated` を受けたときと同じ `InvalidateAllCachesLocally()` を呼ぶ。
  世代の番号 (`allUserRoleEpoch`・`rolesListEpoch`・`globalEpoch`) を進めるので、捨てる前に始まった
  読み込みや policy の解決の結果は積まない
- **`roles` は `userCache` の後に登録する。** 条件付きロールは利用者の行 (`CachedUserRepository`) から
  評価して利用者のロールのキャッシュに積むので、先に捨てると、間に来たリクエストが古い利用者の行で
  評価した結果を積む。meta の base policy はロールのキャッシュに持たず、評価のたびに meta のキャッシュ
  (`cachedMeta`) から読むので、meta の系統との間に順の制約は無い
- 絵文字の `DropLocal()` は、ローカルの一覧と (name, host) の entry の両方を捨てる。公開の
  `Invalidate()` はローカルの一覧しか捨てないので使わない。2 つのデコレーションの `DropLocal()` は
  `InvalidateLocally()` を呼ぶ (公開の `Invalidate()` は他のプロセスに通知する)
- **`emojiDecorations` は `emojis` の後に登録する。** 絵文字由来のデコレーションは絵文字のキャッシュの
  一覧 (`ListLocal`) から作り直すので、先に捨てると、間に来たリクエストが古い一覧から作り直した map を
  積む (他のプロセスからの通知を受ける `cachebus.SubscribeEmojiCache` が同じ順に捨てるのと同じ理由)。
  catalog は DB から読むので順の制約は無い
- `instances` の `DropLocal()` は、停止の判定・応答の健全・受信時刻の窓 (`requestReceivedCache`) を
  捨てる。受信時刻の窓は書き込みの間引きだが、窓の間は SELECT も省くので応答なしの解除と自動停止の解除の
  判定も飛ぶ。古い窓を残すと、戻した DB で停止になっている相手からの正当な受信でも窓 (5 分) の間解除され
  ない (失うのはホストごとに 1 回分の間引きだけ)。捨てる前に読み始めた行から作ったものを積まないよう、
  受信時刻の窓には世代の番号 `dropGen` (#3729。進めるのは `DropLocal()` だけ)、応答の健全には
  `healthGen` (#3729 で `dropGen` と共用で足し、#3731 で分けて not-responding への遷移でも進めるようにした)
  を持たせた。警告の間引き (`lastMetaWarn`) はログのためだけなので捨てない
- `relays` は `InvalidateCacheLocally()` を、`fedRules` は `Invalidate()` を呼ぶ。連合のルールは捨てても
  すぐには空にせず、次の評価で読み直す。**`dropAll` が返った後も、前のスナップショットで評価する時間が
  残る**: 読み直しは 1 つの goroutine だけが行い、並行する評価は読み直しが終わるまで前のスナップショットを
  使う。読み直しに失敗したら `retryInterval` (30 秒) の間、前のスナップショットのまま (空にすると拒否して
  いたものが全部通るため。今と同じ)。戻した直後に同期で読み直す (`Reload()`) かは段階 (6) で決める予定
  だったが、本番から `dropAll` を呼ばないので要らなくなった (#3802。DB を戻すと世代ごと作り直す)。
  3 つとも DB を直接読むので、ほかの系統との間に順の制約は無い
- **キャッシュとして扱わず、一覧に入れないもの。** 値のキャッシュではなく、取得の backoff・同時実行の
  上限・ログの間引きで、捨てると間引きが外れるだけになる: `instance.Service` の警告の間引き
  (`lastMetaWarn`)、`mentionFetchGuard`、`Resolver.keyFetchFailures`、`MetadataRefresher` の実行中の印、
  `clip.Service` の `updated` の間引きの窓 (#3809)
- `MetadataRefresher` の次の確認の時刻 (`nextCheck`) は、DB の `infoUpdatedAt` から計算した取り直しの
  時刻で、その間は DB も見ない。`dropAll` だけで捨てて同じ世代のまま DB を戻すと、戻した行の nodeinfo
  (ソフトウェアの版など) のまま最大 24 時間取り直さない。#3729 では一覧に入れなかった。メンテナンスでは
  世代ごと作り直して捨てるので、一覧に入れる必要は無い (#3802)
- `activitypub.PublicKeyCache` (プラグインの peer の `s.peerDeps.keyCache`、inbox の handler と
  processor が持つもの) と、配送の `keyCache` のうち (kind, keyID, PEM) で引く entry は、PEM の
  解析の結果を覚えるだけで古くならないので、どの系統にも入れない (PEM を鍵に含めて引くので、相手の鍵が
  変われば別の entry になる。PEM 自体は `federationPublicKeys` などの登録済みのキャッシュか DB から来る)。配送の `keyCache` のうち署名者の
  利用者で引く entry は DB の秘密鍵の写しで、#3732 で `deliverSigningKeys` として一覧に入れた (同じ LRU に
  入っているので、捨てるときは解析だけの entry も一緒に捨てる)
- **#3732 の 4 つは、捨てる口と、捨てる前に読んだ値を積まない守りを新しく足した。** `DeliverService`・
  `Resolver`・`DeliverProcessor` は世代の番号を持ち、`DropLocal()` が進めた後は、捨てる前に始まった
  読み込みの結果を積まない。instance actor の秘密鍵は読み込みの間も lock を握るので、`DropLocal()` は
  読み込みが終わるのを待ってから捨てる (番号は要らない)。この秘密鍵だけは読み込みの失敗の backoff も
  捨てる。自分の DB の読み込みの失敗なので、DB を戻した後には当てはまらない (残すと 30 秒の間、署名なしの
  取得が続く)。`DeliverService` は
  singleflight の鍵にも番号を含め、捨てた後の呼び出しが捨てる前の読み込みに相乗りしないようにした。
  `Resolver` が捨てるのは相手の公開鍵と Ed25519 の鍵が無いことの記憶で、取得の失敗の backoff
  (`keyFetchFailures`) は捨てない。鍵を捨てた直後は、`ResolveActor` が鍵の無い actor を相手から取り直す
  ので、外への取得が `keyFetchBackoff` の範囲で増える。instance actor の秘密鍵は、#3732 まで AP の取得用と
  プラグインの peer 用に 2 つ作っていたのを、世代に 1 つにまとめた。system account の利用者を
  `CachedUserRepository` から引くので、`userCache` の後に登録する。ほかの 3 つは DB を直接読む
- 投稿・タイムライン・通知の系統 (#3736) のうち、アンテナ・注目ノートの順位・ストリームの直前のノートは、
  読み込み (pack) の間もロックを握るので、`DropLocal()` は読み込みが終わるのを待ってから捨てる (番号は
  要らない)。アンテナの `DropLocal()` は、hook を呼ばない `Invalidate()` と同じ。タイムラインの JSON と
  push 購読はロックの外で読むので、世代の番号 (`gen`) を足し、捨てる前に読み始めた値を積まないようにした。
  タイムラインの JSON は番号を組み立ての前に読む。push 購読は、捨てた後には読んだ行を Redis の層にも
  書かない
- **ロックや番号で守れるのは、キャッシュ自身が読む分だけ。** 呼び出し側が捨てる前に読んだ値は残りうる。
  タイムラインの JSON は、認証で解決した閲覧者 (`authTokens`・`userCache` から引いたもの) を番号より前に
  受け取るので、捨てる前に解決した閲覧者で組み立てた応答を、捨てた後の番号で積みうる (TTL の間返る)。
  匿名の閲覧者に見せる範囲 (`visitorPolicy`) も番号より前に読むが、キーに入っているので、設定が変わった
  後のリクエストは別のキーを引く。
  ストリームの直前のノートも、呼び出し側が捨てる前に読んだ投稿者で pack すると、次の別のノートまで
  使い回す。本番から `dropAll` を呼ぶなら受け付けを止めてから捨てる前提が要るが、#3802 で呼ばないと決めた
- **push 購読の `DropLocal()` はメモリの層だけを捨てる。** Redis の層 (`userSwSubscriptions:*`) は
  プロセスの間で共有していて、DB を戻したときは `CleanRedisAfterRestore` が消す。`dropAll` だけでは、
  Redis に残った行をメモリへ読み直す
- **ストリームの直前のノートとタイムラインの JSON は、この系統の最後に登録する。** どちらも pack した
  利用者・ロール・絵文字・デコレーション・インスタンスを含むので、先に捨てると、間に来た pack が古い元から
  作り直して積む。注目ノートの順位は Redis の順位から引いた ID だけを持ち、ノートは毎回 DB から読む。
  タイムラインの JSON は opt-in (`enableTimelineCache`) で、有効にしたときだけ登録する
- 管理画面と統計の系統 (#3738) のうち、ドライブの使用量・DB の統計・`/api/stats` の件数は、どれも
  ロックの外で `singleflight` の中で読み、読み終えたら積む。世代の番号を足し、捨てる前に読み始めた値を
  積まないようにした。`singleflight` の鍵にも番号を含め、捨てた後の呼び出しが捨てる前の読み込みに
  相乗りしないようにした (`DeliverService` と同じ)。DB の統計の読み込みは呼び出し元の ctx から切り離して
  いて最大 30 秒かかるので、相乗りさせると、捨てた後の呼び出しがその間古い統計を受け取る
- IP の記録の窓 (`ipLogWindow`) は値のキャッシュではなく、書き込みを重ねないための印で、DB を戻すと
  戻した DB に行が無いのに記録済みとして窓 (1 時間) の終わりまで書かない。`DropLocal()` は印を全部捨て、
  窓を捨てた時刻から数え直す。印はロックの中で付け、DB への書き込みはその後に行うので、番号は要らない。
  捨てる前に始まった書き込みが捨てた後に失敗すると、捨てた後に付けた同じ印を外すが、書き込みは (利用者,
  IP) で upsert するので、行は 1 つのまま観測の回数が 1 つ増えるだけになる
- この 4 つは DB を直接読むか、DB へ書いたことの印を持つだけで、ほかのキャッシュの値を含まないので、
  捨てる順の制約は無い。投稿・タイムライン・通知の系統より後に、それらの pack の元になるキャッシュを
  置かないため、その前に登録する。
  ドライブの使用量は、管理画面の handler に渡す所の引数の中で作っていたのを、一覧に登録するために変数に
  した
- ドライブのストレージの backend (`coredrive.MetaStorage`) は、どの系統にも入れない。使うたびに meta
  (`cachedMeta`) を読み、オブジェクトストレージの設定から作った鍵が変わったときだけ作り直すので、meta を
  捨てれば追従する (meta が読めないときは、前の backend を使い続ける)
- `instance` の書き込みのバッファ (`TouchBuffer`・`CapabilityBuffer`・`CounterBuffer` の保留) は値のキャッシュではなく
  書き込みの保留で、捨ててはいけない。メンテナンスに入ると世代を止め、`StopBackground` の中の `Close` が
  保留の分を書き出してから閉じる (DB を戻すのはその後)。段階 (6) で別に決める予定だったが、#3802 で範囲を
  狭めないと決め、決めることは無くなった
- **外部とファイルの系統 (#3776) で、由来の `cacheFromExternal` と `cacheFromFile` を初めて使った。**
  URL プレビュー・リモート利用者の統計・RSS・peer のプラグイン一覧は外から取った値を、twemoji のバッジは
  ファイル (`frontendutil.TwemojiDir()`、`MISSKEY_TWEMOJI_DIR`) を描いた値を持つ。5 つとも他のプロセスへ
  伝える仕組み (Redis の層や publish) を持たず、ほかのキャッシュの値を含まないので、捨てる順の制約は無い。
  RSS と peer のプラグイン一覧はそれまでの登録の並びより後で作るので、5 つとも `peerDeps` を作った直後
  (プラグインの準備の `return` より前) に登録する。一覧の最後は投稿・タイムライン・通知の系統ではなくなった
  が、この 5 つは pack の元にならないので、`notePublisher` と `timelineJSON` の順の制約は崩れない
- 5 つとも、取得 (twemoji のバッジはファイルの読み込みと描画) をロックの外で行い、終わったらそのまま
  積んでいた。世代の番号を足し、取得の前に番号を読み、比べることと積むことを同じロックの中で行う。
  `singleflight` を使うリモート利用者の統計と RSS は鍵にも番号を含め、捨てた後の呼び出しが捨てる前の
  取得に相乗りしないようにした。RSS は同時実行の上限 (`inflight`) の鍵にも番号を含める。含めないと、
  捨てた後の取得を「既存の取得への合流」と数え、上限を超えて取得する。外の値なので、捨てる前に読み始めた
  値も同じ相手から数秒前に取った値にすぎないが、これまでの段と揃えた。URL プレビューは戻す前の条件で
  取った値を 1 時間積みうるし、twemoji のバッジは TTL が無いので、ファイルの差し替えと重なると古い描画が
  残り続けるため
- URL プレビューの `Fetcher` は、リクエストごとに構造体を複製する (`forRequest`)。LRU・mutex・世代の
  番号はポインタの先の型 (`previewCache`) にまとめ、複製しても同じものを指すようにした
- **URL プレビューの取得の条件 (redirect の可否、本文の上限など) は meta の設定で決まるが、
  `metaUpdated` では捨てない。** DB を戻したときや設定を変えたときに、前の条件で取った結果が最大 1 時間
  残る (以前からの挙動)。meta から作った値ではなく外から取った値なので、由来は `cacheFromExternal` にした
- **キャッシュとして扱わず、捨てないもの。** リモート利用者の統計の `createdHostDown` (応答しなかった
  ホストを 5 分覚える) は取得の間引きで、`DropLocal()` でも残す。捨てると、応答しない相手にフォローの
  処理が 5 秒ずつ待たされる守りが外れる。RSS の同時実行の上限 (`inflight`) と peer のレート制限
  (`peerRateLimiter`) も値のキャッシュではない
- peer のプラグイン一覧と twemoji のバッジは、interface のフィールドの中 (`pluginPeerDeps.remote`) と
  ルートの登録の引数の中で作っていたのを、一覧に登録するために変数 (`pluginPeers`・`twemojiBadge`) にした
- **今の `dropAll` は由来を見ずに全部を捨てる。** DB を戻すたびに呼ぶと、外への取得 (URL プレビュー・
  統計・RSS・nodeinfo) が一斉に増える。由来で絞るかは段階 (6) で決める予定だったが、本番から呼ばない
  ので要らなくなった (#3802)

- **captchaのproviderの組と、リアクションのバッファの切り替え(`enableReactionsBuffering`)は、捨てずに
  読み直す項目として登録する(#3777)。** どちらも構築のときにmetaから作り、`metaUpdated`のhookと購読で
  読み直す(`reloadCaptcha`・`reloadReactionBuffering`)。`cachedMeta`の`DropLocal()`はhookを呼ばないので、
  一覧を捨てるだけでは、captchaは戻す前の組を**期限なく**使い続け、リアクションの切り替えは次のtick
  (最大30秒。その間の加算はどちらのモードでも正しく入る)まで前のままになる。一覧には、`DropLocal()`が
  同じ関数を呼ぶ項目(`reloadOnDrop`、`internal/server/caches.go`)を登録する
- **この2つの`DropLocal()`は、捨てずに読み直す例外。** captchaの組を捨てて空にすると、`Verify`は成功を
  返す(captchaが無効と同じ)ので、安全でない側に倒れる。metaが読めなければ、`metaUpdated`のときと同じく
  前の値を据え置く。`cachedMeta`から読むので、`nodeinfo`の後(meta行を捨てた後)に登録する。逆だと、
  捨てる前の行から読み直す。他のプロセスへは何も送らない
- **DBが読めないときに`dropAll`を呼ぶと、captchaは戻す前の組のまま残る。** 読み直しが読めずに据え置き、
  次の`metaUpdated`(管理画面での変更か他のプロセスからの通知)まで読み直さない。リアクションの切り替えは
  次のtick(30秒)で読み直す。呼ぶならDBが読める状態になってから呼ぶ。本番からは呼ばない(#3802)
- **DeepLのキーは登録しない。** #3663から、翻訳のたびに`cachedMeta`から読むので、meta行を捨てれば揃う
- **読めないときの代わりにだけ使う起動時の値は、登録しない。** chartのフラグ(`ChartsForRemoteUser`など、
  `charthook`)・センシティブの判定の設定(`SetSensitiveSettingsProvider`に渡す`sensCfg`)・
  `proxyRemoteFiles`(`SetProxyRemoteFilesLookup`)は、どれも使うたびに`cachedMeta`から読み、読めなかった
  ときだけ起動時の値を使う。meta行を捨てれば揃い、捨てても代わりが要るので、捨てる対象ではない
- **`/api/emojis`の符号化済みの応答(`api/emojis.Handler.encoded`、#3593)も登録しない。** 使うたびに、
  `emojis`(`CachedEmojiRepository.ListLocal`)が返した一覧と同じsliceかを比べ、違えば作り直すので、
  `emojis`を捨てれば追従する。これまでどこにも記録が無かったので、下の検査の除外に理由付きで載せた
- **`meta`のsetupの印は、組み立てごとの値にした(#3803)。** ローカルの利用者を一度でも見たことを覚える
  印で、以前はパッケージの変数(`setupLatch`)として世代をまたいで残った。利用者の数え方は#3782で、組み立てで
  作る`meta.SetupDetector`として`/api/meta`とfrontendのshellの4つの経路(通常・OAuthの同意画面・permalinkの
  SSR・埋め込み)へ渡す形にし、#3803で印もその値のフィールドにした。組み立て直した世代は、最初のリクエストで
  1回数え直す。一覧には入れず(`DropLocal()`を持たない)、下の検査では`throttle`として除外する。同じ世代の
  ままDBを利用者のいない状態へ戻すと、印は残る
- **frontendのloader(`loader/style.css`・`loader/boot.js`)も、組み立てのときに読む(#3803)。** 以前は
  `frontendutil`がプロセスで1回だけ読んで持っていたので、メンテナンスの間にfrontendを作り直すと、組み立て
  直した世代は新しいentry(`clientEntryFor`。組み立てのたびに読む)と古いloaderを組み合わせ、#2551と同じ形で
  表示が崩れた。今はentryと同じく組み立てのときに読み(`loaderAssetsFor` / `embedLoaderAssetsFor`)、shellの
  4つの経路へ渡す。devでは埋め込まない分岐はそのまま。読む時点は、以前の「最初のリクエスト」から組み立てに
  移った。組み立ての時点でloaderが読めないと、その世代は埋め込まずにURL参照(`/vite/loader/*`)を出し続ける
  (entryも同じで、作り直すか再起動すると読み直す)
- **ストリームの接続ごとの写しは、`dropAll`の後も接続が続く間は残る。** `stream.Connection`のフォロー・
  ハードミュート・ミュートとブロック・policyの写し、リストのチャンネルのメンバーの設定、チャットの部屋を
  見られるかの確認(期限まで)は、接続やチャンネルの開始のときにDBから読む。一覧には入れず、下の検査では
  `perConnection`として除外する。DBを戻すときは世代ごと作り直し、ストリームの接続も閉じる(1012)ので、
  写しは残らない(#3802で範囲を狭めないと決めた)
- **登録漏れは、静的な検査と実行時の検査の2つで落とす(#3777)。** 静的な検査(`make cachelist-check`、
  [gates.md](gates.md#キャッシュの一覧への登録漏れcachelist-check3777))は、同期の部品(`sync.Mutex`
  など)を持つstructとパッケージの変数、`DropLocal()`を持つstruct、それから届くmapのstructを全部数え、
  表(`cacheEntries`)で種類と理由を書かせる。表は全部のフィールドの名前を持つので、どのフィールドを
  足しても分類し直すまで落ちる。`DropLocal()`の型の行は、捨てずに残す状態を理由付きで持つ(`keeps`)。
  実行時の検査(`TestCaches_ListMatchesTheDropLocalTypes`)は、タイムラインのJSONを有効にして組んだ
  サーバーの一覧の値の型の集合が、同じ走査で`DropLocal()`を持つとした型の集合と一致することを見る。
  前者は「キャッシュなら`DropLocal()`を持つか、理由がある」を、後者は「`DropLocal()`を持つなら一覧に
  ある」を守る。走査は`internal/entitycompat`の非テストのコード(`ScanCacheTypes`)に置き、両方から呼ぶ。
  型の一覧を書き写すと、片方だけ直してずれるため
- 表の数(#3788の時点で134行): `dropLocal`が31、持ち主の`DropLocal()`が捨てるか、その後の次の読み込みで
  置き換わる`droppedByOwner`が8
  (`metaResponseCache`・`missingExtraKeys`・`policyProviderRuntime`・`previewCache`・`emojiKeyCache`・
  `relationKindCache`・`tokenCache`・`fedrule.compiledRule`)。除外は95で、`notCache`(値の写しを持たない。
  lock・singleflight・世代の起動と停止・数え・ジョブキューの接続・コードから1回だけ組む値)が58、`processGlobal`(世代をまたぐ
  パッケージの状態。`setupLatch`を除き段階(5))が6、`writeBuffer`(書き込みや他のプロセスへの通知の保留)が7、
  `perConnection`が6、`throttle`(間引き)が5、`contentKeyed`(元の内容を鍵に含める)が5、`perCall`が4、
  `followsRegistered`が2(`emojis.Handler`・`coredrive.MetaStorage`)、`reloadOnDrop`が2(`captcha.Service`・
  `reaction.SwitchableWriter`)。`Resolver`から届く`mentionFetchGuard`・`resolveGroup`・`resolveWaits`などは、
  `Resolver.DropLocal()`が捨てないので除外にした。`MetadataRefresher`の`nextCheck`は上のとおり一覧に
  入れないので、`throttle`として除外に載せた
- #3800で、既定の`entity.Packer`の2行(`processGlobal`)と`constructionMu`(`notCache`)を表から消した。
  `processGlobal`は4、`notCache`は57になった(`grep -c "{kindProcessGlobal,"`などで数えた)
- #3803で、`meta.setupLatch`と`frontendutil.loaderOnce`・`embedLoaderOnce`の3行(`processGlobal`)を消し、
  印を持つようになった`meta.SetupDetector`の1行(`throttle`)を足した。`processGlobal`は1、`throttle`は6、表は
  129行になった(種類ごとは`grep -c "{kindThrottle,"`など、全体は`grep -cE '^\s"[^"]+": \{kind'`で数えた)

テスト (`internal/server/caches_test.go` と、各型のパッケージ):

- `TestCacheList_RejectsInvalidRegistrations` / `TestCacheList_AcceptsNonComparableValues` /
  `TestCacheList_DropAllInOrder` / `TestNewServer_InvalidCacheFailsTheConstruction`: 不正な登録、
  比較できない型、捨てる順、構築の失敗と後始末
- `TestCaches_MetaLine`: サーバー全体を組み、meta の系統が捨てる順で並んでいることを、名前の下限
  (`metaCachesInOrder`) で確かめる。DB の meta 行を直接書き換えて、`dropAll` の後に `/api/meta` と
  `/nodeinfo` が読み直すこと (登録した値が配線で使われている値と同じこと) と、捨てても
  `internal:metaUpdated` を送らないことを見る
- `TestCachedMetaRepository_DropLocalDropsWithoutHook`・`TestMeta_DropLocalDropsBothVariants`・
  `TestNodeinfo_DropLocalDropsEveryVersion`: 各型の `DropLocal()` が全部を捨て、hook を呼ばないこと
- `TestCaches_UserLine` (#3723): 利用者と関係の系統の 3 つが、サーバーが使う値のまま一覧にあることを
  名前の下限 (`userLineCaches`) で確かめる。DB の行 (利用者の名前と凍結、ミュート) を直接書き換えて、
  `dropAll` の後に利用者のキャッシュ・関係のキャッシュと、`/api/i` の認証 (凍結で 403 になる) が読み
  直すことを見る。捨てても `internal:` のチャンネルへ何も publish しないことを、世代を止めて
  (利用者の通知の待ち行列を送り切らせて) から送る目印で確かめる
- `TestCachedUserRepository_DropLocal`・`TestRelationCache_DropLocalDoesNotForward`・
  `TestAuthMiddleware_DropLocal` (#3723): 各型の `DropLocal()` が全部を捨て、他のプロセスへ伝える
  hook や publisher に何も渡さないこと。読み込みの途中で捨てたときに読んだ行を積まないことは、
  利用者のキャッシュは `TestCachedUserRepository_DropLocal` が、tokenCache は
  `TestAuthMiddleware_InvalidationDuringReadIsNotStored` の `drop` の場合が見る
- `TestCaches_RoleLine` (#3725): `roles` がサーバーの使う `role.Service` のまま、`userCache` より後に
  一覧にあることを確かめる。
  DB にロール (管理者) と割り当てを直接書いて、`dropAll` の後に `IsAdministrator` と管理 API の認可
  (`/api/admin/meta` が 200 になる) が読み直すことを見る。`internal:` へ何も publish しないことも見る
  (`watchInternal`。`TestCaches_UserLine` と共通)
- `TestDropLocal_DropsRolesAndPolicyResultsWithoutNotify`・
  `TestEffectivePolicy_DropLocalDoesNotReturnJoinedStaleFlight` (#3725): ロールのキャッシュと
  プラグインの policy の結果を全部捨て、hook を呼ばないこと。捨てる前に始まった policy の解決の
  結果を、捨てた後のリクエストに返さないこと
- `TestCaches_EmojiLine` (#3727): 3 つがサーバーの使う値のまま一覧にあり、`emojiDecorations` が
  `emojis` より後にあることを確かめる。DB に絵文字とデコレーションを直接書いて、`dropAll` の後に
  絵文字の一覧・(name, host)・絵文字由来のデコレーション・catalog が読み直すことを見る。絵文字の通知は
  待ち行列から非同期に送るので、世代を止めてから `internal:` へ何も publish していないことを見る
- `TestEmojiKeyCache_DropLocal`・`TestEmojiKeyCache_DropLocalDuringFetchDiscardsResult`・
  `TestResolvers_DropLocal` (#3727): 各型の `DropLocal()` が全部を捨て、hook を呼ばないこと。
  (name, host) の entry と 2 つのデコレーションは、読み込みの途中で捨てたときに読んだ結果を積まない
  こと (世代の番号 `gen`)。絵文字の一覧はテストしていない。`ListLocal` は排他のロックを握ったまま
  読むので、`DropLocal()` は読み込みが終わるまで待ってから捨てる
- `TestCaches_FederationLine` (#3729): 3 つがサーバーの使う値のまま一覧にあることを確かめる
  (`fedRules` は `crossProcessCaches` から引く)。DB のインスタンスを停止にし、承認済みの relay と、
  そのホストのノートを拒否するルールを直接書いて、`dropAll` の後に配送の判定・relay の判定・ルールの
  評価が読み直すことを見る。`internal:` へ何も publish しないことも見る
- `TestService_DropLocal` (instance)・`TestRecordResponseSuccess_DropLocalDuringReadIsNotCached`・
  `TestMarkRequestReceived_DropLocalDuringReadIsNotCached`・
  `TestDropLocal` (relay)・`TestService_DropLocal` (fedrule) (#3729): 対象 (instance は受信時刻の窓も)
  を全部捨て、hook を呼ばないこと。応答の健全と受信時刻の窓は、読み込みの途中で捨てたときに読んだ行から作ったものを
  積まないこと。
  連合のルールは、読み直しに失敗したら前のルールを使い続けること
- `TestCaches_FederationKeysLine` (#3732): 4 つがサーバーの使う値のまま一覧にあり、instance actor の
  秘密鍵が `userCache` より後にあること、AP の取得とプラグインの peer が一覧の秘密鍵と同じ値を使うことを
  確かめる。DB の相手の公開鍵と instance actor の秘密鍵を直接書き換えて、`dropAll` の後に読み直すことと、
  `internal:` へ何も publish しないことを見る。配送の 2 つは job を流さないと観測できないので、各
  パッケージのテストに任せる
- `TestDeliverService_DropLocal`・`TestDeliverService_DropLocalDuringReadIsNotCached`・
  `TestDeliverService_DropLocalDoesNotJoinAnOlderRead`・`TestResolver_DropLocal`・
  `TestResolver_DropLocalDuringReadIsNotCached`・`TestResolver_DropLocalForgetsMissingExtraKeys`・
  `TestResolver_DropLocalKeepsFetchBackoff`・`TestMissingExtraKeys_DropAll`・
  `TestDeliverProcessor_DropLocal`・`TestDeliverProcessor_DropLocalDuringReadIsNotCached`・
  `TestInstanceActorSigner_DropLocal` (#3732):
  対象を全部捨て、backoff を残すこと。読み込みの途中で捨てたときに読んだ値を積まず、相乗りもさせない
  こと
- `TestCaches_NotesLine` (#3736): 5 つがサーバーの使う値のまま一覧にあり、`notePublisher` と
  `timelineJSON` が pack の元のキャッシュ (`notesLinePackedFrom`) より後にあることを確かめる。DB に
  アンテナ・push 購読・ノートを直接書いて、`dropAll` の後にアンテナの一覧・push 購読・
  `/api/notes/local-timeline` が読み直すことと、`internal:` へ何も publish しないことを見る。注目ノートの
  順位とストリームの直前のノートは、値が同じであることだけを見て、捨てることは各パッケージのテストに任せる。
  `TestCaches_TimelineJSONOnlyWhenEnabled` は、タイムラインの JSON を有効にしないと一覧に無いことを見る
- `TestCachedAntennaRepository_DropLocal`・`TestCache_DropLocal_DropsMemoryOnly`・
  `TestCache_DropLocal_DuringReadIsNotStored` (webpush)・`TestFeaturedRankingCache_DropLocal`・
  `TestTimelineJSONCache_DropLocal`・`TestTimeline_Cache_DropDuringBuildIsNotStored`・
  `TestNotePublisher_DropLocal` (#3736): 対象を全部捨てること。アンテナは hook を呼ばないこと。push 購読は
  Redis の層を残すこと。タイムラインの JSON と push 購読は、読み込みの途中で捨てたときに読んだ値を積まない (push 購読は
  Redis にも書かない) こと
- `TestCaches_AdminStatsLine` (#3738): 4 つがサーバーの使う値のまま一覧にあることを確かめる。DB に
  instance を直接足し、IP の記録の行を消して、`dropAll` の後に `/api/stats` の件数・
  `/api/admin/drive/usage`・`/api/admin/database-health` が読み直すことと、`/api/i` へのリクエストで
  IP の記録を書き直すことを見る。どれも API を通すので、一覧に登録した値と、管理画面・`/api/stats`・
  認証の middleware が使う値が同じであることも確かめる。`internal:` へ何も publish しないことも見る
- `TestService_DropLocal`・`TestService_DropDuringAggregation` (driveusage)・`TestService_DropLocal`・
  `TestService_DropDuringRead` (dbhealth)・`TestStats_DropLocal`・`TestStats_DropDuringCount`・
  `TestService_DropLocal` (iplog) (#3738): 対象を全部捨てること。集計の 3 つは、読み込みの途中で捨てた
  ときに読んだ値を積まず、捨てた後の呼び出しを相乗りさせないこと。IP の記録は窓を捨てた時刻から数え直す
  こと
- `TestCaches_ExternalLine` (#3776): 5 つがサーバーの使う値のまま、決めた由来 (`externalLineCaches`) で
  一覧にあることと、peer のプラグイン一覧がプラグインの通信 (`peerDeps.remote`) と同じ値であることを
  確かめる。`MISSKEY_TWEMOJI_DIR` を一時ディレクトリに向け、URL プレビューと RSS は 127.0.0.1 の
  httptest のサーバーを `allowedPrivateNetworks` で許して、相手の内容とファイルを変えてから、`dropAll` の
  後に `/url`・`/api/fetch-rss`・`/twemoji-badge/…` が取り直す (描き直す) ことと、`internal:` へ何も
  publish しないことを見る。リモート利用者の統計と peer のプラグイン一覧は相手へ https で取りに行くので、
  値が同じであることだけを見て、捨てることは各パッケージのテストに任せる
- `TestFetcher_DropLocal`・`TestFetcher_DropLocalDuringFetchIsNotStored` (urlpreview)・
  `TestRemoteStatsFetcher_DropLocal`・`TestRemoteStatsFetcher_DropDuringFetch`・`TestFetchRSS_DropLocal`・
  `TestFetchRSS_DropDuringFetch`・`TestFetchRSS_DropDuringFetchCountsTowardTheCap`・
  `TestNodeInfoPeerLister_DropLocal`・`TestNodeInfoPeerLister_DropLocalDuringFetchIsNotStored`・
  `TestTwemojiBadge_DropLocal`・`TestTwemojiBadge_DropLocalDuringReadIsNotStored` (#3776): 対象を全部捨てる
  こと (リモート利用者の統計は `createdHostDown` を残すこと)。取得の途中で捨てたときに取った値を積まない
  こと。リモート利用者の統計 (統計と作成日時の両方) と RSS は、捨てた後の呼び出しを相乗りさせないこと。
  RSS は、捨てた後の取得を同時実行の上限に数えること。URL プレビューは設定を毎回読む構成 (`forRequest` で
  複製する) で確かめる。twemoji のバッジは、ファイルを名前付きパイプにして読み込みの途中で止める (unix だけ)

- `TestCaches_ReloadedFromMeta` (#3777): 読み直しの2つの項目が、決めた順(`reloadedOnDrop`)で一覧にある
  ことを確かめる。testcaptchaとリアクションのバッファを無効にして組み、DBのmetaで両方を有効にしてから、
  `dropAll`の後にcaptchaの`Verify`が新しいproviderの組で検証し(トークンが無ければ落ちる)、リアクションの
  writerがバッファへ切り替わることと、`internal:`へ何もpublishしないことを見る。読み直しの項目はfuncの型
  なので値を比べられず、サーバーの使う値(`crossProcessCaches.captcha`・`reactionCountWriter`)の振る舞いで
  確かめる。`metaCachesInOrder`にも2つを足し、`nodeinfo`の後にあることを`TestCaches_MetaLine`が見る。
  `TestReloadOnDrop_DropLocalReads`は、`DropLocal()`が読み直しの関数を毎回呼ぶことを見る
- `TestCaches_ListMatchesTheDropLocalTypes` (#3777、`caches_types_test.go`): タイムラインのJSONを有効にして
  組んだサーバーの一覧の値の型の集合が、`entitycompat.ScanCacheTypes`が`DropLocal()`を持つとした型の集合と
  一致すること。型の集合を比べるので、同じ型の2つ目の値の登録漏れは見えない。読み直しの2つの項目は同じ型
  (`reloadOnDrop`)なので、名前は`TestCaches_ReloadedFromMeta`が見る
- `make cachelist-check` (#3777): 上の静的な検査。中身は[gates.md](gates.md#キャッシュの一覧への登録漏れcachelist-check3777)

#3721・#3723・#3725・#3727・#3729・#3732・#3736・#3738・#3776・#3777 の前後で同じであることは、それぞれ次のように確かめた (道具はコミットしていない)。

- `elythia dump-routes`: 出力 (ルート)、起動ログ、Redis の命令の列、PostgreSQL の文の集合は、どれも前後で
  一致した。前の木を 2 回流して揺れないことも確かめ、後の木も 2 回流した
- 4.1 の道具: #3729 までは、節点 356、後から入れる辺 96 (行番号を除けば前と同じ)、呼び出しが離れている辺
  659 で、前後で同じだった。#3732 では instance actor の秘密鍵を 1 つの変数 (`instanceSigner`) にまとめた
  ので、節点が 357、setter で依存を入れる辺が 872 から 870、呼び出しが離れている辺が 657 になった。後から
  入れる辺は 96 のまま (行番号を除けば前と同じ)。`instanceSigner` は受け手の `apFetcher` より先に作る。
  #3736 では、節点 357・setter の辺 870・後から入れる辺 96・呼び出しが離れている辺 657 のまま変わらない。
  #3738 では、ドライブの使用量の集計を変数 (`driveUsage`) にしたので、節点が 358 になった。setter の辺
  870・後から入れる辺 96・呼び出しが離れている辺 657 は変わらない。`SetDriveUsageProvider` の引数に
  現れる節点が `s.db` から `driveUsage` に替わっただけで、`driveUsage` は受け手の `adminHandler` より先に作る。
  #3776 では、peer のプラグイン一覧と twemoji のバッジを変数 (`pluginPeers`・`twemojiBadge`) にしたので、
  節点が 360 になった。setter の辺 870・後から入れる辺 96・呼び出しが離れている辺 657 は変わらない
- #3777: `elythia dump-routes`の出力(ルート)、起動ログ、Redisの命令の列、PostgreSQLの文の集合は、前後で
  一致した(前の木を2回、後の木を2回流した)。一覧への登録を2つと、テストが読むフィールドを1つ足した
  だけで、読み直しは`dropAll`の中でしか走らない。4.1の道具は流していない

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
| playwright | 299 spec を Elythia backend で（PR ごと、4 シャード）。TS backend は手動 | — |
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

マイグレーション（`migration/`、golang-migrate、現在 138 本）:

- TS Misskey の既存テーブルへは原則**追加のみ**。例外が 22 件あり、うち 16 件は Elythia が自分で作ったものの除去・初期化か upstream 追随 ([TS版からの移行](migration-from-ts.md#破壊的なマイグレーション))。Go 固有の追加列・テーブルは `IF NOT EXISTS`。
- drop-in テストで発見した補完列は専用マイグレーションで追加。
- down スクリプトは必須（data loss する場合は `-- data loss:` で明記する）。**ただしこれは今後の規約で、既存の down は守れていない** — 宣言があるのは 31 本だけで、宣言が無いまま `DROP TABLE` / `DROP COLUMN` する down が 51 本ある（[migration-from-ts.md](migration-from-ts.md#elythia-内での切り戻し)）。

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
