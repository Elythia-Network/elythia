/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 投稿フォームの「...」の「Preview」で、本文の MFM を描いたプレビューを出し入れ
// できることを確かめる (#4043)。
//
// プレビューを出すかどうかは保存される設定 (store の showPreview) なので、
// 再読み込みの後に開いたフォームでも出たままであること、切ると消えることまで見る。
// 描いた結果は、`**...**` が <b> になっていることで確かめる (本文の文字が
// そのまま出ているだけでは、MFM を通したかどうか分からない)。

import { expect, test, type Page } from '@playwright/test';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { clickMenuItem, clickOnlyTestId, openPostForm, setPostText } from '../../../fixtures/ui_post_form';

async function togglePreview(page: Page): Promise<void> {
  await clickOnlyTestId(page, 'post-form-other-settings');
  await clickMenuItem(page, 'Preview');
  // スイッチの項目を押してもメニューは閉じないので、Esc で閉じる
  await page.keyboard.press('Escape');
  await expect(page.locator('[role="menu"]')).toHaveCount(0);
}

test.describe('UI: post form MFM preview', () => {
  test.setTimeout(90_000);

  test('the preview renders MFM and the toggle survives a reload', async ({ page, baseURL, request }) => {
    const user = await signupUser(request, randomUsername('pwprev'));
    await uiSigninAs(page, request, baseURL!, user);
    await openPostForm(page);
    const word = `pwbold${Date.now()}`;
    await setPostText(page, `**${word}**`);

    const preview = page.getByTestId('post-form-preview');
    await expect(preview).toHaveCount(0);
    await togglePreview(page);
    await expect(preview.locator('b', { hasText: word })).toHaveCount(1);

    await page.reload({ waitUntil: 'domcontentloaded' });
    await expect(page.locator('[data-testid="open-post-form"]').first()).toBeVisible({ timeout: 20_000 });
    await openPostForm(page);
    await expect(preview).toHaveCount(1);
    await setPostText(page, `**${word}2**`);
    await expect(preview.locator('b', { hasText: `${word}2` })).toHaveCount(1);

    await togglePreview(page);
    await expect(preview).toHaveCount(0);
  });
});
