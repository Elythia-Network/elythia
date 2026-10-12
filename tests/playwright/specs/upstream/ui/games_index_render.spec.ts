/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /games page で bubble-game / reversi の link panel が hydrate される
// ことを smoke する spec。
//
// /games は MkA (link) を 2 つ render する単純な page。logo image の
// /url 属性で hydration sign を取る。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /games page renders game links', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('games index renders both bubble-game and reversi links', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/games`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // bubble-game / reversi の <a href> が render される
    await page.waitForFunction(
      () => {
        const links = Array.from(document.querySelectorAll('a')).map((a) => a.href);
        return (
          links.some((h) => h.endsWith('/bubble-game') || h.includes('/bubble-game')) &&
          links.some((h) => h.endsWith('/reversi') || h.includes('/reversi'))
        );
      },
      { timeout: 20_000 },
    );
  });
});
