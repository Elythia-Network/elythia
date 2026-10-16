# base と head の性能比較 (bench-compare)

2 つの commit (base と head) を、同じスタックで交互に流して性能を比べる仕組みです (#3542)。PR やリリースの前後で、同じ条件の数字を並べるために使います。

- 集計と判定は`tools/benchcompare`(#3543)、スタックは`tests/bench/ab`(#3544)、workflow は`.github/workflows/bench-compare.yml`(#3545)
- 本家 Misskey との比較 (`tests/bench/http`) とは別物です。こちらは Elythia 同士だけを比べます

## 使い方

### GitHub Actions で流す (比較の実測はこちら)

Actions の「Bench compare」から手動で起動します。

```bash
gh workflow run bench-compare.yml --repo Elythia-Network/elythia \
  -f base=develop -f head=<ブランチか SHA> -f rounds=3 -f scenario=prod-like
```

- `rounds`は 2 か 3 です。4 ラウンドは job の上限 (45 分) に収まりません (下の「所要時間」)
- `base` / `head`には、英数字と`._/-`だけの ref か SHA を渡します。ブランチから届かない commit (fork の PR の commit など) は、40 桁の SHA か`pull/<番号>/head`で渡します。どちらかを解決できなければ、比較を始める前に止まります
- **測る仕組み (`tests/bench/ab`、k6 のシナリオ、`tools/benchcompare`) は、workflow を起動したブランチ (通常は develop) のものを使います。** base / head の側のものではありません。app の image は`./cmd/elythia`をビルドするので、バイナリを`elythia`にまとめた #3394 より前の commit は比べられません
- `head`に fork の PR を渡すときは`pull/<番号>/head`と書きます。workflow は secrets を使わず、token は読み取りだけなので、fork のコードを流しても書き込みはできません
- 結果の表は、実行の Summary に出ます。k6 の生の JSON・リソースの時系列・ログは artifact (`bench-compare-<run id>`、14 日) に残ります
- **結果の表は、対象の PR にもコメントで出ます** (`bench-compare-comment.yml`、#3560)。PR ごとに 1 つのコメントで、流し直すと同じコメントを書き換えます。対象の PR は次のとおりで、どれにも当たらない (develop 同士など)、PR が閉じている、実行を取り消した、実行の間に PR の head が変わった (PR の A/A のとき) のどれかのときはコメントしません
  - PR で自動で流れた A/A: その PR
  - 手動実行で`head`が`pull/<番号>/head`: その番号の PR
  - 手動実行で`head`が 40 桁の SHA: その commit を含む開いている PR が 1 つだけなら、その PR
  - 手動実行で`head`がブランチ名: そのブランチから開いている PR が 1 つだけなら、その PR (fork のブランチは引けないので`pull/<番号>/head`で渡す)
- **コメントは PR ごとに 1 つなので、後から起動した実行 (実行 ID が大きい) の結果で置き換わります。終わった順ではありません。** 手動の比較の後に PR の A/A を起動すれば、A/A の結果になります (逆も同じ)。どちらの結果を残すかは選べません。前の結果は、実行の Summary と artifact に残ります
- 書き換えるのは、前のコメントと同じか新しい実行 (同じ実行の re-run を含む) の結果だけです。古い実行を re-run しても、実行 ID は変わらないので、新しい結果を上書きしません
- 既知の限界: ほぼ同時に終わった別の経路の実行どうし (同じ PR への手動の比較と A/A など) では、古い結果が残ることがあります
- Dependabot が起点の実行 (actions の更新で bench-compare.yml を触る PR の A/A) ではコメントしません。そこから起動された`workflow_run`の token は読み取りだけになり、書き込めないためです
- **fork の PR の A/A の結果は、畳んで「検証されていません」と注記します。** PR の A/A は`paths`の条件どおり、PR が bench-compare.yml などを触ったときにだけ、**PR の側の workflow ファイルで**流れます。fork の PR なら、そのファイルを書き換えて任意の`report.md`を作れるためです。同じリポジトリのブランチからの PR は、書き込み権限のある人が出したものなので、そのまま貼ります
- コメントを書く workflow は、bench-compare の完了 (`workflow_run`) で develop にある版が動きます。head のコードは checkout も実行もせず、artifact の`report.md`を 60000 バイトまでで切って貼るだけです。`&`と`<`は実体参照にし、`@名前`と`#数字`の形のメンションと相互参照が効かないよう、幅 0 の空白を挟みます (それ以外の形の参照までは防ぎません)。対象の PR は artifact からではなく、bench-compare の実行名 (run-name) と GitHub の API から決めます。PR で流れた実行の run-name と artifact は PR を出した人が変えられる (PR の側の workflow ファイルで動くため) ので、**PR の head の SHA と repository が、GitHub が付けた実行の head と一致するときだけ**書きます。手動実行は書き込み権限のある人しか起動できないので、入力から作る run-name をそのまま使います。**bench-compare.yml の`run-name`の形を変えるときは、bench-compare-comment.yml の正規表現も直します**
- ベンチの仕組み (workflow、`tests/bench/ab`、`tests/bench/http`の seed と k6 の共通部品、`tools/benchcompare`) を変えた develop 向けの PR では、PR の head 同士 (A/A) を自動で流します。仕組みを変えたときに、同じ commit で「差なし」がそろうかを見るためです
- **check が見るのは、最後まで流れたかだけです。** 「差なし」かどうかは、Summary の表を見て確かめてください。A/A でも runner の揺れで「差あり」が出ることがあるので、判定で落とすと不安定な check になります
- Summary に`report.md`が無いときは、準備の失敗・時間切れ・取り消し・集計の入力エラーのどれかです。artifact のログを見てください
- required check ではありません

### 手元で流す

```bash
make bench-compare BASE=develop HEAD=HEAD ROUNDS=2 SCENARIO=prod-like SMOKE=1
make bench-compare-down   # 途中で止めたときの片付け
```

- **本番が動いているホストでは、`SMOKE=1`(10 人 × 10 ノート、各エンドポイント 5 秒) で 2 ラウンドの動作確認だけにします。** 比較の数字は CI で取ります。負荷をかけるうえ、ほかのコンテナと CPU を取り合うので、数字も当てになりません
- cgroup v2 が要ります (`/sys/fs/cgroup/cgroup.controllers`が読めること)
- 結果は`tests/bench/ab/results/<時刻>-<シナリオ>/`(gitignore 済み)。`BENCH_OUT=`で変えられます
- `tests/bench/ab/run.sh`の終了コードは`tools/benchcompare`と同じです。0 = 比較できた (判定に関わらず)、1 = 入力を読めない・準備の段階で失敗、2 = 比較できない (途中で終わった回がある、など)。**`make`を通すと、失敗はどれも 2 になります** (make がレシピの失敗をまとめるため)。区別したいときは`run.sh`を直接呼びます

## 測定の条件

### スタック

`tests/bench/ab/compose.yml`(project 名`mk-bench-ab`)。postgres / redis / nginx / k6 の image は digest で固定しています。seed の image は固定していませんが、計測の前に止めるので数字には入りません。

| サービス | 中身 | CPU の上限 |
|---|---|---|
| app | base / head の Elythia (`mk-bench-ab-app:base` / `:head`) | 1.25 |
| postgres | PostgreSQL 18 | 0.75 |
| redis | Redis | 0.5 |
| web | nginx (worker 1) | 0.5 |
| k6 | 負荷をかける側 | 0.75 |

合計は 3.75 で、GitHub-hosted runner の 4 vCPU に収まるようにしています。上限に当たった回数と時間は、回ごとに`throttle.csv`に残ります。

### 流し方

- base と head の image を、それぞれの commit の`git worktree`からビルドします。同じ commit なら 1 回だけビルドします
- ラウンドごとに順番を入れ替えます (ABBA: 奇数のラウンドは base → head、偶数は head → base)。時間とともに変わる runner の状態が、片方にだけ乗らないようにするためです
- 計測の前に、計測しない起動を 1 回挟みます (ページキャッシュなどを温めるため)。起動するのは base の image だけです
- 1 回は次の順に進み、毎回まっさらな DB から始めます
  1. `down -v` → 利用者ごとの制限を外して起動 → seed
  2. 止めて、シナリオの設定で起動し直す (cgroup が作り直され、`memory.peak`が 0 に戻る)
  3. ウォームアップ (各エンドポイント 5 秒)
  4. k6 で計測。並行して sampler が 1 秒ごとにリソースを読む
  5. `down`

### seed

50 人 × 50 ノート。返信 35%、公開範囲は public 80% / home 15% / followers 5%。足りなければ失敗にします (`SEED_STRICT`)。偏った fixture では planner が別の plan を選び、実態と違う結果になるため、分布を実態に寄せています。

### シナリオ

7 つのエンドポイント (`ping` / `meta` / `local-timeline` / `users-show` / `i` / `notes-create` / `home-timeline`) を、1 つずつ順に k6 の ramping-vus で流します。読み取りは 0 → 20 VU (5 秒) → 50 VU (20 秒) → 0 (5 秒)、`notes-create`は 0 → 10 → 20 → 0 で、各 30 秒です。タイムラインのキャッシュは、どちらのシナリオでも off です。

| `scenario` | 内容 |
|---|---|
| `prod-like` (既定) | 利用者ごとのレート制限を残し、IP の制限だけ外す (負荷が 1 つの IP から来るため) |
| `unlimited` | 制限をすべて外す |

`prod-like`の`notes-create`は、トークンを 50 人で順に替えて投げるので、利用者ごとの上限 (1 時間に 300) には届かない見込みです。届いて 429 になったときは、失敗とは別に数えます。

## 数え方

| 指標 | 定義 |
|---|---|
| `med` / `p(95)` / `p(99)` | エンドポイントごとの、成功した応答 (`expected_response:true`) の所要時間 (ms) |
| `requests` | エンドポイントごとの、リクエストの数 (多いほうがよい) |
| `cpu_seconds` | コンテナの cgroup の`cpu.stat`の`usage_usec`の、計測の始めから終わりまでの増分 (秒) |
| `cpu_percent` | `cpu_seconds`を、その間の経過時間で割ったもの。1 コアを使い切ると 100% |
| `memory_peak_mib` | コンテナの cgroup の`memory.peak`(起動し直してからの最大、MiB)。ページキャッシュを含む |
| `heap_inuse_max_mib` | app だけ。pprof の`HeapInuse`を 5 秒ごとに読んだ最大 (MiB) |

配送receiver fixtureを使うrunは、run directoryに任意の`delivery.json`を置けます。`finalized` / `expected` / `attempts` / `verified` / `pending` / `valid` / `unique` / `duplicates` / `invalid` / `contentMismatch` / `unexpected` / `unexpectedAttempts` / `missing`を記録します。`unexpected`は未登録の(Activity ID, Inbox)組の数で、未着のexpected組を埋めません。`valid`は署名に加えてexpected Activity ID・Inbox・本文digestが一致した試行だけです。`tests/bench/ab/run.sh --delivery`を指定すると、各`rN-side/`に receiver の生カウント、`throughputJobs`、`throughputJobsPerSecond`、`drainSeconds`を含む`delivery.json`を出力します。`benchcompare`は全side・roundが妥当なときだけ`unique / drainSeconds`の中央値と範囲を表示し、この指標は参考値として表示して性能判定には使いません。`--delivery-notes`と`--delivery-followers`で合成 fixture の件数を指定でき、既定は2×2です。

JSONを読めない場合、または壊れたJSONの場合だけ入力エラー(exit 1)にします。読み取れた統計の意味上の異常はloaderで拒否せず、`status` / `reasons`と生の件数をレポートに残して比較不能(exit 2)にします。未確定manifest・`pending`・未着は`incomplete`、`expected`が0・件数の整合性違反・`invalid` / `contentMismatch` / `unexpected`があるroundは`invalid`です。

配送統計が1つでもある比較では、すべてのside・roundに統計が必要です。比較不能のときは性能値を出さず、正常配送の性能比較として扱いません。配送数に新しい性能閾値は設けません。重複数は生値として表示します。故意に不正署名・本文・宛先や未到着を起こすcorrectnessケースは、期待結果との一致を別テストで判定し、正常系の性能runやその`delivery.json`に混ぜません。配送統計がどのroundにもなく`run.json`の`deliveryEnabled`もfalseなら配送未計測です。`deliveryEnabled`がtrueの場合は、全ファイルが欠けていても比較不能として報告します。

## 判定

各指標を、base と head それぞれのラウンドの値 (最小〜最大の幅と中央値) で比べます。

| 判定 | 意味 |
|---|---|
| 差あり | 幅が重ならず、**かつ**中央値の差が 5% 以上で、**かつ**指標ごとの絶対値の下限以上 (遅延 1ms、CPU 時間 0.5 秒、CPU% 2、メモリ 8MiB)。改善か悪化かも出す |
| 差なし | 上のどれかを満たさない |
| 比較不能 | 失敗の割合か 429 の割合が、base と head で 1 ポイントを超えて違う (速く失敗しただけかもしれない)、成功した応答が無いラウンドがある、`dropped_iterations`がある、途中で終わった回がある、各側 2 ラウンド未満 |
| 参考 | `p(99)`。差の % は出すが、判定には使わない (エンドポイントの判定にも数えない)。エンドポイントが比較不能のときは、`p(99)`も比較不能 |

- 幅が重ならないだけで判定すると、同じ分布から取っても各側 3 ラウンドで約 10%、2 ラウンドで約 33% の確率で「差あり」になります。指標は 1 回の比較で数十あるので、小さな差は 5% と絶対値で切っています
- **`p(99)`は判定に使いません (#3559)。** 1 ラウンド数千件 (`home-timeline`で約 3,200〜4,700 件) のうち上位 1% で決まり、GC や CPU の取り合いで段を飛ぶように動くためです。同じ commit 同士を`prod-like`の 3 ラウンドで 5 回流したところ (UTC で 2026-10-13 に #3555 の PR で 2 回、10-14 に develop で 3 回。runner の CPU は 3 種類が混ざった)、`home-timeline`の p99 は、ラウンドごとの値が 412〜759ms に散りました。5 回のうち 2 回で、幅が重ならずに「差あり (悪化)」になりました (base 516.4ms [511.0, 519.0] / head 614.7ms [577.4, 704.2] で +19.0%、base 591.8ms [582.3, 597.8] / head 685.8ms [599.8, 759.4] で +15.9%)。この 2 回は下限を 20% にすれば防げますが、記録には約 510ms と約 700ms の段が両方あり、base と head に分かれると約 37% になるので、下限では防げません。`p(99)`を外した判定では、5 回とも全エンドポイントが「差なし」でした
- p99 だけが動いて p50 と p95 が動かない変化は、この比較では揺れと区別できません。気になるときは、流し直して回数を重ねてください (workflow の`rounds`は 3 まで)
- **p50 にも、一部のエンドポイントで段があります。** 上の 5 回では、`notes-create`の p50 がラウンドごとに約 35ms と約 55ms に分かれた回 (base 36 / 52 / 55ms、head 34 / 38 / 59ms) や、head の p50 が`i`で 117 / 140 / 173ms に散った回、`users-show`で 110 / 118 / 178ms に散った回がありました。どれも相手側のラウンドが幅に重なったので「差なし」でしたが、低い段と高い段が base と head に 3 つずつ分かれると「差あり」になります (低い段と高い段が 3 つずつ出たとき、それが base と head にちょうど分かれる確率は約 10%)。`notes-create` / `i` / `users-show`の p50 だけの「差あり」は、まず流し直して確かめてください
- 「差あり」が 1 つ出ても、それだけで回帰とは言えません。A/A (同じ commit 同士) でも runner の揺れで出ることがあります。気になる差は、流し直して回数を重ねてください

## 所要時間

GitHub-hosted runner (ubuntu-24.04、AMD EPYC 7763 の 4 vCPU) で、同じ commit 同士を`prod-like`の 3 ラウンドで流した実測です (UTC で 2026-10-13、#3555 の PR の A/A)。

| 段階 | 時間 |
|---|---|
| image のビルド (1 回) | 1 分 48 秒 |
| 計測しない起動 (burn-in) | 9 秒 |
| 1 回 (base か head の片方) | 約 5 分 21 秒 |
| 3 ラウンド (6 回) | 約 32 分 |
| `run.sh`の合計 | 34 分 04 秒 |
| job 全体 (checkout と setup-go を含む) | 34 分 29 秒 |

- base と head が違う commit なら、image のビルドがもう 1 回 (約 2 分) 増えて、36〜37 分の見込みです
- 4 ラウンドは計測だけで約 43 分になり、ビルドを足すと job の上限 (45 分) を超えるので、`rounds`は 3 までにしています
- この実行では、7 つのエンドポイントと app / postgres / redis のリソースが、すべて「差なし」でした。app は CPU の上限 (1.25) に、計測の区切り (100ms) のうち約 14% で当たっていました (`throttle.csv`)
