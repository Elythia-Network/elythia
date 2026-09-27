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
| `make migrationdoc-check` | migration の本数を述べた doc が実態と合っているか (#2874)。**gate が見るのは 8 ファイル 22 箇所**。1 本足したとき実際に動くのはその一部で、#2866 (000082) では 17 箇所、**うち 5 箇所が漏れた**。総本数 / 破壊的なマイグレーションの件数 / `-- data loss:` 宣言の本数 / 作られるテーブル数と、down が no-op のものの**一覧**を突き合わせる。**一覧が本体** — 件数だけだと「1 本足して 1 本消す」で素通りする。**破壊的の件数は doc 自身の表の行数を truth にする** — migration の中身から「共有テーブルか」を判定すると upstream に無いテーブルを触るものまで拾う。対象外は 3 つ — 「51 本」(2 つの doc で定義が違うのに同じ数)、「102」(「上記 9 件」の定義に依存)、「データを不可逆に変えるのはこのうち 8 本」(機械判定できない)。拾えなかったら落とす |
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
| `feature/<phase>-<要約>` | 機能追加 |
| `fix/<対象>-<要約>` | バグ修正 |

すべての作業は対応するissueを先に作成してから着手する。

### コミットメッセージ

- Phase単位の機能追加: `Phase N.M: <要約>`
- 修正: `Fix <対象>: <要約>`
- コミット前に `make check` を実行 (fmt → lint → actionlint → golangci-lint → test)

### PR作成

- PRタイトル: `Phase〇 <内容>` または作業の要約
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
| `swap-test` / `mkgo-born` / `ed25519-verify` / `federation` | Drop-in e2e | 切替・ロックイン・Ed25519・実連合の 4 シナリオ |

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
