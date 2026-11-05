/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 設定のセキュリティで 2 要素認証 (TOTP) を登録すると、サインインでコードを求められ、
// 正しいコードでだけ入れることを確かめる (#4049)。
//
// コードは、登録の応答 (i/2fa/register) の secret から otplib で計算する。登録の確定
// (i/2fa/done) に使ったコードは、同じ時間枠の中では使い回せない (replay の保護。
// internal/core/twofactor/replay.go)。サインインには次の時間枠のコードを使う (サーバーは
// 前後 1 枠を受け付ける)。間違ったコードで入れないことも見る。テストの stack が検証を
// 素通しにしていない (testMode でない) ことの確かめにもなる。

import { expect, type Page, test } from '@playwright/test';
import { authenticator } from 'otplib';
import { callApi } from '../../../fixtures/api';
import { DEFAULT_TEST_PASSWORD, randomUsername, signupUser } from '../../../fixtures/auth';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { clickButtonByText } from '../../../fixtures/ui_click';
import { waitForApiBody } from '../../../fixtures/ui_post_form';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';

const STEP_MS = 30_000;

function codeAt(secret: string, epoch: number): string {
  return authenticator.clone({ epoch }).generate(secret);
}

// 開いている画面の、ワンタイムコードの欄 (ちょうど 1 つ) に入れる
async function fillOtp(page: Page, code: string): Promise<void> {
  const input = page.locator('input[autocomplete="one-time-code"]');
  await expect(input).toHaveCount(1, { timeout: 10_000 });
  await input.fill(code);
}

test.describe('UI: two-factor authentication (TOTP)', () => {
  test.setTimeout(120_000);

  test('registering TOTP makes sign-in ask for the code', coversRoutes('/settings/security'), async ({ page, baseURL, request, browser }) => {
    const user = await signupUser(request, randomUsername('pwtotp'));
    await uiSigninAs(page, request, baseURL!, user);
    await gotoRoute(page, baseURL!, '/settings/security');

    await clickButtonByText(page, 'Register authenticator app');
    const password = page.locator('input[type="password"]');
    await expect(password).toHaveCount(1, { timeout: 10_000 });
    await password.fill(DEFAULT_TEST_PASSWORD);
    const registered = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/i/2fa/register', { timeout: 15_000 });
    await clickButtonByText(page, 'Continue');
    const reg = await registered;
    expect(reg.status()).toBe(200);
    const secret = (await reg.json()).secret as string;
    expect(secret).toBeTruthy();

    // 認証のダイアログが消えてから、登録のダイアログを進める (消えかけのダイアログにも
    // 同じ「Continue」があり、先に見つかる)
    await expect(password).toHaveCount(0, { timeout: 10_000 });
    await expect(page.getByText('Setup two-factor authentification', { exact: true })).toBeVisible({ timeout: 10_000 });
    await clickButtonByText(page, 'Continue');
    // 2 段目でコードを入れて確定する
    const doneCode = codeAt(secret, Date.now());
    await fillOtp(page, doneCode);
    const done = waitForApiBody(page, 'i/2fa/done');
    const doneResp = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/i/2fa/done', { timeout: 15_000 });
    await clickButtonByText(page, 'Continue');
    expect((await done).token).toBe(doneCode);
    expect((await doneResp).status()).toBeLessThan(300);
    await clickButtonByText(page, 'Done');

    const me = await (await callApi(request, 'i', { i: user.token })).json();
    expect(me.twoFactorEnabled).toBe(true);

    // 別のブラウザでサインインすると、パスワードの後にコードを求められる
    const context = await browser.newContext({ ignoreHTTPSErrors: true, locale: 'en-US' });
    try {
      const fresh = await context.newPage();
      await fresh.setViewportSize({ width: 1600, height: 900 });
      await fresh.goto(`${baseURL}/`, { waitUntil: 'domcontentloaded' });
      await fresh.locator('[data-testid="signin"]').first().click();
      await fresh.locator('[data-testid="signin-username"] input').fill(user.username);
      await fresh.locator('[data-testid="signin-username"] input').press('Enter');
      await fresh.locator('[data-testid="signin-password"] input').fill(DEFAULT_TEST_PASSWORD);
      const askedResp = fresh.waitForResponse((r) => new URL(r.url()).pathname === '/api/signin-flow', { timeout: 15_000 });
      await fresh.locator('[data-testid="signin-password"] input').press('Enter');
      expect((await (await askedResp).json()).next).toBe('totp');

      // 間違ったコードでは入れない
      const wrong = codeAt(secret, Date.now()) === '000000' ? '111111' : '000000';
      await fillOtp(fresh, wrong);
      const rejected = fresh.waitForResponse((r) => new URL(r.url()).pathname === '/api/signin-flow', { timeout: 15_000 });
      await fresh.locator('input[autocomplete="one-time-code"]').press('Enter');
      expect((await rejected).status()).not.toBe(200);
      await expect(fresh.getByText('The one-time password is incorrect or has expired.', { exact: true })).toBeVisible({ timeout: 10_000 });
      await expect(fresh.getByTestId('open-post-form')).toHaveCount(0);
      await fresh.getByTestId('modal-dialog-ok').click();
      await expect(fresh.getByTestId('modal-dialog-ok')).toHaveCount(0, { timeout: 10_000 });

      // 次の時間枠の正しいコードで入れる
      const code = codeAt(secret, Date.now() + STEP_MS);
      const input = fresh.locator('input[autocomplete="one-time-code"]');
      await expect(input).toHaveCount(1, { timeout: 15_000 });
      await input.fill(code);
      const sent = fresh.waitForRequest((r) => r.method() === 'POST' && new URL(r.url()).pathname === '/api/signin-flow', { timeout: 15_000 });
      const accepted = fresh.waitForResponse((r) => new URL(r.url()).pathname === '/api/signin-flow', { timeout: 15_000 });
      await input.press('Enter');
      expect(((await sent).postDataJSON() as Record<string, unknown>).token).toBe(code);
      const body = await (await accepted).json();
      expect(body.finished).toBe(true);
      await expect(fresh.getByTestId('open-post-form').first()).toBeVisible({ timeout: 20_000 });
      const account = await fresh.evaluate(() => JSON.parse(window.localStorage.getItem('account') ?? 'null') as { id: string } | null);
      expect(account?.id).toBe(user.id);
    } finally {
      await context.close();
    }
  });
});
