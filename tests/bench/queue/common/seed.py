"""Seed for queue-bench (#563).

Creates per-stack local users + admin tokens, inserts synthetic followers
(pointing at blackhole by default or an isolated delivery receiver when
enabled), inserts the faker actors as remote users for inbound bench, and
writes the bundle to /state/seed.json for subsequent driver runs.
"""
from __future__ import annotations

import json
import os
import secrets
import string
import sys
import time
from typing import Any

import httpx
import psycopg

STACKS = {
    "mkq": {
        "url": os.environ["MKGO_MKQ_URL"],
        "kind": "mkgo",
        "db_host": "postgres-mkq",
    },
    "ts": {
        "url": os.environ["TS_URL"],
        "kind": "ts",
        "db_host": "postgres-ts",
    },
}
selected_stacks = os.environ.get("QUEUE_BENCH_STACKS", "mkq,ts").split(",")
unknown_stacks = set(selected_stacks) - set(STACKS)
if unknown_stacks or not selected_stacks or any(not stack for stack in selected_stacks):
    raise ValueError(f"invalid QUEUE_BENCH_STACKS: {selected_stacks}")
STACKS = {stack: STACKS[stack] for stack in selected_stacks}

FAKER_URL = os.environ.get("FAKER_URL", "")
FAKER_HOST = os.environ.get("FAKER_HOST", "")
# 受信の計測で活動を送る投稿者の数 (#4138)。1 人から 1 万件を送ると、受信側で
# 同じ利用者の行への書き込みが行ロックの順番待ちになり、処理の速さではなく
# 順番待ちを測ってしまうので、既定は 1000 人 (1 万件なら 1 人 10 件)。
INBOUND_ACTORS = int(os.environ.get("INBOUND_ACTORS", "1000"))
SEED_FAKER = os.environ.get("QUEUE_BENCH_SEED_FAKER", "1") == "1"
OUTBOUND_FOLLOWERS = int(os.environ.get("OUTBOUND_FOLLOWERS", "100"))
DELIVERY_MODE = os.environ.get("QUEUE_BENCH_DELIVERY", "0") == "1"
DELIVERY_RECEIVERS = {
    "mkq": "http://deliveryreceiver-mkq",
    "ts": "http://deliveryreceiver-ts",
}
DB_USER = "misskey"
DB_PASS = "testpass"
DB_NAME = "misskey"


def wait_health(url: str, timeout: int = 240) -> None:
    end = time.time() + timeout
    last_err: Exception | None = None
    while time.time() < end:
        try:
            r = httpx.post(f"{url}/api/ping", json={}, timeout=5, verify=False)
            if r.status_code == 200:
                print(f"  {url} ready", flush=True)
                return
        except Exception as exc:  # noqa: BLE001
            last_err = exc
        time.sleep(2)
    raise TimeoutError(f"{url} not ready: {last_err}")


def create_admin(http: httpx.Client, username: str, password: str) -> str:
    r = http.post("/api/admin/accounts/create", json={"username": username, "password": password})
    if r.status_code == 200:
        return r.json().get("token", "")
    if r.status_code in (400, 403, 409):
        r2 = http.post("/api/signin-flow", json={"username": username, "password": password})
        r2.raise_for_status()
        data = r2.json()
        return data.get("i") or data.get("token") or ""
    r.raise_for_status()
    return ""


def fetch_local_user_id(http: httpx.Client, token: str) -> str:
    r = http.post("/api/i", json={"i": token})
    r.raise_for_status()
    return r.json()["id"]


def aidx_id(prefix: str = "f") -> str:
    """Generate a 16-char id similar to aidx (8 ts + 8 random)."""
    # 任意文字列で良い (受け取り側は string として扱う)。先頭 8 文字を時刻
    # ベースにして lexicographic 順をある程度保つ。
    chars = string.ascii_lowercase + string.digits
    rnd = "".join(secrets.choice(chars) for _ in range(8))
    ts_hex = format(int(time.time() * 1000), "x")[-8:]
    return (prefix + ts_hex + rnd)[:16].ljust(16, "0")


def enable_federation(stack: str, db_host: str) -> None:
    """Force meta.federation = 'all' so that outbound deliver jobs are
    enqueued. mk-go defaults a fresh instance to 'none' which suppresses
    every outbound HTTP, breaking the bench. Misskey TS defaults to 'all'
    but we set it for parity."""
    print(f"  [{stack}] enabling federation=all...", flush=True)
    with psycopg.connect(host=db_host, user=DB_USER, password=DB_PASS, dbname=DB_NAME) as conn:
        conn.autocommit = True
        with conn.cursor() as cur:
            # 列が無いリリースもあるので best-effort 更新。
            cur.execute(
                """
                UPDATE meta SET federation = 'all'
                WHERE federation IS DISTINCT FROM 'all'
                """
            )


