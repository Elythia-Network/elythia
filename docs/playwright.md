# Playwright e2e

Elythia のフロントエンド / API を実ブラウザから検証する e2e。spec は
`tests/playwright/specs/` にあり、現在 397 ファイル
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

2026-11-08 の時点で 42 ファイルあり、すべて #4024 の拡充で足したか書き換えた spec (#4043 が 20、#4046 が 7、
#4049 が 8、#4072 が 7)。ノートの操作 (メニュー・リアクション・画像・投票)、投稿フォーム、タイムラインと
通知のタブ、通知の行、絵文字ピッカー、ドライブ、設定の検索、ダイアログのボタン・入力欄、テーマの管理と
ステータスバーの設定の画面などが該当する。

数え方: 本家 (`.cache/misskey/<版>/packages/frontend/src`) と Elythia
(`frontend/packages/frontend/src`) の `data-testid="…"` の差を取り (2026-11-08 の時点で 145 種。`:data-testid` とテンプレートで作る名前は除く)、その名前を
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
- **この 42 ファイルは、`data-testid` より後ろの期待値も本家と照合されない。** TS backend では
  要素を取る段で落ちるため。たとえば `note_poll_vote` の1つ目のテストは、投票した後に
  `notes/show` の票数を確かめるが、その前の `poll-choice` で落ちるので、票数は本家と照合されない。
  画面の振る舞いだけでなく、UI の spec の中で叩いている API の期待値も、`data-testid` を取る
  行より後ろにあれば照合されない
- TS backend の実行で落ちたら、落ちた locator の `data-testid` が本家の frontend にあるかを
  見る。fixture の中で取っているもの (`ui_drive.ts` の `drive-file` など) や、テンプレートで
  作る名前は、落ちた行を見ただけでは名前が分からないので、呼び出し元まで辿る。本家に無ければ
  この例外で、あれば期待値と本家の挙動の食い違いとして調べる

## CI での扱い

