/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// テーマのインストール (/settings/theme/install) でコードを入れると、テーマの一覧の
// 「Installed themes」に出て、選ぶと画面の色 (CSS の変数) がそのテーマの値になり、
// 再読み込みの後も残ることを確かめる (#4049)。
//
// 入れたテーマは端末に保存する (テーマの同期を入れていなければ、サーバーには無い)。

import { randomUUID } from 'node:crypto';
import { expect, type Page, test } from '@playwright/test';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { pageBody } from '../../../fixtures/route_check';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { clickButtonByText, clickByTestId, clickWhenReady } from '../../../fixtures/ui_click';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';

function themeBg(page: Page): Promise<string> {
  return page.evaluate(() => document.documentElement.style.getPropertyValue('--MI_THEME-bg').trim());
}

test.describe('UI: install a theme', () => {
  test.setTimeout(90_000);

  test('an installed theme is listed, applies its colors, and survives a reload', coversRoutes('/settings/theme/install', '/settings/theme'), async ({ page, baseURL, request }) => {
    const user = await signupUser(request, randomUsername('pwthinst'));
    const name = `PW Theme ${Date.now()}`;
    const code = `{ id: '${randomUUID()}', name: '${name}', author: 'pw', base: 'light', props: { bg: '#123456' } }`;
    await page.emulateMedia({ colorScheme: 'light' });
    await uiSigninAs(page, request, baseURL!, user);
    await gotoRoute(page, baseURL!, '/settings/theme/install');

    const editor = pageBody(page).locator('textarea');
    await expect(editor).toHaveCount(1, { timeout: 15_000 });
    await editor.fill(code);
    await clickButtonByText(page, 'Install');
    await expect(page.getByText(`${name} has been installed`, { exact: true })).toBeVisible({ timeout: 10_000 });
    await clickByTestId(page, 'modal-dialog-ok');
    await expect(page).toHaveURL(/\/settings\/theme$/, { timeout: 15_000 });

    // 一覧の「Installed themes」に出て、選ぶと色が変わる
    await expect(pageBody(page).getByText('Installed themes', { exact: true })).toBeVisible({ timeout: 15_000 });
    expect(await themeBg(page)).not.toBe('rgb(18, 52, 86)');
    await clickWhenReady(page, `テーマ「${name}」`, (n: string) => {
      const hits = Array.from(document.querySelectorAll('label[for^="themeRadio_"]')).filter((l) => (l.textContent ?? '').trim() === n);
      return hits.length === 1 ? hits[0] : null;
    }, name);
    await expect.poll(() => themeBg(page)).toBe('rgb(18, 52, 86)');

    await page.reload({ waitUntil: 'domcontentloaded' });
    await expect(page.getByTestId('open-post-form').first()).toBeVisible({ timeout: 20_000 });
    await expect.poll(() => themeBg(page)).toBe('rgb(18, 52, 86)');
  });
});
