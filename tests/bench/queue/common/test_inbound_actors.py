"""Tests for sending inbound activities from many faker actors (#4138)."""

from __future__ import annotations

import importlib
import io
import json
import os
import sys
import types
import unittest
from typing import Any
from unittest.mock import patch


def load(module: str) -> Any:
    """Load a bench module without its network-only runtime packages."""
    httpx_stub = types.ModuleType("httpx")
    httpx_stub.post = lambda *args, **kwargs: None
    httpx_stub.get = lambda *args, **kwargs: None
    psycopg_stub = types.ModuleType("psycopg")
    queue_probe_stub = types.ModuleType("queue_probe")
    queue_probe_stub.DriverKind = str
    queue_probe_stub.QueueProbe = object
    queue_probe_stub.TS_REDIS_PREFIX = "ts"
    queue_probe_stub.make_probe = lambda *args, **kwargs: None
    environment = {
        "REDIS_MKQ_HOST": "redis-mkq",
        "REDIS_TS_HOST": "redis-ts",
        "MKGO_MKQ_URL": "https://mk-mkq",
        "TS_URL": "https://ts",
        "FAKER_URL": "http://faker:8081",
        "FAKER_HOST": "faker",
    }
    with patch.dict(os.environ, environment), patch.dict(
        sys.modules,
        {"httpx": httpx_stub, "psycopg": psycopg_stub, "queue_probe": queue_probe_stub},
    ):
        sys.modules.pop(module, None)
        return importlib.import_module(module)


seed = load("seed")
driver = load("driver_inbound")
report = load("report")


def faker_listing(count: int) -> dict[str, Any]:
    return {
        "publicKeyPem": "PEM",
        "actors": [
            {
                "id": f"https://faker/users/bench-sender-{n}",
                "username": f"bench-sender-{n}",
                "inbox": f"https://faker/users/bench-sender-{n}/inbox",
                "keyId": f"https://faker/users/bench-sender-{n}#main-key",
            }
            for n in range(count)
        ],
    }


class RecordingCursor:
    def __init__(self, tables: tuple[bool, bool]) -> None:
        self.tables = tables
        self.calls: list[tuple[str, str, Any]] = []

    def executemany(self, sql: str, params: list[tuple[Any, ...]]) -> None:
        self.calls.append(("many", sql, params))

    def execute(self, sql: str, params: Any = None) -> None:
        self.calls.append(("one", sql, params))

    def fetchone(self) -> tuple[bool, bool]:
        return self.tables


class SeedTests(unittest.TestCase):
    def test_default_is_a_thousand_actors(self) -> None:
        self.assertEqual(seed.INBOUND_ACTORS, 1000)

    def test_rows_give_each_actor_its_own_user_and_key_id_with_the_shared_key(self) -> None:
        rows = seed.faker_actor_rows(faker_listing(1000))
        self.assertEqual(len(rows), 1000)
        self.assertEqual(len({r["id"] for r in rows}), 1000)
        self.assertEqual(len({r["uri"] for r in rows}), 1000)
        self.assertEqual(rows[3]["username"], "bench-sender-3")
        self.assertEqual(rows[3]["usernameLower"], "bench-sender-3")
        self.assertEqual(rows[3]["inbox"], "https://faker/users/bench-sender-3/inbox")
        self.assertEqual(rows[3]["keyId"], "https://faker/users/bench-sender-3#main-key")
        self.assertEqual({r["keyPem"] for r in rows}, {"PEM"})

    def test_rows_refuse_duplicate_ids(self) -> None:
        with patch.object(seed, "aidx_id", lambda prefix="f": "fkduplicate00000"):
            with self.assertRaises(RuntimeError):
                seed.faker_actor_rows(faker_listing(2))

    def test_insert_writes_every_actor_and_its_key_for_elythia(self) -> None:
        rows = seed.faker_actor_rows(faker_listing(5))
        cur = RecordingCursor((True, False))
        seed.insert_faker_actor_rows(cur, rows)
        many = [c for c in cur.calls if c[0] == "many"]
        self.assertEqual(len(many), 2)
        self.assertIn('INSERT INTO "user"', many[0][1])
        # 取得済みとして登録しないと、Elythia だけが投稿者を取りに行く
        self.assertIn('"lastFetchedAt"', many[0][1])
        self.assertIn("now()", many[0][1])
        self.assertEqual([p[4] for p in many[0][2]], [r["uri"] for r in rows])
        self.assertEqual({p[3] for p in many[0][2]}, {"faker"})
        self.assertIn("user_publickey", many[1][1])
        self.assertEqual([p[1] for p in many[1][2]], [r["keyId"] for r in rows])

    def test_insert_writes_the_keypair_table_for_misskey(self) -> None:
        rows = seed.faker_actor_rows(faker_listing(3))
        cur = RecordingCursor((False, True))
        seed.insert_faker_actor_rows(cur, rows)
        many = [c for c in cur.calls if c[0] == "many"]
        self.assertIn("user_keypair", many[1][1])
        self.assertEqual([p[0] for p in many[1][2]], [r["id"] for r in rows])


    def test_faker_state_records_the_count_the_driver_reads(self) -> None:
        state = seed.faker_state(faker_listing(1000))
        self.assertEqual(state["actors"], 1000)
        self.assertEqual(driver.sender_actors({"faker": state}), 1000)


