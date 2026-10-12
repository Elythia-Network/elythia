/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /reversi page (Reversi game lobby) で MkButton (matchAny / matchUser /
// cancel) が hydrate されることを smoke する spec。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /reversi lobby page hydrates', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('Reversi lobby renders match buttons', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/reversi`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // i18n.ts._reversi.freeMatch → "Free Match" button text が body に出る
    // (= reversi lobby 固有)。page header の title は <title> tag に入って
    // textContent に出ないので、本体 button text で sign を取る。
    await page.waitForFunction(
      () => {
        const text = document.body.textContent ?? '';
        const buttons = document.querySelectorAll('button').length;
        return text.includes('Free Match') && buttons >= 2;
      },
      { timeout: 20_000 },
    );
  });
});
