/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 認証済 SPA route navigation smoke。signin 後に timeline / profile / settings
// 等の主要 page を navigate して、frontend が API hydration → component mount
// まで通ることを回帰検出する。
//
// 各 test は以下の workflow を取る:
//   1. signin form 経由で root を auth (signin.spec.ts と同 pattern)
//   2. 対象 route に直接 navigate
//   3. route 固有の signature element (= URL ごとに必ず render される marker
//      を 1 つ選ぶ) を visible まで wait
//   4. not-found のページでないことを確かめる (#4025。以前は存在しない /my、
//      /notifications、/timeline/local を開いて not-found のページで緑になっていた。
//      自分のプロフィールは下の /@<username> の test で見る)
//
// authenticated route は frontend の hydration が API call → store 反映 →
// component mount の 3 段で進むため、locator timeout を 15-20s に設定する
// (= 単 spec で multiple ページを連鎖する場合は spec 単位で setTimeout 拡張)。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { expectRoutedPage } from '../../../fixtures/ui_route';

test.describe('UI: authenticated route navigation', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('navigate to /my/notifications after signin', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);

    // 通知は authenticated state でのみ accessible。SPA は full
    // page reload になるので splash → component mount の遷移を待つ。
    const resp = await page.goto(`${baseURL}/my/notifications`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);
    // SPA hydration 完了 = splash が消えて navbar が visible になる
    // (= authenticated state が維持されている確認も兼ねる)
    await expect(page.locator('[data-testid="open-post-form"]').first()).toBeVisible({ timeout: 20_000 });
    await expectRoutedPage(page, '/my/notifications');
  });

  test('navigate to /settings/profile after signin', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);

    const resp = await page.goto(`${baseURL}/settings/profile`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // settings page は input field を多数持つ。少なくとも 1 つ <input> が
    // render されることで authenticated state + settings component mount を確認。
    await page.waitForFunction(
      () => document.querySelectorAll('input').length > 0,
      { timeout: 20_000 },
    );
    await expectRoutedPage(page, '/settings/profile');
  });

  test('navigate to /timeline after signin', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);

    // local / home などは画面の中のタブで切り替える。URL は /timeline の 1 つ
    const resp = await page.goto(`${baseURL}/timeline`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // local timeline は WebSocket + initial GET を経由するため hydration
    // 完了まで時間がかかる。最低限 navbar (= [data-testid="open-post-form"]) が
    // 維持されていれば authenticated state が維持されている確認になる。
    await expect(page.locator('[data-testid="open-post-form"]').first()).toBeVisible({ timeout: 15_000 });
    await expectRoutedPage(page, '/timeline');
  });

  test('navigate to /@alice (own profile by username)', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);

    const resp = await page.goto(`${baseURL}/@${root.username}`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // user profile page には username が必ず render される。
    await page.waitForFunction(
      (username) => document.body.textContent?.includes(username) ?? false,
      root.username,
      { timeout: 20_000 },
    );
    await expectRoutedPage(page, `/@${root.username}`);
  });
});
