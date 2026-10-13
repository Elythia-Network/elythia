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

- `rounds`は 2 か 3 です。job の上限 (45 分) に 3 ラウンドが収まるかは、下の「所要時間」の実測で見直します
- `base` / `head`には、英数字と`._/-`だけの ref か SHA を渡します。ブランチから届かない commit (fork の PR の commit など) は、40 桁の SHA か`pull/<番号>/head`で渡します。どちらかを解決できなければ、比較を始める前に止まります
- **測る仕組み (`tests/bench/ab`、k6 のシナリオ、`tools/benchcompare`) は、workflow を起動したブランチ (通常は develop) のものを使います。** base / head の側のものではありません。app の image は`./cmd/elythia`をビルドするので、バイナリを`elythia`にまとめた #3394 より前の commit は比べられません
- `head`に fork の PR を渡すときは`pull/<番号>/head`と書きます。workflow は secrets を使わず、token は読み取りだけなので、fork のコードを流しても書き込みはできません
- 結果の表は、実行の Summary に出ます。k6 の生の JSON・リソースの時系列・ログは artifact (`bench-compare-<run id>`、14 日) に残ります
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

## 判定

各指標を、base と head それぞれのラウンドの値 (最小〜最大の幅と中央値) で比べます。

| 判定 | 意味 |
|---|---|
| 差あり | 幅が重ならず、**かつ**中央値の差が 5% 以上で、**かつ**指標ごとの絶対値の下限以上 (遅延 1ms、CPU 時間 0.5 秒、CPU% 2、メモリ 8MiB)。改善か悪化かも出す |
| 差なし | 上のどれかを満たさない |
| 比較不能 | 失敗の割合か 429 の割合が、base と head で 1 ポイントを超えて違う (速く失敗しただけかもしれない)、成功した応答が無いラウンドがある、`dropped_iterations`がある、途中で終わった回がある、各側 2 ラウンド未満 |

- 幅が重ならないだけで判定すると、同じ分布から取っても各側 3 ラウンドで約 10%、2 ラウンドで約 33% の確率で「差あり」になります。指標は 1 回の比較で数十あるので、小さな差は 5% と絶対値で切っています
- 「差あり」が 1 つ出ても、それだけで回帰とは言えません。A/A (同じ commit 同士) でも runner の揺れで出ることがあります。気になる差は、ラウンドを増やして流し直してください

## 所要時間

(実測を取ったら書く)
