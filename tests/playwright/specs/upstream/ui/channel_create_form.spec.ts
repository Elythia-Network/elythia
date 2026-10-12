/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /channels/new で MkChannelEditor の name MkInput → Create click →
// /api/channels/create round-trip → SPA は /channels/:id に router.push
// する **真の write-flow** spec。
//
// channelId がない時 button の textContent は i18n.ts.create → "Create"。
// description / banner / pinned notes は default 空のまま。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonContainingText } from '../../../fixtures/ui_click';

test.describe('UI: /channels/new form flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('navigate /channels/new → fill name → Create → channels/create round-trips', async ({
    page,
    baseURL,
  }) => {
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/channels/new`, { waitUntil: 'domcontentloaded' });

    // name MkInput が hydrate (= 最初の input)
    await page.waitForFunction(
      () => document.querySelectorAll('input').length >= 1,
      { timeout: 20_000 },
    );

    const channelName = `pwchanui-${Date.now().toString().slice(-9)}`;
    await page.evaluate((n) => {
      const target = document.querySelector('input') as HTMLInputElement | null;
      if (!target) return;
      target.focus();
      const setter = Object.getOwnPropertyDescriptor(
        window.HTMLInputElement.prototype,
        'value',
      )?.set;
      setter?.call(target, n);
      target.dispatchEvent(new Event('input', { bubbles: true }));
    }, channelName);

    // channels/create response 捕捉して Create click
    const createResp = page.waitForResponse(
      (r) => r.url().includes('/api/channels/create') && r.status() === 200,
      { timeout: 15_000 },
    );
    await clickButtonContainingText(page, 'Create');
    const created = await createResp;
    const body = await created.json();
    expect(body.id).toBeTruthy();
    expect(body.name).toBe(channelName);

    // SPA は /channels/:channelId に router.push する
    await page.waitForFunction(
      (n) => document.body.textContent?.includes(n) ?? false,
      channelName,
      { timeout: 20_000 },
    );
  });
});
