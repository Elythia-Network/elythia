# Playwright e2e

Elythia のフロントエンド / API を実ブラウザから検証する e2e。spec は
`tests/playwright/specs/` にあり、現在 364 ファイル
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

### Elythia で足した `data-testid` を使う spec は、公式 image では落ちる

公式 image は本家の frontend を配るので、Elythia の frontend で足した `data-testid` が無い。
`specs/upstream/ui/` の spec のうち、本家にもある要素を、Elythia で足した `data-testid` で
取っているものは、**TS backend の実行では要素が見つからずに落ちる。既知の例外として扱い、
spec を `elythia/` へ移したり、文言で取り直したりはしない。**

2026-11-06 の時点で 35 ファイルあり、すべて #4024 の拡充で足した spec (#4043 が 20、#4046 が 7、
#4049 が 8)。ノートの操作 (メニュー・リアクション・画像・投票)、投稿フォーム、タイムラインと
通知のタブ、通知の行、絵文字ピッカー、ドライブ、設定の検索、ダイアログのボタンなどが該当する。

数え方: 本家 (`.cache/misskey/<版>/packages/frontend/src`) と Elythia
(`frontend/packages/frontend/src`) の `data-testid="…"` の差を取り (67 種)、その名前を
引用符付きで含む `specs/upstream` の spec と、`fixtures/ui_drive.ts` を使う spec を数える。
テンプレートで作る名前 (通知の行の `notification-<種類>`、公開範囲の `visibility-<範囲>`) と、
ページの見出しのタブの `testId` (`timeline-tab-*` / `notifications-tab-*`。#4046 で足した
仕組みで、本家には無い) を使う spec も足す。増やしたら、この数を直す。

- 置き場所は、検証する機能が本家にもあるかで決める (README の「境界」)。取り方の都合で
  `elythia/` へ移すと、本家にもある機能の spec が Elythia 独自に見えてしまう
- 文言で取る形へ切り替えないのは、文言は locale と本家の版で変わり、同じ文言の別の要素も
  拾いやすいため
- **例外にできるのは、本家にもある要素に `data-testid` だけを足したものに限る。** Elythia
  独自の要素 (独自機能のボタンなど) を取る spec は、例外ではなく `elythia/` に置く
