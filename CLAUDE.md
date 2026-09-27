# CLAUDE.md

このファイルは、このリポジトリで作業する際にClaude Codeが参照するプロジェクト固有のガイドラインです。

本プロジェクトはMisskey（TypeScript/NestJS製の分散型SNS）をGoで書き換えるリライトプロジェクトです。オリジナルMisskeyとのAPI互換性・ActivityPub連合互換性の維持を最優先とします。

タスク管理はGitHub Issues / Pull Requestsで行います（詳細はSection 7）。

## 1. 技術スタック

### コア

| Component | Library | 用途 |
|-----------|---------|------|
| 言語 | **Go 1.27** | `go.mod`でバージョン管理 |
| Webフレームワーク | **Echo v4** (`labstack/echo/v4`) | HTTPルーティング、ミドルウェア、WebSocket |
| ORM | **GORM** (`gorm.io/gorm`) | PostgreSQLアクセス |
| Migration | **golang-migrate** (`golang-migrate/migrate/v4`) | SQLベースのマイグレーション |
| Config | **Viper** (`spf13/viper`) | YAML + 環境変数オーバーライド |
| Logging | **slog** (標準ライブラリ) | 構造化ロギング |

### インフラ

| Component | Library | 用途 |
|-----------|---------|------|
| PostgreSQL Driver | **pgx/v5** (`jackc/pgx/v5`) | PostgreSQL接続 |
| Redis | **go-redis v9** (`redis/go-redis/v9`) | キャッシュ、PubSub |
| Job Queue | **mkq** (`shiroha-a/mkq`) | BullMQ wire互換のRedisジョブキュー。**唯一のdriver** (legacyの`asynq`は#2985で削除) |
| Search | **meilisearch-go** | Meilisearch連携 |
| Object Storage | **aws-sdk-go-v2/s3** | S3互換ストレージ |

### 連合 / ActivityPub

- **HTTP Signatures**: 自前実装（`internal/activitypub/`）
- **JSON-LD**: `piprate/json-gold` (LD-Signature の canonicalize。`internal/activitypub/ld/`)
- **ActivityStreams Types**: カスタム構造体

### 認証

- **bcrypt** (`golang.org/x/crypto/bcrypt`) - パスワードハッシュ
- **pquerna/otp** - TOTP（2FA）
- **go-webauthn/webauthn** - パスキー / セキュリティキー（2FA、`signin-with-passkey`）

### テスト

- **testing** (標準) + **testify** (`stretchr/testify`)
- **testcontainers-go** - 実PostgreSQL/Redisを使った統合テスト
- 単体テストでは`internal/testutil/`のモックを使用

## 2. Project Structure

```
/
├── cmd/
│   ├── misskey/            # メインバイナリのエントリポイント
│   ├── migrate/            # マイグレーションCLIツール
│   ├── backfill-note-tags/ # note.tags を NFKC 正規化し直す一回限りのバッチ
│   ├── backfill-remote-host/ # 保存済みリモート host を punycode 正規化し直すバッチ
│   ├── backfill-emoji-system-file/ # 承認済み自作絵文字の画像を system 所有へ複製し直すバッチ
│   └── backfill-avatar-public-url/ # アイコン / バナーの URL を公開用へ寄せ直すバッチ
├── internal/               # 全26ディレクトリ (`git ls-tree -d HEAD internal/ | wc -l`)
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
│   ├── maintenance/        # SQL migration として書けない後始末バッチ（`cmd/` の CLI から手動で回す）
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
├── test/                   # Go の e2e（`test/e2e` / `test/e2e_federation`）
├── tests/                  # Go 以外の検証基盤（playwright / diff / dropin / bench / upstream-e2e 等）
├── third_party/misskey/    # fork した Misskey TS（submodule。フロントエンドの供給元）
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
└── go.mod                  # Moduleパス: github.com/shiroha-a/mk
```

`built/` と `drive-files/` は gitignored な生成物 / ローカルストレージ。

レイヤ責務：
- **api** → **core** → **repository** → **model** の順に依存。逆向きの依存は禁止。
- **entity**はレスポンス変換専用。ドメインロジックを入れない。
- **activitypub**は`core`から呼び出され、連合処理を担う。

## 3. Development Commands

すべて`Makefile`経由で実行できます。

```bash
# ビルド
make build                  # ./built/misskey に実行ファイル生成
make dev                    # go run で直接起動（開発用）
make run                    # build + 実行

# 依存管理
make tidy                   # go mod tidy。**このリポジトリでは private plugin の解決に
                            # 失敗するので使えない**。依存追加は go get、go.sum の検証は
                            # GOWORK=off go build

# コード品質
make fmt                    # gofmt -s -w . で整形
make lint                   # go vet ./...
make check                  # コミット前に必須 (fmt → lint → actionlint → golangci-lint → test)

# テスト
make test                   # go test ./... -v -race -count=1 -shuffle=3 (CI と同じ**テスト実行**条件)
make test-fast              # -race 抜き (反復用)。**コミット前の検査ではない**
make plugin-test            # 同梱プラグインのテスト (別 module なので ./... に含まれない)
make plugin-doc-check       # docs/plugins/authoring.md の Go スニペットがコンパイルできるか

# 静的 parity ゲート (サーバー / ブラウザ / Docker 不要)
make gates                  # shapecheck / errorid-check / limitspec-check / perm-check / wiring-check / catalog-check / notfound-check / nulparam-check / compose-check / testflags-check / migrationdoc-check / mdtable-check / notiftype-check / pluginembed-check / dockerignore-check / secretfield-check / ipshape-check / iprecord-check / sqlbind-check / submodulepin-check / gaterun-check を一括
make apicompat              # docs/api-compat.md を生成 (route dump に stack 起動が必要)

# プラグインの組み込み
make plugins                # plugins/ を走査して生成 (make build が内部で呼ぶ)
make plugins-all            # disabled のものも含める (CI 検証用)
make plugin-dev             # 編集しながら動かす (PLUGIN=plugins/status)

# 更新 (運用)
make pull                   # 本体 + submodule + plugins/ の独立リポジトリを一括 pull
make uds-update             # pull → ビルド → 再起動 → 配信 entry の検証 (UDS 本番)
make docker-update          # 同上 (Docker Compose 構成)
make uds-restart            # mkgo を再起動して配信 entry を検証だけする
                            # **`up -d` は再起動を保証しない** — frontend は bind mount
                            # なので frontend だけ更新すると recreate されず、mk-go が
                            # 起動時にキャッシュした古い entry を配り続ける (#2885)

# マイグレーション（接続先は -config、既定 .config/default.yml から決まる）
make migrate-up             # 最新まで適用
make migrate-down           # 1段階ロールバック (-steps 1)
go run ./cmd/migrate -direction down   # 全段ロールバック (破壊的。全テーブルが消える)
make migrate-create         # 新規マイグレーションファイル作成（プロンプト対話）

# Docker
make docker-build
make docker-up              # docker compose up -d
make docker-down

# Drop-in e2e (#364 / #365) — Misskey TS 2 インスタンスを立ち上げて
# TS ↔ mk 切替互換性を検証する基盤。詳細は docs/dropin-e2e.md。
make dropin-up              # TS-A / TS-B stack 起動
make dropin-test            # pytest smoke test 実行
make dropin-down            # stack + volume 全削除

# Drop-in mk overlay + swap test (#367) — instance A の backend を mk-go に
# 差し替える e2e シナリオ。
make dropin-mk-up           # base + mk overlay (clean DB から mk-A 起動)
make dropin-mk-test         # mk-A に対する smoke test
make dropin-mk-down         # cleanup
make dropin-swap-test       # TS-then-mk 切替シナリオ (bash orchestrator)

# Drop-in fedibird-mock e2e (#1083) — Fedibird-like ActivityPub mock との
# 双方向 Ed25519 verify を検証する e2e。
make dropin-fedibird-test    # mock ↔ mk-A の Ed25519 inbound/outbound 検証

# 本家 backend e2e (#2347) — Misskey 本家の test/e2e/** をそのまま mk-go に
# 向けて実行する。テスト本体は無改変。詳細は docs/upstream-backend-e2e.md。
make upstream-e2e-deps       # submodule 側の依存を用意 (初回 / submodule bump 後)
make upstream-e2e-up         # e2e 用 PostgreSQL / Redis を起動
make upstream-e2e-migrate    # e2e 用 DB にマイグレーションを適用
make upstream-e2e-test       # mk-go をビルドして vitest を実行 (FILE= で 1 ファイル指定可)
make upstream-e2e            # 上記 4 つを一括実行
make upstream-e2e-down       # volume ごと撤去

# Drop-in frontend e2e (#380 / Phase 14) — 3 Misskey TS インスタンス + cypress
# 実ブラウザでフロントエンド視点の drop-in 互換を検証する基盤。
make dropin-frontend-baseline    # TS-A/B/C + cypress baseline spec 実行
make dropin-frontend-up          # stack だけ立ち上げ (手動デバッグ用)
make dropin-frontend-down        # volume ごと cleanup
make dropin-frontend-swap-test   # TS-A → mk-A 切替まで含む end-to-end (Phase 14-3)
make dropin-frontend-mk-up       # mk overlay だけ立ち上げ (clean DB の mk-A から起動)
make dropin-frontend-mk-down     # mk overlay cleanup

# その他の e2e / 検証
make dropin-mkgo-born-test   # mk-go 生まれの DB を TS に引き渡せるか (#2383)
make federation-misskey-e2e  # 本物の Misskey TS との実連合を起動から撤去まで通しで (#2362)
make diff-check              # mk-go と TS のレスポンスを値レベルで diff (#2078)
make playwright-check        # Playwright を作り直して実行
make frontend-check          # fork frontend の型チェック + submodule 依存のゲート + eslint
make frontend-lint           # eslint だけ (CI と同じ範囲、実測 55 秒)
make e2e-down-all            # 検証用スタックを一括撤去 (**本番 project `mk` は対象外**)
```

**上記は全体ではない。** `make help` が全 139 target を出す (`^名前:.*##` の行を数えた)。一覧と説明は
[docs/development.md](docs/development.md)、CI 上の対応は [docs/ci.md](docs/ci.md)。

エントリポイント：
- メインサーバー: `./cmd/misskey -config .config/default.yml`
- マイグレーション: `./cmd/migrate -direction up`

## 4. Testing

### 基本方針

- 新規機能追加時は**必ずテストを追加**する。
- CIでは**パッケージごとにカバレッジ閾値**を強制する。原則は以下だが、例外パッケージは個別に緩和閾値を設けている：
  - **最低ライン: 90%** — CIゲート。これを下回るとマージ不可。
  - **推奨ライン: 95%** — 通常のPRではここを目指す。
  - **目標ライン: 100%** — 新規パッケージや小規模パッケージでは積極的に狙う。
  - 例外パッケージ：
    - `internal/api/admin`: 80%以上 — `handler_stubs.go`にSMTP/queue/DB集計等の外部依存が多く90%未到達。現状83.8%で小マージン確保のため80%にロック
    - `internal/testutil`: 0% — mock/test helper専用パッケージ。production codeではなく他テストから呼ばれるだけなのでe2eと同様に閾値対象外
    - `internal/server`: 0% — 大部分が`router.go`のwire層 (handler配線/middleware設定) で、e2e/drop-in test経由で実挙動検証する設計。個別handlerファイル (`avatar.go` / `identicon.go`等) は`_test.go`単体で90%相当をカバーする運用は維持するが、`router.go`のウェイトでpackage全体が数%に張り付くためe2eと同様に閾値対象外 (#462)
- テストファイルは対象と同じパッケージに`_test.go`サフィックスで配置。

### 実行方法

```bash
# 全テスト実行（CI と同じテスト実行条件: -race -count=1 -shuffle=3）
# **カバレッジ閾値の検査は再現しない** (CI は別 step)。下の -coverprofile 付きを使う
make test

# -race 抜きで速く回す（反復用）。**コミット前は make check を使うこと**
make test-fast

# 特定パッケージ
go test ./internal/api/notes/...

# レース検出 + カバレッジ（CIと同じ条件）
go test -race -count=1 -shuffle=3 -timeout 10m \
  -coverprofile=coverage.out -covermode=atomic ./...

# カバレッジ閲覧
go tool cover -html=coverage.out
```

### 統合テスト

- **手元には PostgreSQL が要る**。既定は `localhost:5432` の `misskey_test` に `mk` / `mk`。違う接続先を使うときだけ `cp .env.test.example .env.test` して編集する (`internal/testutil` が接続時に読み、設定済みの環境変数は上書きしない)。
- DB を使うテストの主流は `testutil.OpenTestDB` / `MustOpenTestDB` で、**外部の PostgreSQL に直接つなぐ**。`MustOpenTestDB` は失敗時 panic。
- **testcontainers は Redis 用**。`SetupRedis` は 27 パッケージが使うが、`SetupPostgres` は `internal/api/test` / `test/e2e` / `test/e2e_federation` の 3 つだけ。**PostgreSQL は「Docker があれば準備不要」ではない。**
- ローカル実行にはDocker環境が必要。
- CIではGitHub Actionsの`services`でPostgreSQL 18 / Redis 7を起動し、以下の環境変数でDBへ接続する (Redisを要するテストはCIでもtestcontainersを立てる)：
  - `TEST_DB_HOST`, `TEST_DB_PORT`, `TEST_DB_NAME`, `TEST_DB_USER`, `TEST_DB_PASS`, `TEST_DB_SSLMODE`
  - `TEST_REDIS_HOST`, `TEST_REDIS_PORT`

### DB を使うテストの分離 (#2450)

`testutil.OpenTestDB` / `MustOpenTestDB` は**呼び出し元のパッケージ専用の PostgreSQL
schema** に接続する (`internal/api/gallery` → `internal_api_gallery`)。schema 名は
呼び出し元から自動で決まるので、新しいパッケージも何もしなくても隔離される。

`go test` は**パッケージのテストバイナリを並行実行する**。CI は shard ごとに
PostgreSQL を 1 つしか立てないため、共有すると一方の後片付けが他方の前提を壊す。
実際に `internal/charttick` の `DELETE FROM "user"` が `internal/api/gallery` の
所有者 user を消し、**Go を一切触っていない PR で CI が落ちた**。

削除範囲を絞るだけでは解けない。charttick は**テーブル全体の絶対件数**を
アサートするので、絞ると今度は他パッケージの行が混ざって charttick 自身が落ちる。
干渉は双方向。shard 分配は `go list` 順の `NR % 4` なので、テストパッケージを 1 つ
足すだけで同居の組み合わせが変わる。個別の衝突を潰す対処では再発する。

守ること：

- **DB を読み書きするテストで `OpenSharedTestDB` を使わない。** これは
  `internal/db` のように接続処理そのものを試すテスト専用
- schema が分かれているので `DELETE FROM "user"` のような無条件の削除は書いてよい。
  ただし**それは自分の schema に閉じている前提**に依存するので、
  `search_path` を跨ぐ生 SQL (`public.` 明示など) を書かない
- **システムカタログも `search_path` に従わない (#2777)。** 参照は `pg_catalog` で
  解決されるが、**返る行は全 schema 分**。必ず自分の schema に絞る:
  `pg_indexes` は `schemaname = current_schema()`、`information_schema.columns` /
  `.tables` は `table_schema = current_schema()` (このリポジトリで最も多いのは
  こちら)、`pg_class` は `pg_namespace` を join して `n.nspname = current_schema()`
  (`pg_class` は schema を oid で持ち `schemaname` 列が無い。`pg_attribute` は
  relation の oid しか持たないので `pg_class` 経由の 2 段 join になる)。
  **`information_schema.schemata` は対象外** — schema の一覧そのものなので絞る
  概念が無い。絞らないと 2 つ壊れる — (a) 他 schema の同名
  オブジェクトを自分のものと取り違えて regression guard が空振りし、(b) 他
  パッケージの `ApplyMigrations` が DDL 中だと
  `could not open relation with OID (SQLSTATE XX000)` で落ちる。**CI でも起きる** —
  shard は PostgreSQL を 1 つしか立てないので手元と同じ条件が揃い、required check の
  `test` が不定期に赤くなる。
- **複数行が返りうるクエリを `Scan(&string)` で受けない (#2777)。** GORM は `*string` に対し**全行を走査して
  dest を上書きし続ける**ので、複数行が返ると**最後の 1 行**が残る。実測では
  `pg_indexes` の絞りを外すと 17 件中 17 番目 (`internal_repository_ts`) の定義が
  返り、**それでもテストが緑のまま通っていた** — 上の (a) の実例。slice で受けて
  件数と schema 名を確かめる (`internal/repository/index_lookup_test.go` の
  `indexDef` が例)
- 行の投入は**戻り値を検査する** (`require.NoError(t, db.Create(x).Error)`)。
  捨てると FK 違反が黙って流れ、「200 のはずが 400」のような原因から遠い症状に化ける

migration で enum を作るときは `EXCEPTION WHEN duplicate_object THEN NULL` を使う。
`pg_type WHERE typname = ...` は **schema を見ない**ため、別 schema に同名の型が
あるだけで作成を飛ばし、直後の `CREATE TABLE` が落ちる。

**列枠を食う操作を書かない (#2756)。** PostgreSQL は `DROP COLUMN` した列も
1 テーブル 1600 列の上限に数える。手元の schema は実行をまたいで残るので、
テストのたびに列を落とす形にすると枠が減り続け、最後は
`tables can have at most 1600 columns (SQLSTATE 54011)` で落ちる。CI は毎回
クリーンな DB を立てるので**手元で繰り返す開発者だけが踏む**。

- `ApplyMigrations` は適用済みの migration を skip する (`testutil_applied_migrations`
  台帳。ファイル名 + 内容の sha256 で持つので、migration を書き換えれば流し直す。
  失敗したものは記録しないので次回また流す)
- schema の形を変えて試すテストは `testutil.OpenTestDBSchema("<suffix>")` で
  **専用の兄弟 schema** を作り、そこを一度だけその形にして使い回す
  (`internal/repository/dropin_ts_schema_test.go` が例)
- 復旧は schema を作り直すしかない (`ALTER TABLE ... ADD COLUMN` では枠は戻らない):
  `psql ... -c 'DROP SCHEMA "internal_repository" CASCADE'` と**兄弟 schema**
  (`internal_repository_ts`)。次の実行が作り直す (全 migration の適用は実測
  1-3.5 秒)。**テストが途中で死んで schema が壊れたときも同じ手順** — 台帳が
  入ったことで「毎回全部流し直して勝手に直る」挙動は無くなった (ただし
  `000001_initial` が作るものは元から戻らない。詳細は docs/testing.md)

### モック

- `internal/testutil/`配下にRepository、Drive、Block/Muteなどのモック実装がある。
- 単体テストではモックを使い、統合テストでは実DBを使う。DBをモックしないこと。

## 5. Coding Style

### 基本

- **gofmt**（`gofmt -s -w .`）で整形すること。CIで`gofmt -s -d .`による差分チェックが走る。
- **go vet**を通すこと。CIで強制。
- 命名はGoの標準慣習に従う（`camelCase`/`PascalCase`、略語は全て大文字：`URL`, `ID`, `API`）。
- Early returnを優先し、ネストを浅く保つ。
- エラーは`fmt.Errorf("context: %w", err)`でラップする。

### コメントとドキュメント

ユーザーグローバルルール（`~/.claude/CLAUDE.md`）に準拠：

- **英語で書くもの**：
  - GoDoc（関数/型/パッケージのドキュメンテーションコメント）
  - テストケースの`name`フィールド等、コード内のメタ情報
- **日本語で書くもの**：
  - 実装の背景・理由を説明する**インラインコメント**（なぜこの設計か、どんな罠があるか）
- **書かない**：
  - 自明な処理の説明コメント
  - `// TODO`や`// XXX`の乱用
  - 絵文字（全面禁止）

例：

```go
// CreateNote persists a new note and publishes events to subscribers.
// Returns ErrNoteSizeExceeded if content exceeds the configured limit.
func (s *Service) CreateNote(ctx context.Context, input CreateInput) (*model.Note, error) {
    // Misskeyオリジナル実装では空文字列も許容されるが、
    // ファイル添付もない場合は投稿として無効なためここで弾く
    if input.Text == "" && len(input.FileIDs) == 0 {
        return nil, ErrEmptyNote
    }
    ...
}
```

### 日本語の書式

- 日本語の中では不要な半角スペースを入れない。
  - ◯ `Claude Code入門`
  - × `Claude Code 入門`

## 6. Key Conventions

### Misskey互換性

- **API互換性が最優先**。レスポンスのフィールド名・型・エラーコードはオリジナルMisskeyと一致させる。
- バージョン文字列は`internal/config/config.go`の`MisskeyVersion` / `MkGoVersion`定数で管理し、対応するMisskeyバージョンに合わせる（現在: `MisskeyVersion=2026.9.1` / `MkGoVersion=1.4.0`）。
- User-Agentは`mk-go/<version> (<url>)`形式 (#774 で `Misskey-Go/<ver>` から rename)。

### ID生成

- デフォルトIDジェネレータは`aidx`（設定ファイルで指定）。
- `internal/misc/id/`のジェネレータを使用し、モデルから直接`uuid`を呼ばない。

### エラーハンドリング

- APIレスポンスのエラーはMisskey互換のエラーコード・IDを返す（例: `NO_SUCH_NOTE`, 特定UUID）。
- 内部エラーは`slog`で構造化ログに記録、ユーザーには汎用メッセージを返す。

### Redisインスタンス分離

Misskeyは用途別に複数のRedis接続を持つ（`default`, `pubsub`, `jobQueue`, `timelines`, `reactions`）。設定で同じエンドポイントに向けられていても、コード上は用途ごとに別クライアントとして扱うこと。

### ActivityPub

- すべての送信リクエストにHTTP Signatureを付与する。
- リモートオブジェクト取得は`internal/activitypub/resolver.go`経由で行い、キャッシュを活用する。
- `allowedPrivateNetworks`設定を尊重し、プライベートIPへの直接アクセスを防ぐ。

## 7. Git Workflow

複数人での開発を前提とし、タスク管理はGitHub Issues、実装の取り込みはPull Requestで行う。

- **Issue・PRのタイトルおよび本文は日本語で記述することを厳守する**。コード識別子・エラーコード・ファイルパス・コマンド等の技術用語は原文のまま残してよいが、説明文・見出し・箇条書きの地の文は日本語で書く（英語の本文・見出しを混在させない）。
- **プロジェクトの`CHANGELOG.md`はリリース時にまとめて記述する**。個別のPR・fixごとに`## Unreleased`へ追記せず、リリースのタイミングで該当期間の変更を一括で記載する。

### Issue駆動ワークフロー

すべての作業は**対応するissueを先に作成**してから着手する。

- **Issueタイトル形式**: `Phase〇 <内容>`
  - 例: `Phase 10 管理機能`
- **Phaseが複数のサブフェーズに分かれる場合**、サブフェーズごとに個別のissueを立てる。
  - 例: Phase 10が4段階に分かれるなら、`Phase 10-1 <内容>`, `Phase 10-2 <内容>`, `Phase 10-3 <内容>`, `Phase 10-4 <内容>`の4つを作成する。
- **Issue本文**に含める項目：
  - 背景・目的
  - 実装する機能の詳細（作業内容を細かく記述）
  - 影響範囲
  - 完了条件（チェックリスト推奨）
  - 関連する設計ドキュメント・issueへの参照

Issueの作成・操作には`gh`コマンドを使う（`gh issue create`, `gh issue list`等）。

### ブランチ戦略

- `main`: リリースブランチ
- `develop`: 開発ブランチ。フィーチャーブランチのマージ先
- 作業はissueごとに**フィーチャーブランチ**を切って行う
  - ブランチ名例: `feature/phase-10-1-<要約>` / `fix/<対象>-<要約>`
- リモート破壊的操作（`push --force`、`reset --hard`など）は明示的な指示がない限り実行しない

### ドキュメントを直すときのレビュー条件

**doc の誤りを直す作業は、直した先で新しい誤りを作りやすい。** #2640 では敵対的
レビューを 7 周回して、**毎周の High がすべて「直前の修正が作った回帰」**だった。
出た型と確認手順は
[コントリビューション](docs/contributing.md#ドキュメントを直すときのレビュー条件)
にまとめてある。要点だけ:

- **直したら固有の語で `git grep` する** (最多の型。同じ主張が別の場所に残る。
  ディレクトリを列挙すると `Makefile` や `.github/` を落とす)
- **数値・識別子・パス・ログ行・設定の効き方は実行または grep で確かめる。** 推論で書かない
- **「upstream と同じ」「対応済み」と書く前に `docs/divergence.md` とコードコメントの
  既知乖離を見る。** 挙動を書き換えるなら、それを固定しているテストが無いか先に読む
- **数を書くなら数え方も書く** (同じ対象が数え方で 52 / 55 / 63 になる)
- **wire 上の名前とソースのファイル名を区別する** (stream チャンネルの一覧を
  ファイル名から作り、18 件中 11 件が実在しない名前になった)
- **直した結果が元より危険側になっていないか** を「読んだ人が何をするか」で比べる
- **生成物 (`docs/api-compat.md` 等) を手で直さない。** gate を足したら変異させて
  落ちることを確認する

### コミット

- コミット前には`make check`を通すこと（fmt → lint → actionlint → golangci-lint → test）
- Claudeは**コミットを自動作成しない**。ユーザーが明示的に指示した場合のみコミットを作成する
- コミットメッセージは既存の履歴に倣う（例: `Phase 9.2: Remote ActivityPub object resolution`、`Fix CI: twofactor coverage 80% -> 100%`）
- Phase単位の機能追加は`Phase N.M: <要約>`、修正は`Fix <対象>: <要約>`の形式が一般的

### Pull Request

- 実装が完了したらPRを作成し、**必ず対応するissueをcloseする**
  - PR本文に`Closes #<issue番号>`を記載すると、マージ時にissueが自動closeされる
- タイトル・本文フォーマット：
  - **タイトル**: `Phase〇 <内容>` または作業の簡潔な要約
  - **Summary**: 変更の概要と目的
  - **主な変更点**: 変更ファイルの要約、注意点
  - **テスト**: 通ったテスト、追加したテスト、実行方法
  - **Closes**: `Closes #<issue番号>`
  - **その他**: 特記事項
- PR作成は`gh pr create`を使う

#### マージ方法

フィーチャーブランチ → `develop`のPRは**rebase and merge**でマージする（`gh pr merge <N> --rebase --delete-branch`）。

rebase and mergeでは**PRの各コミットがそのまま`develop`の履歴に載る**。したがって：

- コミットは**1つずつビルド・テストが通る順序**で並べる（依存するAPI追加を先、それを使う配線を後）。壊れたコミットが履歴に残ると`git bisect`が効かなくなる
- 確認は使い捨ての`git worktree`を作って各コミットをcheckout→buildするのが安全。作業ツリー上で`git stash`を回す方法は、保留中の別作業を巻き込むので使わない
- 「機能追加 + 無関係なリファクタ」を1コミットに混ぜない（1 PR単位の原則をコミット単位にも適用する）

`main`は**PRをマージしない**（`develop`からのFF pushのみ）。`main`でrebase mergeを使うとSHAが分岐してリリースタグが履歴に乗らなくなるため、こちらの方針とは対象が異なる。

## 8. CI/CD

`.github/workflows/ci.yml`で以下のジョブが`main`と`develop`への push/PR で実行されます。

### `build`ジョブ

checkout / setup-go を除くと step は実行順に 3 つ。**required job なので、コンパイル以外の理由でも赤くなる。**

- `go build ./...`で全パッケージのビルド確認。
- **`Check bundled plugins are disabled by default` step** で、tracked な
  `plugins/*/mk-plugin.yml` が全て `disabled: true` を持つことを見る (#2701)。
  **検証のために一時的に外して戻し忘れる**のを止めるため (trustlevel が実際に
  そうなっていた)。判定は `git ls-files` + grep だけで完結させてある —
  `pluginbuild` に読ませるほうが parser 一致で厳密だが、`pluginbuild` は git では
  なく**ディレクトリ**を走査するので、`plugins/` に自前プラグインを置いている
  手元では誤検知する。手元の再現は `make plugin-vet`。
- **`Vet bundled plugins` step** で同梱プラグインを `go vet` する。`go build` ではなく
  `vet` なのは、テストファイルもコンパイルされるので**公開面を変えて本体だけ直した**
  ときに検出できるため (#2588)。列挙は `git ls-files` なので、新しく同梱した
  ものも自動で対象になる。

### `test-shards`ジョブ + `test` aggregator

- **4-way matrix shard** で並列実行する `test-shards` (`shard: [1,2,3,4]`)。各shardは
  独立したPostgreSQL 18 Alpine / Redis 7 Alpine サービスコンテナを持つ。
- テスト対象は`go list`で絞り込み（テストファイルがあるパッケージのみ）した上で
  `awk 'NF'`で空行除外→ImportPath順にソート→`NR % 4`で各shardに均等割り当て。
  新規パッケージ追加でshard内の構成が変わっても、決定的な分配により再現性は保たれる。
- 実行条件: `-race -count=1 -shuffle=3 -timeout 10m -coverprofile=coverage-shard-N.out -covermode=atomic`。
  **`make test` と揃っていること**を `make testflags-check` が検査する (#2841)
- **`-shuffle` の seed は全 shard 共通の固定値にする (#2795)。** `on` (毎回ランダム) は
  失敗を手元で再現できず、required check の `test` が不定期に赤くなる。**shard 番号も
  使わない** — shard 配属は `NR % 4` なので、テストパッケージが 1 つ増えるだけで既存
  パッケージの seed が変わり、順序が丸ごと入れ替わる (無関係な PR が未実行の順序を
  引いて赤くなる)。1 パッケージが試す順序は 1 通りなので、`-shuffle` だけで全ての
  順序依存が見つかるわけではない。
  **プロセス共有の状態を張り替えて戻さないテストがここで落ちる** — `internal/server` は
  `newServer` / `New` がグローバルを 12 個差し替えており、戻さないまま後続の
  `avatar` / `emoji_redirect` が署名付きプロキシ URL を受け取って落ちていた。
- **カバレッジ閾値チェック** (各shard内で実行)：
  - `internal/api/admin`配下: 80%以上（SMTP/queue/DB集計等の外部依存で90%未到達のため暫定緩和）
  - `e2e`配下: 0%
  - `internal/testutil`: 0%（mock/test helper専用、production codeを含まないためe2eと同様扱い）
  - `internal/server`: 0%（router.goのwire層中心、e2e/drop-in test経由で実挙動検証する設計のため。個別handlerは`_test.go`で個別カバー）
  - それ以外のパッケージ: 90%以上
  - shard内のいずれかのパッケージが閾値未達なら、そのshardが失敗する。
- カバレッジレポートは`coverage-shard-N`アーティファクトとして各shardからアップロード。
- `test` job は `needs: test-shards / if: always()` で全shardを束ね、ブランチ保護が
  要求する `test` という名前の単一checkを公開する。いずれかのshardが失敗したら
  `needs.test-shards.result != 'success'` で `exit 1`。

### `plugin-tests`ジョブ

- 同梱プラグイン (`plugins/*/go.mod` のうち git tracked なもの) のテストを実行する (#2588)。
- プラグインは**別 module** なので `go list ./...` に含まれず `test-shards` の対象に
  ならない。実行時間が短いため shard の分配ロジックに手を入れず独立させている。
- **`MK_PLUGIN_TESTS_REQUIRE_DB` を渡すのが要点。** テストは手元で PostgreSQL を
  用意していない開発者のために接続不能を skip するが、**skip は成功として扱われる**
  ので CI でそのままだと接続に失敗しても緑になる (= 無検証で通る)。この変数がある
  とテスト側が skip せず落ちる。
- 列挙は `git ls-files 'plugins/*/go.mod'`。`plugins/*` は gitignore 済みで同梱する
  ものだけ例外指定しているため、tracked 一覧がそのまま「同梱プラグイン」になる。
  新しく同梱したものは自動で対象になる。
- ローカルでは `make plugin-test` が同じ手順を回す。
- **同 job の末尾で `Check authoring.md snippets compile`** (`make plugin-doc-check`) も
  回す。`docs/plugins/authoring.md` の Go スニペットを使い捨て module に展開して
  ビルドし、doc のとおりに書くとコンパイルできない状態を検出する (#2639)。

### `lint`ジョブ

- `go vet ./...`
- **`Actionlint` step** (`make actionlint`) — workflow の式の typo・存在しない `needs` 参照・
  `runs-on` の誤り・`run:` の中のシェル (shellcheck 経由) を検査する。**CodeQL の `actions`
  とは別物** — あちらは script injection などの**セキュリティ**を見るが、式が壊れているかは
  見ない。workflow のミスは動かすまで分からないので (#2940 で実際に踏んだ)、静的に落とす。
  **版は Makefile 側に 1 つだけ置く** — CI に書き写すと #2841 と同じドリフトが起きる。
  `lint` は required なので固定する (`@latest` だと新しい検査で無関係な PR が赤くなる)。
- `gofmt -s -d .` で差分がないことを確認。差分があれば失敗。
- **`Check duplicate test fixture IDs` step** — テストフィクスチャの ID 重複を検出する。
- **`Golangci-lint` step** (`make golangci-lint`) — `errcheck` / `govet` / `ineffassign` /
  `staticcheck`。`go vet` だけでは見えない層を埋める。設定は `.golangci.yml`。
  **既定の打ち切りを外してある** (同一メッセージ 3 件 / linter 50 件)。切り詰めるだけなので
  赤が緑になることはないが、直すたびに隠れていた分が出てきて「全部直してから有効化する」が
  成立しない。**`checks` は既定を置き換える**ので、既定の無効化も明示的に書き出してある
  (書かないと ST1000 / ST1020 / ST1021 等が黙って有効になる)。**段階的な無効化は残っていない**
  — `unused` / `ST1003` / `ST1012` / `SA1019` はすべて有効で、恒久的に無効なのは `QF*` と
  `S1016` だけ。**除外は 1 つ** — `.golangci.yml` の rule が 1 件 (`test/e2e_federation` の
  パッケージ名 / ST1003) **だけ**。**これは有効化した 4 check に対する数**で、
  `exclusions.presets` の `std-error-handling` (実測 253 件を抑止) は別枠。
  `//nolint:staticcheck` はリポジトリ全体で 2 件 (SA9010 / SA1012) で、どちらも
  今回の 4 check とは無関係。
  版は Makefile 側に 1 つだけ置く。
  **一番重いので step の最後**に置いてある。

### `vulncheck`ジョブ

- `GOOS=linux govulncheck ./...` で依存と Go stdlib の**到達可能な**既知脆弱性を検出する。実際にデプロイするのは Linux なので `GOOS` を明示する (未指定だと host 依存の package load エラーで空振りしうる)。
- あわせて `go.mod` の `go` directive と、golang image を使う**全ての** tracked な Dockerfile (`Dockerfile` / `Dockerfile.bundled` / `deploy/uds/Dockerfile.mkgo` / `tests/` の検証用) の builder tag が同じ patch version を指していることを検査する。govulncheck が見るのは `go.mod` 側だけなので、**Dockerfile だけ古いと CI は緑のまま配る image が脆弱になる**。builder を floating tag (`golang:1.27-alpine`) に戻さないこと (pull 時期で stdlib の patch が変わり、再現可能な形で「既知脆弱性を含まない」と言えない)。配る Dockerfile の base image は tag と digest の併記 (`golang:1.27.1-alpine@sha256:...`) で固定しており、この検査は tag 側で版を照合する。
- 検出は import しているだけのものを含まず、**呼び出しが到達可能なもの**に限られる。無視リストを育てずに運用できるので、抑制ではなく更新で直す。修正版は govulncheck の `Fixed in:` に従うこと (同一モジュールに複数の脆弱性があると必要な版が別々で、低い方に上げても残る)。
- PR の required check には**含めない**。新規 CVE の公開でコードを変えていない PR でも落ちるため。
- 導入は #2387。通常テストが全て緑の状態で到達可能な脆弱性が 11 件残っており、既存の check では捕まらない領域だったため追加した。

### `dependency-review` workflow (PR トリガー)

- `.github/workflows/dependency-review.yml` が、PR が**新しく持ち込む**依存に既知の
  脆弱性が無いかを base と head の差分で見る。
- **`vulncheck` との違いは時点と射程。** あちらは develop に入った後の状態を見て、しかも
  「呼び出しが到達可能なもの」に絞る。こちらは**入る前に**気付ける代わりに到達可能性を
  見ないので、あちらが落とさないものも出る。
- `fail-on-severity: high` から始める。moderate まで落とすと到達不能なものまで止めることに
  なり、依存を上げるだけの PR が通らなくなる。
- **PR へコメントさせない** (`comment-summary-in-pr` は `pull-requests: write` を要る)。
  結果は job のログで読めるので、権限は `contents: read` のままにしてある。
- PR の required check には**含めない**。見ているのは差分だが、判定に使う advisory DB は
  GitHub 側で更新されるので、**同じ差分でも後から赤くなりうる**。

### `codeql` workflow (PR / push / weekly)

- `.github/workflows/codeql.yml` が CodeQL で**自分のコード**を静的解析する。
  `vulncheck` が依存を見るのに対し、こちらはテストが通っていても残る「書いていない分岐」や
  「通ってはいるが危険な形」を拾う。
- **見るのは `go` と `actions` の 2 つだけ。** submodule の外にある .ts/.js/.vue は実測
  340 ファイルで大半が `tests/playwright/specs/**` (うち 189 は upstream 由来の UI spec)、
  Python も `tests/` の検証基盤なので、`javascript-typescript` / `python` は入れない。
  fork frontend は checkout していないので対象外 (upstream のコード)。
- **autobuild を使わない。** 同梱プラグイン (`plugins/*/go.mod`) は別 module で
  `go build ./...` に含まれないため、`git ls-files` で列挙して個別にビルドする
  (`plugin-tests` job が独立しているのと同じ理由)。`go.work` は gitignore 済みなので
  clean checkout では root module だけがビルドされる。
- PR の required check には**含めない**。CodeQL のクエリパックは CLI の更新で増えるので、
  **コードを 1 行も変えていない PR が新しいクエリで赤くなる** (`vulncheck` と同じ理由)。
  代わりに weekly の schedule (月曜 20:30 UTC) を持たせ、クエリが増えた分はそちらで拾う。
- **`ci.yml` に相乗りさせない。** あちらは workflow 直下で `contents: read` に絞っており、
  CodeQL は `security-events: write` を要る。required check を持つ workflow の権限面を
  広げる形は避ける。
- 結果は Actions のログではなく **Code scanning alerts** に出る。誤検知は alert 側で
  dismiss する (ソースに抑制コメントを撒かない)。

### `dropin-e2e` workflow (PR トリガー)

- `.github/workflows/dropin-e2e.yml` が drop-in 互換の e2e を **4 シナリオ並列**で実行する。
  `strategy.matrix.include` で make target と check 表示名を対にしている。

  | check 名 | 実行内容 |
  |---|---|
  | `swap-test` | `make dropin-swap-test` — TS→mk 切替の state preservation (#374) |
  | `mkgo-born` | `make dropin-mkgo-born-test` — mk-go 生まれの DB を TS に引き渡せるか (#2379 / #2383) |
  | `ed25519-verify` | `make dropin-fedibird-test` — Fedibird-like AP mock との Ed25519 双方向 verify (#1083 / #2360) |
  | `federation` | `make federation-misskey-e2e` — 本物の Misskey TS を相手にした実連合 (#2362) |

- `mkgo-born` は `swap-test` と似て見えるが **DB を作った側が違う** (前者は mk-go の
  migration、後者は TypeORM)。TS が一度も触っていない schema を受け取るのは前者だけで、
  運用上は**ロックインの有無そのもの**にあたる。`TestMigrationSeed_CoversUpstream` は
  seed 一覧と upstream migration file の静的な突き合わせに過ぎず、実際に TS を起動して
  確かめてはいない。

- 発火は `pull_request` (paths フィルタ) と `workflow_dispatch`。nightly から PR
  トリガーへ移行済み (#2291)。nightly は失敗に気付くのが翌日になるうえ、1 日分の
  マージがまとまってどの変更が壊したか特定しづらいため。
- PR の required check には**含めない** (federation delivery に flaky 要素があるため)。
  非ブロッキングを `continue-on-error` で実現しないこと (job が成功扱いになる)。
- `fail-fast: false` で 1 つが落ちても他は完走する。これらは実際に別々の壊れ方を
  する (ed25519 側は導入時から 2 箇所壊れていたのに、swap が緑だったため 3 か月
  気付けなかった、#2360)。
- 失敗時は docker compose logs を `dropin-logs-<scenario>` artifact として 14 日保持。
  `swap-test` / `mkgo-born` の orchestrator は `down -v` の**前**に自分で
  `compose.log` / `ps.log` を残すので、workflow 側の収集は `-post` 付きの別名で書く。
  同名にすると撤去済み stack の空ログで上書きしてしまう (#2383)。

### `playwright` workflow (PR トリガー)

- `.github/workflows/playwright.yml` で Playwright spec を実行する。
  `pull_request` (paths フィルタ) と `workflow_dispatch` で発火。nightly から
  PR トリガーへ移行済み (#2291)。
- **4 シャード並列** (`--shard=i/4`)。`fail-fast: false` で 1 つが落ちても
  他は完走する。
- **1 スタックあたりは直列でしか回せない。** 298 spec ファイル中 179 が共有の
  root (alice) で**ブラウザからサインイン**し (数え方は
  `grep -rlE 'uiSigninAsRoot|signin-username' tests/playwright/specs --include='*.spec.ts' | wc -l`)、さらに 32 が
  サインインせず root の token で API を叩く (`root.json` を読むのが 211 で、その差分)。
  instance meta も全 spec が共有する。Playwright は
  ファイル単位で並列化するので、`workers` を上げると `profile_iscat_toggle` と
  `profile_isbot_toggle` が同じアカウントを、`admin_branding_save` と
  `about_page_render` が同じ meta を取り合う。root の quota
  (antenna 5 / webhook 3 / clip 10) を消費するファイルも 18 ある。
  **並列度はスタックごと分ける = シャードでしか稼げない** (#2609)。
- `backend = ts` は `workflow_dispatch` 専用 (plan job が matrix を切り替え)。
  upstream 追従のタイミングだけ回す運用。
- **shard を matrix の軸として書かないこと。** `include` は既存の combination に
  merge できない entry を新規 combination として足す semantics なので、軸と
  併用すると pull_request で TS backend を落とす絞り込みが壊れる。plan job で
  backend x shard の直積を組んで include 配列ごと渡す。
- PR の required check には**含めない**。
- **録画はしない** (`video: 'off'`)。CI は成功 run の成果物を一切アップロード
  しないので録画しても捨てるだけで、失敗 run でも実測 webm 256 本のうち失敗に
  対応するのは 2 本だけだった。調査材料は trace が担う (#2609)。
- 失敗時は `tests/playwright/test-results/` (trace / screenshot 含む) と
  docker compose logs を `playwright-results-<backend>-<shard>` /
  `playwright-logs-<backend>-<shard>` artifact として 14 日保持。

### `upstream-backend-e2e` workflow (PR トリガー)

- `.github/workflows/upstream-backend-e2e.yml` で Misskey 本家の backend e2e
  (`third_party/misskey/packages/backend/test/e2e/**`) を mk-go に向けて実行する。
  テスト本体は無改変で、vitest の `globalSetup` / `setupFiles` だけを差し替える。
- `pull_request` で paths (`internal/**` / `cmd/**` / `migration/**` /
  `tests/upstream-e2e/**` / `third_party/misskey` / `Makefile` / `go.mod` /
  `go.sum` / 当 workflow) に該当する変更のみ発火。`workflow_dispatch` で任意の
  ref に対して手動実行も可。
- **4 シャード並列** (`--shard=i/4`)。`fail-fast: false`。**プロセス内では
  並列にできない**: upstream の vitest 設定が `maxWorkers: 1` で、かつ
  setupFiles がファイルごとに mk-go の `/api/reset-db` (全テーブル truncate) を
  叩くため、同じ DB に 2 ファイルを並行させると片方が相手のフィクスチャを
  実行中に消す。job を分ければ PostgreSQL / Redis の service container も
  別に立つ (#2609)。
- PR の required check には**含めない** (1200 件超のテストに flaky 要素が
  あるため merge ブロッカーには適さない)。非ブロッキングを
  `continue-on-error` で実現しないこと (job が成功扱いになり失敗が不可視になる)。
- 『通らないことが正しい』テストは `tests/upstream-e2e/known-divergences.json` に
  根拠付きで登録し、expected-failure (`task.fails`) として扱う。skip ではないので
  乖離が解消したテストは逆に落ち、一覧の陳腐化に気付ける。
- 失敗時は mk-go のログを `upstream-e2e-mkgo-log-<shard>` artifact として 14 日保持。

### `diff-e2e` workflow (PR トリガー)

- `.github/workflows/diff-e2e.yml` が `make diff-check` を実行し、mk-go と Misskey TS に
  同一リクエストを投げて**レスポンスを値レベルで diff** する (#2078 / #2368、endpoint 比較 35 件)。
- 守備範囲が他のゲートと違う。本家 backend e2e は「本家のテストが通るか」、shape drift は
  「フィールドの有無・型」、diff-e2e は「**同じ入力に対する値そのもの**」を見る。shape が
  合っていても値が違う類のバグはこれでしか捕まらない。
- 意図的な差分は `tests/diff/test_endpoints.py` の ignore-list に**理由付きで**登録する。
  空振りさせると本物の乖離が埋もれるので、追加時は `docs/divergence.md` にも対応する記述が
  あるかを確認すること。
- PR の required check には**含めない**。

### `apicompat` workflow (PR トリガー)

- `.github/workflows/apicompat.yml` が `make apicompat` を回し、**`docs/api-compat.md` が
  実態とずれていないか**を見る。あれは生成物で CLAUDE.md も「手で直さない」と書いているが、
  **再生成が人手に頼っていた**ので、route を足しても upstream が endpoint を増やしても
  マトリクスは黙って古くなる。読む人は「mk-go only 59 件」のような数字を現状だと思う。
- **既存のどの job にも相乗りできない。** submodule (TS の endpoints を読む) と DB / Redis
  (route dump がサーバーを組み立てる) の両方が要るが、`test-shards` は `third_party/misskey`
  を checkout せず、`frontend-check` は DB を持たない。
- **config は `tests/upstream-e2e/mkgo.yml`。** `testMode: true` が要る — 無いと
  `/api/reset-db` が route に載らず、マトリクスが「TS 側に存在するが未実装 1 件」に化ける。
  接続先だけ `MK_*` で service container へ向ける。
- **プラグインは入らない前提。** 同梱の 2 つは `disabled: true` なので `pluginbuild` が
  skip する (#2701)。自前プラグインを `plugins/` に置いた手元で回すと 19 行混入するが、
  clean checkout では起きない。
- PR の required check には**含めない**。判定材料に submodule の内容が入るので、こちらの
  コードを触っていない PR でも upstream の bump で赤くなりうる。

### `frontend-check` job (ci.yml)

- fork frontend (`third_party/misskey`) を `vue-tsc --noEmit` で型チェックする。1.0 以降
  fork frontend は mk-go 独自に進化させる方針なので、型崩れの検出手段が要る。
- **submodule のソースを読むゲートもここで回す** (#2892)。`test-shards` は
  `third_party/misskey` を checkout しないので、そちらでは skip するしかない。
  skip は成功として扱われるため、この job では `MK_FRONTEND_GATES_REQUIRE_SUBMODULE`
  を渡して skip を禁じる (`plugin-tests` の `MK_PLUGIN_TESTS_REQUIRE_DB` と同じ形)。
  **`make gates` には入れない** — あちらは submodule 無しで回る前提で、混ぜると
  checkout していない環境で「検査していないのに緑」になる。
- `make uds-frontend-build` / `e2e-frontend-build` は本番が bind-mount している
  `third_party/misskey/built` を書き換えるため**検証には使えない**。
- required check (build / test / lint) には**含めない**。

### `build-with-plugins` workflow (reusable) / `build-with-plugins-selftest` (PR トリガー)

- `build-with-plugins.yml` は **`workflow_call` 専用**。運営者が自分のリポジトリから
  「使いたいプラグインのリスト」を渡して呼ぶと、それらを `plugins/` へ clone して
  `Dockerfile.bundled` を build し、**呼び出し元の GHCR** へ publish する (#2940)。
  mk-go 側はビルド基盤も成果物も持たない。
- **`permissions` を宣言していない。** reusable workflow の permissions は caller の
  権限以下にしか設定できず、宣言すると caller がそれを持たない場合に run ごと
  拒否される (`push: false` でも同じ)。publish する caller が `packages: write` を書く。
- frontend を持つプラグインがあるかは `pluginbuild` の出力で判定し、あるときだけ
  SPA を自前でビルドして `ASSETS_SOURCE=local` で焼き込む。無ければ公式の
  assets イメージを使って pnpm のビルドを丸ごと省く。
- **要求したプラグインが組み込まれたかを突き合わせる。** `disabled: true` は黙って
  skip されるので、見ないと「指定したのに 0 個入っている image」が緑で出る。
- `build-with-plugins-selftest.yml` が `pull_request` (paths フィルタ) と
  `workflow_dispatch` でそれを呼び、`push: false` でビルドだけ通す。**PR で発火させる
  のが要点** — `workflow_dispatch` は default branch にある workflow しか起動できず、
  それだけだとマージ前に一度も検証できない。check 名は `build / build` (caller の
  job 名 + callee の job 名) で、`gh pr checks` の一覧には現れないので
  `gh run list --workflow build-with-plugins-selftest.yml` で見る。実測 6 分。
  **`docker build --check` が見ない範囲を押さえるのはこれだけ** — stage 名の解決は
  `--check` で分かるが、`pluginbuild` と `go build` が実際に通るか、`assets-local` の
  COPY 元が context に実在するか、pnpm の symlink を越えられるかは RUN / COPY を
  実行しないと分からない。
- PR の required check には**含めない** (外部リポジトリの clone に依存するため)。

### `docker` / `docker-branch` workflow

- `docker.yml` は **`push` / `pull_request` / `workflow_dispatch`** で発火し、
  image がビルドできるかを見る (PR では push しない)。check 名は
  `build-and-push` / `build-and-push-bundled`。`workflow_dispatch` は過去の
  リリースタグから image を publish し直す用途
  (`gh workflow run docker.yml -f tag=1.1.1`)。
- `docker-branch.yml` は **image をビルドしない**。`develop` への push (paths フィルタ付き) と `workflow_dispatch` で、compose ファイルだけを載せた orphan ブランチ `docker` を force-push する (「pull して動かすだけ」の構成を配るため)。検査は `docker compose config --quiet` のみ。
- PR の required check には**含めない**。

### schedule で回る workflow

PR では回らないので、失敗は Actions 上で確認して別 PR で対処する。

| workflow | 内容 | 時刻 |
|---|---|---|
| `dropin-frontend-e2e.yml` | 3 TS インスタンス + cypress で frontend 視点の drop-in 互換 | 19:00 UTC |
| `queue-bench-smoke.yml` | queue driver がジョブを落としていないか (`ok == sent`) | 17:30 UTC |

### CI失敗時の対応

- カバレッジ不足 → テストケースを追加してから再push。
- `gofmt`差分 → `make fmt`をローカルで実行してから再push。
- テスト失敗 → CIログを読み、ローカルで再現させてから修正。`--no-verify`等でフックを飛ばさない。

## 9. Environment Variables

### 設定ファイル

- デフォルト: `.config/default.yml`（Misskey互換YAML, gitignored）
  - 初回は `cp .config/default.yml.example .config/default.yml` で複製
- Docker: `.config/docker.yml.example` を Dockerfile が image に焼き込む
  - operator が独自設定したい場合は `cp .config/docker.yml.example .config/docker.yml` してから docker-compose で volume mount で上書き
- CLIフラグ `-config <path>` でパスを指定。

### 環境変数オーバーライド

`MK_`プレフィックス付きの環境変数で設定値を上書きできる。ネストキーは`_`区切り。

よく使うもの:

| 環境変数 | 対応YAMLキー |
|---------|-------------|
| `MK_URL` | `url` |
| `MK_PORT` | `port` |
| `MK_DB_HOST` | `db.host` |
| `MK_DB_PORT` | `db.port` |
| `MK_DB_DB` | `db.db` |
| `MK_DB_USER` | `db.user` |
| `MK_DB_PASS` | `db.pass` |
| `MK_REDIS_HOST` | `redis.host` |
| `MK_REDIS_PORT` | `redis.port` |
| `MK_REDIS_PASS` | `redis.pass` |
| `MK_ID` | `id` (デフォルト`aidx`) |

**これは一部で、`bindEnvKeys()` は 90 キーを登録している。** 内訳は用途別 Redis 5 系統
(`redis` / `redisForPubsub` / `redisForJobQueue` / `redisForTimelines` /
`redisForReactions`) が各 9、`db.*` が 9、`logging.sql.*` が 2、
`sentryForBackend.options.{dsn,environment}` が 2、残り 32 がトップレベル
(`jobQueueDriver` / `jobQueueAutoScale` / `maxWorkers` / `minWorkers` /
`maxWorkersGlobal` / `enableMetrics` / `trustProxy` など)。全量は
`internal/config/config.go` の `bindEnvKeys()` を見ること。運用向けの説明は
[docs/configuration.md](docs/configuration.md)。

**登録の有無で「作れるか」だけが変わる。** Viper は `AutomaticEnv` を有効にしている
ので、**設定ファイルにそのキーが書かれていれば `MK_` で上書きできる**。
`bindEnvKeys()` に登録されているキーは、ファイルに書かれていなくても `MK_` だけで
設定できる。逆に未登録かつファイルにも無いキーは `MK_` では作れない。

実務上は `.config/*.yml.example` が既定でコメントアウトしているものが引っかかる。
`meilisearch:` と `<queue>JobConcurrency` / `<queue>JobPerSec` は**コメントアウトされた
まま**なので、example をそのまま使う構成では `MK_MEILISEARCH_HOST` /
`MK_DELIVERJOBCONCURRENCY` を export しても効かない。使うならまず yml 側の
コメントを外す。

**`MK_*` はファイルより優先される。** 手元で export したまま `internal/config` の
テストを走らせると、設定ファイルの値を期待するケースが落ちる。

### テスト用環境変数（CI）

- `TEST_DB_HOST`, `TEST_DB_PORT`, `TEST_DB_NAME`, `TEST_DB_USER`, `TEST_DB_PASS`, `TEST_DB_SSLMODE`
- `TEST_REDIS_HOST`, `TEST_REDIS_PORT`

既定は `localhost:5432` の `misskey_test` に `mk` / `mk`。違う接続先を使うときだけ `.env.test` を置くか export する。

**`TEST_REDIS_*` は `.env.test.example` に無い。** これを読むのは `internal/core/chart` だけで、`SetupRedis` は環境変数を見ずに必ず testcontainers を立てる。

### マイグレーションの接続先

`cmd/migrate` は **`DATABASE_URL` を読まない**。`-config` (既定 `.config/default.yml`) を
`config.Load` して `db.*` から DSN を組み立てる。別の DB へ流すなら `-config` を渡すか
`MK_DB_*` で上書きする。

## 10. 開発方針

### Phaseベースの進行

開発はPhase単位で進める。各Phaseの内容・進捗はGitHub Issuesで管理する。新しい作業を始める前に対応するissueを作成し、実装完了時にPRでcloseする（詳細はSection 7）。

### タスクの粒度

- タスクは**1 issue = 1 PRで完結する粒度**に分割する。
- 大きな機能追加は`Phase N.M`のサブフェーズに分けて個別のissueを立て、段階的にマージする。
- 1つのPRで「機能追加 + 無関係なリファクタ」を混ぜない。

### 設計の変更

- 設計方針を変更する場合は、対応するissue（または新規issue）で背景・変更内容を議論・記録してから実装する。
- 実装中に設計の問題に気づいた場合は一度立ち止まり、ユーザーに確認する。

### オリジナルMisskeyの参照

- 実装方針に迷った場合は`.tmp/misskey/`（オリジナルMisskeyのソース、gitignore）を参照する。
- ただし**TypeScriptのパターンをそのままGoに翻訳しない**。Goらしい書き方（インターフェース、明示的エラー、構造体埋め込み）に適応させる。

### 破壊的操作の扱い

- マイグレーションのdownスクリプトは必ず書く。ただしdata lossが発生する場合はその旨コメントする。
- DBテーブル削除、カラム削除は`Phase`をまたぐ段階的移行を検討する。
- 本番に影響する変更はユーザーに事前確認する。

### 補助ツール

- **ライブラリの使い方を調べる際はContext7 MCP**を使って最新情報を取得する。
- 隠しフォルダ（`.tmp`等）を探す際は`List`ではなく`Bash`（`ls -la`）を使う。

---

## 更新記録

本ドキュメントの主要な変更履歴。新規変更時は一番上に追記する（日付降順）。
個別 fix の履歴は CHANGELOG.md 側に集約しており、本セクションは CLAUDE.md 本体
(Section 1-10 の policy / Makefile target / CI 閾値 / CI workflow 等) を変更した
タイミングのみ記録する。

- **2026-09-26**: `.github/workflows/` の action を**全て commit SHA で固定**した (`# vX.Y.Z` の
  コメント付き。`actions/*` も例外にしない)。tag は付け替えられるので、`packages: write` で
  GHCR へ publish する `docker.yml` / `build-with-plugins.yml` の中で動く action が差し替わると
  配る image を書き換えられる。`TestWorkflowActionsArePinnedToSHA` が形を固定し、更新は
  `.github/dependabot.yml` の `github-actions` で受ける。publish する job の checkout には
  `persist-credentials: false` を付けた。手順は docs/ci.md の「action の版固定」。
  同じ理由で、配る Dockerfile (`Dockerfile` / `Dockerfile.bundled` /
  `deploy/uds/Dockerfile.mkgo`) の base image (golang / distroless / alpine) も
  `<tag>@sha256:<digest>` で固定した (`TestDistributedDockerfileBaseImagesArePinnedByDigest`)。
  digest の更新は dependabot の `docker` が受け、tag の版は上げさせない (golang は
  go.mod と揃える必要があるため)。

- **2026-09-23**: mkq を v1.0.8 → **v1.1.1** に更新 (BullMQ 6 へ移行。upstream 2026.9.0 の
  bullmq 6.3.2 と wire が揃う)。**2026-09-22 の SA1019 entry にある「Redis は呼び出し側で
  Start / Stop を入れ替えない」は go-redis v9.21.0 で逆になった** (redis/go-redis#3751)。
  依存の連鎖で go-redis が 9.18 → 9.22 に上がり、`ZRangeArgs` の `Rev` + `ByLex` で
  go-redis が並べ替えなくなったため、アンテナのタイムラインが実 Redis で空を返した
  (`TestNotes_*` が検出)。現在は**呼び出し側で `Rev` のとき Start に大きい方を置く**。
  **pause は BullMQ 6 でフラグだけになり、ジョブは `wait` に残る** — pause 中も
  `Pending` に backlog が見える。オートスケーラはその深さで worker を増やしていたので、
  #3166 で `PendingCount` を `DispatchableCount` (pause 中は 0) に改めた。

- **2026-09-23**: Go を 1.26.6 → **1.27.1** に更新。Section 1 の技術スタック表と Section 8 の
  floating tag の例を合わせた。**`go.work` は生成物なので `make plugins` で作り直す** —
  作り直さないと `go.work` の `go 1.26.6` で toolchain が選ばれ (`go version` が 1.26.6 の
  まま)、ビルドが `requires go >= 1.27.1` で落ちる。あわせて `make golangci-lint` の
  toolchain を go.mod の版に固定した — `go run golangci-lint@v2.13.2` は golangci-lint 自身の
  `go 1.26.0` を基準に選ぶので、手元の go が古いと go1.26 でビルドされて lint を拒否する。
  **CI で 2 つ落ちた (手元の `make check` は緑だった)。** (a) **gofmt の整形結果が変わった**
  (コメントの桁揃え) のに、`make fmt` は PATH の gofmt (1.26) を使っていて気付けなかった。
  `go env GOROOT` の gofmt を使うよう直した。しかも 1.27 の `gofmt -d` は差分があると
  exit 1 を返すので、CI の Format check は `bash -e` で**差分を 1 行も出さずに**落ちていた。
  (b) **カバレッジのブロックが細かく数えられる**ようになり、`tools/pluginbuild` が
  90.8% → 88.6% に落ちた (文の総数 163 → 184。テストされない `main` の重みが増えた)。
  フラグの解析を `parseArgs` に切り出してテストした。

- **2026-09-22**: legacy の **asynq driver を削除**し、mkq を唯一の queue driver にした
  (#2985)。Section 1 の技術スタック表と Section 2 のツリーを実態に合わせてある。
  **既定が mkq になってから 4 か月以上、本番で asynq へ戻す判断は一度も要らなかった**
  (既定の切り替えは #631、2026-05-02。`gh pr view 631 --json mergedAt` で実測)。残して
  いたぶんだけ実装と doc が二重になり、`internal/queue/driver/option.go` だけで asynq に
  言及する行が **10** あった (数え方: 削除前の同ファイルで `grep -c asynq`)。うち
  「silent no-op」「対応 API なし」と書いてあるのは **6 行**で、残りは既定値の違い
  (`asynq defaults to 25`) や意味の違い (`MaxRetry=0` の解釈) の注記。
  **`jobQueueDriver: asynq` は起動エラーにする。** 既定が mkq の状態で明示して asynq を
  選んでいた運用は意図的なので、黙って mkq で起動すると **driver が入れ替わったことに
  気付けない** (予約投稿の可否もレート上限の効き方も変わる)。typo (`mkqq`) とは文面を
  分ける — operator が取るべき行動が違うため、asynq だけ「削除済み。mkq にするか行を
  消せば既定」と案内する。**移行は片道なので、その旨もメッセージに書く** —
  asynq の未処理ジョブは `asynq:{<queue>}:*` に残り mkq (`bull:*`) からは見えないので、
  切り替え前に**旧ビルドで捌ききる**しかない。新ビルドは起動を拒むので、後から捌く
  手段が無い。
  **消える能力差は 2 つ。** `SupportsScheduledNote` (= asynq では予約投稿を
  `TOO_MANY_SCHEDULED_NOTES` で門前払いしていた、#1045 Phase 2-C) と、
  `startAutoScale` の `ErrResizeNotSupported` 起動エラー判定。
  **前者を消すと `notes/drafts/update` から `TOO_MANY_SCHEDULED_NOTES` の唯一の出口が
  消える。** upstream (`NoteDraftService.update`) は「未予約 → 予約」に切り替わるとき
  `scheduledNoteLimit` を見るが、mk-go の update は**元からその gate を持っていない**
  (create 側は持つ)。既定 mkq では capability gate が常に false だったので発火していた
  わけでもなく回帰ではないが、コードが消えると乖離が見えなくなるので
  `docs/divergence.md` に記録した。`errorid-check` は emission しか見ないので落ちない。
  **後者は「残す」と書きかけて、裏取りで誤りだと分かった。** 初稿は「mkqdriver も
  `Server.Start` 前は同じエラーを返すので、配線順を守る guard として生きている」と
  書いたが、**本番配線では一度も発火しない**。`d.dServer` を代入するのは
  `Driver.Server()` で (`Start()` ではない)、`newServer` は構築時に
  `queue.NewServer(queueDriver)` = `d.Server()` を呼ぶ。だから `Driver.Resize` は常に
  `Server.Resize` へ委譲し、pool 未作成時に返るのは `ErrResizeNotSupported` ではなく
  `mkqdriver: Resize: unknown queue "deliver"` で、guard の `errors.Is` に当たらない。
  **述語を `err != nil` に広げる案も採れない** — `TestStartAutoScale_InitialResizeFailureDoesNotPreventStart`
  が「一時的な Resize 失敗で起動を止めない」を固定している (Redis の瞬断で起動不能に
  なる)。**この誤りは敵対的レビューが実測で見つけた。** 「コードを読んだ」で済ませて
  4 箇所へ書き写すところだった。
  **`driver.ErrResizeNotSupported` 自体は残る** — `Driver.Server()` を一度も呼んで
  いない driver が `Resize` された場合の sentinel で、`mkqdriver` 自身のテストが
  固定している (`TestDriver_Resize_BeforeStartReturnsNotSupported`)。
  **テストの移植で 1 群、driver 固有の前提に依存していたものが出た。** `internal/queue` の
  `TestClient_Enqueue*_ClosedClientFails` 4 本は `queue.Client.Close()` 後の enqueue が
  失敗することを見ていた。asynq では `Client.Close` が実接続を閉じるので意味があったが、
  **mkq の `Client.Close` は no-op** (接続は driver が持つ) なので、そのまま移すと落ちる。
  閉じる対象を driver に直して `*_ClosedDriverFails` にした (実測: 元の形に戻すと 4 本とも
  「エラーが返らない」で落ちる)。**「空虚」ではない** — このリポジトリで空虚と呼ぶのは
  「壊しても通る」ほうで、これは「driver を替えると落ちる」形だった。
  attempts の検証も移し替えが要った — **mkq driver は `TaskSummary.MaxRetry` を埋めない**
  ので、BullMQ の `opts.attempts` を読む形にしてある (変異検証: `mkqdriver/option.go` の
  `WithAttempts(o.MaxRetry+1)` を `WithAttempts(o.MaxRetry)` にして
  `go test ./internal/queue/...` を回すと **7 本**落ちる。移植した 6 本と、
  mkqdriver 側で元からある `TestEnqueue_MaxRetryAppliedToBullMQHash`)。
  **`go.mod` は `hibiken/asynq` と `golang.org/x/time` の 2 つを外す。** 後者は
  asynqdriver の rate limiter でしか直接使っていなかった。ただし **消しきれない** —
  `echo/v4/middleware` が要るので `// indirect` へ移す (`go mod tidy` はこのリポジトリでは
  使えないので手で動かし、`GOWORK=off go build ./...` で充足を確認した。**手元は
  `cmd/misskey/plugins_generated.go` が private plugin を import するので、退避してから
  でないと go.sum の検証にならない**)。
  **queue-bench は 2-way (TS ↔ mkq) にした。** 実測表は当時測った値の記録なので残し、
  「#2985 より前の表には asynq 行がある」と注記した。
  **`docs/design/*` は注記だけ足す。** 設計時点の見積もり (`Phase 7 (数ヶ月後)`)、Phase 表の
  見積もり列、実測値は触らない。現在形で書かれていて嘘になった段落 (auto-scale ADR §5.2、
  mkq-design の Status) にだけ「#2985 で削除済み」を添えた。
  **射程外**: 実測値と設計判断そのもの (この更新記録の過去 entry、`docs/update/*`、
  `tests/queue-bench/results/*`、`docs/design/*` の数値と見積もり) は書き換えない。

- **2026-09-22**: `apicompat` workflow を追加。**`docs/api-compat.md` の再生成が人手に
  頼っていた** — CLAUDE.md 自身が「生成物を手で直さない」と書いているのに、古くなっても
  気付く仕組みが無かった。route を足しても upstream が endpoint を増やしてもマトリクスは
  黙ってずれ、読む人は「mk-go only 59 件」のような数字を現状だと思って判断する。
  **既存のどの job にも相乗りできない。** submodule (TS の endpoints を読む) と DB / Redis
  (route dump がサーバーを組み立てる) の両方が要るが、`test-shards` は `third_party/misskey`
  を checkout せず、`frontend-check` は DB を持たない。別 workflow にして paths で絞った。
  **再生成には条件が 2 つある** (1.3.0 のリリースで実際に踏んだ)。`testMode: true` が無いと
  `/api/reset-db` が route に載らず「TS 側に存在するが未実装 1 件」に化けるので、config は
  `tests/upstream-e2e/mkgo.yml` を使い接続先だけ `MK_*` で service container へ向ける。
  **プラグインが入ると 19 行混入する**が、同梱の 2 つは `disabled: true` なので clean
  checkout では起きない (#2701)。
  **required には含めない** — 判定材料に submodule の内容が入るので、こちらのコードを
  触っていない PR でも upstream の bump で赤くなりうる。
  **検証は PR 上で行う** (`docs/ci.md` の方針)。`workflow_dispatch` だけだと default branch に
  あるものしか起動できず、マージ前に一度も確かめられない。
- **2026-09-22**: migration の **up → down → up 往復テスト**を追加
  (`internal/repository/migration_roundtrip_test.go`)。**書いた瞬間に本物のバグを 1 件
  見つけた** — `000001_initial.down.sql` が `DROP TABLE IF EXISTS "schema_migrations"` を
  持っており、golang-migrate が自分で管理するテーブルを消していた。`Down()` は全 down の
  あとに `TRUNCATE schema_migrations` を撃つので、**`go run ./cmd/migrate -direction down`
  (CLAUDE.md Section 3 が全段ロールバックとして案内している手順) は毎回最後に
  `relation does not exist (SQLSTATE 42P01)` で落ちていた**。`make migrate-down`
  (`-steps 1`) も version 1 のときは同じ理由で落ちる。**全段 down の後は
  `schema_migrations` が 1 つだけ空で残る** — Section 3 ほか 6 箇所が「schema が消える」と
  書いていたが `DROP SCHEMA` は一度も走らないので元から不正確で、「全テーブルが消える」へ
  直した。
  **`testutil.ApplyMigrations` では代用できない。** あちらの `findMigrationFiles` は
  `*.up.sql` しか glob しないので **down を 1 本も実行しない**。加えて up 側も `db.Exec` の
  エラーを握り潰す (`continue`) ので壊れた SQL でも緑になる。本番の `cmd/migrate` と同じ
  golang-migrate + pgx5 driver に流す。
  **down は書いた時点でしか実行されない。** 97 本あって、後から up 側だけ直して対応が
  崩れても誰も気付けない。壊れているのは**戻したくなった当日**に分かる。
  **「2 回目の up が通るか」だけでは弱い。** up の 97 本中 96 本は `IF NOT EXISTS` /
  `EXCEPTION WHEN duplicate_object` で守られているので、**down が取りこぼしても再適用が
  通ってしまう**。実測 (down を 1 本ずつ空にする ablation) で 2 回目の up が検出できたのは
  非冪等な `ADD CONSTRAINT` を持つ `000001` だけで、サンプルした他 19 本は緑だった。
  **down 後に schema の中身 (テーブル / view / sequence / enum) が空であることを直接
  アサートする**形にして射程を広げてある — これで `000050` / `000075` を空にする変異も
  検出するようになった (実測)。
  **専用の兄弟 schema を使い、毎回作り直す。** `internal/repository` の schema でやると
  down が他のテストの前提を消す (#2450)。**作り直しが要るのは、途中で落ちたときに残骸が
  残って次の実行が別の理由で落ちるから** — 診断が事実と無関係になる (変異検証で実際に
  そうなった)。
  **変異集合**: down に構文エラー (最後 / 中間) / down を空にする (`000001` / `000050` /
  `000075` / 全部) / `schema_migrations` の DROP を戻す (= 見つけたバグの再導入) /
  `m.Up()` を消す / `m.Down()` を消す / 2 回目の `m.Up()` を消す / `DROP SCHEMA` を消す
  (前の実行が途中で失敗している状態で落ちる)。**`000097` を空にする変異は検出しない** —
  あれは down が元から `-- no-op.` (データ修正の migration) なので、空にしても事実として
  何も変わらない。**`v1 == v2` と `dirty == false` はアサーションにしても恒真**だったので
  (実測で変異が素通りした)、version は `migration/` の最大連番と突き合わせる形に替えた。
  **射程外**: TypeORM 台帳を落とす `000029_db_compat_misskey.down.sql` の
  `DROP TABLE IF EXISTS "migrations"` — 同じクラスのバグだが、up が
  `CREATE TABLE IF NOT EXISTS "migrations"` を持つので往復では対称になり緑で通る
  (既知として `docs/migration-from-ts.md` に記載がある)。
  **`git checkout` で変異を戻さないこと** — 未コミットの修正まで巻き戻す (実際に踏んだ)。
- **2026-09-22**: `echo` の `LoggerWithConfig` を `RequestLoggerWithConfig` へ移行し、
  **`SA1019` の抑制をゼロにした**。同日の SA1019 entry で「移行しない」と判断した唯一の
  1 件で、そのときの理由は「redact の配線を固定するテストが無く、壊しても誰も気付けない」
  だった。**テストを足した時点でその理由は消えていた**ので移した。
  **出力は完全に同じ。** 旧実装と新実装を同一プロセスで並べて実測し、
  `2026-09-22T17:50:40+09:00 GET /api/notes/timeline?i=REDACTED&limit=10 200 757ns` の形式が
  一致することを確認した (差はレイテンシの実測値だけ)。**時刻は `v.StartTime` ではなく
  `time.Now()`** — 旧 `${time_rfc3339}` が書き込み時点を出していたので合わせた
  (`StartTime` はリクエスト開始時刻で値がずれる)。
  **`RequestLoggerConfig` は `Output` を持たない。** `LogValuesFunc` の中で自分で書くので、
  テストと共有するには writer を引数で渡す形になる (`gzipConfig` の「設定を返す」形とは
  少し違う)。
  **`HandleError: true` が要る。** 既定 (false) だと `c.Error(err)` が呼ばれず `res.Status` が
  更新されないので、handler が素の error を返したときに**クライアントには 500 を返しながら
  ログには 200 と書く**。旧実装は `c.Error(err)` を先に呼んでから status を読んでいた
  (レビューの実測で 17 ケース中 4 ケースが食い違い、これを立てると 17/17 一致)。
  **エラーを上位へ返さないようにラップする。** 旧 `LoggerWithConfig` は名前付き戻り値が
  後続の代入で上書きされる副作用で handler のエラーを飲んでいた。素直に移すと
  `return err` するので、**外側の Sentry middleware が 404 / 405 まで capture し始める** —
  未認証で誰でも叩ける経路なので、存在しないパスへ POST を投げるだけで quota を焼ける
  (`sampleRate` の既定は 1.0)。**この副作用は最初のコミットで見落としており、レビューが
  実測で見つけた。** 旧挙動に揃えて握ってある。飲むこと自体の是非は別で、直すなら
  Sentry 側を「5xx だけ capture」にするのが筋。
  **色は元から一度も出ていなかった。** 旧実装は `${status}` を gommon/color 経由で出すが、
  production は `Output` を設定していないので `SetOutput(nil)` が呼ばれる。gommon は
  `*os.File` でない時点で `disabled = true` にして**二度と戻さない**ため、TTY でも色は
  付かなかった (レビューが実 PTY で実測)。**初稿は「TTY のときの色が無くなる」「差が出るのは
  `make dev` のときだけ」と書いたが、どちらも裏取りせずに書いた誤り。**
  **これで `//nolint:staticcheck` は 2 件** (SA9010 / SA1012) になり、どちらも
  今回有効化した 4 check とは無関係になった。
- **2026-09-22**: `unused` を有効化し、**段階的な無効化を解消した**。恒久的に無効なのは
  `QF*` と `S1016` だけになった。削除したのは 20 件で、**19 件はテスト側の未使用 stub**
  (`stubFedCache` とその 4 メソッド、`failingListByUserRepo`、`test/e2e/helpers_test.go` の
  ヘルパー 4 つなど。書いたが使わなかったもの)。
  **本番は 1 件だけで、それは「呼ばれるべきなのに呼ばれていない」バグではなかった。**
  `reaction.Service.normalizeReaction` は `resolveReaction` の戻り値を 1 つに削った薄い
  ラッパーで、呼び出し側が実体を直接呼ぶようになった後の残骸。**正規化そのものは現役**
  (`Create` が `resolveReaction` を呼ぶ) なので、消しても挙動は変わらない。
  **消すと doc の参照が切れる。** `normalizeReaction` は**振る舞いの説明の根拠**として
  9 箇所から名指しされていた (数え方: 宣言行と自身の doc 冒頭を除いた `normalizeReaction` の
  語境界一致)。8 箇所は `entity/emoji_resolver.go` の「local 絵文字を `:name@.:` 形式で
  永続化する」や `server/emoji_redirect.go` の同旨で機械置換でき、9 件目の `resolveReaction`
  自身の doc (「shared core of normalizeReaction」) だけ書き換えが要った。参照を `resolveReaction` へ
  付け替え、**消える doc に書かれていた正規化の規則 (空文字列 → heart、レガシー → Unicode、
  カスタム絵文字の `:name@.:` 化、actorHost へのフォールバック #459) は `resolveReaction` の
  doc へ移した** — 関数を消すときに一緒に消えると、仕様がコードのどこにも残らない。
  **`run.tests` は既定のまま**にしてある。`tests: false` にすると「テストからしか使われない
  もの」まで未使用と出る (数え方: `run: tests: false` を足して `make golangci-lint`。**この
  entry の削除後で 10 件** — 削除前は 11 件で、11 件目が消した `normalizeReaction` 自身だった)。
  **射程外**: `QF*` / `S1016` (恒久)、別 module の `plugins/`。
- **2026-09-22**: `SA1019` (非推奨 API) を有効化。10 件のうち **9 件を移行し、1 件だけ
  `//nolint` で抑えた**。段階的に残っていた `unused` も同日に有効化した (下の entry)。
  **`go/parser.ParseDir` (5 件) は非推奨の理由がこちらの要件に合う。** 「build tag を見ないので
  package とファイルの対応が不正確」というのが非推奨の理由だが、ゲートは**ディレクトリ内の
  .go を全部見たい**ので、その不正確さがむしろ望ましい。代替として案内される
  `golang.org/x/tools/go/packages` は `go list` を起動するぶん重く、package 単位で解決するので
  ディレクトリを直接列挙したいここには合わない。**「型チェックまで走るので読めなくなる」は
  誤り** — 型チェックは `NeedTypes` を渡したときだけ走り、走らせても AST は返る (初稿はそう
  書いてレビューに実測で否定された)。`os.ReadDir` +
  `parser.ParseFile` に置き換えた (`internal/entitycompat` は 2 箇所あるので
  `parseNonTestGoFiles` に寄せた)。
  **Redis は呼び出し側で Start / Stop を入れ替えない。** `ZRangeByLex` /
  `ZRevRangeByLex` は Redis 6.2 で非推奨なので `ZRangeArgs` へ移したが、**`Rev` +
  `ByLex` のときは go-redis の `appendArgs` が `Stop, Start` の順に並べ替える**
  (`sortedset_commands.go`)。Redis の `ZRANGE ... REV` が `start > stop` を要求するのに
  合わせる処理で、呼び出し側は常に「小さい方が Start」で渡す。**入れ替えると範囲が空になる**
  (実測: 入れ替える変異で `TestNotes_Paging*` が落ちる)。
  **`ReverseProxy.Director` -> `Rewrite` で 3 つ変わる。** (a) `SetURL` は宛先へ向けるだけで
  なく **`Out.Host` を空にして `Out.URL.Host` を Host ヘッダにする**ので、`Director` 版の
  `req.Host = remote.Host` に相当する代入は要らない — **最初それを書いたが、変異検証で
  「消しても落ちない」= 冗長だと分かって外した**。(b) **`Rewrite` は X-Forwarded-* を
  落としてから呼ばれる**ので、`NewSingleHostReverseProxy` が `ServeHTTP` で自動付与していた
  `X-Forwarded-For` が消える。`SetXForwarded()` で付け直すが、**等価ではない** —
  `Director` 版は client 由来の値へ追記していたのに対し、あちらは自分が観測した RemoteAddr で
  置き換える (stdlib の doc が「追記したければ呼ぶ前に inbound からコピーしろ」と明示)。
  詐称された chain を流さない方向なので、この挙動で固定した。(c) **`Rewrite` 側は
  `cleanQueryParams` が無条件に走る**ので、`;` や不正な `%` を含む query は該当 param が
  落ちる (通常の Vite クエリでは無変化。レビューが実測)。
  `newViteProxy` にはテストが 1 つも無かったので 2 本足した。変異は 5 形で、4 形が検出・
  1 形 (`r.Out.Host = remote.Host` を足す) は意図どおり非検出 = 冗長の裏取り。
  **`echo` の `LoggerWithConfig` だけ移行しない** (**同日に移行した**。上の entry)。移行先の
  `RequestLoggerWithConfig` には
  **`CustomTagFunc` に相当するものが無い** (フィールドを全列挙して確認)。現在の設定は
  `${uri}` をそのまま出すと `?i=<token>` が残るのを避けるために `${custom}` + `redact.URI`
  を使っており、`LogValuesFunc` で書き直すと**間違えたときに有効な credential が
  アクセスログに残る**。**「出力形式が構造化に変わる」は誤り** — あちらでも
  `LogValuesFunc` の中で同じテキスト行を組み立てられる (初稿はそう書いてレビューに
  実測で否定された)。本当の理由は**この配線を固定するテストが 1 つも無かった**こと
  (`grep CustomTagFunc --include=*_test.go` が 0 件) で、移行時に壊しても誰も気付けない
  状態だった。`accessLogConfig()` に切り出して `TestAccessLogConfig_RedactsToken` で
  出力そのものを見るようにしたうえで、移行自体は単独の変更として扱う。
  `//nolint:staticcheck` に理由を書いて抑えた (外すと 1 件出ることを実測)。**`//nolint` は
  行末に置く** — 独立行に置くと `e.Use(...)` の 10 行全体が死角になり、**まさに守りたい
  redact のコードが検査されなくなる** (実測: `CustomTagFunc` の中に SA1019 を仕込んでも 0 件。
  レビューで指摘され、行末へ移して 1 件出ることを確認した)。
  **射程外**: `QF*` / `S1016` (恒久。`unused` は同日に有効化した)。
  **`echo` の `LoggerWithConfig` も同日に移行した** (上の entry)。
- **2026-09-22**: `ST1003` (命名) と `ST1012` (error var 名) を有効化。**名前を変えても wire と
  DB は動かない** — `model.Meta` の `SMTP*` は gorm / json タグを持ち、`config.Config` は
  JSON 化される経路が無く (`config_dump.go` はキーを文字列リテラルで書く)、
  `PolicyCanSearchIPHistory` は定数「名」で値 `canSearchIpHistory` は据え置き。
  **裏取りは `secretfield-check`** — allowlist を `Meta.SMTPPass` へ追従させ、`make gates` が
  通ることで確かめた (旧名へ戻す変異で `TestModelSecretFieldsAreNotSerialized` が落ちる)。
  **`shapecheck` は根拠にならない** — あちらは `internal/entity` の DTO しか reflect しないので、
  `model.Meta` の gorm / json タグを壊しても緑のまま通る (レビューが実測)。初稿はこれを
  根拠として書いており、**空虚な確認を doc に固定するところだった**。
  **`driver.SkipRetry` -> `ErrSkipRetry` は公開 API の変更ではない** — `internal/queue/driver` は
  `internal/` 配下なのでプラグインから import できない。ただし **`asynq.SkipRetry` (7 箇所) は
  外部パッケージの同名**なので触らない。一時プレースホルダへ退避してから置換した。
  **word boundary だけでは型と変数を区別できない。** `assertAnError` を一括置換したところ、
  **`type assertAnError struct{}` (error を実装する型) を宣言する 3 ファイルと、それを使う
  1 ファイル、計 10 箇所まで巻き込んだ** — ST1012 が指摘したのは `internal/api/reversi` の
  var 1 件だけで、型は対象外。**リネーム前に「その名前が何として宣言されているか」を
  確認すること。** 置換は散文にも当たる (`non-SkipRetry` が `non-ErrSkipRetry` になった)。
  **`test/e2e_federation` のパッケージ名だけ除外した。** ST1003 が指摘するのはパッケージ名で
  ディレクトリ名ではないが、Go の慣習では揃える。ディレクトリ名まで変えると `internal/` の
  実コード 2 ファイルを含む 24 箇所に波及するうえ (数え方:
  `git grep -oI e2e_federation -- '*.go' | wc -l`。**code だけで数える** — doc を含めると、
  この数字に言及した doc 自身が数を変えてしまう。実際、初稿は doc 込みの値を書いて次の
  コミットで陳腐化させた)、外部から import されないテスト専用
  パッケージなので実益が無い。**CI のカバレッジ閾値は ImportPath の `/e2e` で判定する**
  (`ci.yml` の `pkg ~ /\/e2e/` で unanchored な部分一致) ので、**名前が `e2e` で始まる限り**
  0% 例外は維持される — `federatione2e` のように後ろへ回すと外れて 90% になる (実測)。
  実測は ST1012 が 4 種 301 箇所 (`stubError` 112 / `SkipRetry` 182 / `stubReactionError` 3 /
  `assertAnError` 4。数え方: develop で `git grep -oIw <名前> -- '*.go' | wc -l` を足し、
  温存した `asynq.SkipRetry` 7 と戻した `assertAnError` 10 を引く) + 死んだアンカー行 1 の削除 (`var _ error = errors.New("compile-time
  anchor for errors import")`。`errors` は同ファイルの他 2 箇所で使われており不要だった)、
  ST1003 が 21 種 189 箇所。
  **新しい名前が既に在るかも先に測る。** `errStub` は `internal/api/invite` に元から
  あった (今回の対象 10 パッケージに含まれないので衝突しなかっただけ)。同一パッケージ
  だと再宣言でビルドが落ち、別パッケージだと黙って似た名前が増える。
  **`config.Config` は tag を 1 つも持たない**ので、marshal した瞬間に Go のフィールド名が
  wire になり秘密も一緒に出る。今は到達する経路が無いことを実測 (`MarshalJSON` に panic を
  仕込んで全テストを回しても発火しない) で確かめたが、**担保はコメントだけ**なので型宣言の
  直上にその旨を書いた。ゲート化は別途。
  **射程外**: `QF*` / `S1016` (恒久。`SA1019` と `unused` は同日に有効化した)、
  `test/e2e_federation` のパッケージ名、`config.Config` を marshal させないゲート。
- **2026-09-22**: `lint` job に `golangci-lint` を追加。`make help` の target は 138 → 139。
  **自分のコードを見る Go の静的解析が `go vet` だけだった。** `go vet` は「明らかに壊れて
  いるもの」しか見ないので、`errcheck` / `staticcheck` / `ineffassign` が拾う層が空いていた。
  **`staticcheck.checks` は golangci-lint の既定を置き換える。** 既定は
  `[all, -ST1000, -ST1003, -ST1016, -ST1020, -ST1021, -ST1022]` なので、`[all, ...]` と
  書くとそこに入っていた 6 つが**黙って有効になる** (ST1016 だけを意図的に残し、残り
  5 つを書き戻してある)。初版はそれに気付かず、`comments`
  プリセットで 369 件を抑止しながら設定側で ON にする、という循環になっていた
  (敵対的レビューで実測された)。既定の無効化も明示的に書き出して意図を 1 箇所へ。
  `ST1016` (レシーバ名の統一) だけは**意図的に有効**にしてある (実際に 2 件見つけて直した)。
  **除外プリセットは `std-error-handling` だけを入れる。** 残り 3 つは実測で無意味か有害:
  `common-false-positives` は中身が全部 gosec 向けで、gosec を有効にしていないので 100% 死んで
  いる。`legacy` は 4 つのうち 2 つが gosec、残りは govet の unsafe.Pointer 誤用と SA4011 で、
  **本物の指摘を消す方向にしか働かない**。`comments` は上の明示的無効化で不要になった。
  `std-error-handling` が落とすのは `Close` / `Flush` / `print` / `os.Remove` 等だけで、
  **DB 書き込みやパースの戻り値には当たらない** (ルール定義を直読して確認)。
  **既定の打ち切りを外す (`max-issues-per-linter: 0` / `max-same-issues: 0`)。** 同一メッセージ
  3 件・linter あたり 50 件で**切り詰める** (0 件にするわけではないので赤が緑になることはない)。
  害は「直すたびに隠れていた分が出てくる」ことで、**全部直してから有効化するという運用が
  成立しない**。実際これに気付かず測定を 3 回やり直した。**同日の CodeQL entry が射程外に
  挙げた「130 件」もこの打ち切りが効いた値** — errcheck がちょうど 50 (= linter あたりの
  既定上限) なのがその印。
  **`make check` も required check に揃える。** `fmt` / `lint` / `test` だけだと、`lint` job が
  回す actionlint と golangci-lint が手元で一度も走らない (レビューで指摘)。
  **段階的に有効化する。** 現行設定 (本番 / テスト) での実測は `ST1003` 34 件 (16 / 18)、
  `ST1012` 14 件 (1 / 13)、`SA1019` 10 件 (6 / 4)、`unused` 20 件 (1 / 19)。**`ST1003` /
  `ST1012` / `SA1019` はこの日のうちに有効化した** (上の 2 entry)。`QF*` 28 件 (25 / 3) と
  `S1016` は**恒久的に無効** — 前者は好みのリファクタ、後者は「同じ underlying type なら
  構造体変換にできる」という提案だが位置ベースになるので、**片方の struct だけ並べ替えると
  コンパイルが通ったまま値が入れ替わる**。
  **実バグは 1 件も出なかった。** 疑わしい 4 code (5 箇所) を追ったが、`SA9010` と `SA1012` は誤検知
  (副作用目的の呼び出し / nil 安全性を確かめるテストそのもの)、`SA4004` と `SA4010` は
  死んだ構造と死んだ収集だった。**これはバグ発見ではなくハイジーンの整備。**
  **機械的な一括置換は壊れる。** 規則でまとめて処理したあと全テストを回して **2 件の実害**が
  出た — `TestUserList_MissingListID` は `Init` の**失敗が仕様**なのに `require.NoError` で
  包んでしまい、`TestAddContext_IndependentSlices` は足したアサーションが**恒真**だった
  (`len(append(s,x)) == len(s)+1`)。**どちらも `go vet` では捕まらず、テスト実行と変異検証で
  初めて出た。** 後者はレビューの代替案 (`assert.NotContains`) も不十分で、`append` は相手の
  長さより後ろへ書くので backing array を共有していても見えない (実測で変異が素通り)。
  既存要素の書き換えで直接見る形に直し、変異検証に合格させた。
  **`typecheck` が落ちると他の linter が全部黙る。** 自前プラグインを入れている手元では
  `cmd/misskey/plugins_generated.go` が private module を import するので `GOWORK=off` だと
  そうなる (実測で無関係なパッケージの指摘が消えた)。`make golangci-lint` は生成物を退避して
  戻す (trap 付き。**`cp -p` にしないと** mktemp の 0600 を引き継いで mode が 644 → 600 になる)。
  **`go.work` が変えるのは解析対象ではなく build list。** `./...` は module 境界を越えないので
  workspace があっても `plugins/` は lint されない (実測で package 数 207 が一致) が、
  **依存の選択版は変わる** — workspace 内の module の require が MVS に参加するため
  (実測で `golang.org/x/telemetry` が 2025-10-08 → 2026-07-08)。CI は go.work を持たないので、
  `GOWORK=off` を外すと手元だけ別版を解析することになる。
  **`make lint` (go vet) は golangci-lint の govet にほぼ包含される** — `go tool vet` の
  35 analyzer は全て golangci-lint v2.13.2 の既定に含まれ、差は `inline` 1 つ (golangci 側のみ)。
  同じ解析を 2 回回しているが、CI の `Vet` step と 1:1 に対応させるため残してある。
  **射程外**: `QF*` / `S1016` (恒久。`ST1003` / `ST1012` / `SA1019` / `unused` は同日に
  有効化した)、
  別 module の `plugins/`。**`make check` と `lint` job のドリフトを止めるゲートは置いていない**
  (#2841 の `testflags-check` に相当するもの)。`make check` が回すのは `lint` job の静的検査 4 つで、
  `Check duplicate test fixture IDs` と `build` job (`go build ./...` / `make plugin-vet`) は含まない。
- **2026-09-22**: `dependency-review` workflow を追加し、あわせて **`go.sum` の検証方法の記述を訂正**した。
  **`GOFLAGS=-mod=readonly go build` では go.sum を検証できない。** Section 3 と
  `docs/development.md` がそう書いていたが誤り。**Go 1.16 以降 `-mod=readonly` は既定値**なので、
  素の `go build` と同じものを実行しているだけだった。効いていないのは `go.work` のほうで、
  workspace があると `go.sum` ではなく `go.work.sum` が使われる。**実測**: `go.sum` から
  `gorm.io/gorm` の 3 行を消して、(a) `go.work` あり → 素の `go build` も
  `-mod=readonly` も**どちらも exit 0** で素通り、(b) `go.work` なし → どちらも exit 1 で
  `missing go.sum entry`。手元で確かめるなら **`GOWORK=off go build`**。
  **CI には何も足さなくてよい。** `go.work` は `tools/pluginbuild` の生成物で gitignore 済み
  なので、`build` job の `go build ./...` は既に go.sum を検証している。「CI に検証が無い」と
  思って step を足すところだったが、実測したら既に在った。**裏取りせずに CI を足すと、
  効いていない検査が増えるだけになる。**
  `dependency-review` のほうは**時点と射程が `vulncheck` と違う**。あちらは develop に入った
  後の状態を到達可能性で絞って見るが、こちらは PR の差分を base と比べるので**入る前**に
  気付ける (代わりに到達可能性は見ない)。`fail-on-severity: high` から始める — moderate まで
  落とすと到達不能なものまで止めることになり、依存を上げるだけの PR が通らなくなる。
  **PR へコメントさせない** (`pull-requests: write` を要るので、権限は `contents: read` の
  ままにする)。required には**含めない** — 見ているのは差分だが、判定に使う advisory DB は
  GitHub 側で更新されるので、同じ差分でも後から赤くなりうる。
  **あわせてリポジトリ設定の Secret scanning と Push protection を有効化した** (どちらも
  `disabled` だった)。公開リポジトリなので無料で、コードもワークフローも要らない。
  push protection は**コミットされる前**に弾くので、「push してから revoke して履歴を
  書き換える」という一番つらい復旧を避けられる。`secretfield-check` まで作って秘密の露出を
  気にしている以上、ここが無効なのは一貫していなかった。**`non_provider_patterns` は
  有効にしていない** — 秘密鍵などの汎用パターンを見る枝で、Ed25519 / HTTP 署名のテスト
  フィクスチャが誤検知されうるため、実態を見てから判断する。
- **2026-09-22**: CI に `codeql` workflow と `lint` job の actionlint を追加。`make help` の target は 137 → 138。**自分のコードを見る静的解析が `go vet` だけだった。**
  `vulncheck` は依存しか見ず、`make gates` の 21 本は「この形を禁じる」と自分で書いたものしか
  見ない。テストは「書いた振る舞いがその通りか」しか見ないので、**書いていない分岐**と
  **通ってはいるが危険な形**が残る。CodeQL はそこを埋める。
  **見るのは `go` と `actions` の 2 つだけ。** submodule の外にある .ts/.js/.vue は実測 340
  ファイルで、その大半が `tests/playwright/specs/**` (うち 189 は upstream 由来の UI spec)。
  Python も `tests/` の検証基盤。production のコードではないので入れると**ノイズにしかならない**。
  fork frontend は checkout していないので対象外 (upstream のコードで、こちらが直せる範囲ではない)。
  **autobuild を使わない。** autobuild が回すのは root module だけだが、同梱プラグイン
  (`plugins/*/go.mod`、tracked は 2 つ) は**別 module**なので `go build ./...` に含まれない
  (`plugin-tests` job が独立しているのと同じ理由)。`git ls-files` で列挙して個別にビルドする。
  `go.work` は `tools/pluginbuild` の生成物で gitignore 済みなので、clean checkout では root
  module だけがビルドされる。**clean worktree で実測して確認した** (root / `plugins/status` /
  `plugins/trustlevel` の 3 つとも exit 0)。
  **required には含めない。** CodeQL のクエリパックは CLI の更新で増えるので、コードを 1 行も
  変えていない PR が新しいクエリで赤くなる (`vulncheck` を required から外しているのと同じ
  理由)。代わりに weekly の schedule を持たせる — PR トリガーだけだと、触っていないコードに
  対する新規検出が永久に出てこない。**`ci.yml` に相乗りさせない** — あちらは workflow 直下で
  `contents: read` に絞っており、CodeQL は `security-events: write` を要る。
  **actionlint は逆に required に入れる。** 版を固定すれば検査内容が動かないので、`gofmt` や
  `go vet` と同じ扱いにできる。**CodeQL の `actions` とは別物** — あちらは script injection
  などのセキュリティを見るが、式の typo・存在しない `needs` 参照・`runs-on` の誤りは見ない。
  workflow のミスは動かすまで分からない (#2940 で実際に踏んだ) ので、静的に落とす側が要る。
  **導入時に 10 件出た。うち 1 件は実バグ** — `echo "... \`fork frontend の独自変更\` ..."`
  が二重引用符の中にバッククォートを置いており、コマンド置換として実行されていた
  (実測で `fork: command not found` が出てメッセージが欠落する)。同じ step の別の行は
  `\`` でエスケープ済みで、片側だけ漏れていた。残り 9 件は SC2086 の引用漏れ 4、
  sed の後方参照と Markdown のバッククォートに対する SC2016 の誤検知 3、`$(echo $x)` の
  SC2116 / SC2006 が各 1。誤検知は `# shellcheck disable=` を**その行の直前**に置く
  (ブロック先頭に置くと以降の本物まで黙る)。
  **shellcheck が無いと黙って検査が減る。** actionlint は `run:` の中身を shellcheck へ
  渡すが、無ければその分だけ落として**成功で返す**。CI の ubuntu-latest には入っているので、
  **手元だけ通って CI で落ちる** — 実際に踏んだ (手元 0 件 / CI 10 件)。
  `make actionlint` は shellcheck が無ければ落とす (`MK_PLUGIN_TESTS_REQUIRE_DB` /
  `MK_FRONTEND_GATES_REQUIRE_SUBMODULE` と同じ「skip を成功として扱わない」形)。
  **版の定義は Makefile に 1 つだけ置き、CI は `make actionlint` を呼ぶ。** CI 側に書き写すと
  #2841 (`make test` と CI の flag がずれていた) と同じドリフトが起きる。`@latest` にしない —
  新しい検査が増えたときに、workflow を触っていない PR が赤くなる。
  **射程外**: `javascript-typescript` / `python` (上記)、fork frontend、`golangci-lint` が
  見る層 (実測で 130 件 = errcheck 50 / staticcheck 45 / unused 20 / ineffassign 12 /
  govet 3。本番 47・テスト 83。別途対応する)。
- **2026-09-22**: `make gates` に `sqlbind-check` を追加。`make help` の target は 136 → 137。**値をクォートで囲んだリテラルへ差し込まず bind する**、を固定する。
  chart の unique 配列は**行の値そのもの** (外部由来の文字列を含みうる) なので、
  `internal/core/chart/repository.go` の `ApplyDeltas` が `?::varchar[]` で bind して
  いるのが唯一の防波堤になっている。初版 (`50ba697b`、2026-04-09) からその形だが、
  **担保が無かった** — 実測で、配列リテラルを書式へ差し込む形に変えても既存の実 DB
  テストは**緑のまま通った** (`TestIntegration_GormRepository_UniqueIncrementApplyDeltas`
  の値が `u1` / `u2` だけで、構造上意味を持つ文字を含んでいない)。
  **「これは SQL か」を判定しない。** 初版はキーワードの部分一致で SQL らしさを
  判定していたが、敵対的レビューで**両方向に壊れている**ことを実測された。偽陽性:
  `"plugin '%s' returned no values"` のような普通の英文が `values` に当たり、
  「プレースホルダで bind すること」という**事実と逆の診断**が出る (allowlist の理由欄は
  「なぜ SQL として解釈されないか」を書かせる形なので、非 SQL には書きようがない)。
  偽陰性: `'%s'::varchar[]` のような**断片**はキーワードに 1 つも当たらず**収集すら
  されない** — このリポジトリには SQL を断片ごとに組んで `strings.Join` する経路が
  (chart / fsck / maintenance など) あり、そこが丸ごと盲点だった。実際、`Insert` の
  placeholder を `fmt.Sprintf` へ変える変異は**静的ゲートも chart の全テストも
  緑のまま通った** (レビュー側が使い捨て probe で実測し、値が SQL テキストに載ることを確認)。
  判定を「クォートで開いた区間に動詞が在るか」だけにすると射程は広がり、偽陽性は減る。
  **代わりに網は広がる** — 値をクォートで囲んだだけのメッセージも該当しうるので、
  診断は「bind しろ」と決めつけず、SQL でない場合の逃げ道も示す。
  **「プレースホルダが在るか」では守れない。** ダミーの `?` を 1 つ足し、
  `pgArrayLiteral` とは**別名の独立したビルダ**で配列をリテラルへ畳み、要素数で
  分岐させる変異は、**静的ゲートも chart の全テストも緑のまま通った** (実測。
  リテラルが SQL テキストに載ることは手元で確認した。閾値をテストの値数より上に
  置くと振る舞い側も外れる)。
  `ApplyDeltas` が組む**書式集合そのものを pin** すると、書式を**変える**形は落ちる。
  **ただし pin は「その Sprintf が在るか」しか見ない** — 呼び出しを `_ =` にして
  残したまま、実際の組み立てを文字列連結へ移す変異は**静的ゲートを素通りし、
  実 DB の往復テストだけが落とす** (実測)。静的な走査で意図的な回避まで塞ぐことは
  できないので (#3135「名前で見る走査は名前で避けられる」)、ここが受け持つのは
  **普通に書いたときに踏む形**で、意図的な回避は振る舞い側が受ける。
  **書式は定数連結を畳んでから見る。** 長い SQL を `"..." + "..."` で折り返すのは
  普通の書き方で、第 1 引数が `*ast.BasicLit` のときだけ拾う形は**その書式を 1 件も
  収集しない** (実測)。違反ではなく**不在**になるので、違反集合の比較では気付けない。
  **名前を決め打ちにしない。** `pgArrayLiteral` の行き先を `append(args, ...)` で
  見ていたため、局所変数を `params` へ改名しただけでゲートが落ち、しかも**既に
  bind しているコードへ「バインド引数で渡すこと」と事実と逆の指示**を出した (実測)。
  `Exec` / `Raw` へ可変長展開している識別子を解決して突き合わせる形に直した。
  **名指しの一覧は `map` で回さない。** イテレーション順が非決定的なので、走査が
  縮んだときに報告されるサイトが実行ごとに変わる。ソート済みの slice で回し、
  未検出を集めてから 1 回で報告する。**違反の報告も `require` にしない** — 先に
  止まって allowlist の死んだ entry の診断が出ない (#3135 と同じ型)。
  **数え方**: `internal` / `cmd` / `plugin` の非テスト Go を AST で走査し、`fmt.Sprintf`
  の書式リテラル (定数連結は畳む) をサイト単位で数えて **186 件** (ユニークキー 103、
  うち人工ソース 6 サイト)。違反は **4 サイト / 3 キー**で、内訳は人工ソース 2 キーと
  `internal/server/frontend.go#renderFrontendShell` の 1 キー (JS 生成が 2 サイト。
  入るのは Vite manifest のエントリ名とビルド版数)。いずれも allowlist 済みで、
  **本番の未許可の違反は 0**。
  **検出の枝は人工ソース (`internal/entitycompat/sqlbindfixture`) で固定する。** 本番の
  違反が allowlist 済みの 1 キーしか無いので、本番だけを見ていると枝が一度しか通らない。
  `testdata/` に置くと Go ツールチェーンが無視して `go vet` の書式検査も通らないので、
  コンパイルされるパッケージに置く (`_test.go` を持たないパッケージは `go list` の
  テスト対象にも CI のカバレッジ閾値にも入らない)。
  **gofmt はトップレベル宣言の doc コメントの中のクォート 2 連だけを typographic quote へ書き換える**
  (関数の中のコメントには効かない。`b7c419f9` と同じ形)。この作業でも実際に踏んだ。
  **変異検証は 19 形 (production 13 / ゲート側 6)。** production: 素の補間 /
  クォートを書式の外へ出す / decoy (独立ビルダ + ダミーの `?` + 要素数で分岐) /
  `Insert` の placeholder を `Sprintf` 化 / `pgArrayLiteral` のエスケープを外す 3 形
  (`\` のみ・`"` のみ・両方) / 定数連結で折り返した違反形 / 値をクォートで囲んだ
  英文メッセージ (**検出される**。SQL でないので allowlist に値の出どころを書く形) /
  allowlist のキーと同名の宣言を同じファイルへ足す / **落ちてはいけない 2 形**
  (局所変数 `args` を `params` へ改名 / `lit := pgArrayLiteral(...)` と局所変数に受ける) /
  **既知の穴 1 形** (pin 対象の `Sprintf` を `_ =` で残したまま組み立てを文字列連結へ
  移すと**静的ゲートは素通りし、実 DB の往復テストだけが落とす**)。
  ゲート側: `%%` の skip を外す / クォートの状態を反転させない / 定数畳み込みを
  無効化 / 走査ルートから `internal` / `cmd` / `plugin` をそれぞれ外す 3 形
  (`cmd` と `plugin` は名指しの一覧が `internal` に偏っていると黙って通る。
  レビューで指摘されて両 root のアンカーを足した)。
  **クォートが釣り合わない書式は「判定できない」として報告する。** パリティの
  状態機械なので、`-- don't touch` を先頭に置くだけで以降の内外が反転し、**後続の
  本物の違反が 0 件に化ける** (実測)。逆に `can't resolve %s` のような英文は、
  存在しないリテラルの内側だと判定される。安全側へ倒さず、最初のクォートより後ろの
  動詞をすべて報告する形にした (corpus は不変)。
  **キーの曖昧さも見る。** `<file>#<func>` は Go がパッケージ関数と別型のメソッドに
  同名を許すので衝突しうる。ただし同名自体は正当なので (`internal/activitypub/types.go`
  は別々の型に `UnmarshalJSON` を持つ)、落とすのは**その key を allowlist か名指しの
  一覧が使っているときだけ**にしてある。
  **分担が分かれているのが要点** — 書式の退行は静的ゲートが落とし、エスケープの
  退行は書式の形が変わらないので静的ゲートには見えず、`TestPgArrayLiteral_*` と
  実 DB の往復テストが受け持つ。逆に述語の退行は実 DB では何も起きない。
  実 DB テストが担うのは **Go の golden と往復では表現できない側** — 配列リテラルの
  入力パーサは PostgreSQL が権威なので、`pgArrayLiteral` と `parsePgTextArray` を
  対称に壊して Go 内では辻褄が合う形でも、往復の値が変われば落ちる
  (`\` / `"` については `TestPgArrayLiteral_EscapesQuotesAndBackslashes` が
  書き側の出力を golden で pin しているので、そちらでも落ちる)。
  **射程外**: taint 解析はしない (#2644 と同じ理由)。書式を `const` や変数に入れた
  `Sprintf`、書式の途中に変数を連結した形、`fmt.Sprintf` 以外 (`Fprintf` / `Errorf`)、
  `fmt` の別名 import、文字列連結や `strings.Builder` で組む SQL、`Raw` / `Exec` へ渡す
  非リテラル、別 module の `plugins/`、`internal` / `cmd` / `plugin` 以外のツリー。
  **名前で見る走査は名前で避けられる** (#3135) ので、確実に落とすのは
  「普通に書いたときに踏む形」に寄せてある。
- **2026-09-21**: `make gates` に `ipshape-check` を追加 (#3136)。`make help` の target は 134 → 135。**#3066 の完了条件「IP 情報が一般ユーザー向け API や連合へ露出しない」の担保が shapecheck の golden 照合しか無かった** — `UserLite` に `json:"lastIPs"` を足して `make shapecheck` は PASS する (実測)。**いまは漏れていない**が、担保が無かった。
  **reflect で型を手で並べない。** 初版はそう書いて `entity.MeDetailed` (= `/api/i`) を落としていた。**AST で全 struct の json タグを読む**形にすると型を列挙しないので落としようがない。走査は `internal/entity` / `internal/activitypub` だけでは足りない — **handler が自分のファイルに宣言した response struct も stream のイベント payload も、そのまま wire の形になる**ので `internal/api` / `internal/server` / `internal/stream` も入れる。実測は**要素 2,332 / ユニークキー 755** (数え方: `scanJSONTags` が `publicShapeDirs` 全体で返した要素数と、その `Key` のユニーク数)。IP を指すキーを持つのは 11 件で、10 件は `internal/api/admin` の IP 照会 API (モデレーター + policy + scope の 3 段)、1 件はレスポンスに出ない入力構造体。
  **語の切り方は片側に寄せると必ず穴が開く。** 大文字のたびに割ると `lastIPs` が `last` + `i` + `ps` になって Go の命名規約に素直な名前だけが素通りし、「小文字/数字の直後の大文字」だけにすると `IPAddr` / `IPHash` / `IPList` が 1 語に潰れて素通りする。**2 稿にわたって片側ずつ落とした** (どちらも敵対的レビューで実測)。境界は 2 つ要る — 小文字/数字の直後と、**大文字が 2 つ以上続いた後の、小文字が続く大文字**。さらに **`address` 系の alternative も要る** — 割れるのは `IPAddress` だけで `IPaddress` / `ipaddress` は1 語に潰れる。「割るから要らない」と書いて一度落とし、3 周目で実測された。
  **キーの走査だけでは入れ子が見えない。** `Session *model.Signin` を 1 フィールド足すだけで `encoding/json` はその中の `ip` を出す。さらに **`model.User` は自分では IP を持たないのに `Avatar *model.DriveFile` 越しに `avatar.requestIp` を出す**ので、1 段だけでは足りない。`internal/` 全体から「自分の JSON キーに IP を持つ名前付き struct」を**導出して推移的に追う**。**interface と関数型のフィールドは伝播させない** — 混ぜると repository 一式が汚れて 18 件が誤検出になる (実測)。
  **収集ロジックは `encoding/json` の実挙動と突き合わせる。** 人工ソースは `testdata/` ではなく**コンパイルされるパッケージ**に置く — あちらは Go ツールチェーンが無視するので `json.Marshal` と比べられず、期待値を手で書くことになる。拾う形は 5 つ — タグ無し exported はフィールド名、`json:"-"` と非公開は出ない、**タグ無しの匿名埋め込みは昇格**、**タグ付きの匿名埋め込みはタグ名 1 つ**、**struct でない名前付き型の埋め込みは型名**。**昇格の枝は、埋め込まれる型が非公開でないと固定できない** — exported だとその型自身の宣言からも同じキーが出るので、埋め込みを消しても突き合わせが食い違わない (実測)。
  **「違反 0 件が正常」な検査は、抽出側にも下限が要る。** `scanJSONTags` には下限を置いたのに `typeRefs` (参照側) に置かず、**package 修飾の収集を落とす 1 行で `*model.Signin` の漏れごと素通りした** (敵対的レビュー 2 本が独立に実測)。allowlist にも**死んだ entry の検査を必ず付ける** — 付け忘れた側で、**実在しない entry を捏造した状態が全テスト緑のまま通った**。
  **走査が縮んだことは、件数と代表キーでは見られない。** 代表キーが大きな 2 ファイルに偏っていたため、**14 ファイルを落とす変異が本物の漏れごと素通りした**。`json:"` を含むファイルは 1 件以上寄与することを**ファイル単位**で要求し、truth は AST ではなくテキスト走査から採る (AST が壊れれば必ず食い違う)。**その truth 側にも下限が要る** — 1 ファイルに縮める変異で検査が丸ごと無意味になる。
  **射程外**: `map[string]any` を手で組む経路 (`entity.PackSignin` / nodeinfo)、`datatypes.JSON` の中身、**`publicShapeDirs` の外に宣言された型を handler が `c.JSON` にそのまま渡す形** (`/api/server-info` が返す `serverstats.PublicStats` が実例。`internal/core` を走査に足すのは採らなかった — IP を持つ内部の入力構造体が 11 件流れ込んで allowlist が倍増し、本物の signal が埋もれる)、`remoteAddr` / `CDNIPs` / `ip4s` のように語として `ip` を取り出せない綴り。
- **2026-09-21**: `make gates` に `iprecord-check` を追加 (#3135)。`make help` の target は 135 → 136 (同日に入れた `ipshape-check` の次)。**#3105 の関連アカウント検索は `user_ip` の観測だけを見るので、失敗したサインインの IP がそこに入ると第三者が他人の関連候補を作れる** — 攻撃者が対象アカウントの ID で自分の IP から失敗を繰り返せば、その IP が対象の「使用した IP」として記録され、攻撃者自身のアカウントが候補に並ぶ。**いまは入っていない**が、担保が無かった。
  **振る舞いテストだけでは守れない。** 守りたいのは「どこからも呼ばれていない」という構造的な性質で、endpoint ごとのテストは叩いた経路しか見ない。実際、`/api/signin` だけを叩くテストは `SigninFlow` (同梱フロントが実際に使うほう) に記録を足す変異を素通りさせた (実測)。非同期化 (`go h.ipRecorder.Record(...)`) は**初版の振る舞いテストでは不安定にしか捕まらなかった** (実測で `-race` 5 回中 3-4 回)。`fail()` 到達を `require.Eventually` で待つようにしてからは 10/10 で落ちる。
  **静的ゲートだけでも守れない。** allowlist のキーは `<file>#<func>` なので、**allowlist 済みの関数の中に `Record` を足す**変異も、そこから allowlist 済みの `RecordSuccessfulSignin` を呼ぶ変異も、call site の一覧としては何も変わらない。しかも `signin-with-passkey` は振る舞いテストを 1 つも持っていなかったので、**その組み合わせが両側とも空白だった** (敵対的レビュー 2 本が同じ穴を別経路で実測)。passkey の失敗 4 分岐にrecorder のアサーションを置き、`RecordSuccessfulSignin` の呼び出し側も別の allowlist で固定した。
  **名前で見る走査は名前で避けられる。** `rec := h.ipRecorder.Record; rec(a, b)` と書くと呼び出し側が`*ast.Ident` になり、引数 2 の `.Record(` としては現れない。**呼び出さずに値として持ち出す形も call site として数える。** package 変数に入れたクロージャの中も見る (`FuncDecl` だけを走査する形では見えない)。**この対処を片方の走査にしか入れず、2 周目で指摘された** — 成功入口 (`RecordSuccessfulSignin`) の走査に同じ枝が無く、**署名検証より前に呼ばれる `resolvePasskeyUser` にメソッド値で仕込む変異が静的ゲートも振る舞いテストも素通りした** (そこは攻撃者が送った `userHandle` で user を引く段階なので、被害者の `user_ip` に自分の IP を入れられる)。走査は 1 本に統合した。**型の位置とフィールドアクセスは除く** — `a.Record{}` / `h.Record.Val` まで call site にすると診断が事実と無関係なことを断定する。**枝そのものは人工ソース (`internal/entitycompat/recordfixture`) で固定する** — 実データにその形が無いので、枝を消しても実データからは何も起こらない。
  **call site の列挙だけでは順序が固定できない。** パスキーの記録を `fail()` より前へ動かしても場所は変わらないので allowlist は通る。順序テストは**最初の `Record`** を基準にする — 最後のものを見る形だと、既存の呼び出しを残したまま前にもう 1 つ**足す**変異が素通りする (実測)。
  **件数の下限は診断を壊す。** 「allowlist の長さ以上か」は dead-entry 検査より論理的に弱く(下限が落ちる状況では必ず dead-entry も落ちる)、しかも `require` なので**先に止めて正しい診断を奪う**。call site を正当に 1 つ消しただけでも「抽出が壊れている」と事実と逆を出した (実測)。実在する call site を名指しで要求する形 (`secretfield-check` の `mustDetectSecretFields` と同じ) に替えた。
  **allowlist の理由も裏取りする。** 「`RecordSuccessfulSignin` が唯一の成功入口」と書いたが、**すぐ下の entry 自身が「passkey は経由せず直接呼ぶ」と書いており矛盾していた**。同じ型で「呼ぶのは 2 箇所」も誤り (実際は 3 箇所)、`deliveryhealth` の関数名 `Record` も推測 (実際は `RecordDelivery`。ゲート自身に訂正された)。
  **「テストにできない」も裏取りする。** 失敗の記録が非同期だから `fail()` 到達を確かめられない、と書いたが誤りだった — `MockSigninRepository` は mutex 付きの `Len()` を公開しており、**同 package の既存テストが既に `require.Eventually` で待っている**。到達確認を入れないと、将来 `fail()` の手前で返すようになっても「IP を記録しない」が自明に真になって空虚化に気付けない。
  **射程外**: 見るのは `internal/` だけ。**`user_ip` に実際に書く `UserIPRepository.Observe` (3 引数) は引数の数で絞る走査に入らない**。
- **2026-09-17**: `make gates` に `nulparam-check` を追加 (#3025)。`make help` の target は 133 → 134。**認証済みの一般利用者が、パラメータに NUL を 1 文字入れるだけで 500 を起こせた** (`federation/*` や `users/clips` など**未認証**で叩けるものもあった)。NUL はどの列にも入らないうえ、**比較の右辺に置くだけで PostgreSQL がクエリごと落とす** (手元の simple protocol で SQLSTATE 08P01、本番の pgx extended protocol で 22021)。`IsNotFound` でもないので handler は `JSONInternalError` へ倒す。NUL は JSON のエスケープで普通に送れる。#3018 / #3022 は主に「列に**書く**値」を塞いだ (申請 ID のように引く側も一部含む) が、**カーソル / id / 検索語は系統的に残っていた**。
  **upstream は「全部 500」ではない。** ajv に `misskey:id` (`/^[a-zA-Z0-9]+$/`) を登録しているので、その format を持つ `sinceId` / `untilId` / `userId` / `noteId` は**列に届く前に 400 で弾かれる**。format を持たない値 (検索語や `users/show` の `username` など) だけが 500 になる。**この裏取りをせずに「upstream は 500」と書き、敵対的レビューで指摘された。**
  **役割ごとに答えが違う。** カーソルは 400 (空に倒すと「カーソル無し = 先頭から」になり、**利用者の指定と無関係なページを正しい応答として返す**)、単体 id と完全一致で引く値は not-found (一致しえない値は「無い」が事実)、検索語は**空の結果**(「その語を含む行が無い」が事実で、利用者の入力が壊れているわけではないので wire に新しいエラーコードを足さない)。**issue の完了条件は「4xx になる」だったが、検索語だけは 200 + 空にしてある** — 理由は docs/divergence.md に書いた。**単体 id は upstream と code / id が違い、status も一致しない経路がある** (`users/show` は mk-go 404 / upstream 400) ので、これも divergence として記録した。
  **1 箇所で塞げるものとそうでないものがある。** カーソルは `id.NormalizeCursor` を 39 ファイルが通るので choke point になる (数え方: 非テストの `internal/` で `id.NormalizeCursor(` を含むファイル)。**signature を 3 値にしてコンパイルで全件の書き換えを強制した** (呼び出しは実測 88)。id と完全一致の値は共通の入口が無いので、repository の**単一行 lookup** (`Find*` / `Get*` が `(*model.X, error)` を返すもの) と `*ByID*` の 118 メソッドに guard を置いた。検索語は `escapeSQLLikePattern` が NUL を見ていないので、LIKE を組み立てる 16 関数に置いた。
  **#2792 に反しない。** 丸めているのは DB 障害ではなく、**引く前に分かっている「一致しえない」**という事実で、クエリを投げていない以上そこに隠れる障害が無い。逆に言うと、この判定を「引いた後」へ動かすと #2792 違反になるので、gate は**順序も見る**。
  **関数の契約を上書きしない。** 「無い」を `(nil, nil)` で表す lookup (`FindActive` / `FindPendingInvitation`) に `ErrNotFound` を返す guard を入れたところ、呼び出し側が err として扱って **`roles/assignment-show` が 500 のまま残った** (敵対的レビュー 2 周目で実測)。guard はその関数が既に持っている「無い」の表現に合わせること。
  **共有の入力構造体を書き換えない。** `buildV2Query` で `fq.RoleIDs` をフィルタ済みの値に書き戻したら、handler が同じ filter で `ListV2` の直後に `CountV2` を呼ぶため、**2 回目は guard もフィルタも素通りして絞り前の件数を返した** (同 2 周目で実測)。`filter.Query` がポインタなのが効いている。局所変数に受けること。
  **mock では検出できない。** `internal/testutil` の mock repository は NUL を渡しても普通に「見つからない」を返すので、**guard を外しても handler テストは緑のまま通る**。だから (a) 静的な gate、(b) 実 PostgreSQL に対する「引く前に弾いている」テストの 2 本で押さえる。後者は `IsNotFound(err)` が真であることに加えて**普通の入力が引けたまま**であることまで見る (空を返すだけの実装でも「NUL で空になる」テストは通る)。
  **gate は「どの値を見ているか」まで照合する。** 呼び出しの有無だけを見る初版は、敵対的レビューで**バグを再導入する 3 変異が全て素通り**することを実測された — (a) 複数の値を取る lookup で片方だけ guard する、(b) `storableIDs(ids)` と書いて**戻り値を捨てる** (式文として合法で `go vet` も黙る)、(c) LIKE に載る値そのものの guard を落として別の値だけ見る。**実際にその形で本番コードが漏れていた** (`ListUsers` が `Username` は見て `Hostname` を見ていなかった)。パラメータを 1 つ残らず guard の引数と突き合わせ、`storableIDs` は代入し直していること、**guard の分岐が実際に return すること**、**順序はパラメータごとに見ること** (関数単位で「最初の guard」と比べると 2 つ目以降が SELECT の後ろでも通る) まで見る形に直した。
  **カーソル側の gate も 3 つ穴が開いていた。** 「関数のどこかに `!ok` があればよい」形にしていたため、(a) 手前の `pagination.ResolveLimit` の `limitOK` に受け直すだけで黙る (**全 handler がその形の `!limitOK` を持っている**)、(b) guard を DB 呼び出しの後ろへ動かしても緑、(c) `if !ok { sinceID, untilID = "", "" }` と**空に倒して 200 を返す**形も緑。**「カーソルの値を使う前に `!ok` を見て抜ける」**に限定して塞いだ。**「直後の 1 文」に狭めると今度は正当な書き方を落とす** — guard を 2 つ積む / 条件を束ねる / `switch` の case に置く / ループで `continue` する、のどれもが偽陽性になり、しかも診断が「guard が無い」と事実と逆を指す。
  **「呼び出し側を見る」だけでは片側しか塞がらない。** `id.NormalizeCursor` の呼び出しを見る gate は、**そもそも呼んでいない handler** を視界に入れられない。実際 `list-mine` (一般ユーザーが叩ける) と `admin/emoji-application/*` の 3 つが `untilId` をそのまま `"id" < ?` に載せていた。`untilId` / `sinceId` を bind する handler 側を数える gate (実測 44) を別に足してある。
  **「引く前に弾く」の判定に「組み込み以外の呼び出し」を使わない。** `time.Now()` や `r.normalizeHost(host)` を guard の前に 1 行置いただけで落ちるのでは述語と意図が食い違う。DB とみなすのは `tx.` / `db.` / `q.` と、レシーバの**フィールド**越しの呼び出し (`r.db.` / `c.inner.`) だけにする。
  **AND と OR で落とし方が違う。** AND で畳む検索語は 1 つでも一致しえなければ全体が空 (`allStorable`)、**OR で畳む語は要素ごとに落とす** (`storableIDs`)。`multipleWordsToQuery` は後者だが、**`roleIds` を前者で書いて実測で壊した** — `&&` は overlap = OR なので、一緒に指定した他の role の一致まで消えていた (書き込み側の `normalizeEmojiRoleIDs` は #3018 で既に要素ごとに落としている)。
  **gate の対象集合は「LIKE を作る側」だけにする。** `multipleWordsToQuery` を escape helper の集合に入れると、その呼び出し側 (`buildV2Query`) に判定を要求してしまう — あの関数は自分の中で語ごとに落としているので偽陽性になる。集合から外しても、あの関数自身が `escapeLike` を呼ぶので中の判定を外せば捕まる。
  **射程外**: gate が見るのはカーソル (呼び出し側 88 + bind 側 44) / 単一行 lookup + `*ByID*` 118 / LIKE 16 で、値を受ける一覧系は見ていない。**guard を呼ばずに DB を触るメソッドが 311 残っている** (数え方は docs/divergence.md)。**実際に届く経路は測って個別に塞いだ** — 未認証で叩けるものだけでも `federation/followers` / `following` / `users` の `host`、`hashtags/users` / `show` の `tag`、`notes/reactions` の `type`、`users/clips` / `flashs` / `gallery/posts` / `pages` の `userId`。網羅ではないので、一覧系を足すときは受け取る値を自分で弾くこと。
  **変異検証は 25 形** (production 21 + gate の述語 4)。production: `colfit.Storable` を常に真 / `mute.List` の `!cursorOK` を `_` / `ListMine` の `!cursorOK` を `_` / `userRepository.FindByID` の guard を外す / `storableIDs` を素通し / `instanceRepository.List` の guard を外す / `multipleWordsToQuery` の語ごと判定を外す / `hashtags/search` の guard を外す / `hashtags/show` と `users` の guard を外す / `FindByIDAndUserID` の `id` だけ外す / `storableIDs(ids)` の戻り値を捨てる / `SearchMessages` の `query` だけ外す / `ListUsers` の `Hostname` を外す / `following` の `host` guard を外す / `buildV2Query` の `roleIds` フィルタを無効化 / `registry.Get` の `key` の guard だけ SELECT の後ろへ / `SearchUsers` の `query` の guard だけ escape の後ろへ / `buildV2Query` が `fq.RoleIDs` へ書き戻す / `FindActive` の guard を `ErrNotFound` に戻す / `note_reaction.ListByNoteID` の guard を外す / `clip.ListPublicByUser` の guard を外す。gate: カーソルの抽出 (`isCursorCall`) / lookup の対象判定 (`isGuardedLookup`) / LIKE の escape 集合 / `paramKindOf` の `*string` 枝。**下限の件数を持たせているのが要点** — 違反 0 件が正常な状態なので、抽出を壊しても「検出 0 件」と区別が付かない。**`hashtags/show` の handler テストは置いていない** — あちらは `err != nil` を丸ごと 400 に潰す既存実装なので、guard の有無で外から見える応答が変わらず空虚になる (実測)。repository 側のテストで押さえてある。

- **2026-09-12**: `make gates` に `submodulepin-check` を追加 (#2969)。`make help` の target は 132 → 133。**fork frontend の pin が doc と gitlink で食い違ったまま緑になっていた。** #2963 で `third_party/misskey` に commit して fork へ push し、`docs/divergence.md` にも新しい tag を書いたのに、**親リポの gitlink だけ古いまま CI 28 チェックが全て緑でマージされた** (#2965 で解消)。気付いたのはマージ後に `git status` を見たときで、検出が人手に依存していた。
  **実害の経路もある。** `Makefile` の `REVISION_LDFLAGS` は `git -C third_party/misskey describe --tags` で `MkGoFrontendVersion` を作るが、これは **submodule の working tree** を見るので、gitlink が遅れている窓に develop からビルドしたバイナリは古い tag を名乗りつつ doc は新しい tag を書いている状態になる。
  **SHA で突き合わせるのが要点。** doc に書いてあるのは tag 名だが、tag から SHA を解くには**ネットワーク** (`git ls-remote`) か submodule の checkout が要り、`make gates` はどちらも前提にできない。**pin 行に短縮 SHA を併記して親リポだけで完結**させると、`git ls-files -s -- third_party/misskey` が submodule 未初期化の worktree でも gitlink を返すので `make gates` に載る (実測: submodule が空の worktree で PASS し、そこで doc の SHA を変えると落ちることまで確認した)。
  **gitlink は index から読む (`ls-files -s`)。** doc は working tree から読むので、gitlink を `ls-tree HEAD` (= 直前の commit) から読むと**読み元が非対称**になり、submodule を `git add` して doc も直した**コミット直前の状態で必ず落ちる** — しかも診断が「`git add` しろ」= もう済ませた操作を指示する。`make check` はコミット前に回す決まりなので bump のたびに踏む (実測: 直近 30 commit のうち 13 が gitlink を動かしている)。CI は checkout 直後で index == HEAD なので検査は弱まらない。**敵対的レビューで指摘されるまで `ls-tree HEAD` だった。**
  **tag 名の正しさは CI の `build` job で見る。** #2963 の事故は「**tag だけ直して gitlink を忘れた**」形だったので、SHA 側を書き換え忘れると `make gates` は素通りする。`Check submodule commit is pushed` step に `git ls-remote` で tag → commit を解いて gitlink と突き合わせる判定を足した (実測 1.0 秒。annotated / lightweight 両対応)。あわせて doc 内の整合 (pin 行の tag == §4-2 の表の最終行) を `make gates` 側でも見るので、ネットワーク無しでも気付ける経路が 1 本ある。**「別の場所 (`frontend-check` 側) の仕事」と書いたが、そんな実装はどこにも無かった** — 裏取り無しの主張だったのでこの形に直した。
  **pin 行はちょうど 1 件であることも要求する。** `FindSubmatch` で最初の一致だけを採ると、前方に書式の例を書いた瞬間に本物の pin 行が検査対象から外れる (このゲートの失敗メッセージ自身が書式を提示するので、doc へ写す動機がある)。
  **実害の向きに注意。** 「バイナリが古い tag を名乗る」は症状であって実害ではない — develop を clone して submodule を取れば working tree は gitlink (古い方) に置かれるので、`describe --tags` が返す古い tag は**事実として正しい**。嘘をついているのは doc の側で、本当の実害は「**入れたつもりの frontend の修正が develop のビルドに入っていない**」こと。
  **変異検証は 8 形**: doc の SHA を 1 文字変える / pin 行を消す / SHA の併記だけ消す / 正規表現を空振りさせる / pin 行を 2 件にする / pin 行の tag を古くする / CI 側で tag だけ古くする / CI 側で存在しない tag を書く。**「アサーションを外す」「`ls-files` の結果を無視する」はテスト自身の変異なので「検出した」とは言えない** — それが示すのは「そのアサーションが load-bearing である (空虚でない)」ことだけ。

- **2026-09-12**: `make gates` に `secretfield-check` を追加。`make help` の target は 131 → 132。**モデルをそのまま JSON 化する経路があるので、`json:"-"` が唯一の防波堤になっているフィールドがある。** 実測で `internal/model` の該当タグを外しても `make gates` も全テストも緑のままだった。**名前だけでは判定できない** — `Meta` の captcha secret は `admin/meta` が管理画面へ返すうえ moderation log にも載るし、drive の `accessKey` は URL の構成要素で秘密ではない。そこで #2792 と同じ **allowlist に書かせる**方式にした。
  **allowlist には「出してよい理由」ではなく「どの経路で実際に出るか」を書く。** 前者だと、タグが使われていないという誤った前提のまま動かしてしまう — 実際にそれで壊した (下記)。
  **モデルを直接 JSON 化する経路は 6 系統ある** (数え方: `internal/model` の型が `encoding/json` の Marshal/Unmarshal/Encode/Decode か `echo.Context.JSON` に静的型で到達する非テスト箇所)。(a) `internal/core/moderationlog/service.go` が `json.Marshal(info)` でモデルごと記録し (`*model.Meta` の before/after、`[]*model.RegistrationTicket`、`*model.Role`、`*model.Ad`、`*model.SystemWebhook` など)、`admin/show-moderation-logs` が `info` をそのまま返す。(b) `internal/core/ephemeral/store.go` が `model.Note` / `model.User` を Redis へ。(c) `internal/core/webpush/cache.go` が `[]*model.SwSubscription` を Redis へ入れて読み戻す。(d) **admin API のレスポンス本体** — `internal/api/admin/relays.go` が `*model.Relay` / `[]*model.Relay` を `c.JSON` にそのまま渡す。(e) **同** — `abuse_report_notification.go` の `packedRecipient` が `*model.AbuseReportNotificationRecipient` を埋め込んで返し、`admin/show-user` は `[]*model.Role` を入れた map を返す。(f) `drive/chunked_upload.go` が `[]model.ChunkedUploadPart` を jsonb 列へ往復し、`instance_service.go` が `[]model.SuspendedSoftwareEntry` を読み戻す。**「API は map を手で組むからタグは使われていない」も「API のレスポンスは必ず map か entity を通る」も誤り** — (d)(e) はモデルの json タグがそのままレスポンスの shape になる。**この数え落としは 3 度繰り返した** (2 箇所 → 3 系統 → 6 系統)。数えるなら grep ではなく型情報で追うこと。
  **敵対的レビュー 1 周目で 3 つの穴が実測された。** (a) 正規表現が `Pass` を見ておらず **`Meta.SmtpPass`** が検出集合にすら入っていなかった。(b) `Code` も見ておらず、**`SignupApplication.ClaimCodeHash`** — モデル側が「平文では持たない。DB が漏れた時点で全申請が乗っ取れる」と書いて `json:"-"` で守っているフィールド — のタグを外しても緑だった (**ゲートが存在する理由そのものの失敗形**)。(c) `fld.Tag == nil` を skip していたため、**タグを書き忘れた新規フィールドが原理的に見えなかった** (`encoding/json` はタグの無い exported フィールドを Go の名前でそのまま出す)。`Pass` / `Code` / `Auth` / `Hash` の追加による偽陽性は `NoteDraft.Hashtag` の 1 件だけで、allowlist 1 行で済む。
  **2 周目で、その修正が実挙動を壊していることが実測された。** 1 周目の指摘を受けて新検出の 4 件を `json:"-"` にしたが、うち **`Meta.SmtpPass` と `RegistrationTicket.Code` は moderation log の記録を欠けさせた** — 招待コードの監査記録が空になり、`update-meta` の記録も他の secret が全部残る中で smtpPass だけ消えるという不揃いになった。しかも `internal/api/admin/handler.go` には「upstream も mask しない。互換性最優先で同じ挙動」という**意図的な parity 判断のコメントが既にあった**。「挙動を書き換えるなら、それを固定しているテストが無いか先に読む」「コードコメントの既知乖離を見る」に反した形。**`SwSubscription.Auth` も危うく同じ経路で壊すところだった** ((c) の Redis キャッシュ)。タグは allowlist へ戻し、**出ることを固定するテスト** (`TestModelJSONKeepsAuditedFields`) を足した。さらに 3 周目の指摘で、**allowlist に載っているものが `json:"-"` になっていないかを 29 件一律に検査する**形に変えてある — 手で 3 件並べるだけでは、同じ壊れ方が残り 12 件で開いたままだった (実測で 4 形が素通り)。「allowlist に載せた = 出ることが前提」なので `json:"-"` は宣言との矛盾として落ちる。
  **allowlist の dead-entry 検査は万能ではない。** 「実在しないキーが残ると落ちる」形は、**allowlist に該当があるキーしか守らない**。`Pass` / `Code` は該当が全て `json:"-"` 側にあったため、**1 周目の指摘を塞いだ修正そのものが正規表現から外しても緑で巻き戻せる状態**だった (2 周目で実測)。検出集合そのものを `mustDetectSecretFields` で固定して塞いだ。
  **gate は `internal/model` には置けない。** あそこは `_test.go` を 1 つも持たないので、テストを足すと CI のカバレッジ閾値 (90%) の対象に**初めて**入り、wire 層と同じ理由で 0% に張り付いて落ちる (#462 と同型。実測で `coverage: 0.0%`)。`internal/entitycompat` に置き、対象のソースはファイルとして読む。
  **検出ロジックは人工のソースで固定する。** 実モデルは allowlist で全て許可済みなので leak は 0 件で、実モデルだけを見ていると検出の枝が一度も実行されない。静的なタグ検査に加えて代表的な型を実際に `json.Marshal` もする (`MarshalJSON` を自前で実装した型では静的検査が抜けるため)。逆に新しいフィールドを allowlist 無しで足す形は静的検査でしか捕まらない。
  **「N 形中 M 形」と書くなら変異集合そのものを書く。** 1 稿目は「20 形中 19 形」と書いて非検出形を 1 つ挙げたが、そこに上記 (a)(b)(c) は入っていなかった。2 稿目は非検出形を集合から外して「22 形すべて」にし、数字は上がったが情報は減った。**現在の集合は 27 形**: モデル側の `json:"-"` 剥がし 11 (= `json:"-"` を持つ全件) + タグ削除 3 + ゲート側 6 (tag==nil skip の復活 / 正規表現の空振り / allowlist の死んだキー / allowlist から 1 件削除 / 理由を空に / leak 収集を潰す) + 正規表現の alternative 剥がし 4 (`Pass` / `Code` / `Auth` / `Hash`) + 監査側の固定 3 (`Meta.SmtpPass` / `RegistrationTicket.Code` / `SwSubscription.Auth` を `json:"-"` にする)。検出対象は実測 40 件 (`json:"-"` 11 + allowlist 29)。**既知の非検出形**は「実モデルの leak 検査そのものを消す」形 (leak が 0 件である以上、実モデルからは捕まらない。人工ソースのテストが検出ロジック側を押さえる)。
  **既知の取りこぼし**: `Pass` / `Code` / `Auth` / `Hash` は部分一致なので `PassedAt` / `StatusCode` のような名前も拾う (fail-closed なので危険側には倒れない)。`datatypes.JSON` 列の中身、`internal/model` 直下以外 (glob が非再帰)、名前付き型の中の匿名 struct、`internal/queue` など他パッケージの構造体は見ない。`Key` 全体には広げていない — `PublicKey` / `*SiteKey` (captcha のサイトキーはフロントへ配る公開値) / `SortKeys` / `ExcludeKeywords` が誤検知で allowlist を埋め、本物が紛れるため。
- **2026-09-11**: `make gates` に `dockerignore-check` を追加し、`.dockerignore` の漏れを塞いだ (#2942)。`make help` の target は 130 → 131。**`drive-files` と operator-local な設定 (`.config/*.y*ml` / `deploy/uds/config/*.y*ml` / `compose.uds.yaml` / `.claude`) が除外されていなかった。**
  **「配る image に入る」ではない。** mk-go をビルドする Dockerfile はどれも最終 stage が**明示パスの `COPY --from=builder` しか持たない**ので、context に入ったファイルが配布物へ出ることはない (実測: 除外を外して build しても最終 image の `drive-files` は 0 件、builder stage には 2,790 件。修正前の `.dockerignore` でビルドされた本番 image の `/app/drive-files` も空)。**最初これを「image へ焼き込まれる」と裏取りせずに書き、7 箇所に伝播させた。** 守っているのは **build context と builder stage の layer** で、漏れる経路は (a) `cache-to` でキャッシュへ書き出したとき、(b) 手元の builder cache に滞留したとき、の 2 つ。加えて転送量にも効く。
  **`.dockerignore` のパターンはパス全体で照合される。** スラッシュを含まない `node_modules` は `node_modules` にしかマッチせず、**`third_party/misskey/node_modules` (実測 1,133 MB) は残る**。同じことが `.git` にも起きており、`plugins/*/.git` が 4 つ入っていた (#2940 の `pluginresolve` は clone した分だけ自分で消しており、コメントに「`.dockerignore` は `plugins/*/.git` を落とさない」と書いてあった = 既知のまま放置されていた)。入れ子にも効かせるものは `**/` を前置する。**ただし `built` は前置しない** — `third_party/misskey/built` は SPA の成果物で image に要る。
  **`**/node_modules` にもできない。** それだと `packages/backend/node_modules/` 配下の symlink まで落ち、Dockerfile が COPY する `emoji-assets/built/...` が解決できなくなる。`third_party/misskey/node_modules` を名指しで除外し、必要な emoji-assets (44 MB) だけを `!` で再包含する。**再包含は実体側に書く** — pnpm は実体を `.pnpm/` 配下に置き、`packages/backend/node_modules/...` はそこへの symlink なので、symlink 側を再包含しても効かない (実体側の再包含を外すと COPY が `not found` で落ちることを実測)。
  **実測は 1,512 MB → 421 MB。** 手元ではさらに `.pnpm-store` (3.8 GB) が消える。**`docker build --check` では分からない** — `--check` は context を 849B しか送らないので、転送量も COPY の成否も実ビルドでしか測れない。
  **gate はサイズを見ない。** コンテキストが太っても転送が遅くなるだけで、ビルドは通るし気付ける。見るのは「中身が読まれると困るもの」だけ。
  **`.dockerignore` を自前で解釈しない。** `moby/patternmatcher` + `ignorefile` (Docker 本体が使う実装) に「そのパスが除外されるか」を直接判定させる。**最初は文字列の正規化で近似して書き、敵対的レビューで 8 形中 7 形を見逃すことを実測された** (`!*/**` / `!drive-files*` / `!/drive-files` / `!.config/**` / `!.config/*` / `!deploy/uds/config/*` / `!compose.uds.yaml*` がどれも gate PASS のまま再包含される)。`**/` の前置・末尾スラッシュ・先頭スラッシュ・`*` を挟む形・`!` の後勝ちが絡むので、近似は必ず取りこぼす。#2857 が「Makefile を自前でパースせず `make -n` に解決させる」と結論したのと同じ形。**`ignorefile.ReadAll` と組にするのが要点** — 先頭スラッシュの除去はそちらの仕事で、`patternmatcher` 単体だと `!/drive-files` を取り逃がす。両者は既に `go.sum` にある (testcontainers 経由) ので、`go get` するだけで **`go.mod` を変えずに** import できる。**`// indirect` のコメントは残る** — 落とすのは `go mod tidy` の仕事で、このリポジトリでは tidy が使えないため。充足は `GOWORK=off go build ./...` が通ることで確かめる。
  **判定は字句だけで symlink を辿らない。** だから「Dockerfile が COPY する symlink 経路」と「pnpm が実体を置く `.pnpm/` 配下」の**両方**を一覧に入れる必要がある。実体側だけを守っていたときは、`third_party/misskey/node_modules` を `**/node_modules` に広げる変更が **gate 緑のまま全ビルドを壊した** (`!` の再包含は実体側だけを生かすので symlink が落ちる)。しかもそれは `.dockerignore` 自身が名指しで警告している形で、`Dockerfile` の guard は「pnpm install not run?」と**事実と逆**を出す。
  **本物の matcher に解かせると、肯定側のアサーションが書けるようになる。** 「`.config/docker.yml.example` は context に**残る**」「emoji-assets の twemoji は**残る**」を検査対象にできるので、除外を広げすぎて COPY を壊す変更 (`.config/*.yml` → `.config/*`、`built` → `**/built`) がその場で落ちる。**除外側の文字列一致しか見ない形では原理的に書けない検査**で、変異検証でも肯定側 3 件が検出できている (合計 17/17)。
  **`<Dockerfile名>.dockerignore` の存在も見る。** BuildKit はそれがあると root の `.dockerignore` を**一切見ない**ので、ファイル 1 つで全ての除外が静かに無効になる。

- **2026-09-10**: `make gates` に `pluginembed-check` を追加し、運営者向けの reusable workflow (`build-with-plugins.yml`) を新設 (#2940)。`make help` の target は 129 → 130。**`Dockerfile.bundled` が `pluginbuild` を呼んでいなかった** — `plugins/` に置いてビルドしても入らない image が黙って出来ており、しかもエラーにならないので運営者は「入ったつもり」で起動できた。`docs/plugins/operating.md` は「`Dockerfile` / `deploy/uds/Dockerfile.mkgo` の両方が生成ツールを実行する」と書いて bundled を挙げていなかったが、**除外とも書いていなかった**。
  **gate は 3 つの素通りを塞いである** (どれも敵対的レビューで実測された)。(a) **順序を見る** — `pluginbuild` を `go build` の後に置くと生成物が binary に入らないが、Dockerfile としては正当でビルドも成功する。(b) **builder の検出を 1 つの文字列に頼らない** — `./cmd/misskey` だけを探す形は module path (`github.com/shiroha-a/mk/cmd/misskey`) やワイルドカード (`./cmd/...`) で書かれた Dockerfile を検査対象から黙って落とす。**allowlist にも載らないので gate は鳴らない**まま検査が減る (「1 つも拾えなかったら落とす」は全部消えたときしか効かない)。(c) **RUN 内の行末 `#` も落とす** — シェルのコメントなので「書いてあるのに実行されない」状態になる (#2856 が `wiring-check` で `/* */` に対して踏んだのと同型)。(d) **行継続を畳んでから判定する** — `go build` と対象が同じ行にあることを要求すると、ldflags を 1 つ足して折り返した瞬間にその Dockerfile が検査対象から消える。(e) **動詞も 1 つに頼らない** (`go install` で外れる)。
  **落としすぎる strip を builder の判定に使わない。** 行末 `#` の除去は「落としすぎる」側に倒してあるので、`go build` の行にたまたま ` #` があるとその Dockerfile ごと builder 集合から消える。**検出は広い body で、実行されるかの判定は狭い body で**、と分けてある (敵対的レビュー 2 周目で、(c) の対処が (b) の穴を新しく開けていることが実測された)。
  **検出と順序判定でも広さを変える。** 検出は「ファイルのどこかに動詞と対象がある」で広く取る — 動詞と対象が同じコマンドに現れることを要求すると、対象を `ARG MK_MAIN=./cmd/misskey` のような変数に入れただけで検査対象から消える。逆に順序判定は「動詞と対象を**同時に含むコマンド**」だけを基準にする — 畳んだ RUN の中に無関係な `go build` / `go install` があると、そちらが基準点を前へ引っ張って**正しい Dockerfile が順序違反で落ちる** (しかも診断が事実と逆を指す)。**3 周目のレビューでこの 2 つが同時に指摘された** — (b)(d) の対処がそれぞれ別方向の穴を開けていた形。**残る既知の穴は「pluginbuild を使われない別 stage に置く」形** (存在判定はファイル全体を見るため素通りする)。テストの doc コメントに明記してある。
  **突き合わせに使う出力は、無検証の値より前に置く。** `pluginbuild` の行は `dir=` を `name=` より前に出す — `name` は `mk-plugin.yml` の無検証な YAML 文字列で括弧も改行も入れられるので、後ろに置くと `name: "x dir=plugins/victim "` のように**別プラグインの行を偽装でき、無効化されたプラグインが組み込まれたと判定される** (実測)。書式は `tools/pluginbuild` 側のテストで固定した — 呼び出し側が突き合わせに使う契約なので、片側だけ変えて気付かないのを防ぐ。
  **運用の動機は実測。** 定常運用は **343 MiB** (mk-go 91 / PostgreSQL 181 / valkey 69 / nginx 2) で 2GB VPS に載るのに、**Go のビルドは 1200MB 制限・既定の並列度で OOM する** (`-p 1` なら通る)。`/usr/bin/time -v` が出す 976MB は**単一プロセスの最大値**で、並列コンパイラの合計ではないので、コア数が多いホストほど OOM しやすい。さらに **BuildKit は dockerd に組み込まれている**ため、本番ホストでビルドするとヒープが膨らんだまま返らない — **ビルドキャッシュを 60.88GB 削除しても RSS は 4,465 → 4,485MB で不変**だった (削除処理自体で一時的に 6,798MB まで増え、2 分で戻った)。`builder.gc.defaultKeepStorage` はディスクにしか効かない。解放には dockerd の再起動が要る。
  **`ARG` は最初の `FROM` より前に置く。** `FROM assets-${ASSETS_SOURCE}` のような stage 名の展開に使えるのは global ARG だけで、stage 内で宣言したものは参加しない。しかも**`--build-arg` を渡しても救われない** (未宣言の build-arg は metaArgs に入らない) ので、置き場所を間違えると `assets-` という不正な stage 名になり、**プラグイン経路だけでなく既定のビルドまで落ちる**。
  **step の `if:` から `secrets` は参照できない** (使えるのは env / github / inputs / job / matrix / needs / runner / steps / strategy / vars)。書くと式の検証が `Unrecognized named-value` になり、**呼び出し元の job が 1 つも走らずに失敗する** — token を渡さない呼び出しでも同じ。判定は `run` の中で行う (step の `env` は `if` からは見えないが `run` からは見える)。
  **reusable workflow 側で `permissions` を宣言しない。** caller の権限以下にしか設定できないので、宣言した時点で caller がそれを持っていなければ run ごと拒否される (`push: false` でも回避できない)。publish する caller が自分で `packages: write` を書く。
  **要求したプラグインが実際に入ったかを突き合わせる。突き合わせるのはディレクトリ名。** `pluginbuild` が最初に出す名前は `mk-plugin.yml` の `name:` で、**置いたディレクトリ名とは限らない**。運営者が指定できるのはディレクトリ名だけなので、名前で照合すると**正しく組み込まれたプラグインで落ちる** (しかも診断が disabled を疑わせる方向になり事実と逆を指す)。`pluginbuild` の出力に `dir=` を足して、そちらで照合する。あわせて **`-dry-run` の stdout には spec 以外を書かない** — 「指定されたプラグインはありません」を stdout に出していたため、プラグインを全部コメントアウトすると案内文の 1 行目がプラグイン名として読まれて落ちた (どちらも敵対的レビュー 2 周目で、1 周目の修正が作った回帰として検出された)。`pluginbuild` は `mk-plugin.yml` が `disabled: true` のものを黙って skip して exit 0 で終わる。しかも `disabled: true` は #2701 でこのプロジェクト自身が同梱プラグインに要求している書き方なので、作者がそれに倣っていれば必ず踏む。見ないと**指定した N 個のうち 0 個しか入っていない image が緑で push される** = この issue が塞ごうとしている穴そのものになる。
  **新しい workflow は PR 上で発火させて確認する** (`docs/ci.md`)。`workflow_dispatch` だけだと default branch にあるものしか起動できず、マージ前に一度も検証できない。**この PR の High 3 件はそこを踏み外したまま書いたことで生まれた。**
- **2026-09-10**: `make gates` に `mdtable-check` を追加 (#2930)。`make help` の target は 128 → 129。**GFM は列が増えた行を「崩して描画」しない。溢れたセルを黙って捨てる。** ヘッダ行が列数を決め、それを超えたセルは破棄されるので、**ソースには書いてあるのに GitHub 上では読めない**という形で壊れる。ローカルで md を読んでいる限り気付けない。実際に踏んだのは `docs/divergence.md` の `2026.9.0-mk.7` の行で、コードスパンの中に書いた権限式 `$i.isModerator || $i.policies.canManageCustomEmojis` の `||` がセル区切りとして働き、**描画は 599 文字あるべきところ 394 文字で止まって 205 文字 (34.2%) が読めなかった** (数え方は `gh api /markdown --mode gfm` の出力からタグを除いた文字数)。消えた中に「純正へは還元できない行」という分類が入っており、この表を「還元不能な差分の一覧」として読む運用が成立していなかった。
  **コードスパンの中でもパイプは区切りとして働く。** GFM のエスケープ (`\|` → `|`) は inline の解析より**前**に効くので、表セルの中では `` `a \|\| b` `` と書けば区切りにならずコード中の `||` になる。リテラルの `\|` を見せたいときは `` `a \\| b` ``。**表の外にはこの前処理が無い**ので、コードスパンに `\|` と書くとバックスラッシュがそのまま出る (この entry の 1 稿目で実際に間違えた)。
  **見るのは列数だけにしてある。** 敵対的レビューで「列数が一致したままコードスパンが割れる形がある」(3 列の表の `` | `x|y` | z | `` は**セル数がヘッダと同じ 3 になる**) と指摘され、コードスパンの対応付けを自前で持つ実装と、外側パイプ省略に対応するため表の終端をブロック開始で判定する実装を足した。**どちらも次の周で正当な md を落とした** — 前者は**二重バッククォートのコードスパンを含む行**を、後者は**表の直後にリストを置くというごく普通の書き方**を偽陽性にした (どちらも GitHub では正常に描画されることを実測)。自前の inline / block パーサに継ぎ足す形は #2857 が「手当てするたびに隣の穴が開く」と結論した型なので、**列数という 1 つの条件だけ**に戻してある。取りこぼす側 (列数が一致したまま割れる形、外側パイプを省いた表、ヘッダ行自体が壊れた表) はテストの doc コメントに明記した。
  **表は「先頭パイプの行が続く間」とする** — GFM はパイプを含まない行も表の行にするが、そこまで追うには全ブロックの開始判定が要る。このリポジトリの表 227 個はすべて先頭パイプ付きなので取りこぼしは無い。**フェンスの検出は行頭 3 スペースまで** (`^\s*` にすると、フェンスの書き方をインデントブロックで見せているdoc で開いたまま閉じず、そのファイルの残りが未検査になる。現 corpus に該当は無いが仕様どおりにしてある)。**`git ls-files` で見る** (#2857 と同じ理由。submodule の中は出ないので fork frontend の md は対象外)。**1 つも拾えなかったら落とす。** 実測は tracked な md 57 ファイル / 表 227 個で、**現 corpus に対し偽陽性 0・検出 1 件** (= 上記の実バグ)。**あわせて `make gates` の一覧に `notiftype-check` が漏れていたのを Section 3 と `docs/development.md` の両方で直した** (2026-09-08 に追加したときの片側更新)。
- **2026-09-09**: `make frontend-check` に eslint を追加し、`make frontend-lint` を新設 (#2906)。`make help` の target は 127 → 128。**手元で CI と同じ検査ができていなかった** — `frontend-check` は `vue-tsc` と submodule ゲートだけで、eslint は CI の**別 step** (`pnpm eslint`) だった。#2903 で実際に踏んでいる (デッドコードを消したときの空行 2 連続が `@stylistic/no-multiple-empty-lines` で落ちた)。個別ファイルに `npx eslint` を掛けても CI と同じ glob ではないので見落とす。CLAUDE.md 2026-09-05 の #2841 (`make test` と CI の flag がずれていた) と**同じ型**。**引数は書き写さず `package.json` の script を呼ぶ** — 書き写すと #2841 と同じドリフトが起きるので、`npm run --silent eslint` で script を唯一の定義にした (CI は `pnpm eslint` だが手元に pnpm があるとは限らない。既存の `frontend-check` / `frontend-test` も npx を使っている)。**`eslint .` にしないこと** — upstream が lint していない `test/` まで拾い、追従のたびに他人の負債で落ちる。実測 55 秒で、#2903 と同じ違反を入れて `make frontend-check` が exit 2 で落ちることを確認した。**vitest は入れていない** (`make frontend-test`) — CI も別 step で、こちらは #2844 で既に手元の再現手段がある。
- **2026-09-08**: `make gates` に `notiftype-check` を追加 (#2898)。`make help` の target は 126 → 127。通知タイプの一覧が **core の `Type` 定数と `internal/api/notifications` のリテラルの 2 箇所**にあり、片側更新が実際に起きていた (`importCompleted` が core にだけあった。発火箇所が無いので実害は出ていなかったが、固有型を足せば必ず踏む)。API 側を `internal/core/notification` の registry から導出する形にしたうえで、(a) registry と `Type` 定数が 1:1 か、(b) API 側がリテラルに書き戻されていないか、を検査する。**値の一致だけでは足りない** — リテラルに書き戻しても書いた時点の中身は同じなので値比較は通り、落ちるのは core に型を足した後 = 一番検出したい瞬間に検出できない。導出している「形」を AST で固定してある (中身が同一のリテラルへの書き戻しで落ちることを実測)。**固有型を全指定判定に含めるのが要点** — 含めないと upstream の 20 種を全て `excludeTypes` に並べただけで「全部除外」と判定され、除外指定していない固有型の通知まで返らなくなる。
- **2026-09-08**: Section 3 の `make frontend-check` と Section 8 の `frontend-check` job に、submodule のソースを読むゲートを追記 (#2892)。`/about-misskey` の謝辞アイコン 62 枚が `img-src 'self' data: blob:` でブロックされ本番で 1 枚も表示されていなかったのを、`img-src` に固定 2 origin (`avatars.githubusercontent.com` / `assets.misskey-hub.net`) を足して直した。**upstream が host を足すと黙って壊れる**ので、`about-misskey.vue` から外部画像の host を抽出して定数と過不足なく突き合わせるゲートを置いた。**`make gates` には入れない** — あちらは submodule 無しで回る前提で、混ぜると checkout していない環境で skip され「検査していないのに緑」になる。`test-shards` は `third_party/misskey` を checkout しないため、submodule を取る `frontend-check` でだけ回し、`MK_FRONTEND_GATES_REQUIRE_SUBMODULE` で skip を禁じる (`plugin-tests` の `MK_PLUGIN_TESTS_REQUIRE_DB` と同じ形)。**media proxy 経由には落とせない** — mk-go の proxy は upstream と違い open proxy ではなく、allowlist が DB に実在する URL だけを通すので静的な URL は 403 (実測)。**この doc 更新自体が #2892 で漏れていた** — Makefile の target と CI job の中身を変えたのに、それを説明する 5 ファイル 7 箇所が「型チェックだけ」のまま残っていた (CLAUDE.md が「最多の型」と呼ぶ片側更新)。
- **2026-09-07**: Section 3 に「更新 (運用)」のコマンドを足し、Makefile に `pull` / `pull-plugins` / `uds-rebuild` / `uds-restart` / `docker-rebuild` / `docker-restart` の 6 target を追加した (#2885)。`make help` の target は 120 → 126。**`docker compose up -d` は再起動を保証しない** — image と設定が変わらなければコンテナを作り直さないが、frontend は bind mount なので frontend だけ更新したときは何も変わらない。mk-go は `DetectClientEntry` で entry を起動時に 1 回だけ解決してキャッシュするので、再起動しないと消えた古いハッシュを配り続ける。**2026-09-07 に本番で 10 分近くこれを踏んだ** — `built` の最終書き込みが 13:02:52、mk-go の再起動が 13:12:36 で **9 分 44 秒**。`build.ts` は出力先を rm してから作るので、ビルド開始からの実際の窓はさらに長い。mk-go は 05:17 起動のままだった。Makefile と `docs/deployment.md` には「再ビルドと再起動は必ずセット」という原則が元からあったが、**そのセットを `up -d` が実現できていなかった** — 宣言した不変条件を、実行するコマンドが満たしていない型。
  検証は `deploy/check-frontend-entry.sh` が持つ。敵対的レビューで、初版が**この PR が防ぎたい状況で緑を返す**ことが実測で示された (High 2 件)。(a) 公開 URL は Cloudflare の裏なので、直前まで配信していた古いアセットはエッジに残っており、素で叩くと `cf-cache-status: HIT` の 200 が返る (使い捨てクエリを足すと MISS になり、事故当時の entry は 404 と分かる)。(b) index の loader は `CLIENT_ENTRY.replace('scripts', lang)` で**言語ごとのパスへ振り替える**ので、`scripts/` だけ見てもブラウザが読む URL を見ていない。あわせて `sort -u | head -1` は「アルファベット順で最初の `scripts/*.js`」であって CLIENT_ENTRY ではなく、modulepreload が 1 本増えた日に無検証で緑になる形だった。**「CSS では判定できない」の理由も誤っていた** — `emptyOutDir: false` で古いファイルが残るからではなく、`build.ts` が毎回 `built/_frontend_vite_` を消したうえで**内容ハッシュが同じものは同じ名前で作り直す**ため。理由を取り違えると「CSS だけ内容が変わればその名前だけ変わる」という取り逃がしに気付けないので、現在は entry (全言語) と stylesheet の両方を見る。
  **`DOCKER_CONFIG` という make 変数を作ってはいけない。** docker CLI が設定ディレクトリとして読む予約名で、make は環境由来の変数を recipe へ export し直すため、operator の環境にそれがあると値を奪って `docker compose` が `unknown command` で死ぬ (実測)。このリポジトリでは `uds-*` を含む docker 系 target が全滅する。初版で踏んだので `ENTRY_CHECK_DOCKER_CONFIG` に改名した。
  **`docker-*` 系は本番 UDS のホストで叩かない。** `docker-compose.yml` は `name:` を持たないので project 名がディレクトリ名 `mk` になり UDS 本番と同じ project に合流する。`app` / `db` / `redis` が本番の隣に立ち上がり、本番のコンテナは orphan 扱いになる (compose 自身が `--remove-orphans` を勧めてくる)。しかも検証先は `.config/docker.yml` の url なので、**本番を触らないまま緑を返す**。
  プラグインは `plugins/*/` のうち `.git` を持つ 4 つ (fedwatch / genshin / hsr / nowplaying) が独立リポジトリで、`make update` の `--recurse-submodules` では追従しなかった (`status` / `trustlevel` は本体に tracked なので追従する)。dirty なものは名前を出して skip する — 勝手に stash すると編集中の変更が「消えた」ように見えるうえ、復元手順もどこにも残らない。あわせて `update` が `git pull` の終了ステータスを見ておらず、**pull に失敗しても「変更なし」と表示して exit 0** していたのを直した (本番更新の起点になったので影響範囲が広がっていた)。
  **`make pull` は submodule の生成物を先に戻す。** `make plugins` (pluginbuild) が `packages/frontend/src/server-plugins.generated.ts` を、`pnpm -r build` の i18n パッケージが `packages/i18n/src/autogen/locale.ts` を書き換える。どちらも submodule 内の **tracked ファイル**なので、一度でもビルドしたワークツリーは常に dirty になる。dirty なまま gitlink が動くと `git pull --recurse-submodules` は checkout に失敗するため、**frontend の再ビルドが要る回 (= submodule bump 回) に限って一括コマンドが必ず止まり、しかも親リポだけ進んだ混在状態で止まる**。そのまま分解実行を続けると新 backend + 旧 frontend が本番に載る。戻すのは**生成物だけ**にしてある — それ以外の変更が残っていれば git 自身が止まるので、frontend に手を入れている最中の作業を黙って捨てない。
  **検証スクリプトは抽出に失敗したら落とす。** 初版は `LANGS` や stylesheet の書式が変わって正規表現が空振りしても、対象が減るだけで緑を返した (実測: 言語別アセットが全滅していても exit 0)。この PR 自身が引いている「拾えなかったら落とす」に反していたので、`LANGS` が空のとき、および index に `rel="stylesheet"` があるのに href を 1 本も拾えないときは落とすようにした。
  **片側更新が 2 箇所残っていた。** `README.md` と `docs/upstream-catch-up.md` が事故そのものの手順 (`docker compose up -d` / `up --build -d`) を勧めたままで、しかも README は直下で「再ビルドしたら必ず再起動する」と宣言していた。後者は「submodule の静的アセットを image に焼き込んでいるので `--build` 必須」とも書いていたが、vite frontend は bind-mount で渡しており image に入るのは static-assets / twemoji / fluent-emoji だけ。CLAUDE.md 2026-08-20 の「直したら固有の語で `git grep` する」に該当。
  **この PR は停止時間を縮めていない。** frontend のビルドは配信中のディレクトリを開始直後に消すので、ビルド開始から再起動完了までは 404 になる (事故当日の実測ではビルドが約 19 秒、再起動していなかった時間が 9 分 44 秒で、**窓のほぼ全部が後者**だった)。塞いだのは「**恒久的に**壊れたまま気付かない」方だけで、無停止にするには別ディレクトリへビルドして差し替える構成が要る。
- **2026-09-06**: `make gates` に `migrationdoc-check` を追加 (#2874)。`make help` の target は 119 → 120。**gate が見るのは 8 ファイル 22 箇所** (数え方: claim 20 + no-op down の一覧 1 + 破壊的マイグレーションの表 1)。1 本足したとき実際に動くのはその一部で、#2866 (000082 の追加) では 17 箇所 (total 4 + destructive 11 + 一覧 1 + 表 1。テーブルを作らず data loss 宣言も持たないので tables / dataloss は動かない) — #2866 の敵対的レビューで 5 箇所の漏れが見つかっている (`docs/api-compatibility.md` はリンク先と違う数を出したまま、`internal/testutil` の 2 箇所は分母が migration ファイル数。**PR は単一コミットに squash されているので、漏れていた中間状態は履歴に残っていない**)。**一覧の突き合わせが本体** — 件数だけだと「1 本足して 1 本消す」で素通りする (実測で確認)。**破壊的なマイグレーションの件数は doc 自身の表の行数を truth にする** — migration の中身から「共有テーブルに触るか」を機械的に判定しようとすると、upstream に無いテーブル (`signup_application`) を触るものまで拾って人手で外すことになる。表は 1 行 1 migration なので判断が要らない。**最初これを「機械化できない」と誤って結論し、#2866 で実際に壊れた 5 箇所のうち 3 箇所を検査対象から外していた** (敵対的レビューで指摘)。対象外にしたのは 3 つだけ — 「宣言が無いまま DROP する down が 51 本」(`architecture.md` と `migration-from-ts.md` で**定義が違うのに同じ 51** を出しており、どちらを truth にするか決められない。実測ではどちらの定義でも 51)、「102」(「上記 9 件」の定義に依存)、「データを不可逆に変えるのはこのうち 8 本」(機械判定できない)。**拾えなかったら落とす** — 書式を変えて正規表現が空振りすると、検査していないのに緑になる。#2644 が「doc の静的検査は測ったら使い物にならなかった」と結論しているが、あれは**存在しない Makefile target / パスの検出**で不在候補 298 件の大半が偽陽性だった話。件数は数え方が一意に定義でき、実測で偽陽性 0 / claim 20 個と truth・書式の変異を合わせて全件検出、しかも**生きた drift を 1 件見つけた** (`docs/deployment.md` の self-check 出力例が version 81 のままで、82 だと `selfcheck` は FAIL を返すので例として成立していなかった)。**この gate 自身も untracked のまま `make gates` に落とされた** — #2857 の `gaterun-check` が `git ls-files` で見るため。
- **2026-09-06**: `make gates` に `gaterun-check` を追加 (#2857)。`make help` の target は 118 → 119。**`go test -run` は該当が無くても exit 0 で通る** (`ok ... [no tests to run]`) ので、ゲートのテストが消えても `make gates` は緑のままだった。#2840 で実際に踏んでいる — 新設したゲートファイルが untracked のまま、`wiring-check` は PASS が 12 → 11 に減るだけで何も言わずに通った。**件数ではなく名前で突き合わせる** — 期待件数を別に持つと、それ自体が同期を要する第 2 の一覧になる。`-run` に書かれた名前がそのまま一覧なので「その名前に一致する tracked なテストが 1 つ以上あるか」だけを見る。**`git ls-files` で見るのが要点** — ディスクを走査すると `git add` を忘れた新規ゲートが手元では見つかり、CI で初めて落ちる。**完全一致にはしない** — `notfound-check` の `TestScanCollapsedLookups` は `_APILayer` / `_CoreLayer` をまとめて指す前方一致で、厳密にすると正当な書き方が落ちる (実測)。接頭辞を保つ rename は `-run` でも引き続き当たるので、検出したいのは「1 つも当たらなくなった」状態だけ。**`gates:` からの脱落も見る** — -run が解決しても一括実行から漏れていれば誰も回さない (同じ「黙って検査が止まる」型)。
  **Makefile を自前でパースしない** — 行継続・列 0 のコメント・recipe 中の空行・同一 target の複数ルール・集約 target は
  どれも make の仕様で、自前パーサに継ぎ足すと**手当てするたびに隣の穴が開く** (敵対的レビュー 3 周で毎周それを繰り返した)。
  `make -n <target>` と `make -pn` に解決させ、こちらは出力から `go test … -run …` を拾うだけにした
  (`$(shell …)` がこの Makefile に無いので `-n` に副作用も無い)。**make の出力にも行継続は残る**ので、そこだけは畳む。
  実装自体が最初 untracked で落ち、完了条件を自分で実証した。
- **2026-09-05**: `make test` に `-race -count=1` を足し、`-race` 抜きの `make test-fast` を新設 (#2841)。`make help` の target は 116 → 118 (`test-fast` と `testflags-check`。数え方は `^名前:.*##` の行数)。**Section 3 の「115」が古くなった起点は #2828 ではなく #2844** (`frontend-test` の追加で 116。`e1fd1e06` が 115、`f9ec2716` が 116 と実測。#2844 のコミットメッセージ自身が「116 → 117」と誤記していた)。**順序依存は #2795 で seed を揃えて塞いだのに、データ競合は塞げていなかった** — `make test` は `-race` 無しで回るので、手元で緑のまま required check の `test` が落ちる。`fe7ea8f2` (2026-09-03「Fix CI: SendMeasuresEnvelope のテストが -race で落ちる」) で実際に踏んでいる。**実測は 65.0s → 160.5s (2.5 倍)**、`-race` が全 173 パッケージで競合ゼロ (= 揃えるために先に潰す既存の競合は無い) であることも確認した。CI の `test-shards` は 1 shard あたり実測 158-290s (直近 5 run × 4 shard の job 全体。テスト step 単体は 114-241s)。shard は並列なので `test` check の wall clock は max(shard) だが、**実際の往復は push から結果まで 4m10s-4m59s** かかるので、手元で 95s 払うほうが速い。`-count=1` 自体のコストはゼロだった (65.35s → 65.04s)。**`-shuffle` は `go test` の cacheable flag に入っていない**ので、seed を渡している時点でキャッシュは元から無効。`-count=1` を残すのは CI との一致のためで、キャッシュ対策としては効いていない。`-timeout` / `-coverprofile` / `-covermode` は揃えない — 前者は既定と同じ 10m、後 2 つはカバレッジ閾値チェック用で挙動に影響しない。**`make test-fast` はコミット前の検査ではない** (`-race` が無いので CI で落ちるものが手元で緑になる)。編集しながら回す用で、`make check` は `-race` 付きを使う。
  **ドリフト自体を止めるゲートも足した** (`make testflags-check` / `TestMakeTestMatchesCIConditions`)。**CI 側を基準にする** — CI の flag のうち `ciOnlyTestFlags` に理由付きで挙げたもの以外は `make test` にも同じ値で無ければ落ちる。CI に flag が増えたときに「足す」か「無視する理由を書く」かを**選ばせる**形にしてある (無条件に無視すると #2841 と同じことが起きる)。片側だけ変えても落ちるので、`ci.yml` の seed を変えて Makefile を忘れる形も塞がる。**どちらかを読めなかったら落とす** — 書式が変わって拾えなくなると、検査していないのに緑になる (compose-check と同じ判断)。あわせて `TestDocsQuoteTheCIShuffleSeed` で **doc に書かれた seed が CI と一致するか**も見る — これは実際に 2 度起きていて、#2795 で seed を `3` にしたあとも `docs/testing.md` と CLAUDE.md には `2795` が残り、**唯一の再現コマンドが間違ったまま**だった (doc の手順で追うと別の並び順を試すので順序依存が再現せず flaky と誤診断される)。**値の不一致は tracked な md 全体**で見て、**欠落だけ名指しの一覧**で見る — 合計件数だと 1 ファイルが `-shuffle` を丸ごと落としても気付けない (実際 README.md が「CI と同条件」と書きながら `-shuffle` を持っていなかった)。正規表現は `-shuffle` への隣接を要求する — 裸の `2795` は issue 番号としても現れるため。散文で書くと拾えないので、doc 側は `-shuffle=3` のインライン表記に統一してある。
- **2026-09-01**: Section 8 の `test-shards` に `-shuffle` を追加 (#2795)。**`internal/server` は `-shuffle` を有効にすると 5 seed すべてで落ちていた** (落ちるテストは seed ごとに違う)。原因は 2 系統で、どちらも**プロセス共有の状態を張り替えて戻していない**もの。(a) `newServer` / `New` が起動時にグローバルを **12 個** 差し替えるが、テストは同じプロセスで何度も呼ぶので、後続の `avatar` / `emoji_redirect` が素の URL ではなく署名付きプロキシURLを受け取る。(b) `frontendutil` の loader キャッシュはプロセスに 1 つで、fixture は `t.TempDir()` に置くため**ディレクトリが消えた後も内容がキャッシュに残る**。
  seed は **全 shard 共通の固定値**にした。`on` (毎回ランダム) は失敗を手元で再現できず、required check が不定期に赤くなる。**shard 番号も使わない** — shard 配属は `NR % 4` なので、テストパッケージが 1 つ増えるだけで既存パッケージの seed が変わり順序が丸ごと入れ替わる (無関係な PR が未実行の順序を引いて赤くなり、ランダム seed と同じ問題を別経路で持ち込む)。
  **seed は実測で選ぶこと。** 覚えやすい値 (issue 番号など) を置くと検出力を持たない値を引く — 実際 `2795` を置いたが、restore を無効化した変異で落ちる seed は 12 個中 7 個だけで、`2795` は落ちない側だった (= 直したバグを CI が検出しない)。採用した `3` は 6 テストが落ちる。
  **cleanup の登録も一覧も、手で書くと変異検証が効かない形になる。** `TestFrontendHTML_SplashColor` の `<style>` 抽出を splash 名指しに直した時点で、loader cleanup を全部外しても 40 seed で落ちなくなった。restore の一覧も初版は `entity` の 7 つだけで 5 つ落としていた。どちらも AST の gate で形を強制してある (`internal/server/global_state_test.go`)。

- **2026-09-04**: Section 3 に `make compose-check` を追加し `make gates` の一括対象に入れた (#2828)。`make help` の target は 114 → 115。配布する compose 3 つ (`docker-compose.yml` / `docker-compose.image.yml` / `compose.uds.yaml.example`、計 12 サービス) が `logging:` を持たず、Docker 既定の `json-file` が**ローテーションなし**で動いていた。**サービスを足したときが危ない** — anchor (`*default-logging`) を書き忘れても compose は通るし起動もするので、ディスクが埋まるまで気付けない。gate は `max-size` / `max-file` の**値そのもの**を見る (「空でない」だけだと `max-size: 50g` のような「上限を書いたのに実質無制限」が素通りする、実測)。service を 1 つも読めなかったら落とす — 書式が変わって拾えなくなると、検査していないのに緑になるため。**コメントアウトされたサービスは見えない** (YAML パーサはコメントを読まない) ので、既定無効のテンプレート (video-thumb) は gate の対象外。**一覧は手で持つ** — root には検証用の compose が 8 つあるので `git ls-files` の列挙が使えない。**`max-size` は decimal** で読まれる (json-file は `units.FromHumanSize`) ので `50m` は 50,000,000 バイト = 47.7 MiB。`50mib` と書いても同じ扱いで MiB は表現できない。
- **2026-09-01**: Section 3 に `make notfound-check` を追加し `make gates` の一括対象に入れた (#2792)。`make help` の target は 113 → 114。**repository の lookup error を種別を見ずに 4xx へ潰している箇所が 107 件**あり (`internal/api` + `internal/server` の非テスト Go を AST で走査し、`Find` で始まるか `Get` の 単行 lookup の直後 3 文以内にある `if` が、not-found 述語を通さずに 4xx を返す形を数えた。issue 本文の「135 のうち 61」は `FindByID` に限った別の数え方)、DB 接続断が「そんなノートは無い」に化けていた。クライアントからは区別できず、監視でも 5xx が立たない。upstream は `.findOneBy` の結果が `null` かで判定するので障害は例外として 500 になる。一括変換はできない — `if err != nil || !list.IsPublic {` のように not-found 判定と権限判定が同じ条件に混ざる形があるため。gate で**新規流入を止めてから段階的に潰す**方針を採り、107 件すべてを潰して allowlist は空になった。**allowlist を件数で持つのが要点** — key は `<file>:<func>` なので、理由の文字列だけを持つ形だと**その関数に 1 つでも残っていれば何個足しても素通りする** (実測)。判定は条件と body の両方で not-found 述語を探す (正しい直し方は body の中で分けるので、条件だけ見ると**直したものを検出し続ける**)。err 変数は名前のパターンではなく**代入の左辺と突き合わせる** (`err2` を拾うために部分一致にすると `n, e :=` が漏れ、逆もまた然り)。
- **2026-08-31**: Section 4 の「DB を使うテストの分離」に、システムカタログを schema で絞る規則と `Scan(&string)` の罠を追記 (#2777)。あわせて `make catalog-check` を新設し `make gates` に入れた (`make help` の target は 112 → 113)。doc だけだと再発する — schema が 17-19 ある条件は残ったままなので。`pg_indexes` を schema 非限定で引くテストが 3 本あり、**required check の `test` を不定期に落としていた** (PR #2778 の `test-shards (1)` が実際に赤くなった)。#2450 で schema を分けた結果、同名テーブルが 17-19 schema に同時に存在し、他パッケージの `ApplyMigrations` が DDL 中だと `could not open relation with OID (SQLSTATE XX000)` になる。**害はそれだけではない** — 絞らないと他 schema の同名 index を自分のものと取り違えるので、migration が適用されていなくても regression guard が緑になる。実測で `internal_repository_ts` の定義が返っており、3 本とも空振りしていた。`Scan(&string)` は複数行でも**最後の 1 行**を黙って取る (GORM は `*string` に対し全行を走査して dest を上書きする) ので、この取り違えは値が正しく見えて気付けない。
- **2026-08-31**: Section 3 に `make wiring-check` を追加 (#2762)。`make gates` の一括対象も 1 つ増えて `make help` の target は 111 → 112。router で配線しないと効かない設定 (今回は `meta.enableFanoutTimelineDbFallback`) が、**配線を消しても build もテストも通ってしまう**ため。`internal/server` は CI のカバレッジ対象外で router を組み立てるテストも無く、#2762 の穴 (列と admin 公開はあるが読み取り経路に配線されていない) がまさにこれだった。判定は router.go をソースとして読む文字列一致だが、**コメント行は数えない** (コメントアウトして残すのは消すのと同じ)。同 package の既存 gate が生ソースを見ているのに合わせてある。(**#2856 で AST 照合に変えた** — 行頭 `//` だけを除外する形は `/* */` で囲んだ配線を素通りさせていた。あわせて引数まで照合するようになったので、`WireMetaToggles(hook, nil, nil)` も落ちる)
- **2026-08-30**: Section 4 の「DB を使うテストの分離」に列枠の話を追記 (#2756)。PostgreSQL は `DROP COLUMN` した列も 1600 の上限に数えるので、実行のたびに列を落とすテスト構造だと手元でだけ枠が減り続け、最後に落ちる (実測で `clip` / `auth_session` / `app` が 1593 列まで到達した)。原因は 2 つで、`ApplyMigrations` が毎回全 migration を流し直すこと (再適用で実際に枠を食うのは migration が作る 116 テーブル中 `note` の 1 つだけ — `000033` が ADD し `000036` が DROP するため) と、TS 形状を作るテストが列を落として戻していたこと。前者は適用済みを skip する台帳、後者は専用の兄弟 schema を一度だけその形に作る方式で解消した。復旧手順も併記。
- **2026-08-24**: Section 8 の `build` ジョブに `Check bundled plugins are disabled by default` step を追記 (#2701)。同梱サンプルは #2495 で既定無効にする方針にしたが、trustlevel は #2586 で `disabled: true` 付きで同梱したあと **#2585 の実測を採るために意図的に外され、実測が終わっても戻っていなかった**。起きたのは「新しく同梱したものに既定を付け忘れた」ではなく「**検証のために一時的に外して戻し忘れた**」なので、gate はそちらを主対象にしてある。判定は **`git ls-files` + grep だけ**で完結させてある — tracked な `plugins/*/mk-plugin.yml` に `disabled: true` の行があること (列挙が空なら「検査していないのに緑」になるので落とす)。`pluginbuild` に読ませるほうが parser 一致で厳密だが、`pluginbuild` の `discover` は git ではなく**ディレクトリ**を走査するので、`plugins/` に自前プラグインを置いている手元では誤検知するうえ、生成物を書いて `make plugin-dev` の配線を巻き戻す。**残る穴は許容している** — 行ベースの判定なので parser がキーとして読まない位置 (2 つ目の YAML ドキュメント、flow collection の中) に同じ行があると通る。意図的に行わないと踏めない形。手元の再現は `make plugin-vet` (#2701 で新設。`make help` の target は 110 → 111)。
- **2026-08-20**: Section 7 に「ドキュメントを直すときのレビュー条件」を追加 (#2644)。#2637 の完了条件にあった「同じ乖離が再発しにくい仕組み」への回答。本文が候補に挙げていた**静的検査 (存在しない Makefile target / パスの検出) は測ったところ使い物にならなかった** — target は doc 側 107 のうち Makefile に無いのが 3 つで全て grep の取りこぼし (空振りする)、パスは `docs/` と CLAUDE.md / README.md のバッククォート内でスラッシュを含む文字列を拾うと**不在候補が 298 件**で、大半が偽陽性 (API の endpoint パス / CIDR / `internal/` を省いた相対表記)。代わりに #2640 の**敵対的レビュー 7 周で出た High 14 件を型に分類**して確認手順に落とした。最多は**片側更新** (4 件)、次が**裏取りせず書いた** (3 件) と**数え方が未定義 / 数え違い** (3 件)。全文は docs/contributing.md。
- **2026-08-20**: ドキュメント全体監査 (#2637) の残り 94 件を反映 (#2640)。CLAUDE.md 本体では 5 箇所を修正。(1) Section 1 の技術スタック表が Job Queue を **asynq** と書いていた (既定は #571 で mkq。ここを見て実装方針を決めると legacy 側に倒れる)。**`golang-jwt/jwt/v5` は indirect で未使用**、実際に使う `go-webauthn/webauthn` が未記載、JSON-LD は `piprate/json-gold` を直接依存。(2) Section 2 のディレクトリツリーが `...` 無しで閉じているのに、`internal/` 22 のうち 10・`cmd/` 4 のうち 2・トップレベル 7 つが欠落していた。(3) Section 3 に無い target が 76 あったので、**罠のあるものを足したうえで `make help` が全量であることを明記**した (全列挙は腐るので採らない)。`make tidy` はこのリポジトリでは使えない。(4) Section 8 に `docker.yml` (**PR で走る**) / `docker-branch.yml` / schedule の 2 つ、ci.yml の 3 step が無かった。diff-e2e の「43 比較」は pytest 総数で **endpoint 比較は 30**。(5) Section 9 の環境変数表 11 件に対し `bindEnvKeys()` は **86 キー**。登録の有無で変わるのは「**設定ファイルに書かずに env だけで作れるか**」だけで、ファイルにそのキーがあれば未登録でも `MK_` で上書きできる (`AutomaticEnv`)。実務上引っかかるのは example が既定でコメントアウトしている `meilisearch:` と `<queue>JobConcurrency` なので、その条件を明記した。
- **2026-08-19**: ドキュメント全体監査 (#2637) で見つかった、**手順どおりに実行すると壊れる記述**を修正 (#2638)。(1) `make migrate-down` は `-steps` 未指定で全 down が走っていたので `-steps 1` を付け、ヘルプ・doc の「1 段階」と挙動を一致させた (全段は `go run ./cmd/migrate -direction down` を直接叩く)。(2) **`DATABASE_URL` はどこからも読まれていない** — `cmd/migrate` は `-config` から DSN を組み立てる。Section 9 の該当項目を接続先の説明に置き換えた。(3) Section 4 のテスト準備を実態に合わせた: **testcontainers は Redis 用** (`SetupRedis` は 27 パッケージ、`SetupPostgres` は 3 パッケージ) で、PostgreSQL は外部のものを使う (既定は `localhost:5432` の `misskey_test` / `mk`)。`MustOpenTestDB` は失敗時 panic なので「Docker があれば準備不要」ではない。
- **2026-08-18**: Section 8 の `playwright` / `upstream-backend-e2e` を 4 シャード並列として書き換え (#2609)。どちらも**プロセス内では並列にできない** (前者は共有の root アカウントと instance meta、後者は `maxWorkers: 1` + ファイルごとの `/api/reset-db`) ため、並列度はシャードごとに job を分けて稼ぐ。あわせて実態と乖離していた記述を修正: `playwright` は nightly ではなく PR トリガー (#2291 の反映漏れ)、`upstream-backend-e2e` の所要時間は「18-20 min」ではなく分割前で 8.5 分。Playwright の録画を止めた理由も明記。
- **2026-08-16**: `plugin-tests` job を追加 (#2588)。同梱プラグインのテストは**どの job でも実行されていなかった** (別 module で `go list ./...` に含まれず、`build` job に PostgreSQL が無い)。テストが落ちる変更を入れても CI は緑のままだった。あわせて `build` job の同梱プラグイン検証を `go build` から `go vet` に変更 (テストファイルもコンパイルされるので、公開面を変えて本体だけ直したときに検出できる)。Section 3 に `make plugin-test` を追記。
- **2026-08-15**: PostgreSQL を 16 → 18 に統一 (#2513)。compose 全構成・CI service container・testcontainers を `postgres:18-alpine` へ。upstream Misskey の compose 例 (18-alpine) に整合。**postgres:18 image は data layout が変わった** (default PGDATA が `/var/lib/postgresql/18/docker`、VOLUME 宣言が親 `/var/lib/postgresql`) ため、永続 volume を持つ compose のマウント先を `/var/lib/postgresql` へ変更 (旧パスのままだと新規デプロイが匿名 volume に initdb して down で消える。UDS example は明示 PGDATA で回避)。既存の 16 volume は dump→restore が必要 (手順は docs/deployment.md 冒頭)。Section 4 / 8 の版数記述を更新。
- **2026-08-10**: Section 4 に「DB を使うテストの分離」を追記 (#2450)。`testutil.OpenTestDB` が呼び出し元パッケージ専用の PostgreSQL schema に接続するようになった。`go test` はパッケージを並行実行し CI の shard は DB を 1 つしか持たないため、共有すると一方の後片付けが他方を壊す (実際に Go を触っていない PR で CI が落ちた)。削除範囲を絞るだけでは解けない (干渉が双方向) 点と、migration の enum guard に `pg_type WHERE typname` を使わない旨も明記。
- **2026-08-07**: Section 3 に本家 backend e2e の Makefile target (`make upstream-e2e` 系 5 つ) を、Section 8 に `upstream-backend-e2e` workflow を追記 (#2347)。Misskey 本家の `test/e2e/**` を無改変で mk-go に向けて回す PR トリガーの workflow で、required check には含めない。既知乖離は skip でなく expected-failure (`task.fails`) で扱う運用も明記。
- **2026-08-07**: Section 8 に `diff-e2e` workflow と `frontend-check` job を追記 (#2368)。CI 非対象だった検証資産の棚卸しで、値レベル diff と fork frontend の型チェックを載せた。
- **2026-08-08**: Section 8 に `vulncheck` ジョブを追記 (#2387)。`GOOS=linux govulncheck ./...` による到達可能な既知脆弱性の検出と、`go.mod` / `Dockerfile` の Go patch version 整合チェック。required check には含めない (新規 CVE 公開でコード無変更の PR でも落ちるため)。
- **2026-08-08**: Section 8 の `dropin-e2e` workflow に `mkgo-born` シナリオを追加 (#2383)。`make dropin-mkgo-born-test` (mk-go 生まれの DB を TS に引き渡す経路 = ロックインの有無) を CI に載せる。あわせて 2 つの既存不具合を解消: (1) orchestrator が自分で残した診断ログを workflow 側の収集が空ログで上書きしていたので `-post` 付きの別名に分けた、(2) paths フィルタに `docker-compose.dropin*.yml` が無く、drop-in stack の定義を壊す変更で workflow が発火せず緑に見えていた。
- **2026-08-07**: Section 8 の `dropin-e2e` workflow に `federation` シナリオを追加 (#2362)。あわせて Section 3 に `make federation-misskey-e2e` (起動から撤去まで通しで実行) を追記。
- **2026-08-07**: Section 8 の `dropin-e2e` workflow を 2 シナリオ matrix として書き換え (#2360)。`ed25519-verify` (`make dropin-fedibird-test`) を追加し、あわせて nightly → PR トリガーへの移行 (#2291) が未反映だった記述を実態に合わせた。
- **2026-08-04**: Section 7 (Git Workflow) に「マージ方法」を追記。フィーチャーブランチ → `develop` の PR は **rebase and merge** に統一する (それ以前は squash-merge)。各コミットがそのまま develop に載るため、1 コミットずつ build / test が通る順序で並べること、確認は使い捨て `git worktree` で行うこと (作業ツリー上の `git stash` は保留中の別作業を巻き込むので使わない) を併記。`main` は従来どおり PR をマージせず FF push のみで、対象が異なる旨も明記した。
- **2026-06-09**: Section 7 (Git Workflow) に 2 つのルールを追記。(1)「Issue・PR のタイトル・本文は日本語記述を厳守する」(技術用語は原文のまま残してよいが、説明文・見出し・箇条書きの地の文に英語を混在させない)。(2)「`CHANGELOG.md` はリリース時にまとめて記述する」(個別 PR・fix ごとに `## Unreleased` へ追記せず、リリースのタイミングで一括記載する)。
- **2026-05-16**: `Makefile` に `make dropin-fedibird-test` を追加 (#1086)。Section 3 (Development Commands) の Drop-in 系コマンド一覧に Fedibird-like mock との Ed25519 e2e を載せる。
- **2026-05-07**: Playwright nightly CI workflow を Section 8 に追記 (#816)。`.github/workflows/playwright.yml` で Phase 1 spec を毎日 17:00 UTC に develop で実行する、matrix `backend = [mk-go, ts]` 並列、`fail-fast: false`、PR required check には含めない方針を明文化。
- **2026-04-28**: `internal/server`のCIカバレッジ閾値を0%例外に追加 (#462)。`avatar.go`/`avatar_test.go`の追加で同パッケージ初の`_test.go`が入り、`router.go`(2000行超のwire層)込みのpackage全体カバレッジが2.5%で計測されてCIが落ちたため。`testutil`/`e2e`と同じく実挙動はe2e/drop-in testで検証する設計に揃える。個別handlerファイルは`_test.go`単体で90%相当をカバーする運用は維持。
- **2026-04-22**: Section 3 に drop-in frontend e2e Phase 14-3 関連の Makefile target (`make dropin-frontend-mk-up` / `make dropin-frontend-mk-down` / `make dropin-frontend-swap-test`) を追加 (#394)。TS-A 切替後の mk-A でも cypress spec が pass することを検証する swap orchestrator を入口に出す。
- **2026-04-21**: Section 3 に drop-in frontend e2e Phase 14-1 関連の Makefile target (`make dropin-frontend-baseline` / `dropin-frontend-up` / `dropin-frontend-down`) を追加 (#381)。3 Misskey TS インスタンス + cypress runner 構成。
- **2026-04-21**: Section 8 に `dropin-e2e` workflow (nightly) を追記 (#374)。`make dropin-swap-test` を毎日 18:00 UTC で develop に対して実行、PR required check 非対象、失敗時 docker compose logs を 14 日 artifact 化する運用を明文化。
- **2026-04-21**: Section 3 に drop-in e2e Phase 13-2 関連の Makefile target (`make dropin-mk-up` / `dropin-mk-test` / `dropin-mk-down` / `dropin-swap-test`) を追加 (#367)。
- **2026-04-21**: Section 3 に drop-in e2e Phase 13-1 関連の Makefile target (`make dropin-up` / `dropin-test` / `dropin-down`) を追加 (#365)。Section 1 の Tests 配下にも testcontainers-go 周りの拡張ポインタを追記。
- **2026-04-20**: Section 8 の `test`ジョブを 4-way matrix shard 化として書き換え (`test-shards` 4 並列 + `test` aggregator)。総実行時間を約4.7分→約1.5-2分に短縮。各shardは独立サービスコンテナで動作し、ImportPath順modulo分配で決定的にパッケージを割り当てる。
- **2026-04-18**: Section 4 / Section 8 のカバレッジ例外閾値を更新 (#260)。`internal/repository` パッケージのテスト拡充でカバレッジを76.4%→99.9%に引き上げて CI 閾値を 90% に戻し、`internal/api/admin` の閾値を 60%→80% に引き上げ(現状83.8%)。CI step "Run all tests with coverage"に`set -o pipefail`を追加してテスト失敗の握り潰し解消も同時に。
- **2026-04-18**: Section 4 / Section 8 に `internal/repository` パッケージのCIカバレッジ閾値を暫定的に 76% に緩和する例外を追加（#260で90%復帰予定）。
- **2026-04-12**: Section 4 にテストカバレッジ目標を追記（最低90% / 推奨95% / 目標100%）。
- **2026-04-11**: 初版作成。
