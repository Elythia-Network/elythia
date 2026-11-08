/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 管理画面の利用者のページ (/admin/user/:userId) の操作と、/admin の設定の警告を閉じる操作 (#4072)。
//
//   パスワードのリセット  確認を経て新しいパスワードが知らせに出て、そのパスワードで入れる
//   アバターの解除        確認を経てアバターが外れる
//   アカウントの削除      確認と、ユーザー名の入力を経て削除が始まる
//   設定の警告を閉じる    /admin の警告を閉じると隠れた件数が出て、「Show again」で戻る (Elythia の機能。
//                         閉じたものはその管理者の registry に入るので、テストで作った管理者で行う)

import { expect, test } from '@playwright/test';
import { assignRole, cleanupApi, rootFixture, signinAdmin } from '../../../fixtures/admin_table';
import { DEFAULT_TEST_PASSWORD } from '../../../fixtures/auth';
import { uploadTinyPNG } from '../../../fixtures/files';
import { callApi } from '../../../fixtures/api';
import { signinWithState } from '../../../fixtures/settings_table';
import { clickButtonContainingText } from '../../../fixtures/ui_click';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';
import { apiOk, freshUser } from '../../../fixtures/ui_timeline';

test.describe('UI: admin user actions', () => {
  test.setTimeout(60_000);

  test('reset the password of a user', coversRoutes('/admin/user/:userId'), async ({ page, request, baseURL }) => {
    const user = await freshUser(request, 'pwrst');
    await signinAdmin(page, baseURL!, rootFixture());
    await gotoRoute(page, baseURL!, `/admin/user/${user.id}`);
    await clickButtonContainingText(page, 'Reset password');
    const reset = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/admin/reset-password', { timeout: 15_000 });
    await page.getByTestId('modal-dialog-ok').click();
    const r = await reset;
    expect(r.ok(), 'admin/reset-password が失敗した').toBe(true);
    const { password } = await r.json() as { password: string };
    await expect(page.getByText(`The new password is "${password}"`).first(), '新しいパスワードが知らせに出ない').toBeVisible();

    const signin = (pw: string) => request.post(`${baseURL}/api/signin-flow`, { data: { username: user.username, password: pw }, failOnStatusCode: false });
    expect((await signin(password)).status(), '新しいパスワードで入れない').toBe(200);
    // 古いパスワードは「パスワードが違う」(本家と同じ ID) で断られる
    const old = await signin(DEFAULT_TEST_PASSWORD);
    expect(old.status(), '古いパスワードで入れる').toBe(403);
    expect((await old.json() as { error?: { id?: string } }).error?.id).toBe('932c904e-9460-45b7-9ce6-7ed33be7eb2c');
  });

  test('unset the avatar of a user', coversRoutes('/admin/user/:userId'), async ({ page, request, baseURL }) => {
    const user = await freshUser(request, 'pwunav');
    const file = await uploadTinyPNG(request, baseURL!, user.token, 'pw-avatar.png');
    await apiOk(request, 'i/update', { i: user.token, avatarId: file.id });
    await signinAdmin(page, baseURL!, rootFixture());
    await gotoRoute(page, baseURL!, `/admin/user/${user.id}`);
    await clickButtonContainingText(page, 'Unset avatar');
    const unset = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/admin/unset-user-avatar', { timeout: 15_000 });
    await page.getByTestId('modal-dialog-ok').click();
    expect((await unset).ok(), 'admin/unset-user-avatar が失敗した').toBe(true);
    // users/show はアバターの ID を返さないので、本人の i で見る
    expect((await apiOk(request, 'i', { i: user.token }) as { avatarId: string | null }).avatarId, 'アバターが外れない').toBeNull();
  });

  test('delete an account after typing the username', coversRoutes('/admin/user/:userId'), async ({ page, request, baseURL }) => {
    const user = await freshUser(request, 'pwdelac');
    await signinAdmin(page, baseURL!, rootFixture());
    await gotoRoute(page, baseURL!, `/admin/user/${user.id}`);
    await clickButtonContainingText(page, 'Delete account');
    await page.getByTestId('modal-dialog-ok').click();
    // 2 つ目のダイアログ (ユーザー名の入力) が、1 つ目が閉じきる前に開く
    await page.getByTestId('modal-dialog-input').locator('input').last().fill(user.username);
    const deleted = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/admin/delete-account', { timeout: 15_000 });
    await page.getByTestId('modal-dialog-ok').last().click();
    expect((await deleted).ok(), 'admin/delete-account が失敗した').toBe(true);
    // 削除はジョブで進み、終わると利用者ごと消える。削除済みの印か、見つからなくなるのを待つ
    await expect.poll(async () => {
      const r = await callApi(request, 'admin/show-user', { i: rootFixture().token, userId: user.id });
      return r.status() === 404 || (r.ok() && (await r.json() as { isDeleted?: boolean }).isDeleted === true);
    }, { message: '削除が始まらない' }).toBe(true);
  });

  test('dismiss a setting warning and show it again', coversRoutes('/admin'), async ({ page, request, baseURL }) => {
    const root = rootFixture();
    const admin = await freshUser(request, 'pwwarn');
    const roleId = await assignRole(request, root, admin.id, { isAdministrator: true });
    try {
      await signinWithState(page, request, baseURL!, admin);
      await gotoRoute(page, baseURL!, '/admin');
      // 警告は管理画面の左のメニューの側 (pageBody の外) に出る。「Configure」のリンクをちょうど 1 つ
      // 含む枠 (警告 1 つ分) の中の閉じるボタンで探す
      const closers = page.locator('xpath=//button[.//i[contains(concat(" ", normalize-space(@class), " "), " ti-x ")]][ancestor::*[.//a[normalize-space()="Configure"]][1][count(.//a[normalize-space()="Configure"]) = 1]]');
      await expect(closers.first(), '閉じられる設定の警告が無い (前提)').toBeVisible({ timeout: 20_000 });
      const before = await closers.count();

      const saved = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/i/registry/set', { timeout: 15_000 });
      await closers.first().click();
      expect((await saved).ok(), '閉じた警告が保存されない').toBe(true);
      await expect(closers, '閉じた警告が残る').toHaveCount(before - 1);
      await expect(page.getByText('1 warnings are hidden.').first(), '隠れた件数が出ない').toBeVisible();
      const stored = await apiOk(request, 'i/registry/get', { i: admin.token, scope: ['mkgo', 'adminPanel'], key: 'dismissedSettingWarnings' }) as string[];
      expect(stored.length, 'registry に閉じた警告が入らない').toBe(1);

      await clickButtonContainingText(page, 'Show again');
      await expect(closers, '「Show again」で戻らない').toHaveCount(before);
    } finally {
      await cleanupApi(request, 'admin/roles/delete', { i: root.token, roleId });
    }
  });
});
