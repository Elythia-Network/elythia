/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /settings/privacy で 3 番目 switch (publicReactions) を click →
// /api/i/update が走ることを verify する **真の write-flow** spec。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickWhenReady } from '../../../fixtures/ui_click';

test.describe('UI: /settings/privacy publicReactions toggle flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('toggle publicReactions switch (3rd) → /api/i/update round-trips', async ({
    page,
    baseURL,
    request,
  }) => {
    // 値 strict assertion のため初期 state を true (= DB default) に reset。
    await callApi(request, 'i/update', { i: root.token, publicReactions: true });

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/settings/privacy`, { waitUntil: 'domcontentloaded' });

    await page.waitForFunction(
      () => document.querySelectorAll('input[type="checkbox"]').length >= 3,
      { timeout: 20_000 },
    );

    const updateResp = page.waitForResponse(
      (r) => r.url().includes('/api/i/update') && r.status() === 200,
      { timeout: 15_000 },
    );
    await clickWhenReady(page, '3 番目の checkbox', () => {
      // privacy.vue: index 2 = publicReactions
      const cbs = Array.from(
        document.querySelectorAll('input[type="checkbox"]'),
      ) as HTMLInputElement[];
      return cbs[2];
    });
    const update = await updateResp;
    const body = await update.json();
    // beforeAll の API reset で true から始まるので、click 後は必ず false。
    expect(body.publicReactions).toBe(false);
  });
});
