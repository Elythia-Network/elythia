/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/branding で "Save" button click → /api/admin/update-meta
// round-trip する **真の write-flow** spec。
//
// admin/branding.vue はインスタンスのアイコン / 名前 / 説明等の form。
// Save click で現在値を commit (branding.vue:148 / 197)。
//
// 注: 本 spec は form の field を変更せず**現状値をそのまま再 commit する**
// だけなので state mutation が発生せず、cleanup は不要。将来的に field を
// 書き換える test を追加するなら try/finally で原値復元が必要 (#974 review)。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonContainingText } from '../../../fixtures/ui_click';

test.describe('UI: /admin/branding save flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('click Save → /api/admin/update-meta round-trips', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/admin/branding`, { waitUntil: 'domcontentloaded' });

    await page.waitForFunction(
      () => {
        const btns = Array.from(document.querySelectorAll('button'));
        return btns.some((b) => (b.textContent ?? '').includes('Save'));
      },
      { timeout: 20_000 },
    );

    const updateResp = page.waitForResponse(
      (r) => r.url().includes('/api/admin/update-meta') && r.status() < 400,
      { timeout: 15_000 },
    );
    await clickButtonContainingText(page, 'Save');
    const resp = await updateResp;
    expect(resp.status()).toBeLessThan(400);
  });
});
