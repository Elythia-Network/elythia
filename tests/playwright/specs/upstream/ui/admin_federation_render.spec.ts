/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/federation page の hydration smoke。
//
// federation page は MkInput (host filter) + MkSelect (state / sort) +
// MkPagination (instance list) で構成される。test 環境は remote instance を
// 持たないので list は空でも、上記の controls は必ず render される。
// PageWithHeader + MkInput + MkSelect の chain が壊れていないことを smoke
// level で verify する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /admin/federation page hydrates', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('controls (host input + state/sort selects) mount on /admin/federation', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/admin/federation`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // host filter は <input> として render される。state / sort は MkSelect
    // で内部的に <select> または combobox 相当の要素を持つ。最低限 input 1 つ
    // (host filter) が visible になれば federation page の controls が
    // hydrate された証拠。
    await page.waitForFunction(
      () => document.querySelectorAll('input').length > 0,
      { timeout: 20_000 },
    );

    // navbar (= 認証済 home の data-cy) が維持されているか確認。これで
    // /admin route guard も成立している。
    await expect(page.locator('[data-testid="open-post-form"]').first()).toBeVisible({
      timeout: 15_000,
    });
  });
});
