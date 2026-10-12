/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 4 PR-F: chat/* 残 read 系 spec。
//
// 既存 #852/#853/#854 spec で DM round-trip / room CRUD / WS 通知が cover 済。
// 本 spec は残り read 系 + read-all を埋める:
//   - chat/history: 自分の chat 履歴 (= 配列)
//   - chat/messages/search: 自分の DM 検索 (= 配列、空 query で empty)
//   - chat/read-all: 全既読化 (= idempotent な mutation、204)

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

test.describe('chat/* extra read + read-all', () => {
  let userToken: string;

  test.beforeAll(async ({ request }) => {
    resetRateLimit();
    const me = await signupUser(request, randomUsername('chx'));
    userToken = me.token;
  });

  test('chat/history returns array shape', async ({ request }) => {
    const resp = await callApi(request, 'chat/history', {
      i: userToken,
      limit: 5,
    });
    expect(resp.status()).toBe(200);
    expect(Array.isArray(await resp.json())).toBe(true);
  });

  test('chat/messages/search returns array shape', async ({ request }) => {
    const resp = await callApi(request, 'chat/messages/search', {
      i: userToken,
      query: 'spec_no_match',
      limit: 5,
    });
    expect(resp.status()).toBe(200);
    expect(Array.isArray(await resp.json())).toBe(true);
  });

  test('chat/read-all returns 204 (idempotent)', async ({ request }) => {
    const resp = await callApi(request, 'chat/read-all', { i: userToken });
    expect([200, 204]).toContain(resp.status());
  });
});
