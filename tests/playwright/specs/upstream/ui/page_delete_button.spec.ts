/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /pages/edit/:id の page editor 内 Delete button (danger ti-trash) →
// confirm OK → /api/pages/delete round-trip する write-flow spec。
//
// page-editor.vue:13 の MkButton danger は ti-trash + "Delete" text、
// click すると os.confirm warning → 承諾後 pages/delete を叩く (line 183)。
// 同 page editor には eyeCatchingImageRemove (line 43) も ti-trash icon を
// 持つので、textContent で "Delete" 一致を加えて区別する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { NOT_FOUND_STATUS } from '../../../fixtures/backend';
import { clickByTestId, clickWhenReady } from '../../../fixtures/ui_click';

test.describe('UI: /pages/edit/:id delete button flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('Delete button → confirm OK → /api/pages/delete', async ({
    page,
    baseURL,
    request,
  }) => {
    const title = `pwpage-del-${Date.now().toString().slice(-9)}`;
    const slug = `pwpagedel${Date.now().toString().slice(-9)}`;
    const createResp = await callApi(request, 'pages/create', {
      i: root.token,
      title,
      name: slug,
      content: [],
      variables: [],
      script: '',
    });
    expect(createResp.status()).toBe(200);
    const pageId = (await createResp.json()).id;
    expect(pageId).toBeTruthy();

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/pages/edit/${pageId}`, {
      waitUntil: 'domcontentloaded',
    });

    // title input が hydrate するまで待つ (= editor 起動)
    await page.waitForFunction(
      (t) => {
        const inputs = Array.from(document.querySelectorAll('input')) as HTMLInputElement[];
        return inputs.some((i) => i.value === t);
      },
      title,
      { timeout: 20_000 },
    );

    // Delete button を click。eyeCatchingImageRemove も ti-trash を持つので
    // textContent に "Delete" を含むことで区別する。
    await clickWhenReady(page, 'i.ti-trash を持つボタン', () =>
      Array.from(document.querySelectorAll('button')).find(
        (b) =>
          b.querySelector('i.ti-trash') !== null &&
          (b.textContent ?? '').trim().match(/^Delete$/i),
      ),
    );

    // confirm dialog OK click
    await page.waitForFunction(
      () => document.querySelector('[data-testid="modal-dialog-ok"]') !== null,
      { timeout: 10_000 },
    );

    const deleteResp = page.waitForResponse(
      (r) => r.url().includes('/api/pages/delete') && r.status() < 300,
      { timeout: 15_000 },
    );
    await clickByTestId(page, 'modal-dialog-ok');
    await deleteResp;

    // API 経由で削除確認 — pages/show は 404 + NO_SUCH_PAGE を返す
    // (pages/handler.go:418、UUID 222120c0-3ead-4528-811b-b96f233388d7)。
    const showResp = await callApi(request, 'pages/show', {
      i: root.token,
      pageId,
    });
    expect(showResp.status()).toBe(NOT_FOUND_STATUS);
    const showBody = await showResp.json();
    expect(showBody.error?.code).toBe('NO_SUCH_PAGE');
    expect(showBody.error?.id).toBe('222120c0-3ead-4528-811b-b96f233388d7');
  });
});
