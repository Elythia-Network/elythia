/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /play/new で flash editor の title MkInput → Save click →
// /api/flash/create round-trip → SPA は /play:id/edit に router.push する
// **真の write-flow** spec。
//
// title input の他に visibility select, summary textarea, script editor が
// あるが、最低限 title だけ埋めれば flash/create は走る。Save button は
// textContent "Save"。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonContainingText } from '../../../fixtures/ui_click';

test.describe('UI: /play/new flash editor form flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('navigate /play/new → fill title → Save → flash/create round-trips', async ({
    page,
    baseURL,
  }) => {
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/play/new`, { waitUntil: 'domcontentloaded' });

    // title MkInput は default value "New Play" で hydrate する。
    // page 全体には sidebar / widget の hidden input が混じるため、
    // value === "New Play" でユニーク locate する (#744 batch3)。
    await page.waitForFunction(
      () => {
        const inputs = Array.from(document.querySelectorAll('input')) as HTMLInputElement[];
        return inputs.some((i) => i.value === 'New Play');
      },
      { timeout: 20_000 },
    );

    const flashTitle = `pwflashui-${Date.now().toString().slice(-9)}`;
    await page.evaluate((n) => {
      const target = (Array.from(document.querySelectorAll('input')) as HTMLInputElement[]).find(
        (i) => i.value === 'New Play',
      );
      if (!target) return;
      target.focus();
      const setter = Object.getOwnPropertyDescriptor(
        window.HTMLInputElement.prototype,
        'value',
      )?.set;
      setter?.call(target, n);
      target.dispatchEvent(new Event('input', { bubbles: true }));
    }, flashTitle);

    // flash/create response 捕捉して Save click
    const createResp = page.waitForResponse(
      (r) => r.url().includes('/api/flash/create') && r.status() === 200,
      { timeout: 15_000 },
    );
    await clickButtonContainingText(page, 'Save');
    const created = await createResp;
    const body = await created.json();
    expect(body.id).toBeTruthy();
    expect(body.title).toBe(flashTitle);
  });
});
