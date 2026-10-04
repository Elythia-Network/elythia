"""Seed benchmark data for mk-go / Misskey performance comparison.

Creates multiple users and notes on the target instance, then writes
tokens and note IDs to /seed/seed-data.json for k6 to consume.
"""

from __future__ import annotations

import base64
import json
import os
import random
import sys
import time

import httpx

TARGET_URL = os.environ["TARGET_URL"]
TARGET_NAME = os.environ.get("TARGET_NAME", "target")
NUM_USERS = int(os.environ.get("SEED_USERS", "50"))
NUM_NOTES_PER_USER = int(os.environ.get("SEED_NOTES", "50"))
OUTPUT_DIR = os.environ.get("OUTPUT_DIR", "/seed")
# Number of followers each user gets. Default = a complete follow graph
# (NUM_USERS-1) so every note-create fans out to all other users' home
# timelines — without this the bench measures notes-create with zero followers
# (fanout no-op), which is not representative of real load (#1379). Capped at
# 100 so bumping SEED_USERS doesn't blow up seed time on the O(N²) follow loop
# (100 followers/user is already a representative active-instance fan-out); an
# explicit SEED_FOLLOWERS overrides the cap.
FOLLOWERS_PER_USER = int(os.environ.get("SEED_FOLLOWERS", str(min(NUM_USERS - 1, 100))))
# Number of drive files each user uploads (opt-in, default 0). When > 0, a
# fraction of each user's notes attach 1-2 of their files so timeline packing
# exercises the drive-file resolution path (resolveFileOwners / files batch),
# which the text-only default seed never hits. Kept small because each file is
# a real multipart upload.
FILES_PER_USER = int(os.environ.get("SEED_FILES_PER_USER", "0"))
# Number of other users each user mutes (opt-in, default 0). When > 0 the muted
# set is non-empty so authed timelines (home-timeline scenario) exercise the
# mute filter / loadMutedUserIDs path.
MUTES_PER_USER = int(os.environ.get("SEED_MUTES_PER_USER", "0"))

# A minimal valid 1x1 PNG, used for drive uploads when FILES_PER_USER > 0.
# Embedding the bytes keeps the seeder self-contained (no asset file mount).
_PNG_1X1_B64 = (
    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HBgTAAAAC0lEQVR42mNk+M8AAAMCAQDJ"
    "Q2ozAAAAAElFTkSuQmCC"
)


def wait_for_health(url: str, timeout: int = 180) -> None:
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            resp = httpx.post(f"{url}/api/ping", json={}, timeout=5, verify=False)
            if resp.status_code == 200:
                print(f"{url} is ready")
                return
        except Exception:
            pass
        time.sleep(2)
    raise TimeoutError(f"{url} did not become healthy within {timeout}s")


def api(http: httpx.Client, endpoint: str, body: dict | None = None, token: str | None = None) -> dict:
    payload = dict(body or {})
    if token:
        payload["i"] = token
    resp = http.post(f"/api/{endpoint}", json=payload)
    if resp.status_code >= 400:
        raise RuntimeError(f"POST /api/{endpoint} failed ({resp.status_code}): {resp.text[:300]}")
    return resp.json() if resp.content else {}


def create_admin(http: httpx.Client, username: str, password: str) -> str:
    """Create the first (admin) user, or sign in if already exists."""
    resp = http.post("/api/admin/accounts/create", json={"username": username, "password": password})
    if resp.status_code == 200:
        return resp.json().get("token", "")
    # 既に初期化済み
    if resp.status_code in (400, 403, 409):
        resp2 = http.post("/api/signin-flow", json={"username": username, "password": password})
        resp2.raise_for_status()
        data = resp2.json()
        return data.get("i") or data.get("token") or ""
    resp.raise_for_status()
    return ""


def resolve_user_id(http: httpx.Client, token: str) -> str:
    """Resolve a token's own user ID via the `i` endpoint."""
    try:
        return api(http, "i", {}, token).get("id", "")
    except Exception as exc:
        print(f"WARN: resolve user id failed: {exc}", file=sys.stderr)
        return ""


