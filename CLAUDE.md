# CLAUDE.md

このファイルは、このリポジトリで作業するClaude Code(と、それを使う人)のためのルール集です。

## このプロジェクトについて

**Elythia(旧称mk-go)は、Misskeyとの互換性を保ったまま、独自の機能を育てているGo製のMisskey系サーバーです。** 改名の作業は#3180で進めています。関数名などコード上の識別子、`/api/meta`の`mkGoVersion` / `mkGoCommit`、環境変数の接頭辞`MK_`は、改名の後も据え置きます(例外はnodeinfoの宣言で、`mkGoPlugins`から`elythiaPlugins`に改めた。#3400)。

出発点は、Misskey(TypeScript/NestJS)のバックエンドをGoで書き換えるリライトでした。本家との互換を一通り満たした今は、次の方針で開発しています。

- **ActivityPubの連合の互換性は必ず守る。** 他のサーバーから見て、Misskeyと同じように振る舞う
- **REST APIは、本家のクライアントがそのまま動く互換性を保つ。** 独自の拡張は、フィールドやエンドポイントの**追加だけ**で行う。既存のものの意味を変えない
- **frontendはMisskeyのforkで、独自に手を入れてよい**
- **TS版Misskeyからの移行は保証する。** TS版のDBをそのまま引き継いで起動できるようにする。TS版へ戻せること(復路)は保証しない(#3191)。今どこまで戻れるかは`dropin-e2e`で測っており、戻らなくなったものは[docs/migration-from-ts.md](docs/migration-from-ts.md#戻らなくなったもの)に記録する
- 本家と意図的に違える挙動は、理由と一緒に[docs/divergence.md](docs/divergence.md)(目次。中身は領域ごとに`docs/divergence/`の下)に記録する

読み方:

- **Section 0**の約10項目は必ず守ってください。以降の節は、その詳細と、踏むと壊れる罠です
- 各ルールには理由を1文だけ付けています。経緯や実測値は、リンク先の`docs/`にあります
- 関係するファイルを読むと、`.claude/rules/`から「先にこの`docs/`を読む」という指示が出ます(例: `*_test.go`を読むと`docs/testing.md`)。指示が出たら従ってください。指示はファイルを読んだときにだけ出るので、**新しいファイルを一から作るときは出ないことがあります**。そのときは、Section 4 / 7 / 8 / 9のリンク先を自分で開いてください。それ以外の`docs/`も、必要なときに自分で開いてください
- セクション番号(1〜10)は、コードやdocから「CLAUDE.md Section N」の形で参照されています。**番号を変えないでください**
- 運営者の手元の運用(本番環境など)に固有の話は、gitignore済みの`CLAUDE.local.md`に分けてあります。無くても作業できます

---

## 0. 最初に守ること

1. **コミット・push・PRのマージは、頼まれたときだけ行う。** 作業が終わったら、確認を求めて止まる
2. **作業はissueから始める。** issueの本文は、作成する前に文面を提示して確認を取る(Section 7)
3. **コミットの前に`make check`を通す。** fmt → vet → actionlint → golangci-lint → テスト(`-race`付き)の順で回る
4. **新しいテストは、変異させて落ちることを確かめる。** 直したコードを1行戻しても通るテストは、何も守っていない
5. **DBをモックしない。** DBを使うテストは`testutil.OpenTestDB`を通して、実PostgreSQLの専用schemaで動かす(Section 4)
6. **生成物を手で直さない。** 例: `docs/api-compat.md`、`go.work`
7. **`go mod tidy`を使わない。** 依存の追加は`go get`で行い、`GOWORK=off go build ./...`で充足を確かめる
8. **`name:`の無いcompose(`docker-compose.yml`など)は、ディレクトリ名がproject名になることに注意する。** 同じ名前のprojectが既に動いているホストで起動すると、そのコンテナに合流して作り直してしまう。検証用のcomposeには、必ず`name:`で専用の名前を付ける
9. **公開される場所に、要らない情報を書かない。** セッションURL、第三者のホスト、未修正の脆弱性の詳細が該当する(Section 7)
10. **数値・識別子・パスは、実行かgrepで確かめてから書く。** 推論で書いた数字は、高い確率で間違っている

---

## 1. 技術スタック

| 用途 | ライブラリ |
|---|---|
| 言語 | Go 1.27(`go.mod`で管理) |
| Web | Echo v4 |
| ORM | GORM + pgx/v5(PostgreSQL 18) |
| Migration | golang-migrate(SQLファイル) |
| 設定 | Viper(Misskey互換YAML + `MK_`環境変数) |
| ログ | slog |
| Redis | go-redis v9 |
| ジョブキュー | mkq(BullMQとwire互換) |
| 検索 | meilisearch-go |
| ストレージ | aws-sdk-go-v2/s3 |
| HTTP署名 / LD署名 | 自前実装(`internal/activitypub/`)+ `piprate/json-gold` |
| 認証 | bcrypt / pquerna/otp / go-webauthn |
| テスト | testing + testify + testcontainers-go(Redis用) |

## 2. 構成

```
cmd/elythia/    実行バイナリ`elythia`。`serve` / `migrate` / `backfill <名前>`などのサブコマンドで呼び分ける
internal/       本体。依存の向きは api → core → repository → model
  api/          APIハンドラ(エンドポイント単位のサブディレクトリ)
  core/         ビジネスロジック
  repository/   データアクセス
  model/        DBモデル
  entity/       レスポンス用DTO(ドメインロジックを入れない)
  activitypub/  連合(Inbox / Deliver / Renderer / Resolver / 署名)
  cli/          `elythia`のサブコマンドの処理(`cmd/elythia`はこれを呼ぶだけ)
  queue/ stream/ server/ config/ testutil/ ほか
plugin/         プラグインがimportする公開パッケージ
plugins/        プラグイン本体(gitignore済み。同梱するものだけ例外)
migration/      NNNNNN_name.up.sql / .down.sql
tests/          Goのe2e(`tests/e2e` / `tests/e2e-federation`)と、Go以外の検証基盤
frontend/       本家Misskeyから取り込んだfrontend(pnpm workspace、#3379)
tools/          parityゲートとコード生成のCLI
docs/           ドキュメント
```

- **依存は`api → core → repository → model`の向きだけにする。** 逆向きのimportは入れない
- `activitypub`は`core`から呼ぶ
- 全ディレクトリの説明は[docs/architecture.md](docs/architecture.md)にある

## 3. よく使うコマンド

```bash
make build / make dev        # ビルド / go runで起動
make fmt                     # gofmt -s -w(go env GOROOTのgofmtを使う)
make check                   # コミット前に必須
make test                    # CIと同じ条件(-race -count=1 -shuffle=3)
make test-fast               # -race抜き。反復用で、コミット前の検査ではない
make gates                   # 静的なparityゲートを一括(サーバー・Docker不要)
make plugin-test             # 同梱プラグインのテスト(別moduleなので./...に入らない)
make migrate-up / migrate-down   # downは本体の系列を1段だけ戻す(forkの系列はmake migrate-down-local、docs/fork-migrations.md)
make frontend-check          # frontend/の型チェック + frontendを読むゲート + 絵文字の正規表現 + eslint
make frontend-lint / frontend-test   # frontend/のeslint / vitest
```

- 全targetは`make help`で見られる。説明は[docs/development.md](docs/development.md)にある
- **`make tidy`は使わない。** `plugins/`にプラグインを置いた環境では、生成される`cmd/elythia/plugins_generated.go`がプラグインのmoduleをimportするので失敗する。置いていない環境でも、CIと結果がずれる
- **Goの版を上げたら、`make plugins`で`go.work`を作り直す。** `go.work`は生成物で、作り直さないと古い版のtoolchainが選ばれ、ビルドが`requires go >= ...`で落ちる
- **`make uds-*`と`make docker-*`は運営者の環境向け。** 手元の検証には使わない

## 4. テスト

### ルール

- **新しい機能にはテストを付ける。** CIはパッケージごとにカバレッジ90%を要求する(下回るとマージできない)。通常のPRでは95%を、新しいパッケージや小さなパッケージでは100%を目指す
- 例外は`internal/api/admin`(80%)と、`internal/testutil` / `internal/server` / e2e(対象外)。理由は[docs/development.md](docs/development.md)にある
- テストは、対象と同じパッケージの`_test.go`に置く
- **単体テストは`internal/testutil`のモックを使う。統合テストは実DBを使う**

### DBを使うテスト

`testutil.OpenTestDB` / `MustOpenTestDB`は、**呼び出し元のパッケージ専用のPostgreSQL schema**に接続します。`go test`はパッケージを並行に実行し、CIのshardはDBを1つしか持たないためです。

- **DBを読み書きするテストで`OpenSharedTestDB`を使わない。** 接続処理そのものを試すテスト専用
- **`public.`のように、`search_path`をまたぐ生SQLを書かない**
- **システムカタログは自分のschemaに絞る。** `pg_indexes`は`schemaname = current_schema()`、`information_schema`は`table_schema = current_schema()`で絞る。絞らないと、他のschemaの同名オブジェクトを取り違えるか、DDL中のエラーで落ちる
- **複数行が返りうるクエリを`Scan(&string)`で受けない。** GORMは最後の1行を黙って返す
- **`db.Create(x)`の戻り値は必ず検査する。** FK違反が黙って流れる
- **列を落とすテストを書かない。** 落とした列もPostgreSQLの1600列の上限に数えられる。形を変えたいときは`testutil.OpenTestDBSchema("<suffix>")`を使う
- migrationでenumを作るときは`EXCEPTION WHEN duplicate_object`を使う

### テストの書き方

- **プロセス全体で共有される状態(パッケージ変数、キャッシュなど)を張り替えたら、`t.Cleanup`で戻す。** CIは`-shuffle`で順序を変えるので、戻さないと後に走る別のテストが落ちる。`internal/server`の`newServer` / `New`はグローバルを多数差し替えており、戻すべきものの一覧は`TestProcessGlobalsAreRestored`にある

### ゲートを書くとき

`make gates`のような静的な検査を足すときの原則です。どれも、実際に「検査していないのに緑」を出した経験から来ています。個々のゲートの経緯は[docs/gates.md](docs/gates.md)にあります。

- **何も拾えなかったら落とす。** 違反が0件なのが正常な検査は、書式が変わって抽出が空振りしても緑のままになる。抽出する側に下限(実在する対象を名指しで要求する形)を置く
- **allowlistには、使われていない項目を落とす検査を付ける。** 理由の欄には「なぜ許すか」ではなく「どの経路で実際に出るか」を書く
- **自前でパースしない。** Makefileは`make -n`に、`.dockerignore`はDocker本体の実装に解かせる。近似は必ず取りこぼす
- **ファイルは`git ls-files`で列挙する。** ディスクを走査すると、`git add`を忘れた新しいファイルが手元でだけ見つかり、CIで初めて落ちる
- **名前で見る走査は、名前を変えるだけで避けられる。** ゲートが確実に落とすのは「普通に書いたときに踏む形」にし、意図的な回避は振る舞いのテストで受ける
- **足したら変異させて、落ちることを確かめる。** テスト自身を壊して落ちるのは、検出したとは言わない

### 手元の準備

- PostgreSQLを用意する。既定は`localhost:5432`の`misskey_test`で、ユーザーとパスワードはどちらも`mk`。接続先を変えるときは`.env.test`を置く
- Redisはtestcontainersが立てるので、Dockerが要る
- 詳細と、schemaが壊れたときの復旧手順は[docs/testing.md](docs/testing.md)にある

## 5. コーディングスタイル

- `gofmt -s`と`go vet`を通す(どちらもCIで強制)
- 命名はGoの慣習に従う(`URL` / `ID` / `API`は全て大文字)
- early returnでネストを浅くする
- エラーは`fmt.Errorf("context: %w", err)`で包む
- **`//nolint`は、対象の行の行末に置く。** 独立した行に置くと、続くブロック全体が検査されなくなる。理由も書く

### frontend(`frontend/`)

`frontend/`は本家Misskeyのfrontendを取り込んだpnpm workspaceで、本体のコミットとして直接直す(#3379)。詳細は[docs/contributing.md](docs/contributing.md#fork-frontend-frontend-を触るとき)にある。

- **`frontend/`を触ったら、`make frontend-check` / `make frontend-lint` / `make frontend-test`も通す。** `make check`はGoしか見ない。CIではrequiredの`frontend`が見る
- **新しいファイルにも本家と同じSPDXヘッダーを付ける。** `frontend/scripts/check-spdx.mjs`が落とす。Go側の「SPDXを付けない」方針とは逆
- **本番のチェックアウトで`pnpm build` / `pnpm -r build` / `make e2e-frontend-build`を検証目的に流さない。** `frontend/built`を消してから作り直すので、そこをbind mountしている本番が404になる。検証は別のworktreeで行う
- 生成物の`server-plugins.generated.ts`は追跡していない。無ければ`make plugins`で作る

### コメントとドキュメントの言語

| 種類 | 言語 |
|---|---|
| GoDoc、テストケースの`name`などコード内のメタ情報 | 英語 |
| 実装の背景・理由を説明するインラインコメント | 日本語 |
| issue / PR のタイトルと本文 | 日本語(識別子やコマンドは原文のまま) |

- 自明な処理にはコメントを書かない。`// TODO`や`// XXX`を乱用しない
- 絵文字は使わない
- **issue / PRの見出しと本文の地の文に、英語を混ぜない。** 例外は、Section 7のPR本文の見出し「Summary」と「Closes」だけ
- 日本語の中に不要な半角スペースを入れない(○「Claude Code入門」 ×「Claude Code 入門」)

## 6. 互換性と規約

- **レスポンスのフィールド名・型・エラーコード・エラーIDは、本家と一致させる。** 独自の拡張は追加だけにする(冒頭の方針)
- 版は`internal/config/config.go`の`MisskeyVersion` / `MkGoVersion`で管理する
- User-Agentは`Elythia/<version> (<url>)`の形にする(`internal/config.UserAgentProduct`。#3394より前は`mk-go`)
- IDは`internal/misc/id/`のジェネレータで作る(既定は`aidx`)。モデルから直接`uuid`を呼ばない
- 内部エラーは`slog`で記録し、利用者には汎用のメッセージを返す
- Redisは用途ごとに別のクライアントとして扱う(`default` / `pubsub` / `jobQueue` / `timelines` / `reactions`)。接続先が同じでも分ける
- 外向きのActivityPubリクエストには、必ずHTTP署名を付ける
- リモートオブジェクトの取得は`internal/activitypub/resolver.go`を通す
- `allowedPrivateNetworks`を尊重し、プライベートIPへの直接アクセスを防ぐ

## 7. Gitと公開物

### issueとPRの進め方

1. **issueを作る。** 本文はPREP法(結論 → 理由 → 実測値の具体例 → 完了条件)で書く。見出しは「背景・目的 / 実装する内容 / 影響範囲 / 完了条件 / 関連」。**作成する前に文面を提示して確認を取る**
2. **`develop`からブランチを切る。** 名前は`feature/<issue番号>-<要約>`か`fix/<issue番号>-<要約>`にする
3. **実装する**
4. **`develop`へ向けてPRを作る**(`main`はリリース用で、PRを向けない)。 本文は「Summary / 主な変更点 / テスト / Closes / その他」の見出しで書く。テストには、通ったテスト・足したテスト・実行方法を書く。issueを閉じるPRは`Closes #<番号>`を入れる。閉じないPR(段階の途中など)は、見出しを「関連」にして番号を書く。マージは運営者が行う

- **issueのタイトルは、何が起きているか(バグ)か、何をするか(機能)を1文で書く。** 番号や接頭辞は付けない
  - バグの例: `リモート絵文字をその場からインポートすると、ライセンスが空で上書きされる`
  - 機能の例: `バブルゲームに 1:1 の対戦を足す`
- **大きな作業は、親issueと段階ごとのサブissueに分ける。** サブissueのタイトルには`(1)` `(2)`のように段階を付ける(例: `バブルゲームの対戦 (1): エンジンにおじゃま石と攻撃の計算を足す`)
- PRのタイトルは、issueのタイトルか作業の要約にする
- 1 issue = 1 PRにする。機能追加と無関係なリファクタを混ぜない
- issueとPRの操作には`gh`コマンドを使う

### コミット

- **各コミットが単体でビルドとテストを通すこと。** 機能とバグ修正のPRは、コミットがそのまま`develop`の履歴に載る。壊れたコミットが残ると`git bisect`が効かなくなる。確認は使い捨ての`git worktree`で行う(`git stash`は、保留中の別作業を巻き込む)
- 依存するAPIの追加を先に、それを使う配線を後に並べる
- 機能追加と無関係なリファクタを、1つのコミットに混ぜない
- **メッセージは`<種類> <対象>: <要約> (#issue番号)`の形にする。** `<対象>`はパッケージや領域の名前(`federation` / `drive` / `admin` / `frontend`など)
  - 種類は`Fix`(バグ修正)、`Feat`(機能追加)、`Test`(テストだけ)、`Docs`(ドキュメントだけ)、`Refactor`(挙動を変えない整理)、`Bump`(依存やfrontendの版の更新)、`Chore`(その他)のどれか
  - 例: `Fix admin: リモート絵文字のインポートでライセンスを空で上書きしない (#3246)`
  - `Fix:`や`Feature`のように、コロンの位置や綴りを変えない
- `CHANGELOG.md`はPRごとには書かない(リリースのときに運営者がまとめて書く)
- `push --force`や`reset --hard`は、指示が無い限り使わない

### 公開物に書かないもの

- **セッションURL**(`https://claude.ai/code/session_...`)。commit、PR、issue、どこにも書かない。`Co-Authored-By: Claude`と「Generated with Claude Code」のクレジット行は残してよい
- **第三者のホスト名・インスタンス名・acct・外部サービスのアカウント識別子。** 例示には`remote.example`のような仮の値を使う。**issueやPRは編集しても初版が履歴に残る**ので、書く前に確かめる
- **セキュリティ修正の詳細。** セキュリティ修正ではissueを立てず、PRだけを出す。PR本文にも再現手順・対象ファイル・影響範囲を書かない。コミットメッセージには「何が足りなかったか」までは書いてよいが、ペイロードは書かない

### ドキュメントを直すとき

docを直すと、直した先で新しい誤りを作りやすくなります。詳細は[docs/contributing.md](docs/contributing.md#ドキュメントを直すときのレビュー条件)にあります。

- **直した語で`git grep`し、同じ主張が他の場所に残っていないか探す**
- **数を書くなら、数え方も書く**
- **wire上の名前と、ソースのファイル名を区別する。** 例: streamのチャンネル名を、ファイル名から作らない
- **直した結果が、元より危険な方向になっていないかを見る。** 読んだ人が何をするかで比べる
- **「本家と同じ」と書く前に、`docs/divergence.md`とコードコメントの既知の乖離を確かめる**

## 8. CI

**required check は`build` / `test` / `lint` / `frontend`の4つです。** 手元で`make check`を通せば、ほぼ再現できます。`frontend/`を触ったときは、Section 5の`make frontend-*`も通します。

| workflow / job | 発火 | required | 見ているもの |
|---|---|---|---|
| `ci.yml` build | push / PR | ○ | `go build ./...`、同梱プラグインの`disabled: true`、同梱プラグインの`go vet`、同梱プラグインを含めた統合バイナリのビルド |
| `ci.yml` test | push / PR | ○ | 4 shardで`-race -count=1 -shuffle=3`、パッケージごとのカバレッジ閾値 |
| `ci.yml` lint | push / PR | ○ | vet / gofmt / actionlint / golangci-lint / テストfixtureのID重複 |
| `ci.yml` plugin-tests | push / PR | | 同梱プラグインのテスト、`authoring.md`のスニペットのコンパイル |
| `frontend` | push / PR | ○ | `frontend/`の検査(10 workspaceのeslint、typecheck、SPDX、locale、本番ビルド、vitest)、絵文字の正規表現。frontendに関係しない変更ではlintとtestをskipし、集約jobの`frontend`だけが成功する |
| `ci.yml` vulncheck | push / PR | | govulncheck、`go.mod`とDockerfileのGoの版の一致 |
| `dependency-review` | PR | | PRが持ち込む依存の既知脆弱性 |
| `codeql` | PR / push / 週1回 | | Goとworkflowの静的解析 |
| `dropin-e2e` | PR(paths限定) | | TS→mkの切替(往路)、TSへ戻す復路の測定、実Misskey / Mastodonとの連合など5シナリオ |
| `playwright` | PR(paths限定) | | ブラウザのe2e(4 shard) |
| `upstream-backend-e2e` | PR(paths限定) | | 本家のbackend e2eを無改変で実行(4 shard) |
| `diff-e2e` | PR(paths限定) | | TSとの値レベルの差分 |
| `apicompat` | PR(paths限定) | | `docs/api-compat.md`が実態と一致しているか、goldenが本家の版に追いついているか(`make upstream-check`) |
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

## 9. 設定と環境変数

- 設定ファイルの既定は`.config/default.yml`(gitignore済み)。最初は`.config/default.yml.example`を複製する
- `MK_`で始まる環境変数で設定を上書きできる。ネストしたキーは`_`でつなぐ(例: `MK_DB_HOST`)
- **`MK_*`は設定ファイルより優先される。** exportしたまま`internal/config`のテストを回すと落ちる
- **設定ファイルにも`bindEnvKeys()`にも無いキーは、`MK_`では作れない。** exampleでコメントアウトされている`meilisearch:`などは、まずyml側のコメントを外す
- `elythia migrate`は`DATABASE_URL`を読まない。`-config`か`MK_DB_*`で接続先を決める
- 全キーの一覧は`internal/config/config.go`の`bindEnvKeys()`にある。運用向けの説明は[docs/configuration.md](docs/configuration.md)

## 10. 開発方針

- **本番環境に影響する変更は、事前にユーザーに確認する**
- **設計を変えるときは、先にissueで背景と変更内容を記録する。** 実装中に設計の問題に気付いたら、止まってユーザーに確認する
- 本家の実装は参考にするが、TypeScriptのパターンをそのままGoに訳さない
- migrationのdownは必ず書く。データが失われるならコメントに書く
- テーブルや列の削除は、複数のリリースに分けて段階的に移行することを検討する
- ライブラリの使い方は、推測せず最新のドキュメントで確かめる(Context7 MCPが使えるならそれを使う)

---

## 更新記録

このファイル自体を変えたときだけ、1行で追記します(新しいものを上に)。経緯の本文はリンク先にあります。個別のfixの履歴は`CHANGELOG.md`にあります。

- 2026-10-08: forkのmigrationの系列(`migration/local/`)を足したので、Section 3の`migrate-down`の注釈を更新した (#3428) → [docs/fork-migrations.md](docs/fork-migrations.md)
- 2026-10-07: frontendに`elythia-js`のworkspaceを足したので、Section 8の`frontend`の行のeslintの対象を10 workspaceにした (#3418) → [docs/contributing.md](docs/contributing.md#elythia-独自の-api-の型-frontendpackageselythia-js)
- 2026-10-07: `docs/divergence.md`を目次にし、中身を`docs/divergence/`の領域ごとのファイルへ分けたので、冒頭の方針の案内を更新した (#3414) → [docs/divergence.md](docs/divergence.md)
- 2026-10-06: nodeinfoの宣言を`elythiaPlugins`にしたので、冒頭の据え置く識別子の例外を更新した (#3400) → [docs/plugin-peer-protocol.md](docs/plugin-peer-protocol.md)
- 2026-10-06: 本文の名前をElythiaにし、冒頭に旧称と据え置く識別子を書いた (#3394) → [docs/design/project-restructure.md](docs/design/project-restructure.md)
- 2026-10-06: User-Agentを`Elythia/<version> (<url>)`にしたので、Section 6を更新した (#3394) → [docs/divergence.md](docs/divergence.md)
- 2026-10-05: 実行バイナリを`elythia`1つにまとめたので、Section 2の構成、Section 3の`make tidy`の行、Section 9の`migrate`の行を更新した (#3394) → [docs/design/project-restructure.md](docs/design/project-restructure.md)
- 2026-10-05: submodule(`third_party/misskey`)を外したので、Section 2の構成とSection 8の`build`の行を更新した (#3379) → [docs/divergence/frontend.md](docs/divergence/frontend.md#4-2b-frontend-の独自変更-3379-で取り込んだ後)
- 2026-10-05: `frontend`をrequired checkにし、`ci.yml`の`frontend-check` jobをそこへまとめた。Section 3 / 5 / 8に反映し、Section 5にfrontendの節と`.claude/rules/frontend.md`を足した (#3379) → [docs/ci.md](docs/ci.md)
- 2026-10-05: frontendを`frontend/`から読むようにしたので、Section 2の構成とSection 8の`frontend-check`の行を更新し、`frontend`の行を足した (#3379) → [docs/deployment.md](docs/deployment.md#frontend-を本体へ取り込んだ版へ上げる-3379)
- 2026-10-05: 比較対象の本家を`.cache/misskey`から読むようにしたので、Section 8の`apicompat`の行を更新した (#3378) → [docs/ci.md](docs/ci.md)
- 2026-10-04: Goのe2eを`tests/`へ移したのでSection 2の構成を更新した (#3373) → [docs/design/project-restructure.md](docs/design/project-restructure.md)
- 2026-10-04: 復路の保証をやめたことを冒頭の方針とSection 8に反映した (#3191) → [docs/dropin-e2e.md](docs/dropin-e2e.md#復路は測る対象-3191)
- 2026-10-01: 他の人のClaudeが読むことを前提に作り直した。更新記録とSection 8の本文をdocsへ移し、運営者の運用を`CLAUDE.local.md`へ、docsの取り込みを`.claude/rules/`へ分けた (#3248)
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
