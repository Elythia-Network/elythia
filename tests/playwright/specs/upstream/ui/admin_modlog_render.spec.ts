/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/modlog の種類での絞り込み (#4072)。広告の作成と利用者の凍結の記録を作り、絞り込みの
// 部品 (初めは隠れていて ti-filter のボタンで出る) で「Ad created」に絞ると、凍結の記録が消えて
// 広告の作成の記録だけが残ることを確かめる。
//
// 広告は期限の切れたものを作る (記録は残り、タイムラインなどの描画には出ない)。
//
// 記録の行の種類は <b> に出る。選んだ後の MkSelect の表示値にも「Ad created」が出るので、<b> に絞る。
// 絞ると古い一覧がフェードで残り、ローディング (0 件)、新しい一覧の順に移る。凍結の記録が無く広告の
// 作成の記録がある組は、そのどちらの途中でも成り立たない。

import { expect, test } from '@playwright/test';
import { cleanupApi, expectApiOk, fieldByLabel, pickSelect, rootFixture, signinAdmin } from '../../../fixtures/admin_table';
import { callApi } from '../../../fixtures/api';
import { pageBody } from '../../../fixtures/route_check';
import { clickButtonWithIcon } from '../../../fixtures/ui_click';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';
import { freshUser } from '../../../fixtures/ui_timeline';

test.describe('UI: /admin/modlog', () => {
  test.setTimeout(60_000);

  test('filter the moderation logs by type', coversRoutes('/admin/modlog'), async ({ page, baseURL, request }) => {
    const root = rootFixture();
    const now = Date.now();
    const ad = await expectApiOk(request, 'admin/ad/create', {
      i: root.token, url: 'https://remote.example/pw-modlog', memo: 'pw modlog', place: 'horizontal', priority: 'middle', ratio: 1,
      startsAt: now - 120_000, expiresAt: now - 60_000, imageUrl: 'https://remote.example/pw-modlog.png', dayOfWeek: 0,
    }) as { id: string };
    // いちばん新しい記録にするので、絞る前の 1 ページ目に必ず出る
    const user = await freshUser(request, 'pwml');
    await expectApiOk(request, 'admin/suspend-user', { i: root.token, userId: user.id });
    try {
      await signinAdmin(page, baseURL!, root);
      await gotoRoute(page, baseURL!, '/admin/modlog');
      const suspended = pageBody(page).locator('b').filter({ hasText: /^\s*Suspended\s*$/ });
      const adCreated = pageBody(page).locator('b').filter({ hasText: /^\s*Ad created\s*$/ });
      await expect(suspended.first(), '絞る前は凍結の記録が出る (対照)').toBeVisible({ timeout: 20_000 });
      // 広告の作成の記録も前提として見ておく。見えないまま絞ると、絞った後に 0 件になったのが
      // 絞り込みのせいか前提のせいか区別できない
      await expect(adCreated.first(), '絞る前は広告の作成の記録が出る (対照)').toBeVisible();

      await clickButtonWithIcon(page, 'i.ti-filter');
      const filtered = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/admin/show-moderation-logs' && r.request().postDataJSON()?.type === 'createAd', { timeout: 15_000 });
      await pickSelect(page, await fieldByLabel(page, 'Type'), 'Ad created');
      // 絞り込みを変えたら取り直す (#4074)
      expect((await filtered).ok(), 'admin/show-moderation-logs が失敗した').toBe(true);
      await expect.poll(async () => [await suspended.count(), await adCreated.count() > 0], { message: '絞っても別の種類の記録が残る' }).toEqual([0, true]);
    } finally {
      await callApi(request, 'admin/unsuspend-user', { i: root.token, userId: user.id });
      await cleanupApi(request, 'admin/ad/delete', { i: root.token, id: ad.id });
    }
  });
});
