"""Outbound deliver-throughput driver (#563).

Per stack:
  1. Reset blackhole hit counter
  2. Concurrently start a depth/hits poller AND post `OUTBOUND_NOTES`
     notes/create requests against the sender stack
  3. Drain detection = receiver count reaches the expected total AND the
     redis queue depth has settled to 0
  4. Compute drain time = T_drain - T_post_start; signed delivery throughput
     uses only the final, reconciled unique delivery count

The receiver counter is the authoritative source of truth for "job
completed" because the queue probe alone can miss bursts shorter than the
100ms polling interval. Manifest reconciliation runs after the timed interval.
"""
from __future__ import annotations

import json
import os
import sys
import threading
import time
from typing import Any

import httpx

from delivery_manifest import build_deliveries, created_note_id
from delivery_result import throughput_result
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


def receiver_stats(receiver_url: str) -> dict[str, Any]:
    r = httpx.get(f"{receiver_url}/stats", timeout=5)
    r.raise_for_status()
    return r.json()


def wait_receiver(receiver_url: str, timeout: float = 30.0) -> None:
    deadline = time.monotonic() + timeout
    last_error: Exception | None = None
    while time.monotonic() < deadline:
        try:
            health = httpx.get(f"{receiver_url}/healthz", timeout=3)
            health.raise_for_status()
            return
        except Exception as exc:  # noqa: BLE001
            last_error = exc
            time.sleep(0.25)
    raise TimeoutError(f"delivery receiver {receiver_url} not ready: {last_error}")


def prepare_receiver(info: dict[str, Any]) -> None:
    """Register the synthetic sender's existing public key with its receiver."""
    receiver_url = info["delivery_receiver_url"]
    wait_receiver(receiver_url)
    actor = httpx.get(
        f"{info['url']}/users/{info['user_id']}",
        headers={"Accept": "application/activity+json"},
        timeout=10,
        verify=False,
    )
    actor.raise_for_status()
    public_key = actor.json().get("publicKey")
    if not isinstance(public_key, dict):
        raise ValueError("synthetic actor document has no publicKey")
    key_id = public_key.get("id")
    public_pem = public_key.get("publicKeyPem")
    if not isinstance(key_id, str) or not isinstance(public_pem, str):
        raise ValueError("synthetic actor document has an incomplete publicKey")
    configured = httpx.post(
        f"{receiver_url}/keys",
        json={"keys": {key_id: public_pem}},
        timeout=10,
    )
    configured.raise_for_status()


def finalize_delivery(info: dict[str, Any], note_ids: list[str]) -> tuple[dict[str, Any], str | None]:
    """Fetch expected activities after timing and reconcile the receiver."""
    receiver_url = info["delivery_receiver_url"]
    try:
        if len(note_ids) != OUTBOUND_NOTES or any(not note_id for note_id in note_ids):
            raise ValueError(f"expected {OUTBOUND_NOTES} created note IDs, got {len(note_ids)}")
        deliveries: list[dict[str, str]] = []
        for note_id in note_ids:
            response = httpx.get(
                f"{info['url']}/notes/{note_id}/activity",
                headers={"Accept": "application/activity+json"},
                timeout=10,
                verify=False,
            )
            response.raise_for_status()
            deliveries.extend(build_deliveries(response.content, info["delivery_inboxes"]))
        expected = httpx.post(
            f"{receiver_url}/expect",
            json={"deliveries": deliveries},
            timeout=30,
        )
        expected.raise_for_status()
        return receiver_stats(receiver_url), None
    except Exception as exc:  # noqa: BLE001
        error = f"{type(exc).__name__}: {exc}"
        try:
            return receiver_stats(receiver_url), error
        except Exception as stats_exc:  # noqa: BLE001
            return {"finalized": False}, (
                f"{error}; receiver stats unavailable: "
                f"{type(stats_exc).__name__}: {stats_exc}"
            )


