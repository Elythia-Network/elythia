/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 設定のセキュリティ (/settings/security) でパスワードを変えると、新しいパスワードで
// サインインでき、古いパスワードでは入れないことを確かめる (#4049)。
//
// 変更は 3 つのダイアログ (新しいパスワード、もう一度、今のパスワードでの認証) を経る。
// 送った値 (i/change-password) で確かめ、新しいパスワードではフォームから
// サインインし直す。古いパスワードが通らないことは signin-flow で見る。

import { expect, type Page, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { DEFAULT_TEST_PASSWORD, randomUsername, signupUser } from '../../../fixtures/auth';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { clickButtonByText, clickByTestId } from '../../../fixtures/ui_click';
import { waitForApiBody } from '../../../fixtures/ui_post_form';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';

// 開いているダイアログのパスワードの欄 (ちょうど 1 つ) に入れる
async function fillOnlyPasswordInput(page: Page, value: string): Promise<void> {
  const input = page.locator('input[type="password"]');
  await expect(input).toHaveCount(1, { timeout: 10_000 });
  await input.fill(value);
}

test.describe('UI: change password', () => {
  test.setTimeout(90_000);

  test('the new password signs in and the old one no longer does', coversRoutes('/settings/security'), async ({ page, baseURL, request, browser }) => {
    const user = await signupUser(request, randomUsername('pwpasswd'));
    const newPassword = `pw-new-${Date.now()}`;
    await uiSigninAs(page, request, baseURL!, user);
    await gotoRoute(page, baseURL!, '/settings/security');

    // ダイアログは閉じきる前に次が開くので、次のダイアログの題が出て、パスワードの
    // 欄が 1 つになってから入れる
    await clickButtonByText(page, 'Change password');
    await expect(page.getByText('New password', { exact: true })).toBeVisible({ timeout: 10_000 });
    await fillOnlyPasswordInput(page, newPassword);
    await clickByTestId(page, 'modal-dialog-ok');
    await expect(page.getByText('Retype new password', { exact: true })).toBeVisible({ timeout: 10_000 });
    await fillOnlyPasswordInput(page, newPassword);
    await clickByTestId(page, 'modal-dialog-ok');
    // 今のパスワードで認証する
    await expect(page.getByTestId('modal-dialog-ok')).toHaveCount(0, { timeout: 10_000 });
    await expect(page.getByText('Authentication', { exact: true })).toBeVisible({ timeout: 10_000 });
    await fillOnlyPasswordInput(page, DEFAULT_TEST_PASSWORD);
    const sent = waitForApiBody(page, 'i/change-password');
    const accepted = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/i/change-password', { timeout: 15_000 });
    await clickButtonByText(page, 'Continue');
    expect(await sent).toMatchObject({ currentPassword: DEFAULT_TEST_PASSWORD, newPassword });
    expect((await accepted).status()).toBeLessThan(300);

    // 古いパスワードでは入れない
    const old = await callApi(request, 'signin-flow', { username: user.username, password: DEFAULT_TEST_PASSWORD });
    expect(old.status()).not.toBe(200);
    expect((await old.json()).i).toBeUndefined();

    // 新しいパスワードで、フォームからサインインし直せる
    const context = await browser.newContext({ ignoreHTTPSErrors: true, locale: 'en-US' });
    try {
      const fresh = await context.newPage();
      await uiSigninAs(fresh, request, baseURL!, user, newPassword);
    } finally {
      await context.close();
    }
  });
});
