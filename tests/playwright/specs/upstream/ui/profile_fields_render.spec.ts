/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /@:user の profile page で i/update-applied description / fields が API
// hydration 経由で render されることを verify する spec。
//
// API spec (specs/users/profile.spec.ts) は users/show のレスポンス shape
// を verify する API-only。本 spec は MkUserHome + MkMfm chain で
// description が body に出ることまで covers する。
//
// fields の round-trip も #956 で i/update に Fields を accept するよう
// 拡張済 (trim + 空 entry 排除 + maxItems 16 cap)。本 spec では /api/i 上で
// fields が trim 済の name/value で round-trip されることを verify する。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { DEFAULT_TEST_PASSWORD, signupUser } from '../../../fixtures/auth';
import { resetRateLimit } from '../../../fixtures/rate_limit';

test.describe('UI: /@:user renders profile description / fields', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test.setTimeout(60_000);

  test('/@<user> shows i/update-applied description', async ({ page, baseURL, request }) => {
    const userName = `profrnd${Date.now().toString().slice(-9)}`;
    const user = await signupUser(request, userName, DEFAULT_TEST_PASSWORD);

    const description = `playwright-profile-desc ${Date.now()}`;

    const updateResp = await callApi(request, 'i/update', {
      i: user.token,
      description,
    });
    expect(updateResp.status()).toBe(200);

    // anonymous でも /@<user> は users/show 経由で description を取得できる
    // (= profile page の public hydration path を smoke)。
    await page.setViewportSize({ width: 1600, height: 900 });
    const resp = await page.goto(`${baseURL}/@${userName}`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    await page.waitForFunction(
      (d) => document.body.textContent?.includes(d) ?? false,
      description,
      { timeout: 20_000 },
    );
  });

  // #956 fix 後: i/update に fields を投げると user_profile.fields に persist
  // されて /api/i が round-trip 値を返す。
  test('/api/i round-trips fields after i/update', async ({ request }) => {
    const userName = `proffld${Date.now().toString().slice(-9)}`;
    const user = await signupUser(request, userName, DEFAULT_TEST_PASSWORD);

    const fieldName = `playwright-field-${Date.now()}`;
    const fieldValue = 'https://example.invalid/playwright';
    const updateResp = await callApi(request, 'i/update', {
      i: user.token,
      fields: [{ name: fieldName, value: fieldValue }],
    });
    expect(updateResp.status()).toBe(200);

    const meResp = await callApi(request, 'i', { i: user.token });
    expect(meResp.status()).toBe(200);
    const me = await meResp.json();
    expect(me.fields).toEqual([{ name: fieldName, value: fieldValue }]);
  });
});
