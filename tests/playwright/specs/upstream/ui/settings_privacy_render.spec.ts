/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /settings/privacy page で MkSwitch 等が hydrate される smoke spec。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /settings/privacy page hydrates', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('privacy page renders Privacy + multiple switches', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/settings/privacy`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // i18n.ts.privacy → "Privacy" + MkSwitch (= input[type=checkbox]) >=2
    await page.waitForFunction(
      () => {
        const text = document.body.textContent ?? '';
        const checkboxes = document.querySelectorAll('input[type="checkbox"]').length;
        return text.includes('Privacy') && checkboxes >= 2;
      },
      { timeout: 20_000 },
    );
  });
});
