/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /settings/preferences page で UI language / device kind 等の preferences
// form が hydrate されることを smoke する spec。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /settings/preferences page hydrates', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('preferences page renders General + UI language form', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/settings/preferences`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    // preferences page は MkFolder で各 section を folded で render する。
    // "User interface language" は General section 内なので default 非展開。
    // sidebar の "Preferences" + page 内 section button "Timeline and note"
    // (= preferences page 固有) の AND で固有性確保。
    await page.waitForFunction(
      () => {
        const text = document.body.textContent ?? '';
        return text.includes('Preferences') && text.includes('Timeline and note');
      },
      { timeout: 20_000 },
    );
  });
});
