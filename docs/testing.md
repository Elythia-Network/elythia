# テスト

> CI で実際に何が回っていて、落ちたとき何を疑うかは [CI で回る項目](ci.md) を参照。
> 本ドキュメントはテストの種類と書き方を扱う。


## テストの種類

| 種類 | 対象 | DB/Redis | 実行方法 |
|---|---|---|---|
| ユニットテスト | APIハンドラ、サービスロジック | モック | `go test ./internal/api/...` |
| 統合テスト | リポジトリ、Redis連携 | 実 PostgreSQL (`TEST_DB_*`) + Redis (testcontainers) | `go test ./internal/core/...` |
| E2Eテスト (Playwright) | フロントエンド操作 / API | 実DB + フロントエンド | `make playwright-test` (詳細は[Playwright](playwright.md)) |
| 連合テスト | Elythia ↔ 本物の Misskey TS の AP 通信 | Docker Compose多段 | `make federation-misskey-e2e` (起動から撤去まで通し。個別に叩くなら `-up` → `-test` → `-down`) |
| 連合テスト (Mastodon) | Elythia ↔ 本物の Mastodon の引用の承認 (FEP-044f) | Docker Compose多段 | `make federation-mastodon-e2e` (起動から撤去まで通し) |
| Drop-in e2e (pytest) | TS-A backend を mk-A に差し替えて state preservation 検証 | TS 2 instance + mk overlay | `make dropin-swap-test` (#365 / #367 / #372 / #374、詳細は[dropin-e2e.md](dropin-e2e.md)) |
| Drop-in frontend e2e (cypress) | 3 TS instance + mk overlay swap で frontend 視点の互換 | cypress + 3 TS + mk-A | `make dropin-frontend-swap-test` (#380 / #381 / #387 / #394、詳細は[dropin-frontend-e2e.md](dropin-frontend-e2e.md)) |
| Playwright e2e | Elythia と Misskey TS の両 backend で API/frontend 統合互換を検証 | Docker Compose 全部 | `tests/playwright/` 配下 (#744、298 spec ファイル。PR ごとに Elythia、upstream 追従時に TS backend) |
| 本家 backend e2e | Misskey 本家の `test/e2e/**` をテスト本体無改変で Elythia に向けて実行 | PostgreSQL / Redis + Elythia バイナリ | `make upstream-e2e` (#2347、25 ファイル 1256 テスト。詳細は[upstream-backend-e2e.md](upstream-backend-e2e.md)) |

## 手元の準備

**PostgreSQL は自分で用意する。** Redis は testcontainers が立ててくれるが、DB を使うテストの大半は外部の PostgreSQL に直接つなぐので、Docker があるだけでは `make test` は通らない (下記「testcontainers」参照)。

**必要なのは PostgreSQL 側の準備。** 既定では `localhost:5432` の `misskey_test` データベースに `mk` / `mk` で接続する。

```bash
sudo -u postgres psql -c "CREATE ROLE mk LOGIN PASSWORD 'mk'"
sudo -u postgres psql -c "CREATE DATABASE misskey_test OWNER mk"
```

**接続先が既定と違うときだけ** `.env.test` を置く。

```bash
cp .env.test.example .env.test    # 中身は既定値そのままなので、編集して使う
```

`internal/testutil` は接続時 (`OpenTestDB` などの呼び出し時) にこれを読み、既に設定済みの環境変数は上書きしない。**export で直接渡してもよい。** `.env.test` は `.gitignore` 済み。

`TEST_REDIS_*` は `.env.test.example` に無い。これを読むのは `internal/core/chart` だけで、実 Redis (既定 `localhost:6379`) が無ければ **silently skip** する (落ちないがカバレッジだけ下がる)。他は `SetupRedis` が環境変数を見ずに testcontainers を立てる。CI は service container を立てて同じ環境変数を渡している。

## 実行方法

```bash
# 全テスト (.env.test か TEST_DB_* で指した PostgreSQL に接続する)
make test

# 特定パッケージ
go test ./internal/api/notes/...

# レース検出 + カバレッジ (CIと同条件)
go test -race -count=1 -shuffle=3 -timeout 10m \
  -coverprofile=coverage.out -covermode=atomic ./...

# `make test` はこのうち -race / -count=1 / -shuffle=3 の 3 つが同じ (#2841)。
# **カバレッジは取らない**ので、閾値の検査には上のコマンドを使うこと。
# **`-race` は cgo を要求する**ので、CGO_ENABLED=0 や C コンパイラの無い環境では
# `make test-fast` を使う (ただしそれはコミット前の検査にならない)。

# カバレッジHTMLレポート
go tool cover -html=coverage.out
```

## カバレッジ目標

| レベル | 閾値 | 説明 |
|---|---|---|
| CIゲート (最低ライン) | 90% | これを下回るとCIが失敗しマージ不可 |
| 推奨ライン | 95% | 通常のPRではここを目指す |
| 目標ライン | 100% | 新規パッケージや小規模パッケージで積極的に狙う |
| `internal/api/admin` | 80% | SMTP/queue/DB集計等の外部依存で 90% 未到達のため暫定緩和 (#260 以降) |
| `internal/testutil` | 0% | mock / test helper 専用、production code を含まないため閾値対象外 |
| `internal/server` | 0% | router.go (~2000 行) の wire 層中心、e2e/drop-in test で実挙動検証 (#462) |
| e2e | 0% | 統合 test 専用 |

CIではパッケージごとにカバレッジを計測し、閾値未達のパッケージがあればジョブが失敗する。CI は **4-way matrix shard** で並列実行され、ImportPath 順 modulo 分配で決定的にパッケージを割り当てる (約 4.7 分 → 1.5-2 分に短縮)。

CI は **`-shuffle=3` を全 shard 共通の固定値として**回す。`on` (毎回ランダム) は失敗を手元で再現できず、required check の `test` が不定期に赤くなる。**shard 番号も使わない** — shard 配属は`NR % 4` なので、テストパッケージが 1 つ増えるだけで既存パッケージの seed が変わり、順序が丸ごと入れ替わる (無関係な PR が未実行の順序を引いて赤くなる)。落ちたら`go test -race -count=1 -shuffle=3 ./<package>/` でそのまま再現する (`make test` も同条件で走る、#2841)。

**1 パッケージが試す順序は 1 通り**なので、`-shuffle` だけで全ての順序依存が見つかるわけではない。seed の値を時々変えると新しい順序を試せる。

**カバレッジは順序に依存しうる。** 実測で 173 パッケージ中 3 つ (`internal/core/reversi` / `internal/core/role` / `internal/testutil`) が seed によって 0.1-0.5pt 動いた。いずれも閾値まで 2pt 以上あるので現状は落ちないが、閾値ぎりぎりのパッケージを 90.0% 台で放置すると seed 変更で落ちうる。

**プロセス共有の状態を張り替えるテストは必ず戻すこと。** `internal/server` の `newServer` / `New` はグローバルを 12 個 (`entity` の 7 つ: `SetMediaURLContext` / `SetAvatarDecorationLookup` / `SetCanChatLookup` / `SetUserRolesLookup` / `SetShowRemoteBadgesLookup` / `SetInstanceIconURLLookup` / `SetSilencedLookup`、加えて `notehide.SetFollowingRepo` / `coretwofactor.SetTestMode` / `meself.SetEnricher` / `password.SetCost` / `corenote.SetHookConcurrency`) 差し替える。1 度きりの起動を模したテストが戻さなかったため、後続の `avatar` / `emoji_redirect` が素の URL ではなく署名付きプロキシURL を受け取って落ちていた (5 seed すべてで失敗)。`internal/server/global_state_test.go` の `restoreProcessGlobals` を使う。`frontendutil` の loader キャッシュも同様で、fixture は `t.TempDir()` に置くので**ディレクトリが消えた後もキャッシュに内容が残る**。

## DB を使うテストの分離 (#2450)

`testutil.OpenTestDB` / `MustOpenTestDB` は**呼び出し元のパッケージ専用の PostgreSQL schema** に接続する (`internal/api/gallery` なら `internal_api_gallery`)。schema 名は呼び出し元から自動で決まるので、新しいパッケージも何もしなくても隔離される。

`go test` は**パッケージのテストバイナリを並行実行する**。CI は shard ごとに PostgreSQL を 1 つしか立てないため、共有すると一方の後片付けが他方の前提を壊す。実際に `internal/charttick` の `DELETE FROM "user"` が `internal/api/gallery` の所有者 user を消し、**Go を一切触っていない PR で CI が落ちた**。

削除範囲を絞るだけでは解けない。charttick は**テーブル全体の絶対件数**をアサートするので、絞ると今度は他パッケージの行が混ざって charttick 自身が落ちる。干渉は双方向。shard 分配は `go list` 順の `NR % 4` なので、テストパッケージを 1 つ足すだけで同居の組み合わせが変わる。個別の衝突を潰す対処では再発する。

守ること:

- **DB を読み書きするテストで `OpenSharedTestDB` を使わない。** これは `internal/db` のように接続処理そのものを試すテスト専用
- schema が分かれているので `DELETE FROM "user"` のような無条件の削除は書いてよい。ただし**それは自分の schema に閉じている前提**に依存するので、`search_path` を跨ぐ生 SQL (`public.` 明示など) を書かない
- **システムカタログも `search_path` に従わない (#2777)。** 参照は `pg_catalog` で解決されるが、**返る行は全 schema 分**。必ず自分の schema に絞る: `pg_indexes` は `schemaname = current_schema()`、`information_schema.columns` / `.tables` は `table_schema = current_schema()` (このリポジトリで最も多いのはこちら)、`pg_class` は `pg_namespace` を join して `n.nspname = current_schema()` (`pg_class` は schema を oid で持ち `schemaname` 列が無い。`pg_attribute` は relation の oid しか持たないので `pg_class` 経由の 2 段 join になる)。**`information_schema.schemata` は対象外** — schema の一覧そのものなので絞る概念が無い。絞らないと 2 つ壊れる — (a) 他 schema の同名オブジェクトを自分のものと取り違えて regression guard が空振りし、(b) 他パッケージの `ApplyMigrations` が DDL 中だと `could not open relation with OID (SQLSTATE XX000)` で落ちる。**CI でも起きる** — shard は PostgreSQL を 1 つしか立てないので手元と同じ条件が揃い、required check の `test` が不定期に赤くなる
- **複数行が返りうるクエリを `Scan(&string)` で受けない (#2777)。** GORM は `*string` に対し**全行を走査して dest を上書きし続ける**ので、複数行が返ると**最後の 1 行**が残る。実測では `pg_indexes` の絞りを外すと 17 件中 17 番目 (`internal_repository_ts`) の定義が返り、**それでもテストが緑のまま通っていた** — 上の (a) の実例。slice で受けて件数と schema 名を確かめる (`internal/repository/index_lookup_test.go` の `indexDef` が例)
- 行の投入は**戻り値を検査する** (`require.NoError(t, db.Create(x).Error)`)。捨てると FK 違反が黙って流れ、「200 のはずが 400」のような原因から遠い症状に化ける

migration で enum を作るときは `EXCEPTION WHEN duplicate_object THEN NULL` を使う。`pg_type WHERE typname = ...` は **schema を見ない**ため、別 schema に同名の型があるだけで作成を飛ばし、直後の `CREATE TABLE` が落ちる。

### 列枠を食う操作を書かない (#2756)

PostgreSQL は `DROP COLUMN` した列も **1 テーブル 1600 列**の上限に数える。手元の schema は実行をまたいで残るので、テストのたびに列を落とす形にすると枠が減り続け、最後は `tables can have at most 1600 columns (SQLSTATE 54011)` で落ちる。CI は毎回クリーンな DB を立てるので**手元で繰り返す開発者だけが踏む** (実際に `clip` / `auth_session` / `app` が 1593 列まで到達した)。

- `ApplyMigrations` は適用済みの migration を skip する (`testutil_applied_migrations` 台帳。**ファイル名 + 内容の sha256** で持つので、migration を書き換えれば流し直す。失敗したものは記録しないので次回また流す)
- schema の形を変えて試すテストは `testutil.OpenTestDBSchema("<suffix>")` で**専用の兄弟 schema** を作り、そこを一度だけその形にして使い回す (`internal/repository/dropin_ts_schema_test.go` が例)

**復旧は schema を作り直すしかない** (`ALTER TABLE ... ADD COLUMN` では枠は戻らない)。**兄弟 schema も一緒に消すこと。** 次の実行が作り直す (全 migration の適用は実測 1-3.5 秒)。

```bash
# 接続先は既定値。変えているなら .env.test / TEST_DB_* に合わせる。
# 落とすのは「そのテストが使う schema」。名前はパッケージパスの `/` を `_` に
# したもので、兄弟 schema は `<package>_<suffix>` になる。
PGPASSWORD=mk psql -h localhost -U mk -d misskey_test \
  -c 'DROP SCHEMA IF EXISTS "internal_repository" CASCADE' \
  -c 'DROP SCHEMA IF EXISTS "internal_repository_ts" CASCADE''
```

**テストが途中で死んで schema が壊れたときも同じ手順**。`internal/core/fsck` (schema は `internal_core_fsck`) にはテーブルを一時的に rename したり制約を落として `t.Cleanup` で戻すテストがあり、プロセスが殺されると戻らない (`relation "drive_file" does not exist` 等になる)。

**これは台帳を入れる前から直らない。** `drive_file` や `note_userId_fkey` を作る `000001_initial` は既存 schema への再適用が必ず失敗する (制約重複) ので、以前から流し直しても戻らなかった。台帳で新たに直らなくなったのは、**単独で成功する migration が作るオブジェクト** (`000008` 以降の大半) だけ。いずれにせよ手で消す。

## testcontainers

`internal/testutil/containers.go` が testcontainers-go で PostgreSQL 18 / Redis 7 のコンテナを起動するヘルパー (`SetupPostgres` / `SetupRedis` / `SkipIfNoDocker`) を提供する。

**PostgreSQL と Redis で使われ方がまったく違う。**

件数はいずれもリポジトリ全体 (`internal/` + `test/`) の実測。

| | testcontainers | 外部サービス |
|---|---|---|
| Redis | `SetupRedis` を **27 パッケージ**が使う。**`SkipIfNoDocker` を置いているのは 7 つだけで、残り 20 は `TestMain` で `log.Fatalf` する** (= Docker が無いとそのパッケージは落ちる) | — |
| PostgreSQL | `SetupPostgres` は `internal/api/test` / `tests/e2e` / `tests/e2e-federation` の **3 パッケージだけ** | `OpenTestDB` / `MustOpenTestDB` を **15 パッケージ**が使い、`TEST_DB_*` の指す PostgreSQL に直接つなぐ |

つまり **Redis は Docker があれば足りるが、PostgreSQL は自分で用意する必要がある**。`MustOpenTestDB` は失敗時に panic し、しかも `init()` から呼ばれるので、PostgreSQL が無いと該当パッケージはまとめて落ちる (skip されない)。

```go
// PostgreSQLコンテナ起動 + マイグレーション自動適用
testDB, err := testutil.SetupPostgres(ctx)
defer testDB.Teardown(ctx)

// Redisコンテナ起動
testRedis, err := testutil.SetupRedis(ctx)
defer testRedis.Teardown(ctx)

// テスト間のデータクリーンアップ
testDB.TruncateAll()
testRedis.FlushAll(ctx)
```

Docker環境がない場合は`testutil.SkipIfNoDocker(t)`でテストをスキップする。

### CI環境

CI では GitHub Actions の `services` で PostgreSQL / Redis を起動し (`SetupRedis` を使うテストは CI でも別途 testcontainers を立てる)、環境変数で接続先を指定する:

| 環境変数 | 値 |
|---|---|
| `TEST_DB_HOST` | `localhost` |
| `TEST_DB_PORT` | `5432` |
| `TEST_DB_NAME` | `misskey_test` |
| `TEST_DB_USER` | `mk` |
| `TEST_DB_PASS` | `mk` |
| `TEST_DB_SSLMODE` | `disable` |
| `TEST_REDIS_HOST` | `localhost` |
| `TEST_REDIS_PORT` | `6379` |

## テストパターン

### APIハンドラテスト (モック)

```go
func newTestHandler(t *testing.T) (*Handler, *testutil.MockUserRepository) {
    userRepo := testutil.NewMockUserRepository()
    metaRepo := testutil.NewMockMetaRepository()
    metaRepo.Meta = &model.Meta{ID: "x"}
    // モックをサービスに注入
    svc := NewService(userRepo, metaRepo)
    h := NewHandler(svc)
    return h, userRepo
}

func doPost(h func(echo.Context) error, body string, user *model.User) *httptest.ResponseRecorder {
    e := echo.New()
    req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
    req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
    rec := httptest.NewRecorder()
    c := e.NewContext(req, rec)
    if user != nil {
        c.Set(string(middleware.UserContextKey), user)
    }
    _ = h(c)
    return rec
}
```

### サービステスト (実Redis)

```go
var testRedis *testutil.TestRedis

func TestMain(m *testing.M) {
    ctx := context.Background()
    tr, err := testutil.SetupRedis(ctx)
    if err != nil {
        log.Fatalf("redis setup failed: %v", err)
    }
    testRedis = tr
    code := m.Run()
    testRedis.Teardown(ctx)
    os.Exit(code)
}

func newSvc(t *testing.T) *Service {
    t.Helper()
    testRedis.FlushAll(context.Background())
    return NewService(testutil.NewMockRepository(), testRedis.Client)
}
```

## モック一覧

`internal/testutil/`に以下のモック実装がある:

| ファイル | 内容 |
|---|---|
| `mock_repository.go` | User, Note, Following, Reaction, Meta, Role, Channel, Chat等 (~20種) |
| `mock_drive.go` | DriveFileRepository, DriveFolderRepository |
| `mock_block_mute.go` | BlockingRepository, MutingRepository, RenoteMutingRepository |
| `mock_allowlist.go` | AllowlistChecker (mediaproxy用) |
| `errors.go` | テスト用エラー定数 |

各モックはインメモリの`map[string]*Model`でデータを保持し、CRUD操作をシミュレートする。

## 連合テスト

`tests/federation/compose.misskey.yml`でElythiaとMisskey TSの2インスタンスを起動し、AP通信をテストする。

```bash
# ビルド + 起動
make federation-misskey-up

# テスト実行
make federation-misskey-test

# ログ確認
make federation-misskey-logs

# 停止
make federation-misskey-down
```

テストはPython (pytest)で記述され、`tests/federation/`に配置。両インスタンスに共通のAPI互換クライアント(`MisskeyLikeClient`)を使ってフォロー、ノート作成、リアクション等の連合動作を検証する。

## Playwright e2e (drop-in 互換)

`tests/playwright/` 配下の spec を Elythia と Misskey TS の **両 backend** で並列実行し、drop-in 互換 regression を PR ごとに検出する基盤。

- 範囲: 298 spec ファイル (upstream 290 = ui 194 / api 96、mkgo 8) / 40 directory (spec を直接含むもの。`find ... -printf '%h\n' | sort -u | wc -l`)
- トリガー: `pull_request` (paths フィルタ) + `workflow_dispatch`。**nightly ではない** (#2291 で移行)。`.github/workflows/playwright.yml`
- **4 シャード並列** (`--shard=i/4`、`fail-fast: false`)。1 スタックに対しては直列でしか回せない (共有の root と instance meta を spec が取り合う) ので、並列度はシャードごとに独立した stack を立てて稼ぐ (#2609)
- **TS backend は `workflow_dispatch` 専用**で PR では回らない。upstream が変わらない限り答えも変わらないため、追従する本家の版を上げたタイミングだけ回す
- spec は原則 **backend-agnostic** (= URL 切替だけで両 backend で動く)、spec 失敗 = drop-in 互換 regression として issue 化。例外は `specs/mkgo/` の 8 件 (Elythia 独自機能を見るので公式 image では通らない)。`make playwright-ts-test` が `specs/upstream` に絞ることで除外している

### spec を書くときの注意: root の per-user quota

UI spec は root (alice) を共有する。antenna / webhook / clip / avatar decoration には role policy の上限があり、**作りっぱなしにすると枠を使い切って無関係な spec が setup の create で落ちる**。落ちる場所が原因から離れるので診断が難しい (#2254 の調査中、実際に spec 側の bug と誤認しかけた)。

| policy | 既定値 |
|---|---|
| `antennaLimit` | 5 |
| `webhookLimit` | 3 |
| `clipLimit` | 10 |
| `avatarDecorationLimit` | 1 |

対策は 2 段構え (#2264):

- **globalSetup が run の先頭で root の quota を purge する** — 前回 run の残骸対策。同じ stack を使い回してもクリーンな状態から始まる
- **spec 自身が afterEach で片付ける** — 1 回の run の中で枠を食い潰さないため。`fixtures/quota.ts` の `deleteAntennasNamed` / `deleteWebhooksNamed` を使う

上限のあるリソースを新しく作る spec を足すときは、後者を必ず入れること。

```ts
import { deleteAntennasNamed } from '../../fixtures/quota';

const createdAntennas: string[] = [];
test.afterEach(async ({ request }) => {
  await deleteAntennasNamed(request, root.token, createdAntennas);
  createdAntennas.length = 0;
});
```

`test.afterAll` では test-scope の `request` fixture が使えないので `afterEach` を使う。

### Drift detection workflow

Playwright で発見した drift は LCD 化 → strict 化 のサイクルで消化する:

1. spec を書いて両 backend で走らせる
2. 挙動が異なる場合は `expect([200, 204]).toContain(...)` 等の **LCD (Lowest Common Denominator)** で吸収して両 backend pass させる
3. LCD のコメントで drift 内容を記録、別 issue として起票
4. drift fix PR で Elythia 側を strict 仕様 (= upstream Misskey TS の挙動) に揃える
5. 同 PR で spec の LCD を strict (`expect(...).toBe(204)` 等) に格上げ

**実績**: Phase 1-4 で 40+ 件の drift を fix。詳細は [api-compatibility.md](api-compatibility.md) の
「Playwright Phase 1-4 由来の drift backlog」section、または #744 / #947 tracker 参照。

## 差分比較 e2e (値レベル)

Elythia と Misskey TS に**同一リクエストを投げてレスポンスを値レベルで diff** する
(#2078、endpoint 比較 35 件)。守備範囲が他のゲートと違う。

pytest の総数は 48 だが、うち 13 は `diff_core.py` (差分の取り方そのもの) の
ユニットテストで、**Elythia と TS を突き合わせているのは 35 件**。

| ゲート | 見ているもの |
|---|---|
| 本家 backend e2e | 本家のテストが通るか |
| shape drift | フィールドの有無・型 |
| **diff-test** | **同じ入力に対する値そのもの** |

shape が合っていても値が違う類のバグはこれでしか捕まらない。

```bash
make diff-check    # down → up → healthy 待ち → pytest を通しで
make diff-test     # スタックが既に上がっている場合
```

意図的な差分は `tests/diff/test_endpoints.py` の ignore-list に**理由付きで**登録する。
`META_IGNORE` と `USER_IGNORE` は別定義で後者は前者を継承していないので、`policies` の
ような両方に現れるキーは両方へ足す必要がある。

**ignore-list を安易に広げないこと。** 空振りさせると本物の乖離が埋もれる。追加時は
`docs/divergence.md` に対応する記述があるかを確認する。

PR ごとに `.github/workflows/diff-e2e.yml` が実行する (required check ではない)。
詳細は [diff-e2e.md](diff-e2e.md)。

## 本家 backend e2e

Misskey 本家の backend e2e (`make upstream-fetch` が取得する `.cache/misskey/<版>/packages/backend/test/e2e/**`、#3378) を、
**テスト本体に一切手を入れずに** Elythia へ向けて実行する。差し替えるのは vitest 設定の
2 点 (globalSetup = Elythia バイナリの起動、setupFiles = `/api/reset-db`) だけなので、
上流でテストが増えれば自動的に検証対象も増える。

```bash
make upstream-e2e-deps         # 初回 / UPSTREAM_MISSKEY_VERSION を上げた後 (本家の取得も行う)
make upstream-e2e-up           # PostgreSQL / Redis
make upstream-e2e-migrate
make upstream-e2e-test         # FILE=test/e2e/note.ts で 1 ファイルだけも可
```

『通らないことが正しい』テストは `tests/upstream-e2e/known-divergences.json` に**根拠付きで**
登録し、vitest の expected-failure (`task.fails`) として扱う。skip ではないので、乖離が
解消して通るようになったテストは逆に落ちて一覧の陳腐化に気付ける。

PR ごとに CI で実行する (`.github/workflows/upstream-backend-e2e.yml`、required check には
入れない)。詳細は [upstream-backend-e2e.md](upstream-backend-e2e.md)。

## Drop-in テスト

state preservation や frontend 視点の drop-in 互換を検証する 2 系統:

### Drop-in e2e (pytest, `tests/dropin/`)

Misskey TS 2 インスタンス (TS-A / TS-B) を起動して federation smoke を実行する基盤に、`tests/dropin/compose.mk.yml` overlay で TS-A の backend を mk-A に差し替えて **state 引き継ぎ** を検証する。

```bash
make dropin-up                 # TS-A / TS-B 起動 (smoke baseline)
make dropin-mk-up              # 上から mk-A overlay (= clean DB の mk-A)
make dropin-swap-test          # TS-then-mk 切替シナリオ (bash orchestrator)
```

PR ごとに `.github/workflows/dropin-e2e.yml` が **5 シナリオ**を並列実行する
(`fail-fast: false`)。required check には入れない。

| check 名 | make target | 見ているもの |
|---|---|---|
| `swap-test` | `dropin-swap-test` | TS→mk 切替で state が保たれるか (#374)。TS へ戻す stage 6b-9 は測る対象 (#3191) |
| `mkgo-born` | `dropin-mkgo-born-test` | **Elythia 生まれの DB を TS に引き渡せるか** (#2383。測る対象で、保証はしない、#3191) |
| `ed25519-verify` | `dropin-fedibird-test` | Fedibird-like mock との Ed25519 双方向 verify (#1083) |
| `federation` | `federation-misskey-e2e` | 本物の Misskey TS を相手にした実連合 (#2362) |
| `federation-mastodon` | `federation-mastodon-e2e` | 本物の Mastodon を相手にした引用の承認 (FEP-044f、#3234) |

`swap-test` と `mkgo-born` は似て見えるが **DB を作った側が違う** (前者は TypeORM、
後者は Elythia の migration)。TS が一度も触っていない schema を受け取るのは後者だけ。

`make dropin-fedibird-test` は Fedibird-like な AP mock を立てて **Ed25519 署名の
双方向 verify** を検証する (#1083)。Ed25519 は Elythia 独自の先行実装なので、他実装と
相互運用できるかは実際に喋らせないと分からない。ユニットテストは「自分で署名して
自分で検証する」ことしか保証しない。

### Drop-in frontend e2e (cypress, `tests/dropin-frontend/`)

3 Misskey TS インスタンス (A/B/C) + cypress runner で実ブラウザから frontend 視点の drop-in 互換を検証する。Phase 14-3 (#394) で TS-A → mk-A 切替後も spec が pass することを e2e 確認 (`CYPRESS_MODE=baseline|swap` で skip 制御)。

```bash
make dropin-frontend-baseline      # TS-A/B/C + cypress baseline spec 実行
make dropin-frontend-swap-test     # TS-A → mk-A 切替まで含む end-to-end
```

nightly 19:00 UTC で `dropin-frontend-e2e` を実行 (`.github/workflows/dropin-frontend-e2e.yml`)。

## 変更の経緯 (旧 CLAUDE.md の更新記録)

CLAUDE.md の「更新記録」に書かれていた本文を、#3248 でここへ移した。**記述は当時のまま**で、文中の「Section N」は当時の CLAUDE.md の節を指す。新しいものが上。

- **2026-09-22**: migration の **up → down → up 往復テスト**を追加
  (`internal/repository/migration_roundtrip_test.go`)。**書いた瞬間に本物のバグを 1 件
  見つけた** — `000001_initial.down.sql` が `DROP TABLE IF EXISTS "schema_migrations"` を
  持っており、golang-migrate が自分で管理するテーブルを消していた。`Down()` は全 down の
  あとに `TRUNCATE schema_migrations` を撃つので、**`go run ./cmd/elythia migrate -direction down`
  (CLAUDE.md Section 3 が全段ロールバックとして案内している手順) は毎回最後に
  `relation does not exist (SQLSTATE 42P01)` で落ちていた**。`make migrate-down`
  (`-steps 1`) も version 1 のときは同じ理由で落ちる。**全段 down の後は
  `schema_migrations` が 1 つだけ空で残る** — Section 3 ほか 6 箇所が「schema が消える」と
  書いていたが `DROP SCHEMA` は一度も走らないので元から不正確で、「全テーブルが消える」へ
  直した。
  **`testutil.ApplyMigrations` では代用できない。** あちらの `findMigrationFiles` は
  `*.up.sql` しか glob しないので **down を 1 本も実行しない**。加えて up 側も `db.Exec` の
  エラーを握り潰す (`continue`) ので壊れた SQL でも緑になる。本番の `elythia migrate` と同じ
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
- **2026-09-05**: `make test` に `-race -count=1` を足し、`-race` 抜きの `make test-fast` を新設 (#2841)。`make help` の target は 116 → 118 (`test-fast` と `testflags-check`。数え方は `^名前:.*##` の行数)。**Section 3 の「115」が古くなった起点は #2828 ではなく #2844** (`frontend-test` の追加で 116。`e1fd1e06` が 115、`f9ec2716` が 116 と実測。#2844 のコミットメッセージ自身が「116 → 117」と誤記していた)。**順序依存は #2795 で seed を揃えて塞いだのに、データ競合は塞げていなかった** — `make test` は `-race` 無しで回るので、手元で緑のまま required check の `test` が落ちる。`fe7ea8f2` (2026-09-03「Fix CI: SendMeasuresEnvelope のテストが -race で落ちる」) で実際に踏んでいる。**実測は 65.0s → 160.5s (2.5 倍)**、`-race` が全 173 パッケージで競合ゼロ (= 揃えるために先に潰す既存の競合は無い) であることも確認した。CI の `test-shards` は 1 shard あたり実測 158-290s (直近 5 run × 4 shard の job 全体。テスト step 単体は 114-241s)。shard は並列なので `test` check の wall clock は max(shard) だが、**実際の往復は push から結果まで 4m10s-4m59s** かかるので、手元で 95s 払うほうが速い。`-count=1` 自体のコストはゼロだった (65.35s → 65.04s)。**`-shuffle` は `go test` の cacheable flag に入っていない**ので、seed を渡している時点でキャッシュは元から無効。`-count=1` を残すのは CI との一致のためで、キャッシュ対策としては効いていない。`-timeout` / `-coverprofile` / `-covermode` は揃えない — 前者は既定と同じ 10m、後 2 つはカバレッジ閾値チェック用で挙動に影響しない。**`make test-fast` はコミット前の検査ではない** (`-race` が無いので CI で落ちるものが手元で緑になる)。編集しながら回す用で、`make check` は `-race` 付きを使う。
  **ドリフト自体を止めるゲートも足した** (`make testflags-check` / `TestMakeTestMatchesCIConditions`)。**CI 側を基準にする** — CI の flag のうち `ciOnlyTestFlags` に理由付きで挙げたもの以外は `make test` にも同じ値で無ければ落ちる。CI に flag が増えたときに「足す」か「無視する理由を書く」かを**選ばせる**形にしてある (無条件に無視すると #2841 と同じことが起きる)。片側だけ変えても落ちるので、`ci.yml` の seed を変えて Makefile を忘れる形も塞がる。**どちらかを読めなかったら落とす** — 書式が変わって拾えなくなると、検査していないのに緑になる (compose-check と同じ判断)。あわせて `TestDocsQuoteTheCIShuffleSeed` で **doc に書かれた seed が CI と一致するか**も見る — これは実際に 2 度起きていて、#2795 で seed を `3` にしたあとも `docs/testing.md` と CLAUDE.md には `2795` が残り、**唯一の再現コマンドが間違ったまま**だった (doc の手順で追うと別の並び順を試すので順序依存が再現せず flaky と誤診断される)。**値の不一致は tracked な md 全体**で見て、**欠落だけ名指しの一覧**で見る — 合計件数だと 1 ファイルが `-shuffle` を丸ごと落としても気付けない (実際 README.md が「CI と同条件」と書きながら `-shuffle` を持っていなかった)。正規表現は `-shuffle` への隣接を要求する — 裸の `2795` は issue 番号としても現れるため。散文で書くと拾えないので、doc 側は `-shuffle=3` のインライン表記に統一してある。
- **2026-09-01**: Section 8 の `test-shards` に `-shuffle` を追加 (#2795)。**`internal/server` は `-shuffle` を有効にすると 5 seed すべてで落ちていた** (落ちるテストは seed ごとに違う)。原因は 2 系統で、どちらも**プロセス共有の状態を張り替えて戻していない**もの。(a) `newServer` / `New` が起動時にグローバルを **12 個** 差し替えるが、テストは同じプロセスで何度も呼ぶので、後続の `avatar` / `emoji_redirect` が素の URL ではなく署名付きプロキシURLを受け取る。(b) `frontendutil` の loader キャッシュはプロセスに 1 つで、fixture は `t.TempDir()` に置くため**ディレクトリが消えた後も内容がキャッシュに残る**。
  seed は **全 shard 共通の固定値**にした。`on` (毎回ランダム) は失敗を手元で再現できず、required check が不定期に赤くなる。**shard 番号も使わない** — shard 配属は `NR % 4` なので、テストパッケージが 1 つ増えるだけで既存パッケージの seed が変わり順序が丸ごと入れ替わる (無関係な PR が未実行の順序を引いて赤くなり、ランダム seed と同じ問題を別経路で持ち込む)。
  **seed は実測で選ぶこと。** 覚えやすい値 (issue 番号など) を置くと検出力を持たない値を引く — 実際 `2795` を置いたが、restore を無効化した変異で落ちる seed は 12 個中 7 個だけで、`2795` は落ちない側だった (= 直したバグを CI が検出しない)。採用した `3` は 6 テストが落ちる。
  **cleanup の登録も一覧も、手で書くと変異検証が効かない形になる。** `TestFrontendHTML_SplashColor` (現 `TestFrontendHTML_SplashIgnoresThemeColor`) の `<style>` 抽出を splash 名指しに直した時点で、loader cleanup を全部外しても 40 seed で落ちなくなった。restore の一覧も初版は `entity` の 7 つだけで 5 つ落としていた。どちらも AST の gate で形を強制してある (`internal/server/global_state_test.go`)。
- **2026-08-31**: Section 4 の「DB を使うテストの分離」に、システムカタログを schema で絞る規則と `Scan(&string)` の罠を追記 (#2777)。あわせて `make catalog-check` を新設し `make gates` に入れた (`make help` の target は 112 → 113)。doc だけだと再発する — schema が 17-19 ある条件は残ったままなので。`pg_indexes` を schema 非限定で引くテストが 3 本あり、**required check の `test` を不定期に落としていた** (PR #2778 の `test-shards (1)` が実際に赤くなった)。#2450 で schema を分けた結果、同名テーブルが 17-19 schema に同時に存在し、他パッケージの `ApplyMigrations` が DDL 中だと `could not open relation with OID (SQLSTATE XX000)` になる。**害はそれだけではない** — 絞らないと他 schema の同名 index を自分のものと取り違えるので、migration が適用されていなくても regression guard が緑になる。実測で `internal_repository_ts` の定義が返っており、3 本とも空振りしていた。`Scan(&string)` は複数行でも**最後の 1 行**を黙って取る (GORM は `*string` に対し全行を走査して dest を上書きする) ので、この取り違えは値が正しく見えて気付けない。
- **2026-08-30**: Section 4 の「DB を使うテストの分離」に列枠の話を追記 (#2756)。PostgreSQL は `DROP COLUMN` した列も 1600 の上限に数えるので、実行のたびに列を落とすテスト構造だと手元でだけ枠が減り続け、最後に落ちる (実測で `clip` / `auth_session` / `app` が 1593 列まで到達した)。原因は 2 つで、`ApplyMigrations` が毎回全 migration を流し直すこと (再適用で実際に枠を食うのは migration が作る 120 テーブル中 `note` の 1 つだけ — `000033` が ADD し `000036` が DROP するため) と、TS 形状を作るテストが列を落として戻していたこと。前者は適用済みを skip する台帳、後者は専用の兄弟 schema を一度だけその形に作る方式で解消した。復旧手順も併記。
- **2026-08-10**: Section 4 に「DB を使うテストの分離」を追記 (#2450)。`testutil.OpenTestDB` が呼び出し元パッケージ専用の PostgreSQL schema に接続するようになった。`go test` はパッケージを並行実行し CI の shard は DB を 1 つしか持たないため、共有すると一方の後片付けが他方を壊す (実際に Go を触っていない PR で CI が落ちた)。削除範囲を絞るだけでは解けない (干渉が双方向) 点と、migration の enum guard に `pg_type WHERE typname` を使わない旨も明記。
- **2026-04-28**: `internal/server`のCIカバレッジ閾値を0%例外に追加 (#462)。`avatar.go`/`avatar_test.go`の追加で同パッケージ初の`_test.go`が入り、`router.go`(2000行超のwire層)込みのpackage全体カバレッジが2.5%で計測されてCIが落ちたため。`testutil`/`e2e`と同じく実挙動はe2e/drop-in testで検証する設計に揃える。個別handlerファイルは`_test.go`単体で90%相当をカバーする運用は維持。
- **2026-04-18**: Section 4 / Section 8 のカバレッジ例外閾値を更新 (#260)。`internal/repository` パッケージのテスト拡充でカバレッジを76.4%→99.9%に引き上げて CI 閾値を 90% に戻し、`internal/api/admin` の閾値を 60%→80% に引き上げ(現状83.8%)。CI step "Run all tests with coverage"に`set -o pipefail`を追加してテスト失敗の握り潰し解消も同時に。
- **2026-04-18**: Section 4 / Section 8 に `internal/repository` パッケージのCIカバレッジ閾値を暫定的に 76% に緩和する例外を追加（#260で90%復帰予定）。
- **2026-04-12**: Section 4 にテストカバレッジ目標を追記（最低90% / 推奨95% / 目標100%）。
