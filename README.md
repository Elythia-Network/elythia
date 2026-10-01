# mk-go

Misskey互換のGoバックエンド実装。TypeScript/NestJS製の[Misskey](https://github.com/misskey-dev/misskey)と同一のDB・Redis・フロントエンドを共有し、バックエンドを差し替えられる。

互換バージョン: **Misskey 2026.9.1** (mk-go `1.4.0`)

## 特徴

- Go 1.27 / Echo v4 / GORM + pgx / go-redis v9
- Misskeyフロントエンド(SPA)をそのまま配信
- TypeScript版と同じPostgreSQL/Redisを共有、無停止で移行可能
- ActivityPub連合対応（HTTP Signatures、リモートオブジェクト解決、配信キュー）
- ジョブキューは `mkq` (BullMQ wire-compat)
- Playwright e2e (298 spec ファイル) を PR ごとに実行。upstream 追従時は Misskey TS backend に対しても回して drop-in 互換を検証
- `RemoteStatsFetcher` でリモートユーザーの notesCount / followersCount / followingCount を origin から取得 (mk-go 独自拡張)

## クイックスタート

### 最短: pull して起動 (ビルド不要)

フロントエンドアセットを同梱した `bundled` イメージを使う。ビルドが一切要らない。

動かすだけなら [`docker` ブランチ](https://github.com/shiroha-a/mk/tree/docker)を使う。
compose と設定のひな形だけが入った orphan ブランチで、Go のソースも Misskey の
submodule も含まないため数十 KB で済む (`develop` は `.git` だけで 672MB ある)。

```bash
git clone --depth 1 -b docker https://github.com/shiroha-a/mk.git mk
cd mk

mkdir -p files && sudo chown -R 991:991 files
docker compose up -d

# ブラウザで http://localhost:3000 を開く
```

ソースを持っている場合は同じことを Makefile から実行できる。

```bash
mkdir -p files && sudo chown -R 991:991 files
make image-up
```

設定を変える場合は `.config/docker.yml` を用意し、`docker-compose.image.yml` の `app` と `migrate` 両方の volumes コメントを外す。詳細は[デプロイ](docs/deployment.md)を参照。

### ソースからビルドする (Docker Compose)

```bash
git clone --recursive https://github.com/shiroha-a/mk.git
cd mk
```

### 1. フロントエンドをビルドする (初回のみ、3-10分)

```bash
make e2e-frontend-build
```

SPA の JS/CSS は約 200MB あるため image に焼き込まず、`third_party/misskey/built` を bind-mount で渡している。これを省くとフロントエンドのアセットが 404 になり画面が表示されない。

### 2. 設定ファイルを用意する

```bash
cp .config/docker.yml.example .config/docker.yml
# .config/docker.yml の url を実際のアドレスに変更する
```

`url` は必須項目。`.config/docker.yml.example` の既定値は `http://localhost:3000` なので、そのままでは連合できない (`https://example.tld/` は `default.yml.example` 側の値)。編集したら `docker-compose.yml` の **`app` と `migrate` の両方**にある volumes のコメントを外す。

```yaml
- ./.config/docker.yml:/app/.config/default.yml:ro
```

`migrate` 側にも同じ mount を入れないと、マイグレーションと本体が別の DB を見にいく。

ローカルで試すだけなら、この手順は省略して既定の設定のまま起動してもよい。

### 3. ドライブ用ディレクトリの所有権を設定する

```bash
mkdir -p files && sudo chown -R 991:991 files
```

コンテナは Misskey TS 互換の UID/GID 991 で動くため、この所有権でないとファイルをアップロードできない。

### 4. 起動する

```bash
docker compose up -d
```

DB マイグレーションは one-shot の `migrate` サービスが自動適用し、その完走を待って `app` が起動する。手動で流す必要はない。

ブラウザで `http://localhost:3000` を開き、最初のアカウントを作成する。

より詳しい構成 (UDS、prebuilt image、systemd、逆プロキシ) は[デプロイ](docs/deployment.md)を参照。

## アップデート

```bash
# pull → ビルド → 再起動 → 配信アセットの検証まで通す
make docker-update   # Docker Compose 構成
make uds-update      # UDS 構成
```

注意点:

- **`git pull` だけでは submodule が更新されない**。親リポのポインタが動くだけで `third_party/misskey/` の中身は古いまま。`make pull` (または `git pull --recurse-submodules`) を使う
- **フロントエンドを再ビルドしたら必ず mk-go を再起動する**。エントリポイントを起動時に 1 回だけ解決してキャッシュするため、再起動しないと消えた古いファイルを参照し続けて 404 になる。**`docker compose up -d` では再起動されない** — イメージと設定が変わらなければコンテナは作り直されず、フロントエンドは bind-mount なので何も変わらないため。`make *-update` / `make *-restart` は `restart` を明示したうえで、配信中のアセットが実在するかまで検証する (#2885)
- **ビルド中はフロントエンドが 404 になる**。配信中のディレクトリを作り直すため。ビルドが失敗した場合は 404 のまま残るので、成功するまで直すこと
- ブラウザ側に Service Worker が残っている場合はハードリロードする

バイナリ直接実行や UDS 構成でのアップデート手順は[デプロイ](docs/deployment.md#アップデート)を参照。

## ローカルビルド

前提: Go 1.27+、PostgreSQL 18推奨 (16以降で動作、CI検証は18)、Redis 7+、Docker (テスト用)

```bash
git clone --recursive https://github.com/shiroha-a/mk.git
cd mk

# 設定ファイルを作成 (→ docs/configuration.md 参照)
cp .config/default.yml.example .config/default.yml
# default.yml を環境に合わせて編集

# マイグレーション適用 (接続先は上で編集した default.yml から読む)
make migrate-up

# ビルド & 起動
make build
./built/misskey -config .config/default.yml

# 開発モード (go run)
make dev
```

## テスト

```bash
# 全テスト (PostgreSQL が要る。→ docs/testing.md)
make test

# 特定パッケージ
go test ./internal/api/notes/...

# レース検出 + カバレッジ (CIと同条件)
go test -race -count=1 -shuffle=3 -timeout 10m \
  -coverprofile=coverage.out -covermode=atomic ./...
```

## ドキュメント

| ドキュメント | 内容 |
|---|---|
| [アーキテクチャ](docs/architecture.md) | レイヤ構成、パッケージ責務、DI、フックパターン |
| [API互換性](docs/api-compatibility.md) | Misskey-TSとの互換性状況 |
| [API互換性マトリクス](docs/api-compat.md) | エンドポイント単位の実装状況 (`make apicompat` で自動生成) |
| [差分カタログ](docs/divergence.md) | 純正Misskeyに無い機能・意図的に異なる挙動の一覧 |
| [設定リファレンス](docs/configuration.md) | 全設定項目、環境変数オーバーライド |
| [開発ガイド](docs/development.md) | 環境セットアップ、コーディング規約、CI |
| [テスト](docs/testing.md) | テスト戦略、カバレッジ目標、testcontainers |
| [CI で回る項目](docs/ci.md) | どの check が何を見ていて、落ちたとき何を疑うか |
| [ActivityPub連合](docs/federation.md) | AP実装、HTTP Signatures、配信パイプライン |
| [デプロイ](docs/deployment.md) | Docker/Compose/systemd、逆プロキシ |
| [コントリビューション](docs/contributing.md) | Issue/PR運用、レビュー基準 |
| [TS版からの移行](docs/migration-from-ts.md) | 既存Misskeyからの移行手順 |
| [Playwright](docs/playwright.md) | Playwrightによるフロントエンド / API テスト |
| [Drop-in e2e (pytest)](docs/dropin-e2e.md) | TS-A backend を mk-A に差し替えた state preservation 検証 |
| [Drop-in frontend e2e (cypress)](docs/dropin-frontend-e2e.md) | 3 TS instance + cypress で frontend 視点の互換 |
| [差分比較ハーネス](docs/diff-e2e.md) | mk-go と TS の実APIレスポンスを値レベルでdiff |
| [シェイプドリフト検出](docs/shape-drift.md) | レスポンス形状・エラーID・権限のドリフトを検出する静的ゲート |
| [本家 backend e2e](docs/upstream-backend-e2e.md) | Misskey 本家の `test/e2e/**` を無改変で mk-go に向けて実行する |
| [プラグイン](docs/plugins/) | ビルド時組み込みプラグインの書き方・運用 |
| [プラグイン peer プロトコル](docs/plugin-peer-protocol.md) | mk-go 同士でだけ通じる署名付き HTTP チャネルの wire 仕様 |
| [UDSデプロイ](docs/docker-uds.md) | UNIXドメインソケット構成 |
| [queue-bench](docs/queue-bench.md) | BullMQ / mkq の 2-way 比較 (#563) |
| [ベンチプロファイリング](docs/bench-pprof.md) | k6負荷時のpprof取得と解析 |
| [メディアプロキシの govips 評価](docs/mediaproxy-govips-evaluation.md) | 画像変換ライブラリの比較検討 |
| [upstream追従手順](docs/upstream-catch-up.md) | Misskey TSの新リリース取り込みとsubmodule bump |
| [設計メモ](docs/design/) | オートスケール、inbox verify、mkq等の設計判断 |
| [upstream 差分](docs/update/) | Misskey TS 2026.3.2 → 2026.10.0 の backend 差分 (`<yyyymm><nn>diff.md`) と triage note |

## ライセンス

[GNU AGPL-3.0](LICENSE)
