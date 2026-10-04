# コントリビューション

## ワークフロー

1. **Issueを作成** — すべての作業は対応するissueを先に作成してから着手する
2. **ブランチを切る** — `develop`から`feature/<issue番号>-<要約>`または`fix/<issue番号>-<要約>`
3. **実装 + テスト** — カバレッジ90%以上を維持
4. **PR作成** — `gh pr create`で作成し、`Closes #<issue番号>`を本文に記載

## Issue

- タイトルは、何が起きているか (バグ) か、何をするか (機能) を 1 文で書く。番号や接頭辞は付けない (例: `リモート絵文字をその場からインポートすると、ライセンスが空で上書きされる`)
- 大きな作業は、親 issue と段階ごとのサブ issue に分け、サブ issue のタイトルに `(1)` `(2)` のように段階を付ける (例: `バブルゲームの対戦 (1): エンジンにおじゃま石と攻撃の計算を足す`)
- 本文に含める項目: 背景・目的、実装詳細、影響範囲、完了条件、関連
- 以前は `Phase〇 <内容>` の形式を使っていた (#3248 で廃止)

## ブランチ

| ブランチ | 役割 |
|---|---|
| `main` | リリース |
| `develop` | 開発統合 |
| `feature/<issue番号>-<要約>` | 機能追加 |
| `fix/<issue番号>-<要約>` | バグ修正 |

## Pull Request

- タイトル: issue のタイトルか、作業の要約
- 本文に含める項目:
  - **Summary**: 変更の概要と目的
  - **主な変更点**: 変更ファイルの要約
  - **テスト**: 追加したテスト、実行方法
  - **Closes**: `Closes #<issue番号>`

## コミット前チェック

```bash
make check  # fmt → lint → actionlint → golangci-lint → test
```

個別に回すなら `make fmt` / `make lint` / `make actionlint` / `make golangci-lint` / `make test`。
テストは `-race -count=1 -shuffle=3` で CI と同じ実行条件。

CIで`gofmt`差分チェック、`go vet`、actionlint、golangci-lint、カバレッジ閾値チェックが走る。

PR を出すと十数個の check が走る。**required なのは `build` / `test` / `lint` の 3 つだけ**で、
残りは非ブロッキング。どれが何を見ていて落ちたとき何を疑うかは [CI で回る項目](ci.md) に
まとめてある。

## fork frontend (`frontend/`) を触るとき

mk-go 1.0 以降は fork frontend を独自に進化させる。Go 側の `make check` だけでは
frontend の規約違反を拾えないので、`frontend/` を変える PR では以下も確認する。

### 変更の置き場所

frontend は #3379 で Misskey TS の fork (submodule `third_party/misskey`) から
本体の `frontend/` (pnpm workspace) へ取り込んだ。frontend の変更は `frontend/` を
直接直し、Go 側の変更と同じ PR に入れてよい。fork 側へ commit して gitlink を
上げる手順は要らない。

### 手元での確認（CI `frontend-check` job 相当）

`make frontend-check` は**型 (`vue-tsc`) + `frontend/` のソースを読むゲート + eslint** まで (#2892 / #2906)。job 全体はさらに vitest と `make plugins-all` / 統合バイナリの build も走る。下のブロックが eslint を再度呼ぶのは、CI も別 step (`Lint (eslint)`) で回しているのを揃えているため。

`make plugins-all` は workspace のビルドより先に回す。生成物の
`server-plugins.generated.ts` は git で追跡していない (#3379) ので、無いと
frontend のビルドが import で落ちる。Node の版は `frontend/.node-version` に揃える。

```bash
make plugins-all && go build -o /dev/null ./cmd/misskey   # CI と同じ統合ビルド
cd frontend && pnpm install && pnpm build-pre && pnpm -r build && cd ..
make frontend-check
cd frontend/packages/frontend && pnpm eslint && cd ../../..
make frontend-test
```

**本番を動かしているチェックアウトでは `pnpm -r build` / `pnpm build` を流さないこと。** frontend 自身のビルドが `frontend/built` を消してから作り直すので、そこを bind mount している本番が 404 になる (#3379 の切り替え後)。手元の検証は別の worktree で行う。

`make uds-frontend-build` / `make e2e-frontend-build` は本番の `built/` を書き換えるので
検証には使わない ([development.md](development.md))。

### コーディング規約（fork frontend）

| 規約 | 詳細 |
|---|---|
| i18n | パラメータ付き文字列は `i18n.tsx._key.func({ n })`。`i18n.t()` は `@deprecated`（`ts` / `tsx` 直接参照のほうが Vue のキャッシュ効率が良い） |
| import | 型は top-level の `import type { Foo } from '...'`。値 import 内の `type Foo` は eslint `import/consistent-type-specifier-style` で落ちる |
| vitest | `vitest.config.unit.ts` の include は `test/unit/**/*.test.ts` のみ。**`src/` 直下の `.test.ts` は CI でも手元でも実行されない** |

Playwright spec を足すときは [playwright.md](playwright.md) の selector 規約も読む
（位置依存の `querySelector` はフォーム項目が増えると壊れる）。

## ドキュメントを直すときのレビュー条件

**doc の誤りを直す作業は、直した先で新しい誤りを作りやすい。** #2640 では敵対的
レビューを 7 周回して、**毎周の High がすべて「直前の修正が作った回帰」**だった。

出た High は 14 件 (各周のレビュー結果の `## High` 配下の項目数を足したもの)。
数種類の型に収まるので、以下を確認する。

| 型 | 件数 |
|---|---|
| 1. 同じ主張が他に残っている (片側更新) | 4 |
| 2. 裏取りせず書いた | 3 |
| 3. 既存の反証を読んでいない | 2 |
| 4. 数え方が未定義 / 数え違い | 3 |
| 5. 識別子の出どころを取り違えた | 1 |
| 6. 直した結果が元より危険側 | 1 |

自分の変更にも、他人の PR をレビューするときにも同じように使う。

### 1. 同じ主張が他に残っていないか

**最も多い型 (14 件中 4 件)。** 1 箇所を直して、同じことを言っている別の場所を
残す。直した記述の**固有の語**で doc とコードを横断 grep する。

```bash
git grep -n "<直す前の固有の語>"
```

**`git grep` を使う。** ディレクトリを列挙すると必ず落とす — #2640 では
`Makefile` の help、`.github/workflows/`、`deploy/uds/Dockerfile.mkgo` が
それで取り残された。`git grep` は tracked 全体を見て `.git` と生成物を自動で外す。

実例:

- テストを改名して、旧名が **2 つの doc に残った**。説明の陳腐化まで数えると
  4 箇所。しかも**改名の理由だった数え違いを新しく書いた**
- 撤回した主張が `CLAUDE.md` の「更新記録」に太字で残り、同じファイルの本文と
  真逆のことを言っていた
- 「生成物は 58 だった」を 1 箇所だけ直し、**その gate 自身の GoDoc を含む 3 箇所**に
  残した

コード側のコメントも対象。doc だけ直してコメントを残すと、次に読む人が
コメントを信じる。

### 2. 書いたものを実行または grep で確かめたか

**推論で書かない (14 件中 3 件)。** 以下は必ず実物に当たる。

| 書くもの | 確かめ方 |
|---|---|
| 数値 | 数えるコマンドを実際に流す |
| 識別子 (関数 / 型 / テスト名) | `grep -rn "<名前>"` で実在を確認 |
| ファイルパス / バイナリ名 | `ls` で確認 |
| ログ行 | ソースの出力箇所を見る |
| 設定キーの効き方 | **実際に読み込ませて確かめる** |

実例 (いずれも High):

- **実在しない環境変数キーを例として並べた。** `bindEnvKeys()` の登録キーを
  確かめずに「`meilisearch.*` なども登録されている」と書いた
- **設定の効き方を推論で書いた。** それを直した表も「`MK_` で設定できない」と
  書いたが、`AutomaticEnv` があるので**ファイルにそのキーがあれば効く**。
  実際に `config.Load` を叩けば 1 分で分かった
- **package doc と照合しなかった。** `internal/maintenance/` を「起動時に走る」と
  書いたが、package doc 自身が `driven by standalone CLIs under cmd/` と書いており
  サーバー起動経路からは呼ばれない

「コードを読んで挙動を推測した」で止めない。設定の優先順位のように**読んでも
分からないもの**は、小さなプログラムを書いて動かす。

**「存在しない」と書くときは条件も書く。** `./built/migrate` を「存在しない
バイナリ」と書きかけたが、**4 つの Dockerfile がこれを作っている**。正しくは
「`make build` は `./built/misskey` しか作らない」。無条件の否定は、別の文脈
(この場合 docker) で逆に誤導する。

### 3. 既存の反証を読んだか

**書こうとしている挙動を、既にテストが固定していないか (14 件中 2 件)。**

`docs/configuration.md` は長らく「mkq の rate limit は per-Worker なので worker 数で
割って設定せよ」と案内していたが、**同じリポジトリの
`TestServer_RateLimit_BackPressuresDispatch` が最初から反証していた**。

「upstream と同じ」「対応済み」と書くときは、その反対が既に登録されていないかを見る。

- `docs/divergence.md` — 意図的な乖離の一次資料
- コードコメントの `#<issue> L<n>` / `N<n>` — documented limitation の印。
  **`docs/divergence.md` には載っていないものがある** (`grep -rn "#2106 L" internal/`
  が辿り方)

実例: `#2106 L49` としてコード注記で登録済みの「compact を呼ばない」を、新設した
doc が「upstream と同じ順序」と打ち消していた (その後、転送経路は compact するよう
直した。L49 に残っているのは preload 外の context を解決しない点だけ)。

### 4. 数を書くなら数え方も書く

**同じ対象が数え方で 52 / 55 / 63 になる (14 件中 3 件)。** 定義を書かずに数だけ
載せると、次の監査で再現できず「誤り」と判定される。

`/api` の外の登録数はこうなる (`docs/architecture.md` §3.1 に内訳がある)。

| 値 | 数えたもの |
|---|---|
| 52 | `s.echo.[A-Z]+(` の素朴な grep (`OPTIONS` を落とす) |
| 55 | HTTP メソッドの呼び出し (`GET` 45 / `POST` 5 / `OPTIONS` 2 / `Any` 3) |
| 63 | 55 + `Static` 4 + `File` 4 |

```
× ルート登録 (503 + 53)
◯ /api/* 503 (api.POST 466 + api.GET 12 + api.Match 12 × 2 methods + catchall 1)
  + それ以外 55 (s.echo への HTTP メソッド呼び出し。静的配信 8 と /debug/pprof 8 は別)
```

母集団が doc 間で違うのは構わない (`docs/api-compatibility.md` の 9 と
`docs/divergence.md` の 12 は取り方が違うだけ) が、**違うことを両方に書く**。
片方だけに注記を置くと、もう片方から来た人には矛盾に見える。

### 5. 識別子の出どころを取り違えていないか

**ソースのファイル名と wire 上の名前は違う (14 件中 1 件)。**

stream チャンネルの一覧をソースのファイル名 (`chat-room.ts`) から作ったところ、
実際のチャンネル名は `chatRoom` で、**18 件中 11 件が実在しない名前**になった。
しかも同じ文で「名前も upstream に揃えてある」と書いていたので、読者はそのまま
`connect` に渡す。

API のパス / 環境変数名 / queue 名 / エラーコードも同じ。**その名前が実際に
使われている場所**を見る。

### 6. 直した結果が元より危険側になっていないか

**誤りを直したつもりで、より悪い行動を誘発することがある (14 件中 1 件)。**

rate limit の例:

| 版 | 記述 | operator の行動 |
|---|---|---|
| 元 | 「asynq 専用」(誤り) | 設定しない |
| 1 回目の修正 | 「両 driver で効く」 | 正しい値を設定する |
| 2 回目の修正 | 「worker 数で割れ」(誤り) | **128/s を狙って 8 を設定 → 1/16 に絞られる** |
| 現行 | 「per-queue。割らない」 | 狙った値をそのまま設定する |

**誤りは直す。** ただし**元の記述が誤りでも、それを信じた人の行動が安全側な
ことがある**ので、直した版と元の版を「この記述を読んだ人が何をするか」で比べ、
悪化していないことを確かめてから出す。

### 7. 生成物と gate

- `docs/api-compat.md` のような**生成物を手で直さない**。`make apicompat` で再生成する
  (route dump に stack 起動が必要)
- 件数の整合を守る gate は `internal/entitycompat` にある。**gate を足したら、その
  gate を説明している doc の本数も直す** (これも型 1 に当たる)
- gate を足したら**変異させて落ちることを確認する**。落ちない gate は「検査した」と
  誤認させる分だけ有害
- **gate を doc で説明するときは、その gate が検査していない半分も書く。** 7 周の
  Medium / Low で最も繰り返された型がこれ (射程の過大主張・fail-open・偽陽性)。
  手本は `docs/divergence.md` §4-1 の「固定できるのは mk-go 側だけで、
  『upstream は 18』『名前も upstream に揃えてある』は検証していない
  (`test-shards` は submodule を checkout しない)」

## コーディング規約

- `gofmt -s`で整形
- Early returnでネストを浅く保つ
- エラーは`fmt.Errorf("context: %w", err)`でラップ
- GoDocは英語、インラインコメントは日本語

詳細はCLAUDE.md Section 5を参照。

## ライセンス

[GNU AGPL-3.0](../LICENSE)

### Go のソースに SPDX ヘッダーは付けない

AGPL-3.0 が求めるのはライセンス全文を添えること (§4) と、改変の告知 (§5a)、
ネットワーク越しの利用者へのソース提供 (§13) で、**各ファイルのヘッダーは条件では
ない**。GPL の付録 "How to Apply These Terms" が推奨しているだけで、mk-go は
`LICENSE` と README の表記で足りている。

**上流 TS からヘッダーをコピーしないこと。** `SPDX-FileCopyrightText: syuilo and
misskey-project` は upstream Misskey の著作権表示なので、mk-go 自身のコードに
付けると**帰属が逆になる**。実際に `plugin/` の 3 ファイルがその状態だった。

**fork frontend (`frontend/`) は別。** 本体へ取り込んだ後も、上流の SPDX ヘッダーは
そのまま残す。消すのは §4 の「既存の告知をそのまま残す」に反する。AGPL 管轄
ディレクトリへ新規ファイルを足すときも、`frontend/scripts/check-spdx.mjs`
(CI の `frontend` workflow が回す) が落とすのでヘッダーが要る。`tools/pluginbuild`
が生成する `server-plugins.generated.ts` がヘッダーを持つのも同じ理由で、あれは
Go 側の方針の例外ではなく `frontend/` 側の要件。

## 変更の経緯 (旧 CLAUDE.md の更新記録)

CLAUDE.md の「更新記録」に書かれていた本文を、#3248 でここへ移した。**記述は当時のまま**で、文中の「Section N」は当時の CLAUDE.md の節を指す。新しいものが上。

- **2026-08-20**: Section 7 に「ドキュメントを直すときのレビュー条件」を追加 (#2644)。#2637 の完了条件にあった「同じ乖離が再発しにくい仕組み」への回答。本文が候補に挙げていた**静的検査 (存在しない Makefile target / パスの検出) は測ったところ使い物にならなかった** — target は doc 側 107 のうち Makefile に無いのが 3 つで全て grep の取りこぼし (空振りする)、パスは `docs/` と CLAUDE.md / README.md のバッククォート内でスラッシュを含む文字列を拾うと**不在候補が 298 件**で、大半が偽陽性 (API の endpoint パス / CIDR / `internal/` を省いた相対表記)。代わりに #2640 の**敵対的レビュー 7 周で出た High 14 件を型に分類**して確認手順に落とした。最多は**片側更新** (4 件)、次が**裏取りせず書いた** (3 件) と**数え方が未定義 / 数え違い** (3 件)。全文は docs/contributing.md。
- **2026-08-20**: ドキュメント全体監査 (#2637) の残り 94 件を反映 (#2640)。CLAUDE.md 本体では 5 箇所を修正。(1) Section 1 の技術スタック表が Job Queue を **asynq** と書いていた (既定は #571 で mkq。ここを見て実装方針を決めると legacy 側に倒れる)。**`golang-jwt/jwt/v5` は indirect で未使用**、実際に使う `go-webauthn/webauthn` が未記載、JSON-LD は `piprate/json-gold` を直接依存。(2) Section 2 のディレクトリツリーが `...` 無しで閉じているのに、`internal/` 22 のうち 10・`cmd/` 4 のうち 2・トップレベル 7 つが欠落していた。(3) Section 3 に無い target が 76 あったので、**罠のあるものを足したうえで `make help` が全量であることを明記**した (全列挙は腐るので採らない)。`make tidy` はこのリポジトリでは使えない。(4) Section 8 に `docker.yml` (**PR で走る**) / `docker-branch.yml` / schedule の 2 つ、ci.yml の 3 step が無かった。diff-e2e の「43 比較」は pytest 総数で **endpoint 比較は 30**。(5) Section 9 の環境変数表 11 件に対し `bindEnvKeys()` は **86 キー**。登録の有無で変わるのは「**設定ファイルに書かずに env だけで作れるか**」だけで、ファイルにそのキーがあれば未登録でも `MK_` で上書きできる (`AutomaticEnv`)。実務上引っかかるのは example が既定でコメントアウトしている `meilisearch:` と `<queue>JobConcurrency` なので、その条件を明記した。
- **2026-08-04**: Section 7 (Git Workflow) に「マージ方法」を追記。フィーチャーブランチ → `develop` の PR は **rebase and merge** に統一する (それ以前は squash-merge)。各コミットがそのまま develop に載るため、1 コミットずつ build / test が通る順序で並べること、確認は使い捨て `git worktree` で行うこと (作業ツリー上の `git stash` は保留中の別作業を巻き込むので使わない) を併記。`main` は従来どおり PR をマージせず FF push のみで、対象が異なる旨も明記した。
- **2026-06-09**: Section 7 (Git Workflow) に 2 つのルールを追記。(1)「Issue・PR のタイトル・本文は日本語記述を厳守する」(技術用語は原文のまま残してよいが、説明文・見出し・箇条書きの地の文に英語を混在させない)。(2)「`CHANGELOG.md` はリリース時にまとめて記述する」(個別 PR・fix ごとに `## Unreleased` へ追記せず、リリースのタイミングで一括記載する)。
- **2026-04-11**: 初版作成。
