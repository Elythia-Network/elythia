#!/usr/bin/env python3
"""Seeds data on the previous release and verifies it on the current code (#4123).

    scenario.py seed    run against the previous release; writes out/state.json
    scenario.py verify  run against the current code after `migrate up`

Uses only the standard library, so that it runs on the CI runner and on the
host without installing anything.
"""

import base64
import json
import os
import sys
import urllib.error
import urllib.request
import uuid

BASE = os.environ.get("UPGRADE_BASE", "http://127.0.0.1:61814")
HERE = os.path.dirname(os.path.abspath(__file__))
STATE = os.path.join(HERE, "out", "state.json")
PASSWORD = "upgrade-test-password"

# 1x1 の PNG。ドライブのファイルの行を作るだけなので、中身は何でもよい
PNG = base64.b64decode(
    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
)


class APIError(Exception):
    pass


def call(endpoint, body=None, token=None, want=(200, 204)):
    data = dict(body or {})
    if token:
        data["i"] = token
    req = urllib.request.Request(
        f"{BASE}/api/{endpoint}", data=json.dumps(data).encode(), headers={"Content-Type": "application/json"}
    )
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            status, raw = resp.status, resp.read()
    except urllib.error.HTTPError as e:
        status, raw = e.code, e.read()
    if status not in want:
        raise APIError(f"{endpoint}: status {status}: {raw[:300]!r}")
    return json.loads(raw) if raw else None


def upload(token, name, content):
    boundary = uuid.uuid4().hex
    parts = [
        f'--{boundary}\r\nContent-Disposition: form-data; name="i"\r\n\r\n{token}\r\n'.encode(),
        f'--{boundary}\r\nContent-Disposition: form-data; name="name"\r\n\r\n{name}\r\n'.encode(),
        f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="{name}"\r\nContent-Type: image/png\r\n\r\n'.encode()
        + content
        + b"\r\n",
        f"--{boundary}--\r\n".encode(),
    ]
    req = urllib.request.Request(
        f"{BASE}/api/drive/files/create",
        data=b"".join(parts),
        headers={"Content-Type": f"multipart/form-data; boundary={boundary}"},
    )
    with urllib.request.urlopen(req, timeout=30) as resp:
        return json.loads(resp.read())


def expect(cond, message):
    if not cond:
        raise AssertionError(message)


def expect_server(key, want):
    """Checks that BASE answers as the build run.sh started, not a leftover one."""
    if not want:
        return
    got = call("meta", {}).get(key)
    expect(got == want, f"the server at {BASE} reports {key} {got!r}, want {want!r}")


def seed():
    expect_server("mkGoVersion", os.environ.get("UPGRADE_EXPECT_VERSION"))
    admin = call("admin/accounts/create", {"username": "upadmin", "password": PASSWORD})
    a = admin["token"]
    call("admin/update-meta", {"disableRegistration": False}, a)
    alice = call("signup", {"username": "alice", "password": PASSWORD})
    bob = call("signup", {"username": "bob", "password": PASSWORD})
    at, bt = alice["token"], bob["token"]

    notes = {}
    for visibility in ("public", "home", "followers"):
        notes[visibility] = call("notes/create", {"text": f"{visibility} note", "visibility": visibility}, at)["createdNote"]["id"]
    notes["specified"] = call(
        "notes/create", {"text": "for bob", "visibility": "specified", "visibleUserIds": [bob["id"]]}, at
    )["createdNote"]["id"]
    notes["cw"] = call("notes/create", {"text": "hidden", "cw": "warning"}, at)["createdNote"]["id"]
    notes["reply"] = call("notes/create", {"text": "a reply", "replyId": notes["public"]}, bt)["createdNote"]["id"]
    notes["renote"] = call("notes/create", {"renoteId": notes["public"]}, bt)["createdNote"]["id"]

    call("following/create", {"userId": alice["id"]}, bt)
    call("notes/reactions/create", {"noteId": notes["public"], "reaction": "👍"}, bt)

    file = upload(at, "dot.png", PNG)
    clip = call("clips/create", {"name": "kept", "isPublic": True}, at)
    call("clips/add-note", {"clipId": clip["id"], "noteId": notes["public"]}, at)
    antenna = call(
        "antennas/create",
        {
            "name": "watch", "src": "all", "keywords": [["note"]], "excludeKeywords": [], "users": [],
            "caseSensitive": False, "withReplies": False, "withFile": False,
        },
        at,
    )
    role = call(
        "admin/roles/create",
        {
            "name": "kept-role", "description": "", "color": None, "iconUrl": None, "target": "manual",
            "condFormula": {}, "isPublic": True, "isExplorable": False, "isAdministrator": False,
            "isModerator": False, "asBadge": False, "canEditMembersByModerator": False,
            "displayOrder": 0, "policies": {},
        },
        a,
    )
    call("admin/roles/assign", {"roleId": role["id"], "userId": alice["id"]}, a)

    os.makedirs(os.path.dirname(STATE), exist_ok=True)
    with open(STATE, "w", encoding="utf-8") as f:
        json.dump(
            {
                "admin": a, "alice": {"id": alice["id"], "token": at}, "bob": {"id": bob["id"], "token": bt},
                "notes": notes, "file": {"id": file["id"], "md5": file["md5"], "size": file["size"]},
                "clip": clip["id"], "antenna": antenna["id"], "role": role["id"],
            },
            f,
        )
    print(f"seeded: {len(notes)} notes, 3 users, a follow, a reaction, a file, a clip, an antenna, a role")


