/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 4 PR-B: hashtags/* shape spec。
//
// 5 endpoint すべて anonymous (= requireCredential: false)、shape verify のみ:
//   - hashtags/list / search / trend: 配列
//   - hashtags/show: 非存在 tag → 400 + NO_SUCH_HASHTAG (= #925 fix で揃え)
//   - hashtags/users: 非存在 tag → 空配列
//
// #925 fix で mk-go を upstream 揃えに厳格化済:
//   - hashtags/list: sort 必須
//   - hashtags/users: tag + sort 必須
//   - hashtags/show: 不明 tag は 400 NO_SUCH_HASHTAG (= ApiError 既定 status)

import { randomUUID } from 'node:crypto';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

test.describe('hashtags/* shape compat', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  // upstream Misskey TS の paramDef は厳格で、hashtags/list は sort 必須、
  // hashtags/users は tag + sort 必須、hashtags/show / users の tag は
  // alphanumeric のみ accept する。両 backend で動く params で固定する。
  const cleanTag = `spec${randomUUID().replace(/-/g, '')}`;

  test('hashtags/list returns array shape', async ({ request }) => {
    const resp = await callApi(request, 'hashtags/list', {
      limit: 5,
      sort: '+mentionedUsers',
    });
    expect(resp.status()).toBe(200);
    expect(Array.isArray(await resp.json())).toBe(true);
  });

  test('hashtags/search returns array shape', async ({ request }) => {
    const resp = await callApi(request, 'hashtags/search', {
      query: 'spec',
      limit: 5,
    });
    expect(resp.status()).toBe(200);
    expect(Array.isArray(await resp.json())).toBe(true);
  });

  test('hashtags/trend returns array shape', async ({ request }) => {
    const resp = await callApi(request, 'hashtags/trend', {});
    expect(resp.status()).toBe(200);
    expect(Array.isArray(await resp.json())).toBe(true);
  });

  test('hashtags/users returns empty array for unknown tag', async ({ request }) => {
    const resp = await callApi(request, 'hashtags/users', {
      tag: cleanTag,
      sort: '+follower',
      limit: 5,
    });
    expect(resp.status()).toBe(200);
    expect(await resp.json()).toEqual([]);
  });

  test('hashtags/show returns 400 NO_SUCH_HASHTAG for unknown tag', async ({ request }) => {
    // 両 backend ともに 400 + NO_SUCH_HASHTAG body (= ApiError 既定 status、
    // #925 fix で mk-go 揃え済)。
    const resp = await callApi(request, 'hashtags/show', { tag: cleanTag });
    expect(resp.status()).toBe(400);
    const body = (await resp.json()) as { error?: { code?: string } };
    expect(body.error?.code).toBe('NO_SUCH_HASHTAG');
  });
});
