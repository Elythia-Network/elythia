/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 3 #830: charts/* response shape の round-trip。
//
// upstream Misskey TS と mk-go は両方とも 12 chart endpoint を提供:
//   instance-wide:
//     - charts/notes / users / drive / federation / ap-request / active-users
//     - charts/instance (= host required)
//   per-user:
//     - charts/user/{notes, drive, following, pv, reactions} (= userId required)
//
// 各 endpoint は { span: 'hour'|'day', limit: N } を取り、unflatten した
// nested object を返す:
//   {
//     "field1": { "subfield": [n1, n2, ..., nN] },
//     "field2": [n1, n2, ..., nN],
//     ...
//   }
// 各 leaf は length = limit の number 配列。値は集計遅延があり deterministic
// に固定できないので、本 spec は **shape (= status / 各 leaf 配列の長さ)**
// のみ assert する。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

const LIMIT = 5;

// flattenLeafArrays returns all leaf values that look like number arrays from
// chart の unflatten 済 nested object。chart の field 構成は backend / version で
// 変動するので key 名は固定 assert せず、すべての number[] leaf が limit 件か
// を確認する LCD。
function flattenLeafArrays(obj: unknown): number[][] {
  const out: number[][] = [];
  if (Array.isArray(obj)) {
    if (obj.every((x) => typeof x === 'number')) {
      out.push(obj as number[]);
    }
    return out;
  }
  if (obj && typeof obj === 'object') {
    for (const v of Object.values(obj)) {
      out.push(...flattenLeafArrays(v));
    }
  }
  return out;
}

async function assertChartShape(
  request: import('@playwright/test').APIRequestContext,
  endpoint: string,
  body: Record<string, unknown>,
): Promise<void> {
  const resp = await callApi(request, endpoint, body);
  expect(resp.status(), `${endpoint} should return 200`).toBe(200);
  const data = (await resp.json()) as Record<string, unknown>;
  const leaves = flattenLeafArrays(data);
  expect(leaves.length, `${endpoint} should have at least 1 number[] leaf`).toBeGreaterThan(0);
  // flattenLeafArrays が `every typeof === 'number'` で filter 済なので
  // 各要素の type 再 assert は不要 (= dead code 削除、length のみ確認)。
  for (const leaf of leaves) {
    expect(leaf.length, `${endpoint} leaf array should have length=${LIMIT}`).toBe(LIMIT);
  }
}

test.describe('charts/* response shape', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('instance-wide charts return correctly shaped number arrays', async ({ request }) => {
    const me = await signupUser(request, randomUsername('chrA'));

    // host required な instance chart は別系統 (= 自 instance を host で投げる)
    // まず host なしの 6 endpoint を回す。
    for (const ep of [
      'charts/notes',
      'charts/users',
      'charts/drive',
      'charts/federation',
      'charts/ap-request',
      'charts/active-users',
    ]) {
      await assertChartShape(request, ep, {
        i: me.token,
        span: 'hour',
        limit: LIMIT,
      });
    }
  });

  test('charts/instance returns shape for own host', async ({ request }) => {
    const me = await signupUser(request, randomUsername('chrI'));
    // mkgo.local は test stack 内 nginx alias、TS image / mk-go どちらでも
    // local instance host として登録されている (= 自 instance host)。
    await assertChartShape(request, 'charts/instance', {
      i: me.token,
      span: 'hour',
      limit: LIMIT,
      host: 'mkgo.local',
    });
  });

  test('per-user charts return correctly shaped number arrays', async ({ request }) => {
    const me = await signupUser(request, randomUsername('chrU'));
    for (const ep of [
      'charts/user/notes',
      'charts/user/drive',
      'charts/user/following',
      'charts/user/pv',
      'charts/user/reactions',
    ]) {
      await assertChartShape(request, ep, {
        i: me.token,
        span: 'hour',
        limit: LIMIT,
        userId: me.id,
      });
    }
  });

  test('span=day also returns correctly shaped arrays', async ({ request }) => {
    const me = await signupUser(request, randomUsername('chrD'));
    // span=day path も 1 endpoint で touch する (= chart engine の day vs
    // hour bucket logic が両 backend で同 shape を返すかの smoke)。
    await assertChartShape(request, 'charts/notes', {
      i: me.token,
      span: 'day',
      limit: LIMIT,
    });
  });
});