def post_notes_async(url: str, token: str, count: int, done: threading.Event,
                     elapsed: dict[str, float], post_count: dict[str, int],
                     post_responses: list[bytes] | None = None) -> None:
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
                    if post_responses is not None:
                        post_responses.append(r.content)
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
    delivery_enabled = bool(info.get("delivery_enabled"))
    post_responses: list[bytes] | None = [] if delivery_enabled else None

    if not delivery_enabled:
        reset_blackhole()
    if delivery_enabled:
        try:
            prepare_receiver(info)
            httpx.post(f"{info['delivery_receiver_url']}/reset", timeout=5).raise_for_status()
        except Exception as exc:  # noqa: BLE001
            error = f"{type(exc).__name__}: {exc}"
            return {
                "stack": stack,
                "url": info["url"],
                "follower_count": follower_count,
                "notes_posted": 0,
                "expected_jobs": expected_jobs,
                "delivered_hits": 0,
                "post_elapsed_s": 0.0,
                "drain_seconds": 0.0,
                "timed_out": True,
                "throughput_jobs": None,
                "throughput_jobs_per_sec": None,
                "peak_queue_depth": 0,
                "samples": [],
                "delivery": {"finalized": False},
                "deliveryStatus": "incomplete",
                "deliveryReasons": [f"delivery receiver setup failed: {error}"],
                "deliveryPreparationError": error,
            }
    baseline_hits = 0 if delivery_enabled else blackhole_hits()

    samples: list[dict[str, float]] = []
    post_done = threading.Event()
    elapsed: dict[str, float] = {"v": 0.0}
    post_count: dict[str, int] = {"v": 0}

    poller_start = time.monotonic()
    poller = threading.Thread(
        target=post_notes_async,
        args=(info["url"], info["token"], OUTBOUND_NOTES, post_done, elapsed, post_count, post_responses),
    )
    poller.start()

    deadline = time.monotonic() + DRAIN_TIMEOUT_S
    drained_at: float | None = None
    while time.monotonic() < deadline:
        depth = probe.depth()
        hits = (
            int(receiver_stats(info["delivery_receiver_url"]).get("verified", 0))
            if delivery_enabled
            else blackhole_hits() - baseline_hits
        )
        t = time.monotonic() - poller_start
        samples.append({"t": t, "depth": depth, "hits": hits})
        if post_done.is_set() and depth == 0 and hits >= expected_jobs:
            drained_at = t
            break
        time.sleep(POLL_INTERVAL_S)
    poller.join()

    note_ids: list[str] = []
    if post_responses is not None:
        for body in post_responses:
            note_id = created_note_id(body)
            if note_id:
                note_ids.append(note_id)

    final_hits = (
        int(receiver_stats(info["delivery_receiver_url"]).get("verified", 0))
        if delivery_enabled
        else blackhole_hits() - baseline_hits
    )
    drain_time = drained_at if drained_at is not None else (time.monotonic() - poller_start)
    timed_out = drained_at is None
    peak = max((s["depth"] for s in samples), default=0)

    result = {
        "stack": stack,
        "url": info["url"],
        "follower_count": follower_count,
        "notes_posted": post_count["v"],
        "expected_jobs": expected_jobs,
        "delivered_hits": final_hits,
        "post_elapsed_s": elapsed["v"],
        "drain_seconds": drain_time,
        "timed_out": timed_out,
        "throughput_jobs": final_hits,
        "throughput_jobs_per_sec": final_hits / drain_time if drain_time > 0 else None,
        "peak_queue_depth": peak,
        "samples": samples[-200:],
    }
    if delivery_enabled:
        delivery, preparation_error = finalize_delivery(info, note_ids)
        result.update(throughput_result(
            delivery,
            drain_time,
            benchmark_expected=expected_jobs,
            timed_out=timed_out,
            preparation_error=preparation_error,
        ))
        if preparation_error:
            result["deliveryPreparationError"] = preparation_error
    throughput = result["throughput_jobs_per_sec"]
    throughput_label = (
        f"{throughput:.0f} jobs/s" if throughput is not None else "throughput unavailable"
    )
    print(
        f"[{stack}] post {post_count['v']}/{OUTBOUND_NOTES} in {elapsed['v']:.2f}s, "
        f"drain={drain_time:.2f}s, hits={final_hits}/{expected_jobs}, "
        f"{throughput_label}, peak depth={peak}",
        flush=True,
    )
    return result


def delivery_artifact(result: dict[str, Any]) -> dict[str, Any]:
    """Flatten one stack's receiver counters and measured throughput for benchcompare."""
    stats = result.get("delivery")
    artifact = dict(stats) if isinstance(stats, dict) else {"finalized": False}
    artifact.update({
        "status": result.get("deliveryStatus", "incomplete"),
        "reasons": result.get("deliveryReasons", []),
        "throughputJobs": result.get("throughput_jobs"),
        "throughputJobsPerSecond": result.get("throughput_jobs_per_sec"),
        "drainSeconds": result.get("drain_seconds"),
    })
    return artifact


def main() -> int:
    with open("/state/seed.json") as f:
        seed = json.load(f)

    results = {}
    delivery_preparation_failed = False
    for stack, info in seed["stacks"].items():
        results[stack] = run_stack(stack, info)
        delivery_preparation_failed |= bool(results[stack].get("deliveryPreparationError"))

    out_path = "/results/outbound.json"
    with open(out_path, "w") as f:
        json.dump(results, f, indent=2)
    print(f"outbound.json written to {out_path}", flush=True)
    if os.environ.get("QUEUE_BENCH_EXPORT_DELIVERY") == "1":
        stack = os.environ.get("QUEUE_BENCH_DELIVERY_STACK", "mkq")
        delivery_path = "/results/delivery.json"
        with open(delivery_path, "w") as f:
            json.dump(delivery_artifact(results.get(stack, {})), f, indent=2)
        print(f"delivery.json written to {delivery_path}", flush=True)
    return 1 if delivery_preparation_failed else 0


if __name__ == "__main__":
    sys.exit(main())
