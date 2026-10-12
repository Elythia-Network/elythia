/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /settings/privacy で autoAcceptFollowed switch (2nd) を click → /api/i/update
// が round-trip する write-flow spec。
//
// privacy.vue の MkSwitch 順は isLocked / autoAcceptFollowed / publicReactions
// / hideOnlineStatus / noCrawle / preventAiLearning / isExplorable。本 spec は
// 2 番目の switch を click することで、最初の switch だけ動作する偽陽性を排除。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickWhenReady } from '../../../fixtures/ui_click';

test.describe('UI: /settings/privacy autoAcceptFollowed toggle flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('toggle autoAcceptFollowed switch (2nd) → /api/i/update round-trips', async ({
    page,
    baseURL,
    request,
  }) => {
    // 値 strict assertion のため初期 state を false (= DB default) に reset。
    // #972 (PR #973) で i/update が autoAcceptFollowed を accept するように
    // 修正されたので strict 化が機能する。
    await callApi(request, 'i/update', { i: root.token, autoAcceptFollowed: false });

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/settings/privacy`, { waitUntil: 'domcontentloaded' });

    await page.waitForFunction(
      () => document.querySelectorAll('input[type="checkbox"]').length >= 7,
      { timeout: 20_000 },
    );

    const updateResp = page.waitForResponse(
      (r) => r.url().includes('/api/i/update') && r.status() === 200,
      { timeout: 15_000 },
    );
    await clickWhenReady(page, '2 番目の checkbox', () => {
      const cbs = Array.from(
        document.querySelectorAll('input[type="checkbox"]'),
      ) as HTMLInputElement[];
      // index 1 = 2 番目 = autoAcceptFollowed
      return cbs[1];
    });
    const update = await updateResp;
    const body = await update.json();
    expect(body.id).toBeTruthy();
    // beforeAll の API reset で false から始まるので、click 後は必ず true
    // (#972 / PR #973 で i/update accept + #969 PackMeDetailed で response
    // 含む、両 layer 揃って strict round-trip が機能)。
    expect(body.autoAcceptFollowed).toBe(true);
  });
});
