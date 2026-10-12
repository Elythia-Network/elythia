/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /pages/edit/:id で 既存 page の title を変更 → Save click →
// /api/pages/update round-trip する **真の write-flow** spec。
//
// API setup: pages/create で新 page を 1 つ作る (= alice の page)。次に
// /pages/edit/:id に navigate して page-editor.vue が hydrate されたら
// title input を書き換え、Save click。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonContainingText } from '../../../fixtures/ui_click';

test.describe('UI: /pages/edit/:id update form flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('create via API → edit page title → Save → /api/pages/update', async ({
    page,
    baseURL,
    request,
  }) => {
    // setup: 編集対象の page を 1 つ作成
    const initialTitle = `pwpage-init-${Date.now().toString().slice(-9)}`;
    const slug = `pwpageed${Date.now().toString().slice(-9)}`;
    const createResp = await callApi(request, 'pages/create', {
      i: root.token,
      title: initialTitle,
      name: slug,
      content: [],
      variables: [],
      script: '',
    });
    expect(createResp.status()).toBe(200);
    const created = await createResp.json();
    const pageId = created.id;
    expect(pageId).toBeTruthy();

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/pages/edit/${pageId}`, { waitUntil: 'domcontentloaded' });

    // page editor の title MkInput が initialTitle で hydrate
    await page.waitForFunction(
      (t) => {
        const inputs = Array.from(document.querySelectorAll('input')) as HTMLInputElement[];
        return inputs.some((i) => i.value === t);
      },
      initialTitle,
      { timeout: 20_000 },
    );

    // title を書き換え
    const newTitle = `pwpage-updated-${Date.now().toString().slice(-9)}`;
    await page.evaluate(
      ({ from, to }) => {
        const target = (
          Array.from(document.querySelectorAll('input')) as HTMLInputElement[]
        ).find((i) => i.value === from);
        if (!target) return;
        target.focus();
        const setter = Object.getOwnPropertyDescriptor(
          window.HTMLInputElement.prototype,
          'value',
        )?.set;
        setter?.call(target, to);
        target.dispatchEvent(new Event('input', { bubbles: true }));
      },
      { from: initialTitle, to: newTitle },
    );

    // pages/update response 捕捉して Save click
    const updateResp = page.waitForResponse(
      (r) => r.url().includes('/api/pages/update') && r.status() < 400,
      { timeout: 15_000 },
    );
    await clickButtonContainingText(page, 'Save');
    const resp = await updateResp;
    expect(resp.status()).toBeLessThan(400);
  });
});
