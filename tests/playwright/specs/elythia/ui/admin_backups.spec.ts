/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// DB のバックアップの画面 (#3462) を通す (#4072)。閲覧にも 2FA での再認証が要るので、2FA の無い
// 管理者には案内だけが出る。2FA のある管理者は、パスワードとワンタイムのコードで再認証して
// 世代の一覧を出す。
//
// このスタックの保存先は instance.yml の dir (/backup、compose.yml で目印のファイルだけを置いた
// ディレクトリを読み取り専用で渡す) で、バックアップ用のサービスは置いていない。そのため一覧は
// 「サービス未設定」と「世代なし」になる。root に 2FA を付けると他の spec の入り方が変わるので、
// 2FA と管理者のロールは、このテストで作った利用者に付ける。

import { expect, test } from '@playwright/test';
import { authenticator } from 'otplib';
import { assignRole, cleanupApi, expectApiOk, rootFixture, signinAdmin } from '../../../fixtures/admin_table';
import { DEFAULT_TEST_PASSWORD, randomUsername, signupUser } from '../../../fixtures/auth';
import { isTsBackend } from '../../../fixtures/backend';
import { signinWithState } from '../../../fixtures/settings_table';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';

test.describe('admin: backups', () => {
  test.skip(isTsBackend, 'バックアップの画面は Elythia だけにある');
  test.setTimeout(90_000);

  test('an admin without 2FA sees only the notice', coversRoutes('/admin/backups'), async ({ page, baseURL }) => {
    await signinAdmin(page, baseURL!, rootFixture());
    await gotoRoute(page, baseURL!, '/admin/backups');
    await expect(page.getByTestId('backups-two-factor-required')).toBeVisible();
    await expect(page.getByTestId('backups-show')).toHaveCount(0);
  });

  test('an admin with 2FA re-authenticates and lists the generations', coversRoutes('/admin/backups'), async ({ page, request, baseURL }) => {
    const root = rootFixture();
    const admin = await signupUser(request, randomUsername('pwbk'), DEFAULT_TEST_PASSWORD);
    const roleId = await assignRole(request, root, admin.id, { isAdministrator: true });
    try {
      const { secret } = await expectApiOk(request, 'i/2fa/register', { i: admin.token, password: DEFAULT_TEST_PASSWORD }) as { secret: string };
      await expectApiOk(request, 'i/2fa/done', { i: admin.token, token: authenticator.generate(secret) });

      await page.addInitScript(() => {
        window.localStorage.setItem('modifiedVersionMustProminentlyOfferInAgplV3Section13Read', 'true');
      });
      await signinWithState(page, request, baseURL!, admin);
      await gotoRoute(page, baseURL!, '/admin/backups');
      await expect(page.getByTestId('backups-two-factor-required')).toHaveCount(0);
      await page.getByTestId('backups-show').click();

      await page.getByTestId('password-dialog-password').locator('input').fill(DEFAULT_TEST_PASSWORD);
      // 登録で使ったコードは使用済みになる。サーバーは前後 1 ステップを許すので、
      // 次のステップのコードを作れば待たずに通る
      const next = authenticator.clone({ epoch: Date.now() + 30_000 }).generate(secret);
      await page.getByTestId('password-dialog-token').locator('input').fill(next);
      const listed = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/admin/backup/list', { timeout: 15_000 });
      await page.getByTestId('password-dialog-submit').click();
      expect((await listed).ok(), 'admin/backup/list が失敗した').toBe(true);

      await expect(page.getByTestId('backups-service-not-configured')).toBeVisible();
      await expect(page.getByTestId('backups-empty'), '世代の一覧が出ない').toBeVisible();
    } finally {
      await cleanupApi(request, 'admin/roles/delete', { i: root.token, roleId });
    }
  });
});
