"""Compare k6 benchmark results between mk-go and Misskey (TypeScript).

Reads --summary-export JSON files from k6 and produces a markdown
comparison table to stdout and an output file.
"""

from __future__ import annotations

import argparse
import json
from pathlib import Path


ENDPOINTS = [
    "ping",
    "meta",
    "local-timeline",
    "users-show",
    "i",
    "notes-create",
    "home-timeline",
]


def load_summary(path: str) -> dict:
    with open(path) as f:
        return json.load(f)


def extract_metrics(summary: dict) -> dict[str, dict]:
    """Extract per-endpoint metrics from k6 summary-export JSON.

    k6の--summary-exportは以下のフォーマット:
      "http_req_duration{endpoint:ping}": { "avg": ..., "med": ..., "p(90)": ..., "p(95)": ... }
    checksはroot_group.checksに格納される。
    """
    metrics = summary.get("metrics", {})
    checks = summary.get("root_group", {}).get("checks", {})
    results: dict[str, dict] = {}

    for key, val in metrics.items():
        if "endpoint:" not in key:
            continue
        endpoint = key.split("endpoint:")[1].rstrip("}")

        if "http_req_duration" in key:
            # k6 --summary-exportはバージョンによってフラット or values サブキー
            v = val.get("values", val)
            results.setdefault(endpoint, {}).update({
                "med": v.get("med", 0),
                "p90": v.get("p(90)", 0),
                "p95": v.get("p(95)", 0),
                "avg": v.get("avg", 0),
                "min": v.get("min", 0),
                "max": v.get("max", 0),
            })

    # checksからエラー率を計算
    for check_name, check_data in checks.items():
        # "ping 200" → "ping", "local-timeline 200" → "local-timeline"
        ep = check_name.rsplit(" ", 1)[0]
        if ep in ENDPOINTS:
            passes = check_data.get("passes", 0)
            fails = check_data.get("fails", 0)
            total = passes + fails
            if total > 0:
                results.setdefault(ep, {}).update({
                    "error_rate": (fails / total) * 100,
                    "total_reqs": total,
                })

    return results


def fmt(v, decimals=1) -> str:
    if v is None:
        return "-"
    return f"{v:.{decimals}f}"


def collect_profiles(profiles_dir: str | None) -> list[Path]:
    """Return sorted .pb.gz pprof files written by profile-collector.

    profile-collector.sh writes per-scenario CPU profiles plus heap / allocs /
    goroutine snapshots into $OUT (defaults to /output/profiles in compose).
    存在しない or 空のディレクトリは静かにスキップ (#413 #7)。
    """
    if not profiles_dir:
        return []
    p = Path(profiles_dir)
    if not p.is_dir():
        return []
    return sorted(p.glob("*.pb.gz"))


def render_profiles_section(profiles: list[Path], profiles_dir: str | None) -> list[str]:
    if not profiles:
        return []
    rel_dir = Path(profiles_dir).name if profiles_dir else "profiles"
    lines = [
        "",
        "## pprof profiles (mk-go)",
        "",
        f"`profile-collector` が k6-mkgo と並走して採取した profile (場所: `{rel_dir}/`)。",
        "",
        "| File | Size | 用途 |",
        "|------|------|------|",
    ]
    purpose = {
        "heap-pre": "ベンチ開始直前の heap snapshot (baseline)",
        "heap-post": "ベンチ終了直後の heap (leak / steady-state を見る)",
        "allocs-post": "ベンチ全体の累積 allocation (alloc hot path)",
        "goroutine-pre": "ベンチ開始直前の goroutine 状態",
        "goroutine-post": "ベンチ終了直後の goroutine 状態 (leak 検出)",
    }
    for f in profiles:
        size = f.stat().st_size
        stem = f.name.removesuffix(".pb.gz")
        if stem.startswith("cpu-"):
            scenario = stem[len("cpu-"):]
            note = f"`{scenario}` シナリオ steady-state 25s の CPU profile"
        else:
            note = purpose.get(stem, "")
        lines.append(f"| `{f.name}` | {size:,} B | {note} |")
    lines.extend([
        "",
        "解析例:",
        "",
        "```sh",
        f"go tool pprof -http :8080 tests/bench/http/results/{rel_dir}/cpu-users-show.pb.gz",
        f"go tool pprof -http :8080 tests/bench/http/results/{rel_dir}/heap-post.pb.gz",
        "```",
    ])
    return lines


