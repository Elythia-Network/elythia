# CI で回る項目

PR を出すと十数個の check が走る。**どれが何を見ていて、落ちたとき何を疑うか**の early reference。

## 全体像

`build` / `test` / `lint` の 3 つだけが **required check** (これが赤いとマージできない)。
残りは非ブロッキングで、落ちても merge 自体は可能。ただし非ブロッキングは「無視してよい」
意味ではなく、**merge をブロックするには不確実性が高い**という判断にすぎない。赤いまま
放置すると誰も見なくなるので、原因を切り分けてから進めること。

非ブロッキングを `continue-on-error: true` で実現している箇所は無い。あれは job を成功扱いに
するので失敗が完全に不可視になる。job は正しく失敗させ、required に入れないことで
非ブロッキングにしている。

## required check

| check | workflow | 見ているもの | 手元での再現 |
|---|---|---|---|
| `build` | CI | 全パッケージがコンパイルできるか + 同梱プラグインの `go vet` + 同梱サンプルが既定無効か | `go build ./...` / `make plugin-vet` |
| `lint` | CI | `go vet` + **actionlint** + `gofmt -s -d` の差分 + 重複 fixture ID + **golangci-lint** | `make lint` / `make actionlint` / `make fmt` / `make golangci-lint` |
| `test` | CI | 4-way shard の集約。どれか 1 つでも落ちれば赤 | `make test` |

### `test` が落ちたとき

実体は `test-shards (1)` 〜 `(4)`。集約 job のログではなく**失敗した shard のログ**を見る。

主な原因は 3 つ。

1. **カバレッジ閾値割れ** — パッケージごとに 90% (例外あり、CLAUDE.md Section 8 参照)。
   テストを足してから再 push する
