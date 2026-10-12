/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /reset-password/:token? page で password reset form が hydrate される
// ことを smoke する spec。token 付きで navigate すると input が見える。

import { expect, test } from '@playwright/test';

test.describe('UI: /reset-password/:token page hydrates form', () => {
  test.setTimeout(30_000);

  test('reset-password page with dummy token renders new password input', async ({
    page,
    baseURL,
  }) => {
    await page.setViewportSize({ width: 1600, height: 900 });
    // dummy token (実 token ではないが UI hydration 検証は可能 — submit 時に
    // backend が 4xx を返すが本 spec の scope は form mount まで)
    const resp = await page.goto(`${baseURL}/reset-password/playwright-dummy`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    // i18n.ts.newPassword → "New password" + password input が render
    await page.waitForFunction(
      () => {
        const text = document.body.textContent ?? '';
        const passwordInputs = document.querySelectorAll('input[type="password"]').length;
        return text.includes('New password') && passwordInputs >= 1;
      },
      { timeout: 20_000 },
    );
  });
});
