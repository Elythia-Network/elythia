/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /lookup?uri=<acct> を navigate して、acct を解決した user の profile page
// に redirect されることを verify する spec。
//
// /lookup は misskey-hub からの share / drop-in URL 経由 lookup の入口で、
// query param uri を `users/show` または `ap/show` で解決し SPA router 内で
// /@:acct に replace 遷移する。本 spec は acct 経路 (= local user の username
// だけで lookup) を smoke する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /lookup resolves local acct and redirects to profile', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('/lookup?uri=<root.username> redirects to /@<root.username>', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);

    // /lookup?uri=<acct> を navigate。/lookup は initial load 時 fetching
    // → users/show 解決後に SPA replace で /@:acct に飛ぶ (waitUntil:
    // 'domcontentloaded' では replace 前で response.status を捕捉する)。
    const resp = await page.goto(`${baseURL}/lookup?uri=${root.username}`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    // SPA router の replace は client-side で行われるので URL が
    // /@<username> に変わるまで待つ。
    await page.waitForURL((u) => u.pathname.startsWith(`/@${root.username}`), {
      timeout: 20_000,
    });

    // profile page には username が body に出る (= MkUserPage の hydration
    // 完了)。
    await page.waitForFunction(
      (n) => document.body.textContent?.includes(n) ?? false,
      root.username,
      { timeout: 20_000 },
    );
  });
});
