/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 4 PR-D: admin/{get-*-stats, show-*, accounts/find-by-email} read 系。
//
// 6 endpoint:
//   - admin/get-index-stats: object (= per-table index size 集計)
//   - admin/get-table-stats: object (= per-table row count 集計)
//   - admin/get-user-ips: 配列 (= user 別 IP 履歴、root token で root 自身)
//   - admin/show-moderation-logs: 配列
//   - admin/show-users: 配列 (= user list、limit + sort 等)
//   - admin/accounts/find-by-email: 不明 email で 4xx LCD
//
// 全 admin / moderator 権限要、root token を再利用する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

interface RootFixture {
  id: string;
  token: string;
  username: string;
}

test.describe('admin stats / show / find-by-email shape compat', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    resetRateLimit();
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  for (const endpoint of ['admin/get-index-stats', 'admin/get-table-stats']) {
    test(`${endpoint} returns object/array shape`, async ({ request }) => {
      const resp = await callApi(request, endpoint, { i: root.token });
      expect(resp.status()).toBe(200);
      // upstream は配列 (= [{table, ...}, ...]) で返すケースと map で返すケース
      // が混在するので両方許容する LCD。
      const body = await resp.json();
      expect(typeof body === 'object' && body !== null).toBe(true);
    });
  }

  test('admin/get-user-ips returns array shape', async ({ request }) => {
    const resp = await callApi(request, 'admin/get-user-ips', {
      i: root.token,
      userId: root.id,
    });
    expect(resp.status()).toBe(200);
    expect(Array.isArray(await resp.json())).toBe(true);
  });

  test('admin/show-moderation-logs returns array shape', async ({ request }) => {
    const resp = await callApi(request, 'admin/show-moderation-logs', {
      i: root.token,
      limit: 5,
    });
    expect(resp.status()).toBe(200);
    expect(Array.isArray(await resp.json())).toBe(true);
  });

  test('admin/show-users returns array shape', async ({ request }) => {
    const resp = await callApi(request, 'admin/show-users', {
      i: root.token,
      limit: 5,
      offset: 0,
      sort: '+createdAt',
      state: 'all',
      origin: 'combined',
    });
    expect(resp.status()).toBe(200);
    expect(Array.isArray(await resp.json())).toBe(true);
  });

  test('admin/accounts/find-by-email returns negative for unknown email', async ({ request }) => {
    // 不明 email は両 backend ともに 4xx か 200+空 のいずれか。LCD で吸収。
    const resp = await callApi(request, 'admin/accounts/find-by-email', {
      i: root.token,
      email: 'no-such-spec@example.invalid',
    });
    expect([200, 400, 404]).toContain(resp.status());
  });
});
