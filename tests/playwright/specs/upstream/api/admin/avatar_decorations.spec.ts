/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 4 PR-D: admin/avatar-decorations/* CRUD round-trip。
//
// 4 endpoint: list / create / update / delete。
// 全 admin 権限要、root token を再利用する。

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

interface Decoration {
  id: string;
  name?: string;
  description?: string;
  url?: string;
}

test.describe('admin/avatar-decorations/* CRUD round-trip', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    resetRateLimit();
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test('create → list で含まれる → update → delete round-trip', async ({ request }) => {
    const name = `spec_deco_${randomUUID()}`;

    // 1. create (両 backend ともに 200 + decoration object を返す)
    const createResp = await callApi(request, 'admin/avatar-decorations/create', {
      i: root.token,
      name,
      description: 'phase4 spec',
      url: 'https://example.invalid/deco.png',
      roleIdsThatCanBeUsedThisDecoration: [],
    });
    expect(createResp.status()).toBe(200);

    // 2. list で含まれる
    const listResp = await callApi(request, 'admin/avatar-decorations/list', {
      i: root.token,
    });
    expect(listResp.status()).toBe(200);
    const list = (await listResp.json()) as Decoration[];
    expect(Array.isArray(list)).toBe(true);
    const found = list.find((d) => d.name === name);
    expect(found, 'created decoration should appear in list').toBeDefined();
    const decoId = found!.id;

    // 3. update で description + 空 roleIds 配列。#931 fix 後は空配列でも
    // 両 backend で 204 (mk-go は pq.StringArray でラップして '{}' を保存)。
    const updResp = await callApi(request, 'admin/avatar-decorations/update', {
      i: root.token,
      id: decoId,
      name,
      description: 'updated by spec',
      url: 'https://example.invalid/deco.png',
      roleIdsThatCanBeUsedThisDecoration: [],
    });
    expect(updResp.status()).toBe(204);

    // 4. delete (両 backend ともに 204 No Content)
    const delResp = await callApi(request, 'admin/avatar-decorations/delete', {
      i: root.token,
      id: decoId,
    });
    expect(delResp.status()).toBe(204);

    // 5. list 再取得で消えている
    const listAfter = await callApi(request, 'admin/avatar-decorations/list', {
      i: root.token,
    });
    expect(listAfter.status()).toBe(200);
    const listAfterBody = (await listAfter.json()) as Decoration[];
    expect(listAfterBody.find((d) => d.id === decoId)).toBeFalsy();
  });
});
