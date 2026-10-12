/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 4 PR-E: admin/captcha/* spec。
//
// 2 endpoint:
//   - admin/captcha/current: 現在の captcha 設定 (= object shape)
//   - admin/captcha/save: 設定を保存 (= 204、provider='none' で disable)

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../../fixtures/api';
import { resetRateLimit } from '../../../../fixtures/rate_limit';

interface RootFixture {
  id: string;
  token: string;
  username: string;
}

test.describe('admin/captcha/* shape compat', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    resetRateLimit();
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test('current → save (provider=none) round-trip', async ({ request }) => {
    // 1. current で現在の設定 shape を取得
    const curResp = await callApi(request, 'admin/captcha/current', {
      i: root.token,
    });
    expect(curResp.status()).toBe(200);
    const cur = await curResp.json();
    expect(typeof cur).toBe('object');

    // 2. save で provider=none を設定 (= disable captcha、test 環境への影響なし)
    const saveResp = await callApi(request, 'admin/captcha/save', {
      i: root.token,
      provider: 'none',
    });
    expect([200, 204]).toContain(saveResp.status());
  });
});
