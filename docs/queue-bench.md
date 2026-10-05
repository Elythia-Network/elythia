# Queue bench (#563)

ジョブキュー配送スループットを **2 stack** (Misskey TS BullMQ / Elythia mkq) で公正比較するベンチマーク基盤。HTTP latency 用 `tests/bench/http/` とは別運用。

> **以前は asynq を含む 3-way だった。** mk-go の asynq driver は #2985 で削除したので、
> harness からも外してある。下の実測表のうち日付が #2985 より前のものには asynq 行が
> 残っているが、**当時測った値の記録**であって現在再現できる構成ではない。

## 計測対象

### Outbound (deliver job throughput)

各 stack の local user → `blackhole` 受信機への AP deliver job スループット。

- 各 stack に **dummy follower 100 名** (host=`blackhole`, ユニーク inbox URL) を pre-seed
- driver が `notes/create` (visibility=public) を 100 回叩く → fan-out で **10,000 deliver job** が enqueue
- `blackhole` は POST を全て 204 で即返すので receiver overhead 無し
- 計測: post 開始から blackhole hit が expected 数に達するまでの drain 時間 / RPS

### Inbound (inbox job throughput)

`faker` (Go HTTPS, AP HTTP signature 直接) → 各 receiver inbox に signed activity を blast。

- 各 receiver に faker actor を pubkey 込みで pre-seed (外部 fetch 排除)
- `faker` は **pre-sign 並列化** で sender 側を律速から外し、receiver の verify+enqueue が bottleneck になるよう設計
- 計測: faker → receiver の RPS (= 受信→200 までの rate) と post-send drain time

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

# 4a) inbound 計測 (default: Create 経路)
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

### Announce 経路 reference 値 (PR #1158 マージ後、2026-05-21)

**当時の 3-way 構成での実測** (asynq driver は #2985 で削除済み)。
10000 req × 3 target、concurrency 128 で計測。app コンテナは develop @ `9adc836` (PR #1161 マージ後)。

| Stack | Send Duration | Effective rps | Drain time | Peak queue depth | Notes |
|---|---|---|---|---|---|
| mk-go (asynq) | 3.66s | 2729.9 | 47.15s | 9827 | 受信→202 まで 3 秒台 |
| mk-go (mkq) | 3.46s | 2891.3 | 63.43s | 9912 | 同上 |
| Misskey TS | 10.46s | 956.4 | 18.08s | 0 | worker が send と同期処理 |

観測:

- mk-go (asynq/mkq) は **TS の 2.9x** の send-side throughput (Create 経路の 3.0x と同水準)
- Announce 経路は Create 経路と比べて -4〜-12% (HTTP signature verify + target note resolve + renote create の overhead)
- mk-go は peak depth 9800+ まで queue を積んで worker が drain (47-63s)、TS は send と同期で peak=0

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

- 固定 RSA-2048 keypair + 固定 actor URI (`https://faker/users/bench-sender`)
- HTTP signature 計算は **bench 開始前に並列で pre-sign**
- send phase は単純な HTTP POST blast で sender 側がボトルネックにならない

### 計測限界

- inbound で receiver workers が即処理する場合、queue depth は polling 粒度 (50ms) では peak ≈ 0 に見える。実効 throughput は faker.send rps が真の receiver ingest+process rate
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
