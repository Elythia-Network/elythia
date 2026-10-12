/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /settings/webhook/edit/:id で 既存 webhook の name を変更 → Save click →
// /api/i/webhooks/update round-trip する **真の write-flow** spec。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { deleteWebhooksNamed } from '../../../fixtures/quota';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonContainingText } from '../../../fixtures/ui_click';

test.describe('UI: /settings/webhook/edit/:id update flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  // 作った webhook は片付ける。root を共有しているので放置すると
  // webhookLimit (既定 3) を使い切って無関係な spec が create で落ちる (#2264)。
  const createdWebhooks: string[] = [];
  test.afterEach(async ({ request }) => {
    await deleteWebhooksNamed(request, root.token, createdWebhooks);
    createdWebhooks.length = 0;
  });

  test('create webhook via API → edit name → Save → /api/i/webhooks/update', async ({
    page,
    baseURL,
    request,
  }) => {
    const initialName = `pwwh-init-${Date.now().toString().slice(-9)}`;
    createdWebhooks.push(initialName);
    const createResp = await callApi(request, 'i/webhooks/create', {
      i: root.token,
      name: initialName,
      url: 'https://example.test/webhook',
      secret: 'pwwh-secret',
      on: ['follow'],
    });
    expect(createResp.status()).toBe(200);
    const webhookId = (await createResp.json()).id;
    expect(webhookId).toBeTruthy();

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/settings/webhook/edit/${webhookId}`, {
      waitUntil: 'domcontentloaded',
    });

    await page.waitForFunction(
      (n) => {
        const inputs = Array.from(document.querySelectorAll('input')) as HTMLInputElement[];
        return inputs.some((i) => i.value === n);
      },
      initialName,
      { timeout: 20_000 },
    );

    const newName = `pwwh-updated-${Date.now().toString().slice(-9)}`;
    createdWebhooks.push(newName);
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
      (r) => r.url().includes('/api/i/webhooks/update') && r.status() < 400,
      { timeout: 15_000 },
    );
    await clickButtonContainingText(page, 'Save');
    const resp = await updateResp;
    expect(resp.status()).toBeLessThan(400);
  });
});
