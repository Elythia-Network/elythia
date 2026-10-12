/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/email-settings で "Save" button click → /api/admin/update-meta
// round-trip する **真の write-flow** spec。
//
// admin/email-settings.vue は SMTP / email 設定 form。toggle / input は
// 何も変更せずに Save するだけで update-meta が走る (= 現在値を再 commit)。
// frontend は disableEmail / SMTP host 等の既存値を re-send するので
// regression は起きない。
//
// 注: state mutation が発生しないので cleanup は不要 (admin_branding_save と同じ)。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonContainingText } from '../../../fixtures/ui_click';

test.describe('UI: /admin/email-settings save flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('click Save → /api/admin/update-meta round-trips', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/admin/email-settings`, { waitUntil: 'domcontentloaded' });

    // Save button が hydrate するまで待つ (textContent "Save")
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
