# Misskey TS upstream 追従アップデート手順

mk-go の `frontend/` は、本家 Misskey の monorepo から `packages/backend` を除いたもののスナップショットに、mk-go 独自の変更を載せたもの (#3379)。追従している本家の版はリポジトリ直下の `UPSTREAM_MISSKEY_VERSION` に 1 行で書き、比較対象の本家のソースは `make upstream-fetch` で `.cache/misskey/<版>/` に取る (bare mirror の `.cache/misskey/mirror.git` から版ごとに worktree を作る。#3378)。本家の新しい release が出るたびに、frontend の差分の取り込み (`make upstream-sync`) と、backend の差分の triage + Go への移植と、版を上げる作業を行う。

本書は **本家の版を上げた PR がマージされた後、各開発者 / operator が必要な手順** (1 章) と、**本家の新しい release が出た時の取り込み手順** (2 章) を説明する。

> **#3379 より前の運用。** frontend は submodule が指す fork (アーカイブ済み) から供給していて、追従は fork の上での載せ替えと tag の採番、gitlink の bump だった。fork の tag の採番規則と、その頃の独自変更の一覧は凍結した記録として [divergence.md §4-2](divergence.md#4-2-fork-frontend-の独自変更) に残っている。今の独自変更は §4-2b に PR 番号で書く (2-4)。

---

## 1. 既存環境への適用 (= 本家の版を上げた PR のマージ後)

### 1-1. pull

```bash
git pull
```

`frontend/` は本体で追跡しているので、`git pull` だけで新しい版の frontend のソースが揃う。

golden の再生成・本家 backend e2e・apicompat のように本家のソースを読む作業をするときだけ、新しい版の本家を取得する (起動とビルドには要らない)。

```bash
make upstream-fetch   # UPSTREAM_MISSKEY_VERSION の版を .cache/misskey/<版>/ へ取得
```

古い版の worktree は残るので、要らなければ `git -C .cache/misskey/mirror.git worktree remove --force .cache/misskey/<古い版>` で消す。

### 1-2. frontend asset の rebuild が必要なケース

`frontend/packages/frontend/` の vite ビルド成果物 (`frontend/built`) を mk-go が serve しているため、`frontend/` が変わった後に **frontend asset を再ビルド** しないと UI に古い JS が残る (#3379 より前は `third_party/misskey` の成果物を配信していた):

```bash
make uds-frontend-build
# = make e2e-frontend-build と同じ (alias)
```

数分〜10分かかる。docker daemon が必要。UDS / dropin / e2e いずれも同じビルド成果物を共有する。

### 1-3. UDS production stack の再ビルド

`compose.uds.yaml` ([リポジトリにあるのは `.example` 版](../compose.uds.yaml.example)) で本番運用している場合、Misskey TS の prebuilt image を pull しているわけではなく **mk-go バイナリ + `frontend/` の静的アセットを image に焼き込んでビルドしている** ([`deploy/uds/Dockerfile.mkgo`](../deploy/uds/Dockerfile.mkgo) の `COPY . .` 経由)。`frontend/` の更新 + frontend rebuild 後に image を作り直さないと古い asset が image にキャッシュされたまま:

```bash
# pull と 1-2 を済ませた状態 (= frontend/ + frontend asset が最新) で
make uds-build      # image を作り直す
make uds-restart    # 再起動 + 配信アセットの検証
```

**重要**:
- **image の作り直しと再起動は別の話で、両方要る**。image に焼き込むのは `deploy/uds/Dockerfile.mkgo` が `COPY` する 4 つ — static-assets (`frontend/assets`)、repo-assets (`frontend/repo-assets`)、twemoji、fluent-emoji (`frontend/node_modules/@misskey-dev/emoji-assets` から)。`frontend/` の更新でこれらが変わるので `uds-build` が要る
- **SPA のアセット (`frontend/built/_frontend_vite_`) は image に入らない**。bind-mount で渡しているので `uds-frontend-build` (1-2) の出力がそのまま配信される。ただし `compose.uds.yaml` がまだ `./third_party/misskey/built` を指している場合は誰も mount していないので、先に [デプロイの切り替え手順](deployment.md#frontend-を本体へ取り込んだ版へ上げる-3379) を済ませる
- **`--build` を付けても再起動は保証されない**。compose は image と設定が変わらなければコンテナを作り直さないので、bind-mount しか変わっていない場合は何も起きず、mk-go は起動時にキャッシュした古いエントリを配り続ける (#2885)。`make uds-restart` は `restart` を明示したうえで配信中のアセットが実在するかまで検証する
- **その検証は bind-mount の SPA アセットしか見ない**。image 側の asset (twemoji 等) が古いままでも緑になるので、`uds-build` を省かないこと
- `make uds-frontend-build` を skip すると Dockerfile builder の sanity check (`test -f .../1f004.svg` 等) で早期 fail する

`postgres` / `valkey` / `nginx` / `video-thumb` 等の外部 image は Misskey 無関係なので、本家の版を上げても影響を受けない。

### 1-4. migration 適用

本家の版を上げる PR には mk-go 側の migration が同梱されることが多い (例: PR #998 の `migration/000048_avatar_decoration_category.{up,down}.sql`)。本番環境では:

```bash
# 接続先は -config (既定 .config/default.yml) から決まる
make migrate-up
```

migration 連番は `migration/00NNNN_*.up.sql` の命名規則に従う (= 各 migration の番号は前との連続性を保つ)。

---

## 2. 新 upstream release 取り込み手順 (= 開発側)

新 Misskey TS release が出た時、mk-go 側で必要な作業フロー。**1 回の追従は 1 PR にまとめ、段階ごとにコミットを分ける** (frontend の取り込み、版を上げる作業、backend の移植の各 item)。各コミットが単体でビルドとテストを通すこと (CLAUDE.md Section 7)。

### 2-1. tracker issue を起票

`gh issue create --title "Tracker: Misskey TS <prev> → <new> への upstream 追従"` で tracker を作成。次の内容を含める:

- 対象 release tag (例: `2026.5.1`)
- backend 関連 commits 一覧 (`git -C .cache/misskey/mirror.git log --oneline --no-merges <prev>..<new> -- packages/backend/src/ packages/backend/migration/`。本家は `make upstream-fetch` が取得する bare repository から読む (#3378)。`<new>` の tag は `git -C .cache/misskey/mirror.git fetch --no-tags origin "refs/tags/<new>:refs/tags/<new>"` で足す。`make upstream-sync TO=<new> DRY=1` (2-4) も両方の tag を mirror へ取る)
- 関連 frontend / TS-only 変更の参考リスト
- 完了条件 (= sub-issue 全 close + 追従 PR のマージ)

### 2-2. triage doc を作成

release ごとの差分 doc `docs/update/<yyyymm><nn>diff.md` (命名は 3 章。直近は [`20261000diff.md`](./update/20261000diff.md)) を新規作成する。古い前例には `docs/update/yyyymmdd-<tracker-issue>-triage.md` (`docs/update/20260512-947-triage.md`) の形もある。

各 upstream commit について:
- `git -C .cache/misskey/mirror.git show <sha>` で diff 精読
- mk-go 該当箇所を `grep` で特定し file_path:line_number で記録
- Gap 判定 (`既対応 / 部分対応 / 未対応 / 影響なし`)
- 推定難易度 (`S / M / L / N/A`)
- 推定実装方針

末尾に Wave 1-N の推奨実行順 (= まず close 候補をまとめてから S → M → L の順に進む) を記載すると後続作業が読みやすい。

### 2-3. sub-issue 化

triage で判定した item を `gh issue create` で 1 件 1 issue として起票。命名規則 `#<tracker> sub-N (upstream #XXXXX): <要約>`。

各 sub-issue 本文には:
- 親 tracker への参照 (`parent tracker: #<n>`)
- triage doc の該当 item への参照 (`triage detail: PR #<n> (\`docs/update/...\` の item N)`)
- 概要 / 実装方針 / 完了条件

**注意**: GitHub の cross-repo 自動 link を避けるため、upstream PR 参照は `upstream PR <N>` (plain text、`misskey-dev/misskey#N` 形式は使わない) と書く。

### 2-4. frontend の差分を当てる (`make upstream-sync`)

`develop` から切ったブランチで行う。設計は [project-restructure.md の D4](design/project-restructure.md#d4-本家への追従-make-upstream-sync) で、区分の表は同じ文書の D1。

**1. 分類だけを見る。**

```bash
make upstream-sync TO=<新しい版> DRY=1
```

mirror (`.cache/misskey/mirror.git`、無ければ作る) に今の版 (`UPSTREAM_MISSKEY_VERSION`) と `TO` の tag を取り、`tools/upstreamsync` がそれを本体のリポジトリの `refs/upstream/<版>` へ取り込んで、変更されたパスを D1 の区分で数える。

| 区分 | 本家のパス | 扱い |
|---|---|---|
| 取り込む | `packages/` (`packages/backend` を除く)、`packages-private/`、`locales/`、`scripts/`、`patches/`、直下の `package.json` など | `frontend/` の下の同じパスへ当てる |
| 付け替える | `packages/backend/assets/` / 直下の `assets/` | `frontend/assets/` / `frontend/repo-assets/` へ当てる (D3) |
| 作り直す | `pnpm-lock.yaml` | 当てない。手順 4 で作り直す |
| 取り込まない | `packages/backend` (アセットを除く)、`.github/`、`.config/`、`Dockerfile`、文書類など | 当てない |

**どの区分にも当たらないパスがあると、何も当てずに止まる。** 本家が直下に新しく足したファイルや、submodule の gitlink の変更が該当する。「取り込む」で絞るだけだと新しいパスが黙って落ちるため。止まったら、そのパスの区分を決めて `tools/upstreamsync/main.go` の表 (`importFiles` / `importDirs` / `skipFiles` / `skipDirs`) と設計 D1 の表の両方に足し、`go test ./tools/upstreamsync/` を通してからやり直す。

**2. 当てる。**

```bash
make upstream-sync TO=<新しい版>
```

**`frontend/` に commit していない変更があると断る。** 衝突をファイル単位で解くので、手を入れている最中の変更と本家の差分が混ざらないようにするため。当てるのは `git apply --3way` で、付け替え先ごとに 3 回 (`frontend/` / `frontend/assets/` / `frontend/repo-assets/`) に分けて実行する。rename は削除と追加として扱う。

当てる前に全 pass を `git apply --3way --check` で確かめ、1 つでも当たらない差分 (frontend/ に無いファイルや、追跡していないファイルへの変更) があれば何も当てずに止まる。そのときは区分を見直すか、frontend/ を本家に揃えてからやり直す。

衝突しなかったファイルは index に載る。衝突したテキストのファイルは衝突マーカー付きで作業ツリーに残り、index では unmerged になる (`git diff --name-only --diff-filter=U` で一覧できる)。**バイナリが衝突したときはマーカーが付かず、mk-go 側の内容のまま unmerged になる**ので、本家の版を採るなら `git checkout --theirs -- <パス>` で入れ替える。最後に「次にやること」が表示され、衝突が残っていれば終了コードは 0 にならない。

**3. 衝突を解く。** mk-go 独自の frontend の変更は [divergence.md §4-2b](divergence.md#4-2b-frontend-の独自変更-3379-で取り込んだ後) に PR 番号で記録している。衝突したら、その箇所がどの行の変更かを §4-2b で引いて判断する。

- **本家が同じことを直していたら、mk-go の変更を落として本家の形を採り、§4-2b の行を更新する** (消すか、落とした経緯を書く)。§4-2b は「純正へ還元できない差分の一覧」として読むので、本家に入ったものを残さない
- 本家の変更と mk-go の変更が両立するなら、両方を残す形に解く

解いたら `git add` する。取り込みのコミットは衝突を解いた後の 1 つにまとめる。

**4. lock を作り直す。**

```bash
make upstream-sync-lock
```

`frontend/pnpm-lock.yaml` を、**直前の lock を基点に** `pnpm install --lockfile-only` で作り直す (node の container の中で実行する。docker daemon が必要)。本家の lock は使わない。backend を外した lock は本家の lock と大きく違い、本家の lock の差分は毎回衝突するため。

**作り直した lock の差分を目で見る。** `package.json` で変わった依存以外が動いていないことを確かめる。`--lockfile-only` は、lock に無い依存と範囲が変わった依存をその時点の最新に解決するので、本家が試した組とずれうる。`frontend/pnpm-workspace.yaml` の `minimumReleaseAge` (公開から 7 日) に満たない版は選ばれないので、本家が出たばかりの版を指定していると解決に失敗するか、古い版に落ちる。

**5. frontend を検査する。**

```bash
make frontend-check   # 型チェックと、frontend を読むゲート
make frontend-lint
make frontend-test    # 先に cd frontend && pnpm install && pnpm build
```

mfm-js か emoji-data の版が変わっていたら、下の「MFM の絵文字の正規表現」の手順も要る。

`tools/upstreamsync` が本体に置いた `refs/upstream/<版>` は branch でも tag でもないので push されない。要らなくなったら `git update-ref -d refs/upstream/<版>` で消してよい (次の追従でも、今の版の ref は取り直す)。

### 2-5. 本家の版を上げる

frontend の取り込みと同じ PR で、版を次の場所で揃えて上げる。揃っていることは `internal/entitycompat` の `TestUpstreamVersionIsConsistent` が見る (#3378)。

- `UPSTREAM_MISSKEY_VERSION`
- `internal/config/config.go` の `MisskeyVersion`
- e2e で TS 側として立てる `misskey/misskey:<版>` の tag (下の「比較対象の TS image を全部揃える」)

上げたら `make upstream-fetch` で新しい版の本家を取得し、下の「本家の版を上げた後に必須」の節を順に済ませる (golden の再生成、TypeORM migrations seed、index golden など)。`frontend/package.json` の版は `make upstream-sync` が本家の差分として上げるので、手で直さない (`/about-elythia` はこの版を出す)。

### 2-6. backend の移植 (Wave 単位のコミット)

実装方針 (PR #998 で確立):

1. **Infrastructure 先行**: frontend の取り込み (2-4) と版を上げる作業 (2-5)、hardcode 修正
2. **Wave 1 (close 候補)**: comment + regression test で意思表明
3. **Wave 2 (S 難易度)**: 1 commit / 1 sub-issue (or 関連を bundle) で順次
4. **Wave 3 (M 難易度)**: commit 1 件ずつで review しやすく
5. **Wave 4 (L 難易度)**: 削除 endpoint など、版を上げる作業と切り離せないもの
6. **Final audit**: 残り upstream commits も triage 突き合わせて drift を確認、結果を triage doc 末尾に追記
7. **Follow-up**: review で挙がった improvement を nit commit で取り込む

コミットメッセージは CLAUDE.md Section 7 の `<種類> <対象>: <要約> (#issue番号)` の形にする (例: 2026.10.0 の追従 #3285 の `Fix sw: 購読の解除で本家 2026.10.0 と同じパラメータを受け付ける (#3285)`)。sub-issue を閉じるコミットには `Closes #<sub-issue>` を入れる。

### 2-7. 試算: 過去の追従を `make upstream-sync` で分類する

`tools/upstreamsync` を作ったとき (#3379 の段階 P4d-3)、過去 3 回の追従を DRY=1 で分類し、3 回とも分類できないパスが無いことを確かめた。「取り込む」パスの件数は、設計 D4 の試算 (#3370) と同じ。

| 追従 | 「取り込む」パス |
|---|---|
| 2026.7.0 → 2026.9.0 | 105 ファイル |
| 2026.9.0 → 2026.9.1 | 19 ファイル |
| 2026.9.1 → 2026.10.0 | 16 ファイル |

### 本家の版を上げた後に必須: shape drift snapshot の再生成

`UPSTREAM_MISSKEY_VERSION` を新しい版に書き換えて `make upstream-fetch` で本家を取得したら、
entity shape drift gate の golden snapshot を再生成して commit すること (#3378 から、
golden は `.cache/misskey/<版>/` の本家から作る)。新バージョンで追加 / 変更された契約フィールドが次回の
`TestEntityShapeDrift` に反映される。

```bash
make shapecheck-gen           # internal/entitycompat/testdata/ の golden を全て再生成
make shapecheck               # gate がまだ通るか確認 (新規 drift が出たら allowlist or 修正)
go test ./internal/entitycompat/   # schema / migration seed gate も含めて確認
git add internal/entitycompat/testdata/
```

詳細は [shape-drift.md](./shape-drift.md)。

### 本家の版を上げた後に必須: TypeORM migrations seed の追加

upstream に新しい migration が入った場合、`migrations` テーブルへの seed も追加する。
これが漏れると、mk-go で動かした DB に本家を繋ぎ直したときに TypeORM が当該
migration を未実行と判定して**再実行**し、適用済み DDL への `ADD COLUMN` 重複や
`DROP COLUMN` によるデータ喪失につながりうる (#2244)。

復路は保証しない (#3191) が、この seed は `mkgo-born` で戻れる範囲を測る前提なので引き続き足す。
`TestMigrationSeed_CoversUpstream` が漏れを検出するので、落ちたら
`migration/000067_migrations_typeorm_names.up.sql` と同じ形式で seed を足す。

**seed する前に、その migration の DDL が mk-go 側にも入っているか必ず確認すること。**
入っていないまま seed すると、本家が「適用済み」と誤認して skip し、schema が
ずれたまま放置される。DDL が未実装なら先に mk-go 側の migration を書く。

### 本家の版を上げた後に必須: index golden の再生成

upstream が index を足した場合、`golden_upstream_indexes.json` も撮り直す。これは
TypeORM の decorator から正規形を再現できないため **実 DB から採る** 必要がある
(手順は [shape-drift.md](./shape-drift.md#golden-の再生成))。

撮り直したら `TestIndexNaming_NoNewUpstreamDuplicates` を走らせる。mk-go 側に
同内容・別名の index があれば検出されるので、upstream 名に揃えるか
`known_duplicate_indexes.json` に追加して `000068` の扱いを見直す (#2246)。

### 本家の版を上げた後に必須: MFM の絵文字の正規表現

mk-go の MFM パーサは、mfm-js が依存する `@misskey-dev/emoji-data` の `emojiRegex` を Go の正規表現へ移したもの (`internal/activitypub/mfm/emoji_regex_gen.go`) で Unicode 絵文字を読む (#3324)。frontend の mfm-js の版か、それが依存する emoji-data の版が変わると、`emoji-regex-check` (CI では `frontend` workflow の `frontend-lint`、手元では `make frontend-check` から呼ばれる) が落ちる (正規表現が同じでも、snapshot に記録した版と食い違うため)。mfm-js の `unicodeEmoji` の書き方が変わったときも、生成ツールが前提の形を見つけられずに落ちる (下記)。

```bash
make emoji-regex     # 生成物と tools/emojiregex/testdata/source.txt を作り直す
node internal/activitypub/mfm/testdata/emoji_mfmjs.mjs frontend \
  > internal/activitypub/mfm/testdata/emoji_mfmjs.json   # mfm-js の期待値を作り直す
GOWORK=off go test ./internal/activitypub/mfm/ ./tools/emojiregex/
```

生成ツールは、mfm-js の `unicodeEmoji` の書き方 (`regexp(RegExp(emojiRegex.source))` と、U+FE0F だけのときに文字を返す `map`) と、正規表現が使う構文 (`?` だけの量指定・サロゲートペア・決まった位置の否定の先読み) を前提にしている。前提が崩れると生成の時点で落ちるので、その場合は生成ツールを直す。

### 本家の版を上げた後に必須: divergence doc の件数

`golden_upstream_columns.json` を撮り直すと `TestDivergenceDoc_ColumnCountMatchesSchema` が動く。**upstream が列を DROP すると、その列は「mk-go 独自カラム」に転じる**ので `docs/divergence.md` §2-2 の件数が増える (`note_favorite.createdAt` がその経緯で独自列になっている)。

落ちたら doc の件数・内訳・冒頭サマリ・表の行をまとめて直す。gate は 4 箇所すべてを見るので、どれか 1 つを直し忘れると通らない (#2634)。

### 本家の版を上げた後に必須: promo の表示経路が upstream に入っていないか見る

**promo (`admin/promo/create` / `promo/read`)** は upstream にも mk-go にも
**表示経路が無い** — 作成と既読化はできて DB 行も増えるが、`promo_note` を読んで
利用者へ提示するものがどこにも無い (#2781)。mk-go はこの状態を忠実に再現している。

**upstream は一度実装して外している。** 2020-02 に
`server/api/common/inject-promo.ts` で timeline へ直挿しする実装が入ったが
(`a54de07260`)、2021-03 に「クライアントサイドで実装したいため」無効化され
(`73df95c42d`)、2022-09 にファイルごと削除された (`786f1d8be8`)。frontend の
menu も 2024-09 の #14554 で消えている。**再実装される見込みは低いが、endpoint は
残っているので版を上げるごとに一応見る。**

版を上げた後に確認する:

```bash
grep -rlni "promonote\|promoread" .cache/misskey/<版>/packages/backend/src/
```

期待は **8 件** (case-insensitive にしてあるのは型名 `MiPromoNote` や
repository 名 `PromoNotesRepository` を拾うため):

```
di-symbols.ts / postgres.ts
models/_.ts / models/RepositoryModule.ts / models/PromoNote.ts / models/PromoRead.ts
server/api/endpoints/promo/read.ts
server/api/endpoints/admin/promo/create.ts     ← 表示経路はここに無い
```

**件数ではなくリストが一致するかを見る** (加減が相殺すると件数だけでは素通りする)。
違っていたら中身を見て、`docs/api-compatibility.md` の「既知の制限」と
`docs/divergence.md` §7 の promo 行を更新する。増えていれば表示経路が入った可能性、
減っていれば endpoint が削除された可能性。

**0 件や `No such file or directory` が出たら、まず本家の取得を疑う**
(`make upstream-fetch`)。取得済みで 0 件なら、upstream 側でパス構成が変わっている。

**CI の Go テストでは検出できない。** Go テストが走る `test-shards` / `plugin-tests`
は本家を取得しないので、本家を読むテストはそこでは skip される。本家を取得して
skip を禁じて回すのは `apicompat` workflow の `make upstream-check` だけ (#3378) で、
そこに足すなら `internal/misc/achievement/types_test.go` の `TestTypes_MatchUpstream`
が同型 (`internal/upstreamsrc` で本家を探し、不在なら skip、`MK_UPSTREAM_REQUIRE`
が立っていれば落ちる)。

### 本家の版を上げた後に必須: 比較対象の TS image を全部揃える

mk-go と Misskey TS を並べて比較するハーネスは、**比較対象の image tag を
`MisskeyVersion` と同じ版に上げる**こと。ここがずれていると upstream 自身の
バージョン間差分が差分として出てしまい、mk-go 固有の乖離と区別できない。

| ファイル | 対象 |
|---|---|
| `tests/diff/compose.yml` | 差分比較ハーネス ([diff-e2e.md](./diff-e2e.md)) |
| `tests/playwright/compose.ts.yml` | Playwright の TS baseline |
| `.github/workflows/playwright.yml` | 上記の pre-pull (tag が sync していないと pull が無駄になる) |
| `.github/workflows/diff-e2e.yml` | diff ハーネスの pre-pull。**compose 側だけ上げて忘れやすい** (#2877 で実際に残した) |
| `tests/dropin/compose.yml` / `tests/dropin-frontend/compose.yml` / `tests/federation/compose.misskey.yml` | drop-in / 実連合の TS インスタンス |
| `.github/workflows/dropin-e2e.yml` / `dropin-frontend-e2e.yml` | 上記の pre-pull と matrix |
| `tests/bench/http/` / `tests/bench/queue/` の compose | 性能比較の対象 |

**古い tag でも image は問題なくビルドできる**ので、腐っても CI は落ちない —
落ちるのは配った先だけ。`tests/bench/http/` はどの workflow からも参照されていない
(`tests/bench/queue/` は nightly の `queue-bench-smoke.yml` が引く)。

以前は `Dockerfile.bundled` の `MISSKEY_ASSETS_IMAGE` (fork が publish する assets image)
もこの表に入っていて、表に載せた後も実際に置き去りになった (#2877 / #3011)。#3379 で
frontend を image の中でビルドするようになったので、その pin は無くなった。

**探し方は `git grep -n 'misskey/misskey:' -- '*.yml' '*.yaml' '*.md'`。**
表を手で追うより確実で、doc の散文に埋まった版数 (`docs/dropin-e2e.md` のトラブルシュート等) も拾える。

**除外リストの「version-gap」注記は、版を揃えたら必ず読み直す。** 実例として、
diff harness の `META_IGNORE` には `app192IconUrl` / `app512IconUrl` /
`singleUserMode` が「mk-go 2026.6.0 が持ち TS 2026.5.4 に無い」として除外されて
いたが、TS を 2026.7.0 に揃えたら 3 件とも残った。実際は upstream では
`admin/meta` にしか無く公開 `/api/meta` には元から含まれない = **mk-go の余剰
フィールド**で、版ずれが誤診断を固定していた (#2303)。

### 本家の版を上げた後に必須: TS baseline で Playwright を回す

```bash
gh workflow run playwright.yml --ref <branch> -f ref=<branch>   # TS backend も含めて実行される
# または手元で
make playwright-ts-up && make playwright-ts-test && make playwright-ts-down
```

**`-f ref=<branch>` を省かない。** checkout は `inputs.ref || github.ref` だが、入力 `ref` の既定値が
`develop` なので `github.ref` には落ちない。`--ref` だけだと workflow 定義はそのブランチのものを使いつつ
**develop のコードを検証する** (2026.9.1 の追従で 2 回踏んだ。落ちた行番号が修正前のものだった)。
`diff-e2e.yml` / `dropin-e2e.yml` / `upstream-backend-e2e.yml` も同じ形。

Playwright spec は普段 mk-go backend に対してしか走っていない (PR トリガーでも
mk-go のみ)。**TS backend に対して回すのは upstream 追従のタイミングだけ**という
運用にしている。

理由は、spec が「mk-go の挙動を正解として」書かれてしまう事故を、追従の節目で
検出するため。実際 #2276 で 3 ヶ月ぶりに TS backend で回したところ、spec が
mk-go 側の挙動に引きずられていた箇所が 19 件見つかり、そのうち 5 件は mk-go の
実バグだった (#2283 renoteCount の加算条件 / #2284 必須パラメータの未検証 /
#2285 `user.updatedAt` のセマンティクス / #2286 ユーザー検索の実装乖離 /
#2287 余剰フィールド)。

一方で常時 (nightly や PR で) 回す価値は薄い。同一 CI 環境・同一 spec で
所要時間を比較すると mk-go と TS に実用上の差は無く (TS/mk-go の中央値 0.94)、
得られるのは所要時間ではなく **spec の前提が upstream とずれていないか**という
一点だけだから。upstream が変わらない限りその答えも変わらない。

失敗した spec を見るときは以下に注意する。

- mk-go には `docs/divergence.md` に記録した**意図的な差分**がある
  (例: `NO_SUCH_*` を upstream は 400、mk-go は意味的に正確な 404 で返す)。
  spec 側は `tests/playwright/fixtures/backend.ts` の `NOT_FOUND_STATUS` の
  ように backend ごとの期待値で吸収する。ただし**この逃げ道を足すたびに、その
  spec は parity を証明しなくなる**ので、安易に増やさない
- upstream 固有の前提でしか成立しない挙動もある (例: `state:'alive'` は
  `updatedAt > now-5d` で絞るが、upstream が local user の `updatedAt` を
  更新するのは note 投稿時だけなので、signup 直後の user は一覧に出ない)。
  この種は spec の前提条件を直す

### mk-go 側の migration を書くときの必須ルール

mk-go の migration は Misskey TS が作った既存 DB にも流れる。以下は
`TestMigrationIdempotency_RequiresIfExists` が強制する。

- `CREATE TABLE` / `ADD COLUMN` / `CREATE INDEX` は必ず `IF NOT EXISTS`
- `DROP TABLE` / `DROP COLUMN` / `DROP INDEX` は必ず `IF EXISTS`
- upstream に同じ内容の index があるなら **upstream の index 名をそのまま使う**
  (`000058` が前例)。名前が違うと `IF NOT EXISTS` が効かず TS 製 DB で二重化する

---

## 3. 参考リンク

- 直近の triage 例: [`docs/update/20260512-947-triage.md`](./update/20260512-947-triage.md)
- upstream release 差分まとめ: `docs/update/<yyyymm><nn>diff.md` (`nn` は**対象 release の patch 番号**。2026.5.4 なら `20260504`。backend に変更が無い release は doc を作らないので番号は飛ぶ)。triage note は `<yyyymmdd>-<issue>-triage.md`
- PR #998: 2026.3.2 → 2026.5.1 一括取り込みの reference 実装 (= Infrastructure + Wave 1-4 + follow-up audit + #17034)
- [api-compatibility.md](./api-compatibility.md): 互換性追跡
- [migration-from-ts.md](./migration-from-ts.md): TS → mk-go drop-in 切替