def insert_fixture_followers(stack: str, db_host: str, local_user_id: str, count: int,
                             inbox_base: str = "http://blackhole") -> list[dict[str, str]]:
    """Insert N remote follower users that point at a local fixture, then
    follow local_user_id. Returns id, actor URI, and inbox for bookkeeping."""
    print(f"  [{stack}] inserting {count} synthetic followers...", flush=True)
    followers: list[dict[str, str]] = []
    with psycopg.connect(host=db_host, user=DB_USER, password=DB_PASS, dbname=DB_NAME) as conn:
        conn.autocommit = True
        with conn.cursor() as cur:
            for i in range(count):
                fid = aidx_id("b")
                username = f"black{i:04d}"
                uri = f"http://blackhole/users/{fid}"
                # 各 follower にユニークな inbox を割り当てないと
                # Inbox paths are unique per synthetic follower so the
                # receiver can reconcile each expected Activity/inbox tuple.
                inbox = f"{inbox_base}/inbox/{fid}"
                shared_inbox = inbox
                # Misskey の user table はカラムが多いが NOT NULL 制約のある
                # ものだけ埋めれば INSERT 可能。default でも残りは埋まる。
                cur.execute(
                    """
                    INSERT INTO "user" (id, username, "usernameLower", host,
                                        uri, inbox, "sharedInbox")
                    VALUES (%s, %s, %s, %s, %s, %s, %s)
                    ON CONFLICT (id) DO NOTHING
                    """,
                    (fid, username, username.lower(), "blackhole", uri, inbox, shared_inbox),
                )
                # follow 関係: blackhole user が local user を follow している
                cur.execute(
                    """
                    INSERT INTO following (id, "followerId", "followeeId",
                                          "followerHost", "followeeHost",
                                          "followerInbox", "followeeInbox",
                                          "followerSharedInbox", "followeeSharedInbox")
                    VALUES (%s, %s, %s, %s, NULL, %s, NULL, %s, NULL)
                    ON CONFLICT DO NOTHING
                    """,
                    (aidx_id("fl"), fid, local_user_id, "blackhole", inbox, shared_inbox),
                )
                followers.append({"id": fid, "uri": uri, "inbox": inbox})
    print(f"  [{stack}] inserted {len(followers)} followers", flush=True)
    return followers


def fetch_faker_actors(count: int) -> dict[str, Any]:
    """List the faker's first `count` actors and their shared public key."""
    r = httpx.get(f"{FAKER_URL}/actors", params={"count": count}, timeout=30)
    r.raise_for_status()
    return r.json()


def faker_actor_rows(faker: dict[str, Any]) -> list[dict[str, str]]:
    """Turn the faker's actor list into the rows to insert on a receiver.

    Each actor gets its own user id; the key pem is shared (the faker signs
    with one key pair) but the key id is per actor.
    """
    pem = faker["publicKeyPem"]
    rows = []
    for actor in faker["actors"]:
        rows.append({
            "id": aidx_id("fk"),
            "username": actor["username"],
            "usernameLower": actor["username"].lower(),
            "uri": actor["id"],
            "inbox": actor["inbox"],
            "keyId": actor["keyId"],
            "keyPem": pem,
        })
    if len({r["id"] for r in rows}) != len(rows):
        raise RuntimeError("generated duplicate user ids for the faker actors; run the seed again")
    return rows


def faker_state(faker: dict[str, Any]) -> dict[str, Any]:
    """What seed.json records about the faker actors.

    driver はここの数を faker に渡す。登録した投稿者と送る投稿者を揃えるため。
    """
    return {
        "actors": len(faker["actors"]),
        "first_actor_uri": faker["actors"][0]["id"],
    }


def insert_faker_actors(stack: str, db_host: str, faker: dict[str, Any]) -> int:
    """Pre-seed the faker actors as remote users on the receiver stack
    so inbox processing does not require a fresh actor fetch / verify
    round-trip. Returns the number of actors inserted."""
    rows = faker_actor_rows(faker)
    print(f"  [{stack}] inserting {len(rows)} faker actors...", flush=True)

    with psycopg.connect(host=db_host, user=DB_USER, password=DB_PASS, dbname=DB_NAME) as conn:
        with conn.cursor() as cur:
            insert_faker_actor_rows(cur, rows)
        conn.commit()
    return len(rows)


