#!/usr/bin/env python3
"""Markdown comparison report generator (#1126 / #1120 tracker).

Reads tests/bench/queue-autoscale/results/{fixed16,fixed64,auto}.json
and writes results/report.md as a 3-way comparison table per ADR §7.3.
"""
from __future__ import annotations

import json
import os
from datetime import datetime, timezone

HERE = os.path.dirname(__file__)
RESULTS = os.path.join(HERE, "results")
SCENARIOS = ("fixed16", "fixed64", "auto")


def load(scenario: str) -> dict | None:
    path = os.path.join(RESULTS, f"{scenario}.json")
    if not os.path.exists(path):
        return None
    with open(path) as f:
        return json.load(f)


def fmt_drain(r: dict | None) -> str:
    if r is None:
        return "(missing)"
    if r.get("drain_timed_out"):
        return "**TIMED OUT**"
    v = r.get("drain_time_s")
    return f"{v}s" if v is not None else "n/a"


def fmt_throughput(r: dict | None) -> str:
    if r is None:
        return "—"
    v = r.get("throughput_jobs_per_s")
    return f"{v}" if v is not None else "—"


def fmt_int(r: dict | None, key: str) -> str:
    if r is None:
        return "—"
    return str(r.get(key, "—"))


def main() -> None:
    data = {s: load(s) for s in SCENARIOS}

    # bench parameters (どれか 1 つから取れば良い、設定は scenario 共通)
    sample = next((d for d in data.values() if d is not None), None)
    if sample is None:
        print("No scenario results found; bench did not run.")
        return

    notes = sample.get("outbound_notes", "?")
    followers = sample.get("followers_per_note", "?")
    expected = sample.get("expected_deliver_jobs", "?")

    lines = [
        f"# Auto-scale comparison bench (#1126)",
        "",
        f"Generated at: {datetime.now(timezone.utc).isoformat()}",
        "",
        "## Setup",
        "",
        "- Single mkq stack on localhost (1 postgres + 1 redis + 1 mk-go app)",
        f"- Burst: {notes} notes × {followers} followers = **{expected} deliver jobs / scenario**",
        f"- Driver: tests/bench/queue-autoscale/driver/bench-driver.py",
        "- Per scenario: `compose down -v` between runs for clean DB / Redis state",
        "",
        "## Scenarios",
        "",
        "| Scenario | config | 期待 |",
        "|---|---|---|",
        "| `fixed16` | `deliverJobConcurrency: 16` / `inboxJobConcurrency: 16` | TS 互換 default、ベースライン |",
        "| `fixed64` | `deliverJobConcurrency: 64` / `inboxJobConcurrency: 32` | 経験則上の I/O-bound optimal (8-core 想定) |",
        "| `auto` | `jobQueueAutoScale: true` / `minWorkers: 4` / `maxWorkers: 64` | AIMD controller で動的伸縮 |",
        "",
        "## Drain time (deliver burst)",
        "",
        "| Scenario | Drain time | Throughput (jobs/s) | Hits |",
        "|---|---|---|---|",
    ]
    for s in SCENARIOS:
        r = data[s]
        lines.append(
            f"| `{s}` | {fmt_drain(r)} | {fmt_throughput(r)} | "
            f"{fmt_int(r, 'blackhole_hits')} |"
        )

    lines += [
        "",
        "## Redis connections",
        "",
        "アイドル時 / 負荷時の Redis CLIENT 数。pool-of-Workers 構造で 1 Worker = 1 BLPOP connection を保持するため、worker pool サイズが直接 connection 数に反映される。",
        "",
        "| Scenario | Idle clients | Busy clients |",
        "|---|---|---|",
    ]
    for s in SCENARIOS:
        r = data[s]
        lines.append(
            f"| `{s}` | {fmt_int(r, 'idle_redis_clients')} | "
            f"{fmt_int(r, 'busy_redis_clients')} |"
        )

    lines += [
        "",
        "## Post submit time",
        "",
        "notes/create POST の **submit** にかかった時間 (= HTTP response 受け取り完了)。drain time に含まれる前段。fan-out enqueue が遅れていれば post 自体も遅延する。",
        "",
        "| Scenario | Post submit |",
        "|---|---|",
    ]
    for s in SCENARIOS:
        r = data[s]
        v = r.get("post_submit_s") if r else None
        lines.append(f"| `{s}` | {v}s |" if v is not None else f"| `{s}` | — |")

    lines += [
        "",
        "## Raw results",
        "",
    ]
    for s in SCENARIOS:
        r = data[s]
        if r is None:
            lines.append(f"### `{s}`\n\n(missing)\n")
        else:
            lines.append(f"### `{s}`\n\n```json\n{json.dumps(r, indent=2)}\n```\n")

    out_path = os.path.join(RESULTS, "report.md")
    with open(out_path, "w") as f:
        f.write("\n".join(lines))
    print(f"wrote {out_path}")


if __name__ == "__main__":
    main()
