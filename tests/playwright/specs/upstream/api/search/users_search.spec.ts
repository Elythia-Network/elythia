/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 2 #828: users/search の round-trip。
//
// upstream Misskey TS と mk-go は両方とも:
//   - users/search { query } で username / displayName を partial match
//   - response は UserDetailed array
//   - origin (local / remote / combined) で scope を絞れる (#763)
//
// 本 spec は両 backend 共通で:
//   1. unique username で user A signup
//   2. users/search { query: <unique prefix> } で hit、id が A.id と一致
//
// 注意: users/search-by-username-and-host は本 spec scope から外す。
// playwright stack で TS/mk-go 間の input 解釈に drift (= 同 username で
// TS は空配列、mk-go は hit) があるため、shape 互換 spec として round-trip
// 化が難しい。drift は別 issue で揃える方向 (= host=null の semantics
// 整合)。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

interface SearchedUser {
  id: string;
  username: string;
}

test.describe('search: users/search round-trip', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('search by username substring returns the user', async ({ request }) => {
    const me = await signupUser(request, randomUsername('usA'));

    const resp = await callApi(request, 'users/search', {
      i: me.token,
      query: me.username,
      limit: 10,
    });
    expect(resp.status()).toBe(200);
    const list = (await resp.json()) as SearchedUser[];
    expect(Array.isArray(list)).toBe(true);
    const hit = list.find((u) => u.id === me.id);
    expect(hit).toBeDefined();
    expect(hit!.username).toBe(me.username);
  });
});
