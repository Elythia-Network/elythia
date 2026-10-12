/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/users page で MkPagination が admin/show-users 経由で user 一覧を
// hydrate して、各 user の username が MkUserCardMini で render される
// ことを verify する spec。
//
// admin/users は host filter / status filter 等の MkInput / MkSelect を
// 持つが、本 spec は最低限の hydration smoke として root.username が body
// に出ることだけを確認する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /admin/users page renders user list', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('root username appears in /admin/users user list', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/admin/users`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // admin/show-users 経由で root を含む全 user が render される。
    // MkUserCardMini は username を <span> として render するので
    // body.textContent で照合できる。
    await page.waitForFunction(
      (n) => document.body.textContent?.includes(n) ?? false,
      root.username,
      { timeout: 20_000 },
    );
  });
});
