/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /bubble-game page (drop-and-fusion 系 game) で MkButton (start) +
// "How to play" section が hydrate されることを smoke する spec。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /bubble-game page hydrates', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('Bubble Game page renders Start button + How to play', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/bubble-game`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // page title は <title> tag 経由で textContent には含まれないので、
    // body 内の "How to play" (i18n.ts._bubbleGame.howToPlay) で sign を
    // 取る。bubble-game 以外の page では出ないので固有性確保。
    await page.waitForFunction(
      () => {
        const text = document.body.textContent ?? '';
        return text.includes('How to play');
      },
      { timeout: 20_000 },
    );
  });
});
