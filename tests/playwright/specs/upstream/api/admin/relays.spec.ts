/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 4 PR-E: admin/relays/* spec。
//
// 3 endpoint: list / add / remove。
// list は配列、add/remove は 仮想 inbox URL で round-trip。実際の relay
// 通信は federation 環境必須なので scope 外、handler が ticket を作成 /
// 削除する shape のみ verify する。

import { randomUUID } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

interface RootFixture {
  id: string;
  token: string;
  username: string;
}

interface Relay {
  inbox: string;
  status?: string;
}

test.describe('admin/relays/* CRUD', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    resetRateLimit();
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test('list → add → list で含まれる → remove round-trip', async ({ request }) => {
    const inbox = `https://example.invalid/${randomUUID()}/inbox`;

    // 1. add (= 仮想 inbox)
    // mk-go: c.JSON(StatusOK, relay) / TS: handler returns relay → Endpoint base 200。
    // 両 backend ともに 200 + relay object を返すので strict。
    const addResp = await callApi(request, 'admin/relays/add', {
      i: root.token,
      inbox,
    });
    expect(addResp.status()).toBe(200);

    // 2. list で含まれる
    const listResp = await callApi(request, 'admin/relays/list', {
      i: root.token,
    });
    expect(listResp.status()).toBe(200);
    const list = (await listResp.json()) as Relay[];
    expect(Array.isArray(list)).toBe(true);
    expect(list.find((r) => r.inbox === inbox)).toBeDefined();

    // 3. remove
    const removeResp = await callApi(request, 'admin/relays/remove', {
      i: root.token,
      inbox,
    });
    expect([200, 204]).toContain(removeResp.status());

    // 4. list 再取得で消えている
    const listAfter = await callApi(request, 'admin/relays/list', {
      i: root.token,
    });
    expect(listAfter.status()).toBe(200);
    const listAfterBody = (await listAfter.json()) as Relay[];
    expect(listAfterBody.find((r) => r.inbox === inbox)).toBeFalsy();
  });
});
