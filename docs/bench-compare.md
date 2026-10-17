# base と head の性能比較 (bench-compare)

2 つの commit (base と head) を、同じスタックで交互に流して性能を比べる仕組みです (#3542)。PR やリリースの前後で、同じ条件の数字を並べるために使います。

- 集計と判定は`tools/benchcompare`(#3543)、スタックは`tests/bench/ab`(#3544)、workflow は`.github/workflows/bench-compare.yml`(#3545)
- 同じ仕組みで、base を本家 Misskey の公式 image に替えて比べることもできます (`bench-upstream`、#3654)。下の「[本家 Misskey との比較](#本家-misskey-との比較-bench-upstream)」を見てください
- `tests/bench/http`(`make bench-up` / `bench-run`) は、本家と Elythia を 1 つのホストに同時に立てる古い比較です。workflow は無く、pprof を取るときに手元で使います ([bench-pprof.md](bench-pprof.md))

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

`tests/bench/ab/compose.yml`を使います。run.shは`mk-bench-ab-<timestamp>-<pid>`形式の専用project名を使い、`mk-bench-ab-` prefixと長さを検証し、既存Compose projectとの衝突があればcleanup前に停止します。postgres / redis / nginx / k6 の image は digest で固定しています。seed の image は固定していませんが、計測の前に止めるので数字には入りません。

| サービス | 中身 | CPU の上限 |
|---|---|---|
| app | base / head の Elythia (`mk-bench-ab-app:<project>-base-<pid>` / `mk-bench-ab-app:<project>-head-<pid>`) | 1.25 |
| postgres | PostgreSQL 18 (fsync などを切った設定、下の「postgres の設定」) | 0.75 |
| redis | Redis | 0.5 |
| web | nginx (worker 1) | 0.5 |
| k6 | 負荷をかける側 | 0.75 |

合計は 3.75 で、GitHub-hosted runner の 4 vCPU に収まるようにしています。上限に当たった回数と時間は、回ごとに`throttle.csv`(計測の始めと終わりの差) と`resources.csv`(1 秒ごとの累積値) に残ります。k6 のコンテナも記録の対象です (#3656)。

### postgres の設定 (fsync などを切る、#3659)

**postgres は`fsync=off`・`synchronous_commit=off`・`full_page_writes=off`で動かします。** `tests/bench/ab/compose.yml`の postgres の`command`で渡し、本家との比較の overlay (`compose.upstream.yml`) は postgres を触りません。base / head、本家 / Elythia の両側が同じ postgres の service を使うので、compose の上では両側に同じ設定がかかります。`internal/entitycompat`の`TestBenchPostgresSkipsDiskWaits`(`make compose-check`) は次のときに落ちます。

- `compose.yml`の postgres の`command`から、3 つのどれかが欠けたとき
- overlay が postgres を定義したとき、または overlay のどれかの service の`command` / `entrypoint` / `environment`に 3 つの設定の名前が現れたとき (app の`PGOPTIONS`で本家の側だけ`synchronous_commit`を戻す、など)
- `tests/bench/`の外の compose (本番の雛形`compose.uds.yaml.example`、e2e、開発用) のどれかの service の`command` / `entrypoint` / `environment` (`PGOPTIONS`や`POSTGRES_INITDB_ARGS`を含む) に、3 つの設定の名前が値に関係なく (大文字小文字も問わず) 現れたとき。語として見るので、valkey の`--appendfsync`やコメントでは落ちません。`tests/bench/`の下のほかのベンチは本番と関係しないので見ません

ゲートが見るのは compose だけです。`run.sh`や app の設定ファイル (`elythia.yml` / `misskey.yml`) から接続ごとに設定を変える経路は見ていません (今はどちらにも無い)。

理由は、GitHub-hosted runner の機種によって、ディスクの書き込みの遅延が大きく違い、それが`notes-create`の p95 に直接出ていたためです (#3659 の調査、2026-10-17)。

- runner が`AMD EPYC 9V74`のとき、ノートの INSERT とその COMMIT が、ディスクの待ちで断続的に数十〜数百 ms 止まりました。app / postgres が CPU の上限で止められた区切りは (Elythia の回で) 0 で、読み取りだけのエンドポイントは同じ時間帯に止まっていませんでした。本家も同じ機種で同じように止まります (TypeORM の`query is slow: INSERT INTO "note"`)
- 同じ develop 同士の A/A で、`notes-create`の p95 (各ラウンド) は、9V74 の runner で base 2.5 / 3.0 / 13.9ms、head 39.5 / 4.5 / 31.9ms に揺れ、Intel Xeon Platinum 8573C の runner では 2.4〜2.8ms に収まりました
- artifact の残っていた 21 回の比較で、ノートの INSERT の slow log (200ms 以上) は、9V74 の 7 回のうち 6 回に計 570 件あり、AMD EPYC 7763 の 12 回と Intel の 2 回には 1 件もありませんでした
- 機種は選べない (GitHub-hosted では runner の機種を指定できない) ので、機種で比較を止めるのではなく、両側の postgres から fsync・full page write・COMMIT の待ちを外しました
- **この設定で、9V74 でも揺れなくなりました。** PR #3660 の上の実行 (2026-10-17〜18) で 9V74 に当たった回の`notes-create`の p95 (3 ラウンドの中央値 [最小, 最大]) は、bench-compare の A/A で base 3.1 [2.8, 3.1] / head 2.9 [2.6, 3.2] ms (差なし)、本家との比較で本家 4.4 [4.2, 4.8] / Elythia 2.9 [2.6, 3.0] ms でした。同じ PR の 7763 の回 (A/A 2.9 / 2.9 ms、本家 5.3 / Elythia 3.3 ms) とも揃っています。止まっていた箇所が COMMIT の fsync だったのか、full page write やファイルの書き込みだったのかは、切り分けていません

この設定にしたことで、次のことは**もう測りません**。

- **ディスクが遅いときの挙動。** WAL の fsync・full page write・COMMIT の待ちを外すので、ディスクへの書き込みを減らす変更 (COMMIT の回数を減らすなど) の効果は、この比較にはほとんど出ません
- **本番と同じ書き込みの経路。** 本番の PostgreSQL はこの設定にしません (落ちたときにデータが壊れます)。ベンチのスタックは毎回まっさらな DB から始めて捨てる (`run.sh`は各回の始めと終わりに`down -v`する) ので、クラッシュ時の安全は要りません。なお、seed の後の`compose stop`で postgres が 10 秒以内に止まらず SIGKILL されると、`synchronous_commit=off`ではまだ WAL に書いていない直前の COMMIT が消えることがあります。fast shutdown の checkpoint は`fsync=off`で速いので、起きる見込みは低いと見ています。この設定はベンチのスタックだけのもので、本番・e2e の compose には入れません

report の冒頭には、runner の機種 (`run.json`の`runner.cpu_model`) を「runnerのCPU」として、ほかの runner の項目より先に出します。`AMD EPYC 9V74`のときは「この機種では、以前は書き込みが揺れていた (#3659)」と注記します。機種は大文字小文字を区別せずに比べ、`model name`が取れなかったときは「(不明)」と出します。注記は読み方の目安で、比較は止めません。今の設定では 9V74 でも揺れないことを確かめてありますが (上)、測った回数は 2 回なので注記は残しています。`notes-create`だけに差が出たときは、念のため流し直して確かめてください。

### 流し方

- base と head の image を、それぞれの commit の`git worktree`からビルドします。同じ commit なら 1 回だけビルドします
- ラウンドごとに順番を入れ替えます (ABBA: 奇数のラウンドは base → head、偶数は head → base)。時間とともに変わる runner の状態が、片方にだけ乗らないようにするためです
- 計測の前に、計測しない起動を 1 回挟みます (ページキャッシュなどを温めるため)。起動するのは base の image だけです
- 1 回は次の順に進み、毎回まっさらな DB から始めます
  1. `down -v` → 利用者ごとの制限を外して起動 → seed
  2. 止めて、シナリオの設定で起動し直す (cgroup が作り直され、`memory.peak`が 0 に戻る)
  3. ウォームアップ (各エンドポイント 5 秒)
  4. k6 で計測。k6 は`compose run -d`で起動し、そのコンテナも含めて sampler が 1 秒ごとにリソースを読む。k6 は最初のシナリオまで 3 秒待つ (`START_DELAY`)。sampler が k6 を読み始める前に負荷をかけ始めないための待ち。起動の直後に k6 は 100 個の VU を作り、CPU を多く使う (手元で、CPU 0.75 の k6 が約 5 秒かかり、その間に 100ms の区切りが 47 回止められた) が、k6 は VU を作り終えてからシナリオの時刻を数え始め、エンドポイントの区間はシナリオの開始時刻から数えるので、区間には入らない
  5. `down`

### seed

50 人 × 50 ノート。返信 35%、公開範囲は public 80% / home 15% / followers 5%。足りなければ失敗にします (`SEED_STRICT`)。偏った fixture では planner が別の plan を選び、実態と違う結果になるため、分布を実態に寄せています。

### シナリオ

7 つのエンドポイント (`ping` / `meta` / `local-timeline` / `users-show` / `i` / `notes-create` / `home-timeline`) を、1 つずつ順に k6 の`constant-arrival-rate`で 30 秒ずつ流します (#3656)。応答の速さによらず、エンドポイントごとに決めた率 (1 秒あたりの件数) で送ります。率は`tests/bench/http/k6/lib/config.js`の`arrivalRates`に 1 か所で置き、base と head (本家と Elythia) で必ず同じです。タイムラインのキャッシュは、どちらのシナリオでも off です。

| エンドポイント | 率 (件/秒) | 30 秒の件数 |
|---|---|---|
| `ping` / `meta` | 500 | 15000 |
| `local-timeline` | 50 | 1500 |
| `users-show` / `i` | 100 | 3000 |
| `notes-create` | 30 | 900 |
| `home-timeline` | 40 | 1200 |

- VU は初期化のときに 100 個作ります (`preAllocatedVUs`と`maxVUs`をどちらも 100)。k6 は送る時刻に空いた VU が無いと、その iteration を`dropped_iterations`に数えて落としてから VU を足します (毎秒 100 件・1 件 0.5 秒で 50 VU が要るところを`preAllocatedVUs` 10 / `maxVUs` 100 で 5 秒流すと 41 件落ち、どちらも 100 にすると 0 件。k6 0.55.0、2026-10-17)。先に作っておけば、途中で VU を作る CPU も計測の区間に入りません。VU はシナリオの間で使い回すので、作るのは 100 個だけです。100 個が全部応答を待っているほど遅いときは、送れなかった分が数えられます (下の「判定」)
- 区間の終わりには 1 秒だけ待ち (`gracefulStop`)、次のシナリオと重ならないようにします。終わりの時点で 1 秒を超えて応答を待っている iteration は打ち切られ、遅延にも送れなかった件数にも入りません。率がサーバーの上限より十分低いうちは件数が少なく、p95 への影響は小さい見込みです
- 率は、次の 2 つより十分低い初期値です。PR の上の A/A と bench-upstream で見直します
  - k6 の上限: bench-upstream の run 39012182949 (#3655 の PR、2026-10-17) で、0.75 コアの k6 は`ping`を毎秒約 5000〜5800 件で頭打ちにしていました (app のアクセスログを秒ごとに数えた値)。`ping` / `meta`はその 2 割です
  - 遅い側 (本家) が捌ける量: 同じ run で、本家が 30 秒の区間に捌いた件数は`local-timeline`が約 3500〜3600 件 (毎秒約 120)、`home-timeline`が約 2600〜2700 件 (毎秒約 88) でした。どちらもその半分以下です。`i` / `users-show`は k6 の頭打ち (下の「以前のシナリオ」) で本家の上限が分かっていないので、100 にとどめています
- `notes-create`も同じ率で送ります。以前の closed-loop では、速い側ほど多くのノートを作り、後に流す`home-timeline`の DB の中身が両側で違っていました
- ウォームアップと`--smoke`(`SHORT=1`) は、同じ率で各 5 秒です

#### 軽い固定の負荷で測ることの意味

- **遅延は、上限より十分軽い負荷のときの応答の時間です。** 速いエンドポイント (`ping` / `meta` / `users-show` / `i`) では、k6 から見た p50 / p95 が数 ms 以下になる見込みです (調査のときの Elythia のサーバー内の時間は、p50 で`users-show` 0.52ms、`i` 1.41ms、`meta` 0.021ms)。判定の絶対値の下限 (遅延 1ms) がその大半を占めるので、たとえば`i`が 1.5ms から 2.4ms (+60%) に悪化しても、差が 1ms 未満なので「差なし」になります。速いエンドポイントの小さな悪化は、遅延では見えません。下限を変えるかは、今のシナリオでの A/A の揺れを見てから別に決めます
- **サーバーがどれだけ捌けるか (最大の処理量) は、もう測っていません。** どの率も遅い側の上限の半分以下に置いているので、`requests`は率 × 時間でほぼ決まります (判定しない「参考」)。以前のシナリオの`requests`(捌けた件数) に当たるものはありません
- **両側の仕事の量が同じになったので、`cpu_seconds`(app / postgres) を素直に比べられます。** 以前は速い側ほど多くの件数を捌いていたので、CPU 時間の差には仕事の量の差が混ざっていました。今は同じ件数を同じ間隔で送るので、CPU 時間の差は同じ仕事にかかった CPU の差です。処理の重さの差は、遅延の下限に消える速いエンドポイントでも、app の`cpu_seconds`に出ます (ただし 7 つのエンドポイントの合計です)

#### 以前のシナリオ (#3656 より前)

#3656 より前は、ramping-vus (待ち時間なしの closed-loop。読み取りは 0 → 20 VU (5 秒) → 50 VU (20 秒) → 0 (5 秒)、`notes-create`は 0 → 10 → 20 → 0) でした。この形では、サーバーが速いほど k6 が先に CPU の上限 (0.75) で飽和し、k6 自身の待ちを測っていました。さらに、`i` / `users-show` / `notes-create` / `home-timeline`は、iteration ごとに seed の全体 (約 63KB) を JSON.parse し直していた (`SharedArray`は添字で読むたびに要素を parse する) ため、k6 がそれだけで飽和し、両側とも毎秒約 160 件で頭打ちになっていました。

**#3656 より前の結果と、後の結果の数字は比べられません。** 負荷の形が違うので、p50 / p95 の水準も、`requests`(以前は捌けた件数、今は率 × 時間でほぼ決まる) の意味も変わっています。以前の結果の`i` / `users-show`の「差なし」と、100ms 刻みの p50 / p95 (約 100 / 300ms) は、k6 の待ちを測ったものです。また、`resources.csv`の列が増えた (下の「数え方」) ので、以前の結果のディレクトリは今の`tools/benchcompare`では読めません (ヘッダーが違うとして exit 1)。

| `scenario` | 内容 |
|---|---|
| `prod-like` (既定) | 利用者ごとのレート制限を残し、IP の制限だけ外す (負荷が 1 つの IP から来るため) |
| `unlimited` | 制限をすべて外す |

`prod-like`の`notes-create`は、トークンを 50 人で順に替えて投げるので、利用者ごとの上限 (1 時間に 300、50 人で 15000) には届かない見込みです (1 回のウォームアップと計測で (5 + 30) 秒 × 30 = 1050 件)。届いて 429 になったときは、失敗とは別に数えます。

## 数え方

| 指標 | 定義 |
|---|---|
| `med` / `p(95)` / `p(99)` | エンドポイントごとの、成功した応答 (`expected_response:true`) の所要時間 (ms) |
| `requests` | エンドポイントごとの、リクエストの数。到着率で送るので、送れなかった分が無ければ両側ともほぼ率 × 30 秒になる。差は出すが判定しない (「参考」)。送れなかった分が多いときは、別にそのエンドポイントを比較不能にする |
| k6 が止められた区切り | エンドポイントの区間 (シナリオの予定の開始〜終了) で、k6 のコンテナの`cpu.stat`の`nr_throttled`の増分を`nr_periods`の増分で割ったもの (%)。区間はシナリオが gauge (`bench_window_start_ms` / `bench_window_end_ms`) に書き、区間の中にある`resources.csv`の最初と最後の k6 の行で差を取る。report には各側のラウンドの最大を出す |
| 送れなかった件数 | エンドポイントのシナリオの`dropped_iterations`(`scenario`タグで絞る。k6 はシナリオの`tags`をこの metric に付けない)。report には各側の全ラウンドの合計を出す |
| `cpu_seconds` | コンテナの cgroup の`cpu.stat`の`usage_usec`の、計測の始めから終わりまでの増分 (秒) |
| `cpu_percent` | `cpu_seconds`を、その間の経過時間で割ったもの。1 コアを使い切ると 100% |
| `memory_peak_mib` | コンテナの cgroup の`memory.peak`(起動し直してからの最大、MiB)。ページキャッシュを含む |
| `heap_inuse_max_mib` | app だけ。pprof の`HeapInuse`を 5 秒ごとに読んだ最大 (MiB) |

`resources.csv`の列は`t_ms,container,cpu_usage_usec,memory_peak,heap_inuse,nr_periods,nr_throttled,throttled_usec`です。後ろの 3 つは`cpu.stat`の累積値で、cpu controller が無いと空になります。k6 のコンテナ (`container`が`k6`) の行が 2 つ以上無い回は、記録の漏れとして読めない入力 (exit 1) にします。k6 は計測の終わりに自分で終わり、cgroup が消えるので、k6 の行は他のコンテナより先に途切れます。リソースの表には k6 の行も出ますが、負荷をかける側のコストでサーバーの良し悪しではないので、差だけを出して判定しません (「参考」)。到着率が同じでも、両側でほぼ同じになるとは限りません。bench-upstream では、Elythia は応答を gzip で返し、本家は生で返すので、k6 が応答を読む手間が両側で違います。
配送receiver fixtureを使うrunは、run directoryに任意の`delivery.json`を置けます。`finalized` / `expected` / `attempts` / `verified` / `pending` / `valid` / `unique` / `duplicates` / `invalid` / `contentMismatch` / `unexpected` / `unexpectedAttempts` / `missing`を記録します。`unexpected`は未登録の(Activity ID, Inbox)組の数で、未着のexpected組を埋めません。`valid`は署名に加えてexpected Activity ID・Inbox・本文digestが一致した試行だけです。`tests/bench/ab/run.sh --delivery`を指定すると、各`rN-side/`に receiver の生カウント、`throughputJobs`、`throughputJobsPerSecond`、`drainSeconds`を含む`delivery.json`を出力します。`benchcompare`は全side・roundが妥当なときだけ`unique / drainSeconds`の中央値と範囲を表示し、この指標は参考値として表示して性能判定には使いません。`--delivery-notes`と`--delivery-followers`で合成 fixture の件数を指定でき、既定は2×2です。**この既定値は経路・集計のsmokeであり、性能差を推定する標本ではありません。**

配送 throughput は、HTTP A/B の CPU・メモリ上限を設定した Compose 構成とは別の queue fixture で測定します。同一の資源条件での比較値ではないため、参考値として扱います。

JSONを読めない場合、または壊れたJSONの場合だけ入力エラー(exit 1)にします。読み取れた統計の意味上の異常はloaderで拒否せず、`status` / `reasons`と生の件数をレポートに残して比較不能(exit 2)にします。未確定manifest・`pending`・未着は`incomplete`、`expected`が0・件数の整合性違反・`invalid` / `contentMismatch` / `unexpected`があるroundは`invalid`です。

配送統計が1つでもある比較では、すべてのside・roundに統計が必要です。比較不能のときは性能値を出さず、正常配送の性能比較として扱いません。配送数に新しい性能閾値は設けません。重複数は生値として表示します。故意に不正署名・本文・宛先や未到着を起こすcorrectnessケースは、期待結果との一致を別テストで判定し、正常系の性能runやその`delivery.json`に混ぜません。配送統計がどのroundにもなく`run.json`の`deliveryEnabled`もfalseなら配送未計測です。`deliveryEnabled`がtrueの場合は、全ファイルが欠けていても比較不能として報告します。

## 判定

各指標を、base と head それぞれのラウンドの値 (最小〜最大の幅と中央値) で比べます。

| 判定 | 意味 |
|---|---|
| 差あり | 幅が重ならず、**かつ**中央値の差が 5% 以上で、**かつ**指標ごとの絶対値の下限以上 (遅延 1ms、CPU 時間 0.5 秒、CPU% 2、メモリ 8MiB)。改善か悪化かも出す |
| 差なし | 上のどれかを満たさない |
| 比較不能 (エンドポイント) | 失敗の割合か 429 の割合が、base と head で 1 ポイントを超えて違う (速く失敗しただけかもしれない)、成功した応答が無いラウンドがある、**k6 が律速だった** (どれかのラウンドで、k6 が止められた区切りが 2% を超えた。区間に k6 の行が 2 つ無いときも)、**送れなかった件数が多い** (どちらかの側で、全ラウンドの合計が送るべき件数 (送った件数 + 送れなかった件数) の 0.1% を超えた) |
| 比較不能 (比較全体) | 途中で終わった回がある、どのエンドポイントのシナリオにも数えられない`dropped_iterations`がある、各側 2 ラウンド未満 |
| 参考 | `p(99)`、`requests`、k6 のコンテナのリソース。差の % は出すが、判定には使わない (エンドポイントの判定にも数えない)。エンドポイントが比較不能のときは、`p(99)`と`requests`も比較不能 |

- **k6 が律速かどうかの線 (2%) の根拠 (#3656):** k6 が CPU の上限で止められると、止まっている間に返った応答は、止まった分だけ長く測られます。1 つの区切り (100ms) の中で止まるのは枠を使い切った後の残りだけなので、止められた区切りの割合が r なら、止まっていた時間は区間の r 未満です。到着が一定なら、長く測られる応答もおおむね r 未満になります。判定に使ういちばん上の分位は p95 (上位 5%) なので、r が 5% に近づくと p95 が k6 の待ちで決まりえます。2% なら、長く測られた応答がすべて上位に寄っても、p95 はサーバーの分布の p97 までにとどまります。k6 が飽和すると区切りのほぼすべてで止められる (手元で CPU 0.75 の k6 を飽和させたとき、どのエンドポイントの区間も 94.7〜100%) ので、飽和した回はこの線で確実に分かれます。線は`tools/benchcompare/judge.go`の`maxK6ThrottledPercent`です
- 送れなかった件数が多いと、その側は決めた率より軽い負荷で測ったことになります。サーバーか k6 が率に追いつかなかったことを示し、時間は同じ負荷どうしの比較にならないので、比較不能にします。ただし毎秒 500 件のエンドポイントでは、100ms ほどの詰まり (GC の停止や、CPU の区切りの待ち) で応答を待つ VU が 100 個に届き、数件が落ちえます。15000 件のうちの数件は、判定に使う p95 (上位 5%) を動かさないので、1 件で比較不能にはせず、0.1% を線にしました。落ちた iteration は送られず測られないので、落ちた分がすべて遅い応答 (詰まりの最中) だったとすると、測った分布から上位が抜け、p95 はサーバーの分布の p94.9 (95 × 99.9%) の方へ下がります。落とした側がわずかに速く見える向きですが、0.1% までならその偏りは 0.1 分位にとどまります。線は`tools/benchcompare/judge.go`の`maxDroppedPercent`です。どちらの側で落ちたかは report の「送れなかった件数 (合計)」の行で分かります。以前 (#3656 より前) は、`dropped_iterations`が 1 件でもあると比較全体を止めていました
- 幅が重ならないだけで判定すると、同じ分布から取っても各側 3 ラウンドで約 10%、2 ラウンドで約 33% の確率で「差あり」になります。指標は 1 回の比較で数十あるので、小さな差は 5% と絶対値で切っています
- **`p(99)`は判定に使いません (#3559)。** 1 ラウンド数千件 (`home-timeline`で約 3,200〜4,700 件) のうち上位 1% で決まり、GC や CPU の取り合いで段を飛ぶように動くためです。同じ commit 同士を`prod-like`の 3 ラウンドで 5 回流したところ (UTC で 2026-10-13 に #3555 の PR で 2 回、10-14 に develop で 3 回。runner の CPU は 3 種類が混ざった)、`home-timeline`の p99 は、ラウンドごとの値が 412〜759ms に散りました。5 回のうち 2 回で、幅が重ならずに「差あり (悪化)」になりました (base 516.4ms [511.0, 519.0] / head 614.7ms [577.4, 704.2] で +19.0%、base 591.8ms [582.3, 597.8] / head 685.8ms [599.8, 759.4] で +15.9%)。この 2 回は下限を 20% にすれば防げますが、記録には約 510ms と約 700ms の段が両方あり、base と head に分かれると約 37% になるので、下限では防げません。`p(99)`を外した判定では、5 回とも全エンドポイントが「差なし」でした
- p99 だけが動いて p50 と p95 が動かない変化は、この比較では揺れと区別できません。気になるときは、流し直して回数を重ねてください (workflow の`rounds`は 3 まで)
- **p50 にも、一部のエンドポイントで段があります。** (以下は #3656 より前のシナリオでの記録です。`i` / `users-show`の 100ms 刻みの段は、k6 自身の CPU の区切りの待ちでした。今のシナリオで同じ段が出るかは、A/A で確かめ直します) 上の 5 回では、`notes-create`の p50 がラウンドごとに約 35ms と約 55ms に分かれた回 (base 36 / 52 / 55ms、head 34 / 38 / 59ms) や、head の p50 が`i`で 117 / 140 / 173ms に散った回、`users-show`で 110 / 118 / 178ms に散った回がありました。どれも相手側のラウンドが幅に重なったので「差なし」でしたが、低い段と高い段が base と head に 3 つずつ分かれると「差あり」になります (低い段と高い段が 3 つずつ出たとき、それが base と head にちょうど分かれる確率は約 10%)。`notes-create` / `i` / `users-show`の p50 だけの「差あり」は、まず流し直して確かめてください
- 「差あり」が 1 つ出ても、それだけで回帰とは言えません。A/A (同じ commit 同士) でも runner の揺れで出ることがあります。気になる差は、流し直して回数を重ねてください

### report の形の変更 (#3656)

bench-compare (名前の無い比較) と bench-upstream の report は、#3656 で次のように変わりました。PR へのコメント (bench-compare-comment) は`report.md`をそのまま貼るので、同じ形になります。

- 「エンドポイント」の表に、エンドポイントごとに 2 行増えました。「k6が止められた区切り (%、最大)」(各側のラウンドの最大。測れなかった回があると「-」) と「送れなかった件数 (合計)」(各側の全ラウンドの合計) です。k6 が律速だったとき、送れなかった件数が 0.1% を超えたときは、その行の判定が「比較不能」になります
- 「リソース」の表と、本家との比較の「比」の表に、`k6`のコンテナの行が増えました。判定は「参考」です
- 「リクエスト数」の判定が「参考」になりました (到着率で決まるため)
- 冒頭の判定の説明に、上の 2 つの比較不能の条件が加わりました
- `report.json`の各エンドポイントに`baseK6ThrottledPercent` / `headK6ThrottledPercent`(測れなかった回があると`null`) 、`baseDropped` / `headDropped`、`droppedTooMany`が増え、`resources`に`k6`が加わりました。既存のフィールドは、`requests`と k6 の各指標の`verdict`が`reference`になった (`better`が付かない) ほかは変えていません
- `dropped_iterations`は、比較全体ではなく、そのエンドポイントだけを比較不能にするようになりました。比較全体を止める (終了コード 2) のは、どのシナリオにも数えられない分があるときだけです

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
- 上の表は #3656 より前のシナリオでの実測です。今のシナリオは、シナリオの長さ (各 30 秒 + 間の 1 秒) は同じですが、1 回あたり次の分が増えます。k6 の起動を待つ 3 秒 (`START_DELAY`、計測だけ) と、VU の初期化が 50 個から 100 個に増えた分です。後者はウォームアップと計測のそれぞれで数秒 (手元の CPU で約 2.5 秒ずつ) の見込みです

## 本家 Misskey との比較 (bench-upstream)

本家 Misskey の公式 image (`misskey/misskey:<版>`) と Elythia を、上と同じスタック・同じ流し方で交互に流して比べます (#3654)。base が本家、head が Elythia です。workflow は`.github/workflows/bench-upstream.yml`、スタックは`tests/bench/ab`に本家の側の重ね合わせ (`compose.upstream.yml`と`misskey.yml`) を足したものです。

### 発火

| 経路 | Elythia | 本家の版 | ラウンド |
|---|---|---|---|
| schedule (毎週月曜日 05:40 日本時間 = 日曜日 20:40 UTC) | develop | `UPSTREAM_MISSKEY_VERSION` | 3 |
| 手動 (`workflow_dispatch`) | `ref` (既定 develop) | `upstream` (空なら`UPSTREAM_MISSKEY_VERSION`) | `rounds` (2 か 3) |
| `UPSTREAM_MISSKEY_VERSION`か、ベンチの仕組み (`bench-upstream.yml`、`tests/bench/ab/**`、`tests/bench/http/common/**`、`tests/bench/http/k6/lib/**`、`tools/benchcompare/**`) を変えた develop 向けの PR | PR の head | PR の`UPSTREAM_MISSKEY_VERSION` | 3 |

```bash
gh workflow run bench-upstream.yml --repo Elythia-Network/elythia \
  -f ref=develop -f upstream=2026.10.0 -f rounds=3 -f scenario=prod-like
```

- schedule の時刻は、ほかの schedule の workflow (queue-bench-smoke の毎日 17:30 UTC、dropin-frontend-e2e の毎日 19:00 UTC、codeql の月曜日 20:30 UTC) と重ならない枠にしています
- `ref`の受け方 (英数字と`._/-`だけ、fork の PR は`pull/<番号>/head`) は bench-compare と同じです。`upstream`は`2026.10.0`や`2026.10.0-beta.1`の形だけを受けます
- secrets は使わず、token は読み取りだけです。結果を PR にコメントする workflow はありません (bench-compare-comment は bench-compare だけを見ます)
- ベンチの仕組みを変えた PR では、bench-compare の A/A とこの比較の両方が流れます (別の runner で並行に、それぞれ 40 分ほど)。二重に流れるのは意図どおりで、`run.sh --upstream`の経路が壊れたことに、次の月曜日の schedule まで気付かないのを防ぐためです
- check が見るのは、最後まで流れたかだけです。どちらが速いかで落としません
- 結果の表は実行の Summary に出ます。k6 の生の JSON・リソースの時系列・ログは artifact (`bench-upstream-<run id>`、30 日) に残ります
- required check ではありません

### 手元で流す

```bash
make bench-upstream HEAD=HEAD ROUNDS=2 SCENARIO=prod-like SMOKE=1
make bench-compare-down   # 途中で止めたときの片付け (project は bench-compare と同じ mk-bench-ab)
```

- `UPSTREAM_VERSION=`で本家の版を選べます。既定の`default`は`UPSTREAM_MISSKEY_VERSION`の版です
- **本番が動いているホストでは`SMOKE=1`の 2 ラウンドだけにします** (bench-compare と同じ理由)。比較の数字は CI で取ります

### 条件

上の「測定の条件」(スタック・流し方・seed・シナリオ) は、そのまま本家にも当てはまります。違うのは次の点です。

| | 本家 | Elythia |
|---|---|---|
| image | 公式 image を pull し、既定の CMD (`pnpm run migrateandstart`) のまま動かす (運営者が動かすのと同じ形)。tag は付け替えられるので、実際に流した image の digest を省略せずに report の先頭 (本家の行) に出す | 測る ref から`Dockerfile.app`でビルドする |
| 設定 | `tests/bench/ab/misskey.yml`(`NODE_ENV=production`、`enableIpRateLimit: false`) | `tests/bench/ab/elythia.yml` + 環境変数 (bench-compare と同じ) |
| 利用者ごとの制限 | 基本ロールの`rateLimitFactor`で決める。seed の間は 0 (制限なし)、終わったら`prod-like`は 1 (既定の制限)、`unlimited`は 0 | 環境変数`MK_DISABLEENDPOINTRATELIMITS` (bench-compare と同じ) |
| app の cgroup のプロセス | Node.js が 4 つ。pnpm の外側の 2 つ (`pnpm run migrateandstart`と、そこから起動される`pnpm start`)、server を動かす親プロセス、queue を動かす worker 1 つ (`clusterLimit`無しの既定) | `elythia serve`の 1 つ |
| pprof | 無し | sampler がヒープを 5 秒に 1 回読む (Elythia にだけかかる小さな負荷) |
| CPU / メモリの上限 | 1.25 / 2048M | 1.25 / 2048M |

- **本家は`NODE_ENV=production`で動かします。** `tests/bench/http`は`NODE_ENV=development`で制限を外していましたが、production 以外では TypeORM がすべての SQL をログに書く (`packages/backend/src/postgres.ts`) ので、本家だけが遅くなります。制限は、本家に元からある基本ロールの`rateLimitFactor` (0 で利用者ごとの制限を飛ばす) で外します
- seed の間に制限を外すのは、seed の投稿とフォローが本家の利用者ごとの枠 (`notes/create`は 1 時間に 300) を食わないようにするためです。Elythia の側は bench-compare と同じく、seed の間だけ環境変数で外しています
- 本家の CPU 時間とメモリは、pnpm の 2 つを含めた 4 つの合計です。pnpm の分は CPU をほとんど使いませんが、メモリは 1 つあたり匿名メモリで約 100MiB を常に持ちます (公式 image で`pnpm exec`を待たせて`/proc`の`RssAnon`を読んだ値、2026-10-17)。手元の smoke で本家の`memory.peak`は約 595MiB で、そのうち約 200MiB が pnpm の分と見込まれます。本家のサーバーだけと比べた値ではなく、公式 image をそのまま動かしたときの値として読みます
- app のメモリの上限は、両側とも 2048M にしています (bench-compare は 1024M)。本家は 4 つの Node.js のプロセスと、起動のたびの migration があるためです。`memory.peak`は上限ではなく使った量の最大なので、上限を上げても Elythia の値は変わりません
- タイムラインは、両側とも既定のままです (本家は Redis のタイムライン (FTT) が有効、Elythia は bench-compare と同じく JSON のキャッシュが off)
- burn-in は、本家と Elythia の両方を 1 回ずつ起動します (image が 2 つあるため)

### 読み方

- report の「まとめ」の次に「比 (本家 / Elythia)」の表が出ます。エンドポイントごとの p50 / p95 と、app / k6 / postgres / redis / web の CPU 時間・メモリの最大を、本家の中央値 ÷ Elythia の中央値で並べます。**1 より大きければ Elythia のほうが速い (リソースなら少ない)** です
- 判定 (差あり / 差なし / 比較不能) の基準は bench-compare と同じです。「差あり」には、どちらが良いか (「Elythiaが良い」「本家が良い」) を添えます。差なしのときは、比が 1 から離れていてもどちらが速いとは言いません
- p99 は bench-compare と同じく参考です (判定に使わない)
- Go のヒープ (`HeapInuse`) は Elythia にしか無いので、Elythia の値だけを参考として出します。Elythia の側で途中の回だけ取れなかったときと、ヒープ以外の指標が片側で欠けたときは「比較不能」です。**Elythia の側で全回とも取れなかったときは、両側とも値が無いので、ヒープの行が表から黙って消えます** (比較は「完了」のまま。pprof が効かなくなったなど)。ヒープの行が無い report は、artifact の`resources.csv`の`heap_inuse`の列と app のログを確かめてください
- 比較不能のエンドポイントは、比の表でも比を「-」にします
- **回をまたいで絶対値を比べません。** runner の CPU は回ごとに違います (上の「判定」の節の記録で 3 種類)。週ごとの推移を見るときも、同じ回の中の比を並べます
- `memory.peak`はページキャッシュを含みます。Node.js と Go ではメモリの使い方 (ヒープの確保のしかた、GC) が違うので、メモリの比は「その負荷で cgroup がどこまで膨らんだか」として読みます
- k6 が律速だったエンドポイントと、送れなかった件数が 0.1% を超えたエンドポイントも「比較不能」です (上の「判定」)。Elythia と本家で速さが大きく違うので、率が遅い側の上限に近いと、本家の側だけで送れなかった件数が増えます。そのときは本家がその率に追いつかなかったことを示しますが、時間は比べられません。率を見直すときは`lib/config.js`の`arrivalRates`を変えます (両側に同じ率が効きます)
- #3656 より前の実行 (run 39012182949 など) の`i` / `users-show`の「差なし」と、Elythia の`meta`の p95 (本家より遅く見えた) は、どちらも k6 の待ちを測ったもので、サーバーの差ではありません
- 失敗率か 429 の率が両者で 1 ポイントより大きく違うエンドポイントは「比較不能」になります。`prod-like`で本家だけが 429 を返すようになったら、本家の制限が変わった可能性があります。`notes-create`の上限は両側とも 1 時間に 300 です

### 所要時間

手元 (Xeon E-2274G、8 コア) で`SMOKE=1`の 2 ラウンドを流したときの 1 回は、本家が約 2 分 06 秒、Elythia が約 1 分 50 秒でした (2026-10-17)。本家の burn-in (postgres / redis / web と一緒に起動して、空の DB への migration を流し、healthy になってから止めるまで) は約 24 秒で、1 回あたりの差 (約 16 秒) は本家の起動 (1 回に 2 度) の分です。smoke でない 1 回は bench-compare と同じく約 5 分半の見込みで、3 ラウンド (6 回) にビルド・pull・burn-in を足して約 40 分です。4 ラウンドは約 50 分になり、job の上限 (60 分) に対して余裕が 10 分を切るので、`rounds`は 3 までにしています。GitHub-hosted runner (AMD EPYC 9V74 の 4 vCPU) での実測は、#3655 の PR の上の実行 (2026-10-17、`prod-like`、3 ラウンド) で、`run.sh`の合計が 2,156 秒 (約 36 分)、job 全体が約 36 分半でした

### tests/bench/http を直さずに tests/bench/ab に足した理由

- `tests/bench/ab`には、ラウンドを交互に回す流し方、cgroup から読む資源の測り方、digest で固定した周りの image、`tools/benchcompare`の判定がすでにあり、本家の側は app を替えるだけで済む
- `tests/bench/http`は本家と Elythia を同じホストに同時に立てる (CPU を取り合う)、1 回だけ流す (ばらつきが分からない)、資源を測らない、TLS の証明書を作る、Elythia を連合の e2e 用の Dockerfile でビルドする、という作りで、runner の上で比べるには作り直しが要る
- 同じスタックを使うので、seed (利用者・フォロー・ノートの分布) と k6 のシナリオが、bench-compare と本家との比較で必ず揃う