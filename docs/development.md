# 開発ガイド

## 開発環境のセットアップ

### devcontainer (推奨)

VS Codeの[Dev Containers](https://code.visualstudio.com/docs/devcontainers/containers)拡張をインストールして開く。

`.devcontainer/`の構成:
- Go 1.27 + PostgreSQL + Redis (network_mode: host)
- golang-migrate がプリインストール。Node.js / pnpm は `postCreate.sh` が submodule の `.node-version` / `packageManager` を読んで入れる (#2921。image に入るのは bootstrap 用の Node だけ)
- `postCreate.sh`で初期化

`postCreate.sh` が `.config/default.yml` を example から複製し (`.config/*` は gitignore なので clone 直後は存在せず、無いと `failed to load config` で落ちる)、migration まで流す。`TEST_DB_*` は compose が渡すので `.env.test` は要らない。

**`url` は example の `https://example.tld/` のまま残るので、開いたら手で書き換えること。** `mediaProxy` の既定は `url` から組み立てられるため、放置するとリモートの avatar / emoji / 添付が `https://example.tld/proxy?...` になって全部読めない。`MK_URL` で渡す方法は使えない (`MK_*` は viper で設定ファイルより優先されるので `internal/config` のテストが落ちる)。

なお **devcontainer では `make test` で `internal/config` が 2 件落ちる**。compose が渡す `MK_DB_USER` / `MK_DB_PASS` が `TestLoad_DatabaseConfig` を、`MK_REDIS_HOST` が `TestLoad_RedisForPubsub` を、それぞれ fixture より優先して読むため。既知の問題。

```bash
# VS Code で開いたら
make dev
```

### ローカル環境

前提条件:
- Go 1.27+
- PostgreSQL 18推奨 (16以降で動作、CI検証は18)
- Redis 7+
- Docker (テストで Redis を要する箇所が testcontainers を使う)

**テストを回すには PostgreSQL を自分で用意する。** Redis は testcontainers が立てるが、DB を使うテストの大半は外部の PostgreSQL に直接つなぐ。既定の接続先とロール / DB の作り方は [testing.md](testing.md) を参照。

```bash
git clone --recursive https://github.com/shiroha-a/mk.git
cd mk

# 設定ファイルを作成
cp .config/default.yml.example .config/default.yml
# default.yml を編集してDB/Redis接続先を設定

# テスト用 PostgreSQL の接続先が既定 (localhost:5432 / misskey_test / mk) と
# 違うときだけ複製して編集する (→ testing.md)
# cp .env.test.example .env.test

# マイグレーション適用 (接続先は上で編集した default.yml から読む)
make migrate-up

# 起動
make dev
```

## Makefileターゲット

引数なしの `make` (= `make help`) で全ターゲットの一覧が出る。以下はグループごとの説明と、詳細ドキュメントへの入口。

### まとめて実行

| ターゲット | 内容 |
|---|---|
| `make check` | `fmt` → `lint` → `actionlint` → `golangci-lint` → `test`。コミット前に必須 |
| `make gates` | 静的 parity ゲートを一括実行 (内訳は下の「静的 parity ゲート」表) |
| `make version` | mk-go / 互換 Misskey / submodule のバージョンを表示 |
| `make frontend-check` | 同梱フロントエンドの型チェック (`vue-tsc --noEmit`)、**submodule のソースを読むゲート**、**eslint** (#2906)。ビルド成果物を作らないので安全。ゲートを `make gates` に入れないのは、あちらが submodule 無しで回る前提で、混ぜると checkout していない環境で skip され「検査していないのに緑」になるため (#2892)。**vitest は入っていない** (`make frontend-test`) — CI の同名 job はそれと `make plugins-all` / 統合バイナリの build を別 step で走らせる |
| `make diff-check` | 差分比較ハーネスを作り直して実行 (クリーン DB 前提のため) |
| `make playwright-check` | Playwright を作り直して実行 (同上) |
| `make e2e-down-all` | 検証用スタックを一括撤去。**本番 project `mk` は対象外** |

### pull して起動 (ビルド不要)

| ターゲット | 内容 |
|---|---|
| `make image-up` / `image-down` / `image-down-v` / `image-logs` | フロントエンド同梱の `bundled` イメージを pull して起動する (`docker-compose.image.yml`) |
| `make image-build` | `bundled` イメージを手元でビルドする (publish 前の確認用) |

既存の `docker-compose.yml` / `make docker-*` (ソースからビルド) はそのまま使える。置き換えではなく並立する選択肢。

動かすだけならソースを clone する必要すら無い。compose と設定のひな形だけを置いた
[`docker` ブランチ](https://github.com/shiroha-a/mk/tree/docker) が GitHub Actions で
自動生成されている (`.github/workflows/docker-branch.yml`、生成元は
`docker-compose.image.yml` / `.config/docker.yml.example` / `deploy/README.md`)。

```bash
git clone --depth 1 -b docker https://github.com/shiroha-a/mk.git mk
cd mk && docker compose up -d
```

**`docker` ブランチは手で編集しない。** push のたびに履歴ごと作り直されるので、
変更したい場合は生成元を編集する。

### 更新 (運用)

| ターゲット | 内容 |
|---|---|
| `make update` | `git pull --recurse-submodules` して、フロントエンド再ビルドの要否を知らせる |
| `make pull-plugins` | `plugins/` 配下の独立リポジトリを pull |
| `make pull` | `update` + `pull-plugins` (本体・submodule・プラグインを一括) |
| `make docker-rebuild` | フロントエンド + イメージをビルド (Docker Compose 構成) |
| `make docker-restart` | `app` を再起動して配信エントリを検証 (Docker Compose 構成) |
| `make docker-update` | pull → ビルド → 再起動 → 検証 (Docker Compose 構成) |
| `make uds-rebuild` `make uds-restart` `make uds-update` | 同上 (UDS 本番構成) |

`docker-update` / `uds-update` は**フロントエンドの再ビルドと再起動を必ずセットで実行する**。mk-go はエントリポイントを起動時に 1 回だけ解決してキャッシュするため、ビルドだけして再起動しないと HTML が消えた古い `scripts/<hash>.js` を指したまま 404 になる。

**`up -d` では再起動されない。** compose はイメージと設定が変わらなければコンテナを作り直さないが、フロントエンドは bind-mount なので、フロントエンドだけ更新したときは何も変わらない。そのため `*-restart` は `restart` を明示したうえで、[`deploy/check-frontend-entry.sh`](../deploy/check-frontend-entry.sh) で**配信中のエントリが実在するか**まで確かめ、404 なら非ゼロで落ちる (#2885。2026-09-07 に本番で実際に踏んだ)。

**CSS だけ見ても分からない。** vite のファイル名は内容ハッシュなので、内容が変わらないファイルは再ビルドしても同じ名前で作り直され、200 を返し続ける (`build.ts` は毎回 `built/_frontend_vite_` を消してから作るので、「古いファイルが残る」わけではない)。逆に CSS だけ内容が変われば CSS の名前だけが変わるので、スクリプトはエントリの JS と stylesheet の両方を見る。エントリは `CLIENT_ENTRY` から取り、index の loader と同じく言語ごとのパスへ振り替えて確かめる。

**公開 URL は CDN の裏にいることがある。** 素で叩くと、まさに検出したい状況 (直前まで配信していた古いアセットがエッジに残っている) でキャッシュヒットの 200 が返る。スクリプトは使い捨てのクエリを付けて origin まで通す。

`pull-plugins` は `plugins/*/` のうち `.git` を持つものだけを `git pull --ff-only` する。同梱プラグイン (`status` / `trustlevel`) は mk 本体に含まれるので本体の pull で追従する。未コミットの変更があるリポジトリは名前を出して skip する (勝手に stash しない)。

手順の詳細は[デプロイ](deployment.md#アップデート)を参照。

### ビルド・実行

| ターゲット | 内容 |
|---|---|
| `make build` | `./built/misskey`にバイナリ生成 |
| `make dev` | `go run`で直接起動。**ビルド済みフロント (`third_party/misskey/built/_frontend_vite_`、`MISSKEY_FRONTEND_DIR` で上書き可) が無ければ `MK_DEV=1` を立てて**、`/vite/*` を Vite dev server (`localhost:5173`) へ流す。mk-go は dev モード (`dev: true` / `MK_DEV=1`) でしか dev server へ proxy せず、それ以外でビルド出力が無いと `/vite/*` は 404 になる — 以前は「無ければ proxy」だったので、本番でビルド出力が欠けると認証なしで `localhost:5173` へ reverse proxy されていた。ビルド済みでも dev server を使いたいときは `MK_DEV=1 make dev` |
| `make run` | build + 実行 |
| `make clean` | ビルド成果物を削除 |
| `make tidy` | `go mod tidy`。**このリポジトリでは private plugin の解決に失敗するので使えない**。依存追加は `go get`、`go.sum` の充足検証は **`GOWORK=off go build`**。**`-mod=readonly` では効かない** — Go 1.16 以降それは既定値で、素の `go build` と同じ。効いていないのは `go.work` のほうで、workspace があると `go.sum` ではなく `go.work.sum` が使われ、`go.sum` から行を消しても**どちらの書き方でも exit 0 になる** (実測)。CI は `go.work` を持たない (生成物で gitignore 済み) ので、既存の `go build ./...` が既に検証している (→ [プラグインの書き方](plugins/authoring.md)) |
| `make plugins` | `plugins/` を走査して組み込み用ファイルを生成 (#2480)。`make build` が内部で呼ぶ |
| `make plugins-all` | `disabled` のプラグインも含めて生成 (CI 検証用) |
| `make emoji-regex` | MFM の Unicode 絵文字の正規表現 (`internal/activitypub/mfm/emoji_regex_gen.go`) を、mfm-js が依存する `@misskey-dev/emoji-data` の `emojiRegex` から生成 (#3324)。`third_party/misskey` に `pnpm install` 済みであること。生成物は手で直さない |

### コード品質

| ターゲット | 内容 |
|---|---|
| `make fmt` | `gofmt -s -w .` |
| `make lint` | `go vet ./...` |
| `make golangci-lint` | `errcheck` / `govet` / `ineffassign` / `staticcheck`。`go vet` だけでは見えない層を埋める。設定は `.golangci.yml`。**既定の打ち切り (同一メッセージ 3 件 / linter 50 件) を外してある** — 切り詰めるだけなので赤が緑になることはないが、直すたびに隠れていた分が出てきて「全部直してから有効化する」が成立しない。`unused` / `ST1003` / `ST1012` / `SA1019` はすべて有効 (2026-09-22 に全件処理して 0 件。SA1019 は 10 件すべて移行、`unused` は 20 件を削除)。除外は `test/e2e_federation` のパッケージ名 (`.golangci.yml` の rule) 1 つだけ。`QF*` (28) と `S1016` は恒久的に無効 |
| `make actionlint` | GitHub Actions の workflow を検査 (式の typo・存在しない `needs` 参照・`runs-on` の誤り・`run:` の中のシェルを shellcheck 経由で)。**CodeQL の `actions` とは別物** — あちらは script injection などのセキュリティを見るが、式が壊れているかは見ない。`lint` job から `make` 経由で呼ぶので、版の定義は Makefile に 1 つだけ置く |
| `make test` | `go test ./... -v -race -count=1 -shuffle=3` (CI と同じテスト実行条件。PostgreSQL が要る → [testing.md](testing.md)) |
| `make test-fast` | `-race` 抜き (反復用)。**コミット前の検査ではない** — CI で落ちるものが手元で緑になる |
| `make frontend-test` | fork frontend の vitest (#2844) |
| `make plugin-vet` | 同梱プラグインを`go vet` + 既定無効を検査（CIの`build` jobの2 step相当） |
| `make plugin-test` | 同梱プラグインのテスト (別 module なので `./...` に含まれない) |
| `make plugin-doc-check` | `docs/plugins/authoring.md` の Go スニペットが実際にコンパイルできるか |
| `make emoji-regex-check` | `make emoji-regex` の生成物と snapshot (正規表現と mfm-js / emoji-data の版) が、submodule の mfm-js と emoji-data から作り直したものと一致するか。node_modules が要るので `make gates` ではなく `make frontend-check` から呼ばれる (#3324) |
| `make frontend-lint` | fork frontend の eslint。CI `frontend-check` job の Lint step と同じで、範囲は `package.json` の script が持つ (`--quiet "src/**/*.{ts,vue}"`)。`make frontend-check` から呼ばれる (#2906)。実測 55 秒 |
| `make frontend-test` | fork frontend の vitest (`test/unit/**/*.test.ts`)。CI `frontend-check` job の Unit test step と同じ |
| `make plugin-dev` | プラグインを編集しながら動かす (`PLUGIN=plugins/status`) |

### マイグレーション

| ターゲット | 内容 |
|---|---|
| `make migrate-up` | 最新まで適用 |
| `make migrate-down` | 1段階ロールバック (`-steps 1`) |
| `go run ./cmd/migrate -direction down` | **全段ロールバック**。`-steps` 未指定は「全部」の意味で、全テーブルが消える |
| `make migrate-create` | 新規マイグレーションファイル作成 |

#### 大規模テーブルへの index 追加

`golang-migrate/v4` の postgres driver は migration を **transaction 外** (auto-commit) で実行する (`runStatement` が `ExecContext` を直接呼ぶ)。そのため大規模テーブルへの index 追加では `CREATE INDEX CONCURRENTLY` を直接書ける:

```sql
-- migration/0000XX_large_index.up.sql
CREATE INDEX CONCURRENTLY IF NOT EXISTS "IDX_xxx" ON "yyy" ("zzz");
```

通常の `CREATE INDEX` は ACCESS EXCLUSIVE lock を取って書き込みを一時 block するが、`CONCURRENTLY` 付きなら Share lock のみで online で適用できる。production の数百万行クラスのテーブルでは推奨。

注意点:
- **`CONCURRENTLY` を含む migration は single-statement にする**。複数 statement を入れて途中で失敗すると、transaction 外実行ゆえ部分適用 (一部 statement だけ反映、残りは未適用) になり手動 cleanup が必要になる。1 ファイル 1 index が安全
- migration ファイル内に複数 statement を入れるなら `x-multi-statement=true` URL 拡張が必要 (現状未使用、`CONCURRENTLY` migration では避けること)
- `CONCURRENTLY` は失敗時に invalid index が残るので down migration で `DROP INDEX IF EXISTS` を必ず書く

### Docker

| ターゲット | 内容 |
|---|---|
| `make docker-build` | Dockerイメージビルド |
| `make docker-up` | `docker compose up -d` |
| `make docker-down` | `docker compose down` |
| `make docker-rebuild` | フロントエンド + イメージをまとめてビルド |
| `make docker-restart` | `app` を再起動して配信エントリを検証 |

### 静的 parity ゲート

サーバー・ブラウザ・Docker 不要で走る。CI では `go test ./...` の中でも自動実行される。詳細は[シェイプドリフト検出](shape-drift.md)。

| ターゲット | 検出対象 |
|---|---|
| `make shapecheck` | レスポンス形状の drift。`make shapecheck-gen` で golden snapshot を再生成、`make shapecheck-report` でレポート出力 |
| `make errorid-check` | error id / HTTP status / kind の drift。id gate は **router.go に inline endpoint が復活したら落とす** (#2791 で全 14 件を `internal/api` へ移設済み。closure は gate から漏れる) |
| `make limitspec-check` | ページネーションの default / max の drift |
| `make perm-check` | router middleware の権限が Misskey 本家より緩くないか (アクセス階層 / `secure` の双方向 / OAuth scope の 3 本) |
| `make wiring-check` | router / server で配線しないと効かないもの (FTT のトグル、global な security header、invite の moderator bypass、プラグイン peer の本文上限 / rate limit / catchall / 専用キュー / enqueuer、Web Push の producer 配線、chart management の logger を配線時に解決する形、ドライブ使用量の集計と route (#3053)、IP 履歴の記録 3 本 (#3103)、IP からの関連アカウント検索の repository と route (#3104)、`criticalWiringCount` の実数照合) が外れていないか。判定は AST 照合なので `//` でも `/* */` でも落ちる (#2856)。**既知の限界**: `if cond { ... }` の条件付き配線、未参照の関数の中に置かれた同じ配線、`e := e.Group(...)` のようにレシーバを同名でシャドウする形は素通りする |
| `make catalog-check` | `pg_indexes` 等のシステムカタログのクエリが `schemaname` で絞られているか (#2777) |
| `make notfound-check` | repository の lookup error を種別を見ずに 4xx にしている箇所を検出する (#2792)。**allowlist は件数で持つ** — key が `<file>:<func>` なので、同じ関数に足しても key が変わらない。増えたら新規流入、減ったら陳腐化として落ちる。**既知の限界**: **lookup が別ブロックに hoist されている形は射程外** — lookup と `if` のペアリングは「同じブロック内の後続 3 文」で見るので、`goroutine` に切り出して結果を外側の変数へ書く形 (`note_create_service.go` の reply / renote 先取得) は距離ではなくブロックが違うため掛からない。`lookupIfLookahead` を広げても増えない / **service 呼び出しの潰しは射程外** — `h.svc.Show(...)` の err を種別を見ずに 4xx にする形は api / core どちらの述語にも掛からない (lookup メソッドの呼び出ししか見ないため)。数え方: gate の `condChecksErrNonNil` / `bodyReturns4xx` をレシーバが `*svc` / `*Service` の `Show` / `Get` / `Find` / `Resolve` / `Fetch` / `Lookup` / `Require` 呼び出しに向け直して `internal/api` + `internal/server` + `internal/activitypub` を走査すると **29 件** (`ap/handler.go` 7 / `following/*` 7 / `users/*` 5 / `channels/*` 5 / `flash` 2 / `clips` 2 / ほか)。**AP の 404 はリモートに「消えた」と解釈されうる**。`notes/state` / `notes/conversation` / `antennas/show` の 3 件は #2799 で潰した / **重複チェックを DB 障害で skip する形も射程外** — `if dup, err := repo.Find(...); err == nil && dup != nil` は err を握り潰しているので `condMentionsErr` に掛からない。接続断のあいだ重複が作られる。**DB 側の backstop は無い** — 例えば `sw_subscription` に unique index は無く (`000020` が張るのは非 unique、`000068` がそれも落とす)、重複行は恒久的に残って web push が二重配信される。#2792 は handler 側の guard で塞いだだけなので、そこを外すと再発する。signup / page / reaction / poll などにも同じ形が残る / 同じ関数で 1 件直して 1 件足すと件数が変わらず素通りする / 4xx 以外への潰し (204 を返す `invite/handler.go:Delete` など) と `x, _ := repo.Find(...)` の握り潰しは対象外 / lookup メソッド名が `Find` で始まらず `Get` でもないもの (`metaRepo.Fetch()` など) は射程外 / 4xx を返すのが `if` の外や `else` 側にある形、`return h.notFound(c)` のような自前ヘルパー、`switch { case err != nil: ... }` も対象外。`internal/server` の該当は現在 0 件 (非テストの `internal/server/**` で `.Find*(` が 35、`.Get(` を足して 55。うち gate の述語に掛かったのは 2 件で、それも潰した) |
| `make nulparam-check` | 列に入らない値 (NUL) が SQL の bind parameter に載らないか (#3025)。**NUL を含む値はどの列にも入らないうえ、比較の右辺に置くだけで PostgreSQL がクエリごと落とす** (手元の simple protocol で SQLSTATE 08P01、本番の pgx extended protocol で 22021)。`IsNotFound` でもないので handler は 500 に倒し、**認証済みの一般利用者がパラメータ 1 文字で 5xx を立てられた**。3 本ある — カーソル (`id.NormalizeCursor` の ok を**代入の直後の `if !ok` で受けて return している**か。「関数のどこかに `!ok` がある」形だと、手前の `pagination.ResolveLimit` の `limitOK` へ受け直すだけで黙り、guard を DB 呼び出しの後ろへ動かしても空に倒しても緑になる。**ok を伝播する wrapper 5 つの呼び出し側も見る**) / repository の**単一行 lookup** (`Find*` / `Get*` が `(*model.X, error)` を返すもの) と `*ByID*` / LIKE パターンを組み立てる関数。**「どの値を見ているか」まで照合するのが要点** — 呼び出しの有無だけだと、複数の値を取る lookup で片方が無防備なまま緑になる (実際に `ListUsers` が `Username` は見て `Hostname` を見ていなかった)。`storableIDs` は**代入し直していること**まで見る (`storableIDs(ids)` は式文として合法で `go vet` も黙るが、フィルタは効かない)。**mock では検出できない** — `internal/testutil` の mock は NUL を渡しても普通に「見つからない」を返すので、guard を外しても handler テストは緑のまま通る。だから静的に見る。**既知の範囲**: 値を受ける一覧系 (`ListByUser(userID, ...)` など) は見ていない (数え方: 非テストの `internal/repository` 85 ファイルで、レシーバ付きメソッドのうち `string` / `*string` / `[]string` のパラメータを持つものが **524**、うち gate が見る単一行 lookup + `*ByID*` は **118**、guard を呼ばずに DB を触るものが **311** 残っている)。**カーソルだけは「受け取ったのに正規化していない handler」も見る** (`TestCursorParamsAreNormalized`) — 呼び出し側だけを見ていると、そもそも呼んでいない handler が視界に入らない。一覧系で実際に届く経路は測って個別に塞いであり、一覧は docs/divergence.md にある |
| `make compose-check` | 配布する compose がログをローテーションするか (`max-size` / `max-file` の値そのものを見る、#2828) |
| `make testflags-check` | `make test` が CI と同じテスト実行条件 (`-race` / `-count=1` / `-shuffle` seed) で走るか (#2841)。**両方向を見る** — CI にあって手元に無い flag も、手元にあって CI に無い flag (`-short` 等) も落とす。あわせて doc に書かれた `-shuffle` の seed が CI と一致するかも検査する |
| `make migrationdoc-check` | migration の本数を述べた doc が実態と合っているか (#2874)。**gate が見るのは 7 ファイル 21 箇所**。1 本足したとき実際に動くのはその一部で、#2866 (000082) では 17 箇所、**うち 5 箇所が漏れた**。総本数 / 破壊的なマイグレーションの件数 / `-- data loss:` 宣言の本数 / 作られるテーブル数と、down が no-op のものの**一覧**を突き合わせる。**一覧が本体** — 件数だけだと「1 本足して 1 本消す」で素通りする。**破壊的の件数は doc 自身の表の行数を truth にする** — migration の中身から「共有テーブルか」を判定すると upstream に無いテーブルを触るものまで拾う。対象外は 3 つ — 「51 本」(2 つの doc で定義が違うのに同じ数)、「102」(「上記 9 件」の定義に依存)、「データを不可逆に変えるのはこのうち 8 本」(機械判定できない)。拾えなかったら落とす |
| `make mdtable-check` | tracked な md の表の各行がヘッダと同じ列数か (#2930)。**GFM は溢れたセルを黙って捨てる**ので、ソースに書いた内容が GitHub 上で読めなくなる。原因はほぼセル区切りとして働くパイプで、**コードスパンの中でも働く** (`\\|` へエスケープする)。`docs/divergence.md` の 1 行で**描画が 599 文字あるべきところ 394 文字で止まり、205 文字 (34.2%) が読めなかった**。**見るのは列数だけ** — コードスパンの対応付けを自前で持つ案は、次の周で**二重バッククォートのコードスパンを含む行**を落とした (#2857 の「自前パーサに継ぎ足すと手当てするたびに隣の穴が開く」型)。取りこぼす側 (列数が一致したままコードスパンが割れる形など) はテストの doc コメントに明記してある。現 corpus (表 227 個) に対し偽陽性 0 |
| `make notiftype-check` | 通知タイプの一覧が `internal/core/notification` の registry 1 箇所から導出されているか (#2898)。**値の一致だけでは足りない** — リテラルへ書き戻しても書いた時点の中身は同じなので値比較は通り、落ちるのは core に型を足した後 = 一番検出したい瞬間に検出できない。導出している「形」を AST で固定してある |
| `make pluginembed-check` | mk-go をビルドする Dockerfile が `pluginbuild` を **`go build` より前に** 実行するか (#2940)。**組み込みを忘れてもエラーにならない** — `plugins/` に置いたのに入っていない image が黙って出来て、運営者は「入ったつもり」で起動できる。`Dockerfile.bundled` が実際にそうなっていた。検出は `go build` / `go install` × `cmd/misskey` / `cmd/...` の組で行い、**行継続は畳んでから**判定する (折り返した瞬間に検査対象から消えるのを防ぐ)。シェルの行末コメント (` #`) に退避させた `pluginbuild` も実行されないものとして扱う。組み込まない Dockerfile は理由付きで allowlist に登録する (連合 e2e 用など) |
| `make dockerignore-check` | `.dockerignore` がシークレットと利用者データを除外しているか (#2942)。**`.dockerignore` は全 build context 共通**なので、1 行落ちると `Dockerfile` / `Dockerfile.bundled` / `deploy/uds` / e2e の各 stack に同時に効く。`drive-files` (既定の drive の置き場所) と operator-local な設定が実際に抜けていた。**配る image には入らない** (最終 stage が明示パスの `COPY --from=builder` しか持たないため) が、build context と builder stage の layer には入り、`cache-to` を設定していればキャッシュ経由で読める。`!` による打ち消しと per-Dockerfile な `<名前>.dockerignore` の存在も見る。判定は自前ではなく `moby/patternmatcher` (Docker 本体の実装) に解かせる。**「残るべきものが残るか」も見る**ので、除外を広げすぎて COPY 元を巻き込む変更 (`.config/*.y*ml` → `.config/*`、`third_party/misskey/node_modules` → `**/node_modules` など) もここで落ちる。サイズの問題は対象にしていない (転送が遅くなるだけで、落ちても気付ける) |
| `make secretfield-check` | モデルの秘密フィールドが `json:"-"` を保っているか。**モデルをそのまま JSON 化する経路がある**ので、タグ 1 つが唯一の防波堤になっているフィールドがある。実測で `model.User.Token` のタグを外しても `make gates` も全テストも緑のままだった (native token を取れると、そのユーザーとして API を叩けるので権限ゲートを全て迂回できる)。**名前だけでは判定できない** — `Meta` の captcha secret は `admin/meta` が管理画面へ返すし、drive の `accessKey` は URL の構成要素で秘密ではない。そこで #2792 と同じ allowlist 方式にし、出してよいものには理由を書かせる。**allowlist は検出集合と突き合わせる** — 実在しないキーを書いても無視される形だと、守っているつもりで何も検査していない状態になる (初版が実際にそうで、`Meta.SensitiveMediaDetectionAPIKey` を登録していたが正規表現が `ApiKey` しか見ておらず `APIKey` に一致していなかった)。ただしそれが守るのは**allowlist に該当があるキーだけ**なので、該当が全て `json:"-"` 側にある alternative (`Pass` / `Code`) は `mustDetectSecretFields` で別に固定する。静的なタグ検査に加えて、代表的な型を実際に `json.Marshal` して秘密が出ないことも見る。**タグを書き忘れた形も拾う** — `encoding/json` はタグの無い exported フィールドを Go の名前でそのまま出すので、そこを skip すると「フィールドを足してタグを忘れる」という最頻のミスが素通りする。**逆に「落とすと壊れる」側も固定する** — モデルを直接 marshal する経路は 6 系統あり (moderation log / ephemeral store / webpush cache / `admin/relays` などレスポンス本体がモデルそのもの / `packedRecipient` のようにモデルを埋め込む struct / jsonb 列の往復)、`Meta.SMTPPass` や `RegistrationTicket.Code` のタグを落とすと監査記録が黙って欠ける (実際に一度壊した) |
| `make ipshape-check` | 利用者向けレスポンスと連合出力の shape に IP が出ていないか (#3136)。#3066 の完了条件の担保が `shapecheck` の golden 照合しか無く、**フィールドを足す変更は緑のまま通っていた** (実測: `UserLite` に `json:"lastIPs"` を足して `make shapecheck` は PASS)。**AST で全 struct の json タグを読む** — reflect で型を手で並べる形は `entity.MeDetailed` (= `/api/i`) を落としていた。走査は `internal/entity` / `internal/activitypub` に加えて `internal/api` / `internal/server` / `internal/stream` (handler がファイル内に宣言した response struct も stream の payload も wire の形になる)。実測は要素 2,332 / ユニークキー 755。**語の切り方は片側に寄せると穴が開く** — 大文字のたびに割ると `lastIPs` が、「小文字/数字の直後の大文字」だけだと `IPAddr` が素通りする (両方とも実測)。`IPaddress` のように割れない綴りのために `address` 系の alternative も要る。**キーの走査だけでは入れ子が見えない**ので、`internal/` 全体から「自分の JSON キーに IP を持つ型」を導出し、公開 shape がそれを**推移的に**参照していないことも見る (`model.User` は自分では持たないが `avatar.requestIp` を出す)。interface と関数型は伝播させない (混ぜると repository 一式が誤検出になる)。収集ロジックは `ipscanfixture` / `ipbearingfixture` / `marshalerfixture` を実際に `json.Marshal` した結果と突き合わせて固定する (`testdata/` に置くとコンパイルされず突き合わせられない)。**「違反 0 件が正常」な検査は抽出側にも下限が要る** — 参照側に置き忘れて、収集を潰す 1 行で本物の漏れが素通りした。走査の縮みは**ファイル単位**で見る (件数と代表キーだけだと大きなファイルさえ残れば通る)。**射程外**は `map[string]any` を手で組む経路、走査対象外に宣言した型を `c.JSON` にそのまま渡す形 (`/api/server-info`)、`remoteAddr` のように語として `ip` を取り出せない綴り |
| `make iprecord-check` | 利用者の IP を記録する call site が allowlist の外に増えていないか (#3135)。**#3105 の関連アカウント検索は `user_ip` の観測だけを見る**ので、失敗したサインインの IP がそこに入ると第三者が他人の関連候補を作れる。守りたいのは「どこからも呼ばれていない」という構造的な性質で、**endpoint ごとの振る舞いテストは叩いた経路しか見ない**。2 引数の `.Record(` を AST で拾い、許すものには理由を書かせる (IP と無関係な `deliveryhealth` の配送成否も 2 引数なので名前だけでは分けられない)。**成功側の共通入口 `RecordSuccessfulSignin` の呼び出し側も別に固定する** — 失敗経路からそれを呼べば `.Record(` は増えないので、列挙だけでは塞がらない。**呼ばずに値として持ち出す形と package 変数のクロージャの中も数える** — メソッド値にすると呼び出し側が `*ast.Ident` になり、名前で見る走査は名前で避けられる (署名検証より前に呼ばれる `resolvePasskeyUser` にその形を仕込む変異が、静的ゲートも振る舞いテストも素通りした)。型の位置とフィールドアクセスは除く。**枝そのものは人工ソース (`internal/entitycompat/recordfixture`) で固定する** — 実データにその形が無いので、枝を消しても実データからは何も起こらない。**call site の列挙だけでは順序が固定できない**ので、パスキーの記録が `fail()` より後ろにあることを**最初の `Record`** 基準で別に見る (最後のものを見る形だと、既存を残したまま前に 1 つ足す変異が素通りする) |
| `make sqlbind-check` | 値をクォートで囲んだリテラルへ差し込まず bind しているか。**chart の unique 配列は行の値そのもの** (外部由来の文字列を含みうる) なので、`internal/core/chart/repository.go` の `ApplyDeltas` が `?::varchar[]` で bind しているのが唯一の防波堤になる。初版 (`50ba697b`) からその形だが担保が無く、**配列リテラルを書式へ差し込む形に変えても既存の実 DB テストは緑のまま通った** (値が `u1` / `u2` だけで構造上意味を持つ文字を含まない)。**「これは SQL か」は判定しない** — キーワードの部分一致は両方向に壊れる。普通の英文が `values` に当たって事実と逆の診断を出す一方、`'%s'::varchar[]` のような断片はキーワードに当たらず収集すらされない (実際 `Insert` の placeholder を `Sprintf` 化する変異が静的ゲートも実 DB テストも素通りした、実測)。判定は「クォートで開いた区間に書式動詞が在るか」だけで、**定数連結は畳んでから見る** (折り返した書式は丸ごと収集から消えるため)。chart は名指しで別に見る。**`ApplyDeltas` が組む書式集合をそのまま pin する** — 「プレースホルダが在るか」はダミーの `?` 1 つで満たせ、別名の独立ビルダで畳む decoy が静的ゲートも chart 全テストも緑のまま通った (実測)。`pgArrayLiteral` の行き先は `Exec` / `Raw` へ可変長展開している識別子を解決して突き合わせる (`args` を決め打ちにすると、`params` への改名だけで既に bind しているコードへ事実と逆の指示を出す)。**「N 件以上」の下限は持たせず** (正当に 1 つ減らしただけで事実と逆の診断を出す)、実在する call site を `<file>#<func>` のソート済み一覧で名指しする。持つのは「1 つも拾えなかったら落とす」だけ (`map` で回すと報告されるサイトが実行ごとに変わる)。走査は 180 サイト / ユニークキー 98、違反は 4 サイト・3 キーで全て allowlist 済み、本番の未許可違反は 0 (実測)。**検出の枝は人工ソース `internal/entitycompat/sqlbindfixture` で固定する**。**射程外**は taint 解析、書式を `const` や変数に入れた `Sprintf`、書式途中の変数連結、`Fprintf` / `Errorf`、`fmt` の別名 import、文字列連結や `strings.Builder` で組む SQL、`Raw` / `Exec` へ渡す非リテラル、別 module の `plugins/` |
| `make submodulepin-check` | fork frontend の pin が doc (`docs/divergence.md`) と gitlink で一致しているか (#2969)。**submodule に commit して fork へ push したあと、親リポの gitlink を上げ忘れる片側更新**が実際に起きた (#2963)。doc には新しい tag、fork の branch と tag も push 済みなのに gitlink だけ古い、という状態で CI 28 チェックが全部緑のままマージされた。`Makefile` の `REVISION_LDFLAGS` は `describe --tags` で `MkGoFrontendVersion` を作るので、その窓にビルドしたバイナリは古い tag を名乗る。**SHA で突き合わせる** — tag から SHA を解くにはネットワークか submodule の checkout が要るので、pin 行に短縮 SHA を併記して親リポだけで完結させる (`git ls-files -s -- third_party/misskey` は submodule 未初期化でも gitlink を返すので `make gates` に載る)。**gitlink は index から読む** — doc は working tree から読むので、`ls-tree HEAD` にすると bump を `git add` した直後に必ず落ちる。**tag 名の正しさは CI の `build` job** (`git ls-remote` で tag → commit を解く) で見る。doc 内の整合 (pin 行の tag == §4-2 の表の最終行) はこちらでも見る。**配る bundled image が焼き込む assets image の tag** (`Dockerfile.bundled` の `MISSKEY_ASSETS_IMAGE`) も同じ輪に入れてある (#3011) — こちらも古い tag で image はビルドできるので CI は落ちず、配った先にだけ古い frontend が載る (実測で develop は 29 世代ずれていた。pin されていた `mk.0` から数えた間隔。数字付きの tag 30 個から 1 を引いた値で、英字付きを含めると 62 個から 1 を引いて 61)。走査は `git ls-files` で見た Dockerfile (名前が `dockerfile` / `containerfile` そのもの・その拡張子・その接頭辞のもの) なので、assets image を pin するファイルが増えても自動で対象になる。**`ARG` の既定値と、同じ repository を指すリテラル参照の両方**を見るので、`FROM <repo>:<古い tag>` と直接書いた Dockerfile も落ちる |
| `make gaterun-check` | `make gates` の各 target が `-run` で名指しするテストが実在するか (#2857)。**`go test -run` は該当なしでも exit 0 で通る**ので、ゲートが消えても緑になっていた。`git ls-files` で見るので `git add` 忘れも落ちる。`gates:` からの脱落も検査する |
| `make apicompat` | [API 互換性マトリクス](api-compat.md)を生成。内部で `make apicompat-routes` (route dump、stack 起動が必要) と `make apicompat-render` を実行する |

### e2e・互換性検証

いずれも隔離した compose project で動く。詳細は各ドキュメント参照。

| ターゲット | 内容 | 詳細 |
|---|---|---|
| `make playwright-up` `playwright-test` `playwright-down` `playwright-logs` | mk-go backend に対する Playwright spec | — |
| `make playwright-ts-up` `playwright-ts-test` `playwright-ts-down` | 同じ spec を Misskey TS backend に対して実行し、drop-in 互換を担保する | — |
| `make diff-up` `diff-test` `diff-down` `diff-logs` | mk-go と TS に同一リクエストを投げてレスポンスを値レベルで diff | [差分比較ハーネス](diff-e2e.md) |
| `make dropin-up` `dropin-test` `dropin-down` `dropin-logs` | TS 2 インスタンスの federation smoke | [Drop-in e2e](dropin-e2e.md) |
| `make dropin-mk-up` `dropin-mk-test` `dropin-mk-down` `dropin-mk-logs` | 上記の backend を mk-go に差し替えた overlay | 同上 |
| `make dropin-swap-test` | TS → mk-go 切替の state preservation を通しで検証 | 同上 |
| `make dropin-mkgo-born-test` | **mk-go 生まれの DB を TS に引き渡せるか** (= ロックインの有無) | 同上 |
| `make dropin-fedibird-test` | Fedibird-like AP mock との Ed25519 双方向 verify | 同上 |
| `make dropin-frontend-baseline` `dropin-frontend-up` `dropin-frontend-down` `dropin-frontend-logs` | 3 TS インスタンス + cypress | [Drop-in frontend e2e](dropin-frontend-e2e.md) |
| `make dropin-frontend-mk-up` `dropin-frontend-mk-down` `dropin-frontend-swap-test` | 上記の mk-go overlay と切替シナリオ | 同上 |
| `make federation-misskey-build` `federation-misskey-up` `federation-misskey-test` `federation-misskey-down` `federation-misskey-logs` | Misskey 本家インスタンスを立てて実際に連合させる | [ActivityPub連合](federation.md) |
| `make federation-misskey-e2e` | 上記を起動から撤去まで通しで実行 (CI の `federation` シナリオと同じ) | 同上 |
| `make federation-mastodon-e2e` `federation-mastodon-down` | 本物の Mastodon を立てて引用の承認 (FEP-044f) を確かめる。前者は起動から撤去まで通し (CI の `federation-mastodon` シナリオと同じ) | 同上 |
| `make e2e-submodule-init` | submodule を初期化 (本家フロントエンドの取得)。e2e 系の前提 | — |
| `make playwright-up` `playwright-test` `playwright-down` | Playwright によるフロントエンド / API テスト | [Playwright](playwright.md) |
| `make upstream-e2e-deps` `upstream-e2e-up` `upstream-e2e-migrate` `upstream-e2e-test` `upstream-e2e-down` | Misskey 本家の backend e2e をテスト本体無改変で mk-go に向けて実行 | [本家 backend e2e](upstream-backend-e2e.md) |

### ベンチマーク

| ターゲット | 内容 | 詳細 |
|---|---|---|
| `make bench-up` `bench-run` `bench-down` `bench-logs` | k6 で mk-go と Misskey 本家に同一負荷をかけて比較 | [pprof プロファイリング](bench-pprof.md) |
| `make queue-bench-all` (`queue-bench-up` `queue-bench-seed` `queue-bench-outbound` `queue-bench-inbound` `queue-bench-report` `queue-bench-down` `queue-bench-logs`) | BullMQ / mkq の 2-way スループット比較 | [queue-bench](queue-bench.md) |
| `make queue-bench-autoscale-run` `queue-bench-autoscale-down` `queue-bench-autoscale-logs` | worker 数 fixed16 / fixed64 / auto の drain time 比較 | [オートスケール設計](design/auto-scale-job-workers.md) |

### 本番 UDS

| ターゲット | 内容 |
|---|---|
| `make uds-init` `uds-build` `uds-up` `uds-down` `uds-down-v` `uds-logs` `uds-ps` | UNIX ドメインソケット構成の本番スタック操作 ([UDSデプロイ](docker-uds.md)) |
| `make uds-frontend-build` | 本番向けフロントエンドビルド |
| `make uds-rebuild` | フロントエンド + イメージをまとめてビルド |
| `make uds-restart` | `mkgo` を再起動して配信エントリを検証 |

> **警告**: `make uds-frontend-build` と `make e2e-frontend-build` は `third_party/misskey/built` に出力する。**本番コンテナがこのディレクトリを bind-mount している**ため、「ビルドが通るか確かめるだけ」のつもりで実行すると配信中のアセットが差し替わる。mk-go はエントリポイントを起動時に 1 回だけ解決してキャッシュするので、ハッシュが変わると HTML が消えたファイルを指したまま **404 でフロントが起動しなくなる**。
>
> - フロントの型チェックだけなら `third_party/misskey/packages/frontend` で `npx vue-tsc --noEmit` / `npx eslint` を直接叩く (Docker 不要で速い)
> - 本番へ反映する意図で実行した場合は、続けてコンテナを再起動すること

## コーディング規約

- `gofmt -s -w .`で整形 (CIで強制)
- `go vet`を通す (CIで強制)
- 命名はGo標準 (camelCase/PascalCase、略語は全大文字: URL, ID, API)
- Early returnでネストを浅く保つ
- エラーは`fmt.Errorf("context: %w", err)`でラップ
- GoDoc(関数/型のドキュメント)は英語、インラインコメント(実装の背景)は日本語
- 自明な処理の説明コメントは書かない

詳細な規約はCLAUDE.md Section 5を参照。

## ブランチ運用

| ブランチ | 役割 |
|---|---|
| `main` | リリース |
| `develop` | 開発統合 |
| `feature/<issue番号>-<要約>` | 機能追加 |
| `fix/<issue番号>-<要約>` | バグ修正 |

すべての作業は対応するissueを先に作成してから着手する。

### コミットメッセージ

- 形式は `<種類> <対象>: <要約> (#issue番号)` (例: `Fix admin: リモート絵文字のインポートでライセンスを空で上書きしない (#3246)`)
- 種類は `Fix` / `Feat` / `Test` / `Docs` / `Refactor` / `Bump` / `Chore` のどれか。詳細は CLAUDE.md Section 7
- コミット前に `make check` を実行 (fmt → lint → actionlint → golangci-lint → test)

### PR作成

- PRタイトル: issue のタイトルか、作業の要約
- PR本文: Summary、主な変更点、テスト、`Closes #<issue番号>`
- `gh pr create`を使用

## CI/CD

### 必須チェック (`.github/workflows/ci.yml`)

`main`と`develop`へのpush/PRで実行される。branch protectionのrequired checksは`build` / `test` / `lint`の3つ。

#### buildジョブ
`go build ./...`で全パッケージのビルド確認。続けて同梱サンプルが`mk-plugin.yml`で既定無効のままかを検査し (#2701)、同梱プラグインを`go vet`する。**required jobなので、コンパイル以外の理由でも赤くなる**。手元の再現は`make plugin-vet`。

#### test-shardsジョブ + testジョブ
- `shard: [1,2,3,4]`の4-way matrixで並列実行。各shardが独立したPostgreSQL 18 / Redis 7のサービスコンテナを持つ
- 対象パッケージは`go list`でテストファイルを持つものだけに絞り、ImportPath順にソートしてから`NR % 4`で分配する。分配が決定的なので、パッケージが増えても各shardの担当は再現する
- `-race -count=1 -timeout 10m -coverprofile=... -covermode=atomic`
- パッケージ別カバレッジ閾値を各shard内で検証し、1つでも未達ならそのshardが失敗する

| パッケージ | 閾値 | 理由 |
|---|---|---|
| `internal/api/admin` | 80% | `handler_stubs.go`にSMTP / queue / DB集計等の外部依存が多く90%に届かない。現状83.8%と小マージンのため80%でロック |
| `internal/server` | 0% | 大部分が`router.go`のwire層 (handler配線 / middleware設定) で、e2e / drop-in test経由で実挙動を検証する設計。個別handler (`avatar.go`等) は`_test.go`で個別にカバーする運用 |
| `internal/testutil` | 0% | mock / test helper専用でproduction codeを含まない |
| `e2e` 配下 | 0% | 実挙動カバレッジで測る意味が薄い |
| それ以外 | 90% | |

- `test`ジョブは`needs: test-shards` / `if: always()`で全shardを束ね、branch protectionが要求する`test`という単一checkを公開する。いずれかのshardが失敗すれば`exit 1`

#### lintジョブ
- `go vet ./...`
- **actionlint** (`make actionlint`) — workflow の式と `run:` の中のシェル (shellcheck 経由)
- `gofmt -s -d .`で差分チェック (差分ありで失敗)
- 重複 fixture ID の検出
- **golangci-lint** (`make golangci-lint`) — errcheck / govet / ineffassign / staticcheck

### 非ブロッキングのPRチェック

以下はPRで走るが**required checksには入っていない**ので、落ちてもマージはブロックされない。赤いチェックとして表示されるので、内容を確認して別PRで対処する。

| check | workflow | 内容 |
|---|---|---|
| `vulncheck` | CI | 依存・Go stdlib の**到達可能な**既知脆弱性 + Go version の pin 整合 |
| `frontend-check` | CI | fork frontend の型 (`vue-tsc --noEmit`) + submodule のソースを読むゲート + eslint + vitest + `make plugins-all` と統合バイナリの build。**`make frontend-check` は型・ゲート・eslint まで** (#2906) なので、job 全体は [ci.md](ci.md) の手元再現を使う |
| `plugin-tests` | CI | 同梱プラグインのテスト (別 module なので `go list ./...` に入らない) |
| `build-and-push` / `-bundled` | Docker | image がビルドできるか (PR では push しない) |
| `spec (mk-go 1/4)` 〜 `4/4` | Playwright | ブラウザからの統合互換。TS backend での実行は `workflow_dispatch` のみ |
| `e2e (1/4)` 〜 `4/4` | Upstream backend e2e | 本家の backend e2e が mk-go に対して通るか |
| `diff` | Diff e2e | mk-go と TS の**レスポンスの値**が一致するか |
| `swap-test` / `mkgo-born` / `ed25519-verify` / `federation` / `federation-mastodon` | Drop-in e2e | 切替・ロックイン・Ed25519・実連合 (Misskey TS / Mastodon) の 5 シナリオ |

どれが何を守っているかの対比は [ci.md](ci.md) にまとめてある。

### nightly

PR では回らず schedule で実行されるものが 2 つある。

| workflow | 内容 | 時刻 |
|---|---|---|
| `Drop-in frontend e2e (nightly)` | 3 TS インスタンス + cypress で frontend 視点の drop-in 互換 | 19:00 UTC |
| `Queue-bench smoke (nightly)` | queue driver がジョブを落としていないか (`ok == sent`) | 17:30 UTC |

### CI失敗時の対応

- カバレッジ不足 → テストケースを追加してから再push
- `gofmt`差分 → `make fmt`を実行してから再push
- テスト失敗 → CIログを読み、ローカルで再現させてから修正する。`--no-verify`等でフックを飛ばさない
- testcontainersのskip-on-failure起因のflakeがあるため、PRと無関係な失敗は再実行で解消することがある

## コマンド一覧 (旧 CLAUDE.md Section 3)

CLAUDE.md の Section 3 にあった一覧を、#3248 でここへ移した。全 target は `make help` が出す。

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

**上記は全体ではない。** `make help` が全 143 target を出す (`^名前:.*##` の行を数えた)。一覧と説明は
このファイルの上の節、CI 上の対応は [docs/ci.md](ci.md)。

エントリポイント：
- メインサーバー: `./cmd/misskey -config .config/default.yml`
- マイグレーション: `./cmd/migrate -direction up`

## 変更の経緯 (旧 CLAUDE.md の更新記録)

CLAUDE.md の「更新記録」に書かれていた本文を、#3248 でここへ移した。**記述は当時のまま**で、文中の「Section N」は当時の CLAUDE.md の節を指す。新しいものが上。

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
- **2026-09-09**: `make frontend-check` に eslint を追加し、`make frontend-lint` を新設 (#2906)。`make help` の target は 127 → 128。**手元で CI と同じ検査ができていなかった** — `frontend-check` は `vue-tsc` と submodule ゲートだけで、eslint は CI の**別 step** (`pnpm eslint`) だった。#2903 で実際に踏んでいる (デッドコードを消したときの空行 2 連続が `@stylistic/no-multiple-empty-lines` で落ちた)。個別ファイルに `npx eslint` を掛けても CI と同じ glob ではないので見落とす。CLAUDE.md 2026-09-05 の #2841 (`make test` と CI の flag がずれていた) と**同じ型**。**引数は書き写さず `package.json` の script を呼ぶ** — 書き写すと #2841 と同じドリフトが起きるので、`npm run --silent eslint` で script を唯一の定義にした (CI は `pnpm eslint` だが手元に pnpm があるとは限らない。既存の `frontend-check` / `frontend-test` も npx を使っている)。**`eslint .` にしないこと** — upstream が lint していない `test/` まで拾い、追従のたびに他人の負債で落ちる。実測 55 秒で、#2903 と同じ違反を入れて `make frontend-check` が exit 2 で落ちることを確認した。**vitest は入れていない** (`make frontend-test`) — CI も別 step で、こちらは #2844 で既に手元の再現手段がある。
- **2026-09-07**: Section 3 に「更新 (運用)」のコマンドを足し、Makefile に `pull` / `pull-plugins` / `uds-rebuild` / `uds-restart` / `docker-rebuild` / `docker-restart` の 6 target を追加した (#2885)。`make help` の target は 120 → 126。**`docker compose up -d` は再起動を保証しない** — image と設定が変わらなければコンテナを作り直さないが、frontend は bind mount なので frontend だけ更新したときは何も変わらない。mk-go は `DetectClientEntry` で entry を起動時に 1 回だけ解決してキャッシュするので、再起動しないと消えた古いハッシュを配り続ける。**2026-09-07 に本番で 10 分近くこれを踏んだ** — `built` の最終書き込みが 13:02:52、mk-go の再起動が 13:12:36 で **9 分 44 秒**。`build.ts` は出力先を rm してから作るので、ビルド開始からの実際の窓はさらに長い。mk-go は 05:17 起動のままだった。Makefile と `docs/deployment.md` には「再ビルドと再起動は必ずセット」という原則が元からあったが、**そのセットを `up -d` が実現できていなかった** — 宣言した不変条件を、実行するコマンドが満たしていない型。
  検証は `deploy/check-frontend-entry.sh` が持つ。敵対的レビューで、初版が**この PR が防ぎたい状況で緑を返す**ことが実測で示された (High 2 件)。(a) 公開 URL は Cloudflare の裏なので、直前まで配信していた古いアセットはエッジに残っており、素で叩くと `cf-cache-status: HIT` の 200 が返る (使い捨てクエリを足すと MISS になり、事故当時の entry は 404 と分かる)。(b) index の loader は `CLIENT_ENTRY.replace('scripts', lang)` で**言語ごとのパスへ振り替える**ので、`scripts/` だけ見てもブラウザが読む URL を見ていない。あわせて `sort -u | head -1` は「アルファベット順で最初の `scripts/*.js`」であって CLIENT_ENTRY ではなく、modulepreload が 1 本増えた日に無検証で緑になる形だった。**「CSS では判定できない」の理由も誤っていた** — `emptyOutDir: false` で古いファイルが残るからではなく、`build.ts` が毎回 `built/_frontend_vite_` を消したうえで**内容ハッシュが同じものは同じ名前で作り直す**ため。理由を取り違えると「CSS だけ内容が変わればその名前だけ変わる」という取り逃がしに気付けないので、現在は entry (全言語) と stylesheet の両方を見る。
  **`DOCKER_CONFIG` という make 変数を作ってはいけない。** docker CLI が設定ディレクトリとして読む予約名で、make は環境由来の変数を recipe へ export し直すため、operator の環境にそれがあると値を奪って `docker compose` が `unknown command` で死ぬ (実測)。このリポジトリでは `uds-*` を含む docker 系 target が全滅する。初版で踏んだので `ENTRY_CHECK_DOCKER_CONFIG` に改名した。
  **`docker-*` 系は本番 UDS のホストで叩かない。** `docker-compose.yml` は `name:` を持たないので project 名がディレクトリ名 `mk` になり UDS 本番と同じ project に合流する。`app` / `db` / `redis` が本番の隣に立ち上がり、本番のコンテナは orphan 扱いになる (compose 自身が `--remove-orphans` を勧めてくる)。しかも検証先は `.config/docker.yml` の url なので、**本番を触らないまま緑を返す**。
  プラグインは `plugins/*/` のうち `.git` を持つ 4 つ (fedwatch / genshin / hsr / nowplaying) が独立リポジトリで、`make update` の `--recurse-submodules` では追従しなかった (`status` / `trustlevel` は本体に tracked なので追従する)。dirty なものは名前を出して skip する — 勝手に stash すると編集中の変更が「消えた」ように見えるうえ、復元手順もどこにも残らない。あわせて `update` が `git pull` の終了ステータスを見ておらず、**pull に失敗しても「変更なし」と表示して exit 0** していたのを直した (本番更新の起点になったので影響範囲が広がっていた)。
  **`make pull` は submodule の生成物を先に戻す。** `make plugins` (pluginbuild) が `packages/frontend/src/server-plugins.generated.ts` を、`pnpm -r build` の i18n パッケージが `packages/i18n/src/autogen/locale.ts` を書き換える。どちらも submodule 内の **tracked ファイル**なので、一度でもビルドしたワークツリーは常に dirty になる。dirty なまま gitlink が動くと `git pull --recurse-submodules` は checkout に失敗するため、**frontend の再ビルドが要る回 (= submodule bump 回) に限って一括コマンドが必ず止まり、しかも親リポだけ進んだ混在状態で止まる**。そのまま分解実行を続けると新 backend + 旧 frontend が本番に載る。戻すのは**生成物だけ**にしてある — それ以外の変更が残っていれば git 自身が止まるので、frontend に手を入れている最中の作業を黙って捨てない。
  **検証スクリプトは抽出に失敗したら落とす。** 初版は `LANGS` や stylesheet の書式が変わって正規表現が空振りしても、対象が減るだけで緑を返した (実測: 言語別アセットが全滅していても exit 0)。この PR 自身が引いている「拾えなかったら落とす」に反していたので、`LANGS` が空のとき、および index に `rel="stylesheet"` があるのに href を 1 本も拾えないときは落とすようにした。
  **片側更新が 2 箇所残っていた。** `README.md` と `docs/upstream-catch-up.md` が事故そのものの手順 (`docker compose up -d` / `up --build -d`) を勧めたままで、しかも README は直下で「再ビルドしたら必ず再起動する」と宣言していた。後者は「submodule の静的アセットを image に焼き込んでいるので `--build` 必須」とも書いていたが、vite frontend は bind-mount で渡しており image に入るのは static-assets / twemoji / fluent-emoji だけ。CLAUDE.md 2026-08-20 の「直したら固有の語で `git grep` する」に該当。
  **この PR は停止時間を縮めていない。** frontend のビルドは配信中のディレクトリを開始直後に消すので、ビルド開始から再起動完了までは 404 になる (事故当日の実測ではビルドが約 19 秒、再起動していなかった時間が 9 分 44 秒で、**窓のほぼ全部が後者**だった)。塞いだのは「**恒久的に**壊れたまま気付かない」方だけで、無停止にするには別ディレクトリへビルドして差し替える構成が要る。
- **2026-08-19**: ドキュメント全体監査 (#2637) で見つかった、**手順どおりに実行すると壊れる記述**を修正 (#2638)。(1) `make migrate-down` は `-steps` 未指定で全 down が走っていたので `-steps 1` を付け、ヘルプ・doc の「1 段階」と挙動を一致させた (全段は `go run ./cmd/migrate -direction down` を直接叩く)。(2) **`DATABASE_URL` はどこからも読まれていない** — `cmd/migrate` は `-config` から DSN を組み立てる。Section 9 の該当項目を接続先の説明に置き換えた。(3) Section 4 のテスト準備を実態に合わせた: **testcontainers は Redis 用** (`SetupRedis` は 27 パッケージ、`SetupPostgres` は 3 パッケージ) で、PostgreSQL は外部のものを使う (既定は `localhost:5432` の `misskey_test` / `mk`)。`MustOpenTestDB` は失敗時 panic なので「Docker があれば準備不要」ではない。
- **2026-08-15**: PostgreSQL を 16 → 18 に統一 (#2513)。compose 全構成・CI service container・testcontainers を `postgres:18-alpine` へ。upstream Misskey の compose 例 (18-alpine) に整合。**postgres:18 image は data layout が変わった** (default PGDATA が `/var/lib/postgresql/18/docker`、VOLUME 宣言が親 `/var/lib/postgresql`) ため、永続 volume を持つ compose のマウント先を `/var/lib/postgresql` へ変更 (旧パスのままだと新規デプロイが匿名 volume に initdb して down で消える。UDS example は明示 PGDATA で回避)。既存の 16 volume は dump→restore が必要 (手順は docs/deployment.md 冒頭)。Section 4 / 8 の版数記述を更新。
