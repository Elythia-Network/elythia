# Misskey TS upstream 追従アップデート手順

mk-go は `third_party/misskey` submodule で Misskey TS の特定 release tag を pin して、frontend asset + drop-in 互換性の参照点として利用している。upstream の新 release が出るたびに backend 差分の triage + 取り込み + submodule bump を行う必要がある。

本書は **submodule bump を含む PR がマージされた後、各開発者 / operator が必要な手順** と、**新 upstream release が出た時の triage 運用** を説明する。

---

## 1. 既存環境への適用 (= submodule bump PR マージ後)

`git pull` だけでは submodule の working tree は更新されない (= 親リポの gitlink ポインタが移動するのみ)。`third_party/misskey/` 配下の実 file を新 release に揃えるには明示的な submodule update が必要。

### 1-1. 推奨: 1 コマンドで pull + submodule update

```bash
git pull --recurse-submodules
```

このフラグは `git pull` 単体だと毎回つける必要がある。常時 on にしたい場合は次の設定を 1 度実行:

```bash
git config submodule.recurse true
```

→ 以後 `git pull` / `git checkout` / `git merge` で自動的に submodule も追従する。`.git/config` (= repo local) に書かれるため、他開発者には伝播しない。各人が一度設定すること。

### 1-2. 手動 (一括設定なし)

```bash
git pull
git submodule update --init --recursive
```

### 1-3. 確認

```bash
git -C third_party/misskey describe --tags HEAD
# → 例: 2026.5.1-mk.0
git -C third_party/misskey log --oneline HEAD -1
# → 例: 8c292244e7 fix(frontend): add null guard for MkModal content children[0]
```

### 1-4. frontend asset の rebuild が必要なケース

`third_party/misskey/packages/frontend/` の vite ビルド成果物を mk-go が serve しているため、submodule bump 後に **frontend asset を再ビルド** しないと UI に古い JS が残る:

```bash
make uds-frontend-build
# = make e2e-frontend-build と同じ (alias)
```

数分〜10分かかる。docker daemon が必要。UDS / dropin / e2e いずれも同じビルド成果物を共有する。

### 1-5. UDS production stack の再ビルド

`compose.uds.yaml` ([リポジトリにあるのは `.example` 版](../compose.uds.yaml.example)) で本番運用している場合、Misskey TS の prebuilt image を pull しているわけではなく **mk-go バイナリ + submodule の静的アセットを image に焼き込んでビルドしている** ([`deploy/uds/Dockerfile.mkgo`](../deploy/uds/Dockerfile.mkgo) の `COPY . .` 経由)。submodule update + frontend rebuild 後に image を作り直さないと古い asset が image にキャッシュされたまま:

```bash
# 1-1 / 1-2 と 1-4 を済ませた状態 (= submodule + frontend asset が最新) で
make uds-build      # image を作り直す
make uds-restart    # 再起動 + 配信アセットの検証
```

