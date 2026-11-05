/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 入口 (未ログインのトップ) の登録のダイアログで、規約に同意し、入力して登録すると、
// 利用者ができてサインインした状態になり、初期設定のウィザードが出ることを確かめる
// (#4049)。
//
// - 同意するまで「続ける」は押せない
// - 送った値 (signup の username / password) と、API で読み直した利用者で確かめる
// - 登録した後のサインインの状態は、localStorage の account と、そのトークンで
//   サーバーに問い合わせた i で見る (ナビの自分の名前では判定しない)
//
// 規約の欄は meta の設定 (サーバーのルール・利用規約) で増えるので、出ている同意の
// スイッチを全部押す。meta は書き換えない。

import { expect, type Page, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { DEFAULT_TEST_PASSWORD, randomUsername, signin } from '../../../fixtures/auth';
import { clickByTestId, clickWhenReady } from '../../../fixtures/ui_click';

async function fillSignupInput(page: Page, testId: string, value: string): Promise<void> {
  const input = page.locator(`[data-testid="${testId}"] input`);
  await expect(input).toHaveCount(1, { timeout: 15_000 });
  await input.fill(value);
}

test.describe('UI: signup dialog', () => {
  test.setTimeout(90_000);

  test('agree, fill in, sign up, and land in the setup wizard as the new user', async ({ page, baseURL, request }) => {
    const username = randomUsername('pwsignup');
    await page.setViewportSize({ width: 1600, height: 900 });
    await page.goto(`${baseURL}/`, { waitUntil: 'domcontentloaded' });
    await clickByTestId(page, 'signup');

    // 同意するまで続けられない
    const cont = page.getByTestId('signup-rules-continue');
    await expect(cont).toBeVisible({ timeout: 15_000 });
    await expect(cont).toBeDisabled();
    // 同意のスイッチを押すと、確認のダイアログが出る (MkSignupDialog.rules.vue)。
    // ほかの同意 (サーバーのルール・利用規約) が出ていれば、それも同じく押す
    const agreeCount = await page.locator('input[type="checkbox"]').evaluateAll((inputs) =>
      inputs.filter((i) => (i.parentElement?.textContent ?? '').trim() === 'Agree').length);
    expect(agreeCount).toBeGreaterThanOrEqual(1);
    for (let n = 0; n < agreeCount; n++) {
      await clickWhenReady(page, `${n + 1} つ目の同意のスイッチ`, (idx: number) =>
        Array.from(document.querySelectorAll<HTMLInputElement>('input[type="checkbox"]'))
          .filter((i) => (i.parentElement?.textContent ?? '').trim() === 'Agree')[idx], n);
      await clickByTestId(page, 'modal-dialog-ok');
      await expect(page.getByTestId('modal-dialog-ok')).toHaveCount(0, { timeout: 10_000 });
    }
    await expect(cont).toBeEnabled({ timeout: 10_000 });
    await cont.click();

    await fillSignupInput(page, 'signup-username', username);
    await fillSignupInput(page, 'signup-password', DEFAULT_TEST_PASSWORD);
    await fillSignupInput(page, 'signup-password-retype', DEFAULT_TEST_PASSWORD);
    const submit = page.getByTestId('signup-submit');
    await expect(submit).toBeEnabled({ timeout: 15_000 });

    const sent = page.waitForRequest((r) => r.method() === 'POST' && new URL(r.url()).pathname === '/api/signup', { timeout: 15_000 });
    await submit.click();
    const body = (await sent).postDataJSON() as Record<string, unknown>;
    expect(body.username).toBe(username);
    expect(body.password).toBe(DEFAULT_TEST_PASSWORD);

    // 登録した直後にサインインし、初期設定のウィザードが出る
    // (ウィザードの外枠 user-setup-dialog は大きさを持たないので、最初の段のボタンで見る)
    await expect(page.getByTestId('user-setup-dialog').getByTestId('user-setup-continue')).toBeVisible({ timeout: 30_000 });

    // API で読み直す: 利用者ができていて、入れたパスワードでサインインできる
    const shown = await callApi(request, 'users/show', { username });
    expect(shown.status()).toBe(200);
    const user = await shown.json();
    expect(user.username).toBe(username);
    expect(user.host).toBeNull();
    await signin(request, username, DEFAULT_TEST_PASSWORD);

    // ブラウザがその利用者としてサインインしている
    const token = await page.evaluate(() => {
      const raw = window.localStorage.getItem('account');
      return raw === null ? null : (JSON.parse(raw) as { token?: string }).token ?? null;
    });
    expect(token).not.toBeNull();
    const me = await (await callApi(request, 'i', { i: token })).json();
    expect(me.id).toBe(user.id);
  });
});
