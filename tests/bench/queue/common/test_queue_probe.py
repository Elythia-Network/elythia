"""Tests for reading each driver's queue under its own key prefix (#4140)."""

from __future__ import annotations

import importlib
import os
import re
import sys
import types
import unittest
from typing import Any
from unittest.mock import patch
from urllib.parse import urlparse

HERE = os.path.dirname(os.path.abspath(__file__))


class FakeRedis:
    def __init__(self, keys: dict[str, Any] | None = None, **kwargs: Any) -> None:
        self.keys = keys or {}
        self.kwargs = kwargs

    def exists(self, key: str) -> int:
        return int(key in self.keys)

    def llen(self, key: str) -> int:
        return len(self.keys.get(key, []))

    def zcard(self, key: str) -> int:
        return len(self.keys.get(key, []))

    def pipeline(self, transaction: bool = True) -> "FakePipeline":
        return FakePipeline(self, transaction)


class FakePipeline:
    def __init__(self, client: FakeRedis, transaction: bool) -> None:
        self.client = client
        self.transaction = transaction
        self.queued: list[int] = []

    def llen(self, key: str) -> None:
        self.queued.append(self.client.llen(key))

    def zcard(self, key: str) -> None:
        self.queued.append(self.client.zcard(key))

    def execute(self) -> list[int]:
        # MULTI で読まないと、間にジョブが移ったときに数え落とす
        if not self.transaction:
            raise AssertionError("depth must read the keys in one MULTI")
        return self.queued


def load_probe() -> Any:
    redis_stub = types.ModuleType("redis")
    redis_stub.Redis = FakeRedis
    with patch.dict(sys.modules, {"redis": redis_stub}):
        sys.modules.pop("queue_probe", None)
        return importlib.import_module("queue_probe")


probe_module = load_probe()


def read(name: str) -> str:
    with open(os.path.join(HERE, name)) as f:
        return f.read()


def top_level(yml: str, key: str) -> str | None:
    match = re.search(rf"^{key}:[ \t]*(\S+)", yml, re.MULTILINE)
    return match.group(1) if match else None


class KeyPrefixTests(unittest.TestCase):
    def test_mkq_keeps_the_queue_under_bull(self) -> None:
        self.assertEqual(probe_module.queue_key_prefix("mkq", "inbox"), "bull:inbox")

    def test_misskey_repeats_the_queue_name_after_its_redis_prefix(self) -> None:
        # 実物の本家のスタックで見たキー: ts:queue:inbox:inbox:wait
        self.assertEqual(probe_module.queue_key_prefix("bullmq", "inbox", "ts"), "ts:queue:inbox:inbox")
        self.assertEqual(probe_module.queue_key_prefix("bullmq", "deliver", "ts"), "ts:queue:deliver:deliver")

    def test_misskey_without_a_redis_prefix_is_refused(self) -> None:
        with self.assertRaises(ValueError):
            probe_module.queue_key_prefix("bullmq", "inbox")

    def test_unknown_kind_is_refused(self) -> None:
        with self.assertRaises(ValueError):
            probe_module.queue_key_prefix("asynq", "inbox")  # type: ignore[arg-type]


class ProbeTests(unittest.TestCase):
    def test_depth_counts_waiting_active_and_delayed_jobs_under_the_prefix(self) -> None:
        client = FakeRedis({
            "ts:queue:inbox:inbox:wait": [1, 2, 3],
            "ts:queue:inbox:inbox:active": [4],
            "ts:queue:inbox:inbox:delayed": [5, 6],
            "ts:queue:inbox:inbox:completed": [7, 8, 9, 10],
            # 接頭辞の違う同名のキューは数えない
            "bull:inbox:wait": [11, 12],
        })
        probe = probe_module.QueueProbe(name="ts", client=client, key_prefix="ts:queue:inbox:inbox")
        self.assertEqual(probe.depth(), 6)

    def test_require_queue_fails_when_the_meta_key_is_missing(self) -> None:
        probe = probe_module.QueueProbe(name="ts", client=FakeRedis({"bull:inbox:meta": {}}),
                                        key_prefix="ts:queue:inbox:inbox")
        with self.assertRaisesRegex(RuntimeError, "ts:queue:inbox:inbox:meta"):
            probe.require_queue()

    def test_require_queue_passes_when_the_meta_key_exists(self) -> None:
        probe = probe_module.QueueProbe(name="ts", client=FakeRedis({"ts:queue:inbox:inbox:meta": {}}),
                                        key_prefix="ts:queue:inbox:inbox")
        probe.require_queue()

    def test_make_probe_refuses_a_queue_it_cannot_find(self) -> None:
        with patch.object(probe_module.redis, "Redis", lambda **kwargs: FakeRedis({"bull:inbox:meta": {}})):
            self.assertEqual(probe_module.make_probe("mkq", "mkq", "redis-mkq", "inbox").key_prefix, "bull:inbox")
            with self.assertRaises(RuntimeError):
                probe_module.make_probe("ts", "bullmq", "redis-ts", "inbox", "ts")


class MisskeyStackTests(unittest.TestCase):
    def test_redis_prefix_is_the_host_of_the_url_in_ts_yml(self) -> None:
        ts = read("ts.yml")
        # 本家は redisForJobQueue.prefix を書かなければ url.host を使う (config.ts)
        self.assertNotRegex(ts, r"(?m)^\s+prefix:")
        url = top_level(ts, "url")
        self.assertIsNotNone(url)
        # url.host はポートを含む (https://ts:3000/ なら ts:3000)
        self.assertEqual(urlparse(url).netloc, probe_module.TS_REDIS_PREFIX)

    def test_ts_yml_lifts_the_queue_rate_limits(self) -> None:
        ts = read("ts.yml")
        for key in ("inboxJobPerSec", "deliverJobPerSec"):
            value = top_level(ts, key)
            self.assertIsNotNone(value, key)
            # 本家の既定 (32 / 128) やベンチの件数で効く大きさでは、上限の値を測ってしまう
            self.assertGreaterEqual(int(value), 100000, key)

    def test_misskey_runs_in_production(self) -> None:
        compose = read("../compose.yml")
        block = re.search(r"(?ms)^  app-ts:\n(.*?)(?=^  \S)", compose)
        self.assertIsNotNone(block)
        self.assertRegex(block.group(1), r"(?m)^\s+NODE_ENV: production$")


if __name__ == "__main__":
    unittest.main()
