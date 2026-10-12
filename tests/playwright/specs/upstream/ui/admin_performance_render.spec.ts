/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/performance page で server machine stats / quota などの
// performance settings form が hydrate されることを smoke する spec。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /admin/performance page hydrates form', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('performance settings form hydrates', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/admin/performance`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    // page title (i18n.ts.performance → "Performance")
    // + server machine stats の MkSwitch + remote 各種 limit の MkInput
    await page.waitForFunction(
      () => {
        const text = document.body.textContent ?? '';
        const inputs = document.querySelectorAll('input').length;
        return text.includes('Performance') && inputs >= 2;
      },
      { timeout: 20_000 },
    );
  });
});
