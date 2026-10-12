/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /settings/webhook/edit/:id の Delete button (danger ti-trash) → confirm OK
// → /api/i/webhooks/delete round-trip する write-flow spec。
//
// webhook.edit.vue:66 の MkButton danger inline は ti-trash + "Delete" text、
// click すると os.confirm warning → 承諾後 i/webhooks/delete を叩く
// (line 134)。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickByTestId, clickWhenReady } from '../../../fixtures/ui_click';

test.describe('UI: /settings/webhook/edit/:id delete button flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('Delete button → confirm OK → /api/i/webhooks/delete', async ({
    page,
    baseURL,
    request,
  }) => {
    const name = `pwwh-del-${Date.now().toString().slice(-9)}`;
    const createResp = await callApi(request, 'i/webhooks/create', {
      i: root.token,
      name,
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
      name,
      { timeout: 20_000 },
    );

    // Delete button (ti-trash + "Delete" text) を click
    await clickWhenReady(page, 'i.ti-trash を持つボタン', () =>
      Array.from(document.querySelectorAll('button')).find(
        (b) =>
          b.querySelector('i.ti-trash') !== null &&
          (b.textContent ?? '').trim().match(/^Delete$/i),
      ),
    );

    // confirm dialog OK
    await page.waitForFunction(
      () => document.querySelector('[data-testid="modal-dialog-ok"]') !== null,
      { timeout: 10_000 },
    );

    const deleteResp = page.waitForResponse(
      (r) => r.url().includes('/api/i/webhooks/delete') && r.status() < 300,
      { timeout: 15_000 },
    );
    await clickByTestId(page, 'modal-dialog-ok');
    await deleteResp;

    // 削除確認 — webhooks/show or list 経由
    const listResp = await callApi(request, 'i/webhooks/list', {
      i: root.token,
    });
    expect(listResp.status()).toBe(200);
    const list = await listResp.json();
    const found = list.find((w: { id: string }) => w.id === webhookId);
    expect(found).toBeUndefined();
  });
});
