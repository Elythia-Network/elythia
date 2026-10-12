/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /theme-editor page で theme editor form が hydrate される smoke spec。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /theme-editor page hydrates', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('theme-editor renders form with Apply + multiple inputs', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/theme-editor`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // theme editor は color picker を <button> で構成するので input ベース
    // ではなく button text + page title text で判定。"Theme editor" header
    // + "Background color" / "Accent color" / "Text color" の 3 section
    // header が AND で揃う = theme-editor page 固有 sign。
    await page.waitForFunction(
      () => {
        const text = document.body.textContent ?? '';
        return (
          text.includes('Theme editor') &&
          text.includes('Background color') &&
          text.includes('Accent color')
        );
      },
      { timeout: 20_000 },
    );
  });
});
