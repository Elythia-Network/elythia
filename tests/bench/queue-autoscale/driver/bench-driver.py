"""Auto-scale comparison bench driver (#1126 / #1120 tracker).

Runs ONE scenario per invocation against a freshly-restarted mk-go stack:

  1. enable federation='all' in meta (mk-go default 'none' suppresses deliver)
  2. signup admin user → API token
  3. insert N blackhole follower remote actors + follow rows via psycopg
     (so notes/create fan-out generates N × OUTBOUND_NOTES deliver jobs)
  4. reset blackhole hits, snapshot idle Redis CLIENT count
  5. post OUTBOUND_NOTES notes (visibility=public) → fan-out
  6. measure drain time = time until blackhole hits == expected count
  7. snapshot busy Redis CLIENT count
  8. write JSON result to /results/<scenario>.json

Orchestrator (`run.sh`) wraps this to iterate across 3 scenarios
(fixed16, fixed64, auto) by `compose down -v` + re-up with the right
config mount between each invocation, ensuring clean per-scenario state.

Reuses the seeding pattern from tests/bench/queue/common/seed.py
(simplified to a single stack).
"""
from __future__ import annotations

import json
import os
import secrets
import sys
import threading
import time

import httpx
import psycopg
import redis
import urllib3

APP_URL = os.environ["APP_URL"]
BLACKHOLE_URL = os.environ["BLACKHOLE_URL"]
REDIS_HOST = os.environ["REDIS_HOST"]
DB_HOST = os.environ.get("DB_HOST", "postgres")
SCENARIO = os.environ["SCENARIO"]
OUTBOUND_NOTES = int(os.environ.get("OUTBOUND_NOTES", "10"))
FOLLOWERS = int(os.environ.get("FOLLOWERS", "50"))
DRAIN_TIMEOUT_S = float(os.environ.get("DRAIN_TIMEOUT_S", "240"))
POLL_INTERVAL_S = 0.1
IDLE_OBSERVE_S = float(os.environ.get("IDLE_OBSERVE_S", "10"))
RESULTS_DIR = "/results"

# DB credentials は compose の POSTGRES_* と同期する必要がある。env 経由で
# 1 箇所定義 (= compose) に集約し、driver は default だけ持つ形で drift を回避。
DB_USER = os.environ.get("DB_USER", "misskey")
DB_PASS = os.environ.get("DB_PASS", "testpass")
DB_NAME = os.environ.get("DB_NAME", "misskey")

urllib3.disable_warnings(urllib3.exceptions.InsecureRequestWarning)


def aidx_id(prefix: str = "b") -> str:
    """Returns a 16-char unique ID with `a` + prefix char + 14 random
    base36 chars. Matches mk-go aidx の **長さだけ** を模倣する (実 aidx
    は time-base36 8 + nodeID 4 + counter 4 で timeline ordering を保証
    するが、本 helper は完全ランダムで ordering 保証なし)。bench では
    create 順 = ID 順を仮定しないので問題ない。"""
    alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
    rand = "".join(secrets.choice(alphabet) for _ in range(14))
    return f"a{prefix[:1]}{rand}"


def verify_federation_enabled() -> None:
    """Assert meta.federation='all' is set + cached in the running app.
    run.sh runs the UPDATE + restart before invoking this driver, so we
    just verify the live state here."""
    with psycopg.connect(host=DB_HOST, user=DB_USER, password=DB_PASS,
                         dbname=DB_NAME) as conn:
        with conn.cursor() as cur:
            cur.execute("SELECT federation FROM meta")
            row = cur.fetchone()
            if not row or row[0] != "all":
                raise RuntimeError(
                    f"meta.federation must be 'all' for the bench but got {row}; "
                    f"run.sh should UPDATE + restart before driver runs"
                )
    print("[seed] federation=all verified", flush=True)


def signup_admin() -> tuple[str, str]:
    """Returns (user_id, token) for a freshly created admin."""
    username = f"bench_{secrets.token_hex(4)}"
    password = secrets.token_hex(8)
    r = httpx.post(
        f"{APP_URL}/api/admin/accounts/create",
        json={"username": username, "password": password},
        verify=False,
        timeout=30,
    )
    r.raise_for_status()
    body = r.json()
    return body["id"], body["token"]


