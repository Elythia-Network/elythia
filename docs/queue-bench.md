# Queue bench (#563)

ジョブキュー配送スループットを **2 stack** (Misskey TS BullMQ / Elythia mkq) で公正比較するベンチマーク基盤。HTTP latency 用 `tests/bench/http/` とは別運用。

> **以前は asynq を含む 3-way だった。** mk-go の asynq driver は #2985 で削除したので、
> harness からも外してある。下の実測表のうち日付が #2985 より前のものには asynq 行が
> 残っているが、**当時測った値の記録**であって現在再現できる構成ではない。

## 本家の動かし方 (#4140)

**比べているのは、既定の設定どうしではなく、流量の上限を外したエンジンどうし。** 本家は設定が無くても inbox を 32 件/秒、deliver を 128 件/秒に絞る (`QueueProcessorService.ts` の `inboxJobPerSec ?? 32` / `deliverJobPerSec ?? 128`) が、Elythia は設定が無ければ絞らない (この違いは #4141)。絞ったまま測ると上限の値が出るだけなので、`tests/bench/queue/common/ts.yml` で両方を 100000 にして外している。

- **本家は `NODE_ENV: production` で動かす。** production 以外では TypeORM がすべての SQL をログに書き、本家だけが遅くなる ([bench-compare.md](bench-compare.md) と同じ理由)。上限を外した条件で 2,000 件を受けた実測では、development 265.7 件/秒、production 301.0 件/秒だった
- **seed が、相手のサーバー (`blackhole` と `faker`。照合用の受信機へ配るときはその host も) を両方の stack の `instance` に先に登録する。** 本家は初めての相手へのジョブが同時に走ると `instance` の行の作成が競合し、一意制約の違反で失敗して再試行 (最初は約 60 秒後) に回る (`FederatedInstanceService.fetchOrRegister`)。登録しなかった初回の outbound では、1 万件のうち 121 件がそうなって drain が 72.43s に延び、行ができた 2 回目は 8.72s だった
- **probe は driver ごとの key で queue を読む。** mkq は `bull:<queue>`、本家は `<redis の接頭辞>:queue:<queue>:<queue>` (本家は `<redis の接頭辞>:queue:<queue>` を BullMQ の prefix にし、BullMQ がその後ろに queue の名前を足す。redis の接頭辞は `redisForJobQueue.prefix`、書かなければ `url` の host (ポートを含む)。`redisForJobQueue` の節が無ければ `redis` の節の設定を使う。ベンチでは `ts`)。queue の `meta` の key が無ければ、driver は送信や投稿を始める前に止まる。#4140 より前は本家も `bull:` で読んでいて、本家の深さは常に 0 だった
- **seed が、本家の API の利用者ごとの制限を外す。** production では本家の制限が効き、`notes/create` は 1 時間に 300 件までなので、seed の 1 件と合わせて、1 回に 100 件投稿する outbound は 1 時間に 3 回目 (301 件目) で 429 になる。Elythia はベンチでは `MK_DISABLEENDPOINTRATELIMITS` で外しているので、本家も [bench-compare.md](bench-compare.md) と同じく基本ロールの `rateLimitFactor` を 0 にする (両方の stack の `meta.policies` に入れる。Elythia は 0 を制限なしとは扱わないが、環境変数で外しているので影響しない)。2026-11-12 の実測では、seed の 1 件と outbound 3 回の計 301 件を 1 時間のうちに投稿して、429 は出なかった

**#4140 より前に立てた stack は、`make queue-bench-down` から始め直す。** `NODE_ENV` と `ts.yml` は app を作り直さないと効かず、`instance` の行と `rateLimitFactor` は seed を流し直さないと入らない。seed は同じ DB に 2 回入れると落ちる (下の送り手の節)。古い stack のままだと、本家が上限で絞られたままだったり、相手のサーバーが未登録で初回の再試行の待ちを含んだりして、本家の値が処理の速さを表さない

## 計測対象

### Outbound (deliver job throughput)

各 stack の local user → `blackhole` 受信機への AP deliver job スループット。

- 各 stack に **dummy follower 100 名** (host=`blackhole`, ユニーク inbox URL) を pre-seed
- driver が `notes/create` (visibility=public) を 100 回叩く → fan-out で **10,000 deliver job** が enqueue
- `blackhole` は POST を全て 204 で即返すので receiver overhead 無し
- 計測: post 開始から blackhole hit が expected 数に達するまでの drain 時間 / RPS

#### Signed delivery fixture (optional, #3569)

Signed delivery mode の計測区間は投稿 poller の起動から、投稿完了・Redis queue depth=0・receiver verified 数が expected 以上になった時点まで。Activity manifest の取得と receiver との照合は計測区間の後に行い、throughput の分子には妥当性確認済みの `unique` 件数だけを使う。不正・内容不一致・予期しない配送・欠測・未確定・タイムアウトでは throughput を出さず、状態・理由と receiver の生カウントを記録する。drain 判定後に `verified` が増えた場合も、増加分が unique / duplicate のどちらであっても区間外到着として `incomplete` にし、throughput を出さない。重複は分子に含めず、生カウントとして記録する。`verified` は配送試行を含む診断値で、throughput の分子ではない。

`make queue-bench-delivery-outbound` は stack ごとの隔離 receiver を使い、既存 synthetic sender の公開鍵で HTTP 署名を検証する。計測時間の外で各 synthetic note の `/notes/{id}/activity` を取得して `(Activity ID, Inbox, canonical body SHA-256)` の期待集合を作り、receiver の到着・重複・不一致・欠測を `outbound.json` に記録する。fixture はベンチ用 Compose network 内だけに置き、公開ポートを開けない。private key の取得や実インスタンスへの接続は行わない。

正常配送の性能比較では、benchcompareは未確定・`pending`・未着を`incomplete`、期待数0・件数の不整合・`invalid` / `contentMismatch` / `unexpected`を`invalid`として、比較全体を比較不能(exit 2)にする。状態・理由と配送の生値はレポートに残し、性能値は出さない。読み取れない、または壊れた`delivery.json`は入力エラー(exit 1)として扱う。`tests/bench/ab/run.sh --delivery`は各runの`delivery.json`を生成し、準備・配送に失敗したrunでは`delivery-error.txt`も残す。性能劣化の閾値は設けない。故意の不正・未着ケースは期待結果との一致を別テストで判定し、正常系の性能値に混ぜない。

Windows checkoutでComposeが`gen-certs.sh`を直接実行するときは、worktree上の改行コードがLFであることを確認する。CRLFになっている場合は、repo設定を変えずに一時fixture copyをLFへ正規化し、Composeの一時overrideからmountする。

この mode は blackhole mode と同じ synthetic follower を作るため、**既存 seed を再利用せず、volume を消した新しい stack で実行する**。delivery target は seed も実行する。

### Inbound (inbox job throughput)

`faker` (Go HTTPS, AP HTTP signature 直接) → 各 receiver inbox に signed activity を blast。

- 各 receiver に faker の投稿者 (既定 1,000 人) を pubkey 込みで、取得済み (`lastFetchedAt` を seed の時刻) として pre-seed する。活動 i は投稿者 `i % 人数` が送る
- 計測の前に、投稿者ごとに 1 件ずつ送る下準備 (warm-up) をする (`INBOUND_WARMUP=0` で外す)。seed の後に app を再起動するので、Elythia は鍵のキャッシュが空のまま始まり、投稿者ごとに最初の 1 件で公開鍵を取りに行く。本家は keyId で DB から鍵を引くので取りに行かない。下準備をしないと、その取得を Elythia の計測だけに含めてしまう。下準備の結果は `inbound.json` の `warmup` に別に残す
- `faker` は **pre-sign 並列化** で sender 側を律速から外し、receiver の verify+enqueue が bottleneck になるよう設計
- 計測: faker → receiver の RPS (= 受信→200 までの rate) と post-send drain time

#### 送り手の投稿者の数 (`INBOUND_ACTORS`、#4138)

**既定は 1,000 人** (10,000 件なら 1 人あたり 10 件)。seed が受信側に登録する人数で、driver は `seed.json` の `faker.actors` を読んで faker に渡すので、seed のときに決める。inbound のときに違う値を渡しても、警告を出して seed の人数で送る。#4138 より前の `seed.json` (人数が無い) では inbound は止まるので、seed を流し直す。

- `make queue-bench-seed` (既定 1,000 人)
- `INBOUND_ACTORS=1 make queue-bench-seed` (1 人から全件)

**人数を変えるときは、先に `make queue-bench-down` で DB を消す。** seed は同じ DB に 2 回入れると一意制約で落ちる (投稿者の URI とユーザー名は同じで、id だけが毎回変わる)。

**1 人から 1 万件を送る条件は、処理の速さではなく行ロックの順番待ちを測ってしまう。** 受信側はリモートの投稿を取り込むたびに投稿者の `notesCount` を増やす (#3928。本家と同じ) ので、同じ投稿者の行への `UPDATE` が 1 本ずつ順番待ちになる。実際の連合では投稿は多くの投稿者に分かれて届くので、既定を 1,000 人にした。1 人に集中する場面 (1 つのアカウントやリレーからまとめて届く) を見たいときだけ `INBOUND_ACTORS=1` で測る。Announce 経路は、全員が seed の作った同じノートを Announce するので、投稿者を分けても対象のノートの行への集中は残る。

実測 (2026-11-12、#4140 の後、develop `b0a63db423` の mkq の image (運営者の環境で作ったので、同梱でないプラグインを含む。#4138 の実測も同じ image)、Create 経路、10,000 件 × 2 target、concurrency 128、送り手 1,000 人、下準備あり。本家は上 ([本家の動かし方](#本家の動かし方-4140)) の条件):

| 回 | Stack | Effective rps | Drain after send | Peak queue depth |
|---|---|---|---|---|
| 1 回目 | mk-go (mkq) | 6071.6 | 16.78s | 9559 |
| 1 回目 | Misskey TS | 1815.7 | 32.14s | 8863 |
| 2 回目 | mk-go (mkq) | 6304.9 | 18.79s | 9617 |
| 2 回目 | Misskey TS | 2169.9 | 31.72s | 9007 |

- 受信の rps は mkq が TS の 2.9〜3.3 倍。drain (送信を始めてから queue が空になるまで。faker の事前の署名と送信の時間を含む) は TS が mkq の 1.7〜1.9 倍。drain には、両方に共通の事前の署名 (4.74s / 4.81s。この間は 1 件も届かない) が入っているので、それを引くと 1.9〜2.3 倍
- 下準備 (投稿者ごとに 1 件) の drain は、mkq 1.91〜2.94s、TS 3.43〜4.94s
- 同じ日の同じ条件の outbound (3 回): mk-go (mkq) 4.07s / 4.09s / 4.07s (2455.1 / 2445.6 / 2458.8 jobs/s)、Misskey TS 8.94s / 8.67s / 8.73s (1118.9 / 1152.8 / 1145.0 jobs/s)

下は #4138 の時点の実測。**本家は #4140 より前の条件** (流量の上限あり、development、probe が TS の queue を読めない) で動いていた。TS の drain は送信が終わった時刻とほぼ同じ値で、処理の時間ではない。TS の rps も development の値。**mkq の列も、上の表とは比べられない。** 2 つの stack は同じホストで同時に測る (計測限界を参照) ので、当時は絞られて手の空いていた TS のぶんの CPU を、mkq が使えていた可能性がある。上の表で mkq の rps が #4138 の表より平均で約 16% 低いのは、このためと考えている。ただし CPU は測っておらず、片方の stack だけで測ってもいない。mkq の image は同じで、#4140 の途中の実測 (本家の上限を外した後) でも平均で約 15% 下がっていた。1 回ごとのばらつきは ±10% ほどある (下の #1158 の節)。今も有効なのは、同じ条件の中での mkq の 1 人と 1,000 人の比較だけ。

| 送り手 | Stack | Effective rps | Drain after send |
|---|---|---|---|
| 1,000 人 (1 回目) | mk-go (mkq) | 7250.2 | 13.07s |
| 1,000 人 (1 回目) | Misskey TS | 2052.7 | 9.94s |
| 1,000 人 (2 回目) | mk-go (mkq) | 7569.7 | 15.16s |
| 1,000 人 (2 回目) | Misskey TS | 2321.5 | 9.32s |
| 1 人 | mk-go (mkq) | 5248.2 | 49.66s |
| 1 人 | Misskey TS | 2679.9 | 8.49s |

- mkq の drain は、1 人から送ると 3 倍以上かかる。1 人の条件で処理が詰まっている間、inbox の worker 16 本のうち 15 本が `userRepository.IncrementNotesCount` で待っていた (goroutine の一覧で確認)
- 1,000 人の条件の mkq の受信の rps は TS の 3.3〜3.5 倍 (TS は #4140 より前の条件。今の倍率は上の表)
- 下準備 (投稿者ごとに 1 件) の drain は、mkq 2.5〜2.7s
- 同じ日の同じ mkq の image の outbound: mk-go (mkq) 4.36s (2294.9 jobs/s)。同じときの Misskey TS 78.14s (128.0 jobs/s) は、TS の deliver の既定の上限 (128 件/秒、1 万件で 78.1s) そのものを測っていた (#4140)

#### Activity type (`INBOUND_ACTIVITY_TYPE`)

faker payload は 2 種類から選択 (env で driver_inbound に渡す):

- `create` (default): `Create(Note)` — handleCreate 経路を計測 (#569)
- `announce`: `Announce(object=<receiver-local note URI>)` — handleAnnounce 経路を計測 (#1158)
  - `seed` が各 stack に benchsender 経由で 1 件 target note を作成し、URI を `seed.json` に書き出す
  - faker は per-(target, index) で activity.id を完全ユニーク化 (target hash 含む) して dedup 干渉を回避

## 実行

```bash
# 1) 各 stack + blackhole + faker を up (5-10 min、初回 build を伴う)
make queue-bench-up

# 2) seed (user / follower / faker actor を DB 直挿入)
#    meta.federation='all' を強制設定 → app コンテナを restart → nginx も restart
make queue-bench-seed

# 3) outbound 計測
make queue-bench-outbound

# optional: 新しい volume の stack で signed delivery fixture を使う
make queue-bench-down
make queue-bench-up
make queue-bench-delivery-outbound

# 4a) inbound 計測 (default: Create 経路、送り手は seed で登録した投稿者)
make queue-bench-inbound

# 4b) inbound 計測 (Announce 経路、#1158 等で利用)
INBOUND_ACTIVITY_TYPE=announce make queue-bench-inbound

# 5) report 生成 (tests/bench/queue/results/queue-report.md)
make queue-bench-report

# まとめて: queue-bench-all (seed → outbound → inbound → report)
make queue-bench-all

# cleanup
make queue-bench-down
```

## 結果ファイル

`tests/bench/queue/results/`:

- `outbound.json` — 生データ (per-stack drain time / hits / depth time series)
- `inbound.json` — faker.send 統計 + per-receiver drain (最後に走った activity type の値で上書き)
- `inbound-announce-before.json` — Announce 経路の参考値 (PR #1158 適用前、2026-05-24 / #1163)
- `inbound-announce-after.json` — Announce 経路の参考値 (PR #1158 適用後、2026-05-21)
- `queue-report.md` — markdown 比較表

**#4138 より前の inbound の値 (下の Announce 経路の参考値を含む) は、1 人から全件を送り、下準備をしない条件のもの。** 今の既定 (1,000 人、下準備あり) とは比べられない。

**#4140 より前の Misskey TS の値は、処理の速さを表していない。** TS は流量の上限 (inbox 32 件/秒、deliver 128 件/秒) が効いたまま、development で動いていた。probe は TS の queue を読めていなかったので、TS の Drain は送信が終わった時刻とほぼ同じ値で、Peak queue depth の 0 は観測できなかったことを表す。TS の rps も development の値。下の表の TS の値は、当時の記録としてだけ残す。

### Announce 経路 reference 値 (PR #1158 マージ後、2026-05-21)

**当時の 3-way 構成での実測** (asynq driver は #2985 で削除済み)。
10000 req × 3 target、concurrency 128 で計測。app コンテナは develop @ `9adc836` (PR #1161 マージ後)。

| Stack | Send Duration | Effective rps | Drain time | Peak queue depth | Notes |
|---|---|---|---|---|---|
| mk-go (asynq) | 3.66s | 2729.9 | 47.15s | 9827 | 受信→202 まで 3 秒台 |
| mk-go (mkq) | 3.46s | 2891.3 | 63.43s | 9912 | 同上 |
| Misskey TS | 10.46s | 956.4 | 18.08s | 0 | (Drain と Peak は観測できていない。#4140) |

観測:

- mk-go (asynq/mkq) は **TS の 2.9x** の send-side throughput (Create 経路の 3.0x と同水準)
- Announce 経路は Create 経路と比べて -4〜-12% (HTTP signature verify + target note resolve + renote create の overhead)
- mk-go は peak depth 9800+ まで queue を積んで worker が drain (47-63s)。当時は「TS は send と同期で peak=0」と読んでいたが、TS の queue を probe が読めていなかっただけだった (#4140)。#4140 の後の実測 (Create 経路) では、TS も inbound で peak depth 8863〜9007 まで積む

### PR #1158 before/after 比較 (#1163 で実施、2026-05-24)

queue-bench orchestration の startup 非決定性 (複数 stack の並列起動で random に 1 stack が ok=0) が #1163 で解消されたので、handleAnnounce hook async 化 (#1158) の effect を before/after で計測。**当時の 3-way 構成での実測** (asynq driver は #2985 で削除済み)。

**測定条件**:
- 同一ホスト・同一 docker compose stack。before は `git revert 9adc836` 適用後 rebuild、after は develop HEAD (= #1158 適用)
- single-run (= 5-10% の run-to-run variance が乗る前提、平均化は未実施)
- 10000 req × 3 target、concurrency 128、Announce 経路

| Stack | 状態 | Send Duration | rps | Drain | Peak | Δ rps | Δ drain |
|---|---|---|---|---|---|---|---|
| mk-go (asynq) | before #1158 | 3.42s | 2926.0 | 40.76s | 9881 | — | — |
| mk-go (asynq) | after #1158 | 3.67s | 2727.0 | 47.52s | 9829 | **-6.8%** | +16.6% |
| mk-go (mkq) | before #1158 | 3.11s | 3216.7 | 74.33s | 9969 | — | — |
| mk-go (mkq) | after #1158 | 3.46s | 2889.3 | 64.14s | 9933 | **-10.2%** | -13.7% |
| Misskey TS | before #1158 | 9.72s | 1029.0 | 17.34s | 0 | — | — |
| Misskey TS | after #1158 | 10.73s | 932.1 | 18.26s | 0 | -9.4% | +5.3% |

**観測**:

- single-run の variance を考えると **before/after に有意な throughput 差は出ていない**。TS は #1158 の影響を一切受けないはず (#1158 は mk-go 側の federation processor 修正) なのに -9.4% 動いている事実が、run-to-run noise が ±10% オーダーであることを示唆する。
- drain time は asynq で悪化 (+16.6%)、mkq で改善 (-13.7%) と非対称。これも noise レンジ内と判断するのが妥当。
- **#1158 の正味効果は、Announce burst 受信のような bench setup では本 single-run 計測で検出できない** ことが分かった。hook 内 work load が send-side latency の支配的要因ではない (= HTTP handler は enqueue で 202 を返すのみで、hook 実行は worker 側で行うため send-side rps には直接影響しない) ので、当然の結果ともいえる。
- より深い計測には: (a) 5-10 回平均、(b) worker per-job latency を直接測る、(c) hook 内 work が重い scenario (例: cascade fanout を伴う Announce) を組む、のいずれかが必要。本 issue ではここまでで打ち止めとし、後日 perf 専用 issue で扱う。

**結論**: PR #1158 は code-level に handleCreate / handleAnnounce 間の async 対称性を回復した hygiene fix として価値があり、その実効性能 effect は本 bench の precision では計測できなかった。

## 設計メモ

### なぜ TS sender でなく faker?

inbound bench で TS instance を sender に使うと、TS 側の deliver throughput が上限になり、receiver inbox 性能を測れない。faker は Misskey 実装非依存の Go HTTPS server で:

- 固定 RSA-2048 keypair を全投稿者で共用し、投稿者 n の URI は `https://faker/users/bench-sender-<n>` (`keyId` は投稿者ごと)。番号と URI は `tests/bench/queue/faker/actors` が決め、seed は faker の `/actors?count=N` で一覧を受け取って登録する
- HTTP signature 計算は **bench 開始前に並列で pre-sign**
- send phase は単純な HTTP POST blast で sender 側がボトルネックにならない

### 計測限界

- **2 つの stack は同じホストで同時に動き、inbound は両方へ同時に送る。** 一方が速く処理すると、もう一方が使える CPU が減る。値は「同じホストを分け合ったときの速さ」で、1 つだけ動かしたときの速さではない
- **probe の key の組み立ては、本家の queue の接頭辞に依存する。** `ts.yml` の `url` の host (ポートを含む) を変えるか、`redisForJobQueue` の節を消すか、`redis` / `redisForJobQueue` に `prefix` を書いたら、`queue_probe.py` の `TS_REDIS_PREFIX` も直す (`test_queue_probe.py` が `url` と突き合わせている)。食い違うと queue の `meta` が見つからず、driver は送信や投稿を始める前に止まる
- inbound で receiver workers が即処理する場合、queue depth は polling 粒度 (100ms) では peak ≈ 0 に見える。そのときの drain は polling 粒度より細かくは測れない
- inbound の drain は、両方の stack への送信が終わるまで終わらない (faker は両方へ同時に送り、全体の終わりを待つ)。速い stack の drain は、遅い stack への送信の時間より短くならない
- drain は queue が空になった時刻で、ジョブの成否は見ない。再試行を使い切って failed に移ったジョブも「空」に数える
- outbound は blackhole hits (delivered count) が primary signal なので polling race の影響を受けない

### Federation flag 注意

Elythia は新規 DB 初期化時 `meta.federation='none'` (= 連合無効) で立ち上がる。seed が DB 直接 UPDATE で `federation='all'` にしたあと、app の meta cache (5min TTL) を再読み込みさせるため `make queue-bench-seed` の最後で `app-mkq` / `app-ts` を restart する。

**app を restart したら nginx front も必ず restart する (#2917)。** nginx は upstream をホスト名で書くと**起動時に一度だけ**名前解決するので、`restart` で app の IP が入れ替わると**古いアドレスを掴んだまま相手側の app へ繋ぐ**。conf も docker DNS も正しいまま、`nginx-ts` が `app-mkq` へ繋がるといった形になり、Host が食い違って inbound が全件 401 になる (Elythia は `ErrInboxHostMismatch`、TS も `ActivityPubServerService` が同じ判定を持つ)。`queue-bench-seed` は app が healthy になってから nginx を restart し、**各 front が自分の app に繋がっていること** (`/api/meta` の `uri` が自分のホストか) を確かめてから抜ける。**TCP が開いているかだけでは足りない** — 誤配線した front もlistener は生きていて 200 を返すため。

#2364 は同じ罠の別経路 (`--force-recreate` が依存を作り直す) で、そちらは `--no-deps` で塞いである。**doc に書かなかったせいで 2 度踏んだ**ので、ここに残す。

### Network allowlist

Elythia の SSRF 防止 (`allowedPrivateNetworks`) は production default で private IP を block する。bench 内の `blackhole` / faker / 他 stack は Docker network の private IP なので、bench config (`tests/bench/queue/common/mk-mkq.yml`) で `127.0.0.0/8`, `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16` を allowlist 化している。

## 関連

- #560 (HTTP bench 結果トラッキング)
- #561 (HTTP bench nginx fix)
- #562 (HTTP bench rate limit fix)
- #413 (パフォーマンス改善ロードマップ)
