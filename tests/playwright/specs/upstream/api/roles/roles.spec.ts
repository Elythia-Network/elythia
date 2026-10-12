/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 4 PR-B: roles/* (public 側) shape spec。admin/roles/* は別 (PR #890
// で cover 済 / #826 issue close)。本 spec は frontend が roles list / show
// / users / notes を叩く経路を verify する。
//
//   - roles/list (auth required): 配列
//   - roles/show / users / notes: roleId 不明で 4xx (= reversi/show-game pattern)

import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

test.describe('roles/* public shape compat', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test('roles/list returns array shape (auth required)', async ({ request }) => {
    const me = await signupUser(request, randomUsername('rl'));
    const resp = await callApi(request, 'roles/list', { i: me.token });
    expect(resp.status()).toBe(200);
    expect(Array.isArray(await resp.json())).toBe(true);
  });

  for (const endpoint of ['roles/show', 'roles/users']) {
    test(`${endpoint} returns negative for unknown roleId`, async ({ request }) => {
      // upstream paramDef format: 'misskey:id' で TS は 400、mk-go は post-lookup で 404。LCD。
      const resp = await callApi(request, endpoint, {
        roleId: '9zzzzzzzzzzzzzzz',
      });
      expect([400, 404]).toContain(resp.status());
    });
  }

  test('roles/notes returns negative for unknown roleId (auth required)', async ({ request }) => {
    const me = await signupUser(request, randomUsername('rln'));
    const resp = await callApi(request, 'roles/notes', {
      i: me.token,
      roleId: '9zzzzzzzzzzzzzzz',
    });
    expect([400, 404]).toContain(resp.status());
  });
});
