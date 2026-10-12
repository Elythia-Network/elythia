/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// #744 Phase 1: rate limit reset helper.
//
// mk-go の signup endpoint は IP base 1h 5 回 hardcoded
// (internal/server/middleware/ratelimit_defs.go)。spec 累積で 429 になる
// のを防ぐため、test の境界で Redis を flush して counter をゼロから
// 始める。後続 PR で signup endpoint を更に使う spec が増えても、各 spec
// の beforeAll で `resetRateLimit()` を呼べば独立性が保たれる。
//
// **`redis-cli` は使わない。** 以前は Dockerfile.runner が apt で
// redis-tools を入れていたが、この 1 コマンドのために apt 層を持つのは
// 割に合わない。CI ではシャードごとに runner image を作り直すので apt の
// 実行回数がシャード数だけ増え、実際に `apt-get update` がミラー待ちで
// 止まって 1 シャードが 20 分超えた (#2609)。
//
// FLUSHDB は引数なしの単一コマンドなので、RESP を直接書けば依存ゼロで済む。

import { execFileSync } from 'node:child_process';

const REDIS_HOST = process.env.MK_REDIS_HOST ?? 'redis';
const REDIS_PORT = Number(process.env.MK_REDIS_PORT ?? 6379);

// Node subprocess that speaks the two lines of RESP needed for FLUSHDB.
//
// 呼び出し側 (spec の beforeAll) が同期前提で書かれており、**呼び出しが
// 110 箇所ある**。async 化すると spec 全体に波及するので、ここは
// execFileSync で同期のまま保つ。子プロセス起動は redis-cli を exec して
// いたときと同じコストなので、実質の増分は無い。
const FLUSH_SCRIPT = `
const net = require('node:net');
const sock = net.createConnection({ host: process.argv[1], port: Number(process.argv[2]) });
sock.on('connect', () => sock.write('*1\\r\\n$7\\r\\nFLUSHDB\\r\\n'));
sock.on('data', (buf) => {
  const reply = buf.toString('utf8');
  sock.end();
  if (!reply.startsWith('+OK')) { console.error(reply.trim()); process.exit(1); }
  process.exit(0);
});
sock.on('error', (err) => { console.error(err.message); process.exit(1); });
`;

// resetRateLimit issues a Redis FLUSHDB against the test stack's redis.
// Throws when the command is unreachable so callers fail fast (= globalSetup
// or a spec's beforeAll) instead of letting later 429s mask the root cause.
export function resetRateLimit(): void {
  execFileSync(process.execPath, ['-e', FLUSH_SCRIPT, REDIS_HOST, String(REDIS_PORT)], {
    stdio: 'pipe',
    timeout: 5_000,
  });
}
