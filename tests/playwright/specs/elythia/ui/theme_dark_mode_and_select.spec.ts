/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// ライト/ダークの切り替え、OS の設定への追従、組み込みのテーマの選択で、画面の
// CSS の変数 (--MI_THEME-*) が変わり、再読み込みの後も残ることを確かめる (#4049)。
//
// 既定のテーマは Elythia Light / Elythia Dark (Elythia 独自。本家は Mi Light /
// Mi Green+Lime)。値は frontend-shared/themes の l-elythia / d-elythia の bg を、
// テーマの組み立て (compile) と同じく rgb() にしたもの。
//
// テーマとダークモードは端末に保存する (サーバーには無い)。再読み込みの後に、
// ページに当たった CSS の変数と <html> の data-color-scheme を読んで確かめる。
// OS の設定は emulateMedia で変える。

import { expect, type Page, test } from '@playwright/test';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { pageBody } from '../../../fixtures/route_check';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { clickByTestId, clickSwitchByLabel, clickWhenReady } from '../../../fixtures/ui_click';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';

const ELYTHIA_LIGHT_BG = 'rgb(244, 244, 248)';
const ELYTHIA_DARK_BG = 'rgb(6, 10, 31)';
const MI_LIGHT_BG = 'rgb(249, 249, 249)';

function themeState(page: Page): Promise<{ scheme: string | undefined; bg: string }> {
  return page.evaluate(() => ({
    scheme: document.documentElement.dataset.colorScheme,
    bg: document.documentElement.style.getPropertyValue('--MI_THEME-bg').trim(),
  }));
}

// 設定のテーマの画面で、テーマの一覧の項目 (ラベル) を名前で押す
function chooseTheme(page: Page, name: string): Promise<void> {
  return clickWhenReady(page, `テーマ「${name}」(ちょうど 1 つ)`, (n: string) => {
    const hits = Array.from(document.querySelectorAll('label[for^="themeRadio_"]')).filter((l) => (l.textContent ?? '').trim() === n);
    return hits.length === 1 ? hits[0] : null;
  }, name);
}

test.describe('UI: light/dark mode and built-in themes', () => {
  test.setTimeout(90_000);

  test('follows the OS setting, then a manual switch turns syncing off and survives a reload', coversRoutes('/settings/theme'), async ({ page, baseURL, request }) => {
    const user = await signupUser(request, randomUsername('pwdark'));
    await page.emulateMedia({ colorScheme: 'light' });
    await uiSigninAs(page, request, baseURL!, user);
    await expect.poll(() => themeState(page)).toEqual({ scheme: 'light', bg: ELYTHIA_LIGHT_BG });

    // 既定では OS の設定に追従する
    await page.emulateMedia({ colorScheme: 'dark' });
    await expect.poll(() => themeState(page)).toEqual({ scheme: 'dark', bg: ELYTHIA_DARK_BG });
    await page.emulateMedia({ colorScheme: 'light' });
    await expect.poll(() => themeState(page)).toEqual({ scheme: 'light', bg: ELYTHIA_LIGHT_BG });

    // 手で切り替えると、追従を切るかを聞かれ、切ってダークにする
    await gotoRoute(page, baseURL!, '/settings/theme');
    await clickByTestId(page, 'theme-dark-mode-toggle');
    await clickByTestId(page, 'modal-dialog-ok');
    await expect.poll(() => themeState(page)).toEqual({ scheme: 'dark', bg: ELYTHIA_DARK_BG });
    // 追従を切ったので、OS の設定を変えてもダークのまま。変わらないことは待てないので、
    // OS の設定を変えてから 1 秒の間、100ms ごとに見る
    await page.emulateMedia({ colorScheme: 'dark' });
    await page.emulateMedia({ colorScheme: 'light' });
    for (let i = 0; i < 10; i++) {
      expect(await themeState(page)).toEqual({ scheme: 'dark', bg: ELYTHIA_DARK_BG });
      await page.waitForTimeout(100);
    }

    await page.reload({ waitUntil: 'domcontentloaded' });
    await expect(page.getByTestId('open-post-form').first()).toBeVisible({ timeout: 20_000 });
    await expect.poll(() => themeState(page)).toEqual({ scheme: 'dark', bg: ELYTHIA_DARK_BG });

    // 追従を入れ直すと、OS (ライト) に戻る
    await gotoRoute(page, baseURL!, '/settings/theme');
    await clickSwitchByLabel(page, 'Sync Dark Mode with your device settings');
    await expect.poll(() => themeState(page)).toEqual({ scheme: 'light', bg: ELYTHIA_LIGHT_BG });
  });

  test('choosing a built-in theme changes the colors and survives a reload', coversRoutes('/settings/theme'), async ({ page, baseURL, request }) => {
    const user = await signupUser(request, randomUsername('pwtheme'));
    await page.emulateMedia({ colorScheme: 'light' });
    await uiSigninAs(page, request, baseURL!, user);
    await gotoRoute(page, baseURL!, '/settings/theme');
    await expect.poll(() => themeState(page)).toEqual({ scheme: 'light', bg: ELYTHIA_LIGHT_BG });
    await expect(pageBody(page).getByText('Theme to use in Light Mode', { exact: true })).toBeVisible();

    await chooseTheme(page, 'Mi Light');
    await expect.poll(() => themeState(page)).toEqual({ scheme: 'light', bg: MI_LIGHT_BG });
    await page.reload({ waitUntil: 'domcontentloaded' });
    await expect(page.getByTestId('open-post-form').first()).toBeVisible({ timeout: 20_000 });
    await expect.poll(() => themeState(page)).toEqual({ scheme: 'light', bg: MI_LIGHT_BG });

    // Elythia Light に戻せる
    await gotoRoute(page, baseURL!, '/settings/theme');
    await chooseTheme(page, 'Elythia Light');
    await expect.poll(() => themeState(page)).toEqual({ scheme: 'light', bg: ELYTHIA_LIGHT_BG });
    await page.reload({ waitUntil: 'domcontentloaded' });
    await expect(page.getByTestId('open-post-form').first()).toBeVisible({ timeout: 20_000 });
    await expect.poll(() => themeState(page)).toEqual({ scheme: 'light', bg: ELYTHIA_LIGHT_BG });
  });
});
