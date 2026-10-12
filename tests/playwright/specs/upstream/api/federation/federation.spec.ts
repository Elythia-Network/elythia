/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 3 #831: federation/* round-trip。
//
// Playwright stack は federation 設定なし (= local 単一 instance)。実 federation
// delivery / remote resolve は drop-in pytest が cover するので、本 spec は
// **API レベル shape** の drop-in 互換のみ確認する LCD strategy:
//   - federation/instances: list shape (= 配列)
//   - federation/show-instance: unknown host → 204 (両 backend 共通、#915 fix 済)
//   - federation/followers / following / users: unknown host → 空配列 shape
//   - federation/stats: 集計 4 field の shape (= 配列 + number)

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

test.describe('federation/* shape compat', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('federation/instances returns array shape', async ({ request }) => {
    // requireCredential: false で anonymous でも accept される。
    const resp = await callApi(request, 'federation/instances', {});
    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(Array.isArray(body)).toBe(true);
  });

  test('federation/show-instance returns 204 for unknown host', async ({ request }) => {
    // 該当 instance 行なし → upstream Misskey TS / mk-go (#915 fix 済) ともに
    // 204 No Content (= null 相当)。
    const resp = await callApi(request, 'federation/show-instance', {
      host: 'no-such-host-spec.invalid',
    });
    expect(resp.status()).toBe(204);
  });

  // 3 endpoint (followers / following / users) は同じ shape を返すため、
  // unknown host で空配列を返すこと だけを共通 assert として一括検証する。
  for (const endpoint of ['federation/followers', 'federation/following', 'federation/users']) {
    test(`${endpoint} returns empty array for unknown host`, async ({ request }) => {
      const resp = await callApi(request, endpoint, {
        host: 'no-such-host-spec.invalid',
      });
      expect(resp.status()).toBe(200);
      expect(await resp.json()).toEqual([]);
    });
  }

  test('federation/stats returns aggregated counter shape', async ({ request }) => {
    const resp = await callApi(request, 'federation/stats', { limit: 5 });
    expect(resp.status()).toBe(200);
    const body = (await resp.json()) as Record<string, unknown>;
    // upstream Misskey TS / mk-go 共通の必須 field
    expect(Array.isArray(body.topSubInstances)).toBe(true);
    expect(Array.isArray(body.topPubInstances)).toBe(true);
    expect(typeof body.otherFollowersCount).toBe('number');
    expect(typeof body.otherFollowingCount).toBe('number');
  });
});
