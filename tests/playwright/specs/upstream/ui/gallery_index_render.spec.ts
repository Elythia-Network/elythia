/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /gallery page (= gallery index) で Popular / My Gallery / Liked Posts
// tabs が hydrate される smoke spec。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /gallery page hydrates gallery index', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('gallery index renders Popular + My Gallery + Liked Posts tabs', async ({
    page,
    baseURL,
  }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/gallery`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // i18n.ts._gallery.featured/my/liked → "Trending" / "My Gallery" /
    // "Liked Posts"
    await page.waitForFunction(
      () => {
        const text = document.body.textContent ?? '';
        return (
          text.includes('Trending') &&
          text.includes('My Gallery') &&
          text.includes('Liked Posts')
        );
      },
      { timeout: 20_000 },
    );
  });
});
