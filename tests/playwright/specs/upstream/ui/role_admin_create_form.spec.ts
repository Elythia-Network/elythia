/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/roles/new で role name MkInput → Save click →
// /api/admin/roles/create round-trip → SPA が /admin/roles/:id に
// router.push し新 role detail page に遷移する **真の write-flow** spec。
//
// editor の MkInput は default value "New Role"。それを unique 値に
// 書き換えて Save を押す。footer の Save MkButton は textContent "Save"。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonContainingText } from '../../../fixtures/ui_click';

test.describe('UI: /admin/roles/new form flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('navigate /admin/roles/new → fill name → save → admin/roles/create round-trips', async ({
    page,
    baseURL,
  }) => {
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/admin/roles/new`, { waitUntil: 'domcontentloaded' });

    // role.name MkInput が hydrate (= default "New Role" が入る)
    await page.waitForFunction(
      () => {
        const inputs = Array.from(document.querySelectorAll('input')) as HTMLInputElement[];
        return inputs.some((i) => i.value === 'New Role');
      },
      { timeout: 20_000 },
    );

    const roleName = `pwroleui-${Date.now().toString().slice(-9)}`;
    await page.evaluate((n) => {
      const target = (Array.from(document.querySelectorAll('input')) as HTMLInputElement[]).find(
        (i) => i.value === 'New Role',
      );
      if (!target) return;
      target.focus();
      const setter = Object.getOwnPropertyDescriptor(
        window.HTMLInputElement.prototype,
        'value',
      )?.set;
      setter?.call(target, n);
      target.dispatchEvent(new Event('input', { bubbles: true }));
    }, roleName);

    // admin/roles/create response 捕捉して Save click
    const createResp = page.waitForResponse(
      (r) => r.url().includes('/api/admin/roles/create') && r.status() === 200,
      { timeout: 15_000 },
    );
    await clickButtonContainingText(page, 'Save');
    const created = await createResp;
    const createdBody = await created.json();
    expect(createdBody.id).toBeTruthy();
    expect(createdBody.name).toBe(roleName);

    // SPA は /admin/roles/:id に router.push する。新 role 名が body に出る。
    await page.waitForFunction(
      (n) => document.body.textContent?.includes(n) ?? false,
      roleName,
      { timeout: 20_000 },
    );
  });
});
