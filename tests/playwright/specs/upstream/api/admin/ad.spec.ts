/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 4 PR-D: admin/ad/* CRUD round-trip。
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

interface Ad {
  id: string;
  url?: string;
  imageUrl?: string;
  memo?: string;
}

test.describe('admin/ad/* CRUD round-trip', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    resetRateLimit();
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test('list (empty/seeded) → create → list で含まれる → update → delete round-trip', async ({
    request,
  }) => {
    const memo = `spec_ad_${randomUUID()}`;
    const expiresAt = Date.now() + 60 * 60 * 1000; // 1時間後

    // 1. create (両 backend ともに 200 + ad object を返す)
    const createResp = await callApi(request, 'admin/ad/create', {
      i: root.token,
      url: 'https://example.invalid/ad',
      imageUrl: 'https://example.invalid/ad.png',
      place: 'square',
      priority: 'middle',
      ratio: 1,
      dayOfWeek: 0,
      memo,
      expiresAt,
      startsAt: Date.now(),
    });
    expect(createResp.status()).toBe(200);

    // 2. list で memo が含まれる
    const listResp = await callApi(request, 'admin/ad/list', { i: root.token });
    expect(listResp.status()).toBe(200);
    const list = (await listResp.json()) as Ad[];
    expect(Array.isArray(list)).toBe(true);
    const found = list.find((a) => a.memo === memo);
    expect(found, 'created ad should appear in list').toBeDefined();
    const adId = found!.id;

    // 3. update (両 backend ともに 204 No Content を返す)
    const updResp = await callApi(request, 'admin/ad/update', {
      i: root.token,
      id: adId,
      url: 'https://example.invalid/ad-updated',
      imageUrl: 'https://example.invalid/ad-updated.png',
      place: 'square',
      priority: 'middle',
      ratio: 2,
      dayOfWeek: 0,
      memo: `${memo}_updated`,
      expiresAt,
      startsAt: Date.now(),
    });
    expect(updResp.status()).toBe(204);

    // 4. delete (両 backend ともに 204 No Content を返す)
    const delResp = await callApi(request, 'admin/ad/delete', {
      i: root.token,
      id: adId,
    });
    expect(delResp.status()).toBe(204);

    // 5. list 再取得で消えている
    const listAfter = await callApi(request, 'admin/ad/list', { i: root.token });
    expect(listAfter.status()).toBe(200);
    const listAfterBody = (await listAfter.json()) as Ad[];
    expect(listAfterBody.find((a) => a.id === adId)).toBeFalsy();
  });
});
