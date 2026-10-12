/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /invite page (= user-side invite code creation) で MkButton +
// invite code list が hydrate されることを smoke する spec。authenticated
// page で /admin/invites とは別 (user 自身の発行枠)。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /invite (user) page hydrates', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('invite page renders Create invite button', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/invite`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // i18n.ts.invite → "Invite" (page title)。Generate invite button は
    // policies.inviteLimit が 0 の role で非表示になることがあるので、
    // page header "Invite" + empty state ("There's nothing to see here")
    // の AND で固有性確保。
    await page.waitForFunction(
      () => {
        const text = document.body.textContent ?? '';
        return text.includes('Invite') && text.includes("There's nothing to see here");
      },
      { timeout: 20_000 },
    );
  });
});
