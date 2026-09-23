# プロジェクトの構成の再編 (正式改名・frontend の組み込み・テスト配置の整理)

**Status**: Draft (#3180、2026-09-24) / **Scope**: リポジトリ全体の構成、改名の範囲、本家 Misskey への追従方式

未決事項 (末尾) が残っているので、段階を進めるごとにこの文書を更新する。作業の進み具合は #3180 と、段階ごとの sub-issue で管理する。

---

## 目的

仮称「mk-go」を正式な名前に改め、同じ機会に次の 2 つの構成を見直す。

1. 同梱 frontend を本体リポジトリへ組み込み、`third_party/` をなくす
2. テスト関連の配置を整理する

名前が変わるとモジュールパス・リポジトリ名・イメージ名・パスがほぼ全て動くので、構成の見直しを別の機会に分けると同じ箇所を 2 度書き換えることになる。

## 要件

### R1. 改名

| 対象 | 扱い |
|---|---|
| nodeinfo `software.name`、User-Agent | 新しい名前にする |
| Go のモジュールパス (`github.com/shiroha-a/mk`)、プラグインのモジュール名 (`github.com/shiroha-a/mk-plugin-*`) | 新しい名前にする。独立リポジトリのプラグイン 4 つ (fedwatch / genshin / hsr / nowplaying) も追従させる |
| GitHub のリポジトリ名 (`shiroha-a/mk`) | 新しい名前にする (GitHub が旧 URL から転送する) |
| 配布イメージ (`ghcr.io/shiroha-a/mk`、`-bundled`) | 新しい名前にする。旧名での配布の猶予は未決 |
| 実行バイナリ名 (`cmd/misskey`、`built/misskey`) | 新しい名前にする |
| frontend のページ名 (`/about-mkgo` など) と画面の文言 | 新しい名前にする。旧 URL は転送する |
| ドキュメント | 新しい名前にする。CHANGELOG と CLAUDE.md の更新記録の過去の記述は書き換えない |
| 関数名などコード上の識別子 | 据え置き |
| `/api/meta` の `mkGo*` (`mkGoVersion` / `mkGoCommit` / `mkGoFrontendVersion` / `mkGoPlugins` / `mkGo`) | 据え置き (同梱 frontend と外部クライアントが読む wire の項目) |
| 環境変数の接頭辞 `MK_` | 据え置き (運営者の設定を壊さない) |

### R2. frontend の組み込み

- fork (`shiroha-a/misskey-ts`) の中身を本体の `frontend/` へ取り込み、以降は本体のコミットとして編集する
- fork リポジトリはアーカイブする。assets イメージは本体の workflow でビルドする
- `third_party/` をなくす。本家は git 管理外で必要なときだけ取得する
- 本家への追従を 1 コマンドで回せるようにする (版間の差分のうち frontend 側だけを 3-way で当てる)
- frontend の版は本体の版に揃える。`-mk.N` のタグと pin まわりの仕組みは廃止する
- frontend と backend を 1 つの PR / 1 つのコミットで変更できること

### R3. テスト関連の配置

- `test/` (Go の e2e) を `tests/` へまとめる
- 検証用の compose を各スイートの中へ移し、全てに `name:` を付ける (本番 project `mk` への合流を防ぐ)
- ベンチを `tests/bench/` の下にまとめ、名前の揺れを揃える
- リポジトリ直下には運営者向けの compose (`docker-compose.yml` / `docker-compose.image.yml` / `compose.uds.yaml.example`) だけを残し、`docker-compose.yml` にも `name:` を付ける

### 非機能要件

- 各段階の PR は単体で build / test が通り、本番 (UDS) を止めずに移行できること
- rebase and merge の方針どおり、各コミットが単体でビルドできること
- 本家への追従の手間が今 (rebase 1 回) より大きく悪化しないこと。試算で確かめる

## 現状の把握 (2026-09-24 時点)

数え方: 参照ファイル数は `git grep -l 'third_party' -- ':!third_party' | wc -l`、テストは `git grep -l 'third_party/misskey' -- '*_test.go'`、workflow は `grep -l 'submodules:' .github/workflows/*.yml`。fork の独自コミットは `git -C third_party/misskey rev-list --count --no-merges 2026.9.1..2026.9.1-mk.0` (151 個、153 ファイル / +18,523 / -374)。

### `third_party/misskey` の参照 (109 ファイル) は 3 種類に分かれる

| 種類 | 参照元 | 移行先 |
|---|---|---|
| **自分たちの frontend** | `internal/frontendutil` の既定値 (`frontendBase = "third_party/misskey"`、`MISSKEY_FRONTEND_*` で上書き可)、`Dockerfile.bundled` (`built/`・`packages/frontend/assets`・`assets/`)、`compose.uds.yaml.example` の bind mount (`./third_party/misskey/built:/frontend`)、`tools/pluginbuild` / `plugindev` (`packages/frontend/src/server-plugins.generated.ts` などを書く)、`make frontend-check` / `frontend-test`、Playwright のビルド | `frontend/` |
| **比較対象の本家** | tools 7 つ (`apicompat` / `erroriddiff` / `limitspec` / `permspec` / `securespec` (以上 `packages/backend/src/server/api/endpoints`)、`schemadrift` (`packages/backend/migration`・`src/models`)、`shapediff` (`packages/misskey-js/src/autogen/types.ts`))、テスト 9 ファイル、本家 backend e2e (`tests/upstream-e2e`)、promo の確認 | `.cache/misskey/<版>/` (git 管理外) |
| **本家の backend パッケージから借りているもの** | `Dockerfile` / `Dockerfile.bundled` が `packages/backend/assets` (favicon・アイコン = `static-assets`) と `packages/backend/node_modules/@misskey-dev/emoji-assets` (twemoji / fluent-emoji) を焼き込む | 下の D3 で決める |

### submodule を checkout している workflow (10 本)

`ci.yml` (frontend-check)、`docker.yml`、`diff-e2e.yml`、`apicompat.yml`、`queue-bench-smoke.yml`、`dropin-frontend-e2e.yml`、`playwright.yml`、`upstream-backend-e2e.yml`、`dropin-e2e.yml`、`build-with-plugins.yml`

### テスト関連の配置

- `test/`: `e2e`、`e2e_federation` (Go)
- `tests/`: `bench` / `diff` / `dropin` / `dropin_frontend` / `federation` / `playwright` / `plugin-doc` / `queue-bench` / `queue-bench-autoscale` / `resource-bench` / `upstream-e2e`
- 直下の compose 12 個のうち検証用が 9 個。`name:` が無いのは `docker-compose.yml` と overlay 3 個 (`dropin-frontend.mk` / `dropin.mk` / `playwright.ts`)

## 設計

### D1. 目標の構成

```
<リポジトリ>/
├── cmd/ internal/ plugin/ tools/ migration/ ...   (Go の backend は直下のまま)
├── frontend/                  ← 本家の monorepo から backend を除いた部分 (pnpm workspace)
│   ├── package.json / pnpm-workspace.yaml / pnpm-lock.yaml / .node-version / build.ts / scripts/
│   ├── packages/frontend, frontend-shared, frontend-embed, sw, i18n, misskey-js, ...
│   ├── locales/ / assets/
│   └── built/                 ← ビルド成果物 (gitignore。本番はここを bind mount)
├── assets/                    ← 本家 backend から借りていた静的アセット (D3)
├── tests/                     ← R3
├── deploy/
├── UPSTREAM_MISSKEY_VERSION   ← 追従している本家の版 (例: 2026.9.1)
└── .cache/misskey/<版>/        ← 本家そのもの。gitignore。make upstream-fetch で取得
```

- **Go は直下に残す。** 本家に倣って `backend/` へ移す案もあるが、Go のパスが全て変わるうえ得るものが少ない。
- **frontend に取り込む範囲は「本家の monorepo から `packages/backend` を除いたもの」**。画面本体 (`packages/frontend`) だけでは動かず、`misskey-js` / `frontend-shared` / `sw` / `i18n` / `locales` などを workspace として持つ必要がある。取り込む範囲の正確な一覧は P1 の試算で決める。

### D2. 本家の参照 (`.cache/misskey`)

- `UPSTREAM_MISSKEY_VERSION` に追従している本家の版を 1 行で書く (submodule の gitlink の代わり)
- `make upstream-fetch` がその版を `.cache/misskey/<版>/` へ取得する。tools とテストは環境変数 (例 `MK_UPSTREAM_DIR`、既定 `.cache/misskey/$(cat UPSTREAM_MISSKEY_VERSION)`) で場所を受け取る
- golden は既にコミット済みの testdata なので、本家のソースが要るのは追従時の再生成、本家を読むゲート、本家 backend e2e、apicompat だけ
- CI で本家を読む job は `actions/checkout` で `misskey-dev/misskey` を `UPSTREAM_MISSKEY_VERSION` の ref で `.cache/misskey/...` へ取得する
- **本家を読むテストは「無ければ skip」の形を保ち、CI では skip を禁じる** (今の `MK_FRONTEND_GATES_REQUIRE_SUBMODULE` と同じ形)。skip が成功扱いになる問題 (#2892) を持ち込まない
- 取得方法 (毎回 shallow clone か、共有の bare mirror から worktree 展開か) は未決 (Q3)

### D3. 本家 backend から借りているアセット

- `packages/backend/assets` (favicon・アイコン等): リポジトリ直下の `assets/` (名前は要検討) に置き、追従時に本家の差分を当てる対象に含める
- `@misskey-dev/emoji-assets`: frontend の workspace の依存として持つ (本家では backend の依存だが、使うのは画像ファイルだけ)。Dockerfile は `frontend/node_modules/...` から取る
- `.dockerignore` の再包含の記述 (pnpm の実体側を再包含している) も新しいパスに合わせる

### D4. 本家への追従 (`make upstream-sync`)

1. 本家の objects を手元に取る (`git fetch upstream-misskey --tags`、remote は本体リポジトリに追加するが branch は作らない)
2. `git diff <旧版> <新版> -- <frontend 側のパス>` を `git apply --3way --directory=frontend` で当てる。`packages/backend/assets` は `assets/` へ当てる
3. 衝突はファイル単位で衝突マーカーとして残る。解いてコミットし、`UPSTREAM_MISSKEY_VERSION` を上げる
4. backend 側の変更は今と同じく triage して Go に移植する (docs/upstream-catch-up.md)

- 今の fork の rebase (`rebase --onto`) と比べて、衝突の解き方が「コミット単位」から「ファイル単位」になる。独自変更の一覧は git の履歴ではなく `docs/divergence.md` §4-2 で保つ
- **この方式で 2026.9.0 → 2026.9.1 がどう当たるかを P1 で試算してから確定する**

### D5. 版と表示

- frontend の版 = 本体の版。`mkGoFrontendVersion` は「本体の版 + 追従している本家の版」(例 `1.5.0 (misskey 2026.9.1)`) を想定。表示先 (`/about-mkgo`、更新ダイアログの判定) が何を前提にしているかを P4 で確認する
- `-mk.N` のタグ、`submodulepin-check`、`bundled_assets_pin_test`、`docs/divergence.md` の pin 行は廃止または置き換える
- `docs/divergence.md` §4-2 (独自変更の一覧) は tag 列を PR 番号に置き換える

### D6. テスト関連の配置

```
tests/
├── e2e/              ← test/e2e
├── e2e-federation/   ← test/e2e_federation (Go のパッケージ名は据え置き)
├── playwright/       ← compose.yml / compose.ts.yml
├── diff/             ← compose.yml
├── dropin/           ← compose.yml / compose.mk.yml / compose.fedibird.yml
├── dropin-frontend/  ← dropin_frontend を改名、compose をここへ
├── federation/       ← compose.misskey.yml
├── upstream-e2e/
├── plugin-doc/
└── bench/
    ├── http/ (bench)  queue/ (queue-bench)  queue-autoscale/  resource/
```

- CI のカバレッジ閾値は ImportPath の `/e2e` の部分一致なので、`tests/e2e` / `tests/e2e-federation` でも 0% 例外が続く (確認は P2 で)
- compose の相対パスはファイルの置き場所が基準になるので、移すと build context と bind mount がずれる。`--project-directory` で基準をリポジトリ直下に固定するか、パスを書き換えるかを P2 で決める
- overlay にも `name:` を付けるか、Makefile からしか起動しない前提にするかも P2 で決める

### D7. 改名 (名前が決まってから)

- モジュールパスの変更は `go mod edit -module` と import の一括置換。プラグインの公開パッケージ (`plugin/`) の import パスも変わるので、プラグインの作者向けに移行の案内を書く
- リポジトリ名の変更は GitHub の転送に任せるが、`go get` の旧パスは転送されない (モジュールパスは go.mod の宣言が正)
- 配布イメージは旧名でもしばらく publish するか (猶予) を決める
- nodeinfo / UA の変更は連合先の一覧に出る名前が変わる。CHANGELOG の Note に書く

## 段階 (sub-issue の単位)

名前が決まらなくてもできる段階を先に進める。

| 段階 | 内容 | 名前に依存 |
|---|---|---|
| P1 | 追従方式の試算と、取り込む範囲の確定 | しない |
| P2 | テスト関連の配置の整理 (D6) | しない |
| P3 | 本家の参照を `.cache/misskey` へ分離 (D2)。この時点では frontend はまだ submodule のまま | しない |
| P4 | frontend の取り込み (D1 / D3 / D4 / D5)、submodule と fork の廃止 | しない |
| P5 | 正式な名前の決定 | — |
| P6 | 改名 (D7) | する |
| P7 | ドキュメント・CLAUDE.md の整理 | する |

- P3 を P4 より先に分けるのは、「本家を読む側」と「自分たちの frontend を読む側」を別々に切り替えて、壊れたときにどちらが原因か分かるようにするため
- 各段階は本番 (UDS) の更新手順 (`make uds-update`) を壊さないこと。P4 は本番の compose (gitignore されたローカルの `compose.uds.yaml`) の bind mount の書き換えが要るので、移行手順を書いてから行う

## 未決事項

- Q1. 正式な名前 (P5)
- Q2. fork の履歴を持ち込むか (`git subtree add`、独自コミット 151 個) か、スナップショットとして取り込むか
- Q3. 本家の取得方法 (毎回 shallow clone / 共有の bare mirror + worktree)
- Q4. 旧名の配布イメージの猶予期間
- Q5. 旧 URL (`/about-mkgo` など) の転送を残す期間
- Q6. `assets/` (D3) の名前と置き場所
- Q7. `mkGoFrontendVersion` の新しい形式 (D5)

## リスク

- **本番の更新手順が変わる。** bind mount の元 (`third_party/misskey/built`) と、`uds-frontend-build` の出力先が変わる。本番を触る hazard (ビルドが配信物を消してから作る、i18n のビルドが本番の locales へ書く) は新しいパスでも同じなので、Makefile と memory の記述を合わせて更新する
- **独立リポジトリのプラグイン 4 つ** は P6 でモジュールパスが変わると import を直すまでビルドできない。P6 と同時にそれぞれ更新する
- **追従の手間が増えうる。** P1 の試算で悪化の度合いを確かめ、許容できなければ D4 を見直す
