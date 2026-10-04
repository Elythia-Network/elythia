"""Render queue-bench results as a markdown report (#563)."""
from __future__ import annotations

import argparse
import json
import os
from typing import Any

STACK_LABELS = {
    "ts": "Misskey TS (BullMQ)",
    "mkq": "mk-go (mkq)",
}
ORDER = ["ts", "mkq"]


def fmt(v: Any, decimals: int = 2) -> str:
    if v is None:
        return "-"
    if isinstance(v, bool):
        return "yes" if v else "no"
    if isinstance(v, float):
        return f"{v:.{decimals}f}"
    return str(v)


def render_outbound(out: dict[str, Any]) -> list[str]:
    lines = [
        "## Outbound (deliver job throughput)",
        "",
        "各 stack の local user → blackhole に向けた deliver job を drain する時間。",
        "",
        "| Stack | Notes posted | Expected jobs | Drain (s) | Throughput (jobs/s) | Peak depth | Timed out |",
        "|---|---|---|---|---|---|---|",
    ]
    for stack in ORDER:
        if stack not in out:
            continue
        r = out[stack]
        lines.append(
            "| {label} | {posted} | {expected} | {drain} | {tp} | {peak} | {to} |".format(
                label=STACK_LABELS.get(stack, stack),
                posted=fmt(r["notes_posted"], 0),
                expected=fmt(r["expected_jobs"], 0),
                drain=fmt(r["drain_seconds"], 2),
                tp=fmt(r["throughput_jobs_per_sec"], 1),
                peak=fmt(r["peak_queue_depth"], 0),
                to=fmt(r["timed_out"]),
            )
        )
    lines.append("")
    return lines


def render_inbound(inb: dict[str, Any]) -> list[str]:
    lines = [
        "## Inbound (inbox job throughput)",
        "",
        "faker → 各 receiver inbox に signed AP Create を blast。faker 側は",
        "pre-sign 並列化により sender bottleneck を消し、HTTP signature verify +",
        "queue enqueue が直列化される receiver 側を律速にしている。",
        "",
        "**実効 throughput は faker → receiver の RPS** (受信→200 を返すまでの",
        "rate)。queue が即時 drain される (peak depth ≈ 0) 状況では send 側 rps",
        "がそのまま receiver inbox 処理 throughput になる。",
        "",
        "| Stack | Expected jobs | Send Duration (s) | Effective Throughput (req/s) | Drain after send (s) | Peak depth |",
        "|---|---|---|---|---|---|",
    ]
    drain = inb.get("drain", {})
    send_stats_by_target = {s.get("target", ""): s for s in inb.get("send", {}).get("stats", [])}
    target_for_stack = {
        "mkq": "https://mk-mkq/inbox",
        "ts": "https://ts/inbox",
    }
    for stack in ORDER:
        if stack not in drain:
            continue
        r = drain[stack]
        send = send_stats_by_target.get(target_for_stack.get(stack, ""), {})
        lines.append(
            "| {label} | {expected} | {sdur} | {srps} | {drain} | {peak} |".format(
                label=STACK_LABELS.get(stack, stack),
                expected=fmt(r["expected_jobs"], 0),
                sdur=fmt(send.get("durationMs", 0) / 1000.0 if send else 0.0, 2),
                srps=fmt(send.get("rps", 0) if send else 0.0, 1),
                drain=fmt(r["drain_seconds"], 2),
                peak=fmt(r["peak_queue_depth"], 0),
            )
        )

    send = inb.get("send", {})
    if send:
        lines += [
            "",
            "### faker (sender) stats",
            "",
            f"- pre-sign: {fmt(send.get('preSignMs'), 0)} ms",
            f"- total send: {fmt(send.get('totalMs'), 0)} ms",
            f"- concurrent workers per target: {fmt(send.get('Concurrent') or send.get('concurrent'), 0)}",
            "",
            "| Target | Sent | OK | Failed | Duration (ms) | RPS |",
            "|---|---|---|---|---|---|",
        ]
        for stat in send.get("stats", []):
            lines.append(
                "| {target} | {sent} | {ok} | {failed} | {dur} | {rps} |".format(
                    target=stat.get("target", ""),
                    sent=fmt(stat.get("sent"), 0),
                    ok=fmt(stat.get("ok"), 0),
                    failed=fmt(stat.get("failed"), 0),
                    dur=fmt(stat.get("durationMs"), 0),
                    rps=fmt(stat.get("rps"), 1),
                )
            )
    lines.append("")
    return lines


def main() -> int:
    p = argparse.ArgumentParser()
    p.add_argument("--outbound", default="/results/outbound.json")
    p.add_argument("--inbound", default="/results/inbound.json")
    p.add_argument("--out", required=True)
    args = p.parse_args()

    parts: list[str] = ["# Queue-bench report (#563)", "", ""]

    if os.path.exists(args.outbound):
        with open(args.outbound) as f:
            parts += render_outbound(json.load(f))
    else:
        parts += ["## Outbound", "", "(not run)", ""]

    if os.path.exists(args.inbound):
        with open(args.inbound) as f:
            parts += render_inbound(json.load(f))
    else:
        parts += ["## Inbound", "", "(not run)", ""]

    parts += [
        "## Notes",
        "",
        "- Stack: TS = Misskey TS (BullMQ on Redis); mkq = mk-go (jobQueueDriver: mkq)",
        "- Outbound: 各 stack の local user に blackhole follower 100 名を pre-seed",
        "- Inbound: faker (Go HTTPS, pre-sign 並列化) が sender、receiver verify がボトルネックになる前提",
        "- 詳細 metric は `outbound.json` / `inbound.json` 参照",
        "",
    ]

    with open(args.out, "w") as f:
        f.write("\n".join(parts))
    print(f"report written to {args.out}", flush=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
