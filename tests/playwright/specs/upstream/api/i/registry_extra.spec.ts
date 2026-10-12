/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 4 PR-F: i/registry/* 残 spec。
//
// 既存 #906 spec で registry/get-all / get / get-unsecure / set / remove が
// cover 済。本 spec は残り 2 endpoint:
//   - i/registry/keys: 配列 (= scope 内の key 一覧)
//   - i/registry/scopes-with-domain: 配列 (= 全 scope 一覧)

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

test.describe('i/registry/* keys + scopes', () => {
  let userToken: string;

  test.beforeAll(async ({ request }) => {
    resetRateLimit();
    const me = await signupUser(request, randomUsername('reg'));
    userToken = me.token;
  });

  test('i/registry/keys returns array shape', async ({ request }) => {
    const resp = await callApi(request, 'i/registry/keys', {
      i: userToken,
      scope: ['client', 'base'],
    });
    expect(resp.status()).toBe(200);
    expect(Array.isArray(await resp.json())).toBe(true);
  });

  test('i/registry/scopes-with-domain returns array shape', async ({ request }) => {
    const resp = await callApi(request, 'i/registry/scopes-with-domain', {
      i: userToken,
    });
    expect(resp.status()).toBe(200);
    expect(Array.isArray(await resp.json())).toBe(true);
  });
});
