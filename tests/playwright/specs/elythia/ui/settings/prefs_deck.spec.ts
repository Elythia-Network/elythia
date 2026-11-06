/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 設定の表 (#4056): /settings/deck の端末の設定。部品で値を変えて保存と再読み込みを
// 確かめた後、デッキの UI (localStorage の `ui` を `deck` にして開き直す) で効き目を見る。
//
// 列を使う行は、メイン列が 1 つだけのデッキのプロファイルを `pre` で持ち込む。列が
// 無いデッキは案内 (onboarding) だけを真ん中に出すので、寄せ方や列の間隔に差が出ない。
// デッキの部品は CSS Modules のクラスしか持たないので、タグ (`section` / `header`) と
// アイコンの素のクラス (`ti-*`) と位置で見る。

import { expect, type Locator, type Page, test } from '@playwright/test';
import { gotoRoute } from '../../../../fixtures/ui_route';
import { coversPrefs, type PrefRow, prefTitle, runPrefRow } from '../../../../fixtures/settings_table';

/** A deck profile with a single main column, so that columns are laid out. */
const MAIN_COLUMN_DECK: Record<string, unknown> = {
  'deck.profile': 'pw-deck',
  'deck.profiles': [{
    id: 'pw-deck-profile',
    name: 'pw-deck',
    columns: [{ id: 'pw-main', type: 'main', name: null, width: 330 }],
    layout: [['pw-main']],
  }],
};

/** Switches to the deck UI and opens `path`. */
async function openDeck(page: Page, baseURL: string, path = '/'): Promise<void> {
  await page.evaluate(() => window.localStorage.setItem('ui', 'deck'));
  await gotoRoute(page, baseURL, path);
}

/**
 * A button of the deck menu (right or bottom) carrying the icon `icon`.
 * Buttons inside the columns (`section`) are left out.
 */
function deckMenuButton(page: Page, icon: string): Locator {
  return page.locator(`xpath=//button[i[contains(concat(" ", normalize-space(@class), " "), " ${icon} ")]][not(ancestor::section)]`);
}

/** The header of the (only) deck column. */
function columnHeader(page: Page): Locator {
  return page.locator('section header[draggable="true"]').first();
}

test.describe('settings: deck', () => {
  test.setTimeout(60_000);
  const ROWS: PrefRow[] = [
    {
      key: 'deck.alwaysShowMainColumn', route: '/settings/deck', control: { kind: 'switch', on: false }, value: false,
      pre: MAIN_COLUMN_DECK,
      effect: async ({ page, baseURL }) => {
        await openDeck(page, baseURL);
        // デッキが出ていることを先に確かめてから、トップ (index) でメイン列が出ないことを見る
        await expect(deckMenuButton(page, 'ti-trash')).toBeVisible();
        await expect(page.locator('section header[draggable="true"]')).toHaveCount(0);
      },
    },
    {
      key: 'deck.columnAlign', route: '/settings/deck', control: { kind: 'radio', label: 'Left' }, value: 'left',
      pre: MAIN_COLUMN_DECK,
      effect: async ({ page, baseURL }) => {
        await openDeck(page, baseURL);
        const header = columnHeader(page);
        await expect(header).toBeVisible();
        // 真ん中寄せでは、最初の列の左に auto の余白が付く。左寄せでは列の並びの padding (間隔) だけになる
        const offset = await header.evaluate((el) => {
          const section = el.closest('section')!;
          return section.getBoundingClientRect().left - section.parentElement!.getBoundingClientRect().left;
        });
        expect(offset).toBeLessThan(20);
      },
    },
    {
      key: 'deck.columnGap', route: '/settings/deck', control: { kind: 'range', value: 100, min: 3, max: 100 }, value: 100,
      pre: MAIN_COLUMN_DECK,
      effect: async ({ page, baseURL }) => {
        await openDeck(page, baseURL);
        const header = columnHeader(page);
        await expect(header).toBeVisible();
        // 列の並び (section の親) の gap が --columnGap になる
        await expect.poll(() => header.evaluate((el) => getComputedStyle(el.closest('section')!.parentElement!).columnGap)).toBe('100px');
      },
    },
    {
      key: 'deck.menuPosition', route: '/settings/deck', control: { kind: 'radio', label: 'Right' }, value: 'right',
      effect: async ({ page, baseURL }) => {
        await openDeck(page, baseURL);
        const trash = deckMenuButton(page, 'ti-trash');
        await expect(trash).toBeVisible();
        // 右のメニューは幅 32px で画面の右端に付く。下のメニューでは、このボタンは左端 (ナビの隣) にある
        const box = await trash.boundingBox();
        const width = page.viewportSize()!.width;
        expect(box!.x).toBeGreaterThan(width - 64);
      },
    },
    {
      key: 'deck.navWindow', route: '/settings/deck', control: { kind: 'switch', on: false }, value: false,
      pre: MAIN_COLUMN_DECK,
      effect: async ({ page, baseURL }) => {
        await openDeck(page, baseURL);
        await expect(columnHeader(page)).toBeVisible();
        // ナビの「Settings」を押す。窓で開く (既定) と URL は / のまま、メイン列で開くと URL が変わる
        await page.locator('xpath=//a[@href="/settings"][not(ancestor::section)]').first().click();
        await expect(page).toHaveURL(`${baseURL}/settings`);
      },
    },
    {
      key: 'deck.navbarPosition', route: '/settings/deck', control: { kind: 'radio', label: 'Top' }, value: 'top',
      pre: MAIN_COLUMN_DECK,
      effect: async ({ page, baseURL }) => {
        await openDeck(page, baseURL);
        const header = columnHeader(page);
        await expect(header).toBeVisible();
        // ナビの「Timeline」(ホームのアイコン) が列より上にある。左のナビでは列と同じ高さに並ぶ
        const home = page.locator('xpath=//a[@href="/"][.//i[contains(concat(" ", normalize-space(@class), " "), " ti-home ")]][not(ancestor::section)]').first();
        await expect(home).toBeVisible();
        const h = await home.boundingBox();
        const c = await header.boundingBox();
        expect(c!.y).toBeGreaterThanOrEqual(h!.y + h!.height);
      },
    },
    {
      key: 'deck.useSimpleUiForNonRootPages', route: '/settings/deck', control: { kind: 'switch', on: false }, value: false,
      effect: async ({ page, baseURL }) => {
        // / 以外を直接開く。既定ではデッキでなく zen の UI になり、デッキのメニューが無い
        await openDeck(page, baseURL, '/settings/deck');
        await expect(deckMenuButton(page, 'ti-trash')).toBeVisible();
      },
    },
  ];
  for (const row of ROWS) test(prefTitle(row), coversPrefs(row.key), ({ page, request, baseURL }) => runPrefRow({ page, request, baseURL }, row));
});