def seed_following(http: httpx.Client, tokens: list[str], user_ids: list[str]) -> int:
    """Build a follow graph so notes-create actually fans out to followers.

    Each user follows up to FOLLOWERS_PER_USER of the *other* users, so every
    user ends up with followers and every note-create pushes to their home
    timelines. With the default (complete graph) each user has NUM_USERS-1
    followers. Returns the number of follow edges created.
    """
    # seed.py は mk-go 用 / TS 用に別プロセスで実行されるため、固定 seed で
    # follow 先を決定的に選ぶ。未シードだと capped 時 (FOLLOWERS_PER_USER <
    # NUM_USERS-1) に両スタックで異なる graph になり比較が unfair になる。固定
    # seed なら user 数が同じ限り edge 構造が一致し、run 間でも再現性を持つ。
    random.seed(1379)
    edges = 0
    for i, token in enumerate(tokens):
        if not token:
            continue
        # i 番目の user が他 user を follow する。complete graph では全員。
        # FOLLOWERS_PER_USER で絞る場合は random.sample で分散し、特定 user に
        # follower が偏らない (= 先頭 K 固定だと低 index user に集中する) ように
        # する。
        candidates = [user_ids[j] for j in range(len(user_ids)) if j != i and user_ids[j]]
        targets = random.sample(candidates, min(FOLLOWERS_PER_USER, len(candidates)))
        for target_id in targets:
            try:
                api(http, "following/create", {"userId": target_id}, token)
                edges += 1
            except Exception as exc:
                # 既に follow 済み等は best-effort で無視。
                print(f"WARN: follow {i}->{target_id} failed: {exc}", file=sys.stderr)
    return edges


def upload_files(http: httpx.Client, token: str, count: int) -> list[str]:
    """Upload `count` tiny PNGs to the target's drive via multipart, returning
    the created file IDs. Best-effort: failures are logged and skipped so a
    drive-disabled instance does not abort the whole seed.
    """
    png = base64.b64decode(_PNG_1X1_B64)
    ids: list[str] = []
    for k in range(count):
        try:
            resp = http.post(
                "/api/drive/files/create",
                data={"i": token},
                files={"file": (f"bench-{k}.png", png, "image/png")},
            )
            if resp.status_code >= 400:
                print(f"WARN: drive upload failed ({resp.status_code}): {resp.text[:200]}", file=sys.stderr)
                continue
            fid = resp.json().get("id")
            if fid:
                ids.append(fid)
        except Exception as exc:
            print(f"WARN: drive upload failed: {exc}", file=sys.stderr)
    return ids


def seed_mutes(http: httpx.Client, tokens: list[str], user_ids: list[str]) -> int:
    """Each user mutes up to MUTES_PER_USER of the *other* users so authed
    timelines exercise the mute filter. Deterministic via the shared seed.
    Returns the number of mute edges created.
    """
    if MUTES_PER_USER <= 0:
        return 0
    random.seed(13790)
    edges = 0
    for i, token in enumerate(tokens):
        if not token:
            continue
        candidates = [user_ids[j] for j in range(len(user_ids)) if j != i and user_ids[j]]
        targets = random.sample(candidates, min(MUTES_PER_USER, len(candidates)))
        for target_id in targets:
            try:
                api(http, "mute/create", {"userId": target_id}, token)
                edges += 1
            except Exception as exc:
                print(f"WARN: mute {i}->{target_id} failed: {exc}", file=sys.stderr)
    return edges


