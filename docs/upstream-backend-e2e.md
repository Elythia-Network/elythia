# 本家 backend e2e を mk-go に向けて回す

Misskey 本家 (`make upstream-fetch` が取得する `.cache/misskey/<版>/`、#3378) の backend e2e (`packages/backend/test/e2e/**`) を、
そのまま mk-go に対して実行するハーネス。API 互換性の regression を検出する。

**テスト本体には一切手を入れない。** 差し替えるのは vitest 設定の 2 点だけで、
上流でテストが増えれば自動的に検証対象も増える。

## 構成

| ファイル | 役割 |
|---|---|
| `tests/upstream-e2e/harness/vitest.config.e2e.mkgo.ts` | `globalSetup` と `setupFiles` を差し替えた vitest 設定 |
| `tests/upstream-e2e/harness/test-server-mkgo/entry.ts` | NestJS アプリの代わりに mk-go バイナリを子プロセスとして起動する。`MKGO_BIN` / `MKGO_CONFIG` / `MKGO_CWD` が無ければ落ちる |
| `tests/upstream-e2e/harness/test/setup.e2e.mkgo.ts` | 各ファイルの前に `/api/reset-db` を叩く。既知乖離の expected-failure もここで立てる (一覧は `MKGO_CWD` から読む) |
| `tests/upstream-e2e/compose.yml` | ローカル実行用の PostgreSQL / Redis |
| `tests/upstream-e2e/mkgo.yml` | 本家 `.github/misskey/test.yml` の mk-go 版 |
| `tests/upstream-e2e/known-divergences.json` | 『通らないことが正しい』テストの一覧 |

ポートは本家 `.github/misskey/test.yml` に合わせてある (PostgreSQL 54312 /
Redis 56312 / mk-go 61812)。おかげでローカルの compose と CI の service
コンテナで同じ設定ファイルを使い回せる。

本家 `setup.e2e.ts` は `initTestDb(false)` で TypeORM のエンティティ定義から
スキーマを作り直すが、それをやると mk-go の migration にしか無いテーブル
(`relay_observed_user` 等) が消える。代わりに mk-go の `/api/reset-db` で全
テーブルを truncate している (`schema_migrations` は保護される)。

## 実行

```bash
# 初回: 本家を取得し (make upstream-fetch)、本家側の依存を用意する
# (UPSTREAM_MISSKEY_VERSION を上げたときも再実行)
make upstream-e2e-deps

# DB/Redis を起動してマイグレーションを適用
make upstream-e2e-up
make upstream-e2e-migrate

# 全ファイル実行 (実測 14 分ほど)
make upstream-e2e-test

# 1 ファイルだけ
make upstream-e2e-test FILE=test/e2e/note.ts

# 依存の用意からテストまで一括
make upstream-e2e

# 撤去 (volume ごと)
make upstream-e2e-down
```

`make upstream-e2e-deps` がやることは 4 つ。いずれも毎回は要らないので
`upstream-e2e-test` からは切り離してある。

- `pnpm install --frozen-lockfile`
- `pnpm build-pre` — `loadConfig()` が `built/meta.json` を `readFileSync` する
  (frontend の manifest は `existsSync` 判定なので無くてよい)
- `pnpm --filter misskey-js build` — `misskey-js` は exports が `built/` を指すので、
  ビルドしないと `test/e2e/**` の `import ... from 'misskey-js/entities.js'` が解決できない。
  frontend まで含む `pnpm build` (5-10 分) は e2e には不要なので呼ばない
- `.config/test.yml` の設置と `NODE_ENV=test pnpm --filter backend compile-config` —
  本家の `loadConfig()` は YAML ではなく `built/.config.json` を読む (ランタイムに
  js-yaml を含めないため)。生成しておかないと globalSetup が
  「Compiled configuration file not found」で落ちる

`make upstream-e2e-test` は `built/elythia` を先にビルドする。テスト側の
ハーネスがそのバイナリを子プロセスとして起動するので、mk-go を手で立ち上げて
おく必要はない。**むしろ手動で 61812 を掴んでいると、ハーネスの再起動
(`/env` 経由) が `address already in use` で失敗し、環境変数を変えたはずの
テストが古いプロセスに当たって誤った結果になる。**

## 除外しているファイル

`vitest.config.e2e.mkgo.ts` の `exclude` で 4 ファイルを外している。いずれも
**本家の TypeScript 実装を同じ DB に対して起動してしまう**もので、TypeORM が
スキーマを張り直して mk-go の migration にしか無いテーブル / カラムを消すため、
以降 mk-go が起動できず後続の全ファイルが道連れになる。

- `move.ts` — `initTestDb(false)` でスキーマを作り直す
- `exports.ts` / `synalio/abuse-report.ts` / `synalio/user-create.ts` — `startJobQueue()` で本家のジョブキューをプロセス内に起動する

## 既知の乖離 (expected-failure)

mk-go では『通らないことが正しい』テストがある。mk-go 独自の role policy、
意図的に採用していない上流の検索順序、ジョブワーカーを同一プロセスで動かす
ことによるハーネスの構造差など。

それらは `tests/upstream-e2e/known-divergences.json` に**根拠付きで**登録し、
vitest の expected-failure (`task.fails`) として実行する。

- 落ち続ける限り pass (= 既知の乖離、CI は緑のまま)
- 通るようになったら fail (= 乖離が解消したので一覧から外す合図)

`skip` ではないのが要点で、放置してリストが陳腐化することがない。

**載せてよいのは根拠を書ける乖離だけ。** 原因が分かっていない失敗を載せると、
本物のバグを永久に隠すことになる。

entry は `file` / `reason` / `names` の 3 キー。**`names` は配列**で、describe を
` > ` で連ねたフルネームを並べる (ファイル名は含まない)。`test.each` の展開名も
そのまま書く。同名テストが複数あるときは全てが expected-failure になる。

照合は `file` 単位なので、同じ理由の乖離でもファイルが違えば entry を分ける。

```json
{
  "file": "test/e2e/users.ts",
  "reason": "policies に mk-go 独自の分割アップロード用キーが乗るため (#2313)",
  "names": ["ユーザー > が作れる。（作りたての状態で自分のユーザー情報が取れる）"]
}
```

## CI

`.github/workflows/upstream-backend-e2e.yml` が PR トリガー (paths フィルタ
付き) と `workflow_dispatch` で回す。**branch protection の required checks に
は入れない**ので、落ちても merge はブロックされない。1200 件超のテストに flaky
要素があるため、merge ブロッカーには適さない。

**4 シャード並列** (`--shard=i/4`、`fail-fast: false`)。check 名は `e2e (1/4)` 〜
`4/4` で、実測 3-7 min。**プロセス内では並列にできない** — upstream の vitest 設定が
`maxWorkers: 1` で、かつ setupFiles がファイルごとに mk-go の `/api/reset-db`
(全テーブル truncate) を叩くため、同じ DB に 2 ファイルを並行させると片方が相手の
フィクスチャを実行中に消す。job を分ければ PostgreSQL / Redis の service container も
別に立つ (#2609)。

非ブロッキングを `continue-on-error: true` で実現しないこと。あれは job を
成功扱いにするので失敗が完全に不可視になる。job は正しく失敗させ、required に
入れないことで非ブロッキングにする。

失敗時は mk-go のログが `upstream-e2e-mkgo-log-<shard>` artifact として 14 日残る。
vitest の出力だけでは「サーバーが何を返したか」が分からないことが多い。
