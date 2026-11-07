/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 利用者ごとの連合の設定を画面で通す (#4072)。ローカル専用のアカウント (#3115) は
// /settings/other の枠で連合を止めてから戻す。フォローの止め方を「静かに成立させる」(#3466) に
// した人は、日の浅いアカウントからのフォローを通知なしで受け入れ、フォローの申請の画面の
// 専用のタブで後から確かめる。
//
// どちらもこのテストで作った利用者の設定なので、他の spec には効かない。

import { expect, test } from '@playwright/test';
import { isTsBackend } from '../../../fixtures/backend';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';
import { apiOk, freshUser } from '../../../fixtures/ui_timeline';

test.describe('UI: account federation', () => {
  test.skip(isTsBackend, 'ローカル専用のアカウントとフォローの止め方は Elythia 独自');
  test.setTimeout(90_000);

  test('turn federation off and back on', coversRoutes('/settings/other'), async ({ page, request, baseURL }) => {
    const me = await freshUser(request, 'pwlo');
    const status = async () => (await apiOk(request, 'i/federation', { i: me.token }) as { federationStatus: string }).federationStatus;
    expect(await status(), '作った直後は連合している (前提)').toBe('on');
    await uiSigninAs(page, request, baseURL!, me);
    await gotoRoute(page, baseURL!, '/settings/other');
    const folder = page.getByTestId('local-only-folder');
    await folder.getByTestId('folder-header').first().click();
    await expect(folder).toHaveAttribute('aria-expanded', 'true');

    await page.getByTestId('local-only-turn-off').click();
    await page.getByTestId('modal-dialog-ok').click();
    await expect.poll(status, { message: '連合を止められない' }).toBe('off');
    await expect(page.getByTestId('local-only-turn-off')).toHaveCount(0);

    await page.getByTestId('local-only-turn-on').click();
    await page.getByTestId('modal-dialog-ok').click();
    await expect.poll(status, { message: '連合に戻せない' }).toBe('on');
    await expect(page.getByTestId('local-only-turn-on')).toHaveCount(0);
  });

  test('follows accepted silently are listed in their tab', coversRoutes('/my/follow-requests'), async ({ page, request, baseURL }) => {
    const me = await freshUser(request, 'pwsf');
    const follower = await freshUser(request, 'pwsff');
    // 作られて 1 日に満たないローカルのアカウントからのフォローは、通知せずに成立させる
    await apiOk(request, 'i/update', { i: me.token, followApprovalLocalSeconds: 86400, followApprovalAction: 'silentFollow' });
    await apiOk(request, 'following/create', { i: follower.token, userId: me.id });
    const [rel] = await apiOk(request, 'users/relation', { i: me.token, userId: [follower.id] }) as { isFollowed: boolean }[];
    expect(rel.isFollowed, 'フォローが成立しない (前提)').toBe(true);

    await uiSigninAs(page, request, baseURL!, me);
    await gotoRoute(page, baseURL!, '/my/follow-requests');
    await page.getByTestId('follow-requests-tab-silent').click();
    await expect(page.locator(`[data-testid="silent-follow"][data-user-id="${follower.id}"]`), '通知せずに受け入れたフォローが出ない').toHaveCount(1, { timeout: 15_000 });
  });
});
