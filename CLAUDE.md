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

## 8. CI

**required check は`build` / `test` / `lint`の3つです。** 手元で`make check`を通せば、ほぼ再現できます。

| workflow / job | 発火 | required | 見ているもの |
|---|---|---|---|
| `ci.yml` build | push / PR | ○ | `go build ./...`、submoduleのcommitがforkにpush済みで`docs/divergence.md`のpinのtagと一致するか、同梱プラグインの`disabled: true`、同梱プラグインの`go vet` |
| `ci.yml` test | push / PR | ○ | 4 shardで`-race -count=1 -shuffle=3`、パッケージごとのカバレッジ閾値 |
| `ci.yml` lint | push / PR | ○ | vet / gofmt / actionlint / golangci-lint / テストfixtureのID重複 |
| `ci.yml` plugin-tests | push / PR | | 同梱プラグインのテスト、`authoring.md`のスニペットのコンパイル |
| `ci.yml` frontend-check | push / PR | | fork frontendの型チェック、eslint、vitest、submoduleを読むゲート |
| `ci.yml` vulncheck | push / PR | | govulncheck、`go.mod`とDockerfileのGoの版の一致 |
| `dependency-review` | PR | | PRが持ち込む依存の既知脆弱性 |
| `codeql` | PR / push / 週1回 | | Goとworkflowの静的解析 |
| `dropin-e2e` | PR(paths限定) | | TS↔mk切替、実Misskey / Mastodonとの連合など5シナリオ |
| `playwright` | PR(paths限定) | | ブラウザのe2e(4 shard) |
| `upstream-backend-e2e` | PR(paths限定) | | 本家のbackend e2eを無改変で実行(4 shard) |
| `diff-e2e` | PR(paths限定) | | TSとの値レベルの差分 |
| `apicompat` | PR(paths限定) | | `docs/api-compat.md`が実態と一致しているか |
| `build-with-plugins-selftest` | PR(paths限定) | | 運営者向けreusable workflowのビルド |
| `docker` | `main` / `develop` / tagへのpush、PR | | imageがビルドできるか。PR以外ではimageをpublishする |
| `docker-branch` | `develop`へのpush(paths限定) | | composeだけを載せた配布用ブランチ`docker`を更新 |
| `build-with-plugins` | 呼び出し専用(`workflow_call`) | | 運営者が自分のリポジトリから呼び、プラグイン入りのimageを作る |
| `dropin-frontend-e2e` | 毎日(schedule) | | 3つのTSインスタンス + cypressで、frontend視点のdrop-in互換 |
| `queue-bench-smoke` | 毎日(schedule) | | queue driverがジョブを落としていないか |

`ci.yml` / `dependency-review` / `build-with-plugins`以外は、手動(`workflow_dispatch`)でも起動できます。scheduleのものはPRでは回らないので、失敗はActions上で確認して別のPRで直します。各workflowの設計理由と、落ちたときの対処は[docs/ci.md](docs/ci.md)にあります。

### workflowを書くときのルール

- **非ブロッキングにしたいときも`continue-on-error`を使わない。** jobが成功扱いになり、失敗が見えなくなる。required checkから外すことで表す
- **actionはcommit SHAで固定する**(`# vX.Y.Z`のコメントを付ける)。tagは付け替えられるため
- **配るDockerfileのbase imageは`<tag>@sha256:<digest>`で固定する。** floating tag(`golang:1.27-alpine`)に戻さない
- **ツールの版はMakefileに1つだけ置き、CIは`make <target>`を呼ぶ。** 書き写すと版がずれる
- **`make test`とCIのテストの条件を揃える。** `make testflags-check`が検査している
- **skipを成功として扱わせない。** 前提が欠けたら落とす環境変数(`MK_PLUGIN_TESTS_REQUIRE_DB`など)を渡す
- **新しいworkflowはPR上で発火させて確かめる。** `workflow_dispatch`だけでは、default branchにあるものしか起動できない

### CIが落ちたとき

- カバレッジが足りない → テストを足す
- gofmtの差分 → `make fmt`
- テストの失敗 → ログを読み、手元で再現してから直す。`-shuffle`のseedは全shardで共通の`3`なので、`go test -shuffle=3`で順序を再現できる
- フックを`--no-verify`などで飛ばさない

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
