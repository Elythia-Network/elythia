# Auto-scale comparison bench (#1126 / #1120 tracker)

Job queue auto-scale (`jobQueueAutoScale: true`) と **固定 worker 数**運用 (`deliverJobConcurrency: N`) の drain time / resource 使用量を 3-way 比較する measurement-only bench。production decision (= operator が auto-scale を有効化するか判断) の根拠データを供給する。

## 3 シナリオ

| Scenario | 設定 | 想定 |
|---|---|---|
| `fixed16` | `deliverJobConcurrency: 16` / `inboxJobConcurrency: 16` | Misskey TS 互換 default、ベースライン |
| `fixed64` | `deliverJobConcurrency: 64` / `inboxJobConcurrency: 32` | I/O-bound 経験則 optimal (8-core 想定) |
| `auto` | `jobQueueAutoScale: true` / `minWorkers: 4` / `maxWorkers: 64` | AIMD controller で動的伸縮 |

## 構成

```
tests/bench/queue-autoscale/
├── README.md                   (本ファイル)
├── docker-compose.yml          単一 mkq stack (postgres + redis + app + nginx + blackhole)
├── configs/
│   ├── fixed16.yml
│   ├── fixed64.yml
│   └── auto.yml
├── nginx.conf                  TLS terminate + upstream app:3000
├── driver/
│   ├── Dockerfile              python:3.12-slim + httpx + redis + psycopg
│   └── bench-driver.py         seeding + burst + drain measure (1 scenario / 1 invocation)
├── run.sh                      orchestrator (3 scenario を逐次実行)
├── report.py                   markdown 集計
└── results/
    ├── {fixed16,fixed64,auto}.json   driver 出力
    └── report.md                      report.py が生成
```

## 使い方

```sh
# 全 3 scenario 実行 (~5-10 min、scenario 切替で compose down -v するため毎回 fresh DB)
make queue-bench-autoscale-run

# 結果確認
less tests/bench/queue-autoscale/results/report.md

# cleanup
make queue-bench-autoscale-down
```

## 計測内容

各 scenario で:

1. **idle Redis client 数** (= mkq Worker pool 内 BLPOP 接続数の proxy)
   - `auto` は minWorkers=4 で起動するので 7 queue × 4 = 28 接続程度
   - `fixed16` は 16 接続程度
   - `fixed64` は 64 接続程度
2. **deliver burst drain time** = N notes を post → fan-out で N × FOLLOWERS deliver job が enqueue されて全 blackhole にヒットするまでの時間
3. **busy Redis client 数** = burst 中の接続数 (上限指標)
4. **post submit time** = notes/create HTTP POST 完了までの時間 (fan-out enqueue が遅いと膨らむ)

## 環境変数 override

```sh
OUTBOUND_NOTES=50 FOLLOWERS=100 DRAIN_TIMEOUT_S=600 \
    make queue-bench-autoscale-run
```

| 変数 | default | 用途 |
|---|---|---|
| `OUTBOUND_NOTES` | 10 | post 数。実 deliver job 数 = OUTBOUND_NOTES × FOLLOWERS |
| `FOLLOWERS` | 50 | 1 user あたりの blackhole follower 数 |
| `DRAIN_TIMEOUT_S` | 240 | drain 観測の上限。超えると `drain_timed_out: true` で record |
| `IDLE_OBSERVE_S` | 10 | idle 観測の長さ |

## 制限事項

- **Outbound deliver burst のみ** (inbox burst は未実装、follow-up で対応可能)
- **単一 mkq stack** で逐次実行 (= 計測中の host 負荷が scenario 間で揺れる可能性、各 scenario の間に `compose down -v` でクリーンアップして state leak は防ぐ)
- **single-host bench** (multi-pod 分散シナリオは対象外、ADR §3.5 multi-pod 非ゴールと整合)
- **既存 `tests/bench/queue/`** (TS / mkq の 2-way 比較) とは独立、同時に走らせない (volume / network 名は別だが host CPU を奪い合う)

## 関連

- ADR: `docs/design/auto-scale-job-workers.md` §7.3 (本 bench の test spec)
- Tracker: #1120
- 先行 PR: #1127 ADR / #1128 metrics / #1129 controller / #1130 mkqdriver Resize / #1131 wiring