def insert_blackhole_followers(local_user_id: str, count: int) -> None:
    """Insert N remote follower users pointing at blackhole + follow rows.

    `executemany` で 2 batch round-trip に集約 (FOLLOWERS=100 で 2 round-trip、
    旧 1-row-per-INSERT 設計の 200 round-trip 比 100x 削減)。bench の前段
    overhead が無視できる範囲に。
    """
    print(f"[seed] inserting {count} blackhole followers", flush=True)
    user_rows: list[tuple] = []
    follow_rows: list[tuple] = []
    for i in range(count):
        fid = aidx_id("b")
        username = f"black{i:04d}"
        # Per-follower unique inbox so DeliverActivity's seen dedup map
        # does not collapse the burst into a single job. blackhole は
        # port 8080 で listen するので明示。
        uri = f"http://blackhole:8080/users/{fid}"
        inbox = f"http://blackhole:8080/inbox/{fid}"
        user_rows.append((fid, username, username.lower(), "blackhole",
                          uri, inbox, inbox))
        follow_rows.append((aidx_id("f"), fid, local_user_id, "blackhole",
                            inbox, inbox))

    with psycopg.connect(host=DB_HOST, user=DB_USER, password=DB_PASS,
                         dbname=DB_NAME) as conn:
        conn.autocommit = True
        with conn.cursor() as cur:
            cur.executemany(
                """
                INSERT INTO "user" (id, username, "usernameLower", host,
                                    uri, inbox, "sharedInbox")
                VALUES (%s, %s, %s, %s, %s, %s, %s)
                ON CONFLICT (id) DO NOTHING
                """,
                user_rows,
            )
            cur.executemany(
                """
                INSERT INTO following (id, "followerId", "followeeId",
                                      "followerHost", "followeeHost",
                                      "followerInbox", "followeeInbox",
                                      "followerSharedInbox", "followeeSharedInbox")
                VALUES (%s, %s, %s, %s, NULL, %s, NULL, %s, NULL)
                ON CONFLICT DO NOTHING
                """,
                follow_rows,
            )


def reset_blackhole() -> None:
    httpx.post(f"{BLACKHOLE_URL}/reset", timeout=5)


def blackhole_hits() -> int:
    r = httpx.get(f"{BLACKHOLE_URL}/stats", timeout=5)
    r.raise_for_status()
    return int(r.json()["hits"])


def redis_client_count() -> int:
    """Number of Redis CLIENT LIST entries. Proxy for mkq Worker pool
    size (each Worker holds 1 BLPOP-blocking connection in addition to
    short-lived publish connections)."""
    rc = redis.Redis.from_url(f"redis://{REDIS_HOST}")
    try:
        return len(rc.client_list())
    finally:
        rc.close()


def queue_depth_total() -> int:
    """Sum of pending BullMQ lists across all mkq queues. drain == 0
    confirms no in-flight jobs left in any queue.

    queue 名は internal/queue/queue.go の {QueueName, InboxQueueName,
    ExportQueueName, PushQueueName, WebhookQueueName} と 同期する必要が
    ある (Python から Go 定数を import 不可能なため hardcode 必要)。
    queue 追加 / rename 時は本 list も同時に更新すること。
    """
    rc = redis.Redis.from_url(f"redis://{REDIS_HOST}")
    try:
        total = 0
        for q in ("deliver", "inbox", "export", "push", "webhook"):
            total += rc.llen(f"bull:{q}:wait")
            total += rc.llen(f"bull:{q}:active")
        return total
    finally:
        rc.close()


def post_note(token: str, n: int, failures: list[int]) -> None:
    """Posts one note. On failure, log and append to `failures` (caller-
    owned list). Caller passes a shared mutable list so failures across
    threads are observable without locking (list.append is thread-safe
    via the GIL for built-in list ops)."""
    try:
        httpx.post(
            f"{APP_URL}/api/notes/create",
            json={"text": f"bench note #{n}", "visibility": "public"},
            headers={"Authorization": f"Bearer {token}"},
            verify=False,
            timeout=30,
        )
    except Exception as e:
        print(f"[bench] post_note #{n} failed: {e}", flush=True)
        failures.append(n)


