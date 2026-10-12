/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/external-services で 1 番目の MkFolder (Google Analytics) を
// expand → Save click → /api/admin/update-meta round-trip する
// **真の write-flow** spec。
//
// external-services.vue では各サービス別に MkFolder + Save button を
// 持つ。folder は SearchMarker 経由で defaultOpen でないので、folder
// header を expand する必要あり。1 番目 = Google Analytics。
//
// 注: state mutation が発生しないので cleanup は不要 (admin_branding_save と同じ)。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonByText, clickByTestId } from '../../../fixtures/ui_click';

test.describe('UI: /admin/external-services Google Analytics save flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('expand folder → Save → /api/admin/update-meta round-trips', async ({
    page,
    baseURL,
  }) => {
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/admin/external-services`, {
      waitUntil: 'domcontentloaded',
    });

    // 1 つ目の folder header (Google Analytics) を click して expand
    await clickByTestId(page, 'folder-header');

    // Save button が visible になるまで待つ
    await page.waitForFunction(
      () => {
        const btns = Array.from(document.querySelectorAll('button'));
        return btns.some((b) => (b.textContent ?? '').trim() === 'Save');
      },
      { timeout: 10_000 },
    );

    const updateResp = page.waitForResponse(
      (r) => r.url().includes('/api/admin/update-meta') && r.status() < 400,
      { timeout: 15_000 },
    );
    await clickButtonByText(page, 'Save');
    const resp = await updateResp;
    expect(resp.status()).toBeLessThan(400);
  });
});
