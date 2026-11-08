/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// デッキのカラムと、通常の UI の右の列のウィジェットを画面で操作する (#4072)。どちらも API を呼ばず、
// 端末の設定 (deck.profiles / widgets) に入るので、readPref で確かめる。
//
//   デッキ       メニューの「+」からカラムを足し、カラムの「…」の「Column settings」で名前を変え、
//                「Delete」で消す。メイン列が 1 つだけのデッキのプロファイルを持ち込んで始める
//                (prefs_deck.spec.ts と同じ)
//   UI の切替   ナビの「Switch UI」で「Deck」を選ぶと、デッキの UI で読み込み直す (localStorage の ui)
//   ウィジェット 「Edit widgets」から時計を足し、歯車で「透明」にして保存し、「x」で消す
//
// デッキの部品は CSS Modules のクラスしか持たないので、タグ (section / header) とアイコンの素の
// クラス (ti-*) で見る。ウィジェットの部品は本家にもある data-testid で取る。

import { expect, type Locator, type Page, test } from '@playwright/test';
import { pickSelect, setSwitch } from '../../../fixtures/admin_table';
import { readPref, signinWithState } from '../../../fixtures/settings_table';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';
import { clickMenuItem, freshUser } from '../../../fixtures/ui_timeline';

const MAIN_COLUMN_DECK: Record<string, unknown> = {
  'deck.profile': 'pw-deck',
  'deck.profiles': [{
    id: 'pw-deck-profile',
    name: 'pw-deck',
    columns: [{ id: 'pw-main', type: 'main', name: null, width: 330 }],
    layout: [['pw-main']],
  }],
};

type Column = { id: string; type: string; name: string | null };

async function columns(page: Page): Promise<Column[]> {
  const profiles = await readPref(page, 'deck.profiles') as { columns: Column[] }[] | undefined;
  return profiles?.[0]?.columns ?? [];
}

/** The "..." button in the header of the deck column whose title is `title`. */
function columnMenuButton(page: Page, title: string): Locator {
  return page.locator('section').filter({ has: page.locator('header', { hasText: title }) }).locator('header button:has(i.ti-dots)');
}

test.describe('UI: deck and widgets', () => {
  test.setTimeout(60_000);

  test('add, configure and delete a deck column', coversRoutes('/'), async ({ page, request, baseURL }) => {
    const me = await freshUser(request, 'pwdeck');
    await signinWithState(page, request, baseURL!, me, MAIN_COLUMN_DECK);
    await page.evaluate(() => window.localStorage.setItem('ui', 'deck'));
    await gotoRoute(page, baseURL!, '/');
    await expect(page.locator('section'), 'メイン列だけのデッキで始まらない (前提)').toHaveCount(1, { timeout: 20_000 });

    // デッキのメニュー (列の外) の「+」
    await page.locator('xpath=//button[i[contains(concat(" ", normalize-space(@class), " "), " ti-plus ")]][not(ancestor::section)]').first().click();
    // 「+」のツールチップにも「Add column」が出るので、OK のボタンからダイアログの枠をたどる
    await pickSelect(page, page.getByTestId('modal-dialog-ok').locator('xpath=ancestor::div[.//*[@tabindex="0"]][1]'), 'Notifications');
    await page.getByTestId('modal-dialog-ok').click();
    await expect.poll(async () => (await columns(page)).map((c) => c.type), { message: 'カラムが足されない' }).toEqual(['main', 'notifications']);
    await expect(page.locator('section')).toHaveCount(2);

    const name = `pw col ${Date.now().toString(36)}`;
    await columnMenuButton(page, 'Notifications').click();
    await clickMenuItem(page, 'Column settings');
    // 名前の欄は設定のフォームの最初の入力欄
    await page.getByTestId('modal-window-ok').locator('xpath=ancestor::div[.//input][1]').locator('input').first().fill(name);
    await page.getByTestId('modal-window-ok').click();
    await expect.poll(async () => (await columns(page)).find((c) => c.type === 'notifications')?.name, { message: 'カラムの名前が保存されない' }).toBe(name);
    await expect(page.locator('section header', { hasText: name }), '見出しに名前が出ない').toHaveCount(1);

    await columnMenuButton(page, name).click();
    await clickMenuItem(page, 'Delete');
    await expect.poll(async () => (await columns(page)).map((c) => c.type), { message: 'カラムが消えない' }).toEqual(['main']);
    await expect(page.locator('section')).toHaveCount(1);
  });

  test('switch to the deck UI from the navigation', coversRoutes('/'), async ({ page, request, baseURL }) => {
    const me = await freshUser(request, 'pwswui');
    await signinWithState(page, request, baseURL!, me, MAIN_COLUMN_DECK);
    await gotoRoute(page, baseURL!, '/');
    await expect(page.locator('section'), '通常の UI ではデッキのカラムが無い (対照)').toHaveCount(0);
    // ナビの「Switch UI」で「Deck」を選ぶと、localStorage の ui を書いて読み込み直す
    await page.locator('button, a').filter({ hasText: /^\s*Switch UI\s*$/ }).first().click();
    const reloaded = page.waitForEvent('load', { timeout: 20_000 });
    await clickMenuItem(page, 'Deck');
    await reloaded;
    expect(await page.evaluate(() => window.localStorage.getItem('ui')), 'UI の選択が保存されない').toBe('deck');
    await expect(page.locator('section'), 'デッキの UI にならない').toHaveCount(1, { timeout: 20_000 });
  });

  test('add, configure and remove a widget', coversRoutes('/'), async ({ page, request, baseURL }) => {
    const me = await freshUser(request, 'pwwdg');
    // 右の列は幅 1100 以上で出る (signinWithState は 1280x900)
    await signinWithState(page, request, baseURL!, me);
    await gotoRoute(page, baseURL!, '/');
    type Widget = { name: string; data: Record<string, unknown> };
    const widgets = async () => (await readPref(page, 'widgets') as Widget[] | undefined) ?? [];
    const before = (await widgets()).filter((w) => w.name === 'clock').length;
    const clocks = page.getByTestId('mkw-clock');
    const shownBefore = await clocks.count();

    await page.getByTestId('widget-edit').click();
    await pickSelect(page, page.getByTestId('widget-select'), 'Clock');
    await page.getByTestId('widget-add').click();
    await expect.poll(async () => (await widgets()).filter((w) => w.name === 'clock').length, { message: 'ウィジェットが足されない' }).toBe(before + 1);
    await expect(clocks).toHaveCount(shownBefore + 1);

    // 足したもの (いちばん上に入る) の歯車で設定を開き、「透明」にして保存する
    const container = page.getByTestId('customize-container').filter({ has: clocks }).first();
    await container.locator('button:has(i.ti-settings)').click();
    const dialog = page.getByTestId('modal-window-ok').locator('xpath=ancestor::div[.//*[@data-checked]][1]');
    await setSwitch(dialog.locator('[data-checked]').first(), true);
    await page.getByTestId('modal-window-ok').click();
    await expect.poll(async () => (await widgets()).some((w) => w.name === 'clock' && w.data.transparent === true), { message: '設定が保存されない' }).toBe(true);

    await container.getByTestId('customize-container-remove').click();
    await expect.poll(async () => (await widgets()).filter((w) => w.name === 'clock').length, { message: 'ウィジェットが消えない' }).toBe(before);
  });
});
