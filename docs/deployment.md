# デプロイ

## PostgreSQL 16 → 18 への移行 (既存環境)

compose 群と CI は PostgreSQL 18 に統一した (#2513)。**既存の 16 の data volume はイメージを上げるだけでは開けない** — メジャーアップグレードには dump→restore (または pg_upgrade) が必要で、そのまま起動すると `database files are incompatible` で crash loop になる。

さらに `postgres:18` イメージは **data layout が変わった** (default PGDATA が `/var/lib/postgresql/18/docker`、`VOLUME` 宣言が `/var/lib/postgresql` 親ディレクトリへ)。compose 群のマウント先はこれに合わせて `/var/lib/postgresql` に変更済み。旧パス (`/var/lib/postgresql/data`) のままイメージだけ上げると、**新規デプロイでは匿名 volume 側に initdb され、`down` で全データが静かに消える**。自前 compose を使っている場合はマウント先を確認すること。

既存環境の移行手順 (ダウンタイム = dump + restore の時間)。サービス名は
`docker-compose.yml` (TCP 構成) のもの (`app` / `db`)。UDS 構成は `mkgo` /
`postgres` に読み替える (UDS は明示 `PGDATA` なのでマウント先は旧パスのまま
変えなくてよい):

```bash
# 0. 先に compose を 18 版 (イメージ + マウント先) へ更新しておく
#    (git pull。ここを忘れて古い compose のまま進めると 16 で init し直すだけになる)

# 1. アプリを止めて書き込みを停止 (postgres は起動したまま)
docker compose stop app

# 2. dump を取る
docker compose exec -T db pg_dumpall -U <user> > pg16-dump.sql

# 3. 全体を止め、volume 自体もバックアップしてから作り直す
#    (tar は postgres 停止後に取る。稼働中に取ると crash-consistent ですらない)
docker compose down
docker run --rm -v <pg-volume>:/from -v "$PWD":/to alpine tar czf /to/pg16-data.tar.gz -C /from .
docker volume rm <pg-volume>
docker compose up -d db          # ここで postgres:18 が新規 initdb する

# 4. restore (init が作った空 DB を落としてから dump を流す)
docker compose exec -T db psql -U <user> -d postgres -c 'DROP DATABASE <db>;'
docker compose exec -T db psql -U <user> -d postgres < pg16-dump.sql
docker compose exec -T db vacuumdb -U <user> --all --analyze-in-stages

# 5. アプリ再開・確認
docker compose up -d
```

## pg_bigm (日本語の部分一致検索を高速化)

pg_bigm は 2-gram の GIN インデックスで `LIKE` 部分一致を加速する PostgreSQL 拡張。mk-go の `sqlLike` 検索 (デフォルト) は upstream と同じ `lower(text) LIKE ...` 形なので、**拡張とインデックスを作るだけで provider の変更なしに** インデックスが効く (#2514)。

1. pg_bigm 入りイメージを使う。UDS 構成 (`compose.uds.yaml.example`) は既定でこれをビルドする。他の構成では postgres サービスを差し替える:

```yaml
  db:
    build:
      context: deploy/postgres-bigm   # postgres:18-alpine + pg_bigm
```

既存の稼働環境で postgres サービスをこのイメージへ差し替えると、コンテナが recreate される (data volume は残るが短い停止を伴う)。

2. 拡張とインデックスを作る (**インデックス作成は opt-in。migration には入れない** — 拡張の無い環境で落ちるため、pgroonga と同じく operator 責務)。`CREATE INDEX CONCURRENTLY` はトランザクション内で実行できないので、**2 文を別々に実行する** (`psql -c "A;B"` や `-1` でまとめると単一トランザクション扱いになりエラー):

```sql
CREATE EXTENSION IF NOT EXISTS pg_bigm;
CREATE INDEX CONCURRENTLY idx_note_text_bigm ON note USING gin (lower(text) gin_bigm_ops);
```

3. 効いていることを確認:

```sql
EXPLAIN SELECT id FROM note WHERE lower(text) LIKE '%検索語%';
-- → Bitmap Index Scan on idx_note_text_bigm が出れば OK
--   (小さいテーブルでは planner が seq scan を選ぶこともある)
```

`fulltextSearch.provider` は `sqlLike` のままでよい。拡張を外す場合はインデックスを落とすだけで検索自体は動き続ける (seq scan に戻る)。

## Docker Compose (TCP)

最も簡単な起動方法。PostgreSQL、Redis、mk-goの3サービスをTCPで接続する。

```bash
git clone --recursive https://github.com/shiroha-a/mk.git
cd mk

# フロントエンドビルド (初回のみ、3-10分)
make e2e-frontend-build

# 起動
docker compose up -d

# http://localhost:3000 でアクセス
```

`.config/docker.yml.example` が Dockerfile に焼き込まれ、`docker-compose.yml` の DB/Redis 既定値と整合した状態で起動するため、**初期設定なしで `docker compose up` だけで動く**。

設定をカスタマイズしたい場合は example をコピーして編集 + volume mount で上書き:

```bash
cp .config/docker.yml.example .config/docker.yml
# .config/docker.yml を編集 (URL / mediaProxy / etc.)
# docker-compose.yml の volumes コメントを外す:
#   - ./.config/docker.yml:/app/.config/default.yml:ro
docker compose up -d
```

`docker-compose.yml`の構成:
- **app**: mk-goコンテナ (ポート3000)
- **db**: PostgreSQL 18 Alpine
- **redis**: Redis 7 Alpine

ファイルストレージは`./files`にマウントされる。

> **注意**: コンテナは **UID/GID 991** (Misskey TS と同じ) で起動するため、ホスト側の `./files` ディレクトリは UID 991 が書き込めるパーミッションでなければならない。Misskey TS から移行する場合は既に 991 所有なのでそのままで OK。**今まで mk-go の旧 root 構成で運用していて初めて 991 化に追従する場合は、一度だけ `sudo chown -R 991:991 ./files` で所有権を揃える必要がある**。

## Docker Compose (bundled image / ビルド不要)

フロントエンドアセットを同梱した `bundled` イメージを pull するだけで起動できる。**フロントエンドのビルドもイメージのビルドも要らない。**

動かすだけならソースを clone する必要は無い。compose と設定のひな形だけを置いた
[`docker` ブランチ](https://github.com/shiroha-a/mk/tree/docker)を使う (数十 KB)。

```bash
git clone --depth 1 -b docker https://github.com/shiroha-a/mk.git mk
cd mk

mkdir -p files && sudo chown -R 991:991 files
docker compose up -d
```

ソースを持っている場合は Makefile から同じことができる。

```bash
mkdir -p files && sudo chown -R 991:991 files

make image-up          # 起動 (docker-compose.image.yml)
make image-logs        # ログ
make image-down        # 停止
```

`url` を設定する場合は `.config/docker.yml` を用意し、`docker-compose.image.yml` の **`app` と `migrate` 両方**の volumes コメントを外す。マイグレーションは one-shot の `migrate` サービスが自動適用する。

手元の変更を反映したい場合は先にイメージを作る。

```bash
make image-build       # ghcr.io/shiroha-a/mk:bundled をローカルにビルド
```

ソースからビルドする従来の構成 (`docker-compose.yml` / `make docker-*`) はそのまま使える。こちらは置き換えではなく並立する選択肢。

### prebuilt imageについて

イメージは 2 種類ある。

| イメージ | 内容 | 用途 |
|---|---|---|
| `ghcr.io/shiroha-a/mk:bundled` | Goバイナリ + マイグレーション + [後始末バッチ](#後始末バッチ) + **フロントエンドアセット同梱** | pull して即起動 |
| `ghcr.io/shiroha-a/mk:latest` | Goバイナリ + マイグレーション + [後始末バッチ](#後始末バッチ) | アセットを別途用意する構成 |

`bundled` / `latest` は develop の最新を指す **可変タグ**。本番ではバージョンを固定する。

```bash
MK_IMAGE=ghcr.io/shiroha-a/mk:1.4.0-bundled docker compose up -d
```

リリースタグを push すると `<version>` と `<version>-bundled` が publish される
(`.github/workflows/docker.yml`)。過去のリリースを後追いで publish したい場合は
develop に対して dispatch し、タグを input で渡す。

```bash
gh workflow run docker.yml -f tag=1.1.0
```

> `1.0.0` に `-bundled` は存在しない。アセット同梱イメージは 1.1.0 で追加された機能で、
> 1.0.0 のツリーには `Dockerfile.bundled` が無いため。

同梱アセットは fork (`shiroha-a/misskey-ts`) が publish する `ghcr.io/shiroha-a/misskey-ts-assets:<tag>` 由来で、**mk-go 独自のフロントエンド変更を含む**。

> **注意**: 下記のように upstream の `misskey/misskey` イメージからアセットをコピーする方法もあるが、その場合 **mk-go 独自のフロントエンド変更が失われる** (チャット・リバーシの連合が UI 上で「非対応」表示に戻る等)。drop-in 互換の検証目的でなければ `bundled` イメージを使うこと。

`ghcr.io/shiroha-a/mk:latest`等のprebuilt imageにはGoバイナリとマイグレーションSQLのみが含まれ、フロントエンドアセットは同梱されていない。prebuilt imageを使用する場合は以下の環境変数でアセットディレクトリを指定する必要がある:

- `MISSKEY_FRONTEND_DIR` — viteビルド出力
- `MISSKEY_FRONTEND_DIST_DIR` — dist出力 (locales, fonts)
- `MISSKEY_FRONTEND_EMBED_DIR` — embed 用の vite 出力。**既定は `MISSKEY_FRONTEND_DIR` の sibling として解決される**ので通常は不要。既定から外れた配置のときだけ指定する (指定を忘れると `/embed_vite/*` が 404 になる。dev モード以外では dev server へ proxy しない)
- `MISSKEY_SW_DIST_DIR` — service worker の出力。既定値の解決は embed と同じ
- `MISSKEY_TWEMOJI_DIR` — twemoji SVG
- `MISSKEY_FLUENT_EMOJI_DIR` — fluent-emoji (実績バッジ / 通知アイコン)
- `MISSKEY_CLIENT_ASSETS_DIR` — クライアントアセット
- `MISSKEY_STATIC_DIR` — 静的ファイル (backend/assets: favicon等)
- `MISSKEY_REPO_ASSETS_DIR` — リポジトリ直下の共通アセット (ai.png, banner等)

TS版Misskeyのイメージからアセットをコピーすることも可能:

```dockerfile
FROM misskey/misskey:2026.9.1 AS misskey-assets
FROM ghcr.io/shiroha-a/mk:latest
COPY --from=misskey-assets /misskey/built /frontend
COPY --from=misskey-assets /misskey/packages/frontend/assets /client-assets
COPY --from=misskey-assets /misskey/packages/backend/node_modules/@misskey-dev/emoji-assets/built/twemoji /twemoji
COPY --from=misskey-assets /misskey/packages/backend/node_modules/@misskey-dev/emoji-assets/built/fluent-emoji /fluent-emoji
COPY --from=misskey-assets /misskey/packages/backend/assets /static
COPY --from=misskey-assets /misskey/assets /repo-assets
ENV MISSKEY_FRONTEND_DIR=/frontend/_frontend_vite_
ENV MISSKEY_FRONTEND_DIST_DIR=/frontend/_frontend_dist_
ENV MISSKEY_TWEMOJI_DIR=/twemoji
ENV MISSKEY_FLUENT_EMOJI_DIR=/fluent-emoji
ENV MISSKEY_CLIENT_ASSETS_DIR=/client-assets
ENV MISSKEY_STATIC_DIR=/static
ENV MISSKEY_REPO_ASSETS_DIR=/repo-assets
```

## Docker Compose (UDS)

本番向け構成。UNIX Domain Socketのみで通信し、TCPポートの露出を最小化する。

```
nginx:80 → /run/mkgo/mkgo.sock → mk-go → /var/run/postgresql + /run/valkey/valkey.sock
```

```bash
# フロントエンドビルド
make uds-frontend-build

# 起動
make uds-up

# 確認
curl -i http://localhost/
```

詳細は[UDSデプロイ](docker-uds.md)を参照。

## コンテナログの上限

配布する compose 3 つは、全サービスに **50 MB × 3 世代**の上限を掛けている
(`x-logging` アンカー)。1 サービスあたり最大 150 MB。

**`max-size` は decimal で読まれる。** json-file は `units.FromHumanSize` を使うので
`50m` は 50,000,000 バイト (= 47.7 MiB)。`50mib` と書いても同じで、MiB は表現できない。

**Docker 既定の `json-file` はローテーションしない。** 指定しないとコンテナが
動いているあいだログが増え続け、ディスクを埋める。

```bash
# 効いているか確認する
docker inspect <コンテナ名> --format '{{.HostConfig.LogConfig.Config}}'
# → map[max-file:3 max-size:50m]   効いている
# → map[]                          効いていない (作り直しが要る)
```

**既存のコンテナには効かない。** ログの設定はコンテナ生成時に固定されるので、
`docker compose up -d` で作り直すまで反映されない。上限を変えるときも同じ。

**この設定を初めて取り込む `up -d` は全サービスを作り直す。** 全部に同時に入る
ので config hash が全部変わる — DB も一度止まるので、通常のアップデートのつもりで
実行しないこと。

値を変えるなら compose の `x-logging` を書き換える。ホスト全体に掛けたいなら
`/etc/docker/daemon.json` の `log-opts` でもよいが、**compose 側の指定が優先される**。

**anchor はファイルローカル。** `-f` で override ファイルを重ねる運用をしている
場合、override 側で `*default-logging` は参照できない (`unknown anchor` で起動
できない)。override 側には値を直接書くこと。

## バイナリ直接実行

```bash
# 設定ファイルを example から複製 (初回のみ)
cp .config/default.yml.example .config/default.yml
# 必要に応じて .config/default.yml を編集

# ビルド
make build

# マイグレーション適用 (接続先は .config/default.yml から読む)
make migrate-up

# 起動
./built/misskey -config .config/default.yml
```

前提条件: Go 1.27+ (ビルド時)、PostgreSQL 18推奨 (16以降で動作、CI検証は18)、Redis 7+。

設定ファイルの詳細は[設定リファレンス](configuration.md)を参照。

## データ整合性チェック (fsck)

非正規化カウンタが実データとずれていないかを検査する。

```bash
./built/misskey -config .config/default.yml -fsck        # 検査のみ (既定)
./built/misskey -config .config/default.yml -fsck -fix   # カウンタを直す
```

```
  カウンタのずれ: 2 件
    user.followersCount      1 件
    user.notesCount          1 件

    user.followersCount  id=u1  記録 7 → 実際 0
    user.notesCount  id=u1  記録 99 → 実際 2

  修正するには -fix を付けて再実行してください。
```

**既定は読み取り専用**で、`-fix` を付けたときだけ書き戻す。ずれがあれば exit 1 を返す
(`-fix` で直せば 0)。

### 検査するもの

| 対象 | 突き合わせ先 |
|---|---|
| `user.followersCount` / `followingCount` | `following` の実件数 |
| `user.notesCount` | `note` の実件数 |
| `note.repliesCount` / `renoteCount` | `note.replyId` / `renoteId` の実件数 |
| 孤児行 | 存在しない user を参照する `note` / `drive_file` / `following` |

これらのカウンタは増減で維持されており、増減はベストエフォート (戻り値を捨てる呼び出しが
ある) なので失敗すればそのままずれる。

### 孤児行は自動削除しない

**報告に留める。** カウンタは元データから導けるので `-fix` で直せるが、**削除した行は
復元できない**。影響を確認した上で手動で対応する。

### 検査しないもの

`clippedCount` / `pageCount` は**意図的に対象外**。mk-go はクリップ件数の非正規化カウンタを
維持せず `clip_note` を直接数える設計なので、常に 0 が正しい値になる
([divergence.md](divergence.md) 参照)。実件数と突き合わせると全件がずれとして報告される。

## 設定の実効値を確認する

```bash
./built/misskey -config .config/default.yml -config-dump
```

**サーバーを起動しない**ので、DB / Redis に繋がらない状態でも使える。

出力は「設定値」(YAML + 環境変数 + 既定値を畳んだ結果) と「実効値」(設定値そのもの
ではないが実際に効く値) の 2 部構成。**後者が要点**で、設定ファイルを読んだだけでは
分からないものを出す。

実際の出力から抜粋 (`deliverJobConcurrency: 4` / `deliverJobPerSec: 100` の場合)。
**値と説明文は実出力そのままだが、行は間引いて桁は詰めてある** — worker 行と
stuck 検出行は全キュー分出るほか `frontend 配信元` / `maxFileSize` などもあり、
実際の桁幅は一番長いキュー名に揃うので広い。

```
実効値
  process role            both   (1 プロセスで HTTP とジョブの両方を担う)
  worker: deliver         4   (明示指定)
  worker: inbox           16   (既定値)
  rate: deliver           100 jobs/sec   (queue 全体の上限。worker 数を増やしても変わらない)
  stuck 検出: export       無効   (隔離は無効 (長い batch job が正常なキュー、または queueStuckWorkerSeconds が負値)。ただし handler の期限による漏れは最大 6 本まで許容し、達すると worker を 0 にする)
  stuck 検出: inbox        30m0s   (超過した worker は勘定から外して差し替える。実 worker 数は最大 32 本 (放棄した handler と共用の枠))
  handler 期限            1h0m0s (既定)   (batch 系 task type (export / cleanRemoteFiles / deleteAccount 等) は対象外)
  redis pool (job queue)  80   (poolSize 未指定時の自動サイジング結果 (stuck 検出の許容ぶんを含む))
```

`redis pool` は「worker 総数 + 隔離の許容ぶん + 余裕」と `10 x GOMAXPROCS` の
大きいほうになる。この構成では前者が 80 なので **8 コア以下ではどのコア数でも
80** で、9 コア以上から `10 x GOMAXPROCS` 側が効く (実測: 9 コアで 90、
16 コアで 160)。

キューまわりは設定ファイルを読んだだけでは効く値が分からないものが多い。
worker 数は既定値がキューごとに違い、`stuck 検出` は**キューごと**・
`handler 期限` は**task type ごと**に対象外があり、実 worker 数の上限は隔離ぶんを含めて設定値を
超えうる ([#2657](https://github.com/shiroha-a/mk/issues/2657) /
[#2658](https://github.com/shiroha-a/mk/issues/2658))。**自分のインスタンスで
実際にいくつになっているか**はここでしか分からない。

パスワード・鍵・proxy の認証情報はマスクされる。設定の有無だけは分かるようにしてある
(未設定と設定済みの区別は診断に要るため)。

## セルフ診断 (doctor)

構成の詰まりを一括で検査する。**サーバーが起動していなくても回せる**ので、新規構築時に
まず走らせると早い。

```bash
./built/misskey -config .config/default.yml -doctor
```

```
  ok    config.url   https://example.com
  ok    database     接続 ok / migration version 97
  ok    root user    meta.rootUserId 設定済み
  ok    redis        接続 ok
  FAIL  webfinger    status 403 (連合が無効)
        インスタンス設定の `federation` が `none` になっている。連合するなら管理画面で有効にする
  ok    nodeinfo     mk-go 1.4.0
  warn  actor        assertionMethod (Ed25519) が無い
        RSA だけでも連合できる。Ed25519 を公開すると対応実装との署名検証が軽くなる
  ok    tls          証明書の残り 68 日
```

FAIL があれば exit 1 を返すので、デプロイスクリプトの検証段に組み込める。
warn は「見ておくべき」であって「壊れている」ではないため exit code には影響しない。

### 何を見ているか

| 検査 | 内容 |
|---|---|
| `config.url` | 絶対 URL か、https か |
| `database` | 接続と、`schema_migrations` が同梱マイグレーションに追いついているか |
| `root user` | `meta.rootUserId` が設定されているか。**未設定だと root 判定が効かず**、`admin/accounts/create` の初回セットアップ判定がローカル利用者数のガードだけに依存する状態になる。指名し直すには DB を直接更新する (`update-meta` は `rootUserId` を受け付けない) |
| `redis` | 接続 |
| `webfinger` | **公開 URL 経由**で `acct:instance.actor@<host>` が引けるか |
| `nodeinfo` | discovery から本体まで辿れるか |
| `actor` | `application/activity+json` で返るか、`publicKey` / `assertionMethod` が載っているか、`id` の host が `url` と一致するか |
| `tls` | 証明書の残り日数 |

**要点は「内部から叩かない」こと。** リバースプロキシの転送漏れは内部からは正常に
見えるので、必ず `url` の外形 URL 経由で確かめる。

管理画面からは `admin/self-check` で同じ検査を実行できる (moderator 以上)。

## Web と配送を別ノードに分ける

既定では 1 プロセスが HTTP とジョブキューの両方を担う。連合配送のバースト負荷を
ユーザー向けレイテンシから切り離したい場合や、配送だけ独立にスケールさせたい場合は、
**環境変数で役割を分けられる** (upstream Misskey と同じ `MK_ONLY_SERVER` /
`MK_ONLY_QUEUE`)。

```bash
# Web ノード: HTTP を提供し、ジョブは積むだけで処理しない
MK_ONLY_SERVER=1 ./built/misskey -config .config/default.yml

# 配送ノード: ジョブを処理する。API は生えない
MK_ONLY_QUEUE=1 ./built/misskey -config .config/default.yml
```

**設定ファイルは両ノードで同じものを使う。** 同じ DB・同じ Redis を向けていることが
前提で、特に `redisForJobQueue` がずれていると Web が積んだジョブを配送ノードが永久に
拾わない。

`redisForPubsub` などの `redisFor*` は**役割の指定ではない**。あれは用途ごとの Redis
接続先で、Web ノードも `redisForJobQueue` を読んで enqueue する。

### 各ノードが担うもの

| 処理 | Web (`MK_ONLY_SERVER`) | 配送 (`MK_ONLY_QUEUE`) |
|---|---|---|
| HTTP API / フロントエンド / WebSocket | ○ | — |
| ジョブの enqueue | ○ | ○ |
| ジョブの処理 (deliver / inbox 等) | — | ○ |
| cron (chart 集計 / 期限切れ prune 等) | — | ○ |
| worker の auto-scale | — | ○ |
| `serverStats` / `queueStats` の配信 | ○ | — |
| chart のメモリバッファ flush | ○ | ○ |

配送ノードは `/healthz` だけ応答する (`enableMetrics` が真なら `/metrics` も)。
これにより `./built/misskey -healthcheck` と Dockerfile の healthcheck が
**どちらのノードでもそのまま使える**。

なお `-healthcheck` は `127.0.0.1:<port>` を叩くので、`socket` で UDS 運用している
場合は role に関係なく使えない (従来からの制限)。

### 注意

- **両方を同時に指定するとエラーで起動しない。** 矛盾した設定を黙って片方優先で
  流すと「配送が動かない」形で後から気付くことになるため
- 管理画面の `serverStats` は **Web ノードのホスト**の値しか出ない。配送ノードの
  CPU / メモリは別途 Prometheus (`enableMetrics`) 等で見る
- 配送ノードを 0 台にすると**ジョブが処理されない**。最低 1 台は必要
- **更新は配送ノードを先に行う。** AP の配送ジョブは署名鍵を payload に載せず
  `signerUserId` から配送時に引く形になっている。Web ノードだけを新しくすると、
  新しい形のジョブを古い配送ノードが受け取り、鍵を取り出せない。**古いノードは
  それを「PEM が壊れている」と判断してリトライせずに 1 回で failed にする**ので、
  更新の間に積まれた配送は自動では戻らない。逆順 (配送ノードが新しく Web
  ノードが古い) は問題ない — 新しい配送ノードは payload に鍵があればそちらを
  使う。**単一プロセス構成では producer と worker が同じなので影響しない**

## systemdユニット例

```ini
[Unit]
Description=mk-go Misskey Backend
After=network.target postgresql.service redis.service

[Service]
Type=simple
User=misskey
WorkingDirectory=/opt/misskey
ExecStart=/opt/misskey/misskey -config /opt/misskey/.config/default.yml
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

## フロントエンド配信

mk-goはMisskeyのSPAフロントエンドをそのまま配信する。フロントエンドは`third_party/misskey`サブモジュールからビルドする。

> **submodule bump 後の追従手順** (= 新 Misskey TS release を取り込んだ PR をマージした後):
> 詳細は [upstream-catch-up.md](./upstream-catch-up.md#1-既存環境への適用--submodule-bump-pr-マージ後) 参照。`git pull` だけでは submodule の working tree は更新されないため、`git pull --recurse-submodules` または `git submodule update --init --recursive` が必要。frontend asset の再ビルドと再起動は `make uds-update` (または `make uds-rebuild && make uds-restart`) がまとめて行う。**`make uds-frontend-build` 単体で止めないこと** — 再起動しないと配信物と HTML がずれて 404 になる。

環境変数でアセットディレクトリを指定:

| 環境変数 | 内容 |
|---|---|
| `MISSKEY_FRONTEND_DIR` | viteビルド出力 (`built/_frontend_vite_`) |
| `MISSKEY_FRONTEND_DIST_DIR` | dist出力 (`built/_frontend_dist_`) |
| `MISSKEY_CLIENT_ASSETS_DIR` | クライアントアセット (`packages/frontend/assets`) |

## 逆プロキシ (nginx)

### TCP構成

```nginx
upstream mkgo {
    server 127.0.0.1:3000;
}

server {
    listen 443 ssl;
    server_name misskey.example.com;

    ssl_certificate     /path/to/cert.pem;
    ssl_certificate_key /path/to/key.pem;

    client_max_body_size 512M;
    proxy_read_timeout 1d;
    proxy_send_timeout 1d;

    # 診断用の /debug/pprof を外に出さない (下記の注意点を参照)。
    location /debug {
        return 404;
    }

    # /metrics も塞ぐ。`enableMetrics` を有効にすると mk-go が**無認証で**
    # 公開する (下記の注意点を参照)。
    location /metrics {
        return 404;
    }

    location / {
        proxy_pass http://mkgo;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;
        proxy_redirect off;
    }
}
```

**注意点:**
- `client_max_body_size`はmk-goの`maxFileSize`設定 (デフォルト250MB) 以上に設定する
- `proxy_read_timeout 1d`はWebSocket (`/streaming`)のために必要
- `Upgrade`/`Connection`ヘッダーはWebSocketパススルーに必要
- **アクセスログにクエリ文字列を出さない。** 同梱フロントは WebSocket を
  `/streaming?i=<トークン>` で開く。`i` はスコープ制限のないネイティブ
  ログイントークンなので、既定の `combined` (= `$request` を含む) のままだと
  **ログを読めるだけでアカウントを乗っ取れる**。`$request` の代わりに
  `$request_method $uri $server_protocol` を使うこと
  (同梱の `deploy/uds/nginx/mkgo.conf` が実例)
- **`error_log` にはクエリが残る。** nginx はエラー行に
  `request: "<リクエスト行>"` を付けるので、アクセスログを直しても
  `/streaming?i=<トークン>` はそちらから出る。レベルを上げても消えない
  (実測では `[crit]`)。収集側でフィルタするか、診断性を捨てて
  `error_log /dev/null;` にすること
- `location /debug`の404は**`location /`が全部を委譲する構成だから**要る。mk-goは
  `enablePprof: true`のときだけ`/debug/pprof/*`を生やすが、有効化は運用者が診断のために
  行う判断であって公開してよいという意味ではない。ここで落としておかないと、
  一時的に有効化した瞬間に外からruntime内部 (goroutine / heap / 実行中のコマンドライン)
  が読める。診断はUDSへ直接 (`curl --unix-socket`) 行う

### 上限を上げられない場合 (分割アップロード)

Cloudflareを経由する構成ではリクエストボディが**100MB (Free/Pro) を超えるとエッジで弾かれる**。mk-goに到達しないため`maxFileSize`をいくら上げても大きいファイルを送れない。

この場合は**分割アップロード**を有効にする。ファイルを一定サイズのチャンクに割って複数リクエストで送り、オブジェクトストレージのマルチパートアップロードで結合する。Cloudflare固有ではなく、`client_max_body_size`を上げられないリバースプロキシ全般に効く。

**オブジェクトストレージ (`useObjectStorage`) が必須。** ローカルストレージ構成では機能ごと無効になり、`/api/meta`の能力告知が出ないのでクライアントは従来の単発アップロードに倒れる。

設定は**コントロールパネル → オブジェクトストレージ**にある。

| 設定 | 既定 | 意味 |
|---|---|---|
| 分割アップロードを有効にする | `false` | インスタンス全体の有効/無効 |
| チャンクサイズ (MiB) | `10` | 5〜32。1リクエストの上限になる |
| セッションの有効期限 (分) | `60` | 5〜1440 |
| ユーザーあたりの同時セッション数 | `8` | ロールポリシーの上限をここで頭打ちにする |
| ユーザーあたりの未完了バイト数 (MiB) | `2048` | 同上 |

チャンクサイズはボディ上限だけでなく**タイムアウト**で決まる。Cloudflareは100秒でタイムアウトするため、上り5 Mbpsでは10 MiBが約17秒に対し100 MBは約160秒でタイムアウトする。既定の10 MiBは、S3の最小パートサイズ (5 MiB) の2倍かつCloudflareのボディ上限の1/10。`client_max_body_size`を上げられない環境ではこれを下げる。

ユーザーごとの可否と上限は**ロールポリシー**でも制御できる (`canUseChunkedUpload` / `chunkedUploadMaxConcurrentSessions` / `chunkedUploadMaxPendingMb`)。上の管理画面設定が上限になるので、ロールに大きい値を入れてもインスタンス設定は超えられない。

**バケット側にライフサイクルルールを設定すること。** オブジェクトストレージは**未完了のマルチパートアップロードにも課金する**。mk-goは期限切れセッションを15分ごとに`AbortMultipartUpload`で回収するが、これが動かない障害時の保険としてバケット側にも「incomplete multipart uploadを N 日で削除」を入れておく。

### UDS構成

```nginx
upstream mkgo {
    server unix:/run/mkgo/mkgo.sock;
}
```

upstream以外の設定はTCP構成と同じ。

## オブジェクトストレージ

**コントロールパネル → オブジェクトストレージ**で設定する。`meta` テーブルに保存されるため設定ファイルの編集も再起動も不要で、保存した時点から次のアップロードに反映される。**ただし UI に出ているのは有効/無効・チャンクサイズ・TTL の 3 つだけ**で、ロール policy の上限になる `chunkedUploadMaxSessionsPerUser` / `chunkedUploadMaxPendingMbPerUser` は `admin/update-meta` を直接呼ぶしかない (#2900 で確認)。

`objectStorageEndpoint` は**ホスト名だけ**を入れる。`https://` などのスキームやバケット名のパスを含めると、mk-go が `https://` を前置してエンドポイント URL を組むため不正な URL になる (本家 Misskey の `S3Service.getS3Client` も同じ組み立て方をする)。

| 項目 | 例 |
|---|---|
| エンドポイント | `s3.us-west-000.backblazeb2.com` / `<accountid>.r2.cloudflarestorage.com` |
| バケット名 | `misskey-drive` |
| Base URL | `https://files.example.com` (公開 URL のベース。スキーム必須) |
| プレフィックス | `files` |

保存されるファイルの公開 URL は `<Base URL>/<プレフィックス>/<アクセスキー>` になる。Base URL 側にプレフィックスを重ねると二重になるので注意。

### 有効化前に保存したファイル

オブジェクトストレージを有効にする前にアップロードされたファイルは、`drive_file.storedInternal = true` としてローカル FS (`./drive-files`) に残る。**これらは移動されない。** mk-go は有効化後もこの列を見て配信元を切り替えるので、既存ファイルはそのまま表示できる。

したがって、有効化したあともローカルの `drive-files` を消してはいけない。まとめてオブジェクトストレージへ移す機能は未提供。

### 無効化に戻す場合

無効化すると、有効化中に保存されたファイル (`storedInternal = false`) はオブジェクトストレージ側にあるままなので、バケットを消すと表示できなくなる。

## TS版からの移行

既存のMisskey (TypeScript版)からの移行手順は[TS版からの移行ガイド](migration-from-ts.md)を参照。

mk-goはTS版と同じPostgreSQL/Redisを共有できるため、バイナリの差し替えだけで移行可能。マイグレーションはTS版テーブルに対して原則追加のみだが、例外が 15 件ある ([TS版からの移行](migration-from-ts.md#破壊的なマイグレーション))。

## アップデート

どの構成でも共通する原則は 3 つ。

1. **`git pull` だけでは submodule が更新されない**。親リポの gitlink ポインタが動くだけで `third_party/misskey/` の実ファイルは古いまま残る。`git pull --recurse-submodules` を使うか、`git config submodule.recurse true` を一度実行しておく
2. **submodule が動いたらフロントエンドを再ビルドする**。SPA のアセットは image に焼き込まず bind-mount で渡しているため、submodule だけ進めても配信物は変わらない
3. **フロントエンドを再ビルドしたら mk-go を再起動する**。エントリポイント (`scripts/<hash>.js`) を起動時に 1 回だけ解決してキャッシュする実装なので、再起動しないと消えた古いファイルを指し続けて 404 になる。bind-mount であっても再起動は必要

**1.3.0 より後へ上げるときは、先に `backfill-remote-host` を流す (#2996)。** リモート
host の読み取り側にあった、非正規化のまま保存された行むけの互換経路を撤去した。流して
いない環境で上げると**非正規化の行が DB から引けなくなる** (`users/show` が呼ばれる
たびに WebFinger を叩く、AP の acct 解決が 404 になる、リモート宛メンションの通知が
飛ばない)。**保存側を正規化したのが 1.3.0 なので、それより古い版から上げる場合ほど
対象行は多い。** 確認方法と手順は[後始末バッチ](#後始末バッチ)の
`backfill-remote-host`。

**`docker compose up -d` は再起動を保証しない。** compose はイメージと設定が変わらなければコンテナを作り直さないが、フロントエンドは bind-mount なのでフロントエンドだけ更新したときは何も変わらない。原則 3 を満たすには `restart` を明示するか、それを行う `make uds-restart` / `make docker-restart` を使う。2026-09-07 にこれで本番のフロントエンドが 10 分近く起動しなくなった (#2885)。

**壊れても CSS では気付けない。** vite のファイル名は内容ハッシュなので、内容が変わらない CSS は再ビルド後も同じ名前で作り直され 200 を返し続ける。判定にはエントリの JS を直接叩く必要がある。`make *-restart` が呼ぶ [`deploy/check-frontend-entry.sh`](../deploy/check-frontend-entry.sh) はそこまで見て、404 なら非ゼロで落ちる。

マイグレーションは構成によって適用方法が違う (下記参照)。golang-migrate が `schema_migrations` で適用済みバージョンを管理するため、何度流しても冪等。

### Docker Compose (TCP / UDS 共通)

```bash
# 本体・submodule・plugins/ の独立リポジトリをまとめて更新し、
# ビルド → 再起動 → 配信アセットの検証まで通す
make uds-update      # UDS 本番構成
make docker-update   # Docker Compose 構成
```

段階的に実行したい場合は分解できる。

```bash
make pull            # 本体 + submodule + プラグイン
make uds-rebuild     # フロントエンド + イメージ (docker 構成では docker-rebuild)
make uds-restart     # 再起動 + 配信アセットの検証 (同 docker-restart)
```

**フロントエンドのビルドは配信中のディレクトリを直接作り直す** (`packages/frontend/build.ts` が出力先を消してから作る)。ビルド開始から再起動完了までフロントエンドは 404 になる。#2885 の事故では、ビルド完了 (13:02:52) から再起動 (13:12:36) まででも **9 分 44 秒**、ビルド開始からならさらに長い窓ができた。**ビルドが失敗した場合はその窓が閉じない** — 配信物が消えたまま残るので、成功するまで直すこと。無停止で入れ替えたい場合は別ディレクトリにビルドして差し替える構成が要る。

> **注意**: `docker-*` 系は `docker-compose.yml` を使うが、このファイルは `name:` を持たないため project 名がディレクトリ名 (`mk`) になり、**UDS 本番と同じ project に合流する**。本番 UDS を動かしているホストでは `uds-*` 系だけを使うこと。

マイグレーションは構成によって適用者が違う。Docker Compose 構成では one-shot の `migrate` サービスが `app` の起動前に自動適用し、`docker compose up -d` が完了した時点で適用済み。UDS 構成には `migrate` サービスが無く、mkgo の entrypoint が起動のたびに流す (したがって再起動には migration の時間が含まれる)。

`.config/docker.yml` を volume mount で使っている場合、**`migrate` 側の mount も忘れずに維持する**こと。片方だけだとマイグレーションと本体が別の DB を見る。

### バイナリ直接実行

```bash
# 本体・submodule・plugins/ の独立リポジトリをまとめて更新する。
# make build は plugins を組み込むので、git pull だけだとプラグインが古いまま焼き込まれる。
make pull

# third_party/misskey が動いていた場合のみ
make e2e-frontend-build

make build

# マイグレーションは手動適用 (compose と違い自動では走らない)
make migrate-up

# 再起動
sudo systemctl restart misskey    # systemd の場合
```

### 切り戻し

`schema_migrations` のバージョンが進んでいるので、バイナリだけ戻すと古い mk-go が新しいスキーマを読むことになる。追加のみのマイグレーション (`ADD COLUMN` / `CREATE TABLE` / `CREATE INDEX`) であれば旧バイナリでも動くが、破壊的な変更を含むリリースでは `make migrate-down` (1 段) を必要な回数繰り返して戻す。リリースノートで破壊的変更の有無を確認すること。

> **`go run ./cmd/migrate -direction down` を本番で叩かないこと。** `-steps` を省くと「全部」の意味になり、全 down マイグレーションが走って 全テーブルが消える。
>
> **down が用意されていても戻せない migration がある。** `000081` は孤児行を、`000082` は chat room の owner が持つ membership / 招待行を DELETE するが、どちらも削除した行の内容を保存していないので down は no-op。詳細は [TS版からの移行](migration-from-ts.md#破壊的なマイグレーション)。

Misskey TS へ戻す場合は[TS版からの移行](migration-from-ts.md)を参照。

## 後始末バッチ

SQL migration として書けない一回限りの正規化は、独立したバイナリで流す。**migration と
違って自動では走らない**ので、運用者が明示的に実行する必要がある。

**稼働中の本体プロセスに影響しない使い捨てコンテナ**で流す。entrypoint を差し替えるのは
`docker-compose.yml` の `migrate` サービスと同じ手法。

### `backfill-avatar-public-url` — アイコン / バナーの URL を公開用へ寄せ直す

`user.avatarUrl` / `user.bannerUrl` は drive ファイルの**原本**を指していた。原本は
アップロードされたバイト列そのままで、`/files/:accessKey` は変換せずに返すため、
撮影情報を含む画像を設定するとそれが載ったまま配られる。`avatarUrl` はタイムライン・
`users/show`・ActivityPub の actor icon に出るので、影響は本人の画面に留まらない。

書き込み側は修正済みだが、**それは以後の更新にしか効かない**。既に設定されている
アイコン / バナーはこのバッチで寄せ直す。指し先は変えず、同じ drive ファイルの
公開用 (`webpublicUrl`) がある場合にそちらへ向け直すだけ。

**公開用が無い行はこのバッチでは直らない。** 多くは「メタデータが無く再エンコード
される理由が無かった」画像で、その場合は原本のままで問題ない。ただし**そうでない
理由で作られなかった行も混ざる**: デコードできなかった画像、保存に失敗した回、
`image/heic` のようにそもそも再エンコードの対象外になっている形式、そして
メタデータの検出を JPEG 以外へ広げる前 (#3044 / commit `9a8ea509` より前) にアップロードされた
2048px 以下の PNG / WebP / TIFF。これらは利用者がアイコンを設定し直す (= drive
ファイルごと入れ替える) 以外に直す手が無い。

**対象はローカル利用者だけ。** upstream はリモートのアイコンを drive に保存して
`avatarId` を書くので、TS から引き継いだ DB にはリモート利用者の古い id が残って
いる。mk-go はその id を更新しないため、host で絞らないと**取り直した現在の
リモート URL を TS 時代のキャッシュへ巻き戻す**。

**アニメーションになりうる形式で公開用を持つ行は触らない。** 公開用はアニメーションを
保てない (1 コマに潰れる) ので、寄せ直すと**いま動いているアイコンをその瞬間に静止画へ
固定する**。実際にアニメーションかはバイト列を読まないと分からないため、
`image/gif` / `image/apng` / `image/webp` / `image/avif` はまとめて対象外にしてある。

**AVIF は全件が対象外になる。** あの形式は寸法にもメタデータにも関係なく必ず公開用が
作られる (`image_processor.go` の `mimeType != "image/avif"` の枝) ので、上の条件の
右辺が常に真になる。静止画の AVIF で撮影情報を持つものも含めて 1 件も寄せ直されない。
WebP は「公開用を持つもの」= メタデータ付きか 2048px 超だけが外れる。

**取りこぼした行は利用者がアイコンを設定し直せば直る — ただし例外がある。**
設定し直すと書き込み経路が走るので、そこでも公開用が選ばれる。**アニメーションの
AVIF は、設定し直すと静止画になる** (書き込み経路のアニメーション判定は ISOBMFF を
歩けないので AVIF を拾えず、しかも AVIF は寸法にもメタデータにも関係なく公開用が
作られる)。AVIF は現状「アニメーションを保つ」と「撮影情報を落とす」を同時に
満たせない。**アニメーション WebP は #3128 で拾えるようになった**ので、
メタデータを持たないものは設定し直しても静止画にならない
(メタデータを持つものは従来どおり除去を優先するので静止画になる)。

**TS から引き継いだ DB では配信サイズが増える。** upstream は `avatarUrl` 列に
プロキシ URL (avatar mode = 高さ 320) を保存するので、移行済みのインスタンスでは
ローカル利用者がその値を持っている。このバッチはそれを「原本と違う」と判定して
公開用 (最大 2048px) へ書き換えるため、48px 表示のアイコンに大きな画像が流れる。
mk-go 生まれの DB では逆に原本 → 公開用なので改善になる。**構成によって向きが
反転する**ので、移行済みなら流す前に `docs/divergence.md` の `user.avatarUrl` の行を
読むこと。

**アイコンとバナーは 1 本の UPDATE にまとまる。** 条件は AND で積まれるので、片方の
id が実行中に変わるともう片方も書けない。冪等なので**もう一度流せば拾える**。

```bash
# まず件数を見る
docker compose run --rm --no-deps --entrypoint /app/backfill-avatar-public-url app \
  -config /app/.config/default.yml -dry-run

# 実行
docker compose run --rm --no-deps --entrypoint /app/backfill-avatar-public-url app \
  -config /app/.config/default.yml -batch 1000 -sleep-ms 200
```

**`--no-deps` を付ける** (理由は下の `backfill-emoji-system-file` と同じ)。UDS 構成では
サービス名が `mkgo` になる。バイナリ直接実行なら
`go run ./cmd/backfill-avatar-public-url -config .config/default.yml -dry-run`。

**無指定で書き込み、`-dry-run` で抑止する**側の作法。`backfill-emoji-system-file` だけが
逆 (既定 dry-run + `-apply`) なので打ち間違えないこと。

出力の `updated` は**「書く必要があった件数」**で、実行中に変わって書けなかった行も
含む。書けたかを厳密に数えたいなら流した後に `-dry-run` をもう一度当てて 0 を確認する。

冪等。中断したら `-from <最後に出た cursor>` で続きから流せる。

**閲覧側のキャッシュはすぐには消えない。** 旧 URL を掴んでいるブラウザや CDN は
そのまま原本を読む。URL 自体が無効になるわけではないので、確実に見せたくない画像は
利用者がアイコンを設定し直す (= drive ファイルごと入れ替える) 必要がある。

**連合先には伝わらない。** `user.avatarUrl` は ActivityPub の actor icon そのもので、
通常のプロフィール更新なら `Update(Person)` がフォロワーへ配送される
(`core/federation/profile_update_delivery_hook.go`)。このバッチは列を直接書くだけで
配送しないので、**リモートのインスタンスは旧 URL を持ち続け、構成によっては原本を
自分の drive にキャッシュし直す**。ブラウザや CDN のキャッシュより寿命が長い。
配送を起こすには利用者が一度プロフィールを更新する必要がある (アイコンを
設定し直せば同時に両方が片付く)。

### `backfill-emoji-system-file` — 承認済み自作絵文字の画像を system 所有へ複製 (#2990)

#2966 より前に承認された `kind = own` のカスタム絵文字申請は、作られた絵文字が
**申請者所有の drive ファイルの URL をそのまま参照している**。申請者がそのファイルを
drive から消すか、アカウントを削除した時点で表示できなくなる。`emoji` テーブルは drive
ファイル ID を持たず `originalUrl` / `publicUrl` の文字列しか持たないので、参照元を
たどって保護する処理は既存に無い。

このバッチは画像を system 所有 (`userId` / `userHost` が NULL) の drive ファイルへ
複製し、絵文字の `originalUrl` / `publicUrl` / `type` を複製側へ向け直す。**複製には
承認経路と同じ処理 (`drive.Service.CopyToSystemFile`) を使う。**

**申請者所有の元ファイルは読むだけで、変更も移動も削除もしない。**
**`emoji_application.fileId` も書き換えない** (申請時に利用者が提出したファイル、という
意味を保つ)。

```bash
# まず分類だけ見る (既定は dry-run)
docker compose run --rm --no-deps --entrypoint /app/backfill-emoji-system-file app \
  -config /app/.config/default.yml

# 実行
docker compose run --rm --no-deps --entrypoint /app/backfill-emoji-system-file app \
  -config /app/.config/default.yml -apply
```

**`--no-deps` を付ける。** `app` は `db` / `redis` / `migrate` に `depends_on` している
ので、stack を落とした状態で叩くと**読み取りだけのつもりで migration まで走る**。

UDS 構成ではサービス名が異なるので `docker compose -f ... run --rm --no-deps
--entrypoint /app/backfill-emoji-system-file mkgo ...` の形になる。バイナリ直接実行なら
`go run ./cmd/backfill-emoji-system-file -config .config/default.yml`。

**保存先の解決はサーバーと同じ。** オブジェクトストレージを有効にしていれば複製もそちら
へ書き、`storedInternal = true` の行 (移行前に保存されたもの) はローカル FS から読む。
ローカルの探索先は既定で `./drive-files` (WORKDIR が `/app` なので `/app/drive-files`) で、
compose の `run` はサービスの volume をそのまま引き継ぐ。別の場所に置いている構成では
`-drive-dir` を渡すこと。

冪等。途中で失敗しても、作れた複製の分だけ進んだ状態から再実行して安全。

**既定は dry-run で、書き込みには `-apply` が要る。** 姉妹バッチ
(`backfill-remote-host` / `backfill-note-tags` / `backfill-avatar-public-url`) は
**逆** (無指定で書き込み、`-dry-run` で抑止) なので、手が覚えているほうで打たないこと。`-dry-run` と `-apply`
を両方渡すと落ちる。

**dry-run で分かるのは DB だけで判定できるところまで。** 実体を読んで初めて分かる
もの (ストレージからの欠落、複製の上限を超える大きさ、複製した実体の MIME が宣言と
違う) は `-apply` で初めて `unrepairable` に変わる。dry-run が `would-copy` と出した
ものが本実行で要対応になることがある。

**複製の URL が `emoji.originalUrl` / `publicUrl` varchar(512) に入らない**形も
dry-run では出ない。こちらは実体とは関係なく保存先の設定だけで決まる
(`<base>(/<prefix>)/<accessKey 32 桁>`) が、**バッチが URL を知るのは複製を作った後**
なので、判定もそこになる。

**exit code が非ゼロなら一覧を読むこと。** 該当するのは 3 種類。

| 分類 | 意味 | 対処 |
|---|---|---|
| `unrepairable` | 再実行しても直らない (申請ファイルが削除済み / リンク行で実体を持たない / `accessKey` が無く実体を辿れない / 実体がストレージから消えている / 複製の上限 32 MiB を超えている / MIME が絵文字として許可されない / **複製の URL が `emoji.originalUrl` / `publicUrl` varchar(512) に入らない**、#3023) | モデレーターが `admin/emoji/update` で別の画像へ差し替える (下記) か、絵文字を削除する。**承認済みの申請は却下できない**。**URL が入らないものだけは差し替えでも直らない** (下記) |
| `needs-review` | 絵文字が申請ファイルを参照しておらず、参照先も system 所有ではない (#3014 より前にモデレーターが `admin/emoji/update` で差し替えた形)。**まだ誰かの drive 操作で壊れうる** | `admin/emoji/update` で画像を差し替え直す (下記)。次の実行では `already` に落ちる。バッチは差し替えを巻き戻さないので触らない |
| `failed` | ストレージや DB の障害で修復に失敗した / 実行中に他の書き込みと競合した | 原因を取り除いて再実行する。**「複製に失敗」がすべて再実行で直るわけではない** — 保存先の URL が長い構成 (下記) では複製の INSERT が `drive_file.thumbnailUrl` varchar(512) で落ちるので、理由に SQLSTATE 22001 が出ているものは再実行しても同じになる。作りかけの複製はバッチが片付ける。**理由にファイル ID が出ているものは残してある** — 片付けに失敗したか、更新が載ったか確認できず消すほうが危険だと判断したかのどちらか。参照されていなければ `admin/drive/cleanup` (手動) が回収する |

**直すのは `admin/emoji/update` での差し替え** (#3014 から差し替え先も system 所有へ複製
する。`add` は #2999、申請の承認は #2966)。**#3014 より前は「削除して同じ名前で登録し直す」
しか無かった**ので、古い手順を覚えている場合は読み替えること。

1. **差し替える画像を用意する。** ふつうは今の画像をそのまま使うので、**先に手元へ
   保存する** — 参照先が申請者など他人の drive ファイルだと、画像を選ぶ画面
   (`drive/files`) には出てこない。**`unrepairable` のうち「実体がもう無い」ものは
   これができない**ので、別の画像を用意するか絵文字を削除する。保存元は
   **`originalUrl`** — `/files/:accessKey` は
   認証不要なのでブラウザで開けば落とせる。`originalUrl` を返すのは
   **`v2/admin/emoji/list`** (と下記の SQL) で、**`admin/emoji/list` の `url` ではない**
   (あちらは webpublic 版 = 再エンコード済みなので、そこから取ると再エンコードを重ねる)
2. 用意した画像を自分の drive へ上げ、管理画面の絵文字編集 (`admin/emoji/update`) で
   その画像へ差し替える
3. 自分の drive へ上げたファイルは消してよい。差し替えた時点で system 所有の複製が作られて
   おり、絵文字は複製のほうを指している。**消す前に管理画面をリロードすること** — 同梱
   frontend は保存直後の一覧に自前で組んだ値 (= 手元のファイルの URL) を表示するので、
   先に消すとリロードするまでプレビューだけ壊れて見える

**差し替えでは何も入れ直さなくてよい。** 絵文字の id も変わらないので、カテゴリ・
エイリアス・ライセンス・センシティブ・ローカル限定・リアクションに使えるロールはそのまま
残り、`emoji_application.emojiId` も生きたままになる (削除して登録し直すと、申請者の画面
(`emoji-application/list-mine`) に**「承認後に絵文字が削除されました」**と出る)。

**差し替える前に指していたファイルは消えない。** 絵文字が指す先が変わるだけで、申請者の
drive にあるファイルはそのまま残る (バッチと同じ判断)。参照が外れた system 所有のファイルは
`admin/drive/cleanup` (手動) が回収する。

**上限を超える画像と、絵文字として許可されない MIME は差し替えでも入らない**
(`unrepairable` の「複製の上限 32 MiB を超えている」「MIME が絵文字として許可されない」)。
前者は小さくした画像を、後者は別の形式の画像を用意する — **複製した実体の型は
`AnalyseFile` が引き直す**ので、同じ画像を上げ直しても `UNSUPPORTED_FILE_TYPE` で弾かれる。

**URL が列に入らないものは、差し替えでは直らない** (`unrepairable` の「複製の URL が
…入らない」、#3023)。オブジェクトストレージの `objectStorageBaseUrl` + prefix が長く、
`<base>(/<prefix>)/<accessKey 32 桁>` が 512 文字を超える構成で起きる。**どの画像を
選んでも URL の長さは同じ**なので、どれを選んでも入らない。返り方だけが分かれる。

- **サムネイルが作られない画像** (decode できないもの。バッチがこの分類に落とした行の
  画像は必ずこちら — decode できるものは複製の時点で落ちて `failed` 側へ行く) は、
  `admin/emoji/update` が 400 (`INVALID_PARAM`) で弾く
- **decode できる画像**は、複製の INSERT が `drive_file.thumbnailUrl` varchar(512) で
  SQLSTATE 22001 になり 500 になる (サムネイルは decode できる画像なら必ず作られる)

直せるのは**保存先の設定を短くすること**だけで、それまでは絵文字を削除するしかない。
`drive_file` 自身の列の食い違い (`url` は varchar(1024) なのに、そこから導く
`thumbnailUrl` / `webpublicUrl` は varchar(512)) が根にあり、そちらは別 issue。

`skipped` だけは exit code に効かない。入るのは「承認後にモデレーターが絵文字を
削除した」形で、守るものがもう無い (**#3014 より前の手順で登録し直した行もここに落ちる** —
申請が指している id の絵文字はもう無いため)。

**このバッチが直すのは申請経由の絵文字だけ。** `admin/emoji/add` は #2999、
`admin/emoji/update` は #3014 で system 所有へ複製するようになったが、**それ以前に登録・
差し替えた絵文字は対象外のまま**。**管理画面が使う経路 (`fileId`) からはもう増えない**が、
`admin/emoji/add` の **`url` 直接指定** (mk-go 独自の escape hatch。「この URL を指す」が
意味なので取り込まない。`docs/divergence.md` §7) に利用者のファイルの URL を渡せば、
同じ形は今でも作れる。該当するかは以下で分かる (読み取りのみ)。

```sql
SELECT DISTINCT e.id, e.name, e."originalUrl"
FROM emoji e
JOIN drive_file df ON df.url = e."originalUrl"
WHERE e.host IS NULL AND NOT (df."userId" IS NULL AND df."userHost" IS NULL);
```

**このバッチを流したあとで実行すること。** 申請経由の行も同じ条件に当たるので、先に
実行すると**バッチが直せる行まで手作業の対象に見える**。出た行の直し方は上と同じ
(`admin/emoji/update` で画像を差し替え直す)。本番では 0 件 (2026-09-15 実測。このバッチが
直した 3 件を含め、ローカル絵文字 21 件すべてが system 所有を参照している)。

**稼働中の本体には即座には伝わらない。** バッチは DB を直接書き、`emojiUpdated` も
publish しない。サーバー側は `emoji` のキャッシュ (5 分) とアバターデコレーションの
絵文字キャッシュ (30 秒)、**ブラウザ側は同梱 frontend が 1 時間**持つ
(`packages/frontend/src/custom-emojis.ts`。IndexedDB、使えない環境では localStorage)。
再起動は要らない。

**壊れはしない** — 古い URL は申請者の元ファイルを指しており、バッチはそれを消さない
ので、キャッシュが切れるまで従来どおり表示できる。**ただし「もう元ファイルを消して
よい」と申請者に伝えるのは急がないこと。** 開いたままのタブはリロードするまで古い URL
を使い続けるので、1 時間はあくまで**新しく開いたブラウザ**が追いつく目安。

**チャートには載らない。** バッチは drive チャートの hook を配線していないので、複製の
ぶんのストレージ使用量はチャートの累積値に加算されない (行の中身は承認経路が作るものと
同じ)。

### `backfill-remote-host` — 保存済みリモート host の punycode 正規化 (#2706)

`hostFromURI` は #2706 から保存時に正規化 (小文字化 + punycode) を掛けるが、それ以前に
取り込んだ行は `Mixed.Example` のような生の表記のまま残る。

**UTS#46 の文字対応付けを入れた版へ上げたときも流すこと。** それより前の正規化は
全角英字・句点類 (U+3002 等)・soft hyphen を畳まなかったので、`https://ｅｖｉｌ.example/`
の actor が `xn--qi7ciaj2b.example` として保存されている (接続先は `evil.example`)。
その行は `blockedHosts` の `evil.example` にも一覧の絞り込みにも当たらない。バッチは
こうした旧形式を `evil.example` へ畳み直す。正当な IDN の行 (`xn--eckve.example` など)
は変わらない。**由来は区別できない** — 最初から ASCII で `xn--qi7ciaj2b.attacker.example`
と書いた URI の行も同じく畳まれ、実行時に URI から作る host と食い違う。書き換わるのは
その xn-- ラベルだけで上位のドメインは変わらないので、他人のドメインへ寄ることは無い。**連合ゲート
(blocked / silenced host) と timeline の instance-mute は完全一致なので取りこぼし**、
acct 解決も #2996 で両当たりを撤去したので引けない。

`lower()` だけでは `パイ.example` → `xn--eckve.example` を作れず、PostgreSQL に IDNA
変換が無いので SQL migration では書けない。

> **1.3.0 より後へ上げる前に必ず流すこと (#2996)。** 読み取り側が非正規化の行にも
> 当てていた互換経路を撤去したので、**流していない環境では非正規化のまま残っている
> 行が DB から引けなくなる**。経路ごとに症状が違う。
>
> | 経路 | 症状 |
> |---|---|
> | `users/show` | **リモートへ WebFinger で問い合わせ直す。** 相手が応答すれば表示できるが、`LookupActorURI` にキャッシュが無いので**呼ばれるたびに外向きのリクエストが飛ぶ** (未認証でも叩ける)。応答が無ければ DB の行には戻らず `FAILED_TO_RESOLVE_REMOTE_USER` (500)。非正規化の行は #2706 より前に取り込んだものなので、既に消滅・改名した相手が混ざりやすい。解決できたときは**行は増えない** (actor URI は変わらないので `FindByURI` が既存行に当たる) |
> | `/@:acct` の AP JSON | `ShowByUsernameDB` が DB だけを見るので **404**。リモートからの acct 解決はここで失敗する |
> | `/@:acct` の HTML | 404 にはならない。素の SPA shell を 200 で返し (upstream も同じ)、OGP の meta だけが落ちる |
> | `/avatar/@:acct` | `/static-assets/user-unknown.png` へ 302。**`Cache-Control: public, max-age=86400` を先に書く**ので、backfill を流しても最大 1 日はブラウザ側で unknown のまま出る |
> | リモート宛メンション | 宛先を引けないので AP の `Mention` タグが付かない。**フォロワーには通常の配送で届くがメンション通知は飛ばず**、フォロワーでない相手には届かない |
>
> 連合ゲート (blocked / silenced host) と instance-mute は元から完全一致なので、
> そちらは #2706 の時点から取りこぼしている。
>
> **確認は `-dry-run` の `updated` が 0 になること。** 0 でなければ本実行してから
> 上げる。本実行で `conflicts` が出た行は**据え置かれる** (同じリモートが表記違いで
> 2 行に増えていて、正規化すると一意制約に当たる) ので、dry-run の `updated` は
> 0 にならないまま残る。その場合は衝突した行を個別に手当てすること — ログに
> `conflict <table>.<column> ...` の形で出る。
>
> **既定ポート付きの行は対象外。** バッチが掛けるのは host 名の正規化だけで、
> `h:443` のような行は `updated` にも `conflicts` にも出てこない。保存側
> (`hostFromURI`) は #2706 で既定ポートを剥がすようになったので、それ以前の行は
> `updated=0` でも引けないまま残る。**`-dry-run` では気付けない**ので、DB を直接
> 見ること (`emoji` / `note`.`userHost` / `following`.`*Host` も同じ形で数えられる):
>
> ```sql
> SELECT count(*) FROM "user" WHERE host LIKE '%:443' OR host LIKE '%:80';
> SELECT count(*) FROM instance WHERE host LIKE '%:443' OR host LIKE '%:80';
> ```
>
> **非既定ポートは対象ではない。** `hostFromURI` は `h:3000` のようなポートを意図的に
> 残す (別 authority なので畳むと連合ゲートを綴りで回避できる)。バッチは
> ポートを変えないので、そういう行は正規形のまま引ける。

```bash
# まず件数を見積もる (書き込まない)
docker compose run --rm --no-deps --entrypoint /app/backfill-remote-host app \
  -config /app/.config/default.yml -dry-run

# 実行。負荷を絞りたければ -batch / -sleep-ms
docker compose run --rm --no-deps --entrypoint /app/backfill-remote-host app \
  -config /app/.config/default.yml -batch 1000 -sleep-ms 200
```

**`--no-deps` を付ける。** `app` は `db` / `redis` / `migrate` に `depends_on` している
ので、stack を落とした状態で叩くと**読み取りだけのつもりで migration まで走る**。

**`--dry-run` では `conflicts` を数えられない** (UPDATE を撃たないため、常に 0)。
衝突は本実行で初めて分かる。

`note` / `drive_file` は行数が多く、`"userHost" IS NOT NULL` に対応する index は
`ORDER BY id` に効かないため、ローカル note が支配的なインスタンスでは batch あたりの
走査行数が膨らむ。`-batch` / `-sleep-ms` で絞ること。

UDS 構成ではサービス名が異なるので `docker compose -f compose.uds.yaml run --rm
--no-deps --entrypoint /app/backfill-remote-host mkgo ...` の形になる (`mkgo` も
`depends_on` に `condition: service_healthy` を持つので `--no-deps` は同じく要る)。
バイナリ直接実行なら
`go run ./cmd/backfill-remote-host -config .config/default.yml -dry-run`。

**流すのは「いま動いている版」のバッチにすること。** 対象の列一覧はリリースごとに
増えるので、**まだ適用していない migration が作る列**を持つ版で流すとそこで落ちる。

```
backfill chat_room.host (cursor=""): ERROR: column "host" does not exist (SQLSTATE 42703)
```

`chat_room.host` は #2994 の `000090` が作る列で、**それを取り込む前の DB には無い**
(1.3.0 のタグ時点では `000083` まで)。
`docker compose run` は現在の image を使うので **pull の前に流せば自然に避けられる**が、
`git pull` 済みのツリーで `go run ./cmd/backfill-remote-host` を叩くと踏む
(自分でビルドしたバイナリを持ち込む場合も同じ)。

**それで取りこぼしは生じない。** 対象一覧はその版の schema をちょうど覆う —
`TestHostColumns_CoversSchema` が「schema にある host 系の列は対象一覧か明示的な除外
一覧のどちらかに必ず入っている」ことを見る。そして**これから適用される migration が
作る列は、その migration 自身が正規形で埋める** (`chat_room.host` は `000090` が
`user.host` 経由で backfill する)。上げる前に検査すべきなのは既にある列だけで、それは
現行版の一覧と一致する。

落ちても**そこまでの列は検査済み**なので、残りだけ流し直せばよい。

冪等なので、途中で失敗しても再実行して安全。`-table` / `-column` で 1 組だけ流せる。

**`conflicts` が出たら手当てが要る。** 同じリモートが表記違いで 2 行に増えている場合、
正規化すると一意制約 (`user` の `(usernameLower, host)` / `instance` の `host` /
`emoji` の `(name, host)`) に当たる。1 行に畳むには `note` / `following` / `drive_file`
など多数の FK を張り替える必要があるため、**このバッチはマージしない**。衝突した行は
`conflict <table>.<column> <key>=... host=... -> ...` の形でログに出るので、それを見て
個別に判断すること。なお衝突で据え置かれた行は、**他テーブルの写しだけが正規化される**
(`user.host` が残っても `note.userHost` は寄る) ので、突合が必要なら合わせて確認する。

**Unicode IDN 表記で `blockedHosts` / `silencedHosts` / `mediaSilencedHosts` /
`mutedInstances` を登録している場合は punycode へ書き換えること。** これらは運用者が
入力する一覧なのでバッチの対象外で、突合側は lowercase 比較しかしない (upstream も
同じ)。保存形が punycode に揃うと、Unicode 表記で登録したブロック / ミュートが効かなく
なる。

## IP 照会の記録と、そこから分かること (#3106)

IP とアカウントの対応を引く機能は、**照会そのものを別のテーブルに記録する**。
運用で問い合わせを受けたときに、どこまで答えられるかを先に把握しておくこと。

記録の対象は 4 本。mk-go 独自の `admin/ip/accounts` (#3104) と
`admin/ip/related-accounts` (#3105) に加えて、**upstream から引き継いだ
`admin/get-user-ips` と `admin/show-user` の `signins` も記録する** — どちらも
返すのは同じ「利用者 ↔ IP の対応」なので、外すと監査を迂回して同じものを引ける。

**`admin/show-user` は記録が増えやすい。** 管理画面の利用者ページは開くたびに
この口を叩き、凍結・サイレンス・ロール変更・メモ保存などの操作のあとにも引き直す。
`canSearchIpHistory` を持つ相手が利用者ページを 1 回開いて 1 操作すると、それだけで
記録が 2 行増える (ログイン履歴が 0 件の利用者でも `resultCount: 0` の行が残る)。
下の「記録の一覧は最新 10,100 件までしか遡れない」と合わせて考えること。

**監査の一覧 (`admin/ip/lookup-log`) を読んだことは記録しない。** 監査ログの閲覧を
監査し続けると際限が無いので切ってあるが、**この応答にも照会に使った IP が並ぶ**
ことは意識しておくこと (権限は照会と同じ 3 段)。

### `admin/show-user` の `signins` も同じ policy で守る (#3114)

**`admin/show-user` が返す `signins` にはログインのたびの IP が入る。** これは
`user_ip` とは別の `signin` テーブルで、**upstream は policy を見ずに全件返す**
(`admin/show-user.ts` は `requireModerator`、`signin` の json-schema に `ip` がある)。
mk-go は `canSearchIpHistory` を持つ相手にだけ返し、**返したときだけ記録する**
(伏せた応答も、DB 障害で引けなかった応答も、開示が起きていないので記録しない)。
upstream からの意図的な逸脱。**ただし揃っているのは policy 段だけで、scope も
レート制限も件数の上限も `admin/ip/*` とは違う** (後述)。

| | `admin/ip/*` | `admin/get-user-ips` | `admin/show-user` の `signins` |
|---|---|---|---|
| 権限 | モデレーター + `canSearchIpHistory` (既定 false) + scope `read:admin:user-ips` | **管理者** + scope | **モデレーター + `canSearchIpHistory`** + scope `read:admin:show-user` (policy が無ければ `ip` と `headers` は空) |
| 監査 | 残る | 残る | **返したときだけ残る** |
| レート制限 | 1 時間 120 回 | 1 時間 120 回 | **無し** |
| `Cache-Control` | `no-store` | `no-store` | 返したときだけ `no-store` |
| 件数 | 1 ページ 100 件 | 最新 30 件 | **全件** |
| `meta.enableIpLogging` が無効 | 新しい観測が止まる | 同左 | **関係なく記録され続ける** |
| 保持期間 | 90 日で刈る | 同左 | **刈らない (無期限)** |

**残っている差は 4 つ** (上の表で `admin/ip/*` と食い違う行を数えた):

1. **レート制限が無い** (`DefaultEndpointLimits` に `admin/show-user` は無い)
2. **件数の上限が無い** (全件返す)
3. **`meta.enableIpLogging` を無効にしても `signin` には記録され続ける**
4. **保持期間が無い** — つまり `user_ip` が 90 日で消えた後も、同じ時期の
   ログイン IP は `signin` に残っている

**scope も別。** `admin/ip/*` は `read:admin:user-ips` を要求するが、こちらは
`read:admin:show-user`。**`read:admin:show-user` だけを与えたアプリトークンでも、
policy さえあればログイン IP が読める。**

`signin` に保持期間を入れるかは別途判断が要る (既存行を消すので不可逆)。

**伏せたことは応答から分からない。** policy が無い相手には `ip: ""` /
`headers: {}` が返るだけで、「記録が無い」との区別が付かない。policy を
確かめられなかったとき (roleService 未配線など) も同じ応答になる。

### 残るもの

| テーブル | 中身 | 保持期間 | 読める人 |
|---|---|---|---|
| `ip_lookup_log` | 誰が・いつ・どの IP / 利用者を・何日ぶん引いて・何件返したか | **90 日** (日次の掃除) | 管理者、または `canSearchIpHistory` を持つモデレーター |
| `user_ip` | 利用者ごとの IP と初回 / 最終観測・観測回数 | **90 日** (日次の掃除) | 同上 (`admin/ip/*`)。`admin/get-user-ips` は**管理者のみ** |
| `moderation_log` | 凍結・削除などの操作 | **無期限** | 管理者 |

**照会の記録は `moderation_log` には入らない。** あちらは保持期間を持たず永久に
残るのに、照会の記録に入るのは**引いた IP そのもの**で、IP とアカウントの対応と
同じだけ機密性がある。`moderation_log` 全体に保持期間を入れると無関係な記録まで
消えるので、専用テーブルを分けてある。

### 残らないもの (記録粒度の制約)

- **照会の結果は残らない。** 残るのは件数だけで、どのアカウントが候補に出たかは
  記録していない。記録すると、この表が第 2 の「IP とアカウントの対応」になる。
  「あのとき誰が候補に出たか」は後から再現するしかなく、`user_ip` の保持期間
  (90 日) を過ぎていれば再現もできない。**件数は「返した数」**で、利用者の行を
  引けずに落とした観測は含まない
- **弾かれた照会は残らない。** パラメータ不正の 400、権限不足の 403、レート制限の
  429 は**結果を返していない = 開示が起きていない**ので記録しない。DB 障害で 500 に
  なった照会も同じ (`admin/ip/related-accounts` はリモート利用者を弾く前に対象の
  利用者を 1 行引くが、IP は引いていない)。**`admin/get-user-ips` だけ失敗の返し方が
  違う** — あちらは upstream から引き継いだ実装で、DB 障害でも 500 ではなく
  **200 + 空配列**を返す (#2792 に反する既存挙動)。開示は起きていないので監査の穴では
  ないが、「空だった」と「引けなかった」が応答から区別できない
- **監査の一覧を読んだことは残らない** (上記)
- **この機能を入れる前の照会は残らない。** `admin/get-user-ips` は #3106 より前から
  あるので、そこでの閲覧は遡れない
- **90 日より前の照会は残らない。** 「照会されていない」ではなく「もう消した」
  なので、混同しないこと (応答の `retentionDays` が同じ値を返す)
- **監査に書けなかった照会は残らない。** 監査の障害で照会を止めはしないので
  (調査そのものが止まる)、DB 障害中の照会は記録が欠ける。サーバーログに
  `ip lookup audit: record failed` が Error で出る
- **`user_ip` 自体が記録していない範囲は引けない。** `meta.enableIpLogging` が
  無効だった期間、凍結済みアカウントの接続、rate limit で 429 になった
  リクエストは観測が残らない (詳細は [divergence.md](divergence.md) の
  「IP 履歴の記録契機」)
- **記録の一覧は最新 10,100 件までしか遡れない** (`offset` の上限 10000 + 1 ページ
  100 件)。保持期間が 90 日なので、**1 日 113 件以上**の照会がある運用では
  「保持期間の中なのに API からは届かない記録」が出る (`ceil(10100 / 90) = 113`。
  112 件/日なら 90 日で 10,080 件で収まる)。絞り込みの条件も持たないので、
  「この IP を誰が引いたか」はページ送りで探すことになる

### 検索結果の読み方

**観測回数は接続回数ではない。** 検索結果の `observationCount` は `user_ip` に
残っている記録の件数で、**同じ (利用者, IP) の観測は約 1 時間に 1 件へ丸めている**
(毎リクエスト書くと書き込みが接続数に比例するため)。同じ IP から 1 時間に 100 回
繋いでも 1 件にしかならないので、**回数の多寡を「よく使っている」の強さとして
読まないこと。** 同じ理由で、この値はランキングの計算に一切入らない。

**同じ IP を使ったことは、同一人物であることを意味しない。** 家庭・会社・学校の
共有回線、携帯の CGNAT、VPN、公衆 Wi-Fi はいずれも多数の利用者で IP を共有する。
画面が出すのは調査の候補とその根拠であって、同一人物である確率ではない。
応答の `ipAccountCount` (その IP を使ったアカウント数) が大きいほど、その一致が
持つ意味は小さい。

**順位は時間で減衰する。** 共有 IP の一致は**半減期 30 日**で重みが下がる
(30 日前の一致は 0.5、60 日前は 0.25)。減衰の起点は「対象と候補のうち**古い側**の
最終観測」で、片方が最近使っていても、もう片方が古ければ古い一致として扱う。
複数の IP で一致したときは、減衰後の重みを足し合わせる。

### 負荷の上限

| 上限 | 値 | 当たると |
|---|---|---|
| レート制限 | 1 時間 120 回 (endpoint ごと。上記 4 本すべて。**利用者単位で数える**) | 429 |
| 起点の IP | 50 本 (最終観測の新しい順) | `targetIpsTruncated` が立つ |
| 1 つの IP から取る候補 | 200 件 | `candidatesTruncated` が立つ |
| DB の `statement_timeout` | 10 秒 (`user_ip` / `ip_lookup_log` を引くクエリ) | 500 (途中までの結果は返さない) |

**打ち切りが立っているときは順位もスコアも下限**で、全部は調べていない。画面は
その旨を出す。`statement_timeout` に当たった照会は結果を返さない — 途中までを
返すと「これで全部」と読まれるため。

**上限が掛かるのは `user_ip` / `ip_lookup_log` を引くクエリだけ。** 同じ handler が
呼ぶ利用者の解決 (`user` の取得) や meta の読み出しは対象外で、`admin/get-user-ips`
はそもそも通らない (1 利用者の最新 30 件を index で引くだけなので上限が要る形では
ない)。レート制限は 4 本とも揃えてある。

**レート制限は利用者単位だけで数える (IP では数えない)。** レート制限は route の
権限検査より**前**に走るので、IP でも数えると **403 になるリクエストでも枠を消費
する** — 同じ出口 IP (CGNAT・社内 NAT・`trustProxy` の誤設定) から未認証で叩き続け
られるだけで、正当なモデレーターの照会が窓のあいだ 429 になる。ここで守りたいのは
「認証済みの利用者による濫用」で、そちらは利用者単位の枠が押さえる。**この 429 は
どこにも記録されない** (`ip_lookup_log` にも残らず、サーバーログにも出ない) ので、
「IP 照会だけが理由不明で使えない」という形にしないための判断でもある。

## 運用上の注意

### admin/overview の federation pie chart が一時的にずれる場合

`instance.followersCount` / `instance.followingCount` は Follow / Unfollow / Block→自動 unfollow に応じて incremental に維持される (#596) が、以下のような **bulk 操作** は incremental hook を経由しないので drift する:

- `admin/delete-account` でアカウントを大量削除 (`followingRepo.DeleteAllByUser` 経路)
- DB を直接操作した場合
- 起動時に並走する race による微小なズレ

drift は起動時の `RecomputeFollowCounts` で完全に再計算されるため、admin dashboard の federation pie chart に違和感が出たら **mk-go プロセスを再起動** すれば即時整合する。再起動以外で recompute を強制する API はまだ無い (将来 admin endpoint 化を検討)。
