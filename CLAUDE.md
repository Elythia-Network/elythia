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
make federation-mastodon-e2e # 本物の Mastodon と引用の承認 (FEP-044f) を通しで (#3234)
make diff-check              # mk-go と TS のレスポンスを値レベルで diff (#2078)
make playwright-check        # Playwright を作り直して実行
make frontend-check          # fork frontend の型チェック + submodule 依存のゲート + eslint
make frontend-lint           # eslint だけ (CI と同じ範囲、実測 55 秒)
make e2e-down-all            # 検証用スタックを一括撤去 (**本番 project `mk` は対象外**)
```

**上記は全体ではない。** `make help` が全 141 target を出す (`^名前:.*##` の行を数えた)。一覧と説明は
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

- `.github/workflows/dropin-e2e.yml` が drop-in 互換の e2e を **5 シナリオ並列**で実行する。
  `strategy.matrix.include` で make target と check 表示名を対にしている。

  | check 名 | 実行内容 |
  |---|---|
  | `swap-test` | `make dropin-swap-test` — TS→mk 切替の state preservation (#374) |
  | `mkgo-born` | `make dropin-mkgo-born-test` — mk-go 生まれの DB を TS に引き渡せるか (#2379 / #2383) |
  | `ed25519-verify` | `make dropin-fedibird-test` — Fedibird-like AP mock との Ed25519 双方向 verify (#1083 / #2360) |
  | `federation` | `make federation-misskey-e2e` — 本物の Misskey TS を相手にした実連合 (#2362) |
  | `federation-mastodon` | `make federation-mastodon-e2e` — 本物の Mastodon を相手にした引用の承認 (FEP-044f、#3234) |

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

このファイル自体を変えたときだけ、1行で追記します(新しいものを上に)。経緯の本文はリンク先にあります。個別のfixの履歴は`CHANGELOG.md`にあります。

- 2026-09-30: `federation-mastodon-e2e`シナリオを追加 (#3234) → [docs/ci.md](docs/ci.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-09-26: actionとbase imageをSHA / digestで固定 → [docs/ci.md](docs/ci.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-09-23: mkqをv1.1.1へ更新 (BullMQ 6) → [docs/development.md](docs/development.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-09-23: Goを1.27.1へ更新 → [docs/development.md](docs/development.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-09-22: asynq driverを削除 (#2985) → [docs/development.md](docs/development.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-09-22: `apicompat` workflowを追加 → [docs/ci.md](docs/ci.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-09-22: migrationのup → down → up往復テストを追加 → [docs/testing.md](docs/testing.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-09-22: echoのアクセスログを`RequestLoggerWithConfig`へ移行 → [docs/development.md](docs/development.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-09-22: golangci-lintの`unused`を有効化 → [docs/development.md](docs/development.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-09-22: golangci-lintの`SA1019`を有効化 → [docs/development.md](docs/development.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-09-22: golangci-lintの`ST1003` / `ST1012`を有効化 → [docs/development.md](docs/development.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-09-22: `lint` jobにgolangci-lintを追加 → [docs/development.md](docs/development.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-09-22: `dependency-review` workflowを追加、go.sumの検証方法を訂正 → [docs/ci.md](docs/ci.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-09-22: `codeql` workflowとactionlintを追加 → [docs/ci.md](docs/ci.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-09-22: `sqlbind-check`を追加 → [docs/gates.md](docs/gates.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-09-21: `ipshape-check`を追加 (#3136) → [docs/gates.md](docs/gates.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-09-21: `iprecord-check`を追加 (#3135) → [docs/gates.md](docs/gates.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-09-17: `nulparam-check`を追加 (#3025) → [docs/gates.md](docs/gates.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-09-12: `submodulepin-check`を追加 (#2969) → [docs/gates.md](docs/gates.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-09-12: `secretfield-check`を追加 → [docs/gates.md](docs/gates.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-09-11: `dockerignore-check`を追加 (#2942) → [docs/gates.md](docs/gates.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-09-10: `pluginembed-check`とbuild-with-plugins workflowを追加 (#2940) → [docs/gates.md](docs/gates.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-09-10: `mdtable-check`を追加 (#2930) → [docs/gates.md](docs/gates.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-09-09: `make frontend-check`にeslintを追加 (#2906) → [docs/development.md](docs/development.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-09-08: `notiftype-check`を追加 (#2898) → [docs/gates.md](docs/gates.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-09-08: `frontend-check`にsubmoduleを読むゲートを追加 (#2892) → [docs/ci.md](docs/ci.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-09-07: 更新 (運用) のコマンドを追加 (#2885) → [docs/development.md](docs/development.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-09-06: `migrationdoc-check`を追加 (#2874) → [docs/gates.md](docs/gates.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-09-06: `gaterun-check`を追加 (#2857) → [docs/gates.md](docs/gates.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-09-05: `make test`に`-race -count=1`、`testflags-check`を追加 (#2841) → [docs/testing.md](docs/testing.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-09-01: `test-shards`に`-shuffle`を追加 (#2795) → [docs/testing.md](docs/testing.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-09-04: `compose-check`を追加 (#2828) → [docs/gates.md](docs/gates.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-09-01: `notfound-check`を追加 (#2792) → [docs/gates.md](docs/gates.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-08-31: システムカタログをschemaで絞る規則、`catalog-check`を追加 (#2777) → [docs/testing.md](docs/testing.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-08-31: `wiring-check`を追加 (#2762) → [docs/gates.md](docs/gates.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-08-30: 列枠 (1600列) の話を追記 (#2756) → [docs/testing.md](docs/testing.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-08-24: 同梱プラグインの既定無効を`build` jobで検査 (#2701) → [docs/ci.md](docs/ci.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-08-20: ドキュメントを直すときのレビュー条件を追加 (#2644) → [docs/contributing.md](docs/contributing.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-08-20: ドキュメント全体監査の残りを反映 (#2640) → [docs/contributing.md](docs/contributing.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-08-19: 手順どおりに実行すると壊れる記述を修正 (#2638) → [docs/development.md](docs/development.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-08-18: `playwright` / `upstream-backend-e2e`を4シャード並列に (#2609) → [docs/ci.md](docs/ci.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-08-16: `plugin-tests` jobを追加 (#2588) → [docs/ci.md](docs/ci.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-08-15: PostgreSQLを18に統一 (#2513) → [docs/development.md](docs/development.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-08-10: DBを使うテストの分離を追記 (#2450) → [docs/testing.md](docs/testing.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-08-07: 本家backend e2eのtargetとworkflowを追記 (#2347) → [docs/ci.md](docs/ci.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-08-07: `diff-e2e`と`frontend-check`を追記 (#2368) → [docs/ci.md](docs/ci.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-08-08: `vulncheck` jobを追記 (#2387) → [docs/ci.md](docs/ci.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-08-08: `dropin-e2e`に`mkgo-born`を追加 (#2383) → [docs/ci.md](docs/ci.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-08-07: `dropin-e2e`に`federation`を追加 (#2362) → [docs/ci.md](docs/ci.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-08-07: `dropin-e2e`を2シナリオのmatrixに (#2360) → [docs/ci.md](docs/ci.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-08-04: マージ方法をrebase and mergeに統一 → [docs/contributing.md](docs/contributing.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-06-09: issue / PRの日本語記述とCHANGELOGの運用を追記 → [docs/contributing.md](docs/contributing.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-05-16: `make dropin-fedibird-test`を追加 (#1086) → [docs/ci.md](docs/ci.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-05-07: Playwrightのnightly workflowを追記 (#816) → [docs/ci.md](docs/ci.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-04-28: `internal/server`のカバレッジ閾値を0%例外に (#462) → [docs/testing.md](docs/testing.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-04-22: drop-in frontend e2e Phase 14-3のtargetを追加 (#394) → [docs/ci.md](docs/ci.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-04-21: drop-in frontend e2e Phase 14-1のtargetを追加 (#381) → [docs/ci.md](docs/ci.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-04-21: `dropin-e2e` workflowを追記 (#374) → [docs/ci.md](docs/ci.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-04-21: drop-in e2e Phase 13-2のtargetを追加 (#367) → [docs/ci.md](docs/ci.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-04-21: drop-in e2e Phase 13-1のtargetを追加 (#365) → [docs/ci.md](docs/ci.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-04-20: `test` jobを4 shardに分割 → [docs/ci.md](docs/ci.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-04-18: カバレッジの例外閾値を更新 (#260) → [docs/testing.md](docs/testing.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-04-18: `internal/repository`の閾値を暫定で緩和 → [docs/testing.md](docs/testing.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-04-12: カバレッジ目標を追記 → [docs/testing.md](docs/testing.md#変更の経緯-旧-claudemd-の更新記録)
- 2026-04-11: 初版作成 → [docs/contributing.md](docs/contributing.md#変更の経緯-旧-claudemd-の更新記録)