`.github/workflows/playwright.yml` が `pull_request` (paths フィルタ) と
`workflow_dispatch` で発火する。nightly から PR トリガーへ移行済み (#2291)。

**11 シャード並列** (`PLAYWRIGHT_SHARD=i/11`、`fail-fast: false`)。check 名は
`spec (Elythia 1/11)` 〜 `11/11`。

**1 スタックあたりは直列でしか回せない。** 397 spec のうち 194 が共有の root (alice) で
**ブラウザでサインインした状態**を使い (`uiSigninAsRoot` helper が 184、管理画面の表の helper (`fixtures/admin_table.ts` の
`signinAdmin`) を通るものが 9 で、`signin.spec.ts` だけ signin フォームを直接
駆動する。数え方は `grep -rlE 'uiSigninAsRoot|signinAdmin|runMetaRow|runPolicyRow' tests/playwright/specs --include='*.spec.ts' | wc -l` に 1 を足す。フォームから入る spec はほかにもあるが、
自分で作った利用者で入るので数えない)、さらに 45 が root ではサインインせずに root の token で API を叩く (`root.json` を
読み、root でサインインしないもの。数え方は `grep -rl 'root.json' tests/playwright/specs --include='*.spec.ts' | xargs grep -LE 'uiSigninAsRoot|signinAdmin|runMetaRow|runPolicyRow' | grep -v '/signin.spec.ts$' | wc -l`)。
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

**今は 11** (#4043 で 4 から 6 に、#4049 で 7 に、#4056 で 11 に上げた)。目安は「いちばん遅い shard の spec の実行が 600 秒を超えたら、数を見直す」
(#4066 で 300 秒から変えた。下の経緯)。

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
- #4056 (PR #4060) で設定の表 (18 ファイル、手元で計 12.0 分) を足すと、7 shard の spec の実行は
  265〜482 秒になった (足したファイルは所要時間の記録が無く、平均の値で割り振られたため偏った)。
  所要時間の記録をその run の値で取り直すと、見込みは 7 で各約 400 秒、10 で約 280 秒、11 で約 254 秒。
  10 では上の振れで目安を超えるので、11 にした
- #4066 (PR #4069) で管理画面の表 (9 ファイル) を足すと、11 shard の spec の実行は 226〜415 秒、job 全体は
  394〜643 秒になった。取り直した記録での見込みは 11 で約 302 秒、13 で約 256 秒。ただ、required の `test` の
  shard は同じ run で最長 707 秒 (`test-shards (3)`) かかっており、Playwright の job がそれより長くなるまでは、
  shard を増やしても PR の待ち時間は縮まらない (増やすと job が増え、同時実行の上限で待ち行列が伸びる)。
  目安を 600 秒にして、11 のままにした
- #4072 の A (PR #4073) で Elythia 独自の画面の操作の spec (8 ファイル) を足すと、11 shard の spec の実行は
  272〜350 秒、job 全体は 443〜594 秒になり、目安に収まった。取り直した記録での見込みは 11 で約 296 秒
- #4072 の B (PR #4075) で本家のルートの操作のテストを足すと、11 shard の spec の実行は 280〜355 秒、
  job 全体は 449〜548 秒になった。取り直した記録での見込みは 11 で約 312 秒
- #4072 の C (PR #4080) で中・低の優先度の操作のテストを足すと、11 shard の spec の実行は 281〜392 秒、
  job 全体は 444〜577 秒になった。取り直した記録での見込みは 11 で約 325 秒
- **ファイル 1 つより短くはならない。** いちばん長い `route_table_permission_signed_out`
  (全ルートの表の未ログインの権限) は、PR #4080 の run で約 238 秒かかった。ほかの shard が目安を超えたときは、
  shard を増やせば効く。この spec を含む shard が目安に近づいたら、shard を増やしても縮まないので、
  この spec を分ける

直近の実測 (#4072 の C の PR #4080 の run。中・低の優先度の操作のテストを足した後):

| | 1/11 | 2/11 | 3/11 | 4/11 | 5/11 | 6/11 | 7/11 | 8/11 | 9/11 | 10/11 | 11/11 |
|---|---|---|---|---|---|---|---|---|---|---|---|
| spec の実行 (`results.json` の `stats.duration`) | 309 秒 | 357 秒 | 392 秒 | 324 秒 | 329 秒 | 340 秒 | 281 秒 | 339 秒 | 352 秒 | 305 秒 | 327 秒 |
| job 全体 | 493 秒 | 548 秒 | 577 秒 | 515 秒 | 529 秒 | 523 秒 | 444 秒 | 534 秒 | 545 秒 | 475 秒 | 505 秒 |

所要時間の記録 (`shard-durations.json`) は、この run の値で取り直したもの。手元で測った値は CI より短い
(#4043 のとき、全ルートの表の 7 ファイルで手元 658 秒に対して CI 749 秒、約 1.14 倍) ので、spec を足した
PR では、その PR の run の値で取り直す。

数を見直すときは、`playwright.yml` の `SHARD_TOTAL` を変えた PR の run で、上の表と同じ値を測って書き換える。

### project (`main` と `meta`)

instance meta を書き換える spec (`admin/update-meta` か `admin/roles/update-default-policies`
を呼ぶもの、管理画面の表の `runMetaRow` を呼ぶもの。今は 27) は、
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

## 見た目の差分 (#4054)

主な画面を、ライトとダークで撮って基準画像と比べる (`toHaveScreenshot`)。CSS の崩れは操作の spec では
落ちないため。撮る画面は #4054 に挙げた 22 項目で、1 項目を何枚かに分けて撮るものがあるので、基準画像は
62 枚 (`find tests/playwright/specs/elythia/ui/visual -name '*.png' | wc -l`)。spec は
`specs/elythia/ui/visual/` の 5 ファイル、helper は `fixtures/visual.ts`。基準画像は各 spec の隣の
`<spec のファイル名>-snapshots/` に置き、git で追跡する。TS backend の run では流さない
(frontend が同じなので)。

### 基準画像を作り直す

- **意図して見た目を変えた PR では、基準画像も作り直して同じ PR に入れる。** `make playwright-up` の後に
  `make playwright-update-snapshots PLAYWRIGHT_ARGS=specs/elythia/ui/visual/` を流し、書き直された画像を
  見てからコミットする。モードは `changed` で、基準としきい値の中で一致するものは書き直さない
- 手元の OS で作らない (下の「spec を書くときの注意」)。runner の container で作った印として、名前の
  末尾に `-linux` が付く
- 同梱しないプラグインを入れたチェックアウトで作らない。`server-plugins.generated.ts` がプラグインを
  import していると、CI (プラグイン無し) と画面が変わりうる。プラグインの無い worktree で作る
- Playwright の版を上げると Chromium の描画が変わるので、全部作り直す
- CI で落ちたときの画像 (`*-expected.png` / `*-actual.png` / `*-diff.png`) は、`playwright-results-*` の
  artifact に入る
- 名前に project を入れない (`playwright.config.ts` の `snapshotPathTemplate`)。spec が meta を書くように
  なると、project が `main` から `meta` に変わり、基準画像を見失うため

### 毎回同じ絵にするための決まり

- **撮るのは固定の利用者 (`vr_alice` / `vr_bob` / `vr_carol` / `vr_dave`) と、その人たちだけが作った
  データ。** 種まきは 1 つの DB で 1 回だけ (alice の自己紹介が印)。ホームのタイムラインに他の spec の
  ノートが混ざらないよう、3 人は互いにだけフォローする。名前は固定なので、同じスタックで 2 回目を流すと
  利用者を作り直さずに使う
- **Redis にしか無いものを絵に入れない。** globalSetup は流すたびに Redis を空にするので、同じスタックの
  2 回目で消える。該当するのは次の 2 つ
  - 通知: ふだんは空にしておき、通知の画面のテストが作ってから撮り、撮った後に消す
  - ホームのタイムラインの「自分への返信」: Redis の記録からは出るが、記録が無いときの DB からの
    組み立てでは出ない (本家も同じ)。種まきに作らない
- 撮るたびに変わる文字は場所ごと消す (`display: none`)。`<time>` と、`<time>` の外に出る en-US の
  書式の日時 (ノートの見出し、登録日)、ドライブの作った月の見出し (`2026/11`)、about の版 (`2.1.0`)。場所を保って見えなくすると、文字の長さ (`3m ago` と `10m ago`) で幅が
  変わって隣がずれる。mask (四角で塗る) は、要素がダイアログやツールチップの背後にあっても一番上に
  描かれ、手前のものを隠すので使わない
- 他の spec のデータで変わる部分も隠す。入口の流れるタイムライン・人数・連合先は、登録のダイアログの
  背後に回るので、場所を保ったまま見えなくする (`visibility: hidden`。`data-testid` で取る。クラス名は本番のビルドでハッシュになるので使えない)。
  about の統計と管理画面のダッシュボードは、上に何も重ならないので mask で塗る。絵文字ピッカーは
  「Custom Emoji」の見出しより上だけを撮る
- 撮る前に起動の準備をする (`prepareVisualPage`): ウィジェットの既定 (カレンダー・トレンド) の差し替え、
  ソースコードの案内を出さない、`Math.random` の固定。サインインの後に「Tips & Tricks」を全部閉じる。
  ログインと実績の通知は受け取らない (サインインと画面を開いた順で増える)
- アニメーションは、OS の「動きを減らす」(prefer の `animation` の既定がこれに従う) と
  `animations: 'disabled'` で止める
- ページを開いた直後は、通信が静まるのを待つ。同じページで操作した後は、出てくる要素を spec で待ち、
  `toHaveScreenshot` が 2 枚続けて同じになるまで撮り直すのに任せる (`networkidle` は、一度静まった
  文書ではすぐに返る)。画面の中の画像の読み込みも待つ (画面の外の遅延読み込みの画像は待たない)
- 管理画面のトップは、未解決の通報の警告を出さない (`admin/abuse-user-reports` を空で返す)。通報を作って
  解決しない spec が、同じ shard の前に流れるかで変わるため
- しきい値は、1 画素の色の許容 `threshold: 0.02` と、許容を超えた画素の数 `maxDiffPixels: 8`。
  8 は、同じスタックでもときどき出る描画の揺れ (プロフィールの大きなアイコンの縁で 5 画素) より大きく、
  文字 1 つの変化 (所在地の `Tokyo` を `Tokyp` にして 12〜19 画素) より小さい値。割合で認めると
  (`maxDiffPixelRatio: 0.01` で約 1 万画素)、件数の数字や時刻の文字が変わっても通る。
  色の許容を既定 (0.2) のままにすると、テーマの色を少し変えたぐらい (ダークのパネルを `#0e1433` から
  `#162048` へ) は同じ色とみなされ、ノートの写らないダークの画面 (設定・チャットなど) は 1 つも
  落ちなかった。0.02 では落ちる (#4054 で変異させて確かめた)

**揺れたら、しきい値を上げて黙らせない。** 何が変わったかを差分の画像で見て、上の決まりに足す。

## 設定の表 (#4056)

設定の画面で変えた値が、保存され、再読み込みの後も残り、実際に効くことを、1 行 1 設定の表で確かめる。
spec は `specs/elythia/ui/settings/`、helper は `fixtures/settings_table.ts`。端末の設定 (prefer) は
`prefs_*.spec.ts`、アカウントの設定 (`i/update`) は `account_*.spec.ts`。

1 行は次の 4 段を確かめる (`runPrefRow` / `runAccountRow`)。

1. 設定の画面で部品を操作して値を変える。端末の設定の部品は、`MkPreferenceContainer` の `data-pref-key` で取る
   (#4056 で足した)。容器の外にあるものは文言で取る (`labeledSwitch` など)
2. 保存: 端末の設定は localStorage の `preferences` を、アプリと同じ順 (アカウント・サーバー・端末) で読む。
   アカウントの設定は `i` で読み直す
3. 再読み込みの後も、部品がその値を出している (スイッチは `data-checked`。#4056 で `MkSwitch` に足した)。
   範囲の部品 (MkRange) は値を出さないので、保存された値で見る (`numberOfPageCache`・`deck.columnGap`・
   `sound.masterVolume`・`pollingInterval`)
4. 効き目。多くは `prefer.s` (リアクティブでない) で読まれ、次に描画したときに値を拾うので、再読み込みの後に見る

**行ごとに新しい利用者と、空の端末の設定から始める。** サインインはフォームを通さず、globalSetup が保存した
root の状態の `account` を差し替えて読み込む (`signinWithState`)。root の端末の設定と IndexedDB は持ち込まない。
アカウントの設定も行ごとの利用者に残るので、片付けは要らない。

**効き目を確かめられない設定は、理由を書いて省く** (`effect: { skip: '<理由>' }`)。音・触覚・タッチ操作・時刻・
ブラウザの API・連合の相手に依存するもの。保存と再読み込みまでは確かめる。

**既知の不具合の行は、効き目が落ちることを期待する** (`knownBug: '#<issue>'`)。保存・再読み込み・表示までは
普通に確かめ、効き目だけが、不具合を示す主張で落ちることを求める。その主張のメッセージには issue の番号を
入れ (`'#4057: ...'`)、番号を含まない失敗 (前提の API の失敗、要素の待ちの時間切れ) は、そのまま行を落とす
(行全体を `test.fail()` にすると、別の理由の失敗まで飲み込む)。直すと効き目が通り、「knownBug を外す」と言って落ちる。

**TS の backend の run では流さない。** 部品を Elythia の frontend で足した `data-pref-key` と `data-checked` で
取るため (本家の frontend には無い)。

**効き目の確かめが空振りしていないかを、変異で確かめる。** `PW_SETTINGS_MUTATION=1` で流すと、設定を変える操作
だけを省き (ページの移動と再読み込みは同じ順で行う)、効き目の確かめが落ちることを期待する。通ってしまう行は、
既定の値でも通る (何も確かめていない) ことになる。操作の時間切れ (`TimeoutError`) で落ちたものは数えない
(どんな理由でも落ちうるので、確かめている違いで落ちたことにならない)。要素を待つ主張の時間切れ
(`toBeVisible` など) は主張の失敗として数えるので、効き目の手前の前提は、通常の流れで確かめられていることを
頼りにしている。要求やページを待つ確かめは `expectHappens` で主張の失敗にする。前提の操作 (データを作る、別の設定を
変える) は行の `setup` に置き、変異でも省かれないようにする。既知の不具合の行は、変異では流さない。
**変異の確かめは CI では回していない。** 行を足したときと、効き目の確かめが頼る frontend の部品を変えたときに、
手元で流す。

```bash
make playwright-test PW_SETTINGS_MUTATION=1 PLAYWRIGHT_ARGS="specs/elythia/ui/settings/ --retries=0"
```

#4056 で、これで見つけた空振りは 4 行あった (投稿フォームを閉じても公開範囲は下書きとして残る、
ノートの描画の省略が付く要素を取り違えていた、登録した利用者は `autoAcceptFollowed` がオンから始まるのに
オンにする行にしていた、時計を進めたときのストリーミングのつなぎ直しの穴埋めで、取得の頻度の違いを
見分けられなかった)。最後のものは、効き目を省く行にした。

**抜けを落とすゲート** (`make playwright-settings-check`、frontend-check と CI の `frontend`): 設定のページ
(`pages/settings/*.vue`) の `<script setup>` の `prefer.model('<キー>')` を TypeScript の構文木で集め、どのキーにも
`coversPrefs('<キー>')` を付けた行があるかを見る。`prefer.commit` で変えるキー (テーマ、メニュー、ステータスバー、
パレットの一覧など) は対象にしていない。行を作らないキーは `settings-coverage-allowlist.tsv` に理由を書く。
宣言は `playwright test --list` に解かせる (ルートの対応付けと同じ)。拾えたキーの下限と、4 つのページの目印の
キーを置いている。

## 管理画面の設定とロールのポリシーの表 (#4066)

管理画面で変えた設定とロールのポリシーが、保存され、再読み込みの後も残り、実際に効くことを表で確かめる。
流れと変異の確かめ (`PW_SETTINGS_MUTATION=1`)、既知の不具合の印 (`knownBug`) は設定の表と同じ。
spec は `specs/elythia/ui/admin_settings/`、helper は `fixtures/admin_table.ts`。

- **meta の行** (`meta_*.spec.ts`、`runMetaRow`): root で管理画面を操作し、`admin/meta` で保存を確かめる。
  管理画面の保存の単位 (フォームの枠、または画面の下の Save) ごとに 1 行にまとめ、その保存で変わる
  `admin/update-meta` のキーを宣言する (`coversMeta`)。効き目は、利用者の側から読める出力
  (`/api/meta`、`manifest.json`、各 API の振る舞い) で、キーごとに見る。行の前に `restore` のキーを控え、
  終わったら `admin/update-meta` で戻す。値は今の値と違うものにする (変わらないとフォームの保存の帯が
  出ない)。`admin/meta` が別の名前で返すキーは `[書く名前, 読む名前]` の組で書く (`swPublicKey` と
  `swPublickey`)。`admin/meta` が返さないキーは `restorePublic` で `/api/meta` から控える (`serverRules`、
  本家も返さない)。秘密の値は伏せ字で返り、伏せ字を送り返すと「変えない」と読まれるので、設定済みだと戻せない。
  このスタックでは未設定 (`null`) で、設定済みなら行の前に落とす
- **ポリシーの行** (`policy_*.spec.ts`、`runPolicyRow`): 行ごとに手動のロールを API で作って新しい利用者に
  割り当て、root でロールの編集画面から「基本の値を使う」を切って値と優先度を保存し、`admin/roles/show` で
  確かめる。効き目はその利用者で見る。値は既定値と逆にし、優先度は「高」にする。真偽値はどれかのロールが
  真なら真、上限は大きい方が採られるので、優先度が低いと既定値より厳しくできない。部品は
  `roles.policy-editor.vue` の各 `XFolder` に足した `data-policy-key` で取る (#4066)。行が作ったものは
  `cleanup` で消す (ロール、広告、絵文字、アバターの飾り)
- **基本ロール** (`meta_base_role.spec.ts`): `/admin/roles` の「Role template」で既定のポリシーを変える経路を
  2 行で確かめる。既定のポリシーは丸ごと置き換わるので、行の前の値を控えて戻す

**meta の project に入る spec。** `admin/update-meta` を名前で呼ばない spec も、`runMetaRow` を呼べば
`writesMeta` が meta を書く spec として扱う (`shard-plan.mjs` の `META_WRITING_HELPERS`)。ポリシーの行は
meta を書かないので main で流れる。

**効き目を確かめられない行は、理由を書いて省く。** 外部のサービス (S3、SMTP、センシティブの判定、メールの検証、
DeepL)、リモートのサーバー (連合の相手、リレー)、時間の経過、手元の設定 (`disableEndpointRateLimits` があるので
`rateLimitFactor`)、外から見えない内部の経路 (FTT、RBT) が要るもの。保存と表示までは確かめる。

```bash
make playwright-test PLAYWRIGHT_ARGS="specs/elythia/ui/admin_settings/ --retries=0"
make playwright-test PW_SETTINGS_MUTATION=1 PLAYWRIGHT_ARGS="specs/elythia/ui/admin_settings/ --retries=0"
```

**抜けを落とすゲート** (`make playwright-admin-settings-check`、frontend-check と CI の `frontend`):

- meta のキーは、管理画面 (`pages/admin/*.vue` と `pages/instance-info.vue`) が `admin/update-meta` に渡す
  オブジェクトのキーを TypeScript の構文木で集める。spread と条件式の中のオブジェクトもたどる。字面でない
  引数 (`registrationModePatch[mode]`) は、定義のファイルを `RESOLVED_ARGS` で名指しして、そこから集める。
  読めない引数があれば「信用できない」で落とす
- ポリシーのキーは、`roles.policy-editor.vue` の template を Vue のコンパイラ (`vue/compiler-sfc`) で読み、
  `XFolder` の `data-policy-key` を集める。編集するポリシー (`v-model:policyMeta`) と合わないものは落とす
  (`matchQuery` の検索の語がキーと違う枠がある。`canImportUserLists` の枠の検索の語は `canImportUserList`)
- 宣言は `coversMeta` / `coversPolicies`。行を作らないキーは `admin-settings-coverage-allowlist.tsv` に
  `meta:<キー>` / `policy:<キー>` と理由を書く (画面から変えられないキー。操作できないスイッチと、入力欄の無い
  `objectStoragePort`)。拾えたキーの下限と目印のキーを、meta とポリシーのそれぞれに置いている
- **見る範囲は `admin/update-meta` だけ。** 別の endpoint で書く管理画面の設定 (bot 対策の `admin/captcha/save` など) と、
  初期設定のウィザード (`MkServerSetupWizard`。書くキーは管理画面の各ページにもある) は対象外。endpoint の名前を
  文字列の字面で書かない呼び出し (変数に入れる、`as const` を付ける) も拾わない (今は無い)

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
文字の描画が違い、CI で必ず差分になる。撮る画面を同じ絵にする決まりは、上の
「[見た目の差分](#見た目の差分-4054)」にある。

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
