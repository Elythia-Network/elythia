"""Queue depth probes for the drivers under test.

Both drivers store a queue as BullMQ keys (`<prefix>:wait` / `:active` LIST,
`:delayed` ZSET, `:meta` HASH), but under different prefixes (#4140):

- mkq (Elythia):      bull:<queue>
- BullMQ (Misskey TS): <redis prefix>:queue:<queue>:<queue>

Misskey passes `<redis prefix>:queue:<queue>` to BullMQ as its prefix, and
BullMQ appends the queue name again. The redis prefix is
`redisForJobQueue.prefix`, or `url.host` (host and port) when it is not set
(`packages/backend/src/config.ts`). Without a `redisForJobQueue` section,
Misskey uses the `redis` section instead.
"""
from __future__ import annotations

from dataclasses import dataclass
from typing import Literal

import redis

DriverKind = Literal["bullmq", "mkq"]

# tests/bench/queue/common/ts.yml の url (https://ts/) の url.host (ポートを含む)。
# 本家は redisForJobQueue.prefix を書かなければこれをキューの接頭辞に使う。
# ts.yml と食い違うと queue の meta が見つからず driver が止まるので、
# test_queue_probe が ts.yml と突き合わせている
TS_REDIS_PREFIX = "ts"


def queue_key_prefix(kind: DriverKind, queue_name: str, redis_prefix: str = "") -> str:
    """The Redis key prefix under which the driver keeps the queue."""
    if kind == "mkq":
        return f"bull:{queue_name}"
    if kind == "bullmq":
        if not redis_prefix:
            raise ValueError("a Misskey TS queue needs the redis prefix of the instance")
        return f"{redis_prefix}:queue:{queue_name}:{queue_name}"
    raise ValueError(f"unknown driver kind: {kind}")


@dataclass
class QueueProbe:
    name: str  # human label e.g. "ts" / "mkq"
    client: redis.Redis
    key_prefix: str  # e.g. "bull:inbox" / "ts:queue:inbox:inbox"

    def require_queue(self) -> None:
        """Fail unless the queue exists under the prefix.

        A wrong prefix reads every key as empty, so the depth stays 0 and the
        queue looks drained from the first poll (#4140). Both drivers write
        `<prefix>:meta` when a worker or producer opens the queue.
        """
        meta = f"{self.key_prefix}:meta"
        if not self.client.exists(meta):
            raise RuntimeError(f"[{self.name}] queue not found: {meta} does not exist")

    def depth(self) -> int:
        """Pending + active + delayed job count for the configured queue."""
        # 3 つを 1 つの MULTI で読む。別々に読むと、間に再試行のジョブが delayed
        # から active へ移ったとき (BullMQ は 1 つの Lua で移す) どちらでも数えず、
        # 残っているのに 0 と読んで drain と判定してしまう
        pipe = self.client.pipeline(transaction=True)
        pipe.llen(f"{self.key_prefix}:wait")
        pipe.llen(f"{self.key_prefix}:active")
        pipe.zcard(f"{self.key_prefix}:delayed")
        return int(sum(pipe.execute()))


def make_probe(name: str, kind: DriverKind, host: str, queue_name: str, redis_prefix: str = "",
               port: int = 6379, db: int = 0) -> QueueProbe:
    client = redis.Redis(host=host, port=port, db=db, decode_responses=False)
    probe = QueueProbe(name=name, client=client, key_prefix=queue_key_prefix(kind, queue_name, redis_prefix))
    probe.require_queue()
    return probe
