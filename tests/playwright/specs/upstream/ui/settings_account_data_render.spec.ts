/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /settings/account-data page で export/import section が hydrate される
// smoke spec。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /settings/account-data page hydrates', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('account-data renders export/import sections', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/settings/account-data`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    // i18n.ts._exportOrImport.allNotes → "All notes"。home dashboard
    // にはこの文字列無いので固有 sign。
    await page.waitForFunction(
      () => {
        const text = document.body.textContent ?? '';
        return text.includes('All notes');
      },
      { timeout: 20_000 },
    );
  });
});
