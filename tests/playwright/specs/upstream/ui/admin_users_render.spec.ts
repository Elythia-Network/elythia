/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/users の一覧と操作 (#4072)。一覧に作ったばかりの利用者が出ること、状態で絞ると凍結した
// 利用者だけが残ること、見出しの「Add a user」から利用者を作れることを確かめる。
//
// 一覧は既定で新しい順に 10 件なので、他の spec が利用者を作った後では root は 1 ページ目に出ない。
// 名前で探すと、サインイン中の navbar に出る自分の名前に当たって空振りする。そのため、テストの中で
// 作った利用者を、管理画面の利用者ページへのリンクで探す。
//
// 利用者の作成は見出しのアイコンだけのボタン (ti-plus) から、ユーザー名とパスワードの 2 回の
// 入力のダイアログを経て admin/accounts/create を呼ぶ。作った利用者と凍結は消しにくいので、
// 名前は毎回変える。

import { expect, test } from '@playwright/test';
import { expectApiOk, fieldByLabel, pickSelect, rootFixture, signinAdmin } from '../../../fixtures/admin_table';
import { callApi } from '../../../fixtures/api';
import { DEFAULT_TEST_PASSWORD, randomUsername } from '../../../fixtures/auth';
import { pageBody } from '../../../fixtures/route_check';
import { clickButtonWithIcon } from '../../../fixtures/ui_click';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';
import { freshUser } from '../../../fixtures/ui_timeline';

test.describe('UI: /admin/users', () => {
  test.setTimeout(60_000);

  test('a new user appears in /admin/users user list', coversRoutes('/admin/users'), async ({ page, baseURL, request }) => {
    const user = await freshUser(request, 'pwaul');
    await signinAdmin(page, baseURL!, rootFixture());
    await gotoRoute(page, baseURL!, '/admin/users');
    await expect(pageBody(page).locator(`a[href="/admin/user/${user.id}"]`).first(), '作った利用者が一覧に出ない').toBeVisible({ timeout: 20_000 });
  });

  test('filter by state shows only suspended users', coversRoutes('/admin/users'), async ({ page, baseURL, request }) => {
    const root = rootFixture();
    const suspended = await freshUser(request, 'pwaus');
    await expectApiOk(request, 'admin/suspend-user', { i: root.token, userId: suspended.id });
    // 凍結していない対照。いちばん新しいので、絞る前の 1 ページ目に必ず出る
    const active = await freshUser(request, 'pwaua');
    try {
      await signinAdmin(page, baseURL!, root);
      await gotoRoute(page, baseURL!, '/admin/users');
      const activeRow = pageBody(page).locator(`a[href="/admin/user/${active.id}"]`);
      await expect(activeRow.first(), '絞る前は凍結していない利用者が出る (対照)').toBeVisible({ timeout: 20_000 });

      const filtered = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/admin/show-users' && r.request().postDataJSON()?.state === 'suspended', { timeout: 15_000 });
      await pickSelect(page, await fieldByLabel(page, 'State'), 'Suspend');
      expect((await filtered).ok(), 'admin/show-users が失敗した').toBe(true);
      // 絞ると古い一覧がフェードで残り、ローディング (0 件)、新しい一覧の順に移る。凍結した利用者だけが
      // 残る組は、そのどちらの途中でも成り立たない
      const suspendedRow = pageBody(page).locator(`a[href="/admin/user/${suspended.id}"]`);
      await expect.poll(async () => [await suspendedRow.count() > 0, await activeRow.count()], { message: '凍結した利用者だけにならない' }).toEqual([true, 0]);
    } finally {
      await callApi(request, 'admin/unsuspend-user', { i: root.token, userId: suspended.id });
    }
  });

  test('add a user from the header action', coversRoutes('/admin/users'), async ({ page, baseURL, request }) => {
    const root = rootFixture();
    const username = randomUsername('pwauc');
    await signinAdmin(page, baseURL!, root);
    await gotoRoute(page, baseURL!, '/admin/users');
    await clickButtonWithIcon(page, 'i.ti-plus');
    await page.getByTestId('modal-dialog-input').locator('input').fill(username);
    await page.getByTestId('modal-dialog-ok').click();
    // 1 つ目のダイアログが閉じきる前に 2 つ目が開くので、パスワードの欄と後から開いた OK で絞る
    await page.getByTestId('modal-dialog-input').locator('input[type="password"]').fill(DEFAULT_TEST_PASSWORD);
    const created = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/admin/accounts/create', { timeout: 15_000 });
    await page.getByTestId('modal-dialog-ok').last().click();
    expect((await created).ok(), 'admin/accounts/create が失敗した').toBe(true);
    const shown = await expectApiOk(request, 'users/show', { username }) as { id: string };
    await expect(pageBody(page).locator(`a[href="/admin/user/${shown.id}"]`).first(), '作った利用者が一覧に出ない').toBeVisible({ timeout: 15_000 });
  });
});
