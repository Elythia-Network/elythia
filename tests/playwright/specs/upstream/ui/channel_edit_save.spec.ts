/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /channels/:id/edit で 既存 channel の name を変更 → Save click →
// /api/channels/update round-trip する **真の write-flow** spec。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonContainingText } from '../../../fixtures/ui_click';

test.describe('UI: /channels/:id/edit update flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('create channel via API → edit name → Save → /api/channels/update', async ({
    page,
    baseURL,
    request,
  }) => {
    const initialName = `pwchan-init-${Date.now().toString().slice(-9)}`;
    const createResp = await callApi(request, 'channels/create', {
      i: root.token,
      name: initialName,
    });
    expect(createResp.status()).toBe(200);
    const channelId = (await createResp.json()).id;
    expect(channelId).toBeTruthy();

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/channels/${channelId}/edit`, {
      waitUntil: 'domcontentloaded',
    });

    // name MkInput が initialName で hydrate
    await page.waitForFunction(
      (n) => {
        const inputs = Array.from(document.querySelectorAll('input')) as HTMLInputElement[];
        return inputs.some((i) => i.value === n);
      },
      initialName,
      { timeout: 20_000 },
    );

    const newName = `pwchan-updated-${Date.now().toString().slice(-9)}`;
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

    // channel-editor.vue line 61: existing channel の button text は
    // i18n.ts.save = "Save" (channelId が truthy のとき)
    const updateResp = page.waitForResponse(
      (r) => r.url().includes('/api/channels/update') && r.status() < 400,
      { timeout: 15_000 },
    );
    await clickButtonContainingText(page, 'Save');
    const resp = await updateResp;
    expect(resp.status()).toBeLessThan(400);
  });
});