2. **本物の失敗** — ローカルで `go test ./internal/<pkg>/... -race -count=1 -shuffle=3` を回して
   再現させる。**`-race` と `-shuffle` を落とすと再現しない** (データ競合と順序依存は
   これでしか出ない、#2841)。全体なら `make test` が同じ条件で走る
3. **testcontainers の flaky** — PR と無関係なパッケージ (reaction の count_writer 等) で
   落ちていたら再実行を試す

### `lint` の golangci-lint が落ちたとき

`make golangci-lint` で同じものが出る。設定は `.golangci.yml`。

**打ち切りを外してある。** golangci-lint は既定で同一メッセージ 3 件 / linter あたり
50 件で報告を**切り詰める**。0 件にするわけではないので赤が緑になることはないが、
**直すたびに隠れていた分が出てくる**ので「全部直してから有効化する」が成立しない
(導入時にこれで測定を 3 回やり直した)。`max-issues-per-linter: 0` / `max-same-issues: 0`
を入れてある。

**`(typecheck)` が出た run は不完全。** typecheck が落ちると他の linter の結果が
報告されない。自前プラグインを `plugins/` に置いていると `cmd/misskey/plugins_generated.go`
が private module を import するので、**`GOWORK=off` を付けて回すと**起きる (`go.work` が
あるまま素で叩けば解決するが、それだと CI と条件が変わる)。`make golangci-lint` は生成物を
退避して回すので手元では踏まない。

**同時に 2 つ走らせられない。** ロックは `/tmp/golangci-lint.lock` でマシン全体。
重なると `parallel golangci-lint is running` で exit 3 になり、lint 失敗と紛らわしい。

**段階的な有効化は完了した。** `unused` / `ST1003` (命名) / `ST1012` (error var 名) /
`SA1019` (非推奨 API) はすべて有効で、恒久的に無効なのは `QF*` と `S1016` だけ。
除外は 1 つだけ — `test/e2e_federation` の**パッケージ名** (ディレクトリ名まで変えると
24 箇所に波及するため。`git grep -oI e2e_federation -- '*.go' | wc -l`)。

誤検知は `//nolint:staticcheck // 理由` をその行に置く。理由を必ず書く。

**独立行ではなく行末に置く。** 独立行に置くと、対象が複数行にまたがる式のときに
**その全体が死角になる** — 実測で `e.Use(echomw.LoggerWithConfig(...))` の 10 行が
丸ごと黙り、抑制したかった行の下にある処理まで検査されなくなった。既存の 2 件
(`internal/api/signin/passkey_test.go` / `internal/server/middleware/internal_call_test.go`)
は独立行のままだが、どちらも対象が 1 行なので射程はその 1 文に収まっている
(**折り返した瞬間に同じ穴が開く**)。機械的な検査は無い。

### `lint` の actionlint が落ちたとき

`make actionlint` で同じものが出る。workflow の式の typo、存在しない `needs` 参照、
`runs-on` の誤り、`run:` の中のシェル (shellcheck 経由) を見る。

**CodeQL の `actions` とは別物。** あちらは script injection のような**セキュリティ**を
見るが、式が壊れているかどうかは見ない。workflow のミスは動かすまで分からないので、
静的に落とす側が要る。

**版は Makefile に固定してある。** 新しい検査が増えても、workflow を触っていない PR が
赤くなることはない。上げるときは `make actionlint` のバージョンを明示的に変える。

**shellcheck が無いと黙って検査が減る。** actionlint は `run:` の中身を shellcheck へ
渡すが、無ければその分だけ落として**成功で返す**。CI の ubuntu-latest には入っているので、
手元だけ通って CI で落ちる (導入時に実際に踏んだ。手元 0 件 / CI 10 件)。`make actionlint`
は shellcheck が無ければ落とすので、出たら入れる (`sudo apt install shellcheck`)。

誤検知は `# shellcheck disable=SCxxxx` を**その行の直前**に置く。ブロックの先頭に置くと
以降の本物まで黙る。

### `lint` が落ちたとき

`gofmt` 差分なら `make fmt` を実行して再 push。`go vet` は repository interface に
メソッドを足したときに、手動 test fake が複数パッケージに散っていて漏れることが多い。
**push 前に `go vet ./...` を全体にかけること。**

## 非ブロッキングの check

| check | workflow | 見ているもの | 実測 | 手元での再現 |
|---|---|---|---|---|
| `vulncheck` | CI | 依存・Go stdlib の**到達可能な**既知脆弱性 + Go version の pin 整合 | 1 min | `GOOS=linux govulncheck ./...` |
| `review` | Dependency review | PR が**新しく持ち込む**依存に既知の脆弱性が無いか (base と head の差分を比較、high 以上で失敗) | 未計測 | 手元では回せない (GitHub の advisory DB を引く) |
| `analyze (go)` / `analyze (actions)` | CodeQL | **自分のコード**の静的解析 (Go の全 module と workflow の式) | 未計測 | 手元では回せない (CodeQL CLI が要る)。Code scanning alerts で見る |
| `frontend-check` | CI | fork frontend の型 (`vue-tsc --noEmit`) + submodule のソースを読むゲート + eslint (`src/**/*.{ts,vue}`) + vitest + `make plugins-all` と統合バイナリの build | 3〜4 min | 下の「frontend-check の手元再現」。**`make frontend-check` は型・ゲート・eslint まで** (#2906) |
| `plugin-tests` | CI | 同梱プラグインのテスト (別 module なので `go list ./...` に入らない) | 1 min | `make plugin-test` |
| `apicompat` | apicompat | **`docs/api-compat.md` が実態とずれていないか** (生成物なので再生成して diff を見る) | 未計測 | `make apicompat` (**プラグイン抜き + testMode が要る**。手順は docs/development.md) |
| `e2e (1/4)` 〜 `4/4` | Upstream backend e2e | **本家の backend e2e 1256 テスト**が mk-go に対して通るか | 3-7 min | `make upstream-e2e` |
| `diff` | Diff e2e | mk-go と TS の**レスポンスの値**が一致するか (endpoint 比較 35 件) | 4 min | `make diff-check` |
| `swap-test` | Drop-in e2e | TS→mk 切替で state が保たれるか | 5 min | `make dropin-swap-test` |
| `mkgo-born` | Drop-in e2e | **mk-go 生まれの DB を TS に引き渡せるか** (= ロックインの有無) | 5 min | `make dropin-mkgo-born-test` |
| `ed25519-verify` | Drop-in e2e | Fedibird-like mock との Ed25519 双方向 verify | 5 min | `make dropin-fedibird-test` |
| `federation` | Drop-in e2e | 本物の Misskey TS との実連合 (follow/note/reaction/renote/reply/mention/delete) | 4 min | `make federation-misskey-e2e` |
| `spec (mk-go 1/4)` 〜 `4/4` | Playwright | ブラウザからの統合互換 (298 spec ファイル) | 4-9 min | `make playwright-check` |
| `build-and-push` / `-bundled` | Docker | image がビルドできるか (PR では push しない) | 4 min | `docker build -f Dockerfile .` |
| `build / build` | Build with plugins (selftest) | 運営者向けの reusable workflow が通るか。外部プラグインを実際に clone し、frontend を持つので SPA の自前ビルド (`ASSETS_SOURCE=local`) まで走る。**`docker build --check` では見えない範囲** (pluginbuild / go build が通るか、`assets-local` の COPY 元が context に実在するか、pnpm の symlink を越えられるか) を確認できるのはこの check だけ | 6 min | paths に該当する PR で自動発火する。手動なら `gh workflow run build-with-plugins-selftest.yml --ref <branch>` (default branch にある場合のみ) |

### e2e 系が「何を守っているか」の違い

守備範囲が重なっているように見えて、実は別のものを見ている。

| | 見ているもの |
|---|---|
| `e2e` (本家 backend e2e) | **本家のテストが通るか** |
| `diff` | **同じ入力に対する値そのもの** |
| shape drift (`test` に含まれる) | フィールドの有無・型 |
| `swap-test` | DB を引き継いだときに壊れないか |
| `mkgo-born` | **mk-go が作った DB を TS が受け取れるか** |
| `federation` / `ed25519-verify` | 他実装と実際に喋れるか |
| `vulncheck` | **自分のコードではなく依存**に既知の穴が無いか (develop に入った後、到達可能なものだけ) |
| `review` | **入る前**に、その PR が持ち込む依存に既知の穴が無いか (到達可能性は見ない) |
| `analyze (go)` / `analyze (actions)` | **自分のコード**にパターンで見つかる欠陥が無いか |

shape が合っていても値が違う類のバグは `diff` でしか捕まらない。ユニットテストは
「自分で署名して自分で検証する」ことしか保証しないので、相互運用は `federation` /
`ed25519-verify` でしか担保できない。

`vulncheck` だけは毛色が違い、**自分が書いたコードを一切見ない**。テストが全部通っていても
依存の既知脆弱性は素通りするので、別の signal として要る (導入時、通常テストが緑のまま
到達可能な脆弱性が 11 件見つかっている)。

CodeQL はその逆で、**依存ではなく自分のコード**をパターンで見る。テストは「書いた振る舞いが
その通りか」しか見ないので、書いていない分岐や、通ってはいるが危険な形は素通りする。
`actions` の解析も入れてあり、`pull_request` のコンテキストを式に埋める形 (script injection)
のように**レビューで見落としやすく、落ちても気付きにくい**ものを拾う。

`swap-test` と `mkgo-born` は似て見えるが、**DB を作った側が違う**。

|  | DB を作ったのは | 経路 |
|---|---|---|
| `swap-test` | TypeORM | TS → mk-go → TS |
| `mkgo-born` | **mk-go の migration** | mk-go → TS |

後者の方が厳しい。TS が一度も触っていない schema を受け取るので、カラム型・制約・
enum・index 名・default のどれかが TypeORM の期待とずれていれば起動しない。
`TestMigrationSeed_CoversUpstream` は seed 一覧と upstream の migration file を
**静的に突き合わせる**だけで、実際に TS を起動して確かめてはいない。

運用上これは**ロックインの有無そのもの**にあたる。「mk-go で始めた人が Misskey に
移れるか」に答えられるのはこの経路だけで、実際この経路の初回実行で、RSA 秘密鍵が
PKCS#1 のため TS 側の送信連合が全滅する不具合が見つかっている (#2380)。

### `e2e` (本家 backend e2e) が落ちたとき

まず **意図的な乖離かどうか**を判断する。mk-go では『通らないことが正しい』テストが
あり、`tests/upstream-e2e/known-divergences.json` に根拠付きで登録して expected-failure
として扱っている。

- 一覧に載っているテストが `Expect test to fail` で落ちた → **乖離が解消した**。一覧から外す
- 載っていないテストが落ちた → 互換性の regression。直す

`skip` ではなく expected-failure なのは、乖離が解消したときに気付けるようにするため。
詳細は [upstream-backend-e2e.md](upstream-backend-e2e.md)。

### `diff` が落ちたとき

**ignore-list を安易に広げないこと。** 空振りさせると本物の乖離が埋もれる。

mk-go 独自の additive field が原因なら `tests/diff/test_endpoints.py` の ignore-list に
**理由付きで**登録する。その際 [divergence.md](divergence.md) に対応する記述があるかを
確認すること。`META_IGNORE` と `USER_IGNORE` は別定義で後者は前者を継承していないので、
`policies` のように両方に現れるキーは両方へ足す必要がある。

そうでなければ値レベルの regression。条件付きの field 出し分けで壊すことが多い。

### drop-in / federation 系が落ちたとき

federation delivery に flaky 要素があるので、まず再実行を試す価値はある。ただし
**繰り返し落ちるなら本物**。失敗時は `docker compose logs` が
`dropin-logs-<scenario>` artifact として 14 日残るので、それを見る。

artifact には 2 種類のログが入る。`compose.log` / `ps.log` は orchestrator が
`down -v` する**前**に自分で残したもの、`compose-post.log` / `ps-post.log` は
workflow が後から集めたもの。前者がある場合はそちらが本命で、後者は stack が
既に撤去されていて空のことがある。

`mkgo-born` だけは落ち方が他と違い、原因が段階からほぼ特定できる。

| 落ちた段階 | 意味 |
|---|---|
| stage 4b (TS-A healthy 待ちで timeout) | mk-go の migration が作った schema を TypeORM が受け付けなかった |
| stage 4d (migrations digest 不一致) | migration seed (`000029`) に漏れがあり TS が再実行した |
| stage 5 (pytest) | schema は通ったがデータを読めない / 連合が続かない |

手元で再現するときは各 make target を直接叩く。いずれも専用の compose project
(`mk-dropin` / `mk-federation` / `mkdiff`) で隔離されており、**本番 UDS の project `mk` には
触れない**。

### `vulncheck` が落ちたとき

2 つの step があり、落ちた step で意味が違う。

**Go version pin の不一致** — `go.mod` の `go` directive と、golang image を使う Dockerfile
(`git grep` で列挙した全て。`Dockerfile.bundled` や `tests/` の検証用も含む) の builder tag が
ずれている。両方を同じ patch version に揃える。分けて検査しているのは、`govulncheck` が
見るのは `go.mod` 側だけで、**Dockerfile だけ古いと CI は緑のまま配る image が脆弱**に
なるため。builder を `golang:1.27-alpine` のような floating tag に戻すのも不可
(pull 時期で stdlib の patch が変わり、再現可能な形で「既知脆弱性を含まない」と言えない)。
配る Dockerfile (`Dockerfile` / `Dockerfile.bundled` / `deploy/uds/Dockerfile.mkgo`) は
base image を `golang:1.27.1-alpine@sha256:<digest>` のように **tag と digest の併記**で
固定しているので (patch の tag でも publish し直しで中身が変わる。
`TestDistributedDockerfileBaseImagesArePinnedByDigest` が見る)、この検査は tag 側で版を
照合し、digest は形だけを見る。Go の版を上げるときは digest も取り直すこと
(`docker buildx imagetools inspect golang:<ver>-alpine` の Digest 行)。digest だけの更新は
dependabot の `docker` が出す。

**govulncheck の検出** — 手元で同じコマンドを回す。

```
go install golang.org/x/vuln/cmd/govulncheck@latest
GOOS=linux "$(go env GOPATH)/bin/govulncheck" ./...
```

`GOOS=linux` を付けるのは、実際にデプロイするのが Linux だから。付けないと host 依存の
package load エラーで解析が空振りしうる。**ローカルの `go` が古いと govulncheck 自身が
古い toolchain でビルドされ、`package requires newer Go version` で解析できない。**
その場合は `GOTOOLCHAIN=go1.27.1 go install ...` のように明示してビルドし直す。

検出されるのは**呼び出しが到達可能なもの**だけで、import しているだけの脆弱性は落ちない。
無視リストを育てずに運用できる設計なので、**抑制するより直すこと**。対応は原則 2 つ。

- 依存モジュール → `go get <module>@<fixed>` で修正版へ。**修正版の指定は govulncheck の
  `Fixed in:` をそのまま使う。** 同じモジュールに複数の脆弱性があると必要な版が別々で、
  一番低い版に上げても残ることがある
- Go stdlib → `go mod edit -go=<patch>` と Dockerfile の builder tag を上げる (`git grep -n 'FROM golang:'` で全て拾う)

新しい CVE が公開されると、**コードを変えていない PR でも落ちる**。これは required check に
していない理由でもある。落ちたときは自分の変更が原因とは限らないので、まず `Found in:` の
モジュールが PR で触ったものかを見ること。

### `analyze (go)` / `analyze (actions)` が落ちたとき

**コードを変えていない PR でも落ちうる。** CodeQL のクエリパックは CLI の更新で増えるので、
新しいクエリが既存のコードを検出することがある。`vulncheck` を required から外しているのと
同じ理由で、これも required には**含めていない**。

結果は Actions のログではなく **Code scanning alerts** に出る
(`https://github.com/shiroha-a/mk/security/code-scanning`)。まず alert を読み、

- 本物なら直す
- 誤検知なら alert 側で dismiss する (理由を選ぶ)。ソースに抑制コメントを撒かない

**手元では再現できない。** CodeQL CLI と DB の構築が要るので、ローカルで回す手順は用意して
いない。PR で出た alert をそのまま読む運用。

`analyze (go)` がビルドで落ちた場合は解析以前の問題で、`go build ./...` か
同梱プラグイン (`plugins/*/go.mod`) のビルドが壊れている。こちらは `build` job と
`plugin-tests` job でも落ちるはずなので、そちらを先に見る。

### `frontend-check` が落ちたとき

fork frontend (`third_party/misskey`) の型・eslint・vitest のいずれか、または
submodule のソースを読むゲートの失敗 (#2892)。型・ゲート・eslint は
`make frontend-check` で再現する (#2906)。**vitest はそこに入っていない**
(`make frontend-test`)。

**手元再現（CI job 全体）:**

```bash
cd third_party/misskey && pnpm install && pnpm build-pre && pnpm -r build
make plugins-all && go build -o /dev/null ./cmd/misskey
make frontend-check
make frontend-test
```

eslint だけを回すなら `make frontend-lint` (実測 55 秒)。

**`make uds-frontend-build` / `e2e-frontend-build` は検証に使わないこと。** 本番が
bind-mount している `third_party/misskey/built` を書き換えてしまう。`vue-tsc --noEmit` なら
出力物を作らない。

**submodule の gitlink 巻き戻り（祖先関係）はこの job でも検出できない。** 型が通るだけで
ファイルが消えている場合がある。pointer の確認は
[upstream-catch-up.md](upstream-catch-up.md#mk-固有パッチだけを載せるときrelease-bump-以外)。

## nightly のみ

PR では回らない。失敗は Actions 上で確認して別 PR で対処する。

| workflow | 内容 | 時刻 |
|---|---|---|
| Drop-in frontend e2e | 3 TS インスタンス + cypress で frontend 視点の drop-in 互換 | 19:00 UTC |
| Queue-bench smoke | queue driver がジョブを落としていないか (`ok == sent`) | 17:30 UTC |

## 手動実行のみ

| workflow | 内容 | 実行方法 |
|---|---|---|
| Playwright (`spec (ts 1/4)` 〜 `4/4`) | 同じ spec を **Misskey TS backend** に対して実行し、spec が mk-go の挙動に引きずられていないかを検証 | upstream 追従で submodule を bump したとき |
| Docker (`workflow_dispatch`) | 過去のリリースタグから image を publish し直す | `gh workflow run docker.yml -f tag=1.1.1` |

`spec (ts …)` を常時回さないのは、upstream が変わらない限り答えが変わらないため。詳細は
#2289。

## action の版固定

`.github/workflows/` から参照する action と reusable workflow は、**GitHub 公式
(`actions/*`) も含めて全て commit SHA で固定する**。

```yaml
- uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
```

**tag は付け替えられる。** `@v7` のような参照は、action のリポジトリ (あるいはそれを
乗っ取った第三者) が tag を別 commit へ動かした瞬間に中身が変わる。`docker.yml` と
`build-with-plugins.yml` は `packages: write` を持って GHCR へ publish するので、そこで
動く action が差し替わると**配る image そのものを書き換えられる**。公式だけ例外にしないのは、
「どれが例外か」を読む側が毎回判断しなくて済むようにするため。

`TestWorkflowActionsArePinnedToSHA` (`internal/entitycompat/actions_pin_test.go`) が
`owner/repo@<40 桁の SHA> # vX.Y.Z` の形になっているかを見る (`docker://` は
`docker://<image>@sha256:<digest>` を固定として扱う)。`test-shards` で回るので
tag 参照に戻すと required check の `test` が落ちる。同じリポジトリ内の参照 (`./...`) と
YAML のコメント行は対象外。workflow は YAML パーサで読むので、flow 形式
(`- {uses: ...}`) や値を次の行に置く書き方も拾う。**SHA とコメントの版が対応して
いるかは見ない** — tag を解くにはネットワークが要り、ゲートでは判定できない。

**更新は dependabot に任せる。** `.github/dependabot.yml` の `github-actions` ecosystem が
週 1 回、固定した action の新しい版を 1 つの PR にまとめて出す。dependabot は SHA と
`# vX.Y.Z` のコメントを一緒に書き換えるので、コメントの書式 (`# v` + 3 桁の版) を崩さない
こと。

**手で上げるとき**は tag を commit SHA に解いてから書く。

```bash
git ls-remote https://github.com/actions/checkout 'refs/tags/v7*'
```

- **annotated tag は `^{}` の行を使う。** `refs/tags/v7.0.1` の行は tag object の SHA で、
  `refs/tags/v7.0.1^{}` の行が commit。`uses:` に書くのは commit
- `v7` のような major tag は動く前提のもの。**それと同じ commit を指す `vX.Y.Z` を探して
  コメントに書く** (major tag 名をコメントにすると、どの版を固定したのか読めない)
- tag が無く branch だけのもの (`actions/dependency-review-action` の `v4` は branch) は
  `refs/heads/<name>` の SHA と、同じ commit を指す `vX.Y.Z` tag を使う

あわせて **publish する job の `actions/checkout` には `persist-credentials: false` を
付ける** (`docker.yml` の 2 job / `docker-branch.yml` / `build-with-plugins.yml`)。
付けないと token が `.git/config` に残ったまま、後続の `pnpm install` の lifecycle script や
第三者のプラグインのコードが走る。いずれの job も checkout 後に mk-go の git 認証を使って
いない (`docker-branch.yml` の push は token を URL に明示した別リポジトリから行う)。

## 落ちたときの一般的な注意

**「手元では通るのに CI で落ちる」場合、手元の生成物を疑う。** 過去に何度も踏んでいる。

- `third_party/misskey/built/*` — 過去の docker build が root 所有で残っていることがある
- `packages/*/built` — 同上。`i18n` / `misskey-bubble-game` が無いと frontend の型が通らない
- `built/.config.json` / `built/meta.json` — 本家の `loadConfig()` が読む生成物

CI はまっさらな環境なので、手元にある生成物が要件を隠す。新しく CI に載せる workflow は
**その PR 自身の paths に含めて、PR 上で発火させて確認する**こと。

## 関連ドキュメント

- [`testing.md`](testing.md) — テストの種類と書き方
- [`upstream-backend-e2e.md`](upstream-backend-e2e.md) — 本家 e2e と既知乖離の運用
- [`diff-e2e.md`](diff-e2e.md) — 値レベル差分比較ハーネス
- [`dropin-e2e.md`](dropin-e2e.md) — drop-in 切替の検証
- [`shape-drift.md`](shape-drift.md) — entity shape / error id の drift gate
- [`divergence.md`](divergence.md) — 意図的な差分のカタログ
- [`contributing.md`](contributing.md) — コントリビューション手順
