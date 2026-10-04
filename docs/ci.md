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

1. **カバレッジ閾値割れ** — パッケージごとに 90% (例外あり、CLAUDE.md Section 4 参照)。
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
除外は 1 つだけ — `tests/e2e-federation` の**パッケージ名** `e2e_federation` (Go のパッケージ名に
ハイフンは使えないので下線のまま残している。理由は `.golangci.yml` のコメント)。

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
| `apicompat` | apicompat | **`docs/api-compat.md` が実態とずれていないか** (生成物なので再生成して diff を見る)。あわせて golden が本家の版に追いついているか (`make upstream-check`、#3378) | 未計測 | `make apicompat` (**プラグイン抜き + testMode が要る**。手順は docs/development.md) |
| `e2e (1/4)` 〜 `4/4` | Upstream backend e2e | **本家の backend e2e 1256 テスト**が mk-go に対して通るか | 3-7 min | `make upstream-e2e` |
| `diff` | Diff e2e | mk-go と TS の**レスポンスの値**が一致するか (endpoint 比較 35 件) | 4 min | `make diff-check` |
| `swap-test` | Drop-in e2e | TS→mk 切替で state が保たれるか | 5 min | `make dropin-swap-test` |
| `mkgo-born` | Drop-in e2e | **mk-go 生まれの DB を TS に引き渡せるか** (測る対象。保証はしない、#3191) | 5 min | `make dropin-mkgo-born-test` |
| `ed25519-verify` | Drop-in e2e | Fedibird-like mock との Ed25519 双方向 verify | 5 min | `make dropin-fedibird-test` |
| `federation` | Drop-in e2e | 本物の Misskey TS との実連合 (follow/note/reaction/renote/reply/mention/delete) | 4 min | `make federation-misskey-e2e` |
| `federation-mastodon` | Drop-in e2e | 本物の Mastodon との引用の承認 (FEP-044f): 双方向の引用が承認済みになるか、取り消しが双方向で効くか | 未計測 | `make federation-mastodon-e2e` |
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

「mk-go で始めた人が Misskey に移れるか」に答えられるのはこの経路だけ。移れることは
保証しない (#3191) が、どこまで移れるかを測るために残している
([dropin-e2e.md の「復路は測る対象」](dropin-e2e.md#復路は測る対象-3191))。実際この経路の初回実行で、RSA 秘密鍵が
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

**`emoji-regex-check` が落ちたとき (#3324):** MFM の Unicode 絵文字の正規表現 (`internal/activitypub/mfm/emoji_regex_gen.go`) が、submodule に入っている mfm-js / emoji-data と食い違っている。`… is stale` なら mfm-js か emoji-data の版が上がったので、作り直して差分ごとコミットする。生成ツールが `no longer contains` や構文のエラーで落ちたら、mfm-js の `unicodeEmoji` の書き方か正規表現の構文が変わっているので、生成ツール (`tools/emojiregex/`) を直す。

```bash
make emoji-regex
node internal/activitypub/mfm/testdata/emoji_mfmjs.mjs third_party/misskey \
  > internal/activitypub/mfm/testdata/emoji_mfmjs.json
GOWORK=off go test ./internal/activitypub/mfm/ ./tools/emojiregex/
```

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
| Playwright (`spec (ts 1/4)` 〜 `4/4`) | 同じ spec を **Misskey TS backend** に対して実行し、spec が mk-go の挙動に引きずられていないかを検証 | upstream 追従で本家の版を上げたとき |
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

## 各 workflow の設計 (旧 CLAUDE.md Section 8)

CLAUDE.md の Section 8 にあった各 workflow / job の説明を、#3248 でここへ移した。CLAUDE.md には一覧表と、workflow を書くときのルールだけを残してある。

`.github/workflows/ci.yml`で以下のジョブが`main`と`develop`への push/PR で実行されます。

#### `build`ジョブ

checkout / setup-go を除くと step は実行順に 4 つ。**required job なので、コンパイル以外の理由でも赤くなる。**

- `go build ./...`で全パッケージのビルド確認。
- **`Check submodule commit is pushed` step** で、`third_party/misskey` の gitlink が
  指す commit が fork に push 済みか、`docs/divergence.md` の pin 行が指す tag が
  その commit と一致するかを見る (#2969)。submodule を上げたときに fork への push や
  tag の push を忘れると、ここで落ちる。
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

#### `test-shards`ジョブ + `test` aggregator

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

#### `plugin-tests`ジョブ

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

#### `lint`ジョブ

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
  `S1016` だけ。**除外は 1 つ** — `.golangci.yml` の rule が 1 件 (`tests/e2e-federation` の
  パッケージ名 / ST1003) **だけ**。**これは有効化した 4 check に対する数**で、
  `exclusions.presets` の `std-error-handling` (実測 253 件を抑止) は別枠。
  `//nolint:staticcheck` はリポジトリ全体で 2 件 (SA9010 / SA1012) で、どちらも
  今回の 4 check とは無関係。
  版は Makefile 側に 1 つだけ置く。
  **一番重いので step の最後**に置いてある。

#### `vulncheck`ジョブ

- `GOOS=linux govulncheck ./...` で依存と Go stdlib の**到達可能な**既知脆弱性を検出する。実際にデプロイするのは Linux なので `GOOS` を明示する (未指定だと host 依存の package load エラーで空振りしうる)。
- あわせて `go.mod` の `go` directive と、golang image を使う**全ての** tracked な Dockerfile (`Dockerfile` / `Dockerfile.bundled` / `deploy/uds/Dockerfile.mkgo` / `tests/` の検証用) の builder tag が同じ patch version を指していることを検査する。govulncheck が見るのは `go.mod` 側だけなので、**Dockerfile だけ古いと CI は緑のまま配る image が脆弱になる**。builder を floating tag (`golang:1.27-alpine`) に戻さないこと (pull 時期で stdlib の patch が変わり、再現可能な形で「既知脆弱性を含まない」と言えない)。配る Dockerfile の base image は tag と digest の併記 (`golang:1.27.1-alpine@sha256:...`) で固定しており、この検査は tag 側で版を照合する。
- 検出は import しているだけのものを含まず、**呼び出しが到達可能なもの**に限られる。無視リストを育てずに運用できるので、抑制ではなく更新で直す。修正版は govulncheck の `Fixed in:` に従うこと (同一モジュールに複数の脆弱性があると必要な版が別々で、低い方に上げても残る)。
- PR の required check には**含めない**。新規 CVE の公開でコードを変えていない PR でも落ちるため。
- 導入は #2387。通常テストが全て緑の状態で到達可能な脆弱性が 11 件残っており、既存の check では捕まらない領域だったため追加した。

#### `dependency-review` workflow (PR トリガー)

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

#### `codeql` workflow (PR / push / weekly)

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

#### `dropin-e2e` workflow (PR トリガー)

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
  どこまで移れるかを測る唯一の経路にあたる (保証はしない、#3191)。`TestMigrationSeed_CoversUpstream` は
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

#### `playwright` workflow (PR トリガー)

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

#### `upstream-backend-e2e` workflow (PR トリガー)

- `.github/workflows/upstream-backend-e2e.yml` で Misskey 本家の backend e2e
  (本家の `packages/backend/test/e2e/**`) を mk-go に向けて実行する。本家は
  `UPSTREAM_MISSKEY_VERSION` の版を `.cache/misskey/<版>` に checkout する (#3378)。
  テスト本体は無改変で、vitest の `globalSetup` / `setupFiles` (`tests/upstream-e2e/harness/`) だけを差し替える。
- `pull_request` で paths (`internal/**` / `cmd/**` / `migration/**` /
  `tests/upstream-e2e/**` / `UPSTREAM_MISSKEY_VERSION` / `Makefile` / `go.mod` /
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

#### `diff-e2e` workflow (PR トリガー)

- `.github/workflows/diff-e2e.yml` が `make diff-check` を実行し、mk-go と Misskey TS に
  同一リクエストを投げて**レスポンスを値レベルで diff** する (#2078 / #2368、endpoint 比較 35 件)。
- 守備範囲が他のゲートと違う。本家 backend e2e は「本家のテストが通るか」、shape drift は
  「フィールドの有無・型」、diff-e2e は「**同じ入力に対する値そのもの**」を見る。shape が
  合っていても値が違う類のバグはこれでしか捕まらない。
- 意図的な差分は `tests/diff/test_endpoints.py` の ignore-list に**理由付きで**登録する。
  空振りさせると本物の乖離が埋もれるので、追加時は `docs/divergence.md` にも対応する記述が
  あるかを確認すること。
- PR の required check には**含めない**。

#### `apicompat` workflow (PR トリガー)

- `.github/workflows/apicompat.yml` が `make apicompat` を回し、**`docs/api-compat.md` が
  実態とずれていないか**を見る。あれは生成物で CLAUDE.md も「手で直さない」と書いているが、
  **再生成が人手に頼っていた**ので、route を足しても upstream が endpoint を増やしても
  マトリクスは黙って古くなる。読む人は「mk-go only 59 件」のような数字を現状だと思う。
- **既存のどの job にも相乗りできない。** 本家 (TS の endpoints を読む) と DB / Redis
  (route dump がサーバーを組み立てる) の両方が要るが、`test-shards` は本家を checkout せず、
  `frontend-check` は DB を持たない。本家を取得するので、golden の追いつき
  (`make upstream-check`) もこの workflow で見る (#3378)。
- **config は `tests/upstream-e2e/mkgo.yml`。** `testMode: true` が要る — 無いと
  `/api/reset-db` が route に載らず、マトリクスが「TS 側に存在するが未実装 1 件」に化ける。
  接続先だけ `MK_*` で service container へ向ける。
- **プラグインは入らない前提。** 同梱の 2 つは `disabled: true` なので `pluginbuild` が
  skip する (#2701)。自前プラグインを `plugins/` に置いた手元で回すと 19 行混入するが、
  clean checkout では起きない。
- PR の required check には**含めない**。判定材料に本家の内容が入るので、こちらの
  コードを触っていない PR でも upstream の bump で赤くなりうる。

#### `frontend-check` job (ci.yml)

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

#### `build-with-plugins` workflow (reusable) / `build-with-plugins-selftest` (PR トリガー)

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

#### `docker` / `docker-branch` workflow

- `docker.yml` は **`push` / `pull_request` / `workflow_dispatch`** で発火し、
  image がビルドできるかを見る (PR では push しない)。check 名は
  `build-and-push` / `build-and-push-bundled`。`workflow_dispatch` は過去の
  リリースタグから image を publish し直す用途
  (`gh workflow run docker.yml -f tag=1.1.1`)。
- `docker-branch.yml` は **image をビルドしない**。`develop` への push (paths フィルタ付き) と `workflow_dispatch` で、compose ファイルだけを載せた orphan ブランチ `docker` を force-push する (「pull して動かすだけ」の構成を配るため)。検査は `docker compose config --quiet` のみ。
- PR の required check には**含めない**。

#### schedule で回る workflow

PR では回らないので、失敗は Actions 上で確認して別 PR で対処する。

| workflow | 内容 | 時刻 |
|---|---|---|
| `dropin-frontend-e2e.yml` | 3 TS インスタンス + cypress で frontend 視点の drop-in 互換 | 19:00 UTC |
| `queue-bench-smoke.yml` | queue driver がジョブを落としていないか (`ok == sent`) | 17:30 UTC |

#### CI失敗時の対応

- カバレッジ不足 → テストケースを追加してから再push。
- `gofmt`差分 → `make fmt`をローカルで実行してから再push。
- テスト失敗 → CIログを読み、ローカルで再現させてから修正。`--no-verify`等でフックを飛ばさない。

## 変更の経緯 (旧 CLAUDE.md の更新記録)

CLAUDE.md の「更新記録」に書かれていた本文を、#3248 でここへ移した。**記述は当時のまま**で、文中の「Section N」は当時の CLAUDE.md の節を指す。新しいものが上。

- **2026-09-30**: Section 3 に `make federation-mastodon-e2e`、Section 8 の `dropin-e2e` に
  `federation-mastodon` シナリオを追加 (#3234)。`make help` の target は 139 → 141
  (`federation-mastodon-e2e` / `-down`)。**引用の承認 (FEP-044f) は相手の実装が読めるかでしか
  確かめられない** — こちらのユニットテストは「自分で描画して自分で読む」ことしか保証せず、
  Mastodon が `interactionPolicy` をどう解釈し、Accept の `result` をどう検証するかは実物に
  喋らせないと分からない。公式 image (`ghcr.io/mastodon/mastodon:v4.7.2`) をそのまま使い、
  秘密鍵は起動時に生成する (commit しない)。**変異で落ちることを確かめてある** —
  範囲を配らない形で 4 件中 3 件、承認を返さない形で承認のテスト、ブロック時に Reject せず
  承認する形で Reject のテストが落ちる。
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
- **2026-09-08**: Section 3 の `make frontend-check` と Section 8 の `frontend-check` job に、submodule のソースを読むゲートを追記 (#2892)。`/about-misskey` の謝辞アイコン 62 枚が `img-src 'self' data: blob:` でブロックされ本番で 1 枚も表示されていなかったのを、`img-src` に固定 2 origin (`avatars.githubusercontent.com` / `assets.misskey-hub.net`) を足して直した。**upstream が host を足すと黙って壊れる**ので、`about-misskey.vue` から外部画像の host を抽出して定数と過不足なく突き合わせるゲートを置いた。**`make gates` には入れない** — あちらは submodule 無しで回る前提で、混ぜると checkout していない環境で skip され「検査していないのに緑」になる。`test-shards` は `third_party/misskey` を checkout しないため、submodule を取る `frontend-check` でだけ回し、`MK_FRONTEND_GATES_REQUIRE_SUBMODULE` で skip を禁じる (`plugin-tests` の `MK_PLUGIN_TESTS_REQUIRE_DB` と同じ形)。**media proxy 経由には落とせない** — mk-go の proxy は upstream と違い open proxy ではなく、allowlist が DB に実在する URL だけを通すので静的な URL は 403 (実測)。**この doc 更新自体が #2892 で漏れていた** — Makefile の target と CI job の中身を変えたのに、それを説明する 5 ファイル 7 箇所が「型チェックだけ」のまま残っていた (CLAUDE.md が「最多の型」と呼ぶ片側更新)。
- **2026-08-24**: Section 8 の `build` ジョブに `Check bundled plugins are disabled by default` step を追記 (#2701)。同梱サンプルは #2495 で既定無効にする方針にしたが、trustlevel は #2586 で `disabled: true` 付きで同梱したあと **#2585 の実測を採るために意図的に外され、実測が終わっても戻っていなかった**。起きたのは「新しく同梱したものに既定を付け忘れた」ではなく「**検証のために一時的に外して戻し忘れた**」なので、gate はそちらを主対象にしてある。判定は **`git ls-files` + grep だけ**で完結させてある — tracked な `plugins/*/mk-plugin.yml` に `disabled: true` の行があること (列挙が空なら「検査していないのに緑」になるので落とす)。`pluginbuild` に読ませるほうが parser 一致で厳密だが、`pluginbuild` の `discover` は git ではなく**ディレクトリ**を走査するので、`plugins/` に自前プラグインを置いている手元では誤検知するうえ、生成物を書いて `make plugin-dev` の配線を巻き戻す。**残る穴は許容している** — 行ベースの判定なので parser がキーとして読まない位置 (2 つ目の YAML ドキュメント、flow collection の中) に同じ行があると通る。意図的に行わないと踏めない形。手元の再現は `make plugin-vet` (#2701 で新設。`make help` の target は 110 → 111)。
- **2026-08-18**: Section 8 の `playwright` / `upstream-backend-e2e` を 4 シャード並列として書き換え (#2609)。どちらも**プロセス内では並列にできない** (前者は共有の root アカウントと instance meta、後者は `maxWorkers: 1` + ファイルごとの `/api/reset-db`) ため、並列度はシャードごとに job を分けて稼ぐ。あわせて実態と乖離していた記述を修正: `playwright` は nightly ではなく PR トリガー (#2291 の反映漏れ)、`upstream-backend-e2e` の所要時間は「18-20 min」ではなく分割前で 8.5 分。Playwright の録画を止めた理由も明記。
- **2026-08-16**: `plugin-tests` job を追加 (#2588)。同梱プラグインのテストは**どの job でも実行されていなかった** (別 module で `go list ./...` に含まれず、`build` job に PostgreSQL が無い)。テストが落ちる変更を入れても CI は緑のままだった。あわせて `build` job の同梱プラグイン検証を `go build` から `go vet` に変更 (テストファイルもコンパイルされるので、公開面を変えて本体だけ直したときに検出できる)。Section 3 に `make plugin-test` を追記。
- **2026-08-07**: Section 3 に本家 backend e2e の Makefile target (`make upstream-e2e` 系 5 つ) を、Section 8 に `upstream-backend-e2e` workflow を追記 (#2347)。Misskey 本家の `test/e2e/**` を無改変で mk-go に向けて回す PR トリガーの workflow で、required check には含めない。既知乖離は skip でなく expected-failure (`task.fails`) で扱う運用も明記。
- **2026-08-07**: Section 8 に `diff-e2e` workflow と `frontend-check` job を追記 (#2368)。CI 非対象だった検証資産の棚卸しで、値レベル diff と fork frontend の型チェックを載せた。
- **2026-08-08**: Section 8 に `vulncheck` ジョブを追記 (#2387)。`GOOS=linux govulncheck ./...` による到達可能な既知脆弱性の検出と、`go.mod` / `Dockerfile` の Go patch version 整合チェック。required check には含めない (新規 CVE 公開でコード無変更の PR でも落ちるため)。
- **2026-08-08**: Section 8 の `dropin-e2e` workflow に `mkgo-born` シナリオを追加 (#2383)。`make dropin-mkgo-born-test` (mk-go 生まれの DB を TS に引き渡す経路 = ロックインの有無) を CI に載せる。あわせて 2 つの既存不具合を解消: (1) orchestrator が自分で残した診断ログを workflow 側の収集が空ログで上書きしていたので `-post` 付きの別名に分けた、(2) paths フィルタに `docker-compose.dropin*.yml` が無く、drop-in stack の定義を壊す変更で workflow が発火せず緑に見えていた。
- **2026-08-07**: Section 8 の `dropin-e2e` workflow に `federation` シナリオを追加 (#2362)。あわせて Section 3 に `make federation-misskey-e2e` (起動から撤去まで通しで実行) を追記。
- **2026-08-07**: Section 8 の `dropin-e2e` workflow を 2 シナリオ matrix として書き換え (#2360)。`ed25519-verify` (`make dropin-fedibird-test`) を追加し、あわせて nightly → PR トリガーへの移行 (#2291) が未反映だった記述を実態に合わせた。
- **2026-05-16**: `Makefile` に `make dropin-fedibird-test` を追加 (#1086)。Section 3 (Development Commands) の Drop-in 系コマンド一覧に Fedibird-like mock との Ed25519 e2e を載せる。
- **2026-05-07**: Playwright nightly CI workflow を Section 8 に追記 (#816)。`.github/workflows/playwright.yml` で Phase 1 spec を毎日 17:00 UTC に develop で実行する、matrix `backend = [mk-go, ts]` 並列、`fail-fast: false`、PR required check には含めない方針を明文化。
- **2026-04-22**: Section 3 に drop-in frontend e2e Phase 14-3 関連の Makefile target (`make dropin-frontend-mk-up` / `make dropin-frontend-mk-down` / `make dropin-frontend-swap-test`) を追加 (#394)。TS-A 切替後の mk-A でも cypress spec が pass することを検証する swap orchestrator を入口に出す。
- **2026-04-21**: Section 3 に drop-in frontend e2e Phase 14-1 関連の Makefile target (`make dropin-frontend-baseline` / `dropin-frontend-up` / `dropin-frontend-down`) を追加 (#381)。3 Misskey TS インスタンス + cypress runner 構成。
- **2026-04-21**: Section 8 に `dropin-e2e` workflow (nightly) を追記 (#374)。`make dropin-swap-test` を毎日 18:00 UTC で develop に対して実行、PR required check 非対象、失敗時 docker compose logs を 14 日 artifact 化する運用を明文化。
- **2026-04-21**: Section 3 に drop-in e2e Phase 13-2 関連の Makefile target (`make dropin-mk-up` / `dropin-mk-test` / `dropin-mk-down` / `dropin-swap-test`) を追加 (#367)。
- **2026-04-21**: Section 3 に drop-in e2e Phase 13-1 関連の Makefile target (`make dropin-up` / `dropin-test` / `dropin-down`) を追加 (#365)。Section 1 の Tests 配下にも testcontainers-go 周りの拡張ポインタを追記。
- **2026-04-20**: Section 8 の `test`ジョブを 4-way matrix shard 化として書き換え (`test-shards` 4 並列 + `test` aggregator)。総実行時間を約4.7分→約1.5-2分に短縮。各shardは独立サービスコンテナで動作し、ImportPath順modulo分配で決定的にパッケージを割り当てる。
