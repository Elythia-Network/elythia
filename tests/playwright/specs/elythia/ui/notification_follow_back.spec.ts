/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// フォローされた通知の中の「フォロー」のボタンでフォローを返せることを確かめる
// (Elythia 独自、#3185)。
//
// ボタンを押すと確認が出て (alwaysConfirmFollow の既定)、OK で `following/create` が
// フォローしてきた人の id で送られる。返した後は、行のボタンが「Followed」の
// 文字に変わる (disableIfFollowing。押してフォロー解除へ進まないように)。
// 既にフォローしている相手の通知には、最初からボタンを出さない。

import { expect, test, type Page } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { isTsBackend } from '../../../fixtures/backend';
import { pollForNotification } from '../../../fixtures/notifications';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { clickByTestId, clickWhenReady } from '../../../fixtures/ui_click';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';
import { follow, freshUser, pageBody, waitForApiBody } from '../../../fixtures/ui_timeline';

function followRow(page: Page, username: string) {
  return pageBody(page).locator('[data-testid="notification-follow"]').filter({ hasText: username });
}

// MkFollowButton は Playwright の visible 判定に入らない (fixtures/ui_click.ts の
// 注記、#2600) ので、数で見る
function followButton(page: Page, username: string) {
  return followRow(page, username).locator('button', { hasText: /^\s*Follow\s*$/ });
}

test.describe('UI: follow back from a follow notification', () => {
  test.skip(isTsBackend, 'フォロー返しのボタンは Elythia 独自 (#3185)');
  test.setTimeout(90_000);

  test('the button in the follow notification follows the follower back', coversRoutes('/my/notifications'), async ({ page, baseURL, request }) => {
    const viewer = await freshUser(request, 'pwfbv');
    const follower = await freshUser(request, 'pwfbf');
    const mutual = await freshUser(request, 'pwfbm');
    await follow(request, viewer, mutual);
    await follow(request, follower, viewer);
    await follow(request, mutual, viewer);
    await pollForNotification(request, viewer.token, (n) => n.type === 'follow' && n.userId === follower.id);
    await pollForNotification(request, viewer.token, (n) => n.type === 'follow' && n.userId === mutual.id);

    await uiSigninAs(page, request, baseURL!, viewer);
    await gotoRoute(page, baseURL!, '/my/notifications');
    await expect(followButton(page, follower.username)).toHaveCount(1, { timeout: 20_000 });
    // 既にフォローしている相手は文字だけ
    await expect(followRow(page, mutual.username)).toContainText('Followed', { timeout: 20_000 });
    await expect(followRow(page, mutual.username).locator('button')).toHaveCount(0);

    const sent = waitForApiBody(page, 'following/create');
    await clickWhenReady(
      page,
      `${follower.username} の通知の「Follow」`,
      (u: string) => {
        const bodies = document.querySelectorAll('._pageContainer');
        const rows = Array.from(bodies[bodies.length - 1]?.querySelectorAll('[data-testid="notification-follow"]') ?? []);
        const row = rows.find((r) => (r.textContent ?? '').includes(u));
        return Array.from(row?.querySelectorAll('button') ?? []).find((b) => (b.textContent ?? '').trim() === 'Follow');
      },
      follower.username,
    );
    await clickByTestId(page, 'modal-dialog-ok');
    expect(await sent).toMatchObject({ userId: follower.id });

    const resp = await callApi(request, 'users/relation', { i: viewer.token, userId: follower.id });
    const [rel] = await resp.json();
    expect(rel.isFollowing, 'フォローを返せていない').toBe(true);
    await expect(followRow(page, follower.username)).toContainText('Followed');
    await expect(followRow(page, follower.username).locator('button')).toHaveCount(0);
  });
});