- **この 35 ファイルは、`data-testid` より後ろの期待値も本家と照合されない。** TS backend では
  要素を取る段で落ちるため。たとえば `note_poll_vote` は TS のときだけ本家の正しい
  `isVoted` (#4044 の修正後の形) を確かめる分岐を持つが、その前の `poll-choice` で落ちるので、
  分岐はどの経路でも実行されない。画面の振る舞いだけでなく、UI の spec の中で叩いている
  API の期待値も照合されない (`isVoted` は、ほかの層に検査が無い)
- TS backend の実行で落ちたら、落ちた locator の `data-testid` が本家の frontend にあるかを
  見る。fixture の中で取っているもの (`ui_drive.ts` の `drive-file` など) や、テンプレートで
  作る名前は、落ちた行を見ただけでは名前が分からないので、呼び出し元まで辿る。本家に無ければ
  この例外で、あれば期待値と本家の挙動の食い違いとして調べる

## CI での扱い

`.github/workflows/playwright.yml` が `pull_request` (paths フィルタ) と
`workflow_dispatch` で発火する。nightly から PR トリガーへ移行済み (#2291)。

**7 シャード並列** (`PLAYWRIGHT_SHARD=i/7`、`fail-fast: false`)。check 名は
`spec (Elythia 1/7)` 〜 `7/7`。

**1 スタックあたりは直列でしか回せない。** 364 spec のうち 185 が共有の root (alice) で
**ブラウザでサインインした状態**を使い (`uiSigninAsRoot` helper が 184 で、`signin.spec.ts` だけ signin フォームを直接
駆動する。数え方は `grep -rl 'uiSigninAsRoot' tests/playwright/specs --include='*.spec.ts' | wc -l` に 1 を足す。フォームから入る spec はほかにもあるが、
自分で作った利用者で入るので数えない)、さらに 39 がサインインせず root の token で API を叩く (`root.json` を
読み、root でサインインしないもの。数え方は `grep -rl 'root.json' tests/playwright/specs --include='*.spec.ts' | xargs grep -L 'uiSigninAsRoot' | grep -v '/signin.spec.ts$' | wc -l`)。
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

**`workflow_dispatch` の `ref` には SHA を渡す。** shard の job はそれぞれ `ref` を checkout
するので、ブランチ名を渡すと、job の待ち時間の間にブランチが進んだとき、shard ごとに違う
commit を見て、spec が漏れたり 2 回流れたりする。pull_request では `github.sha` (event の
時点の merge commit) を取るので起きない。

### shard の数

**今は 7** (#4043 で 4 から 6 に、#4049 で 7 に上げた)。目安は「いちばん遅い shard の spec の実行が 300 秒を超えたら、数を見直す」。

経緯:

- #4026 (PR #4031) で所要時間で割り振るようにした時点は 4 で、spec の実行は shard ごとに 148〜182 秒、
  job 全体は 258〜335 秒だった。6 にしても、いちばん遅い job は約 75 秒しか縮まない見込みだった (job 1 つに
  環境の起動と後片付けの固定の時間が約 130〜150 秒かかるため)。PR 1 件で動く job (約 30) はすでに GitHub の
  同時実行の上限 (Free で組織全体 20) を超えているので、増やすと待ち行列が伸びる
- #4041 (PR #4042) で全ルートの表 (7 ファイル、手元で計 658 秒) を足すと、4 shard の spec の実行は
  327・331・266・284 秒になり、目安を超えた。#4043 でさらに 24 ファイル (手元で計 116 秒) を足すので、
  6 にした
- #4049 (PR #4050) で 18 ファイルを足すと、6 shard の spec の実行は 270〜353 秒になった。所要時間の記録を
  CI の値で取り直すと見込みは各 shard 約 298 秒で、次の run でも 238〜333 秒と目安を超えた。1 回ごとに
  振れる spec がある (`notification_types_render` は 1 回目の run で 52 秒、2 回目で 25 秒) ので、見込みが
  目安に張り付くと毎回超える。7 にした (見込みは最大約 262 秒、ほかの shard は約 255 秒)
- **7 より増やしても、いちばん遅い shard は約 262 秒より縮まない。** `route_table_permission_signed_out`
  (全ルートの表の未ログインの権限) が 1 ファイルで約 262 秒かかり、7 では 1 つの shard をこれだけが占める。
  ほかの shard が目安を超えたときは、shard を増やせば効く。この spec だけの shard が目安に近づいたら、
  shard を増やしても縮まないので、この spec を分ける

6 にした後の実測 (#4043 の PR の run):

| | 1/6 | 2/6 | 3/6 | 4/6 | 5/6 | 6/6 |
|---|---|---|---|---|---|---|
| spec の実行 (`results.json` の `stats.duration`) | 271 秒 | 215 秒 | 263 秒 | 203 秒 | 279 秒 | 244 秒 |
| job 全体 | 526 秒 | 463 秒 | 519 秒 | 414 秒 | 545 秒 | 477 秒 |

この実測から、所要時間の記録 (`shard-durations.json`) を CI の値で取り直した。手元で測った値より CI の方が遅く
(全ルートの表の 7 ファイルで 658 秒に対して 749 秒、約 1.14 倍)、手元の値で割り振った見込み (最大 228 秒) より実測が長かったため。

数を見直すときは、`playwright.yml` の `SHARD_TOTAL` を変えた PR の run で、上の表と同じ値を測って書き換える。

### project (`main` と `meta`)

instance meta を書き換える spec (`admin/update-meta` か `admin/roles/update-default-policies`
を呼ぶもの。今は 18) は、
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

## ルートと spec の対応付け (#4035)

frontend のルートを足したのに spec を書かないと、`make playwright-route-check` が落ちる。CI では
required の `frontend` (`frontend-lint` job) で回り、手元では `make frontend-check` からも呼ばれる。
spec の一覧を playwright に解かせるので、`tests/playwright` で `npm ci` しておく (サーバーと
ブラウザは要らない)。

**spec の側で宣言する。** `test()` の第 2 引数に `coversRoutes(...)` (`fixtures/ui_route.ts`) を渡す。

```ts
test('the note page shows the reply', coversRoutes('/notes/:noteId/:initialTab?'), async ({ page, baseURL }) => {
```

- パスは `router.definition.ts` の `path` と同じ文字列で書く。子のルートは親と連結する
  (`/settings/profile`、`/admin/roles/:id`)。子の `/` は親と同じなので、親のパスで書く
- **宣言してよいのは、そのルートを開いて、そのページに固有のものを確かめるテストだけ。**
  見出しや `data-testid`、seed した中身、そのページが呼ぶ API、そのページでの操作の結果を
  見ていること。ナビが見える、本文が何文字ある、not-found でない (`gotoRoute` だけ)、では
  宣言しない。ゲートは宣言の有無しか見ないので、ここはレビューで受ける
- **どのページにも出るもので判定しない。** サインイン中は navbar に自分の名前が、画面が広いと右の
  ウィジェット列に通知 (フォローした人の名前、フォローリクエストの承認・拒否のボタンまで) が出る。
  未サインインで幅が広いと、左のダッシュボードにインスタンス名 (未設定なら host) とローカル
  タイムラインが出る。これらと同じ名前・本文・ボタンで判定すると、not-found のページでも通る。
  #4035 で既存の spec に宣言を付けたとき、この形のテストが 11 本あり、宣言を外した (ほかに、管理画面の
  メニューと同じ見出しの文言だけで判定していた 4 本も外した)
- `test.only` が残っていると、ゲートは exit 2 で落ちる (`--forbid-only`)
- 1 つのテストで複数のルートを確かめるなら並べる (`coversRoutes('/a', '/b')`)
- 既に別のオプションを渡しているなら混ぜる (`{ ...coversRoutes('/a'), tag: '@x' }`)。既に `annotation` を渡しているテストでは、
  そのまま混ぜると後に書いた方が上書きする。配列をつなぐ (`annotation: [...coversRoutes('/a').annotation, ...]`)
- `test.skip` / `test.fixme` / `test.describe.skip` のテストの宣言は数えない (一覧で skipped になるもの)。
  実行時の `test.skip(条件)` は一覧からは見えないので、宣言するテストでは使わない

**宣言できないルートは、2 つのリストのどちらかに載せる。** どちらも `<ルート>\t<2 列目>` の TSV。

| リスト | 載せるもの | 2 列目 |
|---|---|---|
| `tests/playwright/route-coverage-allowlist.tsv` | ずっと spec を付けないルート | どの経路でそうなるか (例: `/oauth/authorize` はサーバーが受け、外のクライアントの情報を取れたときだけ描く) |
| `tests/playwright/route-coverage-pending.tsv` | これから spec を付けるルート | 付ける予定の issue の番号 (`#4024(15)` のように親と段階でもよい) |

ゲート (`tests/playwright/route-coverage.mjs`) が落とすのは次のとき。

- 宣言も無く、どちらのリストにも無いルートがある
- 宣言やリストの行が、`router.definition.ts` に無いルートを指している (ルートの改名で古くなった)
- 宣言のあるルートがリストにも載っている。spec を付けたら、リストの行を消す
- 同じルートが 2 つのリストの両方にある
- git が追跡していない spec の宣言 (数えない。手元でだけ緑になるのを防ぐ)
- 2 つのリストに無いルートを、全ルートの表 (下の節) の spec が宣言していない。ほかの spec が宣言
  していても落ちる (表に行を足し忘れたルートを拾う)
- skip / fixme のテストの宣言 (数えない)
- `tests/playwright` に、git が無視する `.js` が残っている (exit 2)。`.ts` の spec や fixture より先に読まれ、
  手元と CI で一覧が食い違うため (下の「spec を書くときの注意」)
- 何も拾えなかった (exit 2)。ルートが 150 未満のとき、目印の 4 つ (`/notes/:noteId/:initialTab?`
  `/settings/profile` `/admin/roles/:id` `/:(*)`) を拾えなかったとき、宣言が 1 件も無いとき

ルートの一覧は `ROUTE_DEF` を TypeScript の構文木で辿って作る。`path` を式で組み立てたときと、
ルートの中に `{ children }` や `...rest` を書いたときは、読めないので落とす。プラグインのルート (`router.ts` の `routeDefsWithPlugins`) は対象にしない。
どのプラグインを入れるかが運営者ごとに違い、`ROUTE_DEF` に現れないため。

## 全ルートの表 (#4041)

frontend の全ルートを、表 (`fixtures/route_table.ts`) 1 つで次の 2 点について確かめる。ルートを足したら
表にも 1 行足す (足さないと、上の対応付けのゲートが落とす。表の spec の宣言を、ほかの spec の宣言とは
別に数えている)。

- **開ける** (`specs/elythia/ui/route_table_open_<領域>.spec.ts`): root で開き、not-found でないこと、
  開いたパスに留まること、そのページの目印 (marker) が出ること、page error が無いことを見る。
  1 つのファイルで 5 件落ちたら、残りは開かずに落とす (stack が壊れているときに、時間切れの積み重ねで
  CI の job の時間を超えないように)。数は `.auth/route-table-failures/` に置く (Playwright はテストが落ちる
  たびに worker を作り直すので、変数では数えられない)。globalSetup が実行ごとに消す
  リダイレクトだけのルートは、行き先へ移ることを見る
- **権限の無い人の扱い**: 未ログイン (`route_table_permission_signed_out.spec.ts`) で、公開のルートは
  開け、ログイン必須のルートは not-found とサインインのダイアログになり、モデレーターのルートは
  not-found になること。ロールの無い利用者で `/admin*` が、チャットを使えないロールの利用者で
  `/chat*` が not-found になること (`route_table_permission_users.spec.ts`)。本当の防御はサーバーの
  認可で、ここで見るのは「画面が出てしまわない」ことだけ

表の書き方:

- **kind は `router.definition.ts` から作らず、手で書く。** 作ると `loginRequired` や `iAmModerator` の
  書き忘れを表も同じく忘れて、拾えない。実際、表を書いたときに 6 ルート (`/registry` など) を
  ログイン必須と思い込んでいて、未ログインの spec で食い違いが見つかった (どれもルートの側が公開で、
  ページが自分でサインインを促す)
- **marker は、そのページが mount したときにだけ出るもので書く。** `title` (ページが `definePage` で
  付ける title)、`text` (ページの本体 = RouterView の `._pageContainer` の最も内側の innerText)、
  `api` (開くときに呼び、2xx が返る API)、`testId` の組み合わせで、書いたものは全部を満たす必要がある。
  設定と管理画面の子ページは title が親 (`Settings` / `Control Panel`) と共通なので、本文で見分ける。上の
  「どのページにも出るもので判定しない」の罠に加えて、次に注意する
  - 親が呼ぶ API は子の目印にならない (管理画面は、どの子を開いても `admin/abuse-user-reports` を呼ぶ)
  - 設定の子ページの本体には、設定のメニューと案内も入る (`settings/index.vue` が自分の `._pageContainer`
    を持たない)。メニューと同じ文言 (Profile、Privacy など) を子の `text` にすると、どの子でも通る
  - パラメータの先が無いときに「見つかりません」を出すページは、title と API の呼び出しだけでは中身を
    見ていない。先があるときにだけ出るもの (相手の名前など) を `text` に足す。テストの stack に作れない
    もの (`/instance-info/:host` の連合先) は、表のコメントに「mount と照会まで」と書いてある
- パラメータは `fixtures/route_seed.ts` が作ったもので埋める。seed は実行ごとに 1 回だけ作って
  `.auth/route-seed.json` に残し、spec のファイルの間で使い回す (ファイルごとに作ると webhook などの
  上限に当たる)。globalSetup は実行の始めに root のアンテナ・webhook・クリップ・リストを消すので、
  同じ所で seed も消す
- `/settings` と `/admin` は、広い画面だと最初の子へ移るので、狭い画面で開く (`narrow`)
- 未ログインで開くと中身が変わる公開のルートは、違いを `signedOut` に理由と一緒に書く (`noTitle`: title を
  付けない、`pageError`: 出る page error の文面の正規表現)。書いた違いは実際に起きることも見るので、直ったら
  行から消す。`pageError` を書いた行でも、正規表現に合わない page error が出たら落ちる。
  今は `/registry` (3 つ)・`/invite`・`/reversi` が、未ログインでもログインの要る API を呼んで失敗を
  受けない (本家から来たページ)
- page error のうち、どのページでも起きるもの (stack の自己署名の証明書で Service Worker の登録が
  失敗する) は `fixtures/route_check.ts` の `KNOWN_PAGE_ERRORS` で除く

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
