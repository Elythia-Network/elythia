/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 設定のメニューの「Sign Out」で、確認のダイアログを経てサインアウトすると、入口
// (未ログインのトップ) に戻り、ログインの要るページが開けなくなることを確かめる
// (#4049)。
//
// 確認でキャンセルしたときは、サインインの状態 (localStorage の account) が残る。
// サインアウトしたことは、localStorage の account が消えたことと、入口のサインインの
// ボタンが出ることで見る (ナビの自分の名前の有無では判定しない)。ログインの要る
// ページは、not-found とサインインのダイアログになる (全ルートの表と同じ判定)。

import { expect, type Page, test } from '@playwright/test';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { expectSigninRequired, openAsOutsider } from '../../../fixtures/route_check';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { clickButtonByText, clickByTestId } from '../../../fixtures/ui_click';
import { gotoRoute } from '../../../fixtures/ui_route';

function storedAccountId(page: Page): Promise<string | null> {
  return page.evaluate(() => {
    const raw = window.localStorage.getItem('account');
    return raw === null ? null : (JSON.parse(raw) as { id?: string }).id ?? null;
  });
}

test.describe('UI: sign out', () => {
  test.setTimeout(90_000);

  test('signing out returns to the entrance and login-only pages stop opening', async ({ page, baseURL, request }) => {
    const user = await signupUser(request, randomUsername('pwsignout'));
    await uiSigninAs(page, request, baseURL!, user);
    await gotoRoute(page, baseURL!, '/settings/profile');

    // キャンセルではサインアウトしない
    await clickButtonByText(page, 'Sign Out');
    await clickByTestId(page, 'modal-dialog-cancel');
    await expect(page.getByTestId('modal-dialog-cancel')).toHaveCount(0, { timeout: 10_000 });
    expect(await storedAccountId(page)).toBe(user.id);
    await expect(page).toHaveURL(/\/settings\/profile$/);

    await clickButtonByText(page, 'Sign Out');
    const reloaded = page.waitForURL((u) => u.pathname === '/', { timeout: 20_000 });
    await clickByTestId(page, 'modal-dialog-ok');
    await reloaded;
    await expect(page.getByTestId('signin').first()).toBeVisible({ timeout: 20_000 });
    expect(await storedAccountId(page)).toBeNull();
    await expect(page.getByTestId('open-post-form')).toHaveCount(0);

    // ログインの要るページは開けない (not-found とサインインのダイアログになる)
    for (const path of ['/settings/profile', '/my/notifications']) {
      await openAsOutsider(page, baseURL!, path);
      await expectSigninRequired(page, path);
    }
  });
});
