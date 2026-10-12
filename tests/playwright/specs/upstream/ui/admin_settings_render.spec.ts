/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/settings page (general settings) で form controls が hydrate
// されることを smoke する spec。
//
// /admin/settings は admin/meta + admin/update-meta 経由で各種一般設定を
// read/write する form の集合 (instance name / description / maintainer
// 等)。本 spec は page title "General" + input >=5 で固有性確保。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /admin/settings (general) page hydrates form controls', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('general settings form hydrates with multiple inputs', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/admin/settings`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // page title (i18n.ts.general → "General") + form input が必要数 mount
    await page.waitForFunction(
      () => {
        const text = document.body.textContent ?? '';
        const inputs = document.querySelectorAll('input').length;
        return text.includes('General') && inputs >= 5;
      },
      { timeout: 20_000 },
    );
  });
});
