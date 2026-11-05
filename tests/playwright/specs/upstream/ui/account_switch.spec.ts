/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 設定のアカウント (/settings/accounts) で 2 つ目のアカウントを足し、切り替えると、
// サインイン中の利用者 ($i) が変わることを確かめる (#4049)。
//
// $i はブラウザの localStorage の account から作られる (src/i.ts)。切り替えた後の
// account と、そのトークンでサーバーに問い合わせた i の両方で確かめる。足した
// アカウントは端末の一覧 (accountTokens) に入り、ページを開き直すと一覧に出る。

import { expect, type Page, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { DEFAULT_TEST_PASSWORD, randomUsername, signupUser } from '../../../fixtures/auth';
import { pageBody } from '../../../fixtures/route_check';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { clickButtonByText } from '../../../fixtures/ui_click';
import { clickMenuItem } from '../../../fixtures/ui_post_form';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';

function storedAccount(page: Page): Promise<{ id: string; token: string } | null> {
  return page.evaluate(() => {
    const raw = window.localStorage.getItem('account');
    return raw === null ? null : JSON.parse(raw) as { id: string; token: string };
  });
}

test.describe('UI: add and switch accounts', () => {
  test.setTimeout(90_000);

  test('adding an existing account and switching to it changes $i', coversRoutes('/settings/accounts'), async ({ page, baseURL, request }) => {
    const first = await signupUser(request, randomUsername('pwacct1'));
    const second = await signupUser(request, randomUsername('pwacct2'));
    // 切り替えた後にウィザードが出ないよう、2 人目も抑止しておく (uiSigninAs と同じ)
    expect((await callApi(request, 'i/registry/set', { i: second.token, scope: ['client', 'base'], key: 'accountSetupWizard', value: -1 })).ok()).toBe(true);
    await uiSigninAs(page, request, baseURL!, first);
    await gotoRoute(page, baseURL!, '/settings/accounts');

    // 既存のアカウントを足す (サインインのダイアログ)
    await clickButtonByText(page, 'Add account');
    await clickMenuItem(page, 'Existing account');
    const username = page.locator('[data-testid="signin-username"] input');
    await expect(username).toBeVisible({ timeout: 10_000 });
    await username.fill(second.username);
    await username.press('Enter');
    const password = page.locator('[data-testid="signin-password"] input');
    await expect(password).toBeVisible({ timeout: 10_000 });
    const signedIn = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/signin-flow' && r.status() === 200, { timeout: 15_000 });
    await password.fill(DEFAULT_TEST_PASSWORD);
    await password.press('Enter');
    await signedIn;
    await expect(page.locator('[data-testid="signin-password"]')).toHaveCount(0, { timeout: 15_000 });
    // 足しただけでは切り替わらない
    expect((await storedAccount(page))?.id).toBe(first.id);

    // 開き直すと、足したアカウントが一覧に出る。選んで切り替える
    await gotoRoute(page, baseURL!, '/settings/accounts');
    const card = pageBody(page).getByText(`@${second.username}`, { exact: false });
    await expect(card).toHaveCount(1, { timeout: 15_000 });
    await card.click();
    const switched = page.waitForEvent('load', { timeout: 20_000 });
    await clickMenuItem(page, 'Switch');
    await switched;
    await expect(page.getByTestId('open-post-form').first()).toBeVisible({ timeout: 20_000 });

    const account = await storedAccount(page);
    expect(account?.id).toBe(second.id);
    const me = await (await callApi(request, 'i', { i: account!.token })).json();
    expect(me.id).toBe(second.id);
    expect(me.username).toBe(second.username);
  });
});
