/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 4 PR-E: admin/invite/* spec。
//
// 2 endpoint:
//   - admin/invite/list: 配列 (= 全 invite ticket 一覧)
//   - admin/invite/create: ticket 作成 → list で含まれる

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

interface RootFixture {
  id: string;
  token: string;
  username: string;
}

interface InviteTicket {
  id: string;
  code: string;
}

test.describe('admin/invite/* round-trip', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    resetRateLimit();
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test('admin/invite/create → admin/invite/list で含まれる', async ({ request }) => {
    // 1. create (= count 枚の ticket を発行、1 で十分)
    const createResp = await callApi(request, 'admin/invite/create', {
      i: root.token,
      count: 1,
    });
    expect(createResp.status()).toBe(200);
    const created = (await createResp.json()) as InviteTicket[];
    expect(Array.isArray(created)).toBe(true);
    expect(created.length).toBeGreaterThanOrEqual(1);
    const ticketCode = created[0].code;

    // 2. list で含まれる
    const listResp = await callApi(request, 'admin/invite/list', {
      i: root.token,
      limit: 50,
    });
    expect(listResp.status()).toBe(200);
    const list = (await listResp.json()) as InviteTicket[];
    expect(Array.isArray(list)).toBe(true);
    expect(list.find((t) => t.code === ticketCode)).toBeDefined();
  });
});
