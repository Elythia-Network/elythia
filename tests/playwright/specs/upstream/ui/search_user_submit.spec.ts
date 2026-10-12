/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /search?type=user で MkInput に query を埋め → Search button click →
// /api/users/search round-trip → 結果一覧に matched user が render される
// **真の write-flow** spec。
//
// 自分 (root) の username で users/search すると最低自分 1 人 hit する。
// Search button textContent "Search" で identify。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonByText } from '../../../fixtures/ui_click';

test.describe('UI: /search user submit flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('navigate /search?type=user → fill query → Search → users/search round-trips', async ({
    page,
    baseURL,
  }) => {
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/search?type=user`, { waitUntil: 'domcontentloaded' });

    // query input が hydrate
    await page.waitForFunction(
      () => document.querySelectorAll('input').length >= 1,
      { timeout: 20_000 },
    );

    // search query に root.username を書く
    await page.evaluate((q) => {
      const target = (Array.from(document.querySelectorAll('input')) as HTMLInputElement[]).find(
        (i) => i.type === 'search',
      );
      if (!target) return;
      target.focus();
      const setter = Object.getOwnPropertyDescriptor(
        window.HTMLInputElement.prototype,
        'value',
      )?.set;
      setter?.call(target, q);
      target.dispatchEvent(new Event('input', { bubbles: true }));
    }, root.username);

    // users/search response 捕捉して Search click
    const searchResp = page.waitForResponse(
      (r) => r.url().includes('/api/users/search') && r.status() === 200,
      { timeout: 15_000 },
    );
    await clickButtonByText(page, 'Search');
    await searchResp;

    // 検索結果の MkUserList に root.username が render される
    await page.waitForFunction(
      (n) => document.body.textContent?.includes(n) ?? false,
      root.username,
      { timeout: 20_000 },
    );
  });
});
