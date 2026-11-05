/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// ノートの画像をクリックするとライトボックスが開き、次へ送れて、閉じるボタンと
// Esc で閉じられることを確かめる (#4043)。
//
// 今どの画像を出しているかは、画面の左端に来ている項目 (lightbox-item) の番号で
// 見る。項目はノートの fileIds の順に並ぶ (MkMediaList の openGallery)。
// 画像は同じ中身を 2 回上げるので、同じハッシュのファイルを返さないよう force を付ける。

import { expect, test, type Page } from '@playwright/test';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { tinyPNG } from '../../../fixtures/files';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { clickWhenReady } from '../../../fixtures/ui_click';
import { clickOnlyTestId } from '../../../fixtures/ui_post_form';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';

/** Index of the lightbox item currently at the left edge of the viewport, or -1. */
function currentIndex(page: Page): Promise<number> {
  return page.evaluate(() => {
    const items = Array.from(document.querySelectorAll('[data-testid="lightbox-item"]'));
    return items.findIndex((el) => {
      const r = el.getBoundingClientRect();
      return r.width > 0 && Math.abs(r.left) < 2;
    });
  });
}

async function openImage(page: Page, name: string): Promise<void> {
  await clickWhenReady(
    page,
    `画像「${name}」`,
    (n: string) =>
      Array.from(document.querySelectorAll('[data-testid="media-image"]')).find((el) => el.querySelector(`[title="${n}"]`) !== null),
    name,
  );
  await expect(page.getByTestId('lightbox')).toBeVisible({ timeout: 10_000 });
}

test.describe('UI: note image lightbox', () => {
  test.setTimeout(90_000);

  test('open, go to the next image, and close with the button and Esc', coversRoutes('/notes/:noteId/:initialTab?'), async ({ page, baseURL, request }) => {
    const author = await signupUser(request, randomUsername('pwlbto'));
    const user = await signupUser(request, randomUsername('pwlb'));
    const names = [`pw-lb-0-${Date.now()}.png`, `pw-lb-1-${Date.now()}.png`];
    const fileIds: string[] = [];
    for (const name of names) {
      const resp = await request.post(`${baseURL}/api/drive/files/create`, {
        multipart: { i: author.token, force: 'true', file: { name, mimeType: 'image/png', buffer: tinyPNG } },
      });
      expect(resp.status()).toBe(200);
      fileIds.push((await resp.json()).id);
    }
    expect(new Set(fileIds).size).toBe(2);
    const text = `pw-lb-${Date.now()}`;
    const created = await request.post(`${baseURL}/api/notes/create`, { data: { i: author.token, text, fileIds } });
    expect(created.status()).toBe(200);
    const noteId = (await created.json()).createdNote.id;
    await uiSigninAs(page, request, baseURL!, user);

    await gotoRoute(page, baseURL!, `/notes/${noteId}`);
    await expect(page.getByTestId('media-image')).toHaveCount(2, { timeout: 20_000 });

    // 開く: 1 枚目から始まる
    await openImage(page, names[0]);
    await expect.poll(() => currentIndex(page)).toBe(0);
    await expect(page.getByTestId('lightbox-prev')).toHaveCount(0);

    // 送る: 2 枚目に移り、最後なので「次へ」が消える
    await clickOnlyTestId(page, 'lightbox-next');
    await expect.poll(() => currentIndex(page)).toBe(1);
    await expect(page.getByTestId('lightbox-next')).toHaveCount(0);

    // 閉じる (ボタン): 今の項目の閉じるボタンを押す
    await clickWhenReady(page, '今の画像の閉じるボタン', () => {
      const items = Array.from(document.querySelectorAll('[data-testid="lightbox-item"]'));
      const current = items.find((el) => Math.abs(el.getBoundingClientRect().left) < 2);
      return current?.querySelector('[data-testid="lightbox-close"]');
    });
    await expect(page.getByTestId('lightbox')).toHaveCount(0, { timeout: 10_000 });

    // 閉じる (Esc): 2 枚目から開いて Esc
    await openImage(page, names[1]);
    await expect.poll(() => currentIndex(page)).toBe(1);
    await page.keyboard.press('Escape');
    await expect(page.getByTestId('lightbox')).toHaveCount(0, { timeout: 10_000 });
  });
});
