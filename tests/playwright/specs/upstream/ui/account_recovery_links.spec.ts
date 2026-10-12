/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// メールから辿るアカウント回復・確認リンクをブラウザで開く (#2441)。
//
//   /reset-password/:token   パスワード再設定
//   /verify-email/:code      メールアドレスの確認
//   /signup-complete/:code   メール必須登録の完了
//
// いずれも未検証だった。これらは **利用者がログインできない状態で踏む**ため、
// 壊れていても本人には「リンクが動かない」としか分からず、報告も上がりにくい。
//
// 有効なトークンはメール送信を伴うので spec からは作れない。代わりに
// **無効なトークンで踏んだときの経路**を検証する。期限切れリンクは実運用で
// 頻繁に踏まれるうえ、ここが白画面だと利用者は詰む。
//
// 3 ページとも「押すまで API を叩かない」作りで、失敗時は
// `os.alert({ type: 'error', title: somethingHappened })` を出す。したがって
// ボタンが出ること + 押すとエラーが出ること の 2 段で確認する。

import { expect, test } from '@playwright/test';

/** Click the primary "Got it!" button these pages gate their API call behind. */
async function clickGotIt(page: import('@playwright/test').Page): Promise<void> {
  const button = page.getByRole('button', { name: 'Got it!' });
  await expect(button).toBeVisible({ timeout: 20_000 });
  await button.click();
}

test.describe('UI: account recovery / confirmation links', () => {
  test.setTimeout(60_000);

  test('/verify-email/:code は無効なコードでエラーを出す', async ({ page, baseURL }) => {
    const resp = await page.goto(`${baseURL}/verify-email/pw-invalid-code`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    await clickGotIt(page);

    // 無言で失敗すると、利用者は確認できたのかどうか分からないまま放置する。
    await expect(page.getByText('An error has occurred', { exact: false }).first()).toBeVisible({
      timeout: 20_000,
    });
  });

  test('/signup-complete/:code は無効なコードでエラーを出す', async ({ page, baseURL }) => {
    const resp = await page.goto(`${baseURL}/signup-complete/pw-invalid-code`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    await clickGotIt(page);

    await expect(page.getByText('An error has occurred', { exact: false }).first()).toBeVisible({
      timeout: 20_000,
    });
  });

  test('/reset-password/:token はパスワード入力欄を出す', async ({ page, baseURL }) => {
    const resp = await page.goto(`${baseURL}/reset-password/pw-invalid-token`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    // token 付きのときだけ入力欄が出る (token 無しはメールアドレス入力の
    // 別画面で、そちらは reset_password_render.spec.ts が見ている)。
    await expect(page.getByText('New password', { exact: false }).first()).toBeVisible({
      timeout: 20_000,
    });
    await expect(page.getByRole('button', { name: 'Save' })).toBeVisible({ timeout: 20_000 });
  });

  test('/reset-password/:token は無効なトークンで再設定できない', async ({ page, baseURL }) => {
    await page.goto(`${baseURL}/reset-password/pw-invalid-token`, {
      waitUntil: 'domcontentloaded',
    });

    const input = page.locator('input[type="password"]').first();
    await expect(input).toBeVisible({ timeout: 20_000 });
    await input.fill('newpassword1234');

    // 無効なトークンで通ってしまうと、リンクを推測した第三者が他人の
    // パスワードを変えられる。
    const rejected = page.waitForResponse(
      (r) => r.url().includes('/api/reset-password') && r.status() >= 400,
      { timeout: 20_000 },
    );
    await page.getByRole('button', { name: 'Save' }).click();
    await rejected;
  });
});
