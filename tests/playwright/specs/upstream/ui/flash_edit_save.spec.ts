/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /play/:id/edit で 既存 flash の title を変更 → Save click →
// /api/flash/update round-trip する **真の write-flow** spec。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonContainingText } from '../../../fixtures/ui_click';

test.describe('UI: /play/:id/edit update flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('create flash via API → edit title → Save → /api/flash/update', async ({
    page,
    baseURL,
    request,
  }) => {
    const initialTitle = `pwflash-init-${Date.now().toString().slice(-9)}`;
    const createResp = await callApi(request, 'flash/create', {
      i: root.token,
      title: initialTitle,
      summary: '',
      script: '/// @ 1.0.0',
      permissions: [],
      visibility: 'public',
    });
    expect(createResp.status()).toBe(200);
    const flashId = (await createResp.json()).id;
    expect(flashId).toBeTruthy();

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/play/${flashId}/edit`, { waitUntil: 'domcontentloaded' });

    await page.waitForFunction(
      (t) => {
        const inputs = Array.from(document.querySelectorAll('input')) as HTMLInputElement[];
        return inputs.some((i) => i.value === t);
      },
      initialTitle,
      { timeout: 20_000 },
    );

    const newTitle = `pwflash-updated-${Date.now().toString().slice(-9)}`;
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

    const updateResp = page.waitForResponse(
      (r) => r.url().includes('/api/flash/update') && r.status() < 400,
      { timeout: 15_000 },
    );
    await clickButtonContainingText(page, 'Save');
    const resp = await updateResp;
    expect(resp.status()).toBeLessThan(400);
  });
});
