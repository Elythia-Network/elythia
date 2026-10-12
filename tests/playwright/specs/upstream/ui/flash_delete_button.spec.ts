/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /play/:id/edit の Delete button (danger ti-trash) → confirm OK →
// /api/flash/delete round-trip する write-flow spec。
//
// flash-edit.vue:32 の MkButton danger は ti-trash + "Delete" text、
// click すると os.confirm warning → 承諾後 flash/delete を叩く (line 467)。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { NOT_FOUND_STATUS } from '../../../fixtures/backend';
import { clickByTestId, clickWhenReady } from '../../../fixtures/ui_click';

test.describe('UI: /play/:id/edit delete button flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('Delete button → confirm OK → /api/flash/delete', async ({
    page,
    baseURL,
    request,
  }) => {
    const title = `pwflash-del-${Date.now().toString().slice(-9)}`;
    const createResp = await callApi(request, 'flash/create', {
      i: root.token,
      title,
      summary: '',
      script: '/// @ 1.0.0',
      permissions: [],
    });
    expect(createResp.status()).toBe(200);
    const flashId = (await createResp.json()).id;
    expect(flashId).toBeTruthy();

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/play/${flashId}/edit`, {
      waitUntil: 'domcontentloaded',
    });

    await page.waitForFunction(
      (t) => {
        const inputs = Array.from(document.querySelectorAll('input')) as HTMLInputElement[];
        return inputs.some((i) => i.value === t);
      },
      title,
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
      (r) => r.url().includes('/api/flash/delete') && r.status() < 300,
      { timeout: 15_000 },
    );
    await clickByTestId(page, 'modal-dialog-ok');
    await deleteResp;

    // 削除確認 — flash/show は 404 + NO_SUCH_FLASH を返す
    // (flash/handler.go:371、UUID f0d34a1a-d29a-401d-90ba-1982122b5630)。
    const showResp = await callApi(request, 'flash/show', {
      i: root.token,
      flashId,
    });
    expect(showResp.status()).toBe(NOT_FOUND_STATUS);
    const showBody = await showResp.json();
    expect(showBody.error?.code).toBe('NO_SUCH_FLASH');
    expect(showBody.error?.id).toBe('f0d34a1a-d29a-401d-90ba-1982122b5630');
  });
});
