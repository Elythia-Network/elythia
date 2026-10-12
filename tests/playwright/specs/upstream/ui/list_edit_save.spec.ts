/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /my/lists/:listId で 既存 list の name を変更 → Save click →
// /api/users/lists/update round-trip する **真の write-flow** spec。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonContainingText, clickByTestId } from '../../../fixtures/ui_click';

test.describe('UI: /my/lists/:listId update flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('create list via API → edit name → Save → /api/users/lists/update', async ({
    page,
    baseURL,
    request,
  }) => {
    const initialName = `pwlist-init-${Date.now().toString().slice(-9)}`;
    const createResp = await callApi(request, 'users/lists/create', {
      i: root.token,
      name: initialName,
    });
    expect(createResp.status()).toBe(200);
    const listId = (await createResp.json()).id;
    expect(listId).toBeTruthy();

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/my/lists/${listId}`, { waitUntil: 'domcontentloaded' });

    // /my/lists/:id の "Settings" MkFolder は defaultOpen ではないので
    // まず folder header を click して expand する。
    await clickByTestId(page, 'folder-header');

    await page.waitForFunction(
      (n) => {
        const inputs = Array.from(document.querySelectorAll('input')) as HTMLInputElement[];
        return inputs.some((i) => i.value === n);
      },
      initialName,
      { timeout: 10_000 },
    );

    const newName = `pwlist-updated-${Date.now().toString().slice(-9)}`;
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
      { from: initialName, to: newName },
    );

    const updateResp = page.waitForResponse(
      (r) => r.url().includes('/api/users/lists/update') && r.status() < 400,
      { timeout: 15_000 },
    );
    await clickButtonContainingText(page, 'Save');
    const resp = await updateResp;
    expect(resp.status()).toBeLessThan(400);
  });
});
