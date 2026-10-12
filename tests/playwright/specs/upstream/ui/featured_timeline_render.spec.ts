/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /explore (default tab = featured) で MkTab inner buttons (Notes / Poll)
// が hydrate されることを smoke する spec。
//
// /explore は default で featured tab を render し、その中で MkNotesTimeline
// + MkTab (Notes / Polls) を mount する。本 spec は inner MkTab labels の
// 存在で featured page hydration を verify する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /explore featured tab renders inner Notes/Poll tab', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('explore default tab (featured) hydrates with Notes/Poll inner tabs', async ({
    page,
    baseURL,
  }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/explore`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // explore.featured.vue は MkTab で notes / polls の 2 button を mount
    // する。i18n.ts.notes → "Notes", i18n.ts.poll → "Poll" の両方が body
    // に出ることを sign にする (= explore tab=featured 固有)。
    await page.waitForFunction(
      () => {
        const text = document.body.textContent ?? '';
        return text.includes('Notes') && text.includes('Poll');
      },
      { timeout: 20_000 },
    );
  });
});
