/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /play page (= flash index) で Popular / My Plays / Liked Plays tabs が
// hydrate される smoke spec。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /play page hydrates flash index', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('flash index renders Popular + My Plays + Liked Plays tabs', async ({
    page,
    baseURL,
  }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/play`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // i18n.ts._play.featured/my/liked → "Popular" / "My Plays" / "Liked Plays"
    await page.waitForFunction(
      () => {
        const text = document.body.textContent ?? '';
        return (
          text.includes('Popular') &&
          text.includes('My Plays') &&
          text.includes('Liked Plays')
        );
      },
      { timeout: 20_000 },
    );
  });
});