def generate_report(mkgo: dict, misskey: dict, profiles: list[Path] | None = None,
                    profiles_dir: str | None = None) -> str:
    lines = [
        "# Benchmark: mk-go vs Misskey (TypeScript)",
        "",
        "| Endpoint | Side | Med (ms) | p90 (ms) | p95 (ms) | Avg (ms) | Reqs | Err % |",
        "|----------|------|----------|----------|----------|----------|------|-------|",
    ]

    for ep in ENDPOINTS:
        m = mkgo.get(ep, {})
        t = misskey.get(ep, {})

        lines.append(
            f"| {ep} | **mk-go** "
            f"| {fmt(m.get('med'))} "
            f"| {fmt(m.get('p90'))} "
            f"| {fmt(m.get('p95'))} "
            f"| {fmt(m.get('avg'))} "
            f"| {fmt(m.get('total_reqs'), 0)} "
            f"| {fmt(m.get('error_rate'), 2)} |"
        )
        lines.append(
            f"| | **Misskey** "
            f"| {fmt(t.get('med'))} "
            f"| {fmt(t.get('p90'))} "
            f"| {fmt(t.get('p95'))} "
            f"| {fmt(t.get('avg'))} "
            f"| {fmt(t.get('total_reqs'), 0)} "
            f"| {fmt(t.get('error_rate'), 2)} |"
        )

        # Speedup (p95 ratio)
        mp95 = m.get("p95", 0)
        tp95 = t.get("p95", 0)
        if mp95 and tp95 and mp95 > 0:
            ratio = tp95 / mp95
            winner = "mk-go" if ratio > 1 else "tie" if ratio == 1 else "Misskey"
            lines.append(f"| | **Ratio** | | | **{ratio:.2f}x** ({winner}) | | | |")

        lines.append("|  |  |  |  |  |  |  |  |")

    lines.extend([
        "",
        "## Notes",
        "",
        "- Ratio = Misskey p95 / mk-go p95 (>1.0 means mk-go is faster)",
        "- Both servers behind nginx with self-signed TLS on the same Docker host",
        "- Misskey rate limiting is disabled via NODE_ENV=development for fair comparison",
        "- Results may vary depending on host machine load",
    ])

    if profiles:
        lines.extend(render_profiles_section(profiles, profiles_dir))

    return "\n".join(lines)


def main() -> None:
    parser = argparse.ArgumentParser(description="Compare k6 benchmark results")
    parser.add_argument("--mkgo", required=True, help="Path to mk-go summary JSON")
    parser.add_argument("--misskey", required=True, help="Path to Misskey summary JSON")
    parser.add_argument("--output", default="/output/report.md", help="Output markdown path")
    parser.add_argument("--profiles-dir", default=None,
                        help="Directory containing pprof .pb.gz files captured during the bench")
    args = parser.parse_args()

    mkgo_metrics = extract_metrics(load_summary(args.mkgo))
    misskey_metrics = extract_metrics(load_summary(args.misskey))

    profiles = collect_profiles(args.profiles_dir)
    report = generate_report(mkgo_metrics, misskey_metrics, profiles, args.profiles_dir)

    Path(args.output).parent.mkdir(parents=True, exist_ok=True)
    with open(args.output, "w") as f:
        f.write(report)

    print(report)
    print(f"\nReport written to {args.output}")


if __name__ == "__main__":
    main()
