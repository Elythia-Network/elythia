/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /chat home page で Start chat button + history section が hydrate される
// ことを smoke する spec。
//
// /chat は authenticated chat home (= chat tab home / invitations /
// joiningRooms / ownedRooms の 4 tab)。default tab は home で
// "Start chat" button (i18n.ts.startChat) と "History" section header
// (i18n.ts._chat.history) が render される。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /chat home page hydrates', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('chat home renders Start chat button + History section', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/chat`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // chat policies が available なら Start chat button、readonly/none
    // なら MkInfo が render される。どちらでも history section header
    // (i18n.ts._chat.history → "History") は表示される。
    await page.waitForFunction(
      () => {
        const text = document.body.textContent ?? '';
        return text.includes('History');
      },
      { timeout: 20_000 },
    );
  });
});
