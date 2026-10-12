/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /about-misskey page (= 公開 Misskey ソフトウェア説明) で
// "Misskey" 文字列が render されることを smoke する spec。anonymous
// access OK の public route。

import { expect, test } from '@playwright/test';

test.describe('UI: /about-misskey renders public software intro', () => {
  test.setTimeout(30_000);

  test('"Misskey" appears on /about-misskey', async ({ page, baseURL }) => {
    await page.setViewportSize({ width: 1600, height: 900 });
    const resp = await page.goto(`${baseURL}/about-misskey`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    await page.waitForFunction(
      () => (document.body.textContent ?? '').includes('Misskey'),
      { timeout: 20_000 },
    );
  });
});
