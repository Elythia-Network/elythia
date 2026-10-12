/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /@:acct/followers と /@:acct/following が API hydration 経由で関係 user
// を render することを verify する spec。
//
// users/followers / users/following endpoint は API spec で個別に cover
// されているが、frontend 側の MkPagination + MkUserInfo + Paginator の
// chain が壊れていないことは別 layer なので本 spec で smoke する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { DEFAULT_TEST_PASSWORD, signupUser } from '../../../fixtures/auth';
import { resetRateLimit } from '../../../fixtures/rate_limit';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /@:acct/followers and /@:acct/following render relations', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    resetRateLimit();
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('follower username appears in /@alice/followers and /@<follower>/following', async ({ page, baseURL, request }) => {
    const followerName = `flwlist${Date.now().toString().slice(-9)}`;
    const follower = await signupUser(request, followerName, DEFAULT_TEST_PASSWORD);

    const followResp = await callApi(request, 'following/create', {
      i: follower.token,
      userId: root.id,
    });
    expect(followResp.status()).toBe(200);

    await uiSigninAsRoot(page, baseURL, root);

    // /@alice/followers — root の followers に follower username が出る
    {
      const resp = await page.goto(`${baseURL}/@${root.username}/followers`, { waitUntil: 'domcontentloaded' });
      expect(resp!.status()).toBe(200);
      await page.waitForFunction(
        (n) => document.body.textContent?.includes(n) ?? false,
        followerName,
        { timeout: 20_000 },
      );
    }

    // /@<follower>/following — follower の following に root.username が出る
    {
      const resp = await page.goto(`${baseURL}/@${followerName}/following`, { waitUntil: 'domcontentloaded' });
      expect(resp!.status()).toBe(200);
      await page.waitForFunction(
        (n) => document.body.textContent?.includes(n) ?? false,
        root.username,
        { timeout: 20_000 },
      );
    }
  });
});
