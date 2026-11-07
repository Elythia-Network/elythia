/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /@:acct/followers と /@:acct/following が、API から取った関係の利用者を出すことと、
// 一覧の各行のフォローボタン (MkFollowButton の mini) でフォローとその解除ができることを
// 確かめる (#4072)。
//
// 行には testid が無く、ボタンは文字の無いアイコンだけなので、対象の利用者の acct を含む行に
// 絞って、行の中のアイコンで押す。フォローとその解除は既定で確認を挟む (alwaysConfirmFollow)。

import { expect, type Page, test } from '@playwright/test';
import { resetRateLimit } from '../../../fixtures/rate_limit';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { clickWhenReady } from '../../../fixtures/ui_click';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';
import { apiOk, follow, freshUser } from '../../../fixtures/ui_timeline';

/** Clicks the follow button (`icon`) in the row of the list that shows `@username`. */
async function clickRowFollowButton(page: Page, username: string, icon: 'ti-plus' | 'ti-minus'): Promise<void> {
  await clickWhenReady(page, `@${username} の行の ${icon} のボタン`, ([u, i]: [string, string]) => {
    // 枠が入れ子なら外側も当たるので、いちばん内側 (文書順で最後) の枠を行とする
    const row = Array.from(document.querySelectorAll('._panel')).filter((p) => (p.textContent ?? '').includes(`@${u}`)).at(-1);
    return Array.from(row?.querySelectorAll('button') ?? []).find((b) => b.querySelector(`i.${i}`) !== null);
  }, [username, icon] as [string, string]);
}

test.describe('UI: /@:acct/followers and /@:acct/following', () => {
  test.beforeAll(() => {
    resetRateLimit();
  });

  test.setTimeout(60_000);

  test('follow a user from the followers list', coversRoutes('/@:acct/followers'), async ({ page, baseURL, request }) => {
    const me = await freshUser(request, 'pwflme');
    const target = await freshUser(request, 'pwflt');
    const listed = await freshUser(request, 'pwfll');
    await follow(request, listed, target);

    await uiSigninAs(page, request, baseURL!, me);
    await gotoRoute(page, baseURL!, `/@${target.username}/followers`);
    await expect(page.getByText(`@${listed.username}`).first(), 'フォロワーが一覧に出ない').toBeVisible({ timeout: 20_000 });

    const followed = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/following/create', { timeout: 15_000 });
    await clickRowFollowButton(page, listed.username, 'ti-plus');
    await page.getByTestId('modal-dialog-ok').click();
    expect((await followed).ok(), 'following/create が失敗した').toBe(true);
    const [rel] = await apiOk(request, 'users/relation', { i: me.token, userId: [listed.id] }) as { isFollowing: boolean }[];
    expect(rel.isFollowing, 'フォローが成立しない').toBe(true);
  });

  test('unfollow a user from the following list', coversRoutes('/@:acct/following'), async ({ page, baseURL, request }) => {
    const me = await freshUser(request, 'pwflme');
    const owner = await freshUser(request, 'pwflo');
    const listed = await freshUser(request, 'pwfll');
    await follow(request, owner, listed);
    await follow(request, me, listed);

    await uiSigninAs(page, request, baseURL!, me);
    await gotoRoute(page, baseURL!, `/@${owner.username}/following`);
    await expect(page.getByText(`@${listed.username}`).first(), 'フォロー中の利用者が一覧に出ない').toBeVisible({ timeout: 20_000 });

    const unfollowed = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/following/delete', { timeout: 15_000 });
    await clickRowFollowButton(page, listed.username, 'ti-minus');
    await page.getByTestId('modal-dialog-ok').click();
    expect((await unfollowed).ok(), 'following/delete が失敗した').toBe(true);
    const [rel] = await apiOk(request, 'users/relation', { i: me.token, userId: [listed.id] }) as { isFollowing: boolean }[];
    expect(rel.isFollowing, 'フォローを解除できない').toBe(false);
  });
});
