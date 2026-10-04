"""Queue depth probes for the drivers under test.

Both drivers store queue state under the same Redis key convention:

- BullMQ (Misskey TS):     bull:<queue>:wait, bull:<queue>:active (LIST)
- mkq (BullMQ-compatible): bull:<queue>:wait, bull:<queue>:active (LIST)

The probe keeps the `kind` discriminator so a future driver with a
different key layout can be added without touching the bench drivers.
"""
from __future__ import annotations

from dataclasses import dataclass
from typing import Literal

import redis

DriverKind = Literal["bullmq", "mkq"]


@dataclass
class QueueProbe:
    name: str  # human label e.g. "ts" / "mkq"
    kind: DriverKind
    client: redis.Redis
    queue_name: str  # "deliver" / "inbox" / etc.

    def depth(self) -> int:
        """Pending + active job count for the configured queue."""
        # bullmq / mkq are wire-compatible
        wait = f"bull:{self.queue_name}:wait"
        active = f"bull:{self.queue_name}:active"
        delayed = f"bull:{self.queue_name}:delayed"
        return (
            self.client.llen(wait)
            + self.client.llen(active)
            + self.client.zcard(delayed)
        )

    def completed(self) -> int:
        """Approximate completed job count (driver-specific best effort)."""
        completed = f"bull:{self.queue_name}:completed"
        return int(self.client.zcard(completed))


def make_probe(name: str, kind: DriverKind, host: str, queue_name: str, port: int = 6379, db: int = 0) -> QueueProbe:
    client = redis.Redis(host=host, port=port, db=db, decode_responses=False)
    return QueueProbe(name=name, kind=kind, client=client, queue_name=queue_name)
