/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /settings/profile page で profile name / description / location 等の
// MkInput / MkTextarea が hydrate されることを smoke する spec。
//
// authenticated_routes.spec.ts は /settings/profile への navigate と
// input 数だけを verify するが、本 spec は profile section の i18n labels
// で固有性を確保する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /settings/profile page hydrates profile form', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('settings/profile renders Name / Bio labels', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/settings/profile`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    // i18n.ts._profile.name → "Name", i18n.ts._profile.description → "Bio"。
    // 両方 + sidebar の Profile (= page nav) が body に出ることで固有性確保。
    await page.waitForFunction(
      () => {
        const text = document.body.textContent ?? '';
        return (
          text.includes('Profile') &&
          text.includes('Name') &&
          text.includes('Bio')
        );
      },
      { timeout: 20_000 },
    );
  });
});
