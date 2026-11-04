"""Generate a benchmark dataset shaped like a medium-sized server (#4016).

The output loads into both upstream Misskey and Elythia, because Elythia keeps
upstream's database schema (a TS database starts on Elythia as is). It is a
directory with:

  <table>.tsv      rows for PostgreSQL COPY (text format), one file per table,
                   with the columns listed in COLUMNS (all of them exist in
                   both schemas)
  redis.resp       commands for `redis-cli --pipe`: notification streams and
                   fanout timelines, keyed like both servers key them
  seed-data.json   what the k6 scenarios read (tokens, note ids, ...)
  stats.json       row counts, for the log and for checks

The data is deterministic for a given profile, seed and --now. Run it with the
standard library only:

  python3 gen.py --profile medium --out DIR --host web \\
      --public-key pub.pem --private-key priv.pem [--now EPOCH_MS]
"""

from __future__ import annotations

import argparse
import bisect
import json
import math
import os
import random
import sys
import time
from dataclasses import dataclass

# The profiles. medium is the size agreed in #4016; tiny keeps the same shape
# at a size the smoke run (and the host that runs production) can afford.
PROFILES = {
    "medium": dict(
        local_users=3000, active_users=600, concurrent_users=150,
        local_notes=300_000, instances=2000, remote_users=30_000, remote_notes=1_000_000,
        celebrities=30, celebrity_follow_p=0.4, following_median=50,
        days=30,
    ),
    "tiny": dict(
        local_users=60, active_users=30, concurrent_users=20,
        local_notes=3000, instances=20, remote_users=600, remote_notes=6000,
        celebrities=2, celebrity_follow_p=0.4, following_median=10,
        days=30,
    ),
}

# Shape of the notes (#4016): share of replies / renotes among all notes,
# share of the other notes with attachments, mean reactions on a local note.
REPLY_RATIO = 0.30
RENOTE_RATIO = 0.15
FILE_RATIO = 0.15
REACTIONS_PER_LOCAL_NOTE = 3.0
# リモートのノートに付くリアクションは、このサーバーの利用者が付けた分だけが
# 届く (他のサーバーの利用者の Like は、ノートのサーバーにしか送られない)
REACTIONS_PER_REMOTE_NOTE = 0.3
REMOTE_FOLLOW_RATIO = 0.4
LOCAL_VISIBILITY = (("public", 80), ("home", 15), ("followers", 5))
REMOTE_VISIBILITY = (("public", 85), ("home", 10), ("followers", 5))
# Unicode reactions, written as escapes so the source has no emoji
REACTIONS = ("\U0001F44D", "\u2764", "\U0001F606", "\U0001F914", "\U0001F62E", "\U0001F389", "\U0001F64F")
REACTION_WEIGHTS = (40, 20, 15, 8, 7, 6, 4)

# Upstream limits the fanout timelines and the notification streams to these
# (Meta defaults and config.perUserNotificationsMaxCount in 2026.10.1).
HOME_TL_MAX = 300
LOCAL_USER_TL_MAX = 300
REMOTE_USER_TL_MAX = 100
LOCAL_TL_MAX = 1000
LOCAL_TL_FILES_MAX = 500
LOCAL_TL_REPLIES_MAX = 300
LOCAL_TL_REPLY_TO_MAX = 30
NOTIFICATIONS_MAX = 500
PAIR_CACHE_MAX = 16
# 本家は投稿のたびに 10% の確率で、最終アクティブが 50 日より前のフォロワーを休眠させ
# (NoteCreateService.checkHibernation)、休眠したフォロワーにはタイムラインを積まない。
# 長く動いている本家では、その人たちはもう休眠しているので、最初から休眠させておく。
# そうしないと、計測の間に本家だけが休眠の UPDATE を流す (Elythia は休眠を読むだけ)
HIBERNATION_MS = 50 * 86400_000

TIME2000 = 946684800000
B36 = "0123456789abcdefghijklmnopqrstuvwxyz"


def b36(n: int, width: int) -> str:
    out = []
    for _ in range(width):
        n, r = divmod(n, 36)
        out.append(B36[r])
    return "".join(reversed(out))


