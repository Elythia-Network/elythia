# ActivityPub連合

## パッケージ構成

### `internal/activitypub/` (プロトコル層)

| ファイル | 責務 |
|---|---|
| `types.go` | ActivityStreams 2.0の型定義 (Object, Person, Note, Activity等) |
| `renderer.go` | Goモデル → AP JSON-LD変換 (RenderPerson, RenderNote, RenderFollow等) |
| `signature.go` | HTTP Signatures (Cavage draft v12) の署名・検証。`rsa-sha256` と `ed25519` |
| `client.go` | AP HTTPクライアント (署名付きPOST/GET、リダイレクト制御) |
| `jsonld.go` | `@context`構築、JSON-LD正規化 (Mastodonプレフィックス互換) |
| `keypair.go` | RSA 2048bit / Ed25519 鍵ペアの生成・PEM解析 |
| `multikey.go` | FEP-521a Multikey (`assertionMethod`) の組み立て・解釈 |
| `public_key_cache.go` | 公開鍵のインメモリキャッシュ |
| `webfinger.go` | WebFinger クライアント |
| `inbox_admission.go` | inbox で受け入れる activity の入口判定 |
| `ld/` | **LD-Signature** (RsaSignature2017) の署名・検証、canonicalize、hardening |
| `mfm/` | MFM(Misskey Flavored Markdown) → HTML変換 |

### `internal/core/federation/` (ビジネスロジック層)

