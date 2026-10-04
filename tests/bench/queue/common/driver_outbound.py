"""Outbound deliver-throughput driver (#563).

Per stack:
  1. Reset blackhole hit counter
  2. Concurrently start a depth/hits poller AND post `OUTBOUND_NOTES`
     notes/create requests against the sender stack
  3. Drain detection = blackhole hits reach the expected total AND the
     redis queue depth has settled to 0
  4. Compute drain time = T_drain - T_post_start, throughput = jobs/s

The blackhole receiver counter is the authoritative source of truth for
"job completed" because the queue probe alone can miss bursts shorter
than the 100ms polling interval.
"""
from __future__ import annotations

import json
import os
import sys
import threading
import time
from typing import Any

import httpx

from queue_probe import DriverKind, make_probe

OUTBOUND_NOTES = int(os.environ.get("OUTBOUND_NOTES", "100"))
POLL_INTERVAL_S = 0.05
DRAIN_TIMEOUT_S = 240.0
BLACKHOLE_URL = os.environ.get("BLACKHOLE_URL", "http://blackhole")

STACK_PROBES: dict[str, tuple[DriverKind, str]] = {
    "mkq": ("mkq", os.environ["REDIS_MKQ_HOST"]),
    "ts": ("bullmq", os.environ["REDIS_TS_HOST"]),
}


def reset_blackhole() -> None:
    httpx.post(f"{BLACKHOLE_URL}/reset", timeout=5)


def blackhole_hits() -> int:
    r = httpx.get(f"{BLACKHOLE_URL}/stats", timeout=5)
    r.raise_for_status()
    return int(r.json()["hits"])


def post_notes_async(url: str, token: str, count: int, done: threading.Event,
                     elapsed: dict[str, float], post_count: dict[str, int]) -> None:
    with httpx.Client(base_url=url, timeout=30, verify=False) as http:
        ok = 0
        start = time.monotonic()
        for i in range(count):
            try:
                r = http.post(
                    "/api/notes/create",
                    json={"i": token, "text": f"queue-bench note {i}", "visibility": "public"},
                )
                if r.status_code in (200, 204):
                    ok += 1
            except Exception:  # noqa: BLE001
                pass
        elapsed["v"] = time.monotonic() - start
        post_count["v"] = ok
    done.set()


def run_stack(stack: str, info: dict[str, Any]) -> dict[str, Any]:
    print(f"[{stack}] outbound bench starting...", flush=True)
    kind, redis_host = STACK_PROBES[stack]
    probe = make_probe(stack, kind, redis_host, "deliver")

    follower_count = info["follower_count"]
    expected_jobs = OUTBOUND_NOTES * follower_count

    reset_blackhole()
    baseline_hits = blackhole_hits()  # 通常 0 だが念の為

    samples: list[dict[str, float]] = []
    post_done = threading.Event()
    elapsed: dict[str, float] = {"v": 0.0}
    post_count: dict[str, int] = {"v": 0}

    poller_start = time.monotonic()
    poller = threading.Thread(
        target=post_notes_async,
        args=(info["url"], info["token"], OUTBOUND_NOTES, post_done, elapsed, post_count),
    )
    poller.start()

    deadline = time.monotonic() + DRAIN_TIMEOUT_S
    drained_at: float | None = None
    while time.monotonic() < deadline:
        depth = probe.depth()
        hits = blackhole_hits() - baseline_hits
        t = time.monotonic() - poller_start
        samples.append({"t": t, "depth": depth, "hits": hits})
        if post_done.is_set() and depth == 0 and hits >= expected_jobs:
            drained_at = t
            break
        time.sleep(POLL_INTERVAL_S)
    poller.join()

    final_hits = blackhole_hits() - baseline_hits
    drain_time = drained_at if drained_at is not None else (time.monotonic() - poller_start)
    timed_out = drained_at is None
    throughput = final_hits / drain_time if drain_time > 0 else 0.0
    peak = max((s["depth"] for s in samples), default=0)

    print(
        f"[{stack}] post {post_count['v']}/{OUTBOUND_NOTES} in {elapsed['v']:.2f}s, "
        f"drain={drain_time:.2f}s, hits={final_hits}/{expected_jobs}, "
        f"{throughput:.0f} jobs/s, peak depth={peak}",
        flush=True,
    )
    return {
        "stack": stack,
        "url": info["url"],
        "follower_count": follower_count,
        "notes_posted": post_count["v"],
        "expected_jobs": expected_jobs,
        "delivered_hits": final_hits,
        "post_elapsed_s": elapsed["v"],
        "drain_seconds": drain_time,
        "timed_out": timed_out,
        "throughput_jobs_per_sec": throughput,
        "peak_queue_depth": peak,
        "samples": samples[-200:],
    }


def main() -> int:
    with open("/state/seed.json") as f:
        seed = json.load(f)

    results = {}
    for stack, info in seed["stacks"].items():
        results[stack] = run_stack(stack, info)

    out_path = "/results/outbound.json"
    with open(out_path, "w") as f:
        json.dump(results, f, indent=2)
    print(f"outbound.json written to {out_path}", flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
