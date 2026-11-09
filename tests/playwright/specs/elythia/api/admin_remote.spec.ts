/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// リモートの行が要る管理画面の独自の API を叩き、応答を /api.json の宣言で照合する
// (#4093)。リモートの絵文字・消えたサーバー・その利用者は API では作れないので、
// テストの間だけ DB に入れて、終わったら消す (fixtures/db.ts)。

import { expect, test } from '@playwright/test';
import { callChecked } from '../../../fixtures/api_schema';
import { rootFixture } from '../../../fixtures/admin_table';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { isTsBackend } from '../../../fixtures/backend';
import { rowId, withDb } from '../../../fixtures/db';
import { resetRateLimit } from '../../../fixtures/rate_limit';

interface RemoteEmojiMeta { fetched: boolean; reason?: string; emojiId: string; name: string; host: string }
interface CleanResult { host: string; removedFollowers: number; removedFollowing: number; removedFollowRequests: number; remaining: number }

test.describe('admin: the Elythia endpoints that need remote rows answer as declared', () => {
  test.skip(isTsBackend, 'ここで叩く口は Elythia 独自');

  test.beforeAll(() => {
    resetRateLimit();
  });

  test('the metadata of a remote emoji, by id and by name and host', async ({ request }) => {
    const root = rootFixture();
    // instance の行を入れないので、相手のソフトウェアが分からず、取りに行かずに unsupported で返る
    const host = `pw-${rowId()}.invalid`;
    const emoji = { id: rowId(), name: `pw_remote_${rowId()}` };
    await withDb((db) => db.query(
      'INSERT INTO emoji (id, name, host, "originalUrl", "updatedAt") VALUES ($1, $2, $3, $4, now())',
      [emoji.id, emoji.name, host, `https://${host}/emoji.png`],
    ));
    try {
      const byId = await callChecked<RemoteEmojiMeta>(request, 'admin/emoji/fetch-remote-meta', { i: root.token, emojiId: emoji.id });
      expect(byId).toMatchObject({ fetched: false, reason: 'unsupported', emojiId: emoji.id, name: emoji.name, host });
      const byName = await callChecked<RemoteEmojiMeta>(request, 'admin/emoji/fetch-remote-meta', { i: root.token, name: emoji.name, host });
      expect(byName.emojiId).toBe(emoji.id);
    } finally {
      await withDb((db) => db.query('DELETE FROM emoji WHERE id = $1', [emoji.id]));
    }
  });

  test('clean a gone instance: the follows with its users go', async ({ request }) => {
    const root = rootFixture();
    const local = await signupUser(request, randomUsername('admgo'));
    const host = `pw-${rowId()}.invalid`;
    const remote = { id: rowId(), username: `gone${rowId()}` };
    const instanceId = rowId();
    const followingId = rowId();
    // 行を入れる途中で落ちても消すよう、入れるのも try の中で行う。goneSuspended の行が残ると、
    // 連合の管理画面の「消えたサーバーは無い」を確かめる spec (admin_federation_rules) が落ちる
    try {
      await withDb(async (db) => {
        await db.query('INSERT INTO instance (id, host, "firstRetrievedAt", "suspensionState", "followersCount") VALUES ($1, $2, now(), $3, 1)', [instanceId, host, 'goneSuspended']);
        await db.query('INSERT INTO "user" (id, username, "usernameLower", host, "followersCount") VALUES ($1, $2, $3, $4, 1)', [remote.id, remote.username, remote.username.toLowerCase(), host]);
        await db.query('INSERT INTO following (id, "followerId", "followeeId", "followeeHost") VALUES ($1, $2, $3, $4)', [followingId, local.id, remote.id, host]);
        // フォローを API で作ったときと同じく、数も 1 ずつ足しておく。片付けが数を戻すことも確かめる
        await db.query('UPDATE "user" SET "followingCount" = "followingCount" + 1 WHERE id = $1', [local.id]);
      });
      const cleaned = await callChecked<CleanResult>(request, 'admin/federation/clean-gone-instance', { i: root.token, host });
      expect(cleaned).toMatchObject({ host, removedFollowers: 0, removedFollowing: 1, removedFollowRequests: 0, remaining: 0 });
      await withDb(async (db) => {
        expect((await db.query('SELECT 1 FROM following WHERE id = $1', [followingId])).rowCount).toBe(0);
        const counts = await db.query('SELECT "followingCount" FROM "user" WHERE id = $1', [local.id]);
        expect(counts.rows[0].followingCount).toBe(0);
      });
    } finally {
      await withDb(async (db) => {
        await db.query('DELETE FROM following WHERE id = $1', [followingId]);
        await db.query('DELETE FROM "user" WHERE id = $1', [remote.id]);
        await db.query('DELETE FROM instance WHERE id = $1', [instanceId]);
      });
    }
  });
});
