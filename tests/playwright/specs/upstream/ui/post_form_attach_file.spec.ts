/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 投稿フォームでファイルを選んで投稿すると、上げたファイルの `fileIds` が送られ、
// 上げる前に指定したセンシティブと説明 (alt text) がファイルに付くことを確かめる (#4043)。
//
// ファイルは投稿するときに上げる (MkPostForm の post → uploadFiles)。センシティブと
// 説明は上げる要求 (multipart) に入るので postDataJSON では読めない。上がった
// ファイルを API で読み直して確かめる。

import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { tinyPNG } from '../../../fixtures/files';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import {
  clickMenuItem,
  clickOnlyTestId,
  openPostForm,
  setPostText,
  submitPostForm,
} from '../../../fixtures/ui_post_form';

test.describe('UI: post form file attachment', () => {
  test.setTimeout(90_000);

  test('choosing a file sends fileIds, with sensitive and alt text on the file', async ({ page, baseURL, request }) => {
    const user = await signupUser(request, randomUsername('pwattach'));
    await uiSigninAs(page, request, baseURL!, user);
    await openPostForm(page);
    await setPostText(page, `pw-attach-${Date.now()}`);

    const chooser = page.waitForEvent('filechooser', { timeout: 15_000 });
    await clickOnlyTestId(page, 'post-form-attach-file');
    const fileName = `pw-attach-${Date.now()}.png`;
    await (await chooser).setFiles({ name: fileName, mimeType: 'image/png', buffer: tinyPNG });

    // 上げる前の項目のメニューで、センシティブを付けてから説明を書く。
    // 画像の前処理 (use-uploader の preprocess) の間はメニューが空 (None) で開き、開いた後に
    // 前処理が終わっても中身は変わらない (#4049 の 7 shard の run で、CI の負荷で踏んだ)。
    // 項目が出るまで、閉じて開き直す。
    const sensitiveItem = page
      .locator('[role="menu"] [role^="menuitem"]')
      .filter({ has: page.getByText('Sensitive', { exact: true }) });
    await expect(async () => {
      await clickOnlyTestId(page, 'uploader-item-menu');
      try {
        await expect(sensitiveItem).toHaveCount(1, { timeout: 3_000 });
      } catch (err) {
        // メニューが開いていないときに Esc を押すと、投稿フォームの側が閉じようとする
        const menus = page.locator('[role="menu"]');
        if ((await menus.count()) > 0) {
          await page.keyboard.press('Escape');
          await expect(menus).toHaveCount(0, { timeout: 5_000 });
        }
        throw err;
      }
    }, 'メニューに「Sensitive」が出ない').toPass({ timeout: 30_000 });
    await clickMenuItem(page, 'Sensitive');
    await clickMenuItem(page, 'Add alt text');
    const caption = `pw-caption-${Date.now()}`;
    // 説明の窓の textarea は、投稿フォームの本文の textarea の他に 1 つだけ出る
    const captionInput = page.locator('textarea:not([data-testid="post-form-text"])');
    await expect(captionInput).toHaveCount(1, { timeout: 10_000 });
    await captionInput.fill(caption);
    await clickOnlyTestId(page, 'modal-window-ok');

    const sent = await submitPostForm(page);
    expect(Array.isArray(sent.fileIds)).toBe(true);
    const fileIds = sent.fileIds as string[];
    expect(fileIds).toHaveLength(1);

    const file = await (await callApi(request, 'drive/files/show', { i: user.token, fileId: fileIds[0] })).json();
    expect(file.name).toBe(fileName);
    expect(file.isSensitive).toBe(true);
    expect(file.comment).toBe(caption);

    const notes = await (await callApi(request, 'users/notes', { i: user.token, userId: user.id })).json();
    expect(notes).toHaveLength(1);
    expect(notes[0].fileIds).toEqual(fileIds);
  });
});
