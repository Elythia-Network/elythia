"""Tests for registering the bench's remote hosts before measuring (#4140)."""

from __future__ import annotations

import io
import unittest
from typing import Any
from unittest.mock import patch

from test_inbound_actors import RecordingCursor, load

seed = load("seed")


class FixtureHostTests(unittest.TestCase):
    def test_hosts_cover_the_followers_the_receiver_and_the_faker(self) -> None:
        self.assertEqual(seed.fixture_hosts("http://blackhole", "faker"), ["blackhole", "faker"])
        # 照合用の受信機へ配るときも、follower の user の host は blackhole のまま
        self.assertEqual(
            seed.fixture_hosts("http://deliveryreceiver-ts", "faker"),
            ["blackhole", "deliveryreceiver-ts", "faker"],
        )

    def test_hosts_without_the_faker(self) -> None:
        self.assertEqual(seed.fixture_hosts("http://blackhole", ""), ["blackhole"])

    def test_rows_register_every_host_as_already_known(self) -> None:
        cur = RecordingCursor((False, False))
        seed.insert_fixture_instance_rows(cur, ["blackhole", "faker"])
        self.assertEqual(len(cur.calls), 1)
        kind, sql, params = cur.calls[0]
        self.assertEqual(kind, "many")
        self.assertIn("INSERT INTO instance", sql)
        self.assertIn('"firstRetrievedAt"', sql)
        # 2 回目の seed で落ちない
        self.assertIn("ON CONFLICT DO NOTHING", sql)
        self.assertEqual([p[1] for p in params], ["blackhole", "faker"])
        self.assertEqual(len({p[0] for p in params}), 2)


class RateLimitTests(unittest.TestCase):
    def test_base_role_rate_limit_factor_is_zero(self) -> None:
        cur = RowsCursor([(1,)])
        seed.lift_rate_limit_rows(cur)
        self.assertIn("UPDATE meta SET policies = policies ||", cur.sql)
        self.assertIn('"rateLimitFactor": 0', cur.sql)

    def test_missing_meta_row_is_refused(self) -> None:
        with self.assertRaises(RuntimeError):
            seed.lift_rate_limit_rows(RowsCursor([]))


class RowsCursor:
    def __init__(self, rows: list[tuple[int]]) -> None:
        self.rows = rows
        self.sql = ""

    def execute(self, sql: str, params: Any = None) -> None:
        self.sql = sql

    def fetchall(self) -> list[tuple[int]]:
        return self.rows


class SeedMainTests(unittest.TestCase):
    def test_main_registers_the_hosts_on_every_stack(self) -> None:
        self.assertEqual(self.run_main(False), {"mkq": ["blackhole", "faker"], "ts": ["blackhole", "faker"]})

    def test_main_registers_each_stacks_delivery_receiver(self) -> None:
        self.assertEqual(self.run_main(True), {
            "mkq": ["blackhole", "deliveryreceiver-mkq", "faker"],
            "ts": ["blackhole", "deliveryreceiver-ts", "faker"],
        })

    def run_main(self, delivery: bool) -> dict[str, list[str]]:
        registered: dict[str, list[str]] = {}
        lifted: list[str] = []

        class Response:
            def raise_for_status(self) -> None:
                pass

            def json(self) -> dict[str, Any]:
                return {"createdNote": {"id": "n1"}}

        class Client:
            def __init__(self, *args: Any, **kwargs: Any) -> None:
                pass

            def __enter__(self) -> "Client":
                return self

            def __exit__(self, *args: Any) -> None:
                pass

            def post(self, *args: Any, **kwargs: Any) -> Response:
                return Response()

        patches = {
            "wait_health": lambda url: None,
            "fetch_faker_actors": lambda count: {"publicKeyPem": "PEM", "actors": [
                {"id": "https://faker/users/bench-sender-0", "username": "bench-sender-0",
                 "inbox": "https://faker/users/bench-sender-0/inbox",
                 "keyId": "https://faker/users/bench-sender-0#main-key"}]},
            "create_admin": lambda http, username, password: "token",
            "fetch_local_user_id": lambda http, token: "user",
            "enable_federation": lambda stack, db_host: None,
            "lift_rate_limits": lambda stack, db_host: lifted.append(stack),
            "insert_fixture_followers": lambda *args: [],
            "insert_faker_actors": lambda stack, db_host, faker: 1,
            "insert_fixture_instances": lambda stack, db_host, hosts: registered.__setitem__(stack, hosts),
            "FAKER_HOST": "faker",
            "DELIVERY_MODE": delivery,
        }
        with patch.multiple(seed, **patches), \
                patch.object(seed.httpx, "Client", Client, create=True), \
                patch("builtins.open", lambda *a, **k: io.StringIO()), \
                patch("sys.stdout", io.StringIO()):
            self.assertEqual(seed.main(), 0)
        self.assertEqual(lifted, ["mkq", "ts"])
        return registered


if __name__ == "__main__":
    unittest.main()
