# Resource bench: mk-go (UDS) vs Misskey TS (just started)

Captured at: 2026-05-14T20:16:05+09:00

nginx は集計対象外。mk-go は compose.uds.yaml の長期稼働 stack、TS は compose.ts.yaml の起動直後 stack。

## mk-go (UDS stack)

| Component | Container | CPU% | Memory | Mem% | Block I/O | PIDs |
|---|---|---:|---|---:|---|---:|
| app | `mk-mkgo-1` | 5.38% | 47.05MiB / 31.19GiB | 0.15% | 1.25MB / 0B | 15 |
| db | `mk-postgres-1` | 3.63% | 41.32MiB / 31.19GiB | 0.13% | 25.8MB / 363MB | 8 |
| redis | `mk-valkey-1` | 3.94% | 39.05MiB / 31.19GiB | 0.12% | 140MB / 13.8GB | 6 |

## Misskey TS (bench stack)

| Component | Container | CPU% | Memory | Mem% | Block I/O | PIDs |
|---|---|---:|---|---:|---|---:|
| app | `mk-bench-ts-web-1` | 4.79% | 350MiB / 31.19GiB | 1.10% | 0B / 8.19kB | 59 |
| db | `mk-bench-ts-db-1` | 0.00% | 39.42MiB / 31.19GiB | 0.12% | 4.56MB / 107MB | 6 |
| redis | `mk-bench-ts-redis-1` | 0.91% | 6.004MiB / 31.19GiB | 0.02% | 1.58MB / 1.36MB | 6 |

## Storage

| Stack | Container | Writable layer | Data volume |
|---|---|---|---|
| mk-go | `mk-mkgo-1` | 0B (virtual 83.8MB) | (drive-files volume, see below) |
| mk-go | `mk-postgres-1` | 0B (virtual 276MB) | 79.4MB |
| mk-go | `mk-valkey-1` | 0B (virtual 42MB) | 61.5MB |
| TS | `mk-bench-ts-web-1` | 330B (virtual 2.46GB) | 4.0KB |
| TS | `mk-bench-ts-db-1` | 63B (virtual 276MB) | 71.6MB |
| TS | `mk-bench-ts-redis-1` | 0B (virtual 41.4MB) | 46.8KB |

## Image sizes (for reference)

```
REPO:TAG  SIZE
mk-mkgo:latest  80.0MB
mk-bench-ts-web:latest  2.3GB
postgres:16-alpine  263.3MB
redis:7-alpine  39.5MB
valkey/valkey:8-alpine  40.1MB
```
