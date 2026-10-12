/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 3 #835: utility root endpoint shape の round-trip。
//
// Misskey root (= /api/<endpoint>) の utility 系 endpoint の shape を
// 両 backend で固定する。値そのものは集計遅延 / 環境依存があるので
// **status + 必須 field の存在 + 配列要素 type** のみ assert する LCD
// strategy。
//
// scope:
//   - stats: { notesCount, originalNotesCount, usersCount, ... }
//   - server-info: server hardware metadata (= cpu / memory shape)
//   - get-online-users-count: { count: number }
//   - get-avatar-decorations: array (= decoration list)
//   - pinned-users: UserLite array
//   - retention: chart-like number array shape
//
// scope 外:
//   - reset-password / fetch-rss は SMTP / mock RSS server 必要、別 PR scope

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

test.describe('utility root endpoints shape', () => {
  // 4 auth-required endpoint で 4 回 signup すると IP rate limit (5 / 1h) を
  // 圧迫する。1 user を beforeAll で作って共有することで signup 数を 1 に
  // 抑える (#909 review N1)。anonymous endpoint (get-avatar-decorations /
  // pinned-users) は token 不要なので user 不要。
  let token: string | null = null;

  test.beforeAll(async ({ request }) => {
    resetRateLimit();
    const me = await signupUser(request, randomUsername('utl'));
    token = me.token;
  });

  test('stats returns counter shape', async ({ request }) => {
    const resp = await callApi(request, 'stats', { i: token });
    expect(resp.status()).toBe(200);
    const body = (await resp.json()) as Record<string, unknown>;
    // upstream Misskey TS / mk-go 共通の必須 field
    for (const key of [
      'notesCount',
      'originalNotesCount',
      'usersCount',
      'originalUsersCount',
      'instances',
    ]) {
      expect(typeof body[key], `stats.${key} should be number`).toBe('number');
    }
  });

  test('server-info returns hardware metadata', async ({ request }) => {
    const resp = await callApi(request, 'server-info', { i: token });
    expect(resp.status()).toBe(200);
    const body = (await resp.json()) as Record<string, unknown>;
    // 両 backend で必ず存在する hardware metadata field 群。
    // 個別 value (= cpu cores / memory bytes) は環境依存なので type のみ。
    expect(body.machine).toBeDefined();
    expect(body.cpu).toBeDefined();
    expect(body.mem).toBeDefined();
    expect(body.fs).toBeDefined();
  });

  test('get-online-users-count returns numeric count', async ({ request }) => {
    const resp = await callApi(request, 'get-online-users-count', { i: token });
    expect(resp.status()).toBe(200);
    const body = (await resp.json()) as { count?: unknown };
    expect(typeof body.count).toBe('number');
  });

  test('get-avatar-decorations returns array', async ({ request }) => {
    // anonymous でも accept されるはず (= meta-like read endpoint)。
    const resp = await callApi(request, 'get-avatar-decorations', {});
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(Array.isArray(body)).toBe(true);
    // 内容は test 環境で空のことが多いので shape のみ assert (= 配列であること)。
  });

  test('pinned-users returns UserLite array', async ({ request }) => {
    const resp = await callApi(request, 'pinned-users', {});
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(Array.isArray(body)).toBe(true);
    // pinnedUsers は admin/meta で設定される、test 環境では未設定で空配列が
    // 一般的。array であることのみ assert (= shape compat)。
  });

  test('retention returns aggregated array shape', async ({ request }) => {
    const resp = await callApi(request, 'retention', { i: token });
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    // retention は { date, users, data: { day_offset_X: count } }[] の
    // 配列 (= upstream / mk-go 共通)。test 環境では空配列のことが多いので
    // array 型のみ assert。
    expect(Array.isArray(body)).toBe(true);
  });
});