| ファイル | 責務 |
|---|---|
| `resolver.go` | リモートアクター/ノートの取得・永続化。公開鍵の2層キャッシュ (メモリ+DB, TTL 24h) |
| `deliver_service.go` | 配信ジョブのエンキュー。フォロワーのInbox収集、ホストブロック判定 |
| `processor.go` | 受信Activityのディスパッチ (Follow, Create, Like, Announce, Delete, Update等) |
| `published_time.go` | AP `published` を parse + clock skew (5min) / 過去 10 年 floor で fallback (#940) |
| `remote_stats.go` | リモート user の notesCount/followersCount/followingCount を origin の `/api/users/show` から取得 (mk-go 独自拡張、#943)。LRU cache size cap 10000 (#945)、SSRF guard 経由 |
| `note_delivery_hook.go` | ノート公開時にCreate/Announceを配信 |
| `following_delivery_hook.go` | フォロー/アンフォロー/承認時にFollow/Undo/Acceptを配信 |
| `reaction_delivery_hook.go` | リアクション時にLikeを配信 |
| `note_delete_delivery_hook.go` | ノート削除時にDeleteを配信 |
| `mention_resolver.go` | メンション → AS Mentionタグ変換 |
| `fetcher.go` | リモートオブジェクト取得。**既定は instance actor による署名付き GET**で、401/403 のときだけ unsigned にフォールバックする (authorized-fetch 対応、#419) |
| `ld_signature_verifier.go` | inbound activity の LD-Signature 検証 |
| `activity_unwrap.go` | **1 要素の JSON 配列**になっている body を剥がす (Foundkey 系、#1185)。ネストした activity の actor 正規化は `processor.go` の `normalizeActor` |
| `pin_delivery_hook.go` | ピン留め時に Add/Remove を配信 (#2024) |
| `poll_delivery_hook.go` | 投票関連の配信 |
| `blocking_delivery_hook.go` / `user_moderation_delivery_hook.go` | ブロック・モデレーション操作の配信 |
| `profile_update_delivery_hook.go` | プロフィール更新時に Update(Person) を配信 |
| `chat_room_inbox.go` / `reversi_inbox.go` | cherrypick 由来 activity の受信 |
| `featured.go` | **リモート** actor の featured を取り込んで `user_note_pining` に反映 (upstream `ApPersonService.updateFeatured` 相当)。ローカルの featured を返すのは `internal/api/ap/handler.go` |
| `permanent_error.go` | リトライしない失敗の判定 |
| `suspended.go` | 配送先インスタンスの software 名 / 版で**送信をスキップ**するかの判定 (`meta.deliverSuspendedSoftware`) |
| `remote_user_resolver.go` | `acct:` からのリモートユーザー解決 |
| `image_dimensions.go` | 添付画像の寸法取得 |

## HTTP Signatures

Cavage draft v12に準拠。

署名アルゴリズムは `rsa-sha256` と `ed25519` の 2 種類。Ed25519 は配送先が
FEP-521a Multikey で公開している場合のみ使う (後述)。

**署名対象ヘッダー:**
- `(request-target)`: メソッド + パス
- `date`: RFC 1123 形式 (`http.TimeFormat`、GMT)
- `host`: リクエストホスト
- `digest`: リクエストボディのSHA-256 (POSTのみ)

```
Signature: keyId="https://example.com/users/abc#main-key",
           algorithm="rsa-sha256",
           headers="(request-target) date host digest",
           signature="base64..."
```

すべての送信リクエストに署名を付与する。受信時は`keyId`からアクターの公開鍵を取得して検証する。

### LD-Signature (RsaSignature2017)

HTTP Signature は**そのリクエストを投げた相手**しか認証しない。リレー経由の
配送のように**転送者と原著者が違う**経路では、body の `actor` が本物かを
HTTP Signature からは言えない。これを埋めるのが LD-Signature。

- 実装: `internal/activitypub/ld/` (`sign.go` / `verify.go` / `canonicalize.go` / `hardening.go`) と `internal/core/federation/ld_signature_verifier.go`
- **通常経路 (HTTP 署名者 == body の actor) では verify しない。** `@context` の
  term 再定義を弾く `CheckForForbiddenDirectives` だけを掛ける (#2106 N26)。
  upstream の `InboxProcessorService` もこの経路では LD-Signature を見ておらず、
  ここで verify すると creator 鍵の未解決や legacy LD-sig で**HTTP 署名済みの正規
  activity を落としてしまう**
- **verify するのは 2 つの経路だけ。** (1) HTTP 署名ヘッダの無い legacy /
  direct-enqueue 経路、(2) **HTTP 署名者と body の actor が食い違う転送経路**。
  後者は LD-Signature が body actor を認証している場合にのみ通す (actor spoofing 対策)
- **転送経路では upstream と同じく compact し、compact 後の文書を処理に渡す。**
  (1) の legacy 経路も同じく compact 後の文書を渡す (本番の inbox handler は必ず
  Headers を詰めるので到達しないが、検証済みとして扱う以上は生 body を渡さない)。
  `signature` を外して upstream の `CONTEXT` (`ld.InboxCompactContext`) へ compact し、
  forbidden directive の検査と RsaSignature2017 の検証もその文書に掛け
  (生の文書の検査はキー名しか見ないので、inline context で `"g": "@graph"` と
  別名を付けると素通りする。compact が `@graph` / `@included` に戻すのでそこで捕まる)、
  `authorizeActor` は actor / id をその文書で見直したうえで handler へ渡す。
  **生 body を処理してはいけない** — AS2 context の `"@vocab": "_:"` により、context で
  定義されていない語 (`_misskey_content` など) は URDNA2015 で blank node 述語として
  捨てられ署名に含まれないので、転送者が署名済み activity にそういうキーを足すと
  原著者名義の本文を差し替えられる (以前の mk-go がそうだった)。compact 後の文書では
  署名外の述語は `_:<name>` のキーで残り、handler からは見えない
- **`signature.created` が過去 7 日 / 未来 1 時間の外なら拒否する。欠落・複数・読めない
  値も拒否する** (upstream には無い。転送経路の replay 対策。divergence 登録済み)。
  **値は JSON の `created` キーではなく、署名された options の正規形 (n-quads) の
  `dc:created` から読む** (`ld.Processor.SignedCreated`)。options は identity/v1 で
  正規化されるので、`created` を `dc:created` (型付き) や完全 IRI に書き換えても RDF は
  同じで署名は通る。キーだけを見ると「欠落」と読まれて窓を外せた
- **preload 外の remote context は解決しない** (#2106 L49、divergence 登録済み)。
  `ld.PreloadedLoader` は HTTP fetch を一切行わない (AS2.0 / security v1 / identity v1 の
  3 つだけを resolve) ので、それ以外を参照する転送 activity は compact 段で
  `ErrCacheFrozen` になり拒否される。freeze は compact の前に置いてある (upstream は後)。
  **将来 fetch fallback を足すなら freeze を compact の後へ動かす必要がある**
- canonicalize は外部 URL を引きうる操作なので、SSRF / キャッシュ増幅 / spoofing 対策を
  `ld/hardening.go` と `ld/loader.go` に置いている

## リモートオブジェクト解決

`resolver.go`がリモートのアクターとノートを取得し、ローカルDBに永続化する。

**公開鍵キャッシュ:**
1. インメモリ (`map[userID]publicKeyEntry`, TTL 24h)
2. DB (`user_publickey`テーブル)
3. HTTPフェッチ (キャッシュミス時。`fetcher.go` 経由なので既定は署名付き GET)

リモートユーザー作成時にはインスタンス情報の登録とチャートメトリクスの記録も行う。

## 配信パイプライン

```
coreサービス (note/following/reaction)
  ↓ フック呼び出し
DeliveryHook (note_delivery_hook等)
  ↓ Activity構築 + エンキュー
DeliverService
  ↓ deliver queue へタスク投入
Redis (ジョブキュー)
  ↓
DeliverProcessor (queue ワーカー)
  ↓ 署名付きPOST
リモートInbox
```

**レスポンス処理:**
- 2xx: 成功
- 410/404: 永続的失敗 (対象が存在しない)、リトライしない
- その他の4xx: 永続的失敗、リトライしない
- 5xx / ネットワークエラー: リトライ (不調状態としてマーク)

### Ed25519署名 (capability-gated, #1067 / #1071)

配送先 actor が FEP-521a Multikey で Ed25519 を expose し、sender が `user_keypair_extra` に Ed25519 鍵を持つ場合のみ Ed25519 で sign を試行する。それ以外は従来通り RSA で sign。

**capability 判定の流れ:**

1. `DeliverToUser(signerUserID, recipient *model.User, body)` で recipient が既知
2. `recipientIsEd25519Capable(recipient)`: `user_publickey_extra` を `ListByUserID(recipient.ID)` で確認 (TTL 5min の in-memory cache あり、N+1 抑制)
3. capable で sender も Ed25519 鍵を持つ → `DeliverPayload.Ed25519KeyID` / `Ed25519PrivPEM` に詰めて enqueue
4. `DeliverProcessor` 側で Ed25519 sign を試行、4xx (410/404 除く) で失敗したら Redis に `ed25519:fail:{host}` を INCR
5. 60s window 内に 3 件以上失敗 → `ed25519:degrade:{host}` を EX=5min で立てる
6. degrade flag 立ち host への次回 deliver は最初から RSA で sign (= broken impl safety net)

`DeliverActivity` / `DeliverToFollowers` 経路 (recipient 不明) は RSA only で動く。

### Redis に格納される秘密情報

DeliverPayload は `KeyPEM` (RSA private key) および `Ed25519PrivPEM` (Ed25519 private key) を含んだ JSON としてジョブキュー (= Redis) に書き込まれる。これは upstream Misskey TS の BullMQ payload と同じ pattern だが、本番運用では以下が推奨される:

- **Redis 通信の TLS 化** (in-transit 暗号化)
- **Redis の persistence 暗号化** (encrypt-at-rest、Disk full encryption / Redis Enterprise の透過暗号化など)
- queue worker と Redis 間のネットワーク隔離 (private VPC / Unix socket)

特に Ed25519 / RSA いずれも HTTP Signature の identity を担う秘密情報のため、queue server を信頼境界の内側に置くこと。

## Inbox処理

`internal/api/inbox/handler.go`がShared InboxとユーザーInboxの両方を処理する。**verify-in-worker 化済 (#565)**: HTTP handler では body + signature header を payload として queue に詰めて 202 即返し、verify は inbox worker で行う。これにより HTTP 受信スループットが TS の 2.6-2.8x。

**HTTP handler フロー (同期、軽量):**
1. リクエストボディ読み取り
2. queue/processors の inbox 用 payload を作成 (body + signature header)
3. inbox queue へ enqueue
4. 202 Accepted 即返し

**inbox worker フロー (非同期、`internal/queue/processors/inbox.go`):**
1. payload から body / Signature header を復元
2. Signature 解析 → `keyId` からアクター解決
3. 公開鍵取得 (キャッシュ優先) → 署名検証
4. ホストブロックチェック
5. インスタンスメタデータ更新 (`coreinstance.TouchBuffer` で per-host 1s buffer 集約、#569)
6. チャートメトリクス記録
7. Processor にディスパッチ
8. fanoutHook / notificationHook を `safeGo` で async 発火 (#569)

未対応のActivity種別にも202 Acceptedを返す (エラーにしない)。

## 遅延配送 note の createdAt (#940)

origin instance の downtime / 遅延配送等で過去の note が遅れて inbox に到着した場合、AP Object の `published` field を parse して time-based ID (aidx) に渡すことで timeline 並びを origin と揃える。

- 受け入れ: RFC3339 / RFC3339Nano
- spoof guard: 未来側 `+5min` skew tolerance を超える値は受信時刻にフォールバック
- parse バグ guard: 過去 10 年以上前の値もフォールバック
- 該当 helper: `internal/core/federation/published_time.go` の `parseAPPublishedTime`

## AP variant handling

upstream / 他実装が出してくる variant に対するロバスト性:

| variant | 状態 |
|---|---|
| `published` parse + 異常値 fallback | 対応済 (#940) |
| `actor` が embedded object のケースを救済 | 対応済 (#999、upstream #17340)。`normalizeActor` が inner activity にも効く |
| `alsoKnownAs` の array / string 双方受け入れ | 対応済 (#1000、upstream #17275)。`APIDList` (単一 string / 配列に加えて `{"id": ...}` 要素も拾い、読めない要素は落とす) |
| 存在しない Actor の Delete を ignore | 対応済 (#1001、upstream #17294)。無いと 404 で queue retry に乗り続ける |
| リレー由来 Announce で renote を作らず元 note を直接 publish | 対応済 (#1002、upstream #17308) |
| ブロック中インスタンスの inbox job 蓄積防止 | **設計上不要** (#1003)。upstream は NoteCreate 深部の `Instance is blocked` を error handler で捕まえて retry を防ぐが、mk-go は verify-in-worker (#565) で signature verify 直後に block check するので body parse にすら届かない |

## RemoteStatsFetcher (mk-go 独自拡張、#943)

upstream Misskey TS の `users/show` は **自インスタンスで観測した範囲** のみで notesCount / followersCount / followingCount を集計するため、リモートユーザーの数値が実体より小さく表示される。mk-go は user.Host が non-local の場合、origin instance の `/api/users/show` を https POST で叩いて公開 counts を取得し、上書き表示する。

- LRU cache: size cap 10000 / positive TTL 1h / negative TTL 5min (#945)
- SSRF guard: `safehttp.NewSSRFSafeTransport` 経由
- host validation: URL injection (`/`, `?`, `#`, `@`, ` `) を url.Parse で reject
- 失敗時は silent fallback で local 観測値を維持
- フォロー一覧 / フォロワー一覧 endpoint は **自インスタンス上に存在する関係のみ** (= 数値だけ remote、一覧は local の非対称設計)

## レンダリング

`renderer.go`がGoモデルをAP JSON-LDに変換する。

**Person:**
- Type: `Person` (ボットは`Service`)
- アイコン/バナー、公開鍵、プロフィールフィールド (PropertyValue)
- カスタム絵文字タグ

**Note:**
- `content`: MFM → HTML変換
- `_misskey_content`: 元のMFM
- メンション、ファイル添付 (Document)、引用URL
- `sensitive`フラグ

## 対応済みActivity

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
| Flag (Report) | o | o |
| Move (Account Migration) | o | o |
| Add/Remove (Pin) | o | o |

送信側の実体: `Flag` は `core/abuse` の forwarder (`RenderFlag`)、`Move` は
`core/move` (`RenderMove`)、`Add`/`Remove` は pin の delivery hook
(`RenderAddToFeatured` / `RenderRemoveFromFeatured`、#2024)。

## ディスカバリ

| エンドポイント | 内容 |
|---|---|
| `GET /.well-known/webfinger` | `acct:user@host`のリソース検索 |
| `GET /.well-known/host-meta` | WebFinger URLテンプレート (XRD) |
| `GET /.well-known/host-meta.json` | 同上の JRD 版 |
| `GET /.well-known/nodeinfo` | NodeInfo URL |
| `GET /nodeinfo/2.0` | サーバーメタデータ (バージョン、プロトコル、利用統計) |
| `GET /nodeinfo/2.1` | 同上 (2.1 スキーマ) |
| `GET /users/:id` | AP Person |
| `GET /notes/:id` | AP Note |
| `GET /notes/:id/activity` | Create / Announce の activity。renderer が広告する `<note URI>/activity` の dereference 先 (ローカルノート専用) |
| `GET /users/:id/followers` | フォロワーコレクション |
| `GET /users/:id/following` | フォローコレクション |
| `GET /users/:id/outbox` | アウトボックスコレクション |
| `GET /users/:id/collections/featured` | ピン留めノートコレクション |
| `POST /inbox` | Shared Inbox |
| `POST /users/:id/inbox` | ユーザー Inbox |

**ピン留めは `/users/:id/featured` ではない。** upstream と同じく
`/users/:id/collections/featured` に登録されており、前者は AP ハンドラに
到達しない。

AP リソース系 (`/users/:id` / `/notes/:id` 等) は Accept ヘッダで content
negotiation する。ブラウザからのリロード (`Accept: text/html`) では SPA 用の
HTML を返すので、`Vary: Accept` が付く。
