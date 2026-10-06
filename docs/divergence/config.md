# 純正 Misskey との差分: 設定・streaming・job queue

[差分カタログの目次](../divergence.md) にある分類と基準 (Elythia と追従している本家の版) は、このファイルにも当てはまる。

## 4. 設定ファイル (YAML) の独自キー

| キー | 用途 |
|---|---|
| `jobQueueDriver` | queue 実装選択。**`mkq` (BullMQ wire 互換) のみ**。未知値は起動時 error。legacy の `asynq` は #2985 で削除し、明示すると「削除済み」と分かる文面で起動エラーにする (黙って mkq へ倒すと、予約投稿の可否やレート上限の効き方まで変わったことに気付けない) |
| `jobQueueAutoScale` / `maxWorkers` / `minWorkers` / `maxWorkersGlobal` / `autoScaleCooldownSeconds` | AIMD auto-scale controller |
| `deliverJobKeepFailed` / `inboxJobKeepFailed` / `deliverJobKeepCompleted` / `inboxJobKeepCompleted` | queue bucket の retention 件数 |
| `nsfwDetectorUrl` / `nsfwDetectorAuthHeader` / `nsfwDetectorTimeout` | Elythia 独自の汎用 NSFW detector 契約 (`POST` 生バイト → `{"score": float64}`)。**upstream 2026.7.0 の公式 sensitive-detector (meta 駆動) が未設定のときの fallback** |
| `videoThumbnailGeneratorMode` | `post` (既定、multipart POST) / `get` (Misskey TS 仕様互換) |
| `mediaProxySecret` | mediaproxy URL の HMAC 署名鍵 |
| `disableEndpointRateLimits` | bench 用。有効時に起動 warn |
| `testMode` / `enablePprof` / `enableMetrics` | 破壊的 endpoint / pprof / Prometheus の有効化。いずれも起動時 warn |
| `enableTimelineCache` / `timelineCacheTtlSeconds` | TL 1 ページ目の viewer 別短 TTL cache (opt-in) |
| `db.maxOpenConns` ほか pool tuning / `redis*.poolSize` | Go 固有 |
| `db.extra.sslmode` / `db.extra.sslrootcert` | DB 接続の TLS を libpq の語彙で指定する。upstream は `db.extra` を node-postgres の pool へ素通しするので `ssl` の object に任意の TLS オプション (PEM の `ca` など) を書けるが、pgx は CA をファイルパスでしか受けないため別キーにした (docs/configuration.md「DB 接続の TLS」) |
| `redis*.path` | ioredis 互換の UDS alias。同じ config を TS/mk で共有する drop-in 切替のため |
| `bcryptCost` | account password のハッシュ強度 (既定 10、範囲 4-31)。upstream は全経路 cost 8 固定で設定不可 |
| `crossOriginOpenerPolicy` | `Cross-Origin-Opener-Policy` の値 (既定 `off`)。upstream はテスト専用の cross-origin-isolation モードでしか出さない |
| `MK_*` 環境変数オーバーライド | upstream に同等機構なし |

逆方向 (upstream にあって Elythia に無い): `threadPoolSize`、`logging.format` / `logging.level` / `logging.domains` / `logging.access` (2026.7.0 のログ基盤刷新分。`logging.sql.*` は Elythia にもある)、`sentryForBackend.disabledIntegrations`。

## 4-1. WebSocket streaming チャンネル