class IdGen:
    """aidx ids (time 8 + node 4 + counter 4), one counter per table.

    The node part tells the tables apart, so ids stay unique within a table
    as long as it has fewer than 36^4 rows.
    """

    def __init__(self, node: str):
        assert len(node) == 4
        self.node = node
        self.counter = 0

    def gen(self, t_ms: int) -> str:
        self.counter += 1
        if self.counter >= 36 ** 4:
            raise RuntimeError(f"id counter of node {self.node} wrapped")
        return b36(max(t_ms - TIME2000, 0), 8) + self.node + b36(self.counter, 4)


def stream_id(aidx: str) -> str:
    """The notification stream id upstream derives from the notification id
    (NotificationService.toXListId): <ms>-<the last 8 characters as base 36>.
    """
    return f"{int(aidx[:8], 36) + TIME2000}-{int(aidx[8:16], 36)}"


def iso(t_ms: int) -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime(t_ms / 1000)) + f".{t_ms % 1000:03d}Z"


def ts(t_ms: int) -> str:
    return iso(t_ms).replace("T", " ").replace("Z", "+00")


# ── COPY text format ────────────────────────────────────────────────
NULL = None


def esc(v) -> str:
    if v is None:
        return "\\N"
    if v is True:
        return "t"
    if v is False:
        return "f"
    if isinstance(v, int):
        return str(v)
    s = str(v)
    return s.replace("\\", "\\\\").replace("\t", "\\t").replace("\n", "\\n").replace("\r", "\\r")


def pg_array(items) -> str:
    # 値は英数字と記号だけ (id、MIME の型、"<id>/<reaction>") なので、要素は引用で囲むだけでよい
    return "{" + ",".join('"' + str(i).replace("\\", "\\\\").replace('"', '\\"') + '"' for i in items) + "}"


COLUMNS = {
    "instance": ["id", "firstRetrievedAt", "host", "usersCount", "notesCount", "followingCount", "followersCount",
                 "softwareName", "softwareVersion", "name"],
    "user": ["id", "username", "usernameLower", "name", "host", "inbox", "sharedInbox", "uri", "followersUri", "token",
             "followersCount", "followingCount", "notesCount", "lastActiveDate", "lastFetchedAt", "isHibernated"],
    "user_profile": ["userId", "userHost"],
    "user_keypair": ["userId", "publicKey", "privateKey"],
    "user_publickey": ["userId", "keyId", "keyPem"],
    "following": ["id", "followeeId", "followerId", "followerHost", "followeeHost", "followeeInbox", "followeeSharedInbox",
                  "isFollowerHibernated"],
    "drive_file": ["id", "userId", "userHost", "md5", "name", "type", "size", "storedInternal", "url", "thumbnailUrl",
                   "isLink", "uri"],
    "note": ["id", "replyId", "renoteId", "text", "userId", "renoteCount", "repliesCount", "reactions", "visibility",
             "uri", "fileIds", "attachedFileTypes", "userHost", "replyUserId", "replyUserHost", "renoteUserId",
             "renoteUserHost", "threadId", "reactionAndUserPairCache"],
    "note_reaction": ["id", "userId", "noteId", "reaction"],
}
# 読み込む順。外部キーの参照先を先に入れる
TABLE_ORDER = ["instance", "user", "user_profile", "user_keypair", "user_publickey", "drive_file", "note",
               "note_reaction", "following"]


class TableWriter:
    def __init__(self, out: str, table: str):
        self.table = table
        self.f = open(os.path.join(out, f"{table}.tsv"), "w", encoding="utf-8", newline="\n")
        self.rows = 0

    def row(self, *values):
        assert len(values) == len(COLUMNS[self.table]), self.table
        self.f.write("\t".join(esc(v) for v in values))
        self.f.write("\n")
        self.rows += 1

    def close(self):
        self.f.close()


# ── Redis protocol ──────────────────────────────────────────────────
class Resp:
    def __init__(self, path: str):
        self.f = open(path, "wb")
        self.commands = 0

    def cmd(self, *args: str):
        parts = [f"*{len(args)}\r\n".encode()]
        for a in args:
            b = a.encode("utf-8")
            parts.append(f"${len(b)}\r\n".encode())
            parts.append(b)
            parts.append(b"\r\n")
        self.f.write(b"".join(parts))
        self.commands += 1

    def close(self):
        self.f.close()


# ── generation ──────────────────────────────────────────────────────
@dataclass
class Note:
    id: str
    t: int
    user: int          # index into users
    visibility: str
    reply: int         # index into notes, -1 when none
    renote: int        # index into notes, -1 when none
    files: tuple       # drive_file ids
    thread: str | None