def insert_faker_actor_rows(cur: Any, rows: list[dict[str, str]]) -> None:
    """Insert the faker actors and their public keys with one cursor.

    lastFetchedAt を今にして、取得済みの投稿者として登録する。無いと Elythia は
    投稿者ごとに最初の活動で actor を同期で取りに行き、その取得を計測に含めて
    しまう (本家も無いと処理の後に裏で取り直す)。実際の連合でも、投稿が届く
    相手の多くは既に知っている投稿者。
    """
    cur.executemany(
        """
        INSERT INTO "user" (id, username, "usernameLower", host,
                            uri, inbox, "lastFetchedAt")
        VALUES (%s, %s, %s, %s, %s, %s, now())
        ON CONFLICT (id) DO UPDATE SET inbox = EXCLUDED.inbox
        """,
        [(r["id"], r["username"], r["usernameLower"], FAKER_HOST, r["uri"], r["inbox"]) for r in rows],
    )
    # public key が user_publickey 等の別テーブルに置かれる場合があるので、
    # 主要 schema (mk-go / Misskey) 両対応で best-effort upsert する。
    cur.execute(
        """
        SELECT to_regclass('public.user_publickey') IS NOT NULL,
               to_regclass('public.user_keypair') IS NOT NULL
        """
    )
    row = cur.fetchone()
    has_pubkey, has_keypair = row if row else (False, False)
    if has_pubkey:
        cur.executemany(
            """
            INSERT INTO user_publickey ("userId", "keyId", "keyPem")
            VALUES (%s, %s, %s)
            ON CONFLICT ("userId") DO UPDATE
            SET "keyId" = EXCLUDED."keyId",
                "keyPem" = EXCLUDED."keyPem"
            """,
            [(r["id"], r["keyId"], r["keyPem"]) for r in rows],
        )
    elif has_keypair:
        # Misskey TS uses user_keypair with publicKey/privateKey columns
        cur.executemany(
            """
            INSERT INTO user_keypair ("userId", "publicKey", "privateKey")
            VALUES (%s, %s, %s)
            ON CONFLICT ("userId") DO UPDATE
            SET "publicKey" = EXCLUDED."publicKey"
            """,
            [(r["id"], r["keyPem"], "") for r in rows],
        )


def main() -> int:
    print("queue-bench seed starting...", flush=True)

    state: dict[str, Any] = {"stacks": {}, "faker": {}}

    for stack, info in STACKS.items():
        wait_health(info["url"])

    faker = fetch_faker_actors(INBOUND_ACTORS) if SEED_FAKER else None
    if faker:
        state["faker"] = faker_state(faker)

    for stack, info in STACKS.items():
        url = info["url"]
        print(f"[{stack}] {url}", flush=True)
        target_note_uri = ""
        with httpx.Client(base_url=url, timeout=20, verify=False) as http:
            password = "bench-pass-1234"
            token = create_admin(http, "benchsender", password)
            user_id = fetch_local_user_id(http, token)
            # Announce 経路 bench 用に target note を 1 件作る。faker が
            # この URI を object として Announce を投げ、handleAnnounce が
            # renote を生成する path を計測する。local note の `uri` は mk /
            # TS どちらでも null なので、AS URI を base_url + /notes/<id> で
            # 組み立てる (Misskey 互換)。
            r = http.post(
                "/api/notes/create",
                json={"i": token, "text": "queue-bench announce target", "visibility": "public"},
            )
            r.raise_for_status()
            note_id = r.json().get("createdNote", {}).get("id", "")
            if note_id:
                target_note_uri = f"{url}/notes/{note_id}"

        enable_federation(stack, info["db_host"])
        receiver_url = DELIVERY_RECEIVERS[stack] if DELIVERY_MODE else "http://blackhole"
        followers = insert_fixture_followers(
            stack, info["db_host"], user_id, OUTBOUND_FOLLOWERS, receiver_url,
        )
        faker_actor_count = (
            insert_faker_actors(stack, info["db_host"], faker)
            if faker
            else 0
        )

        stack_state: dict[str, Any] = {
            "url": url,
            "kind": info["kind"],
            "token": token,
            "user_id": user_id,
            "follower_count": len(followers),
            "faker_actor_count": faker_actor_count,
        }
        if DELIVERY_MODE:
            stack_state["delivery_enabled"] = True
            stack_state["delivery_receiver_url"] = receiver_url
            stack_state["delivery_inboxes"] = [f["inbox"] for f in followers]
        # 空文字列ではなく key 自体を omit する。consumer 側は
        # info.get("target_note_uri", "") で取り出すので互換は維持される。
        if target_note_uri:
            stack_state["target_note_uri"] = target_note_uri
        state["stacks"][stack] = stack_state

    out_path = "/state/seed.json"
    with open(out_path, "w") as f:
        json.dump(state, f, indent=2)
    print(f"seed.json written to {out_path}", flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