| チャンネル | 内容 |
|---|---|
| `notifications` | **Elythia 独自**。upstream の 18 チャンネルに無い (upstream は `main` に通知を流す)。通知だけを購読したいクライアント向け。**これに依存するクライアントは Misskey TS では動かない**ので、Misskey TS でも動かしたいクライアントは `main` を使うこと |
| `bubbleVersus` | **Elythia 独自** (#3230)。バブルゲームの対戦の招待と、その返事 (受けた・断った・取り消した) を本人へ届ける。`read:account` |
| `bubbleVersusMatch` | **Elythia 独自** (#3230)。対戦 1 つ分 (`matchId` を渡す)。**参加者しかつなげない** (観戦は無い)。準備・攻撃・盤面の要約・切断の申告を受け、開始・攻撃・終局を流す。終局の報告は記録が大きいので API (`bubble-game/versus/report`) で送る。`read:account` |

Elythia 独自の 3 つを足して Elythia は 21 チャンネル。

upstream の 18 チャンネルは**すべて実装済み**で、名前も upstream に揃えてある。
以下は wire 上のチャンネル名 (`connect` の `channel` に渡す値 = upstream の `chName`)。
**ソースのファイル名は kebab-case だが、チャンネル名は camelCase** なので取り違えないこと
(`chat-room.ts` の `chName` は `chatRoom`)。

```text
admin antenna channel chatRoom chatUser drive globalTimeline hashtag
homeTimeline hybridTimeline localTimeline main queueStats reversi
reversiGame roleTimeline serverStats userList
```

この一覧と上の表の合計が `internal/server` の `streamRegistry` 登録名と一致すること
は `TestDivergenceDoc_StreamChannelsMatchRegistry` が固定する。ただし固定できるのは
**Elythia 側だけ**で、「upstream は 18」「名前も upstream に揃えてある」の検証は入って
いない (`test-shards` は本家のソースを取得しない)。upstream が増減した場合は
本家の版を上げる PR で人が見る。

## 4-3. job queue の構成差分

**driver は mkq だけ (#2985)。** upstream は BullMQ 一択で driver という概念を持たない。
Elythia には一時期 asynq driver もあったが、既定を mkq にしてから 4 か月以上 asynq へ
戻す判断が要らなかったので削除した。**`jobQueueDriver: asynq` を書き残したままの設定は
起動しない** (`config.Load` を通る `elythia migrate` 等のサブコマンドも同じ) — 黙って mkq で起動すると
driver が入れ替わったことに気付けず、予約投稿の可否やレート上限の効き方まで変わる。
行を消すか `mkq` に直すと起動する。

**移行は片道。** asynq の未処理ジョブは Redis の `asynq:{<queue>}:*` に居り、mkq
(`bull:*`) からは見えないので、残したまま切り替えると未配送の AP activity と未処理の
inbox が黙って消える。**新ビルドは起動を拒むので後から捌けない** — 切り替え前に
**旧ビルドでそのキーが空になるまで回す**しかない。

upstream は用途ごとに **10 queue** に分けるが、Elythia は **8 queue** に集約している (`internal/queue/driver/mkqdriver` の `QueueNames`)。処理する仕事は同じで、束ね方だけが違う。

| upstream の queue | Elythia の実体 |
|---|---|
| `deliver` | `deliver` |
| `inbox` | `inbox` |
| `system` | `maintenance` (cron 群: chart tick/resync/clean, checkExpiredMutings, clean, cleanRemoteNotes, checkModeratorsActivity, instanceRefresh, retentionAggregate, chunkedUploadGc, orphanUserCleanup, orphanAttachmentCleanup) |
| `endedPollNotification` | **queue ではなく常駐 goroutine** (`corepoll.ExpiryWorker`、60 秒間隔) |
| `postScheduledNote` | `deliver` の `note:postScheduled` |
| `db` | `export` の `export` / `import` / `importCustomEmojis`、`deliver` の `maintenance:deleteAccount` |
| `relationship` | `relationship` |
| `userWebhookDeliver` | `webhook` の `webhook:user` |
| `systemWebhookDeliver` | `webhook` の `webhook:system` |
| `objectStorage` | `objectStorage` |
| — | `push` (Web Push 配信、upstream は system queue 内で処理) |

`objectStorage` は `deleteFile` / `cleanRemoteFiles` とも upstream と同じ job 構成 (#2325)。振り分けも upstream に揃えてあり、ローカル FS 保存 (`storedInternal=true`) の実体は同期削除、object storage 上の実体だけを queue に逃がす。`clean-remote-files` は「job 1 本が内部でバッチ削除を回す」形も upstream と同じで、リモートキャッシュの件数ぶん job を積んで Redis を圧迫することはない。ただし Elythia はリモートメディアをキャッシュしないので、**Elythia が作った行に対しては対象が 0 件になる** (対象は `isLink=false` のリモート行で、それを作る経路が無い)。**TS 製の DB を引き継いだときだけ対象がある** (§5.5)。job 構成を upstream に揃えてあるのは drop-in 復路のため。

`note:postScheduled` / `maintenance:deleteAccount` が task type の接頭辞と違う `deliver` に載っているのは意図的なもの。いずれも実行結果が連合配送につながるジョブで、worker 2 本の `maintenance` より 16 本の `deliver` の方が捌ける。task type と queue の対応は `internal/queue/routing_test.go` が表として固定しており、変えると落ちる (#2327)。

cron の多重実行防止は **job option ではなく mkq の job ID 設計**で担保している。mkq は発火 job に決定的な ID (`repeat:<scheduleID>:<nextMillis>`) を振り、`updateJobScheduler-12.lua` が `EXISTS` で重複を弾いて `duplicated` イベントを記録する。加えて `producerId == currentDelayedJobId` の判定で、直前の発火を処理した worker だけが次を積める。

そのため `Scheduler.Register` に渡す `WithUnique` / `WithMaxRetry` / `WithProcessIn` は mkq driver では drop されるが、**現状の呼び出し方では実害が無い** (#2405)。`WithMaxRetry` は Elythia の cron が全て 0 = リトライ無しを渡しており mkq の既定と同じ、`WithProcessIn` はどの cron も渡していない。この性質は `TestScheduler_RepeatedRegisterDoesNotDuplicate` で固定してある。

`relationship` は #2403 まで `deliver` に相乗りしていたが、専用 queue に分離した。大量 follow (アカウント移行 / import) が `deliver` の worker を占有して AP 配信そのものを詰まらせ、片方を絞るともう片方も絞られる状態だったため。worker 数は 4 で、upstream の 16 とは違う。relationship job は DB bound (following 行 + カウンタ + stream publish) で外向き HTTP は `deliver` へ再 enqueue されるだけなので、`db.maxOpenConns` (既定 25) を HTTP 経路と共有する以上 16 を割くと Web 側のテールレイテンシに響く。`relationshipJobConcurrency` / `relationshipJobPerSec` はこの分離で初めて実効を持つようになった (それ以前は config として読むだけの no-op)。

`objectStorage` の worker 数だけは upstream の 16 に対し Elythia は 4。実体削除は S3 への I/O 待ちが主で 1 worker あたりの効率が良く、一括削除の並列度を job 数で稼ぐ設計でもないため、`deliver` と同じ理由 (worker 数 ≒ Redis 接続数) で抑えている。

再試行は **Elythia の方が手厚い**。upstream は `attempts` を設定しないので単発試行で終わり、失敗した実体は failed job として残るだけで自動復旧しない。Elythia は指数バックオフ付きで 4 回まで再試行する (object storage の一時的な 5xx / タイムアウトは待てば回復するため)。queue 自体が使えないときは同期削除にフォールバックし、実体を取りこぼさない。

**管理画面のタブはこの構成に合わせて fork 側で書き換えている** (`misskey-js` の `queueTypes`、`2026.7.0-mk.8`)。upstream のタブは API 応答ではなくこの定数から生成されるため、書き換えないと Elythia に存在しないタブが常時ゼロ表示になり、実在する `push` / `export` / `webhook` / `maintenance` / `objectStorage` が画面から見えなくなる (#2323)。**Elythia の queue を増減したら fork の `queueTypes` も合わせること。**

**プラグイン専用の `plugin:<名前>` キュー (#2818) は意図的にこの規則の外**。名前が構成で変わるので静的な定数には載せられず、**タブとしては出ない**。概要タブのカード (`admin/queue/queues` = 実際に worker が見ているキュー) から辿る。一時停止・再開は API 側が接頭辞で受け付けるので動く。