def off_hibernation_boundary(now: int, last_active: int) -> int:
    """Move a last-active time out of the day on either side of the 50-day
    hibernation boundary, to the hibernated side.
    """
    # 計測の間に境目を越える人が出ると、本家だけがその人を休眠させうる
    if HIBERNATION_MS - 86400_000 <= now - last_active <= HIBERNATION_MS + 86400_000:
        return last_active - 3 * 86400_000
    return last_active


def zipf_weights(n: int, s: float, offset: float = 1.0) -> list[float]:
    return [1.0 / ((i + offset) ** s) for i in range(n)]


def cumulative(weights: list[float]) -> list[float]:
    out, acc = [], 0.0
    for w in weights:
        acc += w
        out.append(acc)
    return out


def pick(rng: random.Random, cum: list[float]) -> int:
    return bisect.bisect_left(cum, rng.random() * cum[-1])


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--profile", choices=sorted(PROFILES), required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--host", default="web", help="the server's host; also the Redis key prefix")
    ap.add_argument("--public-key", required=True)
    ap.add_argument("--private-key", required=True)
    ap.add_argument("--now", type=int, default=0, help="epoch ms the data ends at (default: now)")
    ap.add_argument("--seed", type=int, default=4016)
    args = ap.parse_args()
    p = PROFILES[args.profile]
    os.makedirs(args.out, exist_ok=True)
    rng = random.Random(args.seed)
    now = args.now or int(time.time() * 1000)
    start = now - p["days"] * 86400_000
    host = args.host
    pub = open(args.public_key).read().strip()
    priv = open(args.private_key).read().strip()
    started = time.time()

    ids = {name: IdGen(node) for name, node in
           [("user", "u000"), ("note", "n000"), ("follow", "f000"), ("reaction", "r000"), ("file", "d000"),
            ("instance", "i000"), ("notif", "x000")]}

    # ── instances and users ─────────────────────────────────────────
    n_local, n_remote, n_inst = p["local_users"], p["remote_users"], p["instances"]
    inst_hosts = [f"s{i}.remote.example" for i in range(n_inst)]
    # リモートの利用者は、少数の大きなサーバーに偏らせる
    inst_cum = cumulative(zipf_weights(n_inst, 1.1))
    users_host: list[str | None] = [None] * n_local
    for _ in range(n_remote):
        users_host.append(inst_hosts[pick(rng, inst_cum)])
    n_users = n_local + n_remote
    # 利用者の作成時刻は、データの期間より前 (1 年前まで) に散らす
    user_ids = [ids["user"].gen(start - rng.randint(1, 365) * 86400_000 + rng.randint(0, 86399_999)) for _ in range(n_users)]
    usernames = [f"m{i}" if i < n_local else f"r{i - n_local}" for i in range(n_users)]
    # ローカルの利用者の並び: 先頭から「同時に開いている」→「アクティブ」→ それ以外
    n_active, n_concurrent = p["active_users"], p["concurrent_users"]
    tokens = [b36(rng.getrandbits(80), 16) for _ in range(n_local)]
    last_active = [now - (rng.randint(0, 3600_000) if u < n_concurrent else
                          rng.randint(0, 29 * 86400_000) if u < n_active else
                          rng.randint(31 * 86400_000, 365 * 86400_000)) for u in range(n_local)]
    last_active = [off_hibernation_boundary(now, t) for t in last_active]
    hibernated = [now - t > HIBERNATION_MS for t in last_active]

    # ── follow graph ────────────────────────────────────────────────
    # ローカルの利用者のうち先頭 celebrities 人を人気者にし、各利用者が確率 p で
    # フォローする (上位 1% が 1,000 人以上のフォロワーを持つ)。残りのフォローは、
    # ローカルとリモートそれぞれの人気の偏り (Zipf) から選ぶ
    n_celeb = p["celebrities"]
    local_cum = cumulative(zipf_weights(n_local - n_celeb, 0.9, 5))
    remote_cum = cumulative(zipf_weights(n_remote, 0.9, 5))
    following: list[set[int]] = [set() for _ in range(n_local)]
    sigma = 1.0
    for u in range(n_local):
        want = int(min(max(round(math.exp(math.log(p["following_median"]) + sigma * rng.gauss(0, 1))), 1),
                       n_local + n_remote - 1, 2000))
        fl = following[u]
        for c in range(n_celeb):
            if c != u and rng.random() < p["celebrity_follow_p"]:
                fl.add(c)
        want_remote = int(round(want * REMOTE_FOLLOW_RATIO))
        want_local = max(want - want_remote, len(fl))
        guard = 0
        while len(fl) < want_local and guard < want_local * 20:
            guard += 1
            v = n_celeb + pick(rng, local_cum)
            if v != u:
                fl.add(v)
        guard = 0
        target = len(fl) + want_remote
        while len(fl) < target and guard < want_remote * 20:
            guard += 1
            fl.add(n_local + pick(rng, remote_cum))
    # followers of each user, local followers only (リモートからローカルへのフォローは作らない。
    # 投稿のたびに存在しない宛先へ配送が積まれ、本家と Elythia で再試行の振る舞いが違うため)
    local_followers: list[list[int]] = [[] for _ in range(n_users)]
    for u in range(n_local):
        for v in following[u]:
            local_followers[v].append(u)

    # ── notes ───────────────────────────────────────────────────────
    # 投稿の 95% はアクティブな利用者が書き、その中でも偏らせる
    active_cum = cumulative(zipf_weights(n_active, 0.8, 3))
    notes: list[Note] = []
    n_local_notes, n_remote_notes = p["local_notes"], p["remote_notes"]
    times = sorted([rng.randint(start, now - 60_000) for _ in range(n_local_notes + n_remote_notes)])
    is_local_slot = [True] * n_local_notes + [False] * n_remote_notes
    rng.shuffle(is_local_slot)
    vis_l = [v for v, _ in LOCAL_VISIBILITY]
    vis_lw = [w for _, w in LOCAL_VISIBILITY]
    vis_r = [v for v, _ in REMOTE_VISIBILITY]
    vis_rw = [w for _, w in REMOTE_VISIBILITY]
    files_rows = []  # (id, user, t)
    # 返信先とリノート先は、直近の公開のノートから選ぶ (古いノートほど選ばれにくい)
    recent_public: list[int] = []
    notes_by_user: list[list[int]] = [[] for _ in range(n_users)]
    for k, t in enumerate(times):
        if is_local_slot[k]:
            u = pick(rng, active_cum) if rng.random() < 0.95 else rng.randrange(n_local)
            vis = rng.choices(vis_l, vis_lw)[0]
        else:
            u = n_local + pick(rng, remote_cum)
            vis = rng.choices(vis_r, vis_rw)[0]
        r = rng.random()
        reply = renote = -1
        if recent_public and r < REPLY_RATIO:
            reply = recent_public[-1 - min(int(rng.expovariate(1 / 200)), len(recent_public) - 1)]
        elif recent_public and r < REPLY_RATIO + RENOTE_RATIO:
            renote = recent_public[-1 - min(int(rng.expovariate(1 / 200)), len(recent_public) - 1)]
            if vis == "followers":
                vis = "home"
        files: tuple = ()
        if renote < 0 and rng.random() < FILE_RATIO:
            nf = 1 if rng.random() < 0.7 else 2
            fl = []
            for _ in range(nf):
                fid = ids["file"].gen(t - 1000)
                files_rows.append((fid, u, t))
                fl.append(fid)
            files = tuple(fl)
        thread = None
        if reply >= 0:
            parent = notes[reply]
            thread = parent.thread or parent.id
        nid = ids["note"].gen(t)
        notes.append(Note(nid, t, u, vis, reply, renote, files, thread))
        notes_by_user[u].append(k)
        if vis == "public" and renote < 0:
            recent_public.append(k)
            if len(recent_public) > 5000:
                del recent_public[:1000]

    n_notes = len(notes)
    replies_count = [0] * n_notes
    renote_count = [0] * n_notes
    for k, n in enumerate(notes):
        if n.reply >= 0:
            replies_count[n.reply] += 1
        # 本家は自分のノートのリノートを数えない
        if n.renote >= 0 and notes[n.renote].user != n.user:
            renote_count[n.renote] += 1

    # ── reactions ───────────────────────────────────────────────────
    # k6 の reactions-create は「同時に開いている」利用者の偶数の位置のノートを使う
    # (seed-data.json の noteIds)。そのノートには、同時に開いている利用者からの
    # リアクションを作らない (同じ組を 2 回作れないため)
    k6_notes = []
    for u in range(n_concurrent):
        own = [k for k in notes_by_user[u] if notes[k].renote < 0 and notes[k].visibility != "followers"]
        k6_notes.extend(own[-20:])
    k6_note_set = set(k6_notes)
    react_cum = cumulative(list(REACTION_WEIGHTS))
    reactions_rows = []  # (id, user, note, reaction)
    note_reactions: list[dict | None] = [None] * n_notes
    note_pairs: list[list[str] | None] = [None] * n_notes
    local_reactor_cum = cumulative(zipf_weights(n_local, 0.7, 3))
    notif_events: list[list[tuple]] = [[] for _ in range(n_local)]  # (t, type, notifier, noteId, extra)
    for k, n in enumerate(notes):
        if n.renote >= 0:
            continue
        lam = REACTIONS_PER_LOCAL_NOTE if n.user < n_local else REACTIONS_PER_REMOTE_NOTE
        m = 0
        # Poisson
        L, pp = math.exp(-lam), 1.0
        while True:
            pp *= rng.random()
            if pp <= L:
                break
            m += 1
        if m == 0:
            continue
        reactors = set()
        # フォロワー限定のノートは、フォロワー (このサーバーではローカルの人だけ) にしか見えない
        fol = local_followers[n.user] if n.visibility == "followers" else None
        if fol is not None and not fol:
            continue
        for _ in range(m):
            if fol is not None:
                v = fol[rng.randrange(len(fol))]
                if k in k6_note_set and v < n_concurrent:
                    continue
            elif n.user < n_local and rng.random() < 0.3:
                v = n_local + pick(rng, remote_cum)
            else:
                v = pick(rng, local_reactor_cum)
                if k in k6_note_set and v < n_concurrent:
                    v = n_concurrent + rng.randrange(n_local - n_concurrent)
            if v != n.user:
                reactors.add(v)
        counts: dict[str, int] = {}
        pairs: list[str] = []
        for v in sorted(reactors):
            reaction = REACTIONS[pick(rng, react_cum)]
            t = min(n.t + int(rng.expovariate(1 / 3_600_000)), now - 1000)
            rid = ids["reaction"].gen(t)
            reactions_rows.append((rid, v, k, reaction))
            counts[reaction] = counts.get(reaction, 0) + 1
            if len(pairs) < PAIR_CACHE_MAX:
                pairs.append(f"{user_ids[v]}/{reaction}")
            if n.user < n_local:
                notif_events[n.user].append((t, "reaction", v, n.id, reaction))
        note_reactions[k] = counts
        note_pairs[k] = pairs

    # replies / renotes / follows notify local users too
    for k, n in enumerate(notes):
        if n.reply >= 0:
            target = notes[n.reply].user
            if target < n_local and target != n.user:
                notif_events[target].append((n.t, "reply", n.user, n.id, None))
        if n.renote >= 0:
            target = notes[n.renote].user
            if target < n_local and target != n.user:
                notif_events[target].append((n.t, "renote", n.user, n.id, notes[n.renote].id))
    follow_rows = []
    for u in range(n_local):
        for v in sorted(following[u]):
            t = rng.randint(start - 180 * 86400_000, now - 3_600_000)
            follow_rows.append((ids["follow"].gen(t), v, u))
            if v < n_local:
                notif_events[v].append((t, "follow", u, None, None))

    # ── write PostgreSQL rows ───────────────────────────────────────
    w = {tb: TableWriter(args.out, tb) for tb in TABLE_ORDER}
    inst_users = [0] * n_inst
    inst_notes = [0] * n_inst
    host_index = {h: i for i, h in enumerate(inst_hosts)}
    for u in range(n_local, n_users):
        inst_users[host_index[users_host[u]]] += 1
    inst_followers = [0] * n_inst
    for n in notes:
        if n.user >= n_local:
            inst_notes[host_index[users_host[n.user]]] += 1
    for v in range(n_local, n_users):
        inst_followers[host_index[users_host[v]]] += len(local_followers[v])
    for i, h in enumerate(inst_hosts):
        t = start - rng.randint(30, 700) * 86400_000
        w["instance"].row(ids["instance"].gen(t), ts(t), h, inst_users[i], inst_notes[i], 0, inst_followers[i],
                          "misskey" if i % 3 else "mastodon", "2026.10.1" if i % 3 else "4.4.0", f"Server {i}")

    followers_count = [len(x) for x in local_followers]
    following_count = [len(following[u]) if u < n_local else 0 for u in range(n_users)]
    notes_count = [len(x) for x in notes_by_user]
    for u in range(n_users):
        h = users_host[u]
        if h is None:
            w["user"].row(user_ids[u], usernames[u], usernames[u].lower(), f"Local {u}", None, None, None, None, None,
                          tokens[u], followers_count[u], following_count[u], notes_count[u], ts(last_active[u]), None,
                          hibernated[u])
        else:
            base = f"https://{h}/users/{usernames[u]}"
            w["user"].row(user_ids[u], usernames[u], usernames[u].lower(), f"Remote {u}", h, base + "/inbox",
                          f"https://{h}/inbox", base, base + "/followers", None, followers_count[u], 0, notes_count[u],
                          None, ts(now - rng.randint(0, 7 * 86400_000)), False)
        w["user_profile"].row(user_ids[u], h)
        if h is None:
            w["user_keypair"].row(user_ids[u], pub, priv)
        else:
            w["user_publickey"].row(user_ids[u], f"https://{h}/users/{usernames[u]}#main-key", pub)

    for fid, u, t in files_rows:
        h = users_host[u]
        md5 = b36(rng.getrandbits(128), 25)[:32].ljust(32, "0")
        if h is None:
            w["drive_file"].row(fid, user_ids[u], None, md5, f"{fid}.png", "image/png", 48_213, True,
                                f"http://{host}/files/{fid}", f"http://{host}/files/thumbnail-{fid}", False, None)
        else:
            url = f"https://{h}/files/{fid}.png"
            w["drive_file"].row(fid, user_ids[u], h, md5, f"{fid}.png", "image/png", 48_213, False, url, None, True, url)

    for k, n in enumerate(notes):
        h = users_host[n.user]
        rep = notes[n.reply] if n.reply >= 0 else None
        ren = notes[n.renote] if n.renote >= 0 else None
        text = None if ren else f"Benchmark note {k} " + "lorem ipsum " * rng.randint(1, 12)
        uri = None if h is None else f"https://{h}/notes/{n.id}"
        w["note"].row(
            n.id, rep.id if rep else None, ren.id if ren else None, text, user_ids[n.user],
            min(renote_count[k], 32767), min(replies_count[k], 32767),
            json.dumps(note_reactions[k] or {}, ensure_ascii=False, separators=(",", ":")),
            n.visibility, uri, pg_array(n.files), pg_array(["image/png"] * len(n.files)), h,
            user_ids[rep.user] if rep else None, users_host[rep.user] if rep else None,
            user_ids[ren.user] if ren else None, users_host[ren.user] if ren else None,
            n.thread, pg_array(note_pairs[k] or []))
    for rid, v, k, reaction in reactions_rows:
        w["note_reaction"].row(rid, user_ids[v], notes[k].id, reaction)
    for fid, followee, follower in follow_rows:
        h = users_host[followee]
        w["following"].row(fid, user_ids[followee], user_ids[follower], None, h,
                           None if h is None else f"https://{h}/users/{usernames[followee]}/inbox",
                           None if h is None else f"https://{h}/inbox", hibernated[follower])
    for tw in w.values():
        tw.close()

    # ── Redis: fanout timelines (upstream NoteCreateService.pushToTl) ─
    resp = Resp(os.path.join(args.out, "redis.resp"))
    prefix = host + ":"
    lists: dict[str, list[str]] = {}
    caps: dict[str, int] = {}

    def push(name: str, nid: str, cap: int):
        items = lists.get(name)
        if items is None:
            items = lists[name] = []
            caps[name] = cap
        items.append(nid)
        # 上限の 2 倍に達したら古い方を落とす (全部を持つと medium で 3GB を超える)
        if len(items) >= cap * 2:
            del items[:len(items) - cap]

    def is_reply(n: Note, viewer: int | None) -> bool:
        if n.reply < 0:
            return False
        ru = notes[n.reply].user
        return ru != n.user and ru != viewer

    for n in notes:
        uid = user_ids[n.user]
        local = users_host[n.user] is None
        user_cap = LOCAL_USER_TL_MAX if local else REMOTE_USER_TL_MAX
        for f in local_followers[n.user]:
            # 休眠したフォロワーには積まない。フォローの withReplies は既定の false
            if hibernated[f] or is_reply(n, f):
                continue
            push(f"homeTimeline:{user_ids[f]}", n.id, HOME_TL_MAX)
            if n.files:
                push(f"homeTimelineWithFiles:{user_ids[f]}", n.id, HOME_TL_MAX // 2)
        if local:
            push(f"homeTimeline:{uid}", n.id, HOME_TL_MAX)
            if n.files:
                push(f"homeTimelineWithFiles:{uid}", n.id, HOME_TL_MAX // 2)
        if is_reply(n, None):
            push(f"userTimelineWithReplies:{uid}", n.id, user_cap)
            if n.visibility == "public" and local:
                push("localTimelineWithReplies", n.id, LOCAL_TL_REPLIES_MAX)
                rep_user = notes[n.reply].user
                if users_host[rep_user] is None:
                    push(f"localTimelineWithReplyTo:{user_ids[rep_user]}", n.id, LOCAL_TL_REPLY_TO_MAX)
        else:
            push(f"userTimeline:{uid}", n.id, user_cap)
            if n.files:
                push(f"userTimelineWithFiles:{uid}", n.id, user_cap // 2)
            if n.visibility == "public" and local:
                push("localTimeline", n.id, LOCAL_TL_MAX)
                if n.files:
                    push("localTimelineWithFiles", n.id, LOCAL_TL_FILES_MAX)
    list_entries = 0
    for name, items in lists.items():
        # LPUSH で積んだ形 (新しいものが先頭) にする。上限を超えた古いものは ltrim で消えている
        keep = items[-caps[name]:]
        keep.reverse()
        for i in range(0, len(keep), 1000):
            resp.cmd("RPUSH", prefix + "list:" + name, *keep[i:i + 1000])
        list_entries += len(keep)

    # ── Redis: notification streams (active local users) ─────────────
    notif_entries = 0
    notif_keys = 0
    for u in range(n_active):
        ev = sorted(notif_events[u], key=lambda e: e[0])[-NOTIFICATIONS_MAX:]
        key = prefix + f"notificationTimeline:{user_ids[u]}"
        if ev:
            notif_keys += 1
        for t, typ, notifier, note_id, extra in ev:
            nid = ids["notif"].gen(t)
            body = {"id": nid, "createdAt": iso(t), "type": typ, "notifierId": user_ids[notifier]}
            if note_id:
                body["noteId"] = note_id
            if typ == "reaction":
                body["reaction"] = extra
            if typ == "renote":
                body["targetNoteId"] = extra
            resp.cmd("XADD", key, stream_id(nid), "data", json.dumps(body, ensure_ascii=False, separators=(",", ":")))
            notif_entries += 1
    resp.close()

    # ── k6 ──────────────────────────────────────────────────────────
    # tokens は「同時に開いている」利用者。noteIds / noteOwners はその人たちのノートで、
    # noteOwners は tokens の添字 (scenarios.js の reactions-create が使う)
    k6_notes.sort(key=lambda k: (notes[k].user, notes[k].t))
    seed = {
        "tokens": tokens[:n_concurrent],
        "username": usernames[0],
        "usernames": usernames[:n_concurrent],
        "userIds": user_ids[:n_concurrent],
        "noteIds": [notes[k].id for k in k6_notes],
        "noteOwners": [notes[k].user for k in k6_notes],
        "reaction": REACTIONS[0],
        "adminToken": tokens[0],
    }
    with open(os.path.join(args.out, "seed-data.json"), "w") as f:
        json.dump(seed, f)
    stats = {
        "profile": args.profile, "now": now, "seconds": round(time.time() - started, 1),
        "rows": {tb: tw.rows for tb, tw in w.items()},
        "redis": {"commands": resp.commands, "listKeys": len(lists), "notificationKeys": notif_keys,
                  "listEntries": list_entries, "notifications": notif_entries},
        "prefix": prefix,
        # load.sh が meta.rootUserId にする (本家の /api/meta が requireSetup: true を返さないように)
        "rootUserId": user_ids[0],
        "hibernatedUsers": sum(hibernated),
        # load.sh はこの順と列で COPY する
        "tableOrder": TABLE_ORDER,
        "columns": COLUMNS,
    }
    with open(os.path.join(args.out, "stats.json"), "w") as f:
        json.dump(stats, f, indent=2)
    print(json.dumps(stats))


if __name__ == "__main__":
    sys.exit(main())
