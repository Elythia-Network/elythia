/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 設定の表 (#4056): /settings/emoji-palette の端末の設定。部品で値を変えて保存と
// 再読み込みを確かめた後、絵文字ピッカーで効き目を見る。
//
// ピッカーの大きさ・列の数・高さ・出し方は、同じページの「Preview」で開くピッカーの
// 素のクラス (`omfetrab` に付く `s<N>` / `w<N>` / `h<N>` / `asDrawer`) で見る。
// 使うパレットは、ピッカーの先頭の固定の欄 (見出しの無い section) に出る絵文字で見る。
// パレットを決めないとき (Auto) は、最初のパレットが使われる (utility/emoji-picker.ts)。

import { expect, type Page, test } from '@playwright/test';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { createNote } from '../../../../fixtures/notes';
import { clickButtonByText } from '../../../../fixtures/ui_click';
import { clickOnlyTestId, openPostForm } from '../../../../fixtures/ui_post_form';
import { gotoRoute } from '../../../../fixtures/ui_route';
import { coversPrefs, type PrefRow, prefTitle, runPrefRow } from '../../../../fixtures/settings_table';

/**
 * Two palettes: the first (used when no palette is chosen) and the one the
 * rows choose. Their emojis tell which palette the picker shows.
 */
const PALETTES = [
  { id: 'pw-pal-a', name: 'pw-palette-a', emojis: ['\u{1F363}'] },
  { id: 'pw-pal-b', name: 'pw-palette-b', emojis: ['\u{1F36E}'] },
];

/** The emojis in the pinned (palette) part of the open picker, or null before it opens. */
function pinnedEmojis(page: Page): Promise<string[] | null> {
  return page.evaluate(() => {
    const section = document.querySelector('.omfetrab .index > section:not(:has(> header))');
    return section ? Array.from(section.querySelectorAll('[data-emoji]')).map((el) => el.getAttribute('data-emoji') ?? '') : null;
  });
}

/** Opens the picker with the "Preview" button of the settings page. */
async function openPreviewPicker(page: Page): Promise<void> {
  await clickButtonByText(page, 'Preview');
  await expect(page.locator('.omfetrab')).toHaveCount(1);
}

test.describe('settings: emoji palette', () => {
  test.setTimeout(60_000);
  const ROWS: PrefRow[] = [
    {
      key: 'emojiPaletteForMain', route: '/settings/emoji-palette', control: { kind: 'select', label: 'pw-palette-b' }, value: 'pw-pal-b',
      pre: { emojiPalettes: PALETTES },
      effect: async ({ page }) => {
        await openPostForm(page);
        await clickOnlyTestId(page, 'post-form-emoji');
        await expect.poll(() => pinnedEmojis(page)).toEqual(['\u{1F36E}']);
      },
    },
    {
      key: 'emojiPaletteForReaction', route: '/settings/emoji-palette', control: { kind: 'select', label: 'pw-palette-b' }, value: 'pw-pal-b',
      pre: { emojiPalettes: PALETTES },
      effect: async ({ page, request, baseURL }) => {
        const author = await signupUser(request, randomUsername('pwpalnote'));
        const note = await createNote(request, author.token, { text: `pw-palette-${Date.now()}` });
        await gotoRoute(page, baseURL, `/notes/${note.id}`);
        await expect(page.getByText(note.text!, { exact: true })).toBeVisible({ timeout: 20_000 });
        await clickOnlyTestId(page, 'note-react');
        await expect.poll(() => pinnedEmojis(page)).toEqual(['\u{1F36E}']);
      },
    },
    {
      key: 'emojiPickerScale', route: '/settings/emoji-palette', control: { kind: 'radio', label: 'Big++' }, value: 5,
      effect: async ({ page }) => {
        await openPreviewPicker(page);
        await expect(page.locator('.omfetrab')).toHaveClass(/(^|\s)s5(\s|$)/);
      },
    },
    {
      key: 'emojiPickerWidth', route: '/settings/emoji-palette', control: { kind: 'radio', label: '9' }, value: 5,
      effect: async ({ page }) => {
        await openPreviewPicker(page);
        await expect(page.locator('.omfetrab')).toHaveClass(/(^|\s)w5(\s|$)/);
      },
    },
    {
      key: 'emojiPickerHeight', route: '/settings/emoji-palette', control: { kind: 'radio', label: 'Small' }, value: 1,
      effect: async ({ page }) => {
        await openPreviewPicker(page);
        await expect(page.locator('.omfetrab')).toHaveClass(/(^|\s)h1(\s|$)/);
      },
    },
    {
      key: 'emojiPickerStyle', route: '/settings/emoji-palette', control: { kind: 'select', label: 'Drawer' }, value: 'drawer',
      effect: async ({ page }) => {
        // 既定 (Auto) は、広い画面では popup で開く
        await openPreviewPicker(page);
        await expect(page.locator('.omfetrab')).toHaveClass(/(^|\s)asDrawer(\s|$)/);
      },
    },
  ];
  for (const row of ROWS) test(prefTitle(row), coversPrefs(row.key), ({ page, request, baseURL }) => runPrefRow({ page, request, baseURL }, row));
});
