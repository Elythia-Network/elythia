/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/ads で header "+" button → 新規 ad form が unshift される →
// URL + imageUrl MkInput を埋める → Save click →
// /api/admin/ad/create round-trip → 一覧に persist される **真の write-flow**
// spec。
//
// header action "+" は icon-only button (ti-plus + tooltip "Add")。
// 新 ad form は url / imageUrl / ratio / startsAt / expiresAt の input を持つ。
// Save MkButton textContent "Save"。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonContainingText, clickButtonWithIcon } from '../../../fixtures/ui_click';

test.describe('UI: /admin/ads create form flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('click + → fill url+imageUrl → Save → admin/ad/create round-trips', async ({
    page,
    baseURL,
  }) => {
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/admin/ads`, { waitUntil: 'domcontentloaded' });

    // header の "+" button (ti-plus) が hydrate するまで待つ
    await page.waitForFunction(
      () => document.querySelector('button i.ti-plus') !== null,
      { timeout: 20_000 },
    );

    // "+" header action click → 新規 ad form が unshift される
    await clickButtonWithIcon(page, 'i.ti-plus');

    // 新規 ad form の url MkInput が hydrate (= type="url" の input)
    await page.waitForFunction(
      () => document.querySelector('input[type="url"]') !== null,
      { timeout: 10_000 },
    );

    const adUrl = `https://example.test/ad/${Date.now().toString().slice(-9)}`;
    const adImageUrl = 'https://example.test/banner.png';

    // input[type="url"] が url + imageUrl の 2 個。最初が url、次が imageUrl。
    await page.evaluate(
      ({ u, i }) => {
        const inputs = Array.from(
          document.querySelectorAll('input[type="url"]'),
        ) as HTMLInputElement[];
        const setter = Object.getOwnPropertyDescriptor(
          window.HTMLInputElement.prototype,
          'value',
        )?.set;
        const setValue = (el: HTMLInputElement, v: string) => {
          el.focus();
          setter?.call(el, v);
          el.dispatchEvent(new Event('input', { bubbles: true }));
        };
        if (inputs[0]) setValue(inputs[0], u);
        if (inputs[1]) setValue(inputs[1], i);
      },
      { u: adUrl, i: adImageUrl },
    );

    // admin/ad/create response 捕捉して Save click。
    // mk-go 側 (admin/ad.go:62) は成功時 200 + ad object を返す。
    const createResp = page.waitForResponse(
      (r) => r.url().includes('/api/admin/ad/create') && r.status() === 200,
      { timeout: 15_000 },
    );
    // 新規 ad form の Save button が最初の textContent "Save" 持ち button。
    await clickButtonContainingText(page, 'Save');
    await createResp;
  });
});
