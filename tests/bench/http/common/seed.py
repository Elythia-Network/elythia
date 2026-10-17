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
# Fraction of seed notes that reply to an earlier public / home note (opt-in,
# default 0). Replies make timeline packing resolve the reply target, which a
# reply-free seed never does (#3544).
REPLY_RATIO = float(os.environ.get("SEED_REPLY_RATIO", "0"))
# Visibility weights of seed notes, as "public:80,home:15,followers:5"
# (default: every note public). Followers-only notes make timelines filter by
# the follow graph (#3544).
VISIBILITY = os.environ.get("SEED_VISIBILITY", "public")
# Exit non-zero when a user, follow edge or note could not be created (opt-in,
# default off). A comparison that seeds less data on one side than the other
# would otherwise look like a performance difference (#3544).
STRICT = os.environ.get("SEED_STRICT", "") == "1"
# Role policy rateLimitFactor left on the base role after seeding (opt-in,
# default: the policies are not touched). When set, seeding runs with the
# factor 0 (upstream Misskey skips the per-user limits then) and the given
# factor is written back at the end: "1" keeps the default limits, "0" turns
# them off. Used for upstream Misskey, whose limits are only switched off by
# NODE_ENV (which also logs every SQL query) or by this policy (#3654).
RATE_LIMIT_FACTOR = os.environ.get("SEED_RATE_LIMIT_FACTOR", "")

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


def set_rate_limit_factor(http: httpx.Client, admin_token: str, factor: float) -> None:
    """Write the base role's rateLimitFactor policy.

    update-default-policies replaces the stored policies as a whole; the seed
    starts from an empty database, so there is nothing else to keep.
    """
    api(http, "admin/roles/update-default-policies", {"policies": {"rateLimitFactor": factor}}, admin_token)


def parse_visibility(spec: str) -> list[tuple[str, int]]:
    """Parse SEED_VISIBILITY ("public:80,home:15" or just "public")."""
    weights: list[tuple[str, int]] = []
    for part in spec.split(","):
        name, _, weight = part.strip().partition(":")
        if name not in ("public", "home", "followers"):
            raise ValueError(f"SEED_VISIBILITY: unknown visibility {name!r}")
        weights.append((name, int(weight) if weight else 1))
    if not weights or sum(w for _, w in weights) <= 0:
        raise ValueError(f"SEED_VISIBILITY: no positive weight in {spec!r}")
    return weights


def main() -> None:
    wait_for_health(TARGET_URL)

    http = httpx.Client(base_url=TARGET_URL, timeout=20, verify=False)

    # admin作成
    admin_token = create_admin(http, "benchadmin", "bench1234")
    if not admin_token:
        print("ERROR: failed to get admin token", file=sys.stderr)
        sys.exit(1)

    # 本家は利用者ごとの制限を seed の投稿・フォローにも数える。seed の分が計測の
    # 枠を食わないよう、seed の間は factor 0 (制限なし) にしておく
    rate_limit_factor = float(RATE_LIMIT_FACTOR) if RATE_LIMIT_FACTOR else None
    if rate_limit_factor is not None:
        set_rate_limit_factor(http, admin_token, 0)

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
    #
    # 返信と公開範囲は、既定 (返信 0・全部 public) のときに乱数を引かない。既定の
    # seed で作るデータを #3544 の前と変えないため。乱数は follow / mute と別の
    # 固定 seed の Random を使い、base と head で同じデータにする
    visibilities = parse_visibility(VISIBILITY)
    rng = random.Random(3544)
    # 返信先は、誰からでも見える public / home のノートから選ぶ
    reply_targets: list[str] = []
    replies = 0
    note_ids: list[str] = []
    for idx, token in enumerate(tokens):
        files = user_files[idx]
        for j in range(NUM_NOTES_PER_USER):
            visibility = visibilities[0][0]
            if len(visibilities) > 1:
                visibility = rng.choices([v for v, _ in visibilities], weights=[w for _, w in visibilities])[0]
            body: dict = {
                "text": f"Benchmark seed note {idx}-{j} on {TARGET_NAME}",
                "visibility": visibility,
            }
            # 乱数は返信先の有無に関係なく、1 つのノートにつき同じ回数だけ引く。
            # rng.choice は候補の数で引く回数が変わるので使わない。ノートの作成が
            # 失敗して返信先の数が変わっても、以降の系列がずれないようにする
            if REPLY_RATIO > 0:
                is_reply, pick = rng.random() < REPLY_RATIO, rng.random()
                if is_reply and reply_targets:
                    body["replyId"] = reply_targets[int(pick * len(reply_targets))]
            if files and j % 3 == 0:
                # 1-2 個添付 (file 数に応じて)。決定的に選ぶ。
                pick = files[: 2] if len(files) >= 2 else files[:1]
                body["fileIds"] = pick
            try:
                result = api(http, "notes/create", body, token)
                nid = result.get("createdNote", {}).get("id")
                if nid:
                    note_ids.append(nid)
                    if visibility in ("public", "home"):
                        reply_targets.append(nid)
                    if "replyId" in body:
                        replies += 1
            except Exception as exc:
                print(f"WARN: note create failed: {exc}", file=sys.stderr)

    print(f"Created {len(note_ids)} notes on {TARGET_NAME}")
    if REPLY_RATIO > 0:
        print(f"Of which {replies} are replies (SEED_REPLY_RATIO={REPLY_RATIO})")

    if rate_limit_factor is not None:
        set_rate_limit_factor(http, admin_token, rate_limit_factor)
        print(f"Set rateLimitFactor={rate_limit_factor:g} on {TARGET_NAME}")

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

    if STRICT:
        # 期待値は要求した人数から出す。作れた人数から出すと、欠けた分だけ期待値も下がる
        expected = {
            "users": (len(tokens), NUM_USERS),
            "user ids": (sum(1 for u in user_ids if u), NUM_USERS),
            "follow edges": (edges, NUM_USERS * min(FOLLOWERS_PER_USER, NUM_USERS - 1)),
            "notes": (len(note_ids), NUM_USERS * NUM_NOTES_PER_USER),
        }
        short = [f"{name} {got}/{want}" for name, (got, want) in expected.items() if got < want]
        if short:
            print(f"ERROR: SEED_STRICT: seeded less than requested: {', '.join(short)}", file=sys.stderr)
            sys.exit(1)


if __name__ == "__main__":
    main()
