# プロジェクトの構成の再編 (正式改名・frontend の組み込み・テスト配置の整理・プラグインまわりの名前の整理)

**Status**: Draft (#3180、2026-09-24。正式名は 2026-09-30 に決定) / **Scope**: リポジトリ全体の構成、改名の範囲、本家 Misskey への追従方式

未決事項 (末尾) はすべて決まった (2026-09-30)。再編の機会にあわせて行うこと (R6〜R8、D10〜D13) を 2026-10-04 に足した。P1 の試算 (#3370) の結果で D1 / D3 / D4 を確定した (2026-10-04)。段階を進めるごとにこの文書を更新する。作業の進み具合は #3180 と、段階ごとの sub-issue で管理する。

---

## 目的

仮称「mk-go」を正式な名前 **Elythia** に改め、同じ機会に次の 3 つを見直す。

1. 同梱 frontend を本体リポジトリへ組み込み、`third_party/` をなくす
2. テスト関連の配置を整理する
3. プラグインまわりで旧名 (mk / mk-go) を含む名前を Elythia に揃える。**「plugin」という呼び名そのものは変えない** (2026-09-30 決定)

名前が変わるとモジュールパス・リポジトリ名・イメージ名・パスがほぼ全て動くので、構成の見直しを別の機会に分けると同じ箇所を 2 度書き換えることになる。

あわせて、TS Misskey への復路の保証をやめる (#3191、R5)。構成の再編そのものではないが、TS を立てて確かめる e2e の読み先が P3 / P4 で動くことと、運営者にとっての互換性の区切りを改名の版に 1 回でまとめるために、同じ段階の表で扱う。

## 要件

### R1. 改名

**名前は `Elythia` (2026-09-30 決定)。** 置き場所は GitHub organization の `Elythia-Network`。

**機械が読む名前はすべて小文字にする。** 大文字を使うのは画面とドキュメントの表示 (`Elythia` / `Elythia-Network`) だけ。

- GHCR はイメージ名に大文字を受け付けない
- Go のモジュールプロキシは大文字を `!e` のようにエスケープする (`github.com/!elythia-!network/...`)
- GitHub は大文字小文字を区別しないので、小文字のモジュールパスでも同じリポジトリに届く

| 用途 | 名前 |
|---|---|
| 表示 (画面・ドキュメント) | `Elythia` |
| nodeinfo `software.name` | `elythia` |
| User-Agent | `Elythia/<version> (<url>)` |
| GitHub のリポジトリ | `Elythia-Network/elythia` (URL は `github.com/elythia-network/elythia` でも届く) |
| Go のモジュールパス | `github.com/elythia-network/elythia` |
| プラグインのモジュール | `github.com/elythia-network/elythia-plugin-<名前>` |
| プラグインのマニフェスト | `elythia-plugin.yml` |
| nodeinfo のプラグインの宣言 | `metadata.elythiaPlugins` |
| 配布イメージ | `ghcr.io/elythia-network/elythia` (`-bundled` も同じ置き場所) |
| 実行バイナリ | `elythia` (`cmd/elythia`、`built/elythia`) |
| frontend のページ | `/about-elythia` (旧 `/about-mkgo` は転送) |

| 対象 | 扱い |
|---|---|
| nodeinfo `software.name`、User-Agent | `elythia` / `Elythia/<version> (<url>)` にする |
| Go のモジュールパス (`github.com/shiroha-a/mk`)、プラグインのモジュール名 (`github.com/shiroha-a/mk-plugin-*`) | `github.com/elythia-network/...` にする。独立リポジトリのプラグイン 4 つ (fedwatch / genshin / hsr / nowplaying) も追従させる |
| GitHub のリポジトリ (`shiroha-a/mk`) | `Elythia-Network` へ移管し、名前を `elythia` にする (GitHub が旧 URL から転送する。D7) |
| 配布イメージ (`ghcr.io/shiroha-a/mk`、`-bundled`) | `ghcr.io/elythia-network/elythia` にする。**旧名での配布は改名の版で止める** (Q4)。旧名の最後の版と CHANGELOG の Note で移転を案内する |
| 実行バイナリ名 (`cmd/misskey`、`built/misskey`) | `elythia` にする |
| frontend のページ名 (`/about-mkgo` など) と画面の文言 | `/about-elythia` と表示名 `Elythia` にする。**旧 URL は転送しない** (Q5。専用ページをわざわざブックマークする人はいない) |
| ドキュメント | 新しい名前にする。CHANGELOG と CLAUDE.md の更新記録の過去の記述は書き換えない |
| 関数名などコード上の識別子 | 据え置き |
| `/api/meta` の `mkGoVersion` / `mkGoCommit`、`internal/core/procstats` の `mkGo` | 据え置き (同梱 frontend と外部クライアントが読む wire の項目)。nodeinfo の `mkGoPlugins` は連合で使う宣言なので R4 で改める |
| `/api/meta` の `mkGoFrontendVersion` | **廃止する** (Q7、D5)。frontend を本体と同じ版で管理するので、frontend だけの版という概念が要らない |
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
- リポジトリ直下には運営者向けの compose (`docker-compose.yml` / `docker-compose.image.yml` / `compose.uds.yaml.example`) だけを残す。`docker-compose.yml` への `name:` は、named volume の名前が変わり既存の運営者が DB の volume を見失うので、P6 で移行手順と一緒に行う (#3373)

### R4. プラグインまわりの名前を Elythia に揃える

**「plugin」という呼び名は変えない (2026-09-30 決定)。** 当初は Misskey 本家の frontend にある**クライアントプラグイン** (AiScript、`/settings/plugin`) と紛らわしいので語ごと変える案だったが、据え置くことにした。紛らわしさは今と同じく、管理 API の名前空間を分けて避ける (`admin/server-plugins`、`internal/api/admin/server_plugins.go`)。

変えるのは、**旧名 (mk / mk-go) を名前に含むものだけ**。R1 の「機械が読む名前は小文字」に従う。

| 対象 | 今の名前 | 扱い |
|---|---|---|
| マニフェスト | `mk-plugin.yml` | `elythia-plugin.yml` にする。旧名は読まない (Q9、D8) |
| プラグインのモジュール名・リポジトリ名 | `github.com/shiroha-a/mk-plugin-*` | `github.com/elythia-network/elythia-plugin-*` にする (R1 のモジュールパス変更と同時) |
| nodeinfo の宣言 (連合) | `metadata.mkGoPlugins` | `metadata.elythiaPlugins` にする。旧名は出さず、読まない (Q9、D8) |
| 公開パッケージ | `plugin/` (`plugintest` / `peercache` を含む) | 据え置き。import パスは R1 のモジュールパス変更で `github.com/elythia-network/elythia/plugin` に変わる |
| 置き場 | `plugins/` | 据え置き |
| 運営者の設定キー | `plugins.<name>.*` | 据え置き |
| 管理 API | `admin/server-plugins` | 据え置き |
| プラグインごとの DB schema | `plugin_<name>` | 据え置き (移行は要らない) |
| プラグインのジョブキュー | `plugin:<name>` | 据え置き (移行は要らない) |
| 相手サーバーのプラグインを呼ぶ経路 (連合) | `/plugin/<name>/...` | 据え置き |
| ツール・生成物・ドキュメント | `tools/pluginbuild` / `plugindev` / `pluginresolve`、`*plugins.generated.*`、`docs/plugins/` | 据え置き。文面の「mk-go」は R1 のドキュメントの改名で Elythia にする |
| 内部のコード上の識別子 | `pluginstore`、`PluginQueuePrefix` など | 据え置き (R1 と同じ) |

### R5. TS Misskey への復路の保証をやめる (#3191)

- **往路 (TS → この実装) は引き続き保証する。** TS の DB をそのまま引き継ぐための互換は守る
- **復路 (この実装の DB を TS へ渡して戻ること) は保証しない。** 今後の変更に、TS へ戻せることを理由にした制約を課さない
- 復路を確かめる CI (`dropin-e2e` の `mkgo-born`) は残し、今どこまで戻れるかを**測る**用途に変える
- 既に入っている migration は書き換えない (復路のために付けた列や形は残す)
- 運営者への宣言は、改名 (P6) を出す版の CHANGELOG の Note で行う

### R6. 改名の版を 2.0.0 にする

改名の版は **2.0.0** にする (2026-10-04 決定)。モジュールパス・配布イメージ名・nodeinfo の `software.name`・プラグインのマニフェストと宣言・`mkGoFrontendVersion` の廃止・復路の保証をやめる宣言 (R5) が同じ版で変わるので、運営者とプラグイン作者に互換性の区切りを 1 つの番号で伝える。

### R7. 実行バイナリを 1 つにまとめる

**実行バイナリを `elythia` 1 つにし、用途をサブコマンドで分ける** (2026-10-04 決定)。バイナリ名が変わる機会 (R1) にあわせて行う。

- 今の `cmd/` には、本体 (`misskey`)・`migrate`・一回限りの後始末バッチ 5 つ (`backfill-avatar-public-url` / `backfill-emoji-system-file` / `backfill-instance-counts` / `backfill-note-tags` / `backfill-remote-host`) がある (数え方: `ls cmd/`)。バッチを足すたびに Dockerfile 3 つ (`Dockerfile` / `Dockerfile.bundled` / `deploy/uds/Dockerfile.mkgo`) へ build と COPY の行を足している
- 目標の形は `elythia serve` / `elythia migrate` / `elythia backfill <名前>`。本番でのバッチの流し方は `docker exec <container> elythia backfill <名前> -dry-run` に揃う
- 役目を終えたバッチは、まとめるときに残すか撤去するかを決める (例: `backfill-remote-host` は 2026-10-04 の本番の dry-run で差分が 0 件)

### R8. 再編の機会にあわせて整えること

構成が動く段階に寄せて、次の 3 つを行う (2026-10-04 決定)。

- **frontend の CI を required check にする。** 今の `frontend-check` は submodule の都合で required から外している。frontend を本体に取り込めば外す理由が無くなるので、`build` / `test` / `lint` に並べる。frontend の依存の更新も本体の仕組み (Dependabot など) に載せる
- **ライセンスと著作権の表示を整える。** frontend を取り込むと本家 Misskey (AGPL-3.0) のコードが本体に入る。`frontend/` に本家の `LICENSE` / `COPYING` を残し、リポジトリ直下に由来を説明する `NOTICE` を置く
- **`docs/divergence.md` を分ける。** 今は 1 ファイルに REST・連合・MFM・frontend・意図的な安全側の差が混ざっている (1764 行。数え方: `wc -l docs/divergence.md`)。P7 で領域ごとのファイルに分け、読んでいるゲート (`TestDivergenceDoc` など) も合わせて直す

### 非機能要件

- 各段階の PR は単体で build / test が通り、本番 (UDS) を止めずに移行できること
- rebase and merge の方針どおり、各コミットが単体でビルドできること
- 本家への追従の手間が今 (rebase 1 回) より大きく悪化しないこと。P1 (#3370) の試算で、過去 3 回とも同じ量だと確かめた
- 本番 (UDS) の切り替えが要る段階は、手順を先に `docs/deployment.md` に書き、隔離した環境で一度通してから本番に当てること (D13)

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
│   ├── package.json / pnpm-workspace.yaml / pnpm-lock.yaml / .node-version / patches/ / scripts/
│   ├── packages/frontend, frontend-shared, frontend-embed, sw, i18n, misskey-js, ...
│   ├── packages-private/
│   ├── locales/
│   ├── assets/                ← 本家 backend から借りていた静的アセット (D3)
│   ├── repo-assets/           ← 本家の直下の assets/ (ai.png など。D3)
│   └── built/                 ← ビルド成果物 (gitignore。本番はここを bind mount)
├── tests/                     ← R3
├── deploy/
├── UPSTREAM_MISSKEY_VERSION   ← 追従している本家の版 (例: 2026.9.1)
└── .cache/misskey/<版>/        ← 本家そのもの。gitignore。make upstream-fetch で取得
```

- **Go は直下に残す。** 本家に倣って `backend/` へ移す案もあるが、Go のパスが全て変わるうえ得るものが少ない。
- **frontend に取り込む範囲は「本家の monorepo から `packages/backend` を除いたもの」**。画面本体 (`packages/frontend`) だけでは動かず、`misskey-js` / `frontend-shared` / `sw` / `i18n` / `locales` などを workspace として持つ必要がある。一覧は P1 (#3370) で次のとおり確定した。

| 区分 | 本家のパス |
|---|---|
| 取り込む | `package.json`、`pnpm-workspace.yaml`、`.node-version`、`patches/`、`scripts/`、`locales/`、`packages/` (`packages/backend` を除く)、`packages-private/`、`LICENSE`・`COPYING` (D12)、`.gitattributes` (改行を LF に揃える指定。本体には無い)、`.gitignore` (`built/` などの生成物)、`assets/` (`repo-assets/` へ。D3)、`packages/backend/assets/` (`assets/` へ。D3) |
| 作り直す | `pnpm-lock.yaml` (D4) |
| 取り込まない | `packages/backend` (アセットを除く)、`.github/`、`.config/`、`Dockerfile`、`Dockerfile.assets` (fork 独自。P4 で置き換える)、`compose*.yml`、`healthcheck.sh`、`Procfile`、`.dockerignore`・`.dockleignore` (本体の Dockerfile が持つ)、`.gitmodules`、文書類 (`CHANGELOG.md`・`CONTRIBUTING.md`・`README.md`・`CODE_OF_CONDUCT.md`・`ROADMAP.md`・`SECURITY.md`)、AI エージェント向けの設定 (`.agents/`・`.claude/`・`AGENTS.md`・`CLAUDE.md`)、エディタと bot の設定 (`.vscode/`・`.devcontainer/`・`.editorconfig`・`.vsls.json`・`.coderabbit.yaml`・`codecov.yml`・`crowdin.yml`・`renovate.json5`)、`idea/` |

- 取り込んだ `package.json` / `pnpm-workspace.yaml` からは `packages/backend` を外す。直下の `package.json` に残る backend 向けの script (`migrate` など) は動かないが、追従の衝突を減らすため消さずに置く。
- `packages-private/diagnostics-backend` は `packages/backend` を測る道具なので、取り込んでも動かない。P4 で外すか残すかを決める。
- **付け替えに伴って直す参照が 2 系統ある。** どちらも独自変更として残り、追従で衝突しうる。
  - vite の alias `'/static-assets/'` が `../backend/assets/` を指している (`packages/frontend/vite.config.ts` と `packages/frontend-embed/vite.config.ts`)。`frontend/assets/` へ向け直す
  - `scripts/build-assets.mjs` と vite の開発時の設定が `.config/default.yml` を読む。本番のビルドは読めなくても進むが、開発サーバーは本体の設定を読む形に直す

### D2. 本家の参照 (`.cache/misskey`)

- `UPSTREAM_MISSKEY_VERSION` に追従している本家の版を 1 行で書く (submodule の gitlink の代わり)
- `make upstream-fetch` がその版を `.cache/misskey/<版>/` へ取得する。tools とテストは環境変数 (例 `MK_UPSTREAM_DIR`、既定 `.cache/misskey/$(cat UPSTREAM_MISSKEY_VERSION)`) で場所を受け取る
- golden は既にコミット済みの testdata なので、本家のソースが要るのは追従時の再生成、本家を読むゲート、本家 backend e2e、apicompat だけ (`misskey-js` の型の生成に使う `api.json` を本家 backend のビルドで作る場合は、追従時の再生成に含める。P4 で決める)
- CI で本家を読む job は `actions/checkout` で `misskey-dev/misskey` を `UPSTREAM_MISSKEY_VERSION` の ref で `.cache/misskey/...` へ取得する
- **本家を読むテストは「無ければ skip」の形を保ち、CI では skip を禁じる** (今の `MK_FRONTEND_GATES_REQUIRE_SUBMODULE` と同じ形)。skip が成功扱いになる問題 (#2892) を持ち込まない
- **取得方法は手元と CI で分ける** (Q3)。手元は共有の bare mirror を 1 つ持ち、版ごとに worktree で展開する (追従作業で旧版と新版を並べるため。2 回目以降の取得が速い)。CI は今と同じく `actions/checkout` で毎回取る (shallow)

P3 (#3378) で上のとおりにした (2026-10-05)。

- 場所は `internal/upstreamsrc` が返す。**submodule へ戻す経路は作らなかった** — P4 で fork が消えたときに、黙って fork を読んでいたことが隠れるため。本家が無ければ「`make upstream-fetch` を実行する」と出して落ちる
- skip を禁じる変数は `MK_UPSTREAM_REQUIRE`。本家を読むテストは今 `achievement` の 1 本で、`make upstream-check` (golden を本家から作り直して差分が無いこと + このテスト) が立てる。`apicompat` workflow が回す
- 本家 backend e2e の 3 ファイルは `tests/upstream-e2e/harness/` に置き、実行のたびに本家の `packages/backend/` へコピーする。vitest の設定を本家の外に置いたまま走らせる形は、設定ファイルの import が設定ファイルの場所から解決されて失敗するので採らない
- 試算: fork と本家は、比較対象として読むパス (`packages/backend/src` / `migration` / `misskey-js/src/autogen`) で差分が 0。本家から作り直した golden 11 個 (`make shapecheck-gen` が書く全て) は、commit 済みのものとバイト一致した
- `UPSTREAM_MISSKEY_VERSION`・`config.MisskeyVersion`・e2e の `misskey/misskey:<版>` が揃っていることを `TestUpstreamVersionIsConsistent` が見る

### D3. 本家 backend から借りているアセット

- `packages/backend/assets` (favicon・アイコン等): **`frontend/assets/` に置く** (Q6)。本家では backend 側にあるが、配信しているのは画面向けの画像なので実態に合う
- **本家の直下の `assets/` (`ai.png`・バナーなど) は `frontend/repo-assets/` に移す** (2026-10-04、#3370)。本家の monorepo の直下には既に `assets/` があり、Q6 のままでは名前がぶつかる。本体はこれを image の中で `/app/repo-assets` として配信しているので、名前をそれに揃える
- 追従で差分を当てるときのパスの付け替えは、この 2 か所になる (D4)。
- `@misskey-dev/emoji-assets`: frontend の workspace の依存として持つ (本家では backend の依存だが、使うのは画像ファイルだけ)。Dockerfile は `frontend/node_modules/...` から取る
- `.dockerignore` の再包含の記述 (pnpm の実体側を再包含している) も新しいパスに合わせる

### D4. 本家への追従 (`make upstream-sync`)

1. 本家の objects を手元に取る (`git fetch upstream-misskey --tags`、remote は本体リポジトリに追加するが branch は作らない)
2. **変更されたパスを、D1 の 3 区分のどれかに分類する。** どれにも当たらないパス (本家が直下に新しく足したファイルなど) があれば止める。「取り込む」で絞るだけだと、新しいパスが黙って落ちるため
3. 付け替え先ごとに `git diff --binary --no-renames <旧版> <新版> -- <パス>` を取り、`git apply --3way` で当てる。`--directory` は 1 回に 1 つしか指定できないので、3 回に分ける。`--no-renames` で rename をファイルの削除と追加として扱い、付け替えた先で rename の元のパスを探さずに済むようにする
   - `packages/backend/assets/` → `frontend/assets/` (`-p4 --directory=frontend/assets`)
   - `assets/` → `frontend/repo-assets/` (`-p2 --directory=frontend/repo-assets`)
   - それ以外の「取り込む」パス → `frontend/` の下の同じパス (`--directory=frontend`)。pathspec に `':(exclude)packages/backend'` を添える。添えないと backend の差分が `frontend/packages/backend/` へ新しいファイルとして当たる
4. `pnpm-lock.yaml` は当てる対象から外し、**直前の `frontend/pnpm-lock.yaml` を基点に** `frontend/` で `pnpm install --lockfile-only` を実行して作り直す。本家の lock は使わない。backend を外した lock は本家の lock より約 4800 行少なく (2026.10.0 で実測)、本家の lock の差分は毎回衝突するため。`--lockfile-only` は lock に無い依存と範囲が変わった依存をその時点の最新に解決するので、本家が試した組とずれうる。作り直した lock の差分を目視し、`package.json` で変わった依存以外が動いていないことを確かめる
5. 衝突はファイル単位で衝突マーカーとして残る。解いてコミットし、`UPSTREAM_MISSKEY_VERSION` を上げる
6. backend 側の変更は今と同じく triage して Go に移植する (docs/upstream-catch-up.md)

- 今の fork の rebase (`rebase --onto`) と比べて、衝突の解き方が「コミット単位」から「ファイル単位」になる。独自変更の一覧は git の履歴ではなく `docs/divergence.md` §4-2 で保つ
- 取り込まないパスを絞り込みで外すのは、本家が版ごとに `.github/workflows` などを変えるため (2026.9.1 では `.github/workflows` だけで 24 ファイル)。外さないと、無いファイルへの差分として当たらない。
- **P1 (#3370) の試算:** 過去 3 回の追従を scratch で再現し、今の方式 (独自コミットを 1 件ずつ当て直す) と比べた。どちらも衝突の量は同じで、衝突したファイル以外は実際の `-mk.0` の木 (backend を除く) と完全に一致した。試算では `packages/backend` 以外の差分をまとめて当てた。D1 の「取り込む」パスだけに絞ると、当てるファイルはさらに減る (表の「うち「取り込む」パス」の列)

| 追従 | 本家の差分 (backend 以外、`.github` なども含む) | うち「取り込む」パス | 独自コミット | 今の方式 | D4 の方式 |
|---|---|---|---|---|---|
| 2026.7.0 → 2026.9.0 | 138 ファイル | 105 ファイル | 50 | 1 回止まる、1 ファイル | 1 ファイル、1 か所 |
| 2026.9.0 → 2026.9.1 | 45 ファイル | 19 ファイル | 151 | 衝突なし | 衝突なし |
| 2026.9.1 → 2026.10.0 | 20 ファイル | 16 ファイル | 170 | 1 回止まる、3 ファイル | 3 ファイル、5 か所 |

- 数え方: ファイル数は `git diff --name-only <旧版> <新版>` を、`packages/backend` を除いて (2 列目)、または D1 の「取り込む」パスに絞って `--no-renames` で (3 列目) 数えた。`packages/backend/assets` の変更は 3 回とも 0。独自コミットは `git rev-list --count --no-merges <旧版>..<その版の最後の -mk タグ>`

### D5. 版と表示

- **frontend の版 = 本体の版。`mkGoFrontendVersion` は廃止する** (Q7)。読んでいるのは同梱 frontend の `/about-mkgo` の表示だけで (2026-09-30 に確認)、更新ダイアログの判定には使っていない (`check-client-update.ts` のコメントも「fork のタグでは判定できない」として使っていない)。追従している本家の版は `/api/meta` の `version` に既に出ている。ビルド時に埋める `MkGoFrontendVersion` の ldflags (`Makefile` と `Dockerfile` / `deploy/uds/Dockerfile.mkgo`) と、`tests/diff` の除外も合わせて消す
- `-mk.N` のタグ、`submodulepin-check`、`bundled_assets_pin_test`、`docs/divergence.md` の pin 行は廃止または置き換える
- `docs/divergence.md` §4-2 (独自変更の一覧) は ~~tag 列を PR 番号に置き換える~~ **#3379 で取り込むまでの記録として凍結し、取り込んだ後の変更は PR 番号を鍵にした新しい節 (§4-2b) に書く** (P4d-2 で変更)。143 行の tag を PR 番号へ置き換えると対応表を作る手間が大きく、アーカイブした fork の tag との対応も失われるため

### D6. テスト関連の配置

```
tests/
├── e2e/              ← test/e2e
├── e2e-federation/   ← test/e2e_federation (Go のパッケージ名は据え置き)
├── playwright/       ← compose.yml / compose.ts.yml
├── diff/             ← compose.yml
├── dropin/           ← compose.yml / compose.mk.yml / compose.fedibird.yml
├── dropin-frontend/  ← dropin_frontend を改名、compose をここへ
├── federation/       ← compose.misskey.yml / compose.mastodon.yml
├── upstream-e2e/     ← compose.yml
├── plugin-doc/
└── bench/
    ├── http/ (bench、compose.yml)  queue/ (queue-bench、compose.yml)  queue-autoscale/  resource/
```

ファイル名は 2 つだけ据え置いた。`queue-autoscale/docker-compose.yml` は `cd` して `-f` 無しで起動し、同じ場所に `docker-compose.override.yml` を書き出すため。`resource/compose.ts.yaml` は単体で使う TS 側の構成で、呼び出し側の名前を変える利点が無いため。

P2 (#3373) で上のとおりにした (2026-10-04)。

- CI のカバレッジ閾値は ImportPath の `/e2e` の部分一致なので、`tests/e2e` / `tests/e2e-federation` でも 0% 例外が続く。shard の割り当ても変わらないことを確かめた
- **compose の相対パスは書き換えた** (`--project-directory` で基準をリポジトリ直下に固定する案は採らない)。`tests/` の下の既存の compose と書き方が揃い、`--project-directory` の付け忘れ (bind mount が空のディレクトリとして作られる) と、`name:` の無いファイルの project 名が `mk` に戻る危険を避けるため。移す前と後で `docker compose config` の解決結果が同じことを、使っている組み合わせごとに確かめた
- **overlay にもベースと同じ `name:` を付けた**
- これらは `make compose-check` のゲート (`internal/entitycompat/test_compose_paths_test.go`) が固定する: `tests/` の下の compose が `name:` を持ち `mk` でない、overlay の `name:` がベースと同じ、独立した compose 同士で重ならない、相対パスが git で追跡されたものを指す、直下の compose が運営者向けの 3 つだけ

### D7. 改名

- **リポジトリは作り直さずに移管 (transfer) する。** issue・PR・スター・履歴が残り、旧 URL から GitHub が転送する。移管と同時に名前を `mk` から `elythia` に変える。移管の後は、手元の `origin` の URL と、CI / workflow に書いたリポジトリ名を直す
- **fork frontend (`shiroha-a/misskey-ts`) は移管しない。** P4 で本体の `frontend/` へ取り込んでアーカイブするため
- モジュールパスの変更は `go mod edit -module` と import の一括置換。プラグインの公開パッケージ (`plugin/`) の import パスも変わるので、プラグインの作者向けに移行の案内を書く
- リポジトリ名の変更は GitHub の転送に任せるが、`go get` の旧パスは転送されない (モジュールパスは go.mod の宣言が正)
- 配布イメージは**旧名での publish を改名の版で止める** (Q4)。旧名の最後の版に移転の案内を載せ、CHANGELOG の Note にも書く
- nodeinfo / UA の変更は連合先の一覧に出る名前が変わる。CHANGELOG の Note に書く

### D8. プラグインまわりの名前の移行 (R4)

呼び名を変えないので、**保存されたデータ (DB schema・ジョブキュー) の移行は要らない。** 移すのは、旧名を含む名前のうち運営者とプラグイン作者と連合先に見えるものだけ。**旧名を読む猶予期間は設けない** (Q9)。代わりに、旧名のまま上げた運営者が黙って壊れないようにする。

- **マニフェスト** (`mk-plugin.yml` → `elythia-plugin.yml`): 旧名は読まない。**ただし旧名だけがあるディレクトリは、黙って skip せずビルドエラーにする** (新しい名前へ変えるよう案内する)。黙って skip すると、プラグインが組み込まれていない image が緑で出来る (#2940 で `disabled: true` について踏んだのと同じ形)
- **nodeinfo の宣言** (`metadata.mkGoPlugins` → `metadata.elythiaPlugins`): 旧名は出さず、読まない。**その間、旧版のままの相手とはプラグインどうしの連合が止まる** (相手は新しい key を知らないので、こちらを対応サーバーと見なさない)。プラグインの連合の経路 (`/plugin/<name>/...`) は変わらないので、相手が上げれば戻る。CHANGELOG の Note に書く
- **モジュール名** (`mk-plugin-*` → `elythia-plugin-*`): R1 のモジュールパス変更と同時に行う。独立リポジトリのプラグイン 4 つも同じ段階で追従させ、プラグイン作者向けの移行の案内 (D7) にまとめる

### D9. 復路の保証をやめる (R5)

- **`mkgo-born` の失敗の扱いを変える。** これまでは失敗したら設計を直して復路を守っていた。今後は、意図的な変更で失敗したら設計を戻さずにシナリオの期待値を更新し、**何が戻らなくなったか**を `docs/migration-from-ts.md` に記録する。記録は「今の版で TS へ戻したとき、何が残り何が失われるか」を運営者が確かめられる形にする
- **P3 / P4 より前に済ませる。** P3 と P4 では TS を立てる e2e (`mkgo-born` / `swap-test`) の読み先も動く。復路を保証したままだと `mkgo-born` を落とさないことが P3 / P4 の完了条件になり、壊れたときに「構成を変えたせいか、復路の互換が崩れたせいか」を毎回切り分けることになる。先に測る運用へ変えておけば、P3 / P4 では記録するだけで済む
- **復路を理由にした設計の記述を仕分ける。** model / api / repository のコメントと migration の注記のうち、往路にも要るものは残し、復路だけのためのものは実態に合わせて書き直す
- **宣言は P6 の版に載せる。** 作業は先に develop へ入れるが、「TS へ戻せることを保証しない」を運営者に告げる CHANGELOG の Note は、改名を出す版に載せる。運営者にとっての互換性の区切りを 1 回にまとめる

### D10. リポジトリの移管を独立した段階にする

**移管 (D7) は、コードの改名 (P6) より前に単独で行う** (2026-10-04 決定)。移管で動くものが多いので、コードの改名と同じ日にまとめると、壊れたときに原因を切り分けにくい。先に移管だけを済ませ、GitHub の旧 URL からの転送で今のコードがそのまま動くことを確かめてから、P6 で名前を変える。

移管の前後で確かめるもの:

- GHCR の package の置き場所と権限 (配布イメージの publish が新しい organization で通るか)
- Actions の secrets / variables、branch protection と required check、CodeQL
- 運営者のリポジトリから呼ばれている reusable workflow (`build-with-plugins`) の参照先
- 手元の `origin` の URL と、`gh` の呼び出しを本家に飛ばさないためのフック (`--repo` の指定)
- 独立リポジトリのプラグイン 4 つと、それらが本体を参照している箇所

### D11. 実行バイナリのサブコマンド化 (R7)

- `cmd/elythia` に 1 つの main を置き、今の各 `cmd/*` の main は関数として呼ぶ形にする。flag は今と同じものをサブコマンドの flag として受ける
- Dockerfile は `elythia` だけを build / COPY する。entrypoint の migrate の呼び出し (`mkgo-entrypoint`) も `elythia migrate` に変える
- 旧名のバイナリは置かない (R1 と同じく改名の版で切り替える)。`docs/deployment.md` の後始末バッチの手順を新しい呼び方に書き換える

### D12. ライセンスの表示 (R8)

- `frontend/` に本家の `LICENSE` / `COPYING` を取り込み時のまま置く
- リポジトリ直下の `NOTICE` に、`frontend/` が Misskey (AGPL-3.0) に由来すること、本家の版 (`UPSTREAM_MISSKEY_VERSION`) を追っていることを書く
- P4 の完了条件に含める

### D13. 本番の切り替え手順

P4 (bind mount の元が `third_party/misskey/built` から `frontend/built` に変わる)、D10 (移管)、P6 (配布イメージ名・バイナリ名が変わる) は本番の構成に触る。各段階で次を完了条件にする。

1. 手順を `docs/deployment.md` に書く (gitignore されたローカルの `compose.uds.yaml` の書き換えを含む)
2. 本番と同じ構成を隔離した名前の compose で立て、その手順を一度通す
3. 本番に当てる。`make uds-update` は切り離して実行する

本番を触る hazard の記述 (Makefile のコメント、運営者のメモ) も同じ段階で新しいパスに合わせる。

## 段階 (sub-issue の単位)

名前が決まらなくてもできる段階を先に進める。

| 段階 | 内容 | 名前に依存 |
|---|---|---|
| P1 | 追従方式の試算と、取り込む範囲の確定 (#3370、2026-10-04 に完了) | しない |
| P1b | 復路の保証をやめる (D9、#3191)。宣言は P6 の版の CHANGELOG | しない |
| P2 | テスト関連の配置の整理 (D6。#3373、2026-10-04 に完了) | しない |
| P3 | 本家の参照を `.cache/misskey` へ分離 (D2。#3378、2026-10-05 に完了)。この時点では frontend はまだ submodule のまま。fork の `packages/backend` にある、本家 backend e2e を mk-go へ向けて走らせる 3 ファイル (`test-server-mkgo/entry.ts` など) を `tests/` へ移す | しない |
| P4 | frontend の取り込み (D1 / D3 / D4 / D5)、submodule と fork の廃止。frontend の CI の required 化とライセンスの表示 (R8 / D12)、本番の切り替え (D13) を含む。あわせて、`@misskey-dev/emoji-assets` を frontend の依存に持ち直す (今は backend の `node_modules` から取っている)、Node.js の版を本家の `Dockerfile` でなく `.node-version` から読む、fork の assets image (`Dockerfile.assets` と publish の workflow。`Dockerfile.bundled` が使う) を本体の workflow でのビルドに置き換える (R2)、`misskey-js` の型の生成 (`build-misskey-js-with-types`) が使う `api.json` の作り方を決める (`api.json` は本家のソースに無く、本家 backend をビルドして `generate-api-json` で作る生成物。`.cache/misskey` で本家 backend をビルドするか、本体の API から作るか) | しない |
| P5 | 正式な名前 (**決定: Elythia**) と、プラグインの呼び名 (**決定: 据え置き**) の決定。どちらも 2026-09-30 | — |
| P5b | リポジトリの移管 (D10)。コードの名前は変えず、旧 URL からの転送で動くことを確かめる | する |
| P6 | 改名 (D7) と実行バイナリのサブコマンド化 (R7 / D11)。2.0.0 として出す (R6)。`docker-compose.yml` に `name:` を付ける (R3。named volume の移行手順と一緒に) | する |
| P6b | プラグインまわりの名前の移行 (D8)。連合に出る nodeinfo の宣言を含むので P6 とは別 PR にするが、同じ版で出す | する |
| P7 | ドキュメント・CLAUDE.md の整理。`docs/divergence.md` の分割 (R8) を含む | する |

- **P1 (試算) を最初に済ませた (#3370)。** 追従の方式 (D4) が実用になるかで P4 の作業量が変わるため。2026.7.0 → 2026.9.0、2026.9.0 → 2026.9.1、2026.9.1 → 2026.10.0 の 3 回で確かめた。P1b と P2 は他の段階と触るファイルが重ならないので、P1 と並行して進める
- P1b を P3 / P4 より先に置くのは、TS を立てる e2e の読み先が動く段階で `mkgo-born` を「守る」対象から「測る」対象に変えておくため (D9)
- P5b (移管) を P6 より先に分けるのは、移管で動くもの (D10) とコードの改名で動くものを別々に確かめるため
- P6 と P6b を同じ版で出すのは、運営者が上げる手間 (イメージ名・バイナリ名・マニフェスト名・nodeinfo の宣言) を 1 回にまとめるため
- P3 を P4 より先に分けるのは、「本家を読む側」と「自分たちの frontend を読む側」を別々に切り替えて、壊れたときにどちらが原因か分かるようにするため
- 各段階は本番 (UDS) の更新手順 (`make uds-update`) を壊さないこと。P4 は本番の compose (gitignore されたローカルの `compose.uds.yaml`) の bind mount の書き換えが要るので、移行手順を書いてから行う

## 未決事項

- ~~Q1. 正式な名前 (P5)~~ → **Elythia** に決定 (2026-09-30)。置き場所は `Elythia-Network`、機械が読む名前は小文字 (R1)
- ~~Q2. fork の履歴を持ち込むか~~ → **スナップショットとして取り込む** (2026-09-30)。独自変更の経緯は `docs/divergence.md` §4-2 と、アーカイブした fork で追う
- ~~Q3. 本家の取得方法~~ → **手元は共有の bare mirror + worktree、CI は毎回 shallow** (2026-09-30、D2)
- ~~Q4. 旧名の配布イメージの猶予期間~~ → **設けない**。改名の版で旧名での publish を止め、移転を案内する (2026-09-30)
- ~~Q5. 旧 URL (`/about-mkgo` など) の転送を残す期間~~ → **転送しない** (2026-09-30)
- ~~Q6. `assets/` (D3) の名前と置き場所~~ → **`frontend/assets/`** (2026-09-30、D3)。本家の直下の `assets/` は `frontend/repo-assets/` へ移す (2026-10-04、#3370)
- ~~Q7. `mkGoFrontendVersion` の新しい形式~~ → **廃止する** (2026-09-30、D5)。frontend だけの版という概念が要らない
- ~~Q9. プラグインの旧名を読み続ける猶予期間~~ → **設けない** (2026-09-30、D8)。旧名だけのマニフェストはビルドエラーにする

## リスク

- **本番の更新手順が変わる。** bind mount の元 (`third_party/misskey/built`) と、`uds-frontend-build` の出力先が変わる。本番を触る hazard (ビルドが配信物を消してから作る、i18n のビルドが本番の locales へ書く) は新しいパスでも同じなので、Makefile と memory の記述を合わせて更新する
- **独立リポジトリのプラグイン 4 つ** は P6 でモジュールパスが変わると import を直すまでビルドできない。P6 と同時にそれぞれ更新する
- ~~**追従の手間が増えうる。**~~ P1 (#3370) の試算で、過去 3 回とも今の方式と同じ量だった