def drain_loop(expected_hits: int, started_at: float) -> tuple[float, int]:
    """Polls blackhole hits + queue depth until either:
      - hits >= expected AND queue depth == 0 → return (elapsed, hits)
      - DRAIN_TIMEOUT_S exceeded → return (-1, hits)
    """
    deadline = started_at + DRAIN_TIMEOUT_S
    while True:
        hits = blackhole_hits()
        depth = queue_depth_total()
        now = time.monotonic()
        if hits >= expected_hits and depth == 0:
            return now - started_at, hits
        if now >= deadline:
            return -1.0, hits
        time.sleep(POLL_INTERVAL_S)


def main() -> None:
    os.makedirs(RESULTS_DIR, exist_ok=True)

    verify_federation_enabled()
    user_id, token = signup_admin()
    print(f"[{SCENARIO}] admin user_id={user_id}", flush=True)
    insert_blackhole_followers(user_id, FOLLOWERS)

    # idle observation: 10s ほど何もせず Redis CLIENT 数を観測
    print(f"[{SCENARIO}] idle observe ({IDLE_OBSERVE_S}s)...", flush=True)
    time.sleep(IDLE_OBSERVE_S)
    idle_clients = redis_client_count()
    print(f"[{SCENARIO}] idle redis clients = {idle_clients}", flush=True)

    # burst: OUTBOUND_NOTES 件の note を一気に POST → fan-out で
    # OUTBOUND_NOTES × FOLLOWERS = N deliver job が enqueue される
    reset_blackhole()
    started = time.monotonic()
    expected_hits = OUTBOUND_NOTES * FOLLOWERS
    print(f"[{SCENARIO}] posting {OUTBOUND_NOTES} notes "
          f"(expected {expected_hits} deliver jobs)", flush=True)
    post_failures: list[int] = []
    threads = [
        threading.Thread(target=post_note, args=(token, i, post_failures), daemon=True)
        for i in range(OUTBOUND_NOTES)
    ]
    for t in threads:
        t.start()
    for t in threads:
        t.join(timeout=60)
    post_submit = time.monotonic() - started

    # post 失敗があった場合、実 fan-out が減るので drain time を
    # underestimate しないよう expected_hits を実成功 post 数で計算し直す。
    successful_posts = OUTBOUND_NOTES - len(post_failures)
    effective_expected = successful_posts * FOLLOWERS
    print(f"[{SCENARIO}] all posts submitted in {post_submit:.2f}s "
          f"(failures={len(post_failures)}), draining...", flush=True)
    drain_s, hits = drain_loop(expected_hits=effective_expected, started_at=started)
    busy_clients = redis_client_count()

    result = {
        "scenario": SCENARIO,
        "outbound_notes": OUTBOUND_NOTES,
        "followers_per_note": FOLLOWERS,
        "expected_deliver_jobs": effective_expected,
        "post_failures": len(post_failures),
        "post_submit_s": round(post_submit, 3),
        "drain_time_s": round(drain_s, 3) if drain_s >= 0 else None,
        "drain_timed_out": drain_s < 0,
        "blackhole_hits": hits,
        "throughput_jobs_per_s": (
            round(hits / drain_s, 1) if drain_s > 0 else None
        ),
        "idle_redis_clients": idle_clients,
        "busy_redis_clients": busy_clients,
    }
    out_path = os.path.join(RESULTS_DIR, f"{SCENARIO}.json")
    with open(out_path, "w") as f:
        json.dump(result, f, indent=2)
    print(f"[{SCENARIO}] result: {json.dumps(result)}", flush=True)


if __name__ == "__main__":
    try:
        main()
    except Exception as e:
        import traceback
        print(f"[{SCENARIO}] FAILED: {e}", file=sys.stderr, flush=True)
        traceback.print_exc()
        sys.exit(1)
