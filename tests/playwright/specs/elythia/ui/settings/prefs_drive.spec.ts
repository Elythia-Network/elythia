/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 設定の表 (#4056): /settings/drive の端末の設定。部品で値を変えて保存と再読み込みを
// 確かめた後、投稿フォームでファイルを選び、上げる前の項目 (アップローダー) に既定の値が
// 入ることで効き目を見る。
//
// ファイルは上げない。既定の値は、選んだ時点で項目に写される (use-uploader.ts の
// initializeFile) ので、項目の名前とメニューで見られる。

import { expect, type Locator, type Page, test } from '@playwright/test';
import { tinyPNG } from '../../../../fixtures/files';
import { clickOnlyTestId, openPostForm } from '../../../../fixtures/ui_post_form';
import { coversPrefs, type PrefRow, prefTitle, runPrefRow } from '../../../../fixtures/settings_table';

/** Opens the post form and chooses `file` with the attach button. */
async function attachFile(page: Page, file: { name: string; mimeType: string; buffer: Buffer }): Promise<void> {
  await openPostForm(page);
  const chooser = page.waitForEvent('filechooser', { timeout: 15_000 });
  await clickOnlyTestId(page, 'post-form-attach-file');
  await (await chooser).setFiles(file);
  await expect(page.getByTestId('uploader-item-menu')).toHaveCount(1, { timeout: 15_000 });
}

/** Opens the menu of the uploader item until it has the item whose text matches `title`. */
async function uploaderMenuItem(page: Page, title: RegExp): Promise<Locator> {
  const item = page.locator('[role="menu"] [role^="menuitem"]').filter({ hasText: title });
  // 前処理 (use-uploader の preprocess) の間はメニューが空で開き、開いた後に前処理が
  // 終わっても中身は変わらない (post_form_attach_file.spec.ts と同じ)。出るまで開き直す
  await expect(async () => {
    await clickOnlyTestId(page, 'uploader-item-menu');
    try {
      await expect(item).toHaveCount(1, { timeout: 3_000 });
    } catch (err) {
      // メニューが開いていないときに Esc を押すと、投稿フォームの側が閉じようとする
      const menus = page.locator('[role="menu"]');
      if ((await menus.count()) > 0) {
        await page.keyboard.press('Escape');
        await expect(menus).toHaveCount(0, { timeout: 5_000 });
      }
      throw err;
    }
  }, `アップローダーのメニューに ${title} が出ない`).toPass({ timeout: 30_000 });
  return item;
}

const WATERMARK_PRESET = { id: 'pw-wm-preset', name: 'pw-wm', layers: [] };

test.describe('settings: drive', () => {
  test.setTimeout(60_000);
  const ROWS: PrefRow[] = [
    {
      key: 'keepOriginalFilename', route: '/settings/drive', control: { kind: 'switch', on: false }, value: false,
      effect: async ({ page }) => {
        await attachFile(page, { name: 'pw-keep-original.png', mimeType: 'image/png', buffer: tinyPNG });
        // 元の名前を残さないと、項目の名前はクライアントの genId (10 文字) と拡張子になる
        const item = page.getByTestId('uploader-item-menu').locator('xpath=../..');
        await expect(item).toContainText(/[0-9a-z]{10}\.png/);
        await expect(item).not.toContainText('pw-keep-original');
      },
    },
    {
      key: 'defaultImageCompressionLevel', route: '/settings/drive', control: { kind: 'select', label: 'None' }, value: 0,
      effect: async ({ page }) => {
        await attachFile(page, { name: 'pw-compress.png', mimeType: 'image/png', buffer: tinyPNG });
        await expect(await uploaderMenuItem(page, /Compress/)).toContainText('Compress: None');
      },
    },
    {
      key: 'defaultVideoCompressionLevel', route: '/settings/drive', control: { kind: 'select', label: 'None' }, value: 0,
      effect: async ({ page }) => {
        // 中身は動画でなくてよい。圧縮の前処理は失敗しても握りつぶされ、メニューは同じく出る
        await attachFile(page, { name: 'pw-compress.mp4', mimeType: 'video/mp4', buffer: Buffer.from('pw-not-a-real-video') });
        await expect(await uploaderMenuItem(page, /Compress/)).toContainText('Compress: None');
      },
    },
    {
      key: 'defaultWatermarkPresetId', route: '/settings/drive', control: { kind: 'select', label: 'pw-wm' }, value: 'pw-wm-preset',
      // 選べるプリセットが要る。ウォーターマークの部品は、ポリシーの watermarkAvailable (既定は true) のときだけ出る
      pre: { watermarkPresets: [WATERMARK_PRESET] },
      effect: async ({ page }) => {
        await attachFile(page, { name: 'pw-watermark.png', mimeType: 'image/png', buffer: tinyPNG });
        // 既定のプリセットが掛かると、メニューの「Watermark」の caption にプリセットの名前が出る
        await expect(await uploaderMenuItem(page, /Watermark/)).toContainText('pw-wm');
      },
    },
  ];
  for (const row of ROWS) test(prefTitle(row), coversPrefs(row.key), ({ page, request, baseURL }) => runPrefRow({ page, request, baseURL }, row));
});