def main() -> None:
    wait_for_health(TARGET_URL)

    http = httpx.Client(base_url=TARGET_URL, timeout=20, verify=False)

    # admin作成
    admin_token = create_admin(http, "benchadmin", "bench1234")
    if not admin_token:
        print("ERROR: failed to get admin token", file=sys.stderr)
        sys.exit(1)

    tokens = [admin_token]
    usernames = ["benchadmin"]

    # 追加ユーザー作成
    for i in range(1, NUM_USERS):
        username = f"benchuser{i}"
        try:
            resp = http.post(
                "/api/admin/accounts/create",
                json={"username": username, "password": "bench1234", "i": admin_token},
            )
            if resp.status_code == 200:
                token = resp.json().get("token", "")
                if token:
                    tokens.append(token)
                    usernames.append(username)
                    continue
            # ユーザーが既に存在する場合はsignin
            resp2 = http.post("/api/signin-flow", json={"username": username, "password": "bench1234"})
            if resp2.status_code == 200:
                data = resp2.json()
                token = data.get("i") or data.get("token") or ""
                if token:
                    tokens.append(token)
                    usernames.append(username)
        except Exception as exc:
            print(f"WARN: failed to create {username}: {exc}", file=sys.stderr)

    print(f"Created {len(tokens)} users on {TARGET_NAME}")

    # 各 token の user ID を解決し、follower graph を張る。これがないと
    # notes-create が 0 follower で fan-out しない非代表な計測になる (#1379)。
    user_ids = [resolve_user_id(http, t) for t in tokens]
    edges = seed_following(http, tokens, user_ids)
    print(f"Created {edges} follow edges on {TARGET_NAME} "
          f"(~{FOLLOWERS_PER_USER} followers/user)")

    # mute graph (opt-in)。authed timeline で mute filter を踏ませる。
    mute_edges = seed_mutes(http, tokens, user_ids)
    if MUTES_PER_USER > 0:
        print(f"Created {mute_edges} mute edges on {TARGET_NAME} "
              f"(~{MUTES_PER_USER} mutes/user)")
        if mute_edges == 0:
            print(f"WARN: SEED_MUTES_PER_USER={MUTES_PER_USER} requested but 0 mute "
                  f"edges created on {TARGET_NAME} — mute path will not be exercised.",
                  file=sys.stderr)

    # drive files (opt-in)。各 user 分を先にまとめて上げて token idx で引けるように。
    user_files: list[list[str]] = [[] for _ in tokens]
    total_files = 0
    if FILES_PER_USER > 0:
        for idx, token in enumerate(tokens):
            if not token:
                continue
            user_files[idx] = upload_files(http, token, FILES_PER_USER)
            total_files += len(user_files[idx])
        print(f"Uploaded {total_files} drive files on {TARGET_NAME} "
              f"(~{FILES_PER_USER}/user)")
        # silent failure を検知可能にする: drive 未設定等で 0 件だと、片 backend
        # だけ file 無しになり cross-backend 比較が非対称になる。要求したのに 0 /
        # 期待数未満なら loud に警告して operator が両 stack の fileCount を突き合
        # わせられるようにする (= 黙って skew した比較を出さない)。
        expected = FILES_PER_USER * sum(1 for t in tokens if t)
        if total_files == 0:
            print(f"WARN: SEED_FILES_PER_USER={FILES_PER_USER} requested but 0 files "
                  f"uploaded on {TARGET_NAME} (drive disabled?). Cross-backend "
                  f"comparison will be ASYMMETRIC — compare both stacks' fileCount.",
                  file=sys.stderr)
        elif total_files < expected:
            print(f"WARN: only {total_files}/{expected} files uploaded on "
                  f"{TARGET_NAME} (some uploads failed) — verify both stacks match.",
                  file=sys.stderr)

    # ノート投入。FILES_PER_USER > 0 のとき一部の note に自分の file を添付して
    # timeline packing の drive-file 解決経路を踏ませる (決定的: 3 note に 1 回)。
    note_ids: list[str] = []
    for idx, token in enumerate(tokens):
        files = user_files[idx]
        for j in range(NUM_NOTES_PER_USER):
            body: dict = {
                "text": f"Benchmark seed note {idx}-{j} on {TARGET_NAME}",
                "visibility": "public",
            }
            if files and j % 3 == 0:
                # 1-2 個添付 (file 数に応じて)。決定的に選ぶ。
                pick = files[: 2] if len(files) >= 2 else files[:1]
                body["fileIds"] = pick
            try:
                result = api(http, "notes/create", body, token)
                nid = result.get("createdNote", {}).get("id")
                if nid:
                    note_ids.append(nid)
            except Exception as exc:
                print(f"WARN: note create failed: {exc}", file=sys.stderr)

    print(f"Created {len(note_ids)} notes on {TARGET_NAME}")

    # 結果出力
    os.makedirs(OUTPUT_DIR, exist_ok=True)
    output = {
        "tokens": tokens,
        "noteIds": note_ids,
        "adminToken": admin_token,
        "username": "benchadmin",
        "usernames": usernames,
        "userIds": user_ids,
        "followEdges": edges,
        "muteEdges": mute_edges,
        "fileCount": total_files,
    }
    path = os.path.join(OUTPUT_DIR, "seed-data.json")
    with open(path, "w") as f:
        json.dump(output, f, indent=2)
    print(f"Wrote seed data to {path}")


if __name__ == "__main__":
    main()
