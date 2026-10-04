"""Inbound inbox-throughput driver (#563).

Drives the faker control API to blast pre-signed AP Create activities
at each receiver's `/inbox`, while polling the receiver's inbox queue
depth until drained. Compares pure receiver inbox processing throughput
across the three drivers.
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

INBOUND_COUNT = int(os.environ.get("INBOUND_COUNT", "10000"))
INBOUND_CONCURRENCY = int(os.environ.get("INBOUND_CONCURRENCY", "128"))
# Activity type を切替: create (default) / announce。announce の場合は
# seed.json から各 stack の target_note_uri を読んで faker に objects map を
# 渡す。#1158 の handleAnnounce async 化の効果を計測するときに announce 指定。
INBOUND_ACTIVITY_TYPE = os.environ.get("INBOUND_ACTIVITY_TYPE", "create")
POLL_INTERVAL_S = 0.1
DRAIN_TIMEOUT_S = 600.0
FAKER_URL = os.environ["FAKER_URL"]

STACK_PROBES: dict[str, tuple[DriverKind, str]] = {
    "mkq": ("mkq", os.environ["REDIS_MKQ_HOST"]),
    "ts": ("bullmq", os.environ["REDIS_TS_HOST"]),
}

# Receiver hostname → inbox URL に変換するためのマップ。faker は `target` に
# 受信側 inbox URL をそのまま受け取る。
INBOX_URLS = {
    "mkq": "https://mk-mkq/inbox",
    "ts": "https://ts/inbox",
}


def faker_send(targets: list[str], count: int, concurrency: int,
               activity_type: str, objects: dict[str, str]) -> dict[str, Any]:
    payload: dict[str, Any] = {
        "targets": targets,
        "count": count,
        "concurrency": concurrency,
        "activityType": activity_type,
    }
    if activity_type == "announce":
        payload["objects"] = objects
    # send は count*len(targets) 件を blasting するので timeout は十分長く。
    r = httpx.post(f"{FAKER_URL}/send", json=payload, timeout=DRAIN_TIMEOUT_S)
    r.raise_for_status()
    return r.json()


def drain_one(stack: str, info: dict[str, Any], expected: int,
              send_done: threading.Event) -> dict[str, Any]:
    kind, redis_host = STACK_PROBES[stack]
    probe = make_probe(stack, kind, redis_host, "inbox")

    samples: list[dict[str, float]] = []
    deadline = time.monotonic() + DRAIN_TIMEOUT_S
    start = time.monotonic()
    drained_at: float | None = None
    saw_work = False  # True になった以降に depth=0 を観測したら drained と扱う

    while time.monotonic() < deadline:
        depth = probe.depth()
        samples.append({"t": time.monotonic() - start, "depth": depth})
        if depth > 0:
            saw_work = True
        # drain 判定: faker.send がまだ走っている間は depth==0 を瞬間的に
        # observe しても drained と扱わない。faker が送り終わって (send_done)、
        # かつそれまでに少なくとも 1 度 depth>0 を見ている (saw_work)、かつ
        # 現時点で depth==0 — の 3 条件揃って初めて drained 扱い。
        # 例外: faker が瞬間的に終わって receiver workers が即処理した
        # (= saw_work が立たない) ケースは send_done 後の depth==0 で抜ける。
        if depth == 0 and send_done.is_set() and len(samples) > 1:
            drained_at = samples[-1]["t"]
            break
        time.sleep(POLL_INTERVAL_S)

    drain_time = drained_at if drained_at is not None else (time.monotonic() - start)
    timed_out = drained_at is None
    throughput = expected / drain_time if drain_time > 0 else 0.0
    peak = max((s["depth"] for s in samples), default=0)
    return {
        "stack": stack,
        "expected_jobs": expected,
        "drain_seconds": drain_time,
        "timed_out": timed_out,
        "throughput_jobs_per_sec": throughput,
        "peak_queue_depth": peak,
        "saw_work": saw_work,
        "samples": samples[-200:],
    }


def main() -> int:
    """Run the inbound bench against every stack declared in seed.json.

    Return codes (#1163 documented contract for CI integration):
      0 — bench completed successfully (results written to /results/inbound.json)
      2 — announce mode requested but seed.json is missing target_note_uri
          for one or more stacks (= caller must re-run `make queue-bench-seed`)

    Other non-zero exit codes propagate from sys.exit / uncaught exceptions
    (httpx errors / JSON decode failures / I/O errors) without explicit
    handling here.
    """
    with open("/state/seed.json") as f:
        seed = json.load(f)

    targets = [INBOX_URLS[stack] for stack in seed["stacks"].keys()]

    # announce mode: target inbox URL → 受信側 local note URI へのマップを
    # seed.json から作る。target_note_uri が欠けている stack があると
    # faker 側で「announce mode requires Objects[...]」error になり bench が
    # 中途半端に進むため、driver 側で先に abort して原因 (= seed 失敗) を
    # 明示する。
    objects: dict[str, str] = {}
    if INBOUND_ACTIVITY_TYPE == "announce":
        missing: list[str] = []
        for stack, info in seed["stacks"].items():
            uri = info.get("target_note_uri", "")
            if uri:
                objects[INBOX_URLS[stack]] = uri
            else:
                missing.append(stack)
        if missing:
            # CJK double-width を踏まえて 1 行 80 col 相当に分割 (#1163)。
            print(
                f"error: announce mode but {len(missing)} stacks miss "
                f"target_note_uri: {missing}.\n"
                "seed が失敗している可能性があります。\n"
                "make queue-bench-seed のログを確認してください。",
                flush=True,
            )
            return 2

    print(
        f"firing faker: {len(targets)} targets x {INBOUND_COUNT} count "
        f"({INBOUND_COUNT*len(targets)} total) @ concurrency={INBOUND_CONCURRENCY} "
        f"activityType={INBOUND_ACTIVITY_TYPE}",
        flush=True,
    )

    # faker.send は同期的に blast 完了まで待つ。一方で receiver の inbox
    # ジョブは送出と同時にエンキューされ始めるので、receiver 側の drain
    # 監視は faker 呼び出しと並行で行う必要がある。並行 thread で立ち
    # 上げる。send_done event を共有して、drain 判定が faker 完了前に
    # 早期 break しないようにする (#564 Devin BUG-1)。
    drain_results: dict[str, dict[str, Any]] = {}
    send_done = threading.Event()

    def drain_thread(stack: str, info: dict[str, Any]) -> None:
        drain_results[stack] = drain_one(stack, info, INBOUND_COUNT, send_done)

    threads = [
        threading.Thread(target=drain_thread, args=(stack, info))
        for stack, info in seed["stacks"].items()
    ]
    for t in threads:
        t.start()

    # send 開始
    send_start = time.monotonic()
    send_resp = faker_send(targets, INBOUND_COUNT, INBOUND_CONCURRENCY,
                           INBOUND_ACTIVITY_TYPE, objects)
    send_elapsed = time.monotonic() - send_start
    send_done.set()
    print(
        f"faker send done in {send_elapsed:.2f}s "
        f"(presign {send_resp['preSignMs']:.0f}ms, total {send_resp['totalMs']:.0f}ms)",
        flush=True,
    )

    for t in threads:
        t.join()

    out = {
        "send": send_resp,
        "send_elapsed_s": send_elapsed,
        "drain": drain_results,
    }
    out_path = "/results/inbound.json"
    with open(out_path, "w") as f:
        json.dump(out, f, indent=2)
    print(f"inbound.json written to {out_path}", flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