def verify():
    with open(STATE, encoding="utf-8") as f:
        s = json.load(f)
    alice, bob, notes = s["alice"], s["bob"], s["notes"]

    expect_server("mkGoCommit", os.environ.get("UPGRADE_EXPECT_COMMIT"))
    # token とセッションは前の版のまま使える。パスワードの hash も読める
    expect(call("i", {}, alice["token"])["id"] == alice["id"], "alice's token no longer works")
    expect(call("i", {}, s["admin"])["isAdmin"], "the admin is no longer an administrator")
    expect(call("users/show", {"username": "bob"})["id"] == bob["id"], "bob is gone")
    signin = call("signin-flow", {"username": "bob", "password": PASSWORD})
    expect(signin.get("finished") and signin.get("id") == bob["id"], f"bob cannot sign in with his password: {signin}")

    want = {
        "public": ("public note", "public"), "home": ("home note", "home"),
        "followers": ("followers note", "followers"), "specified": ("for bob", "specified"),
        "cw": ("hidden", "public"),
    }
    for key, (text, visibility) in want.items():
        note = call("notes/show", {"noteId": notes[key]}, bob["token"])
        expect((note["text"], note["visibility"]) == (text, visibility), f"{key} note changed: {note['text']!r} {note['visibility']}")
    expect(call("notes/show", {"noteId": notes["cw"]}, alice["token"])["cw"] == "warning", "the cw is gone")
    expect(call("notes/show", {"noteId": notes["reply"]})["replyId"] == notes["public"], "the reply lost its parent")
    expect(call("notes/show", {"noteId": notes["renote"]})["renoteId"] == notes["public"], "the renote lost its note")
    # 宛先でない人には本文が見えないまま (管理者は見えるので、サインインしていない人で見る)。
    # 本家と同じく、ノートは返して本文を伏せる (isHidden)
    hidden = call("notes/show", {"noteId": notes["specified"]})
    expect(hidden.get("isHidden") is True and hidden.get("text") is None, f"the specified note is visible to a stranger: {hidden.get('text')!r}")

    public = call("notes/show", {"noteId": notes["public"]})
    expect(public["reactions"] == {"👍": 1}, f"the reaction is gone: {public['reactions']}")
    # ノートの reactions は数を写した列なので、リアクションの行そのものも読む
    reactions = call("notes/reactions", {"noteId": notes["public"]})
    expect([(r["user"]["id"], r["type"]) for r in reactions] == [(bob["id"], "👍")], f"bob's reaction row is gone: {reactions}")
    expect(public["renoteCount"] == 1 and public["repliesCount"] == 1, f"the counters changed: {public['renoteCount']} {public['repliesCount']}")
    following = call("users/following", {"userId": bob["id"]}, bob["token"])
    expect([f["followeeId"] for f in following] == [alice["id"]], "bob no longer follows alice")
    expect(call("users/show", {"userId": alice["id"]})["followersCount"] == 1, "alice's follower count changed")

    file = call("drive/files/show", {"fileId": s["file"]["id"]}, alice["token"])
    expect((file["md5"], file["size"]) == (s["file"]["md5"], s["file"]["size"]), "the drive file row changed")
    clipped = call("clips/notes", {"clipId": s["clip"]}, alice["token"])
    expect([n["id"] for n in clipped] == [notes["public"]], "the clip lost its note")
    expect(call("antennas/show", {"antennaId": s["antenna"]}, alice["token"])["keywords"] == [["note"]], "the antenna changed")
    members = call("admin/roles/users", {"roleId": s["role"]}, s["admin"])
    expect([m["user"]["id"] for m in members] == [alice["id"]], "the role lost alice")

    # 引き継いだデータの上に書き足せる
    new = call("notes/create", {"text": "after the upgrade", "replyId": notes["public"]}, alice["token"])["createdNote"]
    call("notes/reactions/create", {"noteId": new["id"], "reaction": "👍"}, bob["token"])
    expect(call("notes/show", {"noteId": new["id"]})["reactions"] == {"👍": 1}, "a reaction after the upgrade did not stick")
    timeline = call("notes/timeline", {"limit": 10}, bob["token"])
    expect(new["id"] in [n["id"] for n in timeline], "bob's home timeline misses alice's new note")
    print("verified: the data of the previous release reads and takes new writes")


def main():
    if len(sys.argv) != 2 or sys.argv[1] not in ("seed", "verify"):
        sys.exit("usage: scenario.py seed|verify")
    try:
        seed() if sys.argv[1] == "seed" else verify()
    except (APIError, AssertionError) as e:
        sys.exit(f"{sys.argv[1]}: {e}")


if __name__ == "__main__":
    main()