class DriverTests(unittest.TestCase):
    def test_sender_actors_follows_the_seed(self) -> None:
        self.assertEqual(driver.sender_actors({"faker": {"actors": 1000}}), 1000)

    def test_sender_actors_refuses_a_seed_without_actors(self) -> None:
        # #4138 より前の seed.json は番号の無い bench-sender を登録していて、faker はもう応答しない
        for old in ({"faker": {}}, {}, {"faker": {"actors": 0}}):
            with self.assertRaises(ValueError):
                driver.sender_actors(old)

    def test_sender_actors_warns_when_the_inbound_run_asks_for_another_count(self) -> None:
        out = io.StringIO()
        with patch("sys.stdout", out):
            self.assertEqual(driver.sender_actors({"faker": {"actors": 1000}}, "1"), 1000)
        self.assertIn("INBOUND_ACTORS=1 is ignored", out.getvalue())
        out = io.StringIO()
        with patch("sys.stdout", out):
            driver.sender_actors({"faker": {"actors": 1000}}, "1000")
        self.assertEqual(out.getvalue(), "")

    def test_main_sends_from_the_actors_the_seed_registered(self) -> None:
        seed_json = json.dumps({"faker": {"actors": 1000}, "stacks": {"mkq": {}, "ts": {}}})
        written: dict[str, str] = {}

        class Sink(io.StringIO):
            def __init__(self, path: str) -> None:
                super().__init__()
                self.path = path

            def close(self) -> None:
                written[self.path] = self.getvalue()
                super().close()

        def fake_open(path: str, mode: str = "r", *args: Any, **kwargs: Any) -> io.StringIO:
            if path == "/state/seed.json":
                return io.StringIO(seed_json)
            return Sink(path)

        calls: list[dict[str, Any]] = []

        def fake_send(targets: list[str], count: int, concurrency: int, activity_type: str,
                      objects: dict[str, str], actors: int = 1) -> dict[str, Any]:
            calls.append({"count": count, "type": activity_type, "actors": actors})
            return {"preSignMs": 0, "totalMs": 0, "stats": []}

        with patch("builtins.open", fake_open), \
                patch.object(driver, "faker_send", fake_send), \
                patch.object(driver, "drain_one", lambda *a, **k: {}), \
                patch("sys.stdout", io.StringIO()):
            self.assertEqual(driver.main(), 0)
        # 下準備で投稿者ごとに 1 件、続けて計測の全件を、登録した投稿者から送る
        self.assertEqual(calls, [
            {"count": 1000, "type": "create", "actors": 1000},
            {"count": driver.INBOUND_COUNT, "type": driver.INBOUND_ACTIVITY_TYPE, "actors": 1000},
        ])
        out = json.loads(written["/results/inbound.json"])
        self.assertEqual(out["actors"], 1000)
        self.assertIsNotNone(out["warmup"])

    def test_sender_actors_warns_on_a_value_that_is_not_a_number(self) -> None:
        out = io.StringIO()
        with patch("sys.stdout", out):
            self.assertEqual(driver.sender_actors({"faker": {"actors": 1000}}, "many"), 1000)
        self.assertIn("INBOUND_ACTORS=many is ignored", out.getvalue())

    def test_main_stops_when_the_warmup_is_not_delivered(self) -> None:
        def fake_send(targets: list[str], count: int, *args: Any, **kwargs: Any) -> dict[str, Any]:
            return {"preSignMs": 0, "totalMs": 0, "stats": [{"target": "https://mk-mkq/inbox", "sent": count, "ok": count - 1}]}

        seed_json = json.dumps({"faker": {"actors": 5}, "stacks": {"mkq": {}}})

        def fake_open(path: str, mode: str = "r", *args: Any, **kwargs: Any) -> io.StringIO:
            return io.StringIO(seed_json) if path == "/state/seed.json" else io.StringIO()

        with patch("builtins.open", fake_open), \
                patch.object(driver, "faker_send", fake_send), \
                patch.object(driver, "drain_one", lambda *a, **k: {}), \
                patch("sys.stdout", io.StringIO()):
            self.assertEqual(driver.main(), 3)

    def test_main_stops_when_the_warmup_does_not_drain(self) -> None:
        calls: list[int] = []

        def fake_send(targets: list[str], count: int, *args: Any, **kwargs: Any) -> dict[str, Any]:
            calls.append(count)
            return {"preSignMs": 0, "totalMs": 0, "stats": []}

        seed_json = json.dumps({"faker": {"actors": 5}, "stacks": {"mkq": {}, "ts": {}}})

        def fake_open(path: str, mode: str = "r", *args: Any, **kwargs: Any) -> io.StringIO:
            return io.StringIO(seed_json) if path == "/state/seed.json" else io.StringIO()

        def drain(stack: str, *args: Any, **kwargs: Any) -> dict[str, Any]:
            return {"timed_out": stack == "ts"}

        with patch("builtins.open", fake_open), \
                patch.object(driver, "faker_send", fake_send), \
                patch.object(driver, "drain_one", drain), \
                patch("sys.stdout", io.StringIO()):
            self.assertEqual(driver.main(), 3)
        # 計測の送信はしない
        self.assertEqual(calls, [5])

    def test_main_can_skip_the_warmup(self) -> None:
        calls: list[int] = []

        def fake_send(targets: list[str], count: int, *args: Any, **kwargs: Any) -> dict[str, Any]:
            calls.append(count)
            return {"preSignMs": 0, "totalMs": 0, "stats": []}

        seed_json = json.dumps({"faker": {"actors": 5}, "stacks": {"mkq": {}}})

        def fake_open(path: str, mode: str = "r", *args: Any, **kwargs: Any) -> io.StringIO:
            return io.StringIO(seed_json) if path == "/state/seed.json" else io.StringIO()

        with patch("builtins.open", fake_open), \
                patch.object(driver, "faker_send", fake_send), \
                patch.object(driver, "drain_one", lambda *a, **k: {}), \
                patch.object(driver, "INBOUND_WARMUP", False), \
                patch("sys.stdout", io.StringIO()):
            self.assertEqual(driver.main(), 0)
        self.assertEqual(calls, [driver.INBOUND_COUNT])

    def test_send_and_drain_stops_before_sending_when_a_queue_is_missing(self) -> None:
        sent: list[int] = []

        def missing(stack: str) -> None:
            if stack == "ts":
                raise RuntimeError("[ts] queue not found")

        with patch.object(driver, "inbox_probe", missing), \
                patch.object(driver, "faker_send", lambda *a, **k: sent.append(1)):
            with self.assertRaises(RuntimeError):
                driver.send_and_drain({"stacks": {"mkq": {}, "ts": {}}}, [], 10, "create", {}, 1)
        self.assertEqual(sent, [])

    def test_inbox_probe_passes_the_misskey_redis_prefix(self) -> None:
        calls: list[tuple[Any, ...]] = []
        with patch.object(driver, "make_probe", lambda *args: calls.append(args)):
            driver.inbox_probe("ts")
            driver.inbox_probe("mkq")
        self.assertEqual(calls, [
            ("ts", "bullmq", "redis-ts", "inbox", "ts"),
            ("mkq", "mkq", "redis-mkq", "inbox", ""),
        ])

    def test_faker_send_passes_the_actors(self) -> None:
        sent: dict[str, Any] = {}

        class Response:
            def raise_for_status(self) -> None:
                pass

            def json(self) -> dict[str, Any]:
                return {}

        def post(url: str, json: dict[str, Any], timeout: float) -> Response:
            sent.update(json)
            return Response()

        with patch.object(driver.httpx, "post", post):
            driver.faker_send(["https://mk-mkq/inbox"], 10, 4, "create", {}, 1000)
        self.assertEqual(sent["actors"], 1000)
        self.assertEqual(sent["count"], 10)


class ReportTests(unittest.TestCase):
    def test_inbound_report_shows_the_actors(self) -> None:
        lines = report.render_inbound({"actors": 1000, "drain": {}, "send": {}})
        self.assertIn("送り手の投稿者: 1000 人", lines)
        # #4138 より前の inbound.json は 1 人から送っている
        self.assertIn("送り手の投稿者: 1 人", report.render_inbound({"drain": {}, "send": {}}))


if __name__ == "__main__":
    unittest.main()
