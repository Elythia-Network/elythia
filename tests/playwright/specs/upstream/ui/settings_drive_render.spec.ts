/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /settings/drive page で usage amount + drive 関連 form が hydrate される
// smoke spec。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /settings/drive page hydrates', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('drive settings renders Drive + Usage labels', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/settings/drive`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // i18n.ts.drive → "Drive" + i18n.ts.usageAmount → "Usage" or similar
    await page.waitForFunction(
      () => {
        const text = document.body.textContent ?? '';
        return text.includes('Drive') && text.includes('Usage');
      },
      { timeout: 20_000 },
    );
  });
});
