"""Seed for queue-bench (#563).

Creates per-stack local users + admin tokens, inserts dummy followers
(pointing at blackhole) for outbound bench, inserts the faker actor as
a remote user for inbound bench, and writes the bundle to /state/seed.json
so subsequent driver runs can read tokens / actor metadata.
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

FAKER_URL = os.environ["FAKER_URL"]
FAKER_HOST = os.environ["FAKER_HOST"]
FAKER_BASE_HTTPS = os.environ["FAKER_BASE_HTTPS"]
FAKER_ACTOR_PATH = "/users/bench-sender"
OUTBOUND_FOLLOWERS = int(os.environ.get("OUTBOUND_FOLLOWERS", "100"))
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


def insert_blackhole_followers(stack: str, db_host: str, local_user_id: str, count: int) -> list[dict[str, str]]:
    """Insert N remote follower users that point at the blackhole, then
    follow local_user_id. Returns list of (id, uri) for caller bookkeeping."""
    print(f"  [{stack}] inserting {count} blackhole followers...", flush=True)
    followers: list[dict[str, str]] = []
    with psycopg.connect(host=db_host, user=DB_USER, password=DB_PASS, dbname=DB_NAME) as conn:
        conn.autocommit = True
        with conn.cursor() as cur:
            for i in range(count):
                fid = aidx_id("b")
                username = f"black{i:04d}"
                uri = f"http://blackhole/users/{fid}"
                # 各 follower にユニークな inbox を割り当てないと
                # DeliverActivity の dedup (seen map) で 1 job に縮退する。
                # blackhole は path をすべて 204 で受けるので path だけ
                # 変えれば良い。
                inbox = f"http://blackhole/inbox/{fid}"
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
                followers.append({"id": fid, "uri": uri})
    print(f"  [{stack}] inserted {len(followers)} followers", flush=True)
    return followers


def fetch_faker_actor() -> dict[str, Any]:
    r = httpx.get(f"{FAKER_BASE_HTTPS}{FAKER_ACTOR_PATH}", timeout=10, verify=False)
    r.raise_for_status()
    return r.json()


def insert_faker_actor(stack: str, db_host: str, faker_actor: dict[str, Any]) -> str:
    """Pre-seed the faker actor as a remote user on the receiver stack
    so inbox processing does not require a fresh actor fetch / verify
    round-trip."""
    print(f"  [{stack}] inserting faker actor...", flush=True)
    fid = aidx_id("fk")
    username = "bench-sender"
    uri = faker_actor["id"]
    inbox = faker_actor["inbox"]
    pubkey_pem = faker_actor["publicKey"]["publicKeyPem"]
    key_id = faker_actor["publicKey"]["id"]

    with psycopg.connect(host=db_host, user=DB_USER, password=DB_PASS, dbname=DB_NAME) as conn:
        conn.autocommit = True
        with conn.cursor() as cur:
            cur.execute(
                """
                INSERT INTO "user" (id, username, "usernameLower", host,
                                    uri, inbox)
                VALUES (%s, %s, %s, %s, %s, %s)
                ON CONFLICT (id) DO UPDATE SET inbox = EXCLUDED.inbox
                """,
                (fid, username, username.lower(), FAKER_HOST, uri, inbox),
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
                cur.execute(
                    """
                    INSERT INTO user_publickey ("userId", "keyId", "keyPem")
                    VALUES (%s, %s, %s)
                    ON CONFLICT ("userId") DO UPDATE
                    SET "keyId" = EXCLUDED."keyId",
                        "keyPem" = EXCLUDED."keyPem"
                    """,
                    (fid, key_id, pubkey_pem),
                )
            elif has_keypair:
                # Misskey TS uses user_keypair with publicKey/privateKey columns
                cur.execute(
                    """
                    INSERT INTO user_keypair ("userId", "publicKey", "privateKey")
                    VALUES (%s, %s, %s)
                    ON CONFLICT ("userId") DO UPDATE
                    SET "publicKey" = EXCLUDED."publicKey"
                    """,
                    (fid, pubkey_pem, ""),
                )
    return fid


def main() -> int:
    print("queue-bench seed starting...", flush=True)

    state: dict[str, Any] = {"stacks": {}, "faker": {}}

    for stack, info in STACKS.items():
        wait_health(info["url"])

    faker_actor = fetch_faker_actor()
    state["faker"] = {
        "actor_uri": faker_actor["id"],
        "inbox": faker_actor["inbox"],
        "publicKeyId": faker_actor["publicKey"]["id"],
    }

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
        followers = insert_blackhole_followers(stack, info["db_host"], user_id, OUTBOUND_FOLLOWERS)
        faker_remote_id = insert_faker_actor(stack, info["db_host"], faker_actor)

        stack_state: dict[str, Any] = {
            "url": url,
            "kind": info["kind"],
            "token": token,
            "user_id": user_id,
            "follower_count": len(followers),
            "faker_remote_user_id": faker_remote_id,
        }
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
