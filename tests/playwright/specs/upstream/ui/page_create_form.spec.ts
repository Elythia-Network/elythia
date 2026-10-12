/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /pages/new で MkInput (title / summary / name) → Save click →
// /api/pages/create round-trip → backend に page が persist される
// **真の write-flow** spec。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonContainingText } from '../../../fixtures/ui_click';

test.describe('UI: /pages/new form flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('navigate /pages/new → fill title + name → save → /api/pages/create persists', async ({
    page,
    baseURL,
    request,
  }) => {
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/pages/new`, { waitUntil: 'domcontentloaded' });

    // page editor の MkInput x3 (title / summary / name) が hydrate
    await page.waitForFunction(
      () => document.querySelectorAll('input').length >= 3,
      { timeout: 20_000 },
    );

    const title = `pwpageui-title-${Date.now().toString().slice(-9)}`;
    const slug = `pwpageui${Date.now().toString().slice(-9)}`;

    // input 0 = title, input 1 = summary, input 2 = name (slug)
    await page.evaluate(
      ({ t, s }) => {
        const inputs = Array.from(document.querySelectorAll('input')) as HTMLInputElement[];
        const setter = Object.getOwnPropertyDescriptor(
          window.HTMLInputElement.prototype,
          'value',
        )?.set;
        const setValue = (el: HTMLInputElement, v: string) => {
          el.focus();
          setter?.call(el, v);
          el.dispatchEvent(new Event('input', { bubbles: true }));
        };
        if (inputs[0]) setValue(inputs[0], t);
        if (inputs[2]) setValue(inputs[2], s);
      },
      { t: title, s: slug },
    );

    // pages/create response 捕捉して Save click
    const createResp = page.waitForResponse(
      (r) => r.url().includes('/api/pages/create') && r.status() === 200,
      { timeout: 15_000 },
    );
    await clickButtonContainingText(page, 'Save');
    const created = await createResp;
    const createdBody = await created.json();
    expect(createdBody.id).toBeTruthy();
    expect(createdBody.title).toBe(title);
    expect(createdBody.name).toBe(slug);

    // backend で /api/pages/show 経由で round-trip 確認
    const showResp = await callApi(request, 'pages/show', {
      i: root.token,
      pageId: createdBody.id,
    });
    expect(showResp.status()).toBe(200);
    expect((await showResp.json()).title).toBe(title);
  });
});
