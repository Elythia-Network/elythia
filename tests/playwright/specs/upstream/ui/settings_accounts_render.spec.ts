/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /settings/accounts (アカウント切替) をブラウザで操作する (#2441)。
//
// このページは **ブラウザに保存されたアカウント**を並べる。サーバーの API では
// なく `@/accounts.js` のローカルストアが出所なので、他の settings 画面とは
// 壊れ方が違う。ストアが読めないと一覧が空になり、複数アカウントを持つ利用者は
// 切替手段を失う。
//
// カードを押すと popup menu (Switch / Remove) が出る。押しても何も起きない、
// という壊れ方はサーバー側の検証では捕まらない。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

/**
 * Scope to the accounts pane.
 *
 * The navbar and the widget column both render the signed-in user's name, so a
 * page-wide text match would pass even if the account list were empty.
 */
function accountsPane(page: import('@playwright/test').Page) {
  // 「Add account」だけを含む内側の div (`._buttons`) を掴むとカードが範囲外に
  // なるので、**アバターも含む**ことを条件に加えて一段外側を取る。
  return page
    .locator('div')
    .filter({ has: page.getByRole('button', { name: 'Add account' }) })
    .filter({ has: page.locator('img') })
    .last();
}

test.describe('UI: /settings/accounts', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('ログイン中のアカウントが一覧に出る', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/settings/accounts`, { waitUntil: 'domcontentloaded' });

    const pane = accountsPane(page);
    await expect(page.getByRole('button', { name: 'Add account' })).toBeVisible({
      timeout: 20_000,
    });
    // 自分自身が出ないなら、ローカルのアカウントストアが読めていない。
    // **navbar とウィジェット欄にも同じ username が出る**ので、必ずペイン内で探す
    // (page 全体で探すと store が空でも通ってしまう)。
    await expect(pane.getByText(root.username, { exact: false }).first()).toBeVisible({
      timeout: 20_000,
    });
  });

  test('カードを押すと切替メニューが出る', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/settings/accounts`, { waitUntil: 'domcontentloaded' });

    // MkUserCardMini の root は div で、そこに @click.prevent が付いている。
    // ペイン内に絞らないと、DOM 順で後ろに来るウィジェット欄のアバターを掴む。
    const card = accountsPane(page).locator('div:has(img)').filter({ hasText: root.username }).last();
    await expect(card).toBeVisible({ timeout: 20_000 });
    await card.click();

    // Switch が出ないと、複数アカウントを登録していても行き来できない。
    await expect(page.getByText('Switch', { exact: true }).first()).toBeVisible({
      timeout: 20_000,
    });
  });
});
