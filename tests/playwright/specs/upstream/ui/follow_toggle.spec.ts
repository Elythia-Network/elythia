/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /@<other-user> page で MkFollowButton を click → /api/users/show で
// isFollowing=true round-trip を verify する **真の write-flow** spec。
//
// MkFollowButton に data-cy は無いが、未 follow 時は i18n.ts.follow
// → "Follow" text を持つ <button> として render される。この text を述語に
// 待機と click を束ねる (fixtures/ui_click.ts)。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { DEFAULT_TEST_PASSWORD, signupUser } from '../../../fixtures/auth';
import { resetRateLimit } from '../../../fixtures/rate_limit';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonByText, clickByTestId } from '../../../fixtures/ui_click';

test.describe('UI: follow button click toggles following relation', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    resetRateLimit();
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('navigate /@other → click Follow → users/show.isFollowing=true', async ({
    page,
    baseURL,
    request,
  }) => {
    // root が follow する対象 user を作成
    const otherName = `flwbtn${Date.now().toString().slice(-9)}`;
    const other = await signupUser(request, otherName, DEFAULT_TEST_PASSWORD);

    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/@${otherName}`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // **username が body に出ることを hydration の代理にしない。** username は
    // skeleton や header の時点で既に DOM にあるので、MkFollowButton の描画を
    // 待つ条件になっていなかった。実測で click が navigation の 360ms 後に
    // 走り、ボタン未描画のまま次へ進んでいた (#2600)。
    //
    // 待機と click は clickButtonByText が 1 つの述語で束ねるのでずれない
    // (#2617)。programmatic click なのは MkFollowButton が DOM にあっても
    // Playwright の visible 判定に入らないため。詳細は fixtures/ui_click.ts。
    await clickButtonByText(page, 'Follow');

    // MkFollowButton click → prefer.alwaysConfirmFollow=true (def.ts:370)
    // で os.confirm() dialog が出る → OK click が follow API trigger。
    const followResp = page.waitForResponse(
      (r) => r.url().includes('/api/following/create') && r.status() === 200,
      { timeout: 15_000 },
    );
    await clickByTestId(page, 'modal-dialog-ok', { timeout: 10_000 });
    await followResp;

    // backend で users/show を引いて isFollowing=true を verify
    const showResp = await callApi(request, 'users/show', {
      i: root.token,
      userId: other.id,
    });
    expect(showResp.status()).toBe(200);
    const shown = await showResp.json();
    expect(shown.isFollowing).toBe(true);
  });
});
