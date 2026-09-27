# 設定リファレンス

## 設定ファイル

設定ファイルはMisskey互換のYAML形式。CLIフラグで指定する:

```bash
./built/misskey -config .config/default.yml
```

### 初回セットアップ

`.config/` 配下には Misskey 本家流儀で `.example` テンプレートを配布している。初回は以下のいずれかを手元の `.config/default.yml` (および `docker.yml`) として複製してから編集する:

```bash
# ローカル開発 / バイナリ直起動
cp .config/default.yml.example .config/default.yml

# Docker / docker-compose
cp .config/docker.yml.example .config/docker.yml
```

`.config/default.yml` と `.config/docker.yml` は `.gitignore` 対象 (operator-local) なので、コピーした手元のファイルが履歴に混入することはない。Docker image は build 時に `.config/docker.yml.example` をコンテナ内の `/app/.config/default.yml` として焼き込むので、運用環境では docker-compose 等で実 config を volume mount で上書きする想定。

## 全設定項目

### 基本設定

| キー | 型 | デフォルト | 説明 |
|---|---|---|---|
| `url` | string | (必須) | サーバーの公開URL (例: `https://misskey.example.com`) |
| `port` | int | - | HTTPポート。**既定値は無い** (`SetDefault` していないので未指定だと `:0` を listen し、OS が空きポートを割り当てる)。`socket` を使うときを除いて必ず指定する |
| `socket` | string | - | UNIXドメインソケットパス (設定するとTCPの代わりにUDSでリッスン) |
| `chmodSocket` | string | - | UDSファイルのパーミッション (例: `"770"`) |
| `id` | string | `"aidx"` | IDジェネレータ (`aidx`, `aid`, `meid`, `ulid`, `objectid`) |
| `setupPassword` | string | - | 初期セットアップ時のパスワード |
| `disableHsts` | bool | `false` | HSTSヘッダーを無効化。`url` が https のとき既定で `Strict-Transport-Security: max-age=15552000; preload` を付ける (upstream ServerService と同値・同条件)。http の構成では `disableHsts` に関わらず付けない — 平文で配っている host に付くと、ブラウザが以後 https でしか開かなくなり max-age の間は戻せない |
| `bcryptCost` | int | `10` | アカウントパスワードの bcrypt work factor (4-31)。upstream は全経路 8 固定で設定不可。**+1 ごとにハッシュ生成も検証も倍**になるので、ログインの待ち時間でもある (実測: 10=約50ms / 12=約185ms / 13=約370ms)。cost は `$2a$NN$` に埋まるので上げても drop-in で TS 側が検証できる。既存利用者のハッシュはログイン成功時に焼き直されるので、上げれば戻ってきた順に移行する。範囲外は警告して既定に落とす |
| `crossOriginOpenerPolicy` | string | `off` | `Cross-Origin-Opener-Policy` の値。`off` / `same-origin-allow-popups` / `same-origin`。upstream はテスト専用の cross-origin-isolation モードでしか出さない。**既定を off にしてあるのは、外部アプリが認証ページをポップアップで開いて閉じるのを待つ形の連携を切りうるため。** MiAuth / OAuth は callbackUrl で完結するので通常は問題にならないが、切れたときの症状から原因に辿り着きにくい。未知の値は警告して無視 |
| `noteHookConcurrency` | int | `GOMAXPROCS * 2` | 投稿後のベストエフォートフック (タイムライン配布 / 通知 / 連合 / チャンネル / 索引 / チャート / webhook) を種別ごとに何本まで同時に走らせるか。**絞らないと応答のテールが伸びる** — 投稿の集中で数百 goroutine が生まれ、応答を返す goroutine と実行機会を奪い合うため (2 CPU の実測で `notes/create` p95 が 105ms → 181ms)。枠は「タイムライン配布」と「その他」の 2 つに分けてあり、遅いフックが配布を止めて反映が遅れるのを防ぐ。0 以下は既定値扱い |
| `mediaProxyConcurrency` | int | `GOMAXPROCS / 2` (最低 1) | media proxy の画像処理 (decode/resize/encode) を同時に何本走らせてよいか (#3032)。**ピークメモリの上限**にあたる — 同時に走る本数がそのまま同時に確保される中間バッファの本数になり、内蔵プロキシは API サーバーと同じプロセスなのでそれがインスタンス全体の RSS になる。8 core で AVIF を c=8 流したときの peak RSS の実測は枠 1 → 131MB / 2 → 409MB / 4 → 636MB / 8 → 995MB / 無制限 → 959MB、同条件の jpg→avatar スループットは 30.7 / 45.5 / 63.9 / 71.2 / 71.6 rps。既定は peak RSS -34% をスループット -11% で買う点。枠が 5 秒空かなければ 503 (`?fallback` 指定時はダミー画像) で落とす。0 以下は既定値扱い |
| `enableIpRateLimit` | bool | `true` | IPベースのレート制限を有効化 |
| `disableEndpointRateLimits` | bool | `false` | per-endpoint rate limit table 全体を無効化。Misskey TS の `NODE_ENV=development` 相当で、ベンチマーク等で公正比較する用途専用。**本番で絶対に使わない** |
| `pidFile` | string | - | PIDファイルパス |
| `testMode` | bool | `false` | テスト用エンドポイント(/api/reset-db)を有効化。**本番で絶対に使わない** |
| `enablePprof` | bool | `false` | `/debug/pprof/*` ハンドラを公開。ローカルプロファイリング専用。**本番で絶対に使わない**。`MK_ENABLEPPROF`で上書き可。診断のためどうしても一時的に有効化する場合は、reverse proxy 側で`/debug`を落としてからにする (`deploy/uds/nginx/mkgo.conf`と[deployment.md](deployment.md)のnginx例はどちらも404にしてある)。取得はUDSへ直接 (`curl --unix-socket`)。 |
| `enableMetrics` | bool | `false` | Prometheus `/metrics` エンドポイントを公開。job queue 系 metric (`mk_job_workers_active` / `mk_job_workers_quarantined` / `mk_job_handlers_abandoned` / `mk_job_handler_abandonments_total` / `mk_job_queue_pending` / `mk_job_dispatch_wait_seconds` / `mk_job_processing_seconds` / `mk_job_scale_events_total` / `mk_job_scrape_errors_total`) を expose。認証無しで公開されるため、外部公開する場合は nginx / LB ACL で access 制限すること。詳細は `docs/design/auto-scale-job-workers.md` §6.1。`MK_ENABLEMETRICS`で上書き可。 |
| `jobQueueDriver` | string | `"mkq"` | ジョブキュー実装の選択。**指定できるのは `mkq` だけ** (BullMQ wire-compatible で admin queue 画面が Misskey TS frontend 前提のまま動く + per-queue concurrency / rate-limit が効く)。legacy の `asynq` は #2985 で削除済みで、明示すると**起動エラー**になる (黙って別の driver で起動しないため。行を消せば既定の `mkq`)。**移行は片道** — asynq の未処理ジョブは Redis の `asynq:{<queue>}:*` に居り mkq (`bull:*`) からは見えないので、切り替え前に**旧ビルドで捌ききる**こと (新ビルドは起動を拒むので後から捌けない)。`MK_JOBQUEUEDRIVER`で上書き可。 |
| `publishTarballInsteadOfProvideRepositoryUrl` | bool | `false` | **mk-go では効かない** (upstream YAML 互換のために読むだけ)。upstream は `built/tarball` を `/tarball/` に静的配信するが mk-go にはそのルートが無く、`/api/meta` の `providesTarball` は常に `false` を返す。有効にすると起動時に warn が出る。**404 で気付ける類ではない** — SPA の catchall が拾うので `misskey-<version>.tar.gz` という名前の HTML が 200 で返る。ソースの案内は `meta.repositoryUrl` (管理画面の 全般 → 情報) で行うこと。新規インスタンスは mk-go のリポジトリが既定で入る (#2700) |
| `effectivePolicyProviderCacheEntries` | int | `10000` | build-time pluginのeffective-policy providerごとに保持する成功結果LRUの最大件数。正の値だけを受理し、0以下は警告して既定へ戻す。TTLは無く、eviction時はresolverを再実行する。変更にはprocess再起動が必要。`MK_EFFECTIVEPOLICYPROVIDERCACHEENTRIES`で上書き可 |

### データベース (`db.*`)

| キー | 型 | デフォルト | 説明 |
|---|---|---|---|
| `db.host` | string | `"localhost"` | ホスト (`/`始まりならUNIXソケット) |
| `db.port` | int | `5432` | ポート |
| `db.db` | string | `"misskey"` | データベース名 |
| `db.user` | string | `"misskey"` | ユーザー名 |
| `db.pass` | string | - | パスワード |
| `db.disableCache` | bool | `false` | **no-op**。Misskey YAML 互換のために受け付けるだけで、どこからも読んでいない |
| `db.extra.ssl` | bool / map / `no-verify` | `false` | TLS接続。`true` は**サーバー証明書とホスト名を検証する** (`sslmode=verify-full`、システムの CA)。upstream (node-postgres) の形 `{ rejectUnauthorized: false }` と `'no-verify'` は検証しない TLS (`sslmode=require`)。詳細は下記 |
| `db.extra.sslmode` | string | - | **mk-go 独自**。libpq の `sslmode` を直接指定する (`disable` / `allow` / `prefer` / `require` / `verify-ca` / `verify-full`)。`db.extra.ssl` と同時には書けない |
| `db.extra.sslrootcert` | string | - | **mk-go 独自**。検証に使う CA 証明書 (PEM) の**ファイルパス**。自己署名 / 私設 CA の DB に使う |

`dbReplications: true`とすると`dbSlaves`設定でリードレプリカを使用可能。レプリカは primary の `db.extra` の TLS 設定を引き継ぐ (ホスト名の検証はレプリカ自身の `host` で行う)。

#### DB 接続の TLS (`db.extra`)

| 書き方 | 意味 | pgx の `sslmode` |
|---|---|---|
| 未設定 / `ssl: false` | 平文 | `disable` |
| `ssl: true` | TLS + 証明書とホスト名の検証 (システムの CA) | `verify-full` |
| `ssl: true` + `sslrootcert: /path/ca.pem` | TLS + 指定した CA で検証 | `verify-full` |
| `ssl: { rejectUnauthorized: false }` / `ssl: no-verify` | TLS だが**検証しない** (盗聴・なりすましに無防備) | `require` |
| `sslmode: <値>` (+ `sslrootcert`) | libpq の意味そのまま (`prefer` / `allow` と `sslrootcert` の併記は起動エラー) | 指定値 |

- **`ssl: true` の意味が変わった (破壊的変更)。** 以前の mk-go は `ssl: 'true'` (文字列) のときだけ `sslmode=require` = **証明書を検証しない TLS** で繋いでいた。しかも YAML の bool の `ssl: true` は設定の読み込みで `"1"` になって比較に外れ、**平文で繋いでいた**。upstream (node-postgres) の `ssl: true` は Node の既定 (`rejectUnauthorized: true`) で検証するので、それに揃えた。自己署名証明書の DB に `ssl: true` で繋いでいた構成は起動時に `certificate signed by unknown authority` 等で落ちる。その場合は (a) CA を `db.extra.sslrootcert` に指定する、または (b) 検証を明示的に無効にする (`ssl: { rejectUnauthorized: false }`) のどちらかを選ぶ。起動エラーにも同じ案内を出す。
- **upstream の object 形式で解釈するのは `rejectUnauthorized` だけ。** node-postgres の `ca` は PEM の中身を受けるが、pgx はファイルパスしか受けないので `ca` は起動エラーにする (CA をファイルに保存して `sslrootcert` へ)。`cert` / `key` / `servername` 等も黙って捨てずに起動エラーにする。`ssl` 以外の `extra` のキー (`statement_timeout` 等、upstream が pool へ渡すもの) は無視する。
- **`ssl: no-verify` は `{ rejectUnauthorized: false }` と同じ。** node-postgres (upstream が使う pg 8.23.0 の `lib/connection-parameters.js`) が同じ意味で受ける書き方なので、そのまま移せる。
- 矛盾する組み合わせ (`ssl` と `sslmode` の併記、TLS 無効なのに `sslrootcert`、`rejectUnauthorized: false` / `no-verify` と `sslrootcert` の併記、`sslmode: prefer` / `allow` と `sslrootcert` の併記) は起動エラーにする。最後のものは、pgx が `prefer` / `allow` では CA を渡しても**証明書を検証せず**、TLS を断られると**平文へ落ちる**ため (CA を書いた運営者の意図と逆になる)。検証したいなら `verify-full` (または `verify-ca`) にする。
- 証明書の検証に失敗したときの起動エラーには、書いた設定に合わせた対処を添える (`ssl: true` なら「以前は検証していなかった」旨と `sslrootcert` / `rejectUnauthorized: false`、`sslmode` を明示していれば `sslrootcert` / `sslmode: require`、`sslrootcert` を指定していればその CA ファイルと `db.host` の確認)。
- **環境変数で渡すなら設定ファイルにキーを書いておく。** `db.extra.*` は `bindEnvKeys()` に登録していないので、`MK_DB_EXTRA_SSL` / `MK_DB_EXTRA_SSLMODE` / `MK_DB_EXTRA_SSLROOTCERT` は**設定ファイルに同じキー (`extra:` の下の `ssl:` 等) があるときだけ**効く (下の「環境変数オーバーライド」を参照)。ファイルに無いまま export しても黙って平文で繋ぐ。
- **UNIX ソケット (`host` が `/` 始まり) では TLS を張らない** (`sslmode=disable` 固定)。
- `cmd/migrate` / `misskey doctor` / backfill 系の CLI も本体と同じ接続設定 (TLS・パスワードのエスケープ) を使う。

### Redis (`redis.*`)

基本のRedis設定。全フィールドは各用途別Redis設定にも適用可能。

| キー | 型 | デフォルト | 説明 |
|---|---|---|---|
| `redis.host` | string | `"localhost"` | ホスト |
| `redis.port` | int | `6379` | ポート |
| `redis.pass` | string | - | パスワード |
| `redis.db` | int | `0` | データベース番号 |
| `redis.username` | string | - | ユーザー名 (Redis 6+ ACL) |
| `redis.prefix` | string | - | キープレフィックス |

### 用途別Redis

設定しない場合は`redis`にフォールバック。

| キー | 用途 |
|---|---|
| `redisForPubsub.*` | PubSub (イベント配信) |
| `redisForJobQueue.*` | ジョブキュー (mkq が使う) |
| `redisForTimelines.*` | タイムラインキャッシュ |
| `redisForReactions.*` | リアクションバッファ |

### ジョブキュー制御

| キー | 型 | デフォルト | 説明 |
|---|---|---|---|
| `deliverJobConcurrency` | int | `16` | AP配信worker数。deliver queue 専用の worker 数 |
| `inboxJobConcurrency` | int | `16` | Inbox処理 worker 数 (#534 で非同期化済)。inbox queue 専用 worker (未指定時の default は 16) |
| `frontendContentSecurityPolicy` | string | `off` | SPA shell (frontend HTML) と `/embed/` に付ける CSP。`off` / `report-only` / `enforce` (#2425)。**upstream Misskey には無い mk-go 独自の hardening** なので opt-in。`report-only` は何もブロックせず違反を報告するだけだが、違反があると全利用者のブラウザ console に出る。`off` 以外にすると `POST /csp-report` が生え、ブラウザからの違反報告を INFO ログに落とす。**公開インスタンスで有効にする場合は、前段の nginx / CDN で `/csp-report` にレート制限を掛けること** (認証不要の POST なので、無いとログ増幅に使える。mk-go 側は body 64KiB 上限のみ)。`useObjectStorage` が有効なら `objectStorageBaseUrl` の origin を、外部 `mediaProxy` 構成ならその origin を `img-src` / `media-src` / `connect-src` に自動で加える (drive のファイル配信と通知音の fetch が別オリジンになるため、#2501)。有効な captcha 業者 (hCaptcha / reCAPTCHA / Turnstile) の origin も `script-src` (hCaptcha は公式要求に従い `connect-src` / `style-src` も) に自動で加える (#2502。mcaptcha は iframe + バンドル済み glue のため不要)。**captcha の origin は SPA shell だけ** — `/embed/` はサインアップ経路を持たないので足さない (#2789)。あわせて **`img-src` には固定で 2 つの host が入る** (`avatars.githubusercontent.com` / `assets.misskey-hub.net`)。upstream の `/about-misskey` が謝辞のアイコン 62 枚をそこから直接読むためで、許さないとそのページに壊れた画像が並ぶ (#2892)。設定では消せない。captcha と同じ理由で `/embed/` には足さない |
| `queueIdlePollSeconds` | int | mkq 既定 | ジョブが無いとき worker が marker key を待つ秒数の**下限**。mkq v1.0.4 以降、空振りのたびに待ちが倍になり 30 秒で頭打ちになる (ジョブを処理すると下限に戻る) ので、これは初回の待ちにあたる。アイドル時の Redis 負荷は放っておいても下がるため、**通常この値を設定する理由は無い**。**ジョブ取得は遅くならない** — Lua が marker を push するので worker はミリ秒で起きる (interval 30 秒でも取得 18.9ms を実測)。**停止も遅くならない** — Stop が marker を突いて待機中の worker を起こすので、interval に関わらずミリ秒台 (mkq v1.0.4 以前は発行済みの BZPOPMIN を中断できず、停止に最大 interval かかっていた) |
| `queueStuckWorkerSeconds` | int | キューごと | 1 件の job の handler が何秒戻ってこなければ、その worker を**キューの worker 数に数えない**扱いにするか。worker は 1 本が 1 dispatcher goroutine なので、handler から戻らなくなるとその分だけキューの処理能力が黙って減る (#2657 の本番障害では inbox の 4 本すべてが AP handler の中で 1 日以上戻らなかった)。該当 worker は**停止せず**脇に退ける + 代わりを立てるので、単に遅かっただけの job はそのまま完走し、完走後に pool へ戻る。**退けている間は実効の並列度が上がる** (上限は「到達した最大 worker 数 + max(設定値, 4)」。この枠は `queueHandlerDeadlineSeconds` で放棄した handler と共用で、達するとそのキューの worker を 0 まで縮めて止める。autoscale 無効なら設定値 + max(設定値, 4)、有効なら max(設定値, `maxWorkers`) + max(設定値, 4))。未設定はキューごとの既定 = deliver / inbox / relationship / push / webhook は 30 分、export / objectStorage / maintenance は**追跡しない** (分単位のページング処理が正常なため)。**既定が長いのは意図的**で、狙いは「恒久的に戻ってこない worker の回収」(#2657 は 1 日以上戻らなかった) であって遅い job の検出ではない。短くすると、正当に遅い job のたびに worker を差し替えて実効の並列度が設定値を超えた状態が定常化する。正値は全キューに適用、負値で機能ごと無効 |
| `queueHandlerDeadlineSeconds` | int | `3600` | 1 件の job の handler を何秒待ってから諦めて worker を返すか。**超過しても handler は止まらない** — Go では goroutine を殺せないので、止まるのは待つ側だけ。放棄した handler は DB 接続などを掴んだまま走り続け、job は失敗して retry に回るので、放棄した実行と retry が重なりうる。それでも 1 回の詰まりで dispatcher を永久に失う (#2657 は inbox の 4 本すべてを失った) よりは軽い。**放棄した handler は `queueStuckWorkerSeconds` の隔離と同じ枠を食い**、枠に達するとそのキューの worker を 0 まで縮めて止める (goroutine と DB 接続を食い潰してプロセス全体を巻き込むより、キュー 1 本を止めるほうがましという判断)。この枠は隔離の有無に関わらず効くので、隔離対象外の maintenance / export / objectStorage でも同じ。`queueStuckWorkerSeconds` より後に発火するのは意図的で、あちらが先に capacity を戻し、こちらが後から worker 本体を回収する。1 job が分単位でページングするのが正常な task type (export / import / objectStorage:cleanRemoteFiles / maintenance:deleteAccount / maintenance:cleanRemoteNotes 等) は**対象外**でこの値を無視する (期限を切ると job が失敗して retry に回り永久に完了しない)。負値で機能ごと無効 |
| `relationshipJobConcurrency` | int | `4` | follow / unfollow / block / unblock の worker 数。relationship queue 専用 worker (#2403)。既定を upstream の 16 でなく 4 にしているのは、relationship job が DB bound で `db.maxOpenConns` (既定 25) を HTTP 経路と共有するため |
| `deliverJobPerSec` | int | - | AP配信レート上限 (tasks/sec)。設定すると `mkq.WithRateLimit` で worker dispatch が back-pressure される |
| `inboxJobPerSec` | int | - | Inbox処理レート上限 (tasks/sec) (#534)。設定すると `mkq.WithRateLimit` で worker dispatch が back-pressure される |
| `relationshipJobPerSec` | int | - | relationship 処理のレート上限 (tasks/sec)。設定すると `mkq.WithRateLimit` で worker dispatch が back-pressure される (#2403) |
| `deliverJobMaxAttempts` | int | driver 既定 | AP配信の**総試行回数** (初回 + retry) のdefault。BullMQ `attempts` と同じ意味で、TS Misskey YAML 互換。EnqueueDeliver で `WithMaxRetry` 未指定時にだけ適用される (#495)。例: `deliverJobMaxAttempts: 8` で 1 回失敗するごとに retry されて最大 8 回試行 |
| `inboxJobMaxAttempts` | int | driver 既定 | Inbox処理の**総試行回数** (#534)。BullMQ `attempts` と同じ意味で、TS Misskey YAML 互換。EnqueueInbox で `WithMaxRetry` 未指定時にだけ適用される |
| `jobQueueAutoScale` | bool | `false` | AIMD auto-scale controller (#1120) を opt-in 有効化。`true` で個別 `<queue>JobConcurrency` 未設定の queue が `[minWorkers, maxWorkers]` 範囲で動的に伸縮する。詳細は `docs/design/auto-scale-job-workers.md`。 |
| `maxWorkers` | int | `runtime.NumCPU() × 16` | auto-scale の **per-queue 上限**。`jobQueueAutoScale: true` のときだけ意味を持つ。例: 8-core で default 128 per queue。 |
| `minWorkers` | int | `4` | auto-scale の **per-queue 下限**。idle 状態でも各 queue で常時起動する worker 数。 |
| `maxWorkersGlobal` | int | (unset) | 全 auto-scale 対象 queue 合計の worker 数 hard cap。**未設定は無制限** (= 各 queue が独立に maxWorkers まで膨張)。multi-pod 運用で DB pool / Redis pool を共有する場合のみ明示設定 (例: 3 pod、DB pool=240 → per-pod `maxWorkersGlobal: 64`)。 |
| `autoScaleCooldownSeconds` | int | `1` | scale event 発火後の連続発火抑止間隔。通常は default 1 秒で OK。 |

> **worker 数の扱い**:
> - `mkq` driver は queue ごとに worker を分けているので `deliverJobConcurrency` / `inboxJobConcurrency` / `relationshipJobConcurrency` はそれぞれの queue 専用 worker 数として扱われる。明示指定の無い queue は hot queue 優先の per-queue 既定値 (inbox=16 / deliver=16 / relationship=4 / objectStorage=4 / webhook=4 / push=4 / export=2 / maintenance=2) を使う。旧来の `総budget / len(queues)` 均等割りだと inbox が starve していた (#1374) ため hot-tuned default に変更。worker 数 ≒ Redis 接続数 (worker 毎に BZPopMin で接続を保持) なので、`redisForJobQueue.poolSize` 未設定時は mkq driver が worker 総数 + 隔離許容ぶん (`queueStuckWorkerSeconds` 参照) + 余裕に自動サイジングする (`<queue>JobConcurrency` を上げると pool も追従)。明示設定した場合はその値が尊重される。**自動サイジングが効くのは `poolSize` 未指定のときだけで、`jobQueueAutoScale` による増加は見込んでいない。** autoscale を有効にするなら、追跡対象キューでは worker が最大 `max(<queue>JobConcurrency, maxWorkers) + max(<queue>JobConcurrency, 4)` 本走りうる (`queueStuckWorkerSeconds` 参照) ので、おおむね `maxWorkers` の 2 倍を賄える値を明示すること (隔離の許容幅は autoscale ではなく**設定値**に紐づくので、`maxWorkers + max(<queue>JobConcurrency, 4)` が正確な上限)。
>
> **rate limit (`*JobPerSec`) の挙動**:
> - `mkq.WithRateLimit` で **worker pull レイヤ** に制御が入るため、レート制限が他 queue の処理を阻害しない。
> - リミッタの実体は Redis キーなので**プロセスを跨いで効く**。queue プロセスを複数立てても合計は設定値のまま。
>
> **`mkq` driver の rate limit は per-queue**:
> - mk-go の `mkq` driver は queue ごとに **N 個の `mkq.Worker`** を起動する pool-of-Workers 構造で運用される (auto-scale (#1120) と queue 単位 dynamic Resize を実現するため)。
> - それでも **`mkq.WithRateLimit` は queue 単位で効く**。リミッタの実体は BullMQ 互換の
>   `bull:<queue>:limiter` という **queue ごとに 1 本の Redis キー**で、pool 内の全 Worker が
>   同じキーを INCR する。Worker ごとの独立したバケットではない。
> - したがって `deliverJobPerSec: 128` は **worker 数によらず約 128 jobs/sec**。
>   `deliverJobConcurrency` で割る必要は無い。
> - **以前ここには「per-Worker なので合計は N × rl。目標値を worker 数で割って設定せよ」と
>   書いてあったが誤り** (#2640)。そのとおりにすると狙った値の 1/N しか出ない。
>   `TestServer_RateLimit_IsPerQueueNotPerWorker` が worker 1 と 16 で実効レートが
>   変わらないことを固定している (実測 2.02s / 2.01s)。

### メディア

ローカルストレージ (S3未設定時) のファイル保存先は`./drive-files`固定。Docker環境ではコンテナ内の`/app/drive-files`にボリュームマウントが必要。

| キー | 型 | デフォルト | 説明 |
|---|---|---|---|
| `maxFileSize` | int | `262144000` (250MB) | アップロードファイルの最大サイズ (バイト) |
| `mediaProxy` | string | - | メディアプロキシURL |
| `mediaProxySecret` | string | - | メディアプロキシの署名シークレット |
| `videoThumbnailGenerator` | string | - | 動画サムネイル生成サービスURL |
| `videoThumbnailGeneratorMode` | string | `post` | 生成サービスの呼び出し方式。`post` (multipart POST、SSRF surface を generator に持ち込まない) / `get` (Misskey TS の GET `?url=` 仕様互換) |
| `nsfwDetectorUrl` | string | - | mk-go 独自の汎用 NSFW 判定 service endpoint。`POST` に画像/動画 bytes、応答は `{"score": float64}`。**upstream 2026.7.0 の公式 sensitive-detector (meta の `sensitiveMediaDetectionApiUrl`) が未設定のときの fallback** として動く。空なら手動 isSensitive フラグのみ |
| `nsfwDetectorAuthHeader` | string | - | 上記 detector へ付与する 1 行 HTTP header (例 `Authorization: Bearer xxx`)。SaaS detector を thin proxy 無しで直接呼ぶ用 |
| `nsfwDetectorTimeout` | duration | `30s` | 上記 detector 1 リクエストあたりの timeout |

### ネットワーク

| キー | 型 | デフォルト | 説明 |
|---|---|---|---|
| `proxy` | string | - | 外向き HTTP のプロキシ URL (PR #485)。`proxyBypassHosts` 以外の宛先は proxy に渡す前に mk-go 側で検査する (リテラル IP はそのまま、ホスト名は手元で解決した全アドレス。1 つでも private / 予約済みなら拒否し、手元で解決できない宛先も渡さない。Unicode の IDN は接続時と同じ punycode の名前で解決する。`0x7f.1` / `2130706433` / `127.1` のような厳密な dotted-quad でない数値表記は渡さない)。通過した宛先は 30 秒間覚えて、その間はリクエストごとの名前解決を省く (拒否した宛先は覚えない)。**ただし proxy は宛先を自分で再解決するので DNS rebinding の窓は残る。proxy 側でも private / loopback / link-local (`169.254.169.254` など) 宛てを拒否する設定にすること** |
| `proxySmtp` | string | - | SMTP 配送のプロキシ URL。`http://host:port` (HTTP CONNECT)、`https://host:port`、`socks5://[user:pass@]host:port` (#496) |
| `proxyBypassHosts` | []string | - | プロキシを迂回するホスト (HTTP のみ)。リクエストの host を小文字化 + punycode にした形との完全一致で照合する (一覧側も同じ形に揃えるので、Unicode や大文字で書いてもよい) |
| `allowedPrivateNetworks` | []string | - | プライベート IP / loopback / metadata service へのアウトバウンド接続を許可する CIDR allowlist。AP fetch / URL preview / mediaproxy / `RemoteStatsFetcher` (#943) で共通に効く。`proxy` 設定時も同じ判定を proxy へ渡す前の宛先検査に使うので、ここに入れた範囲は proxy 経由でも許可される (proxy 自体への接続はこの設定と無関係に常に許可)。開発時の self-loop 用途 (`127.0.0.0/8` 等)、本番では空のまま運用する |
| `outgoingAddress` | string | - | 外向き HTTP の送信元 IP として bind するアドレス。複数 NIC 環境で federation 配信の source IP を固定する用途 (#496)。不正値は警告のみで kernel auto-pick に fallback |
| `trustProxy` | []string / string / `false` | private + loopback (下記) | `X-Forwarded-For` を信頼する前段 proxy。詳細は下記 |
| `outgoingAddressFamily` | string | `dual` | DNS 解決後の IP family 制限。`"ipv4"` / `"ipv6"` 指定で該当 family のみで dial、`"dual"` または空で両方 (#496) |

#### 前段 proxy の信頼 (`trustProxy`)

クライアント IP (rate limit・サインイン履歴・IP 履歴に使う) は `X-Forwarded-For` を右から左へ見て、**`trustProxy` に含まれない最初のアドレス**を採る。接続元が `trustProxy` に含まれなければ `X-Forwarded-For` は見ない。

| 書き方 | 意味 |
|---|---|
| 未設定 | 既定値 `10.0.0.0/8` / `172.16.0.0/12` / `192.168.0.0/16` / `127.0.0.1/32` / `::1/128` / `fc00::/7` (upstream と同じ) |
| CIDR / 素の IP のリスト | 書いたものだけを信頼する。素の IP は `/32` (`/128`) として扱う。`loopback` / `linklocal` / `uniquelocal` (upstream の proxy-addr の名前) も書ける |
| カンマ区切りの文字列 | リストと同じ (`MK_TRUSTPROXY=10.0.0.0/8,192.0.2.1` のように環境変数で渡すとき) |
| `false` / `[]` / `''` | どの proxy も信頼しない (`X-Forwarded-For` を無視して接続元アドレスを使う)。**UNIX ソケットで待ち受けているときは例外**で、接続元アドレスが無いので `X-Forwarded-For` (無ければ `X-Real-IP`) を読む (下記) |
| `true` / 数値 | **起動エラー** |

- **書いた範囲だけを信頼する。** 以前は Echo の既定で loopback / link-local / private が常に信頼されていたので、CDN の範囲だけを書いても private アドレスからの `X-Forwarded-For` が信頼され続けた。既定値 (未設定) のときの挙動は、`127.0.0.2`-`127.255.255.255` と link-local (`169.254.0.0/16` / `fe80::/10`) を信頼しなくなった点を除いて変わらない (どちらも upstream の既定にも入っていない)。必要なら `loopback` / `linklocal` を足す。
- **アップグレード時の注意: `trustProxy` を明示している構成は、前段 proxy のアドレスが入っているか確かめる。** 以前は上記の理由で、`trustProxy` に CDN の範囲だけを書き、nginx は同じホスト (`127.0.0.1`) や Docker の bridge network (`172.16.0.0/12` 等) から繋ぐ構成が**たまたま動いていた**。今はその nginx を信頼しないので、**全利用者のクライアント IP が nginx のアドレスになる** — rate limit のバケットを全員で共有して 429 が多発し、サインイン履歴と IP 履歴 (`user_ip`) が 1 アドレスに集まる。nginx のアドレス、または `uniquelocal` (private の範囲) / `loopback` を `trustProxy` に足す (例: `trustProxy: ['203.0.113.0/24', 'uniquelocal', 'loopback']`)。未設定のまま使っている構成は既定値に private と loopback が入っているので影響しない。
- **気付けるように 2 か所で警告を出す。** (a) 起動時に、`trustProxy` が loopback / private / unique-local のどの範囲も含まなければ (`false` / `[]` を含む) `trustProxy trusts no loopback or private address` を Warn で出す。前段を置かずに直接公開している構成ではそれが正しい設定なので、起動は止めない。UNIX ソケットで待ち受けているときは出さない (下記の理由でこの問題が起きない)。(b) 実行中に、信頼していない loopback / private アドレスから `X-Forwarded-For` 付きのリクエストが届いたら、`received X-Forwarded-For from a loopback or private address that trustProxy does not trust` を**プロセスごとに 1 回だけ** Warn で出す (`remoteAddr` にそのアドレスが載る)。(a) は設定だけを見るので、private の範囲を書いていても実際の proxy がその外にいる構成 (別の bridge network 等) は (b) でしか気付けない。
- **解釈できない値は起動エラーにする。** 以前は警告して捨てていたため、全部捨てると `X-Forwarded-For` の**最左を無条件に信頼する** Echo の既定に落ち、誰でも IP を詐称できた (`trustProxy: true` や `proxy.internal` のようなホスト名で実際にそうなる)。
- **`trustProxy: true` は受け付けない。** upstream (Fastify) では「全ての hop を信頼する」意味で、前段を経由せずに届いたリクエストが IP を自由に名乗れる。前段 proxy のアドレスを列挙すること。hop 数 (数値) も同じ理由で受け付けない。
- **`[]` は「信頼しない」。** 以前の mk-go は空リストを既定値として扱っていたが、upstream (`config.trustProxy ?? 既定`) では空リストがそのまま効くので揃えた。
- UNIX ソケットで待ち受けている場合は接続元アドレスが無いので、`X-Forwarded-For` を右から左へ見て `trustProxy` に含まれない最初の (解析できる) アドレスを採る。`X-Forwarded-For` が無ければ `X-Real-IP` を使う (接続できるのは同じホストの proxy だけという前提)。**`false` / `[]` でもこれらのヘッダは読む** — その場合は `X-Forwarded-For` の右端 (前段 proxy が追記した接続元) がそのまま採られる。

### 検索

| キー | 型 | 説明 |
|---|---|---|
| `fulltextSearch.provider` | string | 検索プロバイダ名。`sqlLike` (デフォルト) / `sqlPgroonga` / `meilisearch`。**pg_bigm を使う場合も `sqlLike` のまま** — クエリ形が `lower(text) LIKE` (upstream と同じ) なので、拡張と GIN インデックスを作るだけで index が効く (#2514、手順は [deployment.md](deployment.md)) |
| `meilisearch.host` | string | Meilisearchホスト |
| `meilisearch.port` | int | Meilisearchポート |
| `meilisearch.apiKey` | string | APIキー |
| `meilisearch.ssl` | bool | SSL接続 |
| `meilisearch.index` | string | インデックス名 |
| `meilisearch.scope` | string | 検索スコープ |

provider 別の挙動:

- `sqlLike` (デフォルト) — `lower(text) LIKE '%q%'` で部分一致検索する。**ILIKE ではない** — pg_bigm の GIN index (`gin (lower(text) gin_bigm_ops)`) は LIKE しか加速せず、ILIKE だと拡張を入れても index が効かないため。追加の DB 拡張は不要だが、index が無ければ note 行数に対して線形スキャンになる。
- `sqlPgroonga` — PGroonga の `&@~` 演算子で全文検索する。日本語を含む高速な転置 index 検索が利用可能。**事前に PostgreSQL 側で pgroonga 拡張のインストールと note.text への index 作成が必要** (下記)。
- `meilisearch` — 別プロセスの Meilisearch にノートを index する。`meilisearch.host` 必須。未設定時は自動的に `sqlLike` にフォールバックする。

#### PGroonga セットアップ

`sqlPgroonga` を使うには PostgreSQL に PGroonga 拡張が導入されている必要がある (公式 image には含まれていない)。インストール手順は [pgroonga.github.io](https://pgroonga.github.io/install/) を参照。導入後、各データベースで一度だけ拡張を有効化し、note.text に index を貼る:

```sql
-- DB ごとに 1 回だけ
CREATE EXTENSION IF NOT EXISTS pgroonga;

-- note.text 用の全文検索 index
CREATE INDEX IF NOT EXISTS pgroonga_note_text_idx ON note USING pgroonga (text);
```

mk-go 側のマイグレーションには含めていない。pgroonga 拡張の有無は運用環境依存のため、operator が明示的に install/index 作成する責務とする (upstream Misskey TS と同じ方針)。

### パフォーマンス

| キー | 型 | デフォルト | 説明 |
|---|---|---|---|
| `deactivateAntennaThreshold` | int | `604800000` (7 日) | この期間アクセスの無いアンテナを非活性化する閾値 (ミリ秒)。`clean` ジョブが使う |

以下の 2 つは **no-op**。Misskey YAML 互換のために受け付けて既定値も持つが、
`internal/config` の外から読まれていない。

| キー | 型 | デフォルト |
|---|---|---|
| `perChannelMaxNoteCacheCount` | int | `1000` |
| `perUserNotificationsMaxCount` | int | `500` |

### ロギング

| キー | 型 | 説明 |
|---|---|---|
| `logging.sql.disableQueryTruncation` | bool | SQLログのクエリ切り詰めを無効化 |
| `logging.sql.enableQueryParamLogging` | bool | SQLログにパラメータ値を含める |

## 環境変数オーバーライド

`MK_`プレフィックス付きの環境変数で設定値を上書きできる。ネストキーは`_`区切り。

| 環境変数 | 対応YAMLキー |
|---|---|
| `MK_URL` | `url` |
| `MK_PORT` | `port` |
| `MK_SOCKET` | `socket` |
| `MK_DB_HOST` | `db.host` |
| `MK_DB_PORT` | `db.port` |
| `MK_DB_DB` | `db.db` |
| `MK_DB_USER` | `db.user` |
| `MK_DB_PASS` | `db.pass` |
| `MK_REDIS_HOST` | `redis.host` |
| `MK_REDIS_PORT` | `redis.port` |
| `MK_REDIS_PASS` | `redis.pass` |
| `MK_REDIS_DB` | `redis.db` |
| `MK_REDIS_USERNAME` | `redis.username` |
| `MK_ID` | `id` |
| `MK_MAXFILESIZE` | `maxFileSize` |
| `MK_MEDIAPROXYSECRET` | `mediaProxySecret` |
| `MK_TESTMODE` | `testMode` |
| `MK_DISABLEENDPOINTRATELIMITS` | `disableEndpointRateLimits` |
| `MK_BCRYPTCOST` | `bcryptCost` |
| `MK_CROSSORIGINOPENERPOLICY` | `crossOriginOpenerPolicy` |
| `MK_NOTEHOOKCONCURRENCY` | `noteHookConcurrency` |
| `MK_MEDIAPROXYCONCURRENCY` | `mediaProxyConcurrency` |
| `MK_QUEUEIDLEPOLLSECONDS` | `queueIdlePollSeconds` |
| `MK_QUEUESTUCKWORKERSECONDS` | `queueStuckWorkerSeconds` |
| `MK_QUEUEHANDLERDEADLINESECONDS` | `queueHandlerDeadlineSeconds` |
| `MK_EFFECTIVEPOLICYPROVIDERCACHEENTRIES` | `effectivePolicyProviderCacheEntries` |

用途別Redisも同様 (例: `MK_REDISFORPUBSUB_HOST`)。

**上表は一部。** `internal/config/config.go` の `bindEnvKeys()` は **90 キー**を
登録している。内訳は用途別 Redis 5 系統 (`redis` / `redisForPubsub` /
`redisForJobQueue` / `redisForTimelines` / `redisForReactions`) が各 9、`db.*` が 9、
`logging.sql.*` が 2、`sentryForBackend.options.{dsn,environment}` が 2、残り 32 が
トップレベル。全量はその関数を見ること。

**登録の有無で「作れるか」だけが変わる。** Viper は `AutomaticEnv` を有効にしている
ので、**設定ファイルにそのキーが書かれていれば `MK_` で上書きできる**。`bindEnvKeys()`
に登録されているキーは、ファイルに書かれていなくても `MK_` だけで設定できる。

| | `bindEnvKeys()` に登録あり | 登録なし |
|---|---|---|
| yml にキーがある | `MK_` で上書きできる | **`MK_` で上書きできる** |
| yml にキーが無い | `MK_` だけで設定できる | **効かない** (黙って無視される) |

引っかかるのは `.config/*.yml.example` が既定でコメントアウトしているものです。
`#meilisearch:` と `#<queue>JobConcurrency` / `#<queue>JobPerSec` は**コメントアウト
されたまま**なので、example をそのまま使う構成では `MK_MEILISEARCH_HOST` /
`MK_DELIVERJOBCONCURRENCY` を export しても効きません。使うならまず yml 側の
コメントを外してください。`db.extra.ssl` (example ではコメントアウト) も同じで、
`MK_DB_EXTRA_SSL` を効かせるには yml に `extra:` と `ssl:` の行が要ります。

キー単位の判定です。`meilisearch:` ブロックだけあって `apiKey:` 行が無ければ
`MK_MEILISEARCH_APIKEY` は効きません。

ファイルに書かずに環境変数だけで設定したいものは `bindEnvKeys()` への追加が要ります。

**`MK_*` は設定ファイルより優先されます。** 手元で export したまま
`internal/config` のテストを走らせると、ファイルの値を期待するケースが落ちます。

新しいオーバーライド対象を追加する場合は`internal/config/config.go`の`bindEnvKeys()`にキーを追加する。

## プロセスの役割 (Web / 配送の分離)

対応するYAMLキーは無く、**環境変数だけ**で指定する (upstream Misskey と同じ変数名)。
設定ファイルは両ノードで同一のものを使う。

| 環境変数 | 用途 |
|---|---|
| `MK_ONLY_SERVER` | HTTPのみ提供。ジョブは積むが処理しない |
| `MK_ONLY_QUEUE` | ジョブキューのみ処理。APIは生えず`/healthz`だけ応答する |

値は`1` / `true` / `yes` / `on`で有効、`0` / `false` / `no` / `off` / 空で無効。
未知の値と、両方を同時に有効化した場合は起動時エラーになる。

未指定なら1プロセスが両方を担う (既定・従来どおり)。運用手順は
[デプロイ](deployment.md#web-と配送を別ノードに分ける)を参照。

## フロントエンド関連環境変数

| 環境変数 | 用途 |
|---|---|
| `MISSKEY_FRONTEND_DIR` | ビルド済み SPA (vite 出力)。既定 `third_party/misskey/built/_frontend_vite_` |
| `MISSKEY_FRONTEND_DIST_DIR` | locales / fonts 等の dist。既定 `third_party/misskey/built/_frontend_dist_` |
| `MISSKEY_FRONTEND_EMBED_DIR` | embed 用の vite 出力。**既定値は `MISSKEY_FRONTEND_DIR` の sibling として解決される** — 別の変数を要求すると設定漏れに気付けないまま `/embed_vite/*` が 404 になるため (以前は dev server proxy へ落ちて 502 だった。dev モード以外では proxy しない) |
| `MISSKEY_SW_DIST_DIR` | service worker の出力。既定値の解決は embed と同じ |
| `MISSKEY_FLUENT_EMOJI_DIR` | fluent-emoji ディレクトリ (実績バッジ / 通知アイコン) |
| `MISSKEY_TWEMOJI_DIR` | Twemojiアセットディレクトリ |
| `MISSKEY_CLIENT_ASSETS_DIR` | クライアントアセットディレクトリ |
| `MISSKEY_STATIC_DIR` | 静的ファイルディレクトリ (backend/assets: favicon等) |
| `MISSKEY_REPO_ASSETS_DIR` | リポジトリ直下アセット (ai.png, banner等) |

## テスト用環境変数

CIでのテスト実行時に使用。Redis は testcontainers が立てるが、**PostgreSQL は自分で用意する**。既定は `localhost:5432` の `misskey_test` に `mk` / `mk` で、違う接続先を使うときだけ `.env.test` を置くか export する (→ [testing.md](testing.md))。

`TEST_REDIS_*` を読むのは `internal/core/chart` だけで、`SetupRedis` は環境変数を見ずに必ず testcontainers を立てる。

| 環境変数 | 用途 |
|---|---|
| `TEST_DB_HOST` | テスト用PostgreSQLホスト |
| `TEST_DB_PORT` | テスト用PostgreSQLポート |
| `TEST_DB_NAME` | テスト用データベース名 |
| `TEST_DB_USER` | テスト用ユーザー名 |
| `TEST_DB_PASS` | テスト用パスワード |
| `TEST_DB_SSLMODE` | テスト用SSLモード |
| `TEST_REDIS_HOST` | テスト用Redisホスト |
| `TEST_REDIS_PORT` | テスト用Redisポート |

## マイグレーションの接続先

`cmd/migrate` は **`DATABASE_URL` を読まない**。`-config` (既定 `.config/default.yml`) を読み、`db.*` から DSN を組み立てる。TLS (`db.extra`) とパスワードのエスケープは本体と同じ規則に従う (以前は `db.extra.ssl` を見ずに常に平文で繋いでいた)。

```bash
make migrate-up                                    # .config/default.yml へ
go run ./cmd/migrate -config .config/other.yml     # 別の設定ファイルへ
MK_DB_HOST=other-host make migrate-up              # 個別キーだけ環境変数で上書き
```

## 設定例

### 最小構成

```yaml
url: http://localhost:3000
port: 3000
db:
  host: localhost
  port: 5432
  db: misskey
  user: misskey
  pass: misskey
redis:
  host: localhost
  port: 6379
id: aidx
```

### 本番構成例

```yaml
url: https://misskey.example.com
port: 3000
db:
  host: db.internal
  port: 5432
  db: misskey
  user: misskey
  pass: strong-password
  extra:
    ssl: true
redis:
  host: redis.internal
  port: 6379
  pass: redis-password
redisForJobQueue:
  host: redis-jobs.internal
  port: 6379
  pass: redis-password
  db: 1
meilisearch:
  host: search.internal
  port: 7700
  apiKey: your-api-key
  index: misskey
id: aidx
maxFileSize: 524288000
```
