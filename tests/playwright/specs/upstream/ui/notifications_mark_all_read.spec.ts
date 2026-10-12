/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /my/notifications で header の "Mark all as read" button click →
// /api/notifications/mark-all-as-read round-trip する **真の write-flow** spec。
//
// notifications.vue の headerActions に "Mark all as read" (icon ti-check)
// が定義されていて (line 71-77)、tab='all' のときだけ visible。本 spec は
// 通知 0 件でも button click で API は走ること (= empty state でも UI が
// stale で hang しない) を verify する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonWithIcon } from '../../../fixtures/ui_click';

test.describe('UI: /my/notifications mark-all-as-read flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('click mark-all-as-read header → /api/notifications/mark-all-as-read', async ({
    page,
    baseURL,
  }) => {
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/my/notifications`, { waitUntil: 'domcontentloaded' });

    // header action "Mark all as read" は icon ti-check の icon-only button
    // (tooltip text のみ、textContent は空)。ti-check icon を持つ button を
    // header から取る。
    await page.waitForFunction(
      () => document.querySelector('button i.ti-check') !== null,
      { timeout: 20_000 },
    );

    // mk-go (notifications/handler.go) は成功時 204 を返す。upstream Misskey
    // TS は 200 で response body 無し。drop-in 互換で 200/204 両方を accept。
    const markResp = page.waitForResponse(
      (r) =>
        r.url().includes('/api/notifications/mark-all-as-read') && r.status() < 300,
      { timeout: 15_000 },
    );
    // header の filter / mark-all-as-read 2 button が並ぶ。filter は
    // ti-filter icon、mark-all-as-read は ti-check icon。
    await clickButtonWithIcon(page, 'i.ti-check');
    const resp = await markResp;
    expect(resp.status()).toBeLessThan(300);
  });
});
