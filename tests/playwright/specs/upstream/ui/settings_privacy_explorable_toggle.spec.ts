/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /settings/privacy で isExplorable switch を click → /api/i/update が
// {isExplorable: ...} で round-trip する **真の write-flow** spec。
//
// privacy.vue では isLocked / autoAcceptFollowed / publicReactions /
// hideOnlineStatus / noCrawle / preventAiLearning / isExplorable の順で
// MkSwitch が並んでいる (line 14, 22, 29, 48, 55, 62, 69)。本 spec は
// 7 番目の switch (isExplorable) を click することで、最初の switch だけ
// が動作する偽陽性を排除する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickWhenReady } from '../../../fixtures/ui_click';

test.describe('UI: /settings/privacy isExplorable toggle flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('toggle isExplorable switch (7th) → /api/i/update round-trips', async ({
    page,
    baseURL,
    request,
  }) => {
    // 値 strict assertion を効かせるため、初期 state を true (= DB default)
    // に reset。prior run 累積で false のまま残っていると click で true に
    // 反転して期待と逆になるので、明示的に既知 state から始める。
    await callApi(request, 'i/update', { i: root.token, isExplorable: true });

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/settings/privacy`, { waitUntil: 'domcontentloaded' });

    // 7 個以上の checkbox が hydrate するまで待つ
    await page.waitForFunction(
      () => document.querySelectorAll('input[type="checkbox"]').length >= 7,
      { timeout: 20_000 },
    );

    const updateResp = page.waitForResponse(
      (r) => r.url().includes('/api/i/update') && r.status() === 200,
      { timeout: 15_000 },
    );
    await clickWhenReady(page, '7 番目の checkbox', () => {
      // 7 番目 (index 6) = isExplorable switch (privacy.vue:69)
      const cbs = Array.from(
        document.querySelectorAll('input[type="checkbox"]'),
      ) as HTMLInputElement[];
      return cbs[6];
    });
    // beforeAll の API reset で isExplorable=true から始まるので、click
    // 後は必ず false が返る strict assertion。i/update が MeDetailed
    // shape (#968 / PR #969) を返すことも併せて verify。
    const update = await updateResp;
    const body = await update.json();
    expect(body.id).toBeTruthy();
    expect(body.username).toBe(root.username);
    expect(body.isExplorable).toBe(false);
  });
});
