# Playwright e2e

Elythia のフロントエンド / API を実ブラウザから検証する e2e。spec は
`tests/playwright/specs/` にあり、現在 301 ファイル
(`find tests/playwright/specs -name '*.spec.ts' | wc -l`)。

Cypress からの移行完了に伴い、frontend e2e はこちらに一本化した (#2437)。本家も
Cypress を廃止して Playwright へ移行しており、参照先が消滅したため Elythia 側の
Cypress ラッパー (`e2e/cypress`) は削除した。

## 実行

```bash
# Elythia backend に対して実行
make playwright-up      # postgres / redis / mkgo を起動
make playwright-test    # spec 実行
make playwright-down    # volume ごと撤去

# Misskey TS backend に対して実行 (期待値そのものの検証)
make playwright-ts-up
make playwright-ts-test
make playwright-ts-down
```

`make playwright-check` は起動から実行までを一括で行う (クリーン DB 前提)。

## なぜ TS backend にも投げるのか

spec は **upstream Misskey TS の API 互換挙動を期待値として書いている**。
Elythia に対してだけ回すと「Elythia がその通り動く」ことしか分からず、**期待値自体が
upstream と食い違っていても気付けない**。

同じ spec を本家 backend にも投げて両方 pass することで、期待値が upstream の
実挙動と一致していることを担保する。

ただし **TS backend は `workflow_dispatch` 専用**で、PR では回らない。TS baseline の
価値は「spec が Elythia の挙動を正解として書かれていないか」を検出する一点にあり、
**upstream が変わらない限り答えも変わらない**ので、常時回す意味が薄いため。追従する本家の版を
上げたときに回す (`docs/upstream-catch-up.md`)。

## CI での扱い

`.github/workflows/playwright.yml` が `pull_request` (paths フィルタ) と
`workflow_dispatch` で発火する。nightly から PR トリガーへ移行済み (#2291)。

**4 シャード並列** (`PLAYWRIGHT_SHARD=i/4`、`fail-fast: false`)。check 名は
`spec (Elythia 1/4)` 〜 `4/4`。

**1 スタックあたりは直列でしか回せない。** 301 spec のうち 180 が共有の root (alice) で
**ブラウザでサインインした状態**を使い (数え方は `grep -rlE 'uiSigninAsRoot|signin-username' tests/playwright/specs --include='*.spec.ts' | wc -l`。
`uiSigninAsRoot` helper が 179 で、`signin.spec.ts` だけ signin フォームを直接駆動する)、
さらに 33 がサインインせず root の token で API を叩く (`root.json` を読むのが 213 で、その差分)。
instance meta は全 spec が共有する。Playwright はファイル単位で並列化する
ので `workers` を上げると `profile_iscat_toggle` と `profile_isbot_toggle` が同じ
アカウントを、`admin_branding_save` と `about_page_render` が同じ meta を取り合う。
**並列度はスタックごと分ける = シャードでしか稼げない** (#2609)。

### shard の割り振り

**shard は所要時間で割り振る** (#4026)。`--shard=i/N` はファイルの並び順で切る
だけなので、軽い `upstream/api` が前半の shard に、重い `upstream/ui` が後半の
shard に集まっていた (直近 3 回で、1/4 が約 100 秒、3/4 と 4/4 が約 430 秒)。
Playwright には時間で割り振る機能が無いので、`tests/playwright/shard-plan.mjs` が
ファイルごとの所要時間 (`tests/playwright/shard-durations.json`) から、合計が揃う
ように割り振る。`playwright.config.ts` は `PLAYWRIGHT_SHARD=i/N` を受け取ると、
自分の shard のファイルだけを流す。手元では `make playwright-test PLAYWRIGHT_SHARD=2/4`。

- 記録の無い新しい spec は、記録の平均の時間として割り振られる。記録の無い spec が
  全体の 2 割を超えるか、消えた spec の記録が残っていると、`shard-plan.test.mjs`
  (playwright workflow の `plan` job) が落ちる
- 記録の作り直し: 全件を流した後の `results.json` から作る。複数の shard の結果を
  まとめて渡してもよい

```bash
make playwright-up
make playwright-test
node tests/playwright/shard-plan.mjs update tests/playwright/results.json
node tests/playwright/shard-plan.mjs show 4   # 各 shard の見込み時間
```

記録は手元で測った時間でよい。CI の時間とは倍率が違うが、割り振りに要るのは
spec の間の比だけ。CI の結果から更新するときは、各 shard の `playwright-json-<backend>-<shard>`
artifact (7 日保持) の `results.json` を渡す。渡した report に無い spec の記録は残る
ので、shard の一部だけを渡してもよい。spec を消したり名前を変えたりしたときは、
report を渡さずに `node tests/playwright/shard-plan.mjs update` を流すと、消えた spec の
記録だけを落とす。

`plan` job は `shard-plan.test.mjs` を流すので、これが無い ref (#4026 より前) は
`workflow_dispatch` で流すと `plan` で落ちる。古い ref を手で流したいときは、その
ref の版の workflow を使う。

### project (`main` と `meta`)

instance meta を書き換える spec (`admin/update-meta` か `admin/roles/update-default-policies`
を呼ぶもの。今は 16) は、
`meta` の project に分けてある。判定は `shard-plan.mjs` の `writesMeta` が自動で
行うので、spec を書く側で振り分ける必要は無い。数え方 (リポジトリの root で):

```bash
node -e "import('./tests/playwright/shard-plan.mjs').then((m) => { const fs = require('fs'); console.log(m.listSpecFiles('tests/playwright').filter((f) => m.writesMeta(fs.readFileSync('tests/playwright/' + f, 'utf-8'))).length); })"
```

`meta` は `main` の後に流れる。workers が 1 なら、project は config に書いた順に
流れる。これは Playwright の文書が保証する挙動ではなく、実装 (test group の並び) に
よるもので、1.62.1 で、順を入れ替えると流れる順も入れ替わることを確かめた。
Playwright の版を上げたら、`PLAYWRIGHT_SHARD` を付けて流し、`[meta]` が最後に並ぶ
ことを見る。meta を戻し損ねた spec があっても、後に走る別の spec を巻き込まない
ようにするため。`meta` を `main` に依存させる (`dependencies`) 形にはしない。
`main` が 1 件でも落ちると、`meta` が丸ごと skip されて見えなくなる。

PR の required check には**含めない**。TS image の pull と spec 増加で実行時間が
伸びるうえ、外部 image 由来の flaky 要素があるため。

失敗時は `tests/playwright/test-results/` (trace / screenshot 含む) と docker
compose logs を `playwright-results-<backend>-<shard>` /
`playwright-logs-<backend>-<shard>` として 14 日保持する。

**録画はしない** (`video: 'off'`)。CI は成功 run の成果物を一切アップロードしないので
録画しても捨てるだけで、失敗 run でも実測 webm 256 本のうち失敗に対応するのは 2 本
だけだった。調査材料は trace が担う。手元で欲しいときは `--video=on` を渡す (#2609)。

## CSP enforce で回す

`tests/playwright/instance.yml` は `frontendContentSecurityPolicy: enforce` を持つ
(#2788)。**実ブラウザでしか分からない CSP 回帰**を spec のゲートにするため。

Go 側の `TestFrontendCSP_HashesCoverRenderedInlineScripts` は HTML と CSP header の
突き合わせで、**ブラウザが実際に script を実行できるか**は見ない。inline event
handler が hash では通らない (`'unsafe-hashes'` が要る、#2786) のように、
ブラウザに実行させないと分からない挙動がある。

**`report-only` では駄目。** あちらは違反を報告するだけで script は動くので、
壊れた状態でも spec は緑になる。`specs/elythia/ui/csp_enforce.spec.ts` は
header が `Content-Security-Policy` (report-only でない) であることを先に確認する
— この確認が無いと `instance.yml` から設定が消えても violation ゼロで緑になり、
「CSP を検査していないのに通る」状態に落ちる。

ゲートとして効いていることは実測済み (7 変異とも spec が落ちる):

| 変異 | 落ちる assertion |
|---|---|
| `instance.yml` を `off` に | CSP header が無い |
| `instance.yml` を `report-only` に | report-only が返っている |
| `script-src` に `'unsafe-inline'` を戻す | `'unsafe-inline'` が戻っている |
| hash を 1 つも登録しない | `script-src` に hash が無い |
| `bootGlobals` を hash 計算から外す | inline script が block された |
| shell に hash 無しの `<script>` を 1 つ足す | inline script が block された |
| `loader.JS` だけ hash から外す (SPA が一切 mount しない) | inline script が block された |

最後の 1 つが**このゲートの主目的そのもの**で、`waitForFunction` の
`TimeoutError` を catch している理由でもある。catch しないと mount 待ちの時間切れが
throw されて assertion に届かず、失敗メッセージが
`page.waitForFunction: Timeout ...` になる — **どの script が何の directive で
落ちたかという一番欲しい情報が、まさにそのときだけ失われる**。

### img-src の違反は既知

全 spec を enforce で回すと `img-src` の violation が 360 件出る (292 spec 時点の実測。以降は再測定していない)。**`script-src` /
`default-src` / `worker-src` はゼロ。** 数え方:

```bash
L='docker logs mk-playwright-mkgo-1'
$L 2>&1 | grep -c msg=csp-report                                     # 総数
$L 2>&1 | grep msg=csp-report | sed -E 's/.*directive=([a-z-]+).*/\1/' | sort | uniq -c
$L 2>&1 | grep msg=csp-report | grep -oE 'blockedUri="?https://[^/"]+' | sed 's|.*//||' | sort | uniq -c
$L 2>&1 | grep msg=csp-report | grep -oE 'documentUri=\S+' | sort | uniq -c
```

内訳:

| 出どころ | 件数 | 実害 |
|---|---|---|
| `/about-misskey` の外部画像 (`assets.misskey-hub.net` 112 + `avatars.githubusercontent.com` 12) | 124 | **クレジット画像が全滅する** |
| spec のフィクスチャが作る実在しないホスト (`example.invalid` / `example.test` / `pwad-*.invalid`) | 236 | 無し。CSP が無くても読めない |

**集計の正規表現でクォートを必須にしないこと。** `slog` の `TextHandler` は値に
`?` や空白を含むときだけクォートする。`avatars.githubusercontent.com/u/...?v=4` は
`blockedUri="https://..."` になるが、`assets.misskey-hub.net/patrons/....jpg` は
クォートが付かない。`blockedUri="https://` で引くと**後者 348 件を丸ごと落として**
「sponsors の違反は出ていない」という誤った結論になる (実際に踏んだ)。上の
コマンドが `"?` にしてあるのはこのため。

`/about-misskey` を単独で開いて 8 秒待つと **62 件**で、
`frontend/packages/frontend/src/pages/about-misskey.vue` の外部 `<img>`
の枚数と一致する (contributor 6 + sponsors 6 + patron 50)。`loading="lazy"` も
`v-if` も折りたたみも無いので、ページを開いた時点で全部読まれる。上の 124 件は
spec が `/about-misskey` を 2 回開いた時点の実測で、**#2700 の
`specs/elythia/ui/about_elythia.spec.ts` が 2 回開くようになったので現在は 4 回**
(件数は 248 前後になる)。**開く回数を変えたらこの数も動く** — 内訳の妥当性は
「62 × 開いた回数」で確かめること。

**任意 origin は開かない。** `img-src` を `'self' data: blob:` に絞るのはリモート画像を
media proxy 経由にする設計と対で、外部 origin をワイルドカードで許すと投稿経由で
トラッキング画像を読ませる経路が開く。**#2892 で足したのは固定の 2 host だけ**で、
ユーザーが URL を指定できる経路 (投稿・カスタム絵文字・アバター) からそこへ到達する
手段は無い (リモート画像はいずれも proxy 経由になる)。

**#2892 で解消済み。** `img-src` に `avatars.githubusercontent.com` と
`assets.misskey-hub.net` を足したので、`/about-misskey` 由来の違反は出なくなる
(`creditImageOrigins`、`internal/server/frontend_csp.go`)。**消える件数は上の表の 124 では
なく、開く回数が 4 になった現在は 248 前後** (1 回あたり 62 枚)。**足すのは固定の 2 origin だけ**で、投稿から任意の
host を読ませる経路は開かない。**上の内訳と件数は #2892 より前の実測**なので、
再測するときは残る 236 件 (spec のフィクスチャが作る実在しないホスト) が基準になる。

なぜ CSP 側で解いたか。#2700 が `/about-mkgo` を作ったときに `about-misskey.vue` を
作り直す案もあったが、**upstream のプロジェクトメンバー・スポンサー・パトロンは
消さない**方針を採った (upstream が頻繁に更新するファイルなので、書き換えると追従の
たびにコンフリクトを手で解くことになる。[乖離一覧](divergence/frontend.md) の `2026.9.0-mk.3`
の行)。**media proxy 経由にも落とせない** — Elythia の proxy は upstream と違い open
proxy ではなく、allowlist は DB に実在する URL だけを通すので、静的にハードコード
された URL は 403 になる (実測)。謝辞を残す以上、CSP を足す以外に表示させる道が無い。

Elythia 独自の `/about-elythia` (#3394 より前は `/about-mkgo`) は外部画像を持たない
(Elythia-Network への案内のアイコンは同梱のアセット)。

### embed も enforce の対象

`/embed/` にも同じ CSP が付く (#2789)。既存の `specs/upstream/ui/embed/embed.spec.ts`
が **iframe を張って投稿本文が読めること**まで見るので、CSP で bundle や inline
script が block されればそこで落ちる。実測: embed の inline script を hash 計算から
外すと「iframe 内で描画され本文が読める」が `element(s) not found` で落ちた。

embed 経由の violation は **0 件** (数え方:
`docker logs mk-playwright-mkgo-1 2>&1 | grep msg=csp-report | grep -oE 'documentUri=\S+' | grep /embed/`)。

**ただし spec が開くのは本文だけのローカル投稿の embed 2 ページ。** メディア添付・
引用・カスタム絵文字を含む embed は未検証で、object storage 構成の
`img-src` / `media-src` / `connect-src` も実ブラウザでは踏んでいない
(Go 側の `TestEmbedShell_CSP` が header の値だけを固定している)。

### ゲートの守備範囲

`csp_enforce.spec.ts` が見るのは **未サインインの `/` が mount を終えた時点**まで。
mount 後に遅延ロードされる chunk (route 単位の code splitting、shiki の
`https://esm.sh` 動的 import) やサインイン後の画面で起きる違反は数えない。
issue #2788 の主目的 (shell に hash 無しの inline script が入る) はこれで足りる。

## spec を書くときの注意

`_spec.ts` は silent skip される。ファイル名は必ず `.spec.ts`。

vite の hash class を selector に使わない (`[class*="_button_"]` 等)。production
ビルドで hash が変わると落ちる。`data-testid` か role / text で取る。

**ルートを開いたら、not-found のページでないことを確かめる。** router は見つからない
パスを not-found のページに回すので、status (常に 200) やナビの有無では区別できない。
ルートは `fixtures/ui_route.ts` の `gotoRoute(page, baseURL, path)` で開く (#4026)。開いて、
応答が 200 であることと、not-found のページでないこと (`expectRoutedPage`) を確かめる。
`expectRoutedPage` は、起動画面が消え、通信が止むのを待ってから、not-found のページの title
かどうかで判定する。title の形だけを待つと、サーバーが title を描くルート (`/@:acct`、
`/notes/:id` など) では mount の前に通ってしまう。`page.goto` で開いた直後に呼ぶ (SPA の中の
遷移では、前のページの状態が残る)。#4025 までは 18 パスが存在しないパスを開いて緑になって
いた。判定そのものは `specs/elythia/ui/route_not_found_guard.spec.ts` が、存在しないパスで
落ち・実在するパスで通ることを確かめている。

**サインインは helper を通す。** root なら `uiSigninAsRoot` (`fixtures/ui_auth.ts`)。フォームは
通さず、globalSetup が 1 回だけフォームでサインインして保存した state
(`.auth/root-state.json`) を `context.setStorageState()` で読み込む (#4026)。root の
アカウントの状態 (プロフィール、設定) を変える spec や、他の spec と同じ値を取り合う spec は、
`signupUser` で利用者を作って `uiSigninAs(page, request, baseURL, user)` でサインインする
(初回のウィザードも抑止する)。登録のレート制限は両方の backend で切ってある
(`instance.yml` の `disableEndpointRateLimits`、TS は `NODE_ENV`) ので、利用者を作る数は
気にしなくてよい。helper が本当にその利用者になるかは
`specs/elythia/ui/signin_state_guard.spec.ts` が、ページの token でサーバーに問い合わせて
確かめている。

**見た目の差分の基準画像は runner の container で作る。** `toHaveScreenshot` の基準画像は
`make playwright-update-snapshots PLAYWRIGHT_ARGS=<spec>` で作り直す。手元の OS で作ると
文字の描画が違い、CI で必ず差分になる。

**要素は文字で、完全一致かつちょうど 1 つで取る。** スイッチは `clickSwitchByLabel`
(`fixtures/ui_click.ts`)、MkFolder の中の要素は `openFolderByLabel` / `fillFolderTextarea` /
`clickFolderButton` (`fixtures/ui_folder.ts`)、MkInput は `fillInputByLabel`
(`fixtures/ui_input.ts`)。「含む」で最初のものを取ると、「Prohibited words」と
「Prohibited words for usernames」のように前方が同じものを取り違える。ラベルは画面に
出る文字の全体を渡す (`(Recommended)` のような後ろの文字も含める)。

**保存の spec は、値を変えて、読み直して確かめる。** 保存の応答が返るのを待つだけだと、
値を変えずに押しても、別の項目を書いても緑になる。`meta` を書き換える spec は、始める
前の値を覚えておき `finally` で戻す (他の spec と共有しているため。空の値に戻すと既定値を
消す)。

**同種の要素が複数あるとき、DOM 上の位置（先頭・末尾）で取らない。**
`document.querySelector('textarea')` や `querySelectorAll(...)[length - 1]` は、
フォームに項目が 1 つ増えただけで別フィールドを指す（例: 通報フォームの
必須 `details` と任意 `evidence` の 2 textarea）。`data-testid` を付けるか、
ラベル文字列から親要素を辿って `textarea` を取る。

Playwright は **required check ではない**（`spec (Elythia …)` は PR で走り、落ちれば
check に fail が出るが、マージはブロックされない）。paths フィルタ外の変更だけだと
workflow が発火しないので、fixture を触った PR では check を見る。手元では
`make playwright-check` でも再現できる。spec 追加時は上の規約を守る。

**`.ts` の隣に `.js` を残さない。** import は拡張子なし
(`from '../../../../fixtures/rate_limit'`) で、同名の `.js` があると playwright は
そちらを先に解決する。`tsc` を手で走らせた残骸が典型で、import 先だけでなく
**spec 本体の `.js` も出る** — そちらは shadow されず `.ts` と両方走る (既定の
`testMatch` が `.js` も拾う)。**エラーの有無で切り分けない**
— 症状が「`.ts` を直したのに効かない」だけのこともあれば、欠けた export が
`undefined` として流れて無関係に見える場所で落ちることもある。

`tests/playwright/.gitignore` の `*.js` が止めるのは commit までで、**手元の
shadowing は止まらない** (生成された時点で import 先の `.ts` は読まれていない)。container 実行
(`make playwright-check`) も spec を bind mount するので同じ。疑ったら探して `rm`:

```bash
find tests/playwright -name '*.js' \
  -not -path '*/node_modules/*' -not -path '*/playwright-report/*'
```

素の `git status` には出ず (`--ignored` が要る)、`git clean -fd` も消してくれない。
`playwright-report/` は `--reporter=html` を手で渡したときだけ生成されるが、その
viewer 資産で `.js` が 7 本出るので除外に入れてある。

3 件とも実際に踏んだ罠。

## 関連

- [ci.md](ci.md) — CI 全体の構成
- [dropin-e2e.md](dropin-e2e.md) — drop-in 切替の検証 (別系統)
- [upstream-backend-e2e.md](upstream-backend-e2e.md) — 本家の backend e2e を Elythia に向ける (別系統)
