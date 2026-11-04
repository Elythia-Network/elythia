"""Consistency checks of gen.py on the tiny profile (#4016).

The pull request bench runs only use the small data, so these tests are what
CI runs on the generator. They check what the servers rely on: counters that
match the rows, the fanout timeline rules and limits, notification stream ids,
hibernation, and the pairs the k6 reactions-create scenario will use.

  python3 -m unittest discover -s tests/bench/data -p 'test_*.py' -v
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import importlib.util
import unittest
from collections import Counter, defaultdict
from datetime import datetime

HERE = os.path.dirname(os.path.abspath(__file__))
NOW = 1793000000000


def generate(out: str) -> dict:
    keys = os.path.join(out, "keys")
    os.makedirs(keys)
    for name in ("pub.pem", "priv.pem"):
        with open(os.path.join(keys, name), "w") as f:
            f.write(f"-----BEGIN TEST {name}-----\n")
    data = os.path.join(out, "data")
    subprocess.run(
        [sys.executable, "-I", os.path.join(HERE, "gen.py"), "--profile", "tiny", "--out", data, "--now", str(NOW),
         "--public-key", os.path.join(keys, "pub.pem"), "--private-key", os.path.join(keys, "priv.pem")],
        check=True, stdout=subprocess.DEVNULL)
    with open(os.path.join(data, "stats.json")) as f:
        return json.load(f)


def unescape(v: str):
    if v == "\\N":
        return None
    return v.replace("\\t", "\t").replace("\\n", "\n").replace("\\r", "\r").replace("\\\\", "\\")


def read_table(data: str, stats: dict, table: str) -> list[dict]:
    cols = stats["columns"][table]
    rows = []
    with open(os.path.join(data, f"{table}.tsv"), encoding="utf-8") as f:
        for line in f:
            vals = line.rstrip("\n").split("\t")
            assert len(vals) == len(cols), (table, line)
            rows.append({c: unescape(v) for c, v in zip(cols, vals)})
    return rows


def pg_array(v: str) -> list[str]:
    inner = v[1:-1]
    return [x.strip('"') for x in inner.split(",")] if inner else []


def read_resp(path: str) -> list[list[str]]:
    with open(path, "rb") as f:
        buf = f.read()
    cmds, i = [], 0
    while i < len(buf):
        assert buf[i:i + 1] == b"*"
        j = buf.index(b"\r\n", i)
        n = int(buf[i + 1:j])
        i = j + 2
        args = []
        for _ in range(n):
            j = buf.index(b"\r\n", i)
            ln = int(buf[i + 1:j])
            args.append(buf[j + 2:j + 2 + ln].decode("utf-8"))
            i = j + 2 + ln + 2
        cmds.append(args)
    return cmds


def load_gen():
    spec = importlib.util.spec_from_file_location("gen", os.path.join(HERE, "gen.py"))
    mod = importlib.util.module_from_spec(spec)
    # dataclass は sys.modules から自分のモジュールを引く
    sys.modules["gen"] = mod
    spec.loader.exec_module(mod)
    return mod


class HibernationBoundaryTest(unittest.TestCase):
    def test_moves_times_near_the_boundary_to_the_hibernated_side(self):
        gen = load_gen()
        day = 86400_000
        for age_days in (49, 49.5, 50, 50.5, 51):
            moved = gen.off_hibernation_boundary(NOW, NOW - int(age_days * day))
            self.assertGreater(NOW - moved, 51 * day, age_days)
        for age_days in (0, 29, 48.9, 51.1, 300):
            t = NOW - int(age_days * day)
            self.assertEqual(gen.off_hibernation_boundary(NOW, t), t, age_days)


class GenTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.TemporaryDirectory()
        cls.stats = generate(cls.tmp.name)
        cls.data = os.path.join(cls.tmp.name, "data")
        cls.t = {tb: read_table(cls.data, cls.stats, tb) for tb in cls.stats["tableOrder"]}
        cls.users = {u["id"]: u for u in cls.t["user"]}
        cls.notes = {n["id"]: n for n in cls.t["note"]}
        cls.redis = read_resp(os.path.join(cls.data, "redis.resp"))
        with open(os.path.join(cls.data, "seed-data.json")) as f:
            cls.seed = json.load(f)

    @classmethod
    def tearDownClass(cls):
        cls.tmp.cleanup()

    def test_deterministic(self):
        with tempfile.TemporaryDirectory() as other:
            generate(other)
            for name in os.listdir(self.data):
                if name == "stats.json":
                    continue
                with open(os.path.join(self.data, name), "rb") as a, open(os.path.join(other, "data", name), "rb") as b:
                    self.assertEqual(a.read(), b.read(), name)

    def test_row_counts_match_stats(self):
        for tb, rows in self.t.items():
            self.assertEqual(len(rows), self.stats["rows"][tb], tb)

    def test_ids_are_aidx_and_unique(self):
        for tb in ("user", "note", "following", "note_reaction", "drive_file", "instance"):
            ids = [r["id"] for r in self.t[tb]]
            self.assertEqual(len(ids), len(set(ids)), tb)
            for i in ids:
                self.assertRegex(i, r"^[0-9a-z]{16}$")

    def test_user_counters(self):
        notes_by_user = Counter(n["userId"] for n in self.t["note"])
        followers = Counter(f["followeeId"] for f in self.t["following"])
        following = Counter(f["followerId"] for f in self.t["following"])
        for u in self.t["user"]:
            self.assertEqual(int(u["notesCount"]), notes_by_user[u["id"]], u["id"])
            self.assertEqual(int(u["followersCount"]), followers[u["id"]], u["id"])
            self.assertEqual(int(u["followingCount"]), following[u["id"]], u["id"])

    def test_instance_counters(self):
        users = Counter(u["host"] for u in self.t["user"] if u["host"])
        notes = Counter(n["userHost"] for n in self.t["note"] if n["userHost"])
        followers = Counter(f["followeeHost"] for f in self.t["following"] if f["followeeHost"])
        for i in self.t["instance"]:
            self.assertEqual(int(i["usersCount"]), users[i["host"]])
            self.assertEqual(int(i["notesCount"]), notes[i["host"]])
            self.assertEqual(int(i["followersCount"]), followers[i["host"]])

    def test_note_counters(self):
        replies = Counter(n["replyId"] for n in self.t["note"] if n["replyId"])
        # 本家は自分のノートのリノートを数えない
        renotes = Counter(n["renoteId"] for n in self.t["note"]
                          if n["renoteId"] and self.notes[n["renoteId"]]["userId"] != n["userId"])
        for n in self.t["note"]:
            self.assertEqual(int(n["repliesCount"]), replies[n["id"]], n["id"])
            self.assertEqual(int(n["renoteCount"]), renotes[n["id"]], n["id"])

    def test_reply_and_renote_fields(self):
        for n in self.t["note"]:
            if n["replyId"]:
                parent = self.notes[n["replyId"]]
                self.assertEqual(n["replyUserId"], parent["userId"])
                self.assertEqual(n["replyUserHost"], parent["userHost"])
                self.assertEqual(n["threadId"], parent["threadId"] or parent["id"])
            else:
                self.assertIsNone(n["threadId"])
            if n["renoteId"]:
                self.assertEqual(n["renoteUserId"], self.notes[n["renoteId"]]["userId"])
                self.assertIsNone(n["text"])

    def test_reactions(self):
        by_note = defaultdict(list)
        for r in self.t["note_reaction"]:
            by_note[r["noteId"]].append(r)
        follows = {(f["followerId"], f["followeeId"]) for f in self.t["following"]}
        pairs = set()
        for nid, rs in by_note.items():
            n = self.notes[nid]
            for r in rs:
                self.assertNotIn((r["userId"], nid), pairs, "a user reacted twice to a note")
                pairs.add((r["userId"], nid))
                self.assertNotEqual(r["userId"], n["userId"], "a user reacted to their own note")
                if n["visibility"] == "followers":
                    self.assertIn((r["userId"], n["userId"]), follows, "a non-follower reacted to a followers-only note")
        for n in self.t["note"]:
            rs = by_note.get(n["id"], [])
            self.assertEqual(json.loads(n["reactions"]), dict(Counter(r["reaction"] for r in rs)), n["id"])
            cache = pg_array(n["reactionAndUserPairCache"])
            self.assertLessEqual(len(cache), 16)
            self.assertEqual(len(cache), min(len(rs), 16))
            self.assertTrue(set(cache) <= {f'{r["userId"]}/{r["reaction"]}' for r in rs})

    def test_hibernation(self):
        day = 86400_000
        for u in self.t["user"]:
            if u["host"] is None:
                last = int(datetime.fromisoformat(u["lastActiveDate"]).timestamp() * 1000)
                # 本家の checkHibernation と同じく、最終アクティブが 50 日より前なら休眠
                self.assertEqual(u["isHibernated"] == "t", NOW - last > 50 * day, u["id"])
                # 境目の前後 1 日には置かない (計測の間に越えないように)
                self.assertFalse(49 * day <= NOW - last <= 51 * day, u["id"])
            else:
                self.assertEqual(u["isHibernated"], "f")
        hibernated = {u["id"] for u in self.t["user"] if u["isHibernated"] == "t"}
        self.assertEqual(len(hibernated), self.stats["hibernatedUsers"])
        self.assertGreater(len(hibernated), 0)
        for f in self.t["following"]:
            self.assertEqual(f["isFollowerHibernated"] == "t", f["followerId"] in hibernated)
        # 休眠した人のホームには、他人のノートを積まない
        prefix = self.stats["prefix"]
        for args in self.redis:
            if args[0] == "RPUSH" and args[1].startswith(prefix + "list:homeTimeline:"):
                uid = args[1].rsplit(":", 1)[1]
                if uid in hibernated:
                    for nid in args[2:]:
                        self.assertEqual(self.notes[nid]["userId"], uid)
        for u in self.seed["userIds"]:
            self.assertNotIn(u, hibernated, "a k6 user is hibernated")

    def test_timelines(self):
        prefix = self.stats["prefix"]
        caps = {"homeTimeline": 300, "homeTimelineWithFiles": 150, "localTimeline": 1000,
                "localTimelineWithFiles": 500, "localTimelineWithReplies": 300, "localTimelineWithReplyTo": 30}
        lists = defaultdict(list)
        for args in self.redis:
            if args[0] == "RPUSH":
                self.assertTrue(args[1].startswith(prefix + "list:"))
                lists[args[1][len(prefix + "list:"):]].extend(args[2:])
        self.assertEqual(len(lists), self.stats["redis"]["listKeys"])
        reached = set()
        for name, ids in lists.items():
            kind = name.split(":")[0]
            cap = caps.get(kind)
            if kind.startswith("userTimeline"):
                local = self.users[name.split(":")[1]]["host"] is None
                cap = (300 if local else 100) // (2 if kind == "userTimelineWithFiles" else 1)
            if cap is not None:
                self.assertLessEqual(len(ids), cap, name)
                if len(ids) == cap:
                    reached.add(kind)
            # 新しいものが先頭 (LPUSH で積んだ形)
            self.assertEqual(ids, sorted(ids, reverse=True), name)
            for nid in ids:
                n = self.notes[nid]
                is_reply = n["replyId"] is not None and n["replyUserId"] != n["userId"]
                if kind == "localTimeline":
                    self.assertEqual((n["visibility"], n["userHost"], is_reply), ("public", None, False))
                if kind == "userTimeline":
                    self.assertFalse(is_reply)
                if kind.endswith("WithFiles"):
                    self.assertNotEqual(pg_array(n["fileIds"]), [])
        # 上限を確かめたことにするには、上限まで積まれる一覧が要る
        self.assertTrue({"homeTimeline", "homeTimelineWithFiles", "localTimeline", "localTimelineWithReplies"} <= reached,
                        reached)

    def lists(self) -> dict[str, list[str]]:
        prefix = self.stats["prefix"]
        out = defaultdict(list)
        for args in self.redis:
            if args[0] == "RPUSH":
                out[args[1][len(prefix + "list:"):]].extend(args[2:])
        return out

    def is_reply(self, n: dict, viewer: str | None) -> bool:
        # 本家の misc/is-reply.ts
        return n["replyId"] is not None and n["replyUserId"] != n["userId"] and n["replyUserId"] != viewer

    def test_home_timelines_follow_pushtotl(self):
        """Rebuild each local user's home timeline from the rows and compare."""
        lists = self.lists()
        hibernated = {u["id"] for u in self.t["user"] if u["isHibernated"] == "t"}
        followees = defaultdict(set)
        for f in self.t["following"]:
            followees[f["followerId"]].add(f["followeeId"])
        notes_by_id = sorted(self.t["note"], key=lambda n: n["id"], reverse=True)
        checked = 0
        for u in self.t["user"]:
            if u["host"] is not None:
                continue
            uid = u["id"]
            want = []
            for n in notes_by_id:
                if n["userId"] == uid:
                    want.append(n["id"])
                elif uid not in hibernated and n["userId"] in followees[uid] and not self.is_reply(n, uid):
                    want.append(n["id"])
                if len(want) == 300:
                    break
            self.assertEqual(lists.get(f"homeTimeline:{uid}", []), want, uid)
            if uid not in hibernated and any(self.notes[i]["userId"] != uid for i in want):
                checked += 1
        self.assertGreater(checked, 0)

    def test_all_timelines_follow_pushtotl(self):
        """Rebuild every fanout timeline from the rows with upstream's
        NoteCreateService.pushToTl rules, and compare keys and contents.
        """
        hibernated = {u["id"] for u in self.t["user"] if u["isHibernated"] == "t"}
        followers = defaultdict(list)
        for f in self.t["following"]:
            if f["followerHost"] is None and f["isFollowerHibernated"] == "f":
                followers[f["followeeId"]].append(f["followerId"])
        want: dict[str, list[str]] = defaultdict(list)
        caps: dict[str, int] = {}

        def push(name, nid, cap):
            want[name].append(nid)
            caps[name] = cap

        for n in sorted(self.t["note"], key=lambda n: n["id"]):
            uid, nid = n["userId"], n["id"]
            local = n["userHost"] is None
            files = pg_array(n["fileIds"]) != []
            user_cap = 300 if local else 100
            for f in followers[uid]:
                if self.is_reply(n, f):
                    continue
                push(f"homeTimeline:{f}", nid, 300)
                if files:
                    push(f"homeTimelineWithFiles:{f}", nid, 150)
            if local:
                push(f"homeTimeline:{uid}", nid, 300)
                if files:
                    push(f"homeTimelineWithFiles:{uid}", nid, 150)
            if self.is_reply(n, None):
                push(f"userTimelineWithReplies:{uid}", nid, user_cap)
                if n["visibility"] == "public" and local:
                    push("localTimelineWithReplies", nid, 300)
                    if n["replyUserHost"] is None:
                        push(f"localTimelineWithReplyTo:{n['replyUserId']}", nid, 30)
            else:
                push(f"userTimeline:{uid}", nid, user_cap)
                if files:
                    push(f"userTimelineWithFiles:{uid}", nid, user_cap // 2)
                if n["visibility"] == "public" and local:
                    push("localTimeline", nid, 1000)
                    if files:
                        push("localTimelineWithFiles", nid, 500)
        expected = {name: list(reversed(ids[-caps[name]:])) for name, ids in want.items()}
        got = self.lists()
        self.assertEqual(set(got), set(expected))
        for name in expected:
            self.assertEqual(got[name], expected[name], name)
        self.assertTrue(hibernated)

    def test_user_and_local_timelines_follow_pushtotl(self):
        lists = self.lists()
        for name, ids in lists.items():
            kind, _, arg = name.partition(":")
            for nid in ids:
                n = self.notes[nid]
                if kind == "userTimeline":
                    self.assertEqual(n["userId"], arg)
                    self.assertFalse(self.is_reply(n, None), nid)
                elif kind == "userTimelineWithReplies":
                    self.assertEqual(n["userId"], arg)
                    self.assertTrue(self.is_reply(n, None), nid)
                elif kind == "localTimelineWithReplies":
                    self.assertEqual((n["visibility"], n["userHost"]), ("public", None))
                    self.assertTrue(self.is_reply(n, None), nid)
                elif kind == "localTimelineWithReplyTo":
                    # 返信先がローカルの利用者のときだけ
                    self.assertEqual((n["replyUserId"], n["replyUserHost"]), (arg, None))
                    self.assertEqual((n["visibility"], n["userHost"]), ("public", None))
                    self.assertTrue(self.is_reply(n, None), nid)
        # 自分への返信は返信として扱わない (userTimeline に入る)
        self_replies = [n["id"] for n in self.t["note"]
                        if n["replyId"] and n["replyUserId"] == n["userId"]]
        self.assertGreater(len(self_replies), 0)
        in_user_tl = {i for name, ids in lists.items() if name.startswith("userTimeline:") for i in ids}
        self.assertTrue(in_user_tl & set(self_replies))

    def test_notification_streams(self):
        prefix = self.stats["prefix"]
        last: dict[str, tuple[int, int]] = {}
        per_key = Counter()
        for args in self.redis:
            if args[0] != "XADD":
                continue
            key, sid = args[1], args[2]
            self.assertTrue(key.startswith(prefix + "notificationTimeline:"))
            ms, seq = (int(x) for x in sid.split("-"))
            self.assertGreater((ms, seq), last.get(key, (0, -1)), "stream ids must grow within a key")
            last[key] = (ms, seq)
            per_key[key] += 1
            body = json.loads(args[4])
            self.assertEqual(args[3], "data")
            # 本家の toXListId: <ms>-<id の後ろ 8 文字を 36 進で読んだ値>
            self.assertEqual(sid, f"{int(body['id'][:8], 36) + 946684800000}-{int(body['id'][8:], 36)}")
            self.assertIn(body["notifierId"], self.users)
            if "noteId" in body:
                self.assertIn(body["noteId"], self.notes)
        self.assertEqual(len(per_key), self.stats["redis"]["notificationKeys"])
        self.assertTrue(all(c <= 500 for c in per_key.values()))

    def test_k6_seed(self):
        tokens = self.seed["tokens"]
        by_token = {u["token"]: u["id"] for u in self.t["user"] if u["token"]}
        token_users = [by_token[t] for t in tokens]
        self.assertEqual(token_users, self.seed["userIds"])
        self.assertEqual(len(self.seed["noteIds"]), len(self.seed["noteOwners"]))
        for nid, owner in zip(self.seed["noteIds"], self.seed["noteOwners"]):
            self.assertEqual(self.notes[nid]["userId"], token_users[owner])
            self.assertIsNone(self.notes[nid]["renoteId"])
        # reactions-create の組 (k6 の利用者, k6 のノート) は、seed に無い
        k6_notes = set(self.seed["noteIds"])
        for r in self.t["note_reaction"]:
            if r["noteId"] in k6_notes:
                self.assertNotIn(r["userId"], token_users)


if __name__ == "__main__":
    unittest.main()
