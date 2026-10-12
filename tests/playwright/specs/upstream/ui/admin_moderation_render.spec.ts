/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/moderation page で MkSwitch + MkSelect + textarea 等の form
// controls が hydrate されることを smoke する spec。
//
// /admin/moderation は registration / email-required / serverRules /
// ugcVisibility 等を admin/meta 経由で read/write する form の集合。本
// spec は controls が必要数 mount されるかだけを smoke する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /admin/moderation page hydrates form controls', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('moderation form (switches / selects) hydrates', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/admin/moderation`, { waitUntil: 'domcontentloaded' });
    expect(resp!.status()).toBe(200);

    // openRegistration / emailRequiredForSignup の MkSwitch + ugcVisibility
    // / sensitiveWords 等の MkSelect/MkTextarea があり、admin/meta が
    // hydrate されないと bind されないので、checkbox/input が複数 mount
    // される=hydrate 完了の sign。
    await page.waitForFunction(
      () => {
        const checkboxes = document.querySelectorAll('input[type="checkbox"]').length;
        const inputs = document.querySelectorAll('input').length;
        return checkboxes >= 2 || inputs >= 3;
      },
      { timeout: 20_000 },
    );
  });
});
