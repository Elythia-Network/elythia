/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// テストのスタックの PostgreSQL に直接つなぐ (#4093)。API では作れない行 (リモートの
// サーバー・利用者・絵文字) を、テストの間だけ入れて、終わったら消すために使う。
// スタックの起動時に入れると、ほかの spec の画面 (連合の管理画面など) にも見えてしまう。
//
// 接続先は compose.yml の postgres。runner は同じネットワークにいる。

import { randomBytes } from 'node:crypto';
import { Client } from 'pg';

/** Runs fn with a client connected to the test stack's database, and closes it. */
export async function withDb<T>(fn: (db: Client) => Promise<T>): Promise<T> {
  const db = new Client({
    host: process.env.MK_TEST_DB_HOST ?? 'postgres',
    port: Number(process.env.MK_TEST_DB_PORT ?? 5432),
    user: process.env.MK_TEST_DB_USER ?? 'misskey',
    password: process.env.MK_TEST_DB_PASS ?? 'testpass',
    database: process.env.MK_TEST_DB_NAME ?? 'misskey',
  });
  await db.connect();
  try {
    return await fn(db);
  } finally {
    await db.end();
  }
}

/** A random id for a row a spec inserts (16 chars of [0-9a-z], like aidx). */
export function rowId(): string {
  return BigInt(`0x${randomBytes(10).toString('hex')}`).toString(36).padStart(16, '0').slice(-16);
}