**重要**:
- **image の作り直しと再起動は別の話で、両方要る**。image に焼き込むのは `deploy/uds/Dockerfile.mkgo` が `COPY` する 4 つ — static-assets (`packages/backend/assets`)、repo-assets (`third_party/misskey/assets`)、twemoji、fluent-emoji。submodule bump でこれらが変わるので `uds-build` が要る
- **SPA のアセット (`built/_frontend_vite_`) は image に入らない**。bind-mount で渡しているので `uds-frontend-build` (1-4) の出力がそのまま配信される
- **`--build` を付けても再起動は保証されない**。compose は image と設定が変わらなければコンテナを作り直さないので、bind-mount しか変わっていない場合は何も起きず、mk-go は起動時にキャッシュした古いエントリを配り続ける (#2885)。`make uds-restart` は `restart` を明示したうえで配信中のアセットが実在するかまで検証する
- **その検証は bind-mount の SPA アセットしか見ない**。image 側の asset (twemoji 等) が古いままでも緑になるので、`uds-build` を省かないこと
- `make uds-frontend-build` を skip すると Dockerfile builder の sanity check (`test -f .../1f004.svg` 等) で早期 fail する

`postgres` / `valkey` / `nginx` / `video-thumb` 等の外部 image は Misskey 無関係なので submodule bump で影響を受けない。

### 1-6. migration 適用

submodule bump PR には mk-go 側の migration が同梱されることが多い (例: PR #998 の `migration/000048_avatar_decoration_category.{up,down}.sql`)。本番環境では:

```bash
# 接続先は -config (既定 .config/default.yml) から決まる
make migrate-up
```

migration 連番は `migration/00NNNN_*.up.sql` の命名規則に従う (= 各 migration の番号は前との連続性を保つ)。

---

## 2. 新 upstream release 取り込み手順 (= 開発側)

新 Misskey TS release が出た時、mk-go 側で必要な作業フロー:

### 2-1. tracker issue を起票

`gh issue create --title "Tracker: Misskey TS <prev> → <new> への upstream 追従"` で tracker を作成。次の内容を含める:

- 対象 release tag (例: `2026.5.1`)
- backend 関連 commits 一覧 (`git -C third_party/misskey log --oneline --no-merges <prev>..<new> -- packages/backend/src/ packages/backend/migration/`)
- 関連 frontend / TS-only 変更の参考リスト
- 完了条件 (= sub-issue 全 close + submodule bump PR マージ)

### 2-2. triage doc を作成

`docs/update/yyyymmdd-<tracker-issue>-triage.md` を新規作成。前例: `docs/update/20260512-947-triage.md`。

各 upstream commit について:
- `git -C third_party/misskey show <sha>` で diff 精読
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

### 2-4. submodule bump + Wave 単位の実装 PR

実装方針 (PR #998 で確立):

1. **Infrastructure 先行**: submodule bump (新 tag) + `MisskeyVersion` 定数更新 + hardcode 修正
2. **Wave 1 (close 候補)**: comment + regression test で意思表明
3. **Wave 2 (S 難易度)**: 1 commit / 1 sub-issue (or 関連を bundle) で順次
4. **Wave 3 (M 難易度)**: PR 1 本ずつ / commit 1 件ずつで review しやすく
5. **Wave 4 (L 難易度)**: submodule bump とセット (例: 削除 endpoint)
6. **Final audit**: 残り upstream commits も triage 突き合わせて drift を確認、結果を triage doc 末尾に追記
7. **Follow-up**: review で挙がった improvement を nit commit で取り込む

各 commit は `2026.X.Y Wave N (M/N): <要約>` 命名で、`Closes #<sub-issue>` で sub-issue を自動 close する。

### 2-5. submodule bump 時の fork 運用

mk-go は `shiroha-a/misskey-ts` fork を経由して submodule を pin している (= upstream の release tag + mk 固有のパッチを cherry-pick したもの)。新 release を取り込む手順:

```bash
cd third_party/misskey
git fetch upstream <tag>
# 例: <tag>=2026.5.1
git checkout -b <tag>-fix <tag>
git cherry-pick <既存 patch sha>  # 例: 79ccc36ec0 (MkModal null guard)
git tag <tag>-mk.0
git push origin <tag>-fix
git push origin <tag>-mk.0
cd -
git add third_party/misskey
# commit + PR
```

#### fork タグの採番規則

形式は `<upstream release>-mk.<N>[<英字>]`（例: `2026.9.0-mk.39`、`2026.9.0-mk.34c`）。
**lightweight tag** で、fork の `mk-<upstream release>` 系列の先端に打つ。

| 進めるもの | いつ | 例 |
|---|---|---|
| 数字 (`N`) | **新機能**、および**直前の数字タグとは無関係な修正** | `mk.38` → `mk.39` |
| 英字 | **直前の数字タグで入れた変更の後追い修正** | `mk.39` → `mk.39a` → `mk.39b` |

**英字は「直前の数字タグの後始末」に限る。** バグ修正だから英字、ではない —
**世代をまたぐ修正は新しい数字を取る**。英字は列の順序を保つためのもので、`-mk.24` の後に
`-mk.12a` を打つと `git describe --tags` が後戻りして見えるため (先例は `-mk.23` /
`-mk.25` / `-mk.28`。規則の出どころは `docs/divergence.md` の「fork frontend の変更」で、
`assertForkTagSequence` の GoDoc も同じことを書いている)。

**1 PR = 1 タグ。** frontend に複数コミットを積む PR でも打つタグは 1 つで、`docs/divergence.md`
§4-2 の表も 1 行になる。1 コミット = 1 タグにしないのは、表を「還元不能な差分の一覧」として
読むときの単位が PR だから。

**`N` は upstream release ごとに 0 から数え直す。** 取り込み直後の素の状態が `-mk.0` で、
載せ替え (`git rebase --onto <新 release> <旧 release>`) で持ち込んだ custom commit も
`-mk.0` に含める。§4-2 の tag 列は「その変更が**最初に入った世代**」なので、載せ替えても
古い `2026.7.0-mk.*` の行はそのまま残す。

**英字が `z` に達したら数字を上げる。** 26 回も後追いの修正が要る変更はもう別物とみなす。
実測では 2026.9.0 の 82 タグを通して英字の最長連続は 8 なので、通常は到達しない。

**`fix` のコミットでも数字を取ることがある。** `2026.9.0` の数字タグ 40 件のうち 13 件は
`fix(...)` だが (`mk.1` / `mk.2` / `mk.4` …)、**どれも直前の数字タグとは無関係な修正**なので
規則どおり。commit prefix と採番は 1 対 1 ではない — 見るのは「直前の数字タグの後始末か」
であって、`feat` か `fix` かではない。

**過去のタグは振り直さない。** タグは push 済みで、fork 側の
`Publish frontend assets image` workflow が `*-mk.*` で発火して
`ghcr.io/shiroha-a/misskey-ts-assets:<tag>` を publish しているため、打ち直すと配布物との
対応が壊れる。

タグを打ったら、**親リポ側で 4 箇所を同時に更新する**（順序は「submodule に commit →
fork へ push → tag を push → 親リポの gitlink と doc」。逆順だと CI の checkout が
`not our ref` で死ぬ）:

- `docs/divergence.md` の pin 行（tag と**短縮 SHA の併記**。`make submodulepin-check` が gitlink と突き合わせる）
- `docs/divergence.md` §4-2 の表に 1 行
- 同ファイル冒頭サマリの件数と範囲（`TestDivergenceDoc_*` が表と突き合わせる）
- `Dockerfile.bundled` の `MISSKEY_ASSETS_IMAGE`（配る image に焼く frontend。ずれても image はビルドできるので CI は落ちない）。**`<tag>@sha256:<digest>` で書き、digest も取り直す** — BuildKit は digest を優先するので、tag だけ上げると新しい tag を名乗る古い frontend が焼き込まれる。digest は `docker buildx imagetools inspect --raw ghcr.io/shiroha-a/misskey-ts-assets:<tag> | sha256sum` で取れる (manifest のバイト列の sha256 が digest)。tag と digest の対応は `docker.yml` の `build-and-push-bundled` が registry に問い合わせて見る (過去タグを `-f tag=` で publish し直すときは、当時の Dockerfile.bundled が digest を持たないので照合を skip して warning に落とす)

機械で守られているのはこのうち「表の連番が規則どおりか」（`assertForkTagSequence`。
数字 +1 か、同じ数字への次の英字しか許さない）と「pin 行 ↔ gitlink」「tag → commit」
(`make submodulepin-check` と CI の `build` job)。

**数字と英字のどちらを選ぶかは機械では見ていないし、見られない。** 判定には「この修正は
直前の数字タグで入れた変更の後始末か」という意味判断が要り、commit の件名からは導けない。
`fix` なら英字という形なら機械化できるが、それは上のとおり規則と違う (#3141 で検討して
採らなかった。実測で 121 タグはすべて規則どおりで、止めるべきドリフトが無かった。
`docs(` や `Revert:` の commit も実在するので `feat` / `fix` の二択にも寄せられない)。


#### mk 固有パッチだけを載せるとき（release bump 以外）

upstream release の取り込み以外で fork frontend だけを直す PR でも、**submodule の gitlink は同じ規律で扱う**。

mk の `develop` が追跡しているのは fork の **`mk-2026.x.x` 系列**（例: `mk-2026.9.0`）であり、fork の `develop` とは別系列。系列ごとに独自コミットが積まれており、**misskey-ts 側の PR を `develop` にマージしただけでは mk の submodule 系列には入らない**。差分の大きさは時期で変わるので、**fork 側** (`third_party/misskey`) で次を実行する:

```bash
cd third_party/misskey
git fetch origin develop mk-2026.9.0   # 例: mk が指す系列
git rev-list --left-right --count origin/develop...origin/mk-2026.9.0
```

| やること | 理由 |
|---|---|
| misskey-ts の PR base を **mk が指している `mk-2026.x.x`** に合わせる | マージ後に submodule を fast-forward で載せられる |
| mk 側の bump 前に祖先関係を確認する | 誤った SHA だと fork 独自コミットが巻き戻る |
| **閉じた PR の head SHA** を gitlink に使わない | 別系列・古い base のコミットを指しやすい |

bump 前の確認（`third_party/misskey` 内で実行）:

```bash
OLD=<現在の mk develop が指す submodule SHA>
NEW=<misskey-ts PR マージ後に載せたい SHA>

git merge-base --is-ancestor "$OLD" "$NEW" && echo "fast-forward 可"
git rev-list --count "$NEW..$OLD"    # 失われる mk 独自コミット数。0 であること
git diff --diff-filter=D --name-only "$OLD" "$NEW" | wc -l   # 削除ファイル。0 であること
```

**CI は祖先関係の巻き戻りを検出しない。** `build` job は gitlink の SHA が fork に **push 済みか**は見るが、fast-forward 可能か（祖先関係）は見ない。`build` / `test` / `lint` の required check は submodule を checkout しない。`frontend-check` は型・eslint・vitest を見るが、**ファイルが消えても型が通る**場合がある。pointer の妥当性は上のコマンドで人手確認する。

### submodule bump 後に必須: shape drift snapshot の再生成

`third_party/misskey` を bump したら、entity shape drift gate の golden snapshot を
再生成して commit すること。新バージョンで追加 / 変更された契約フィールドが次回の
`TestEntityShapeDrift` に反映される。

```bash
make shapecheck-gen           # internal/entitycompat/testdata/ の golden を全て再生成
make shapecheck               # gate がまだ通るか確認 (新規 drift が出たら allowlist or 修正)
go test ./internal/entitycompat/   # schema / migration seed gate も含めて確認
git add internal/entitycompat/testdata/
```

詳細は [shape-drift.md](./shape-drift.md)。

### submodule bump 後に必須: TypeORM migrations seed の追加

upstream に新しい migration が入った場合、`migrations` テーブルへの seed も追加する。
これが漏れると、mk-go で動かした DB に本家を繋ぎ直したときに TypeORM が当該
migration を未実行と判定して**再実行**し、適用済み DDL への `ADD COLUMN` 重複や
`DROP COLUMN` によるデータ喪失につながりうる (#2244)。

`TestMigrationSeed_CoversUpstream` が漏れを検出するので、落ちたら
`migration/000067_migrations_typeorm_names.up.sql` と同じ形式で seed を足す。

**seed する前に、その migration の DDL が mk-go 側にも入っているか必ず確認すること。**
入っていないまま seed すると、本家が「適用済み」と誤認して skip し、schema が
ずれたまま放置される。DDL が未実装なら先に mk-go 側の migration を書く。

### submodule bump 後に必須: index golden の再生成

upstream が index を足した場合、`golden_upstream_indexes.json` も撮り直す。これは
TypeORM の decorator から正規形を再現できないため **実 DB から採る** 必要がある
(手順は [shape-drift.md](./shape-drift.md#golden-の再生成))。

撮り直したら `TestIndexNaming_NoNewUpstreamDuplicates` を走らせる。mk-go 側に
同内容・別名の index があれば検出されるので、upstream 名に揃えるか
`known_duplicate_indexes.json` に追加して `000068` の扱いを見直す (#2246)。

### submodule bump 後に必須: divergence doc の件数

`golden_upstream_columns.json` を撮り直すと `TestDivergenceDoc_ColumnCountMatchesSchema` が動く。**upstream が列を DROP すると、その列は「mk-go 独自カラム」に転じる**ので `docs/divergence.md` §2-2 の件数が増える (`note_favorite.createdAt` がその経緯で独自列になっている)。

落ちたら doc の件数・内訳・冒頭サマリ・表の行をまとめて直す。gate は 4 箇所すべてを見るので、どれか 1 つを直し忘れると通らない (#2634)。

### submodule bump 後に必須: promo の表示経路が upstream に入っていないか見る

**promo (`admin/promo/create` / `promo/read`)** は upstream にも mk-go にも
**表示経路が無い** — 作成と既読化はできて DB 行も増えるが、`promo_note` を読んで
利用者へ提示するものがどこにも無い (#2781)。mk-go はこの状態を忠実に再現している。

**upstream は一度実装して外している。** 2020-02 に
`server/api/common/inject-promo.ts` で timeline へ直挿しする実装が入ったが
(`a54de07260`)、2021-03 に「クライアントサイドで実装したいため」無効化され
(`73df95c42d`)、2022-09 にファイルごと削除された (`786f1d8be8`)。frontend の
menu も 2024-09 の #14554 で消えている。**再実装される見込みは低いが、endpoint は
残っているので bump ごとに一応見る。**

bump 後に確認する:

```bash
grep -rlni "promonote\|promoread" third_party/misskey/packages/backend/src/
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

**0 件や `No such file or directory` が出たら、まず submodule の checkout を疑う**
(`git submodule update --init`)。checkout 済みで 0 件なら、upstream 側でパス構成が
変わっている。

**CI の Go テストでは検出できない。** submodule を checkout する job は 8 つある
(`ci.yml` の `frontend-check`、diff-e2e の `diff`、docker の `build-and-push`、
dropin-e2e の `dropin`、dropin-frontend-e2e の `frontend-e2e`、playwright の `spec`、
queue-bench-smoke の `smoke`、upstream-backend-e2e の `e2e`) が、**いずれも Go
テストを実行する step を持たない**。Go テストが走る `test-shards` / `plugin-tests`
の checkout に `submodules` 指定は無いので、submodule を読むテストは CI では常に
skip され gate にならない。ローカルの tripwire として書くなら
`internal/misc/achievement/types_test.go` の `TestTypes_MatchUpstream` が同型
(submodule を読み、不在なら `t.Skipf`)。

### submodule bump 後に必須: 比較対象の TS image を全部揃える

mk-go と Misskey TS を並べて比較するハーネスは、**比較対象の image tag を
`MisskeyVersion` と同じ版に上げる**こと。ここがずれていると upstream 自身の
バージョン間差分が差分として出てしまい、mk-go 固有の乖離と区別できない。

| ファイル | 対象 |
|---|---|
| `docker-compose.diff.yml` | 差分比較ハーネス ([diff-e2e.md](./diff-e2e.md)) |
| `docker-compose.playwright.ts.yml` | Playwright の TS baseline |
| `.github/workflows/playwright.yml` | 上記の pre-pull (tag が sync していないと pull が無駄になる) |
| `.github/workflows/diff-e2e.yml` | diff ハーネスの pre-pull。**compose 側だけ上げて忘れやすい** (#2877 で実際に残した) |
| `docker-compose.dropin.yml` / `docker-compose.dropin-frontend.yml` / `docker-compose.federation.misskey.yml` | drop-in / 実連合の TS インスタンス |
| `.github/workflows/dropin-e2e.yml` / `dropin-frontend-e2e.yml` | 上記の pre-pull と matrix |
| `tests/bench/` / `tests/queue-bench/` の compose | 性能比較の対象 |
| `Dockerfile.bundled` の `MISSKEY_ASSETS_IMAGE` | **配る image に焼く frontend**。これだけは TS image ではなく fork の assets image (`ghcr.io/shiroha-a/misskey-ts-assets:<tag>-mk.N`) で、**submodule のタグと 1:1 で対応させる**。ずれると 2026.9.0 の backend に古い frontend を載せた image を配ることになる。`make submodulepin-check` が `docs/divergence.md` の pin 行と突き合わせる (#3011) |

**`Dockerfile.bundled` は表に無かったせいで実際に 23 世代遅れた** (#2877 の時点で
`2026.7.0-mk.10`。数え方は fork の `*-mk.*` タグを `2026.7.0-mk.10` より後で数えた値で、
`mk.11`〜`mk.22` の 12 個に加えて `mk.22a`〜`mk.22j` の 10 個と `2026.9.0-mk.0` を含む)。
**古い tag でも image は問題なくビルドできる**ので、腐っても CI は落ちない —
落ちるのは配った先だけ。`tests/bench/` も同じ性質で、こちらはどの workflow からも
参照されていない (`tests/queue-bench/` は nightly の `queue-bench-smoke.yml` が引く)。
assets image は fork 側の `Publish frontend assets image` workflow が `*-mk.*` タグで
発火して publish するので、**submodule のタグを push した後**に上げること
(`gh run list --repo shiroha-a/misskey-ts` で success を確認できる)。

**表に載せただけでは止まらなかった。** #2877 で表へ載せた後も `Dockerfile.bundled` の
pin は `2026.9.0-mk.0` に置き去りのままで、**リリースした `1.3.0-bundled` は既に 2 世代**
(submodule は `2026.9.0-mk.2`)、develop では 29 世代ずれていた (pin されていた `mk.0` から数えた間隔。数字付きの tag 30 個から 1 を引いた値で、英字付きを含めると 62 個から 1 を引いて 61)。現在は
`make submodulepin-check` が pin 行の tag と突き合わせるので、片側だけ上げると
`make gates` が落ちる (#3011)。**publish 済みかどうかまでは見ない** —
ネットワークが要るので `make gates` では取れない。tag を上げたら上の `gh run list` で
assets image の workflow が success していることを必ず確認し、**失敗していたら fork 側で
回し直す** (`gh workflow run assets-image.yml --repo shiroha-a/misskey-ts -f tag=<tag>`。
`tag` は `required: true` の input で、workflow 自身がその tag を checkout するので `--ref` では代用できない)。
publish されていない tag を pin すると `docker.yml` の `build-and-push-bundled` が `FROM` の
pull で落ちる。実測で `2026.7.0-mk.18` は**タグはあるのに image が無い**状態で残っている
(`2026.7.0-mk.4` 以前は workflow 導入前なので期待どおり)。

**探し方は `grep -rn 'misskey/misskey:' --include='*.yml' --include='*.yaml' --include='*.md' . | grep -v third_party`。**
表を手で追うより確実で、doc の散文に埋まった版数 (`docs/dropin-e2e.md` のトラブルシュート等) も拾える。
**ただし `Dockerfile.bundled` だけは拾えない** — image 名が `misskey-ts-assets` で、
`--include` にも Dockerfile が無い。そちらは `make submodulepin-check` が見る。

**除外リストの「version-gap」注記は、版を揃えたら必ず読み直す。** 実例として、
diff harness の `META_IGNORE` には `app192IconUrl` / `app512IconUrl` /
`singleUserMode` が「mk-go 2026.6.0 が持ち TS 2026.5.4 に無い」として除外されて
いたが、TS を 2026.7.0 に揃えたら 3 件とも残った。実際は upstream では
`admin/meta` にしか無く公開 `/api/meta` には元から含まれない = **mk-go の余剰
フィールド**で、版ずれが誤診断を固定していた (#2303)。

### submodule bump 後に必須: TS baseline で Playwright を回す

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
