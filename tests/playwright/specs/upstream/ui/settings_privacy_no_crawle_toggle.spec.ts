/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /settings/privacy で 5 番目 switch (noCrawle) を click → /api/i/update
// が走ることを verify する **真の write-flow** spec。
//
// 異なる index の switch を toggle することで「最初の switch だけ動く偽
// 陽性」regression を排除する補助 spec (#744 batch4)。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickWhenReady } from '../../../fixtures/ui_click';

test.describe('UI: /settings/privacy noCrawle toggle flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('toggle noCrawle switch (5th) → /api/i/update round-trips', async ({
    page,
    baseURL,
    request,
  }) => {
    // 値 strict assertion を効かせるため初期 state を false (= DB default)
    // に reset。
    await callApi(request, 'i/update', { i: root.token, noCrawle: false });

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/settings/privacy`, { waitUntil: 'domcontentloaded' });

    await page.waitForFunction(
      () => document.querySelectorAll('input[type="checkbox"]').length >= 5,
      { timeout: 20_000 },
    );

    const updateResp = page.waitForResponse(
      (r) => r.url().includes('/api/i/update') && r.status() === 200,
      { timeout: 15_000 },
    );
    await clickWhenReady(page, '5 番目の checkbox', () => {
      // privacy.vue の switch 順: isLocked / autoAcceptFollowed /
      // publicReactions / hideOnlineStatus / noCrawle (= index 4) /
      // preventAiLearning / isExplorable
      const cbs = Array.from(
        document.querySelectorAll('input[type="checkbox"]'),
      ) as HTMLInputElement[];
      return cbs[4];
    });
    const update = await updateResp;
    const body = await update.json();
    expect(body.id).toBeTruthy();
    // beforeAll の API reset で false から始まるので、click 後は必ず true
    // (#969 の MeDetailed packer 経由で noCrawle が body に含まれる)。
    expect(body.noCrawle).toBe(true);
  });
});
