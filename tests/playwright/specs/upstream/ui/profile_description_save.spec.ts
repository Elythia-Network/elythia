/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /settings/profile で description (= "Bio") MkTextarea を編集 → save click
// → /api/i.description が更新されることを verify する write-flow spec。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonContainingText } from '../../../fixtures/ui_click';

test.describe('UI: /settings/profile description save flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('edit description textarea → click save → /api/i.description updated', async ({
    page,
    baseURL,
    request,
  }) => {
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/settings/profile`, { waitUntil: 'domcontentloaded' });

    // textarea が hydrate するまで待つ (Bio MkTextarea)
    await page.waitForFunction(
      () => document.querySelectorAll('textarea').length > 0,
      { timeout: 20_000 },
    );

    const newBio = `pwbio-${Date.now().toString().slice(-9)}`;
    // 最初の textarea (= profile.description / Bio) に書き込み
    await page.evaluate((b) => {
      const ta = document.querySelector('textarea') as HTMLTextAreaElement | null;
      if (!ta) return;
      ta.focus();
      const setter = Object.getOwnPropertyDescriptor(
        window.HTMLTextAreaElement.prototype,
        'value',
      )?.set;
      setter?.call(ta, b);
      ta.dispatchEvent(new Event('input', { bubbles: true }));
      ta.dispatchEvent(new Event('change', { bubbles: true }));
    }, newBio);

    // Save button が出現するまで待つ
    await page.waitForFunction(
      () => {
        const btns = Array.from(document.querySelectorAll('button'));
        return btns.some((b) => (b.textContent ?? '').includes('Save'));
      },
      { timeout: 10_000 },
    );

    // i/update response を捕捉して save click。textarea 用の save button は
    // 最初の "Save" button (= name は別 input なのでこの pageで複数 Save が
    // 同時に出ることはない、textarea を編集した時点で description の
    // save だけが mount される)
    const updateResp = page.waitForResponse(
      (r) => r.url().includes('/api/i/update') && r.status() === 200,
      { timeout: 15_000 },
    );
    await clickButtonContainingText(page, 'Save');
    await updateResp;

    const meResp = await callApi(request, 'i', { i: root.token });
    expect(meResp.status()).toBe(200);
    const me = await meResp.json();
    expect(me.description).toBe(newBio);
  });
});
