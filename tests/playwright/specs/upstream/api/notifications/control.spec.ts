/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 4 PR-B: notifications/* control endpoints (auth required)。
// 既存 notifications/*.spec.ts が event 系 (= reaction / mention / follow 等の
// 通知発生 path) を cover、本 spec は self-issued control endpoint を埋める:
//
//   - notifications/create: { body, header?, icon? } で自分宛 通知作成 → 204
//   - notifications/flush: 全削除 → 204
//   - notifications/test-notification: 連動 test 通知発火 → 204
//   - notifications/mark-all-as-read: 既存 spec で 1 度 cover 済だが、ここでも
//     state-less に再 hit (= idempotent)

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

test.describe('notifications/* control', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('create / mark-all-as-read / test-notification / flush', async ({ request }) => {
    const me = await signupUser(request, randomUsername('nc'));

    // create で自分宛 notification を 1 件発生させる
    const createResp = await callApi(request, 'notifications/create', {
      i: me.token,
      body: 'spec self-notification',
    });
    expect([200, 204]).toContain(createResp.status());

    // mark-all-as-read → 204 (= 既存 spec と同 shape、idempotent)
    const markResp = await callApi(request, 'notifications/mark-all-as-read', {
      i: me.token,
    });
    expect([200, 204]).toContain(markResp.status());

    // test-notification: server が test 通知を発火 → 204
    const testResp = await callApi(request, 'notifications/test-notification', {
      i: me.token,
    });
    expect([200, 204]).toContain(testResp.status());

    // flush: 自分宛 notification を全削除 → 204
    const flushResp = await callApi(request, 'notifications/flush', {
      i: me.token,
    });
    expect([200, 204]).toContain(flushResp.status());
  });
});
