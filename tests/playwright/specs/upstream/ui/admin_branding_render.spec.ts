/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/branding page で MkRadios + MkSwitch + MkInput が hydrate される
// ことを verify する spec。
//
// /admin/branding は server settings (entrancePageStyle / iconUrl 等) を
// admin/meta + admin/update-meta 経由で読み書きする。本 spec は controls
// が mount されることだけを smoke する (= hidden form なら必ず複数 input
// が render される)。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /admin/branding page hydrates form controls', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('branding form controls (radios / switches / inputs) appear', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/admin/branding`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // /admin/branding 固有 sign: definePage の title が i18n.ts.branding
    // (= "Branding") + iconUrl の MkInput type=url が必ず存在する。
    // 単に input 数だけだと home dashboard の widgets でも通るので、
    // 「Branding」文字列 + url input >=1 + 全 input >=5 の AND で固有性確保。
    await page.waitForFunction(
      () => {
        const text = document.body.textContent ?? '';
        const allInputs = document.querySelectorAll('input').length;
        const urlInputs = document.querySelectorAll('input[type="url"]').length;
        return text.includes('Branding') && allInputs >= 5 && urlInputs >= 1;
      },
      { timeout: 20_000 },
    );
  });
});
