/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 通知の発生 (= 別 user が follow / mention) を API で trigger → UI 側で
// /my/notifications を navigate して通知 item が render されることを確認する
// mixed e2e。
//
// upstream Misskey は notification timeline の root に data-cy attribute が
// 無いため、本 spec は body.textContent 包含 check (= follower username が
// 通知 list の renderer に到達している) で hydration 完了を verify する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { DEFAULT_TEST_PASSWORD, signupUser } from '../../../fixtures/auth';
import { resetRateLimit } from '../../../fixtures/rate_limit';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: notification timeline renders after API-triggered follow', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    resetRateLimit();
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('a fresh follower user appears in /my/notifications after follow', async ({ page, baseURL, request }) => {
    // root を follow する fresh user を作成 (= follow 通知が root inbox に届く)
    const followerName = `follwer${Date.now().toString().slice(-9)}`;
    const follower = await signupUser(request, followerName, DEFAULT_TEST_PASSWORD);

    const followResp = await callApi(request, 'following/create', {
      i: follower.token,
      userId: root.id,
    });
    expect(followResp.status()).toBe(200);

    // root として signin → /my/notifications を navigate
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/my/notifications`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // notification list は MkPagination で打ち、各 item に follower の
    // username が含まれる (= MkUserName の render を経由する)。username
    // 文字列が body に出るまで polling で待機。
    await page.waitForFunction(
      (n) => document.body.textContent?.includes(n) ?? false,
      followerName,
      { timeout: 20_000 },
    );
  });
});
