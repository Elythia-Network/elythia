/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /settings/security page で password / signin history / regenerate token
// 等の form sections が hydrate されることを smoke する spec。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /settings/security page hydrates', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('settings/security renders Password / Sign-in history sections', async ({
    page,
    baseURL,
  }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/settings/security`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    // i18n.ts.password → "Password", i18n.ts.signinHistory → "Login history"
    // 等の label が render される。"Password" + "Login history" の AND
    // で固有性確保。
    await page.waitForFunction(
      () => {
        const text = document.body.textContent ?? '';
        return text.includes('Password') && text.includes('Login history');
      },
      { timeout: 20_000 },
    );
  });
});
