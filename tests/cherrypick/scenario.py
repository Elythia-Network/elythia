#!/usr/bin/env python3
"""CherryPick からの移行の検査のデータを作り (seed)、移行した後に確かめる (verify)。

run.sh から呼ぶ (#4249)。標準ライブラリだけを使う。

    scenario.py seed   <CherryPick の URL> <state.json>
    scenario.py verify <Elythia の URL> <state.json> <resolvers.json>
    scenario.py verify-after-cleanup <Elythia の URL> <state.json>
"""
import json
import sys
import time
import urllib.error
import urllib.request


def api(base, endpoint, body=None, token=None):
    body = dict(body or {})
    if token:
        body["i"] = token
    req = urllib.request.Request(
        base.rstrip("/") + "/api/" + endpoint,
        data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(req, timeout=30) as res:
            raw = res.read()
            return res.status, (json.loads(raw) if raw else None)
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            return e.code, json.loads(raw)
        except ValueError:
            return e.code, None


def ok(base, endpoint, body=None, token=None):
    status, res = api(base, endpoint, body, token)
    if status >= 300:
        raise SystemExit(f"{endpoint}: {status} {res}")
    return res


def seed(base, path):
    admin = ok(base, "admin/accounts/create", {"username": "admin", "password": "adminpass1"})["token"]
    ok(base, "admin/update-meta", {"disableRegistration": False}, admin)
    alice = ok(base, "signup", {"username": "alice", "password": "alicepass1"})
    a = alice["token"]
    # 承認制より前からの会員。run.sh が approved を false にして、承認の印の無い会員にする
    ok(base, "signup", {"username": "dave", "password": "davepass1"})
    notes = {
        "first": ok(base, "notes/create", {"text": "CherryPickからの最初の投稿"}, a)["createdNote"]["id"],
        # 予約削除。run.sh が期限を過去にする
        "scheduled": ok(base, "notes/create", {"text": "予約削除の投稿", "scheduledDelete": {"deleteAfter": 86400000}}, a)["createdNote"]["id"],
        "edited": ok(base, "notes/create", {"text": "編集前の本文"}, a)["createdNote"]["id"],
    }
    ok(base, "notes/update", {"noteId": notes["edited"], "text": "編集後の本文", "cw": None}, a)
    ok(base, "admin/abuse-report-resolver/create", {
        "name": "spam", "targetUserPattern": None, "reporterPattern": None,
        "reportContentPattern": "spam", "expiresAt": "indefinitely", "forward": False,
    }, admin)
    ok(base, "admin/update-meta", {"approvalRequiredForSignup": True}, admin)
    # 承認待ち。CherryPick では理由が必須で、サインインは止まる
    status, _ = api(base, "signup", {"username": "carol", "password": "carolpass1", "reason": "参加したいです"})
    if status >= 300:
        raise SystemExit(f"signup carol: {status}")
    status, res = api(base, "signin-flow", {"username": "carol", "password": "carolpass1"})
    if status != 403:
        raise SystemExit(f"CherryPick let the pending user sign in: {status} {res}")
    with open(path, "w") as f:
        json.dump({"admin": admin, "alice": a, "notes": notes}, f)
    print("seeded", notes)


def verify(base, path, resolvers_path):
    with open(path) as f:
        state = json.load(f)
    failures = []

    def check(name, cond, detail=""):
        print(("ok   " if cond else "FAIL ") + name + (f": {detail}" if detail and not cond else ""))
        if not cond:
            failures.append(name)

    status, res = api(base, "i", {}, state["alice"])
    check("CherryPick のトークンがそのまま使える", status == 200 and res.get("username") == "alice", status)
    status, _ = api(base, "signin-flow", {"username": "alice", "password": "alicepass1"})
    check("パスワードでサインインできる", status == 200, status)
    status, _ = api(base, "signin-flow", {"username": "carol", "password": "carolpass1"})
    check("承認待ちだった利用者はサインインできない", status == 403, status)
    status, res = api(base, "admin/show-users", {"username": "carol", "origin": "local"}, state["admin"])
    carol = [u for u in (res or []) if u.get("username") == "carol"] if status == 200 else []
    check("承認待ちだった利用者は凍結されている", len(carol) == 1 and carol[0].get("isSuspended") is True, res)
    status, _ = api(base, "signin-flow", {"username": "dave", "password": "davepass1"})
    check("承認の印の無い会員は、選んだとおり凍結されてサインインできない", status == 403, status)
    status, res = api(base, "admin/show-users", {"username": "dave", "origin": "local"}, state["admin"])
    dave = [u for u in (res or []) if u.get("username") == "dave"] if status == 200 else []
    check("承認の印の無い会員は凍結されている", len(dave) == 1 and dave[0].get("isSuspended") is True, res)
    status, _ = api(base, "signin-flow", {"username": "admin", "password": "adminpass1"})
    check("承認の印の無い初期ユーザーは、凍結されずにサインインできる", status == 200, status)
    status, res = api(base, "notes/show", {"noteId": state["notes"]["first"]})
    check("投稿が読める", status == 200 and res.get("text") == "CherryPickからの最初の投稿", status)
    status, res = api(base, "notes/show", {"noteId": state["notes"]["edited"]})
    check("編集した投稿は編集後の本文で出る", status == 200 and res.get("text") == "編集後の本文", status)
    status, res = api(base, "notes/show", {"noteId": state["notes"]["scheduled"]})
    code = ((res or {}).get("error") or {}).get("code")
    check("期限を過ぎた予約削除の投稿は消えている", status == 400 and code == "NO_SUCH_NOTE", (status, code))
    status, meta = api(base, "admin/meta", {}, state["admin"])
    check("隔離していたホストがブロックに入る", status == 200 and "remote.example" in (meta or {}).get("blockedHosts", []), meta and meta.get("blockedHosts"))
    check("repositoryUrl が直っている", (meta or {}).get("repositoryUrl") == "https://github.com/Elythia-Network/elythia")
    check("feedbackUrl が直っている", (meta or {}).get("feedbackUrl") == "https://github.com/Elythia-Network/elythia/issues/new")
    with open(resolvers_path) as f:
        resolvers = json.load(f)
    check("通報の自動処理の規則が書き出されている", [r["name"] for r in resolvers] == ["spam"], resolvers)
    status, _ = api(base, "notes/create", {"text": "Elythiaに移ってからの投稿"}, state["alice"])
    check("移った後に投稿できる", status == 200, status)

    if failures:
        raise SystemExit(f"{len(failures)} check(s) failed")


def verify_after_cleanup(base, path):
    """後始末で列と表を消した後も、user と note を読み書きできることを確かめる。"""
    with open(path) as f:
        state = json.load(f)
    status, res = api(base, "i", {}, state["alice"])
    if status != 200 or res.get("username") != "alice":
        raise SystemExit(f"i after cleanup: {status} {res}")
    status, res = api(base, "notes/show", {"noteId": state["notes"]["first"]})
    if status != 200:
        raise SystemExit(f"notes/show after cleanup: {status} {res}")
    status, res = api(base, "notes/create", {"text": "後始末の後の投稿"}, state["alice"])
    if status != 200:
        raise SystemExit(f"notes/create after cleanup: {status} {res}")
    print("ok   後始末の後も読み書きできる")


def main():
    if len(sys.argv) >= 4 and sys.argv[1] == "seed":
        seed(sys.argv[2], sys.argv[3])
    elif len(sys.argv) >= 5 and sys.argv[1] == "verify":
        verify(sys.argv[2], sys.argv[3], sys.argv[4])
    elif len(sys.argv) >= 4 and sys.argv[1] == "verify-after-cleanup":
        verify_after_cleanup(sys.argv[2], sys.argv[3])
    else:
        raise SystemExit(__doc__)


if __name__ == "__main__":
    main()
