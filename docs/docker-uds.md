# docker-compose で動かす UDS-only スタック

Phase 12-1 で入った UNIX domain socket (UDS) 対応を使って、mk-go の全コンポーネントを TCP 無しで動かす参照デプロイメントです。ブラウザ側には同梱フロントエンド (`frontend/`、Misskey の fork) の vite ビルド成果物を配信するので、`http://localhost/` を開けば Misskey の UI が出ます。

- nginx が受けるのは host の 80 番だけ (HTTP のみ)
- nginx → mk-go は UDS (`/run/mkgo/mkgo.sock`)
- mk-go → postgres は UDS (`/var/run/postgresql/.s.PGSQL.5432`)
- mk-go → valkey は UDS (`/run/valkey/valkey.sock`、`port 0` で TCP 完全無効)

既存の `docker-compose.yml` (TCP 版 quick-start) とは別ファイル (`compose.uds.yaml`) として併存しているので、従来の `docker compose up -d` 体験は壊れません。

## 前提条件

- Docker と docker compose v2
- host 側のインストールは不要です。フロントエンドは本体の `frontend/` (#3379 で取り込んだ pnpm workspace) から docker 経由でビルドします。

## 初回セットアップ

### 0. 設定ファイルのコピー

`compose.uds.yaml` と `deploy/uds/config/default.yml` はデプロイ先ごとに URL や公開ポートを書き換える想定で gitignore しています。リポジトリには `.example` 版のみ含まれているので、初回のみ以下でコピーしてください。以降は自分の環境に合わせて自由に編集して構いません。

```sh
make uds-init
```

`uds-init` は order-only prerequisite で実装されているので、ファイルが既にある場合は何もしません (`.example` を更新してもローカル編集は上書きされない)。`uds-build` / `uds-up` / `uds-restart` / `uds-down` / `uds-down-v` / `uds-logs` / `uds-ps` も同じ prerequisite を持つため、コピー忘れでエラーになることはありません (`uds-frontend-build` は compose / config を参照しないので対象外。`uds-rebuild` 自身は prerequisite を持ちませんが、呼び出す `uds-build` が満たします)。

### 1. フロントエンドのビルド

初回のみ、`frontend/` の vite ビルドを行います (3〜10 分)。`make uds-frontend-build` は既存の `e2e-frontend-build` と同一のターゲットで、`node:<版>-<distro>` の image の中で `pnpm install --frozen-lockfile && pnpm build` を走らせます。Node の版は `frontend/.node-version` (CI の `node-version-file` と同じもの) から、distro は Makefile の `FRONTEND_NODE_DISTRO` (既定 `trixie`) から取ります。pnpm の版は `frontend/package.json` の `packageManager` から取ります (#2921)。以前は `node:22-bookworm` 固定で、CI が `.node-version` を見るのに本番のビルドだけ Node 22 という食い違いがあり、しかも `node:22-bookworm` の 22.22.2 は `engines.node` の下限ちょうどでした。

ビルドの前に `make plugins` が走り、`plugins/` に置いたプラグインの frontend の登録 (`frontend/packages/frontend/src/server-plugins.generated.ts`) を生成します。このファイルは git で追跡していないので、`frontend/` を手で直接ビルドするときも先に `make plugins` を実行してください。

```sh
make uds-frontend-build
```

終了すると以下のパスに成果物ができます。`compose.uds.yaml` が read-only で bind mount します。

- `frontend/built/_frontend_vite_/manifest.json`
- `frontend/built/_frontend_dist_/`

なお `pnpm install --frozen-lockfile` が `frontend/` の node_modules も生成するため、絵文字のアセットも同時に揃います。これらと、git で追跡しているアセットは `deploy/uds/Dockerfile.mkgo` が `COPY` で runtime image に焼き込み、mk-go の `/twemoji/*` / `/fluent-emoji/*` / `/assets/*` / `/static-assets/*` ルートから配信します。

- `frontend/node_modules/@misskey-dev/emoji-assets/built/twemoji/` (twemoji SVG set。`pnpm install` で揃う)
- `frontend/node_modules/@misskey-dev/emoji-assets/built/fluent-emoji/` (実績バッジ / 通知アイコン。`pnpm install` で揃う)
- `frontend/repo-assets/` (`ai.png` 等、約684KB。git で追跡)
- `frontend/assets/` (favicon やアイコン等。git で追跡)

`make uds-frontend-build` を省略すると、image ビルド時に絵文字のアセットの存在チェックが fail してビルドが止まります。

### 2. スタックの起動

```sh
make uds-up
```

以下が順番に立ち上がります。

1. `postgres` — `/var/run/postgresql/.s.PGSQL.5432` を作成
2. `valkey` — `/run/valkey/valkey.sock` を作成 (TCP は `port 0` で無効)
3. `mkgo` — マイグレーション実行後、`/run/mkgo/mkgo.sock` で HTTP listen
4. `nginx` — host の 80 番で受けて mk-go の socket に proxy

```sh
make uds-ps
```

全サービスが `healthy` になれば準備完了です。

### 3. 動作確認

```sh
curl -i http://localhost/
curl -i -X POST http://localhost/api/meta -H 'Content-Type: application/json' -d '{}'
```

ブラウザで `http://localhost/` を開き、Misskey のログイン画面が出ることを確認してください。DevTools の Network タブで `/streaming` が `ws://localhost/streaming` で確立していれば WebSocket の upgrade も成功しています。

## UDS で繋がっていることの確認

どのサービスにも `ports:` を切っていないので、ホスト側で 80 番以外の TCP は一切 listen していないはずです。

```sh
ss -tlnp | grep -E ':(80|3000|5432|6379)'
```

80 番だけが出て、3000 / 5432 / 6379 は出てこないことを確認してください。

socket ファイルの実体も `docker compose exec` で覗けます。

```sh
docker compose -f compose.uds.yaml exec mkgo ls -la /var/run/postgresql /run/valkey /run/mkgo
docker compose -f compose.uds.yaml exec nginx ls -la /run/mkgo
```

## 停止 / クリーンアップ

```sh
# コンテナだけ停止 (named volume のデータは残る)
make uds-down

# named volume まで削除 (DB / valkey / drive files が全部消える)
make uds-down-v
```

## ログ確認

```sh
make uds-logs            # 全サービスのログを follow
docker compose -f compose.uds.yaml logs -f mkgo
docker compose -f compose.uds.yaml logs -f nginx
```

### ログの上限

`compose.uds.yaml.example` は全サービスに **50 MB × 3 世代**の上限を掛けている
(`x-logging` アンカー)。Docker 既定の `json-file` はローテーションしないので、
指定しないとディスクが埋まる。`max-size` は decimal で読まれるので `50m` は
50,000,000 バイト (= 47.7 MiB)。

**既存のコンテナには効かない。** ログの設定は生成時に固定されるので、
`make uds-up` で作り直すまで反映されない。**この設定を初めて取り込む起動は
4 サービス全部を作り直す**ので、postgres も一度止まる。

```sh
docker inspect mk-mkgo-1 --format '{{.HostConfig.LogConfig.Config}}'
# → map[max-file:3 max-size:50m]   効いている
# → map[]                          効いていない
```

自分の `compose.uds.yaml` は gitignore されているので、`.example` を更新しても
**自動では反映されない**。差分を手で取り込むこと。

## トラブルシューティング

### `frontend/built` が無い

`compose.uds.yaml` は `./frontend/built` と `./frontend/packages/frontend/assets` を read-only で bind mount しています。`make uds-frontend-build` を先に実行してください。bind mount の source が存在しないと `uds-up` が失敗します。

#3379 より前の版から上げた環境で、`compose.uds.yaml` がまだ `./third_party/misskey/built` を指している場合は、[デプロイの切り替え手順](deployment.md#frontend-を本体へ取り込んだ版へ上げる-3379)に従って向け直してください。

### マイグレーションが失敗してコンテナが crash loop する

`mkgo-entrypoint.sh` は `set -e` で migrate を実行してから mk-go server を起動します。migrate が失敗するとそのまま container exit し、`restart: unless-stopped` のため compose が再起動 → 同じエラーで再度 exit、というループに入ります。

検出方法:

```sh
make uds-logs                                            # 全サービスのログを follow
docker compose -f compose.uds.yaml logs mkgo | tail -50  # mkgo だけ、過去ログ含めて
```

`[mkgo-entrypoint] running migrations...` の直後に migrate がエラーを吐いていれば該当します。

対応:

- スキーマが壊れている場合は手動で `psql` で問題を解消する。migration を巻き戻すなら `docker compose -f compose.uds.yaml exec mkgo /app/migrate -direction down -steps 1`。**UDS image は `/app/migrate` を同梱していて entrypoint がこれを叩く** (`deploy/uds/Dockerfile.mkgo`)。コンテナには Go toolchain が無いので `go run ./cmd/migrate` は使えない。手元のツリーから叩く場合は `go run ./cmd/migrate` で、`make build` は `./built/misskey` しか作らない
- volume 自体がおかしい場合は `make uds-down-v` で named volume を消して綺麗な状態から再構築する (**DB データは全部消える**ので注意)
- **`-steps` を省略すると全段 down する** (全テーブルが消える)。1 段だけ戻したいときは必ず `-steps 1` を付ける

### `/healthz` が 404 になる

mk-go 側の実装変更で `/healthz` のパスが変わっている可能性があります。`internal/server/router.go` を grep して、存在するパスに合わせてください。**healthcheck を定義しているのは `compose.uds.yaml` の mkgo service** で、`deploy/uds/Dockerfile.mkgo` には `HEALTHCHECK` 命令はありません (Dockerfile がやっているのは curl の同梱だけ)。

### nginx が `connect() to unix:/run/mkgo/mkgo.sock failed (13: Permission denied)`

`chmodSocket: "666"` が正しく反映されていません。`deploy/uds/config/default.yml` を確認してください。mk-go の起動ログは `starting Misskey server socket=<path> url=<url>` の形 (`[server] listening on unix:` という行は出ません)。実際のパーミッションは `ls -l` で直接見るのが確実です。

### valkey への接続が `resource temporarily unavailable` で失敗する

```
time=... level=WARN msg="redis: connection pool: failed to dial after 5 attempts: dial unix /run/valkey/valkey.sock: connect: resource temporarily unavailable" source=go-redis
```

go-redis が内部ロガーに出すものを slog へ流している (#2659)。**`source=go-redis`
で絞れる。**

それ以前のビルドでは slog を通らず stderr に直接出る。形は
`redis: <日付> <時刻> pool.go:617: redis: connection pool: ...` で、
**prefix と `file:line` の間に日時が挟まる** (`log.LstdFlags|log.Lshortfile`)。
`grep "redis: pool.go:"` では**一致しない**ので、古いログは
`grep "failed to dial after"` のように本文で探すこと。

EAGAIN で、**UDS の listen backlog が溢れている**ときに出る。TCP と挙動が違う
点が 2 つある。

- TCP の非ブロッキング connect は `EINPROGRESS` を返すので Go は epoll で
  待てるが、**AF_UNIX は `EAGAIN` を返す**。Go の `fd.connect` はこれを
  即エラーにする (`GOROOT/src/net/fd_unix.go` が待つのは
  `EINPROGRESS` / `EALREADY` / `EINTR` だけ)。1 回の dial で待ってはくれない
- ただし go-redis は既定で 5 回・100ms 間隔で dial し直す
  (`DialerRetries` / `DialerRetryTimeout`)。**"failed to dial after 5 attempts"
  が出たということは、accept キューが 0.5 秒前後ふさがり続けた**ということ
  (失敗するたびに毎回 sleep するので 5 回で約 500ms)。瞬間的な溢れでは
  このログにはならない
- backlog 溢れはカーネル側で起きるので valkey からは見えない。
  `rejected_connections` は maxclients による拒否のカウンタで、これは増えない。
  AF_UNIX には TCP の `ListenOverflows` に相当するカウンタも無く、**事後に
  確認する手段が無い**

実効 backlog は `min(tcp-backlog, net.core.somaxconn)`。既定は
`tcp-backlog 511` で、`deploy/uds/valkey/valkey.conf` では設定していない。

```
docker exec mk-valkey-1 valkey-cli -s /run/valkey/valkey.sock config get tcp-backlog
docker exec mk-valkey-1 cat /proc/sys/net/core/somaxconn
```

`somaxconn` は **valkey が listen している netns のものが効く**ので、host 側で
`cat` しない (`--sysctl net.core.somaxconn` を指定した構成だと値が食い違う)。

本番の実測は `tcp-backlog` 511 / `somaxconn` 4096 なので、**実効 backlog は
511 で、効いているのは `tcp-backlog` のほう**。valkey は要求した backlog が
`somaxconn` に切り詰められると起動時に
`WARNING: The TCP backlog setting of ... cannot be enforced` を出すが、本番の
ログには 1 件も無い = 511 がそのまま通っている。**上げるなら `tcp-backlog`
であって `somaxconn` ではない** (後者を上げても min は変わらない)。

**2026-08 時点では上げないと判断した** (#2659)。理由は実効値ではなく**レート**で、
接続レートが 9 日平均で 776 conn/hour (0.22/sec、`total_connections_received` /
`uptime_in_seconds`) しかないため。slowlog の最遅コマンドも 25.1ms
(08-23 時点の全 entry の最大値。`slowlog-max-len` は 128 で溢れておらず、
**記録は valkey の起動 08-13 から続いている**ので 08-20 18:42 も窓の中) で、
accept を長時間止めるコマンドは見当たらなかった。`MinIdleConns` で事前に張る案は #2648 / #2649 で
減らした常駐リソースと逆行するので採らなかった。

**ただし「溢れていない」ことを示せたわけではない。** 9 日平均はサブ秒の
バーストを否定しないし、go-redis は `MinIdleConns: 0` なので負荷の立ち上がりで
一斉に dial が走る。バースト深さ (valkey の netns で見た `ss -lx` の
Recv-Q、または `total_connections_received` の秒単位の差分) は**測っていない**
(netns の話は下の再発時の手順を参照)。

**18:42 に何が起きたかは特定できていない。** ただし valkey 自身のログに
近接した異常が残っている (コンテナは UTC なので JST に読み替えること):

```
1:M 20 Aug 2026 09:40:30.397 * Asynchronous AOF fsync is taking too long (disk is busy?).
1:M 20 Aug 2026 09:40:32.669 * Asynchronous AOF fsync is taking too long (disk is busy?).
1:M 20 Aug 2026 09:42:44.048 * 100 changes in 300 seconds. Saving...
```

**手がかりとして意味があるのは fsync のほうだけ。** この警告はログ全体で
7 件しかなく、**うち 2 件がこの 2 分間に集中している**。AOF flush は valkey の
メインスレッドを止めうるので、accept が止まれば backlog は積む。

一方 **`Saving...` (RDB の fork) は異常ではない。** 5 分ごとの定期実行で、
ログ全体に 23,000 件超・08-20 だけで 287 件ある
(`docker logs mk-valkey-1 | grep -c "Saving\.\.\."` と、それを `20 Aug 2026`
で絞ったもの)。18:42:44 のものが特別だと考える根拠は無い。並べて書くと同じ
珍しさに見えるので注意。

**fsync 遅延も fork も `slowlog` では見えない。** slowlog はコマンドの実行時間
だけを測るので、AOF flush・fork・serverCron のようなコマンド外のイベントループ
停止は原理的に載らない。再発時に見るべきものは順に:

```
# イベントループ停止。Saving... は定期実行なので fsync だけを見る
docker logs mk-valkey-1 | grep -i "fsync is taking too long"
docker exec mk-valkey-1 valkey-cli -s /run/valkey/valkey.sock info persistence \
  | grep aof_delayed_fsync   # 再起動でリセットされるのでログの件数とは一致しない
# fork の所要時間は persistence ではなく stats 側にある。ただし
# latest_fork_usec は直近 1 回の値でしかない (valkey は最大値を持たない) ので、
# 過去の fork が速かった証拠には使えない
docker exec mk-valkey-1 valkey-cli -s /run/valkey/valkey.sock info stats \
  | grep -E "latest_fork_usec|total_forks"
docker exec mk-valkey-1 valkey-cli -s /run/valkey/valkey.sock slowlog get 128
```

未 accept の滞留 (Recv-Q) を見るには **valkey の netns に入る必要がある**。
AF_UNIX の listen は netns に閉じているので host 側の `ss -lx` には**出ない**
(空振りするだけでエラーにならないので「滞留していない」と誤読しやすい)。
container には `ss` が入っていないので host のものを持ち込む:

```
sudo nsenter -t $(docker inspect -f '{{.State.Pid}}' mk-valkey-1) -n ss -lx \
  | grep valkey.sock
```

#### この障害と #2657 (worker の詰まり) の関係

**因果は示せていない。** #2657 は当初「この障害が引き金」と書いていたが、
それを支える証拠は無く、逆に**この障害と無関係に同じ症状が全部出る**ことが
確認できている。以下は 2026-08-23 時点の実測。

**1. 同じ失敗が 18:42 より前から同じペースで出ていた。** 08-20 の 18:42 より
前に、`stc=2` の stalled 失敗が 5 件ある。

```
11:43:28   13:25:43 (x2)   16:42:56   16:50:20      いずれも stc=2
```

7 時間で 5 件 = 約 0.7 件/時で、18:42 以降のペースと変わらない。当初の記録に
あった「それ以前は 1 日 1 件程度」は誤りで、**この前後比較が「18:42 が引き金」
という見立ての唯一の量的根拠だった**。

(これらが blip を踏んだのと同一プロセスかは**確かめられない**。当時の
container は既に削除されており (現行は 08-21 02:03 JST 作成)、job HASH にも
worker を特定する情報が無い。ただし前後比較の反証に同一性は要らない。)

**なお 18:45-18:58 のバースト自体も、blip では説明しにくい。** 失敗した 5 件の
うち 3 件は **08-19 に作られた job** (11:26 / 19:17 / 19:39) で、23 時間ほど
掴まれたままだった。長く掴まれていた active job がまとめて失敗するのは
**プロセスの再起動が active を孤児にしたとき**の形で、記録にある
「18:44:57 から Stop タイムアウトが始まっている」とも整合する。

**2. valkey のエラーを一度も出していないプロセスで、症状が全部再現している。**
現行 mk-mkgo-1 は 08-21 02:03 JST 起動で `resource temporarily unavailable` が
0 件 (`docker logs mk-mkgo-1 | grep -c "resource temporarily unavailable"`)。
その無傷のプロセスで:

- `bull:inbox:active` に job が 4 件、31-41 時間掴まれたまま。**4 件とも
  このプロセスの起動後に作られている**ので引き継ぎではない
- 起動後に失敗した job が 27 件、うち **25 件**が
  `job stalled more than allowable limit` (残り 2 件は inbox 処理中の外向き
  取得が 530 / タイムアウトになったもので別件)
- そのうち **8 件が 08-22 23:00-23:30 に集中**。08-20 の「13 分で 5 件」と
  同じ形のバーストが、エラー無しで起きている

数え方: `ZRANGEBYSCORE bull:inbox:failed <起点の epoch-ms> +inf` で対象を取り、
各 job の `failedReason` を `HGET` して理由ごとに数えた。

**3. そもそも一瞬のエラーでは failed にならない。** mkq が job を stalled 理由で
failed にするのは lua の `stalledCount > maxStalledJobCount` を満たしたときだけ
で、既定は `maxStalledCount = 1`。つまり **`stc` が 2 に達する = 30 秒
(`stalledInterval`) 以上離れた回収が 2 回**必要になる。18:45-18:58 に失敗した
5 件はすべて `stc=2` だった。1 回きりの EAGAIN では届かず、**継続的に lock を
失わせる何か**が要る。

**その「何か」の候補は分かっている。** 詰まった handler が worker を占有し、
autoscale がそれを健全と数えて resize を繰り返し、`Stop` がタイムアウトして
lock が切れる、というループ。このループだけで、バーストを含む症状全体が実際に
再現している (上の 2)。

```
docker logs mk-mkgo-1 | grep -c "autoscale resized.*inbox"   # 08-23 時点で 3000 超
docker logs mk-mkgo-1 | grep -c "worker stop error"          # 同 50 前後
```

どちらも稼働中ずっと増えるので、値そのものより桁を見ること。

**示せた範囲の限界。** ここまでで言えるのは「valkey のエラーは症状の必要条件
ではない」まで。**18:42 の blip が寄与した可能性そのものは否定できない**
(1 回の `ExtendLock` 失敗が回収を 1 つ増やす経路は存在する)。ただし blip の
前から同じ失敗が同じペースで出ている以上、**原因として blip を書くのは誤り**。

補足を 2 つ。

- 18:45-18:58 の失敗は **5 件**。#2657 の初期の記録にある「20 件」は当時の
  failed セット全体の件数で、この時間帯の件数ではない。**しかもその 20 件も
  全部が stalled ではなく 16 件** (残り 4 件は 500 x3 / pure renote x1)
- `stc` 未設定を「lock が途切れていない」の証拠に使えるのは、**同じプロセスで
  stalled-check が現に動いていた**から (同期間に 25 件を回収している)。checker
  が止まっていれば `stc` が増えないのは当然なので、この前提は毎回確認すること

詰まりの側は #2657 (隔離) と #2658 (handler の期限) で対処してある。

### valkey が `Creating Server TCP listening socket *:6379: bind` で起動しない

`deploy/uds/valkey/valkey.conf` の `port 0` が効いていません。`command:` で正しく `valkey.conf` を読み込めているか、bind mount のパスを確認してください。

## TLS を足したい場合

このデプロイは HTTP のみです。本番で TLS を張りたい場合は以下のいずれかで。

- 上位に Caddy や Cloudflare Tunnel を置いて TLS 終端する (nginx の 80 番はそのまま)
- `deploy/uds/nginx/mkgo.conf` に 443 listen + `ssl_certificate` を足す (`tests/federation/common/nginx-mkgo-ssl.conf` が参考になる)

## 構成ファイル一覧

`compose.uds.yaml` と `deploy/uds/config/default.yml` はリポジトリには `.example` 版のみ存在し、`make uds-init` (または初回の `make uds-*`) で `.example` からコピーされる。以下の表はセットアップ後のローカルファイル名で記載している。

| ファイル | 役割 |
|---------|------|
| `compose.uds.yaml` (`.example` から生成) | 全サービスを繋ぐ compose エントリポイント |
| `deploy/uds/Dockerfile.mkgo` | mk-go runtime image (migrate 同梱 + curl) |
| `deploy/uds/mkgo-entrypoint.sh` | migrate → exec misskey |
| `deploy/uds/nginx/mkgo.conf` | UDS upstream + WebSocket upgrade 付き nginx 設定 |
| `deploy/uds/valkey/valkey.conf` | `port 0` + UNIX socket listen の valkey 設定 |
| `deploy/uds/config/default.yml` (`.example` から生成) | UDS 前提の mk-go 設定 |
