/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /my/follow-requests page で MkPagination が hydrate されて empty state
// (i18n.ts.noFollowRequests = "No follow requests") が render される
// ことを smoke する spec。
//
// root を locked にして follow request を発生させるパターンも考えられる
// が、認証 / 状態変更の副作用が大きいので、empty state による hydration
// smoke を採用する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /my/follow-requests page hydrates', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('follow-requests page renders empty state', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/my/follow-requests`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    // root に pending follow request は無いので empty state が表示される。
    // i18n.ts.noFollowRequests → "You don't have any pending follow requests"。
    await page.waitForFunction(
      () => {
        const text = document.body.textContent ?? '';
        return text.includes("You don't have any pending follow requests");
      },
      { timeout: 20_000 },
    );
  });
});
