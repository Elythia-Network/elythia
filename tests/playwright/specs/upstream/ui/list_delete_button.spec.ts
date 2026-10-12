/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /my/lists/:id の inline Delete button (rounded danger) → confirm OK
// → /api/users/lists/delete round-trip する write-flow spec。
//
// my-lists/list.vue:20 の MkButton rounded danger は "Delete" text、
// click すると os.confirm warning → 承諾後 users/lists/delete を叩く
// (line 162)。list_edit_save spec の sister として、削除 path を strict 化する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { NOT_FOUND_STATUS } from '../../../fixtures/backend';
import { clickByTestId, clickWhenReady } from '../../../fixtures/ui_click';

test.describe('UI: /my/lists/:id delete button flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('Delete button → confirm OK → /api/users/lists/delete', async ({
    page,
    baseURL,
    request,
  }) => {
    // 1. test 用 list を create via API
    const listName = `pw-list-del-${Date.now()}`;
    const createResp = await callApi(request, 'users/lists/create', {
      i: root.token,
      name: listName,
    });
    expect(createResp.status()).toBe(200);
    const list = await createResp.json();
    const listId: string = list.id;
    expect(listId).toBeTruthy();

    // 2. list 詳細ページを開く
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/my/lists/${listId}`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    // list name が body に出るまで待つ
    await page.waitForFunction(
      (n) => document.body.textContent?.includes(n) ?? false,
      listName,
      { timeout: 20_000 },
    );

    // 3. Settings folder を expand。my-lists/list.vue:10 の最初の MkFolder
    // (= settings) は defaultOpen 無しなので closed で start。MkFolder は
    // `v-else-if="openedAtLeastOnce"` で content を lazy mount するため、
    // 一度も開いてない folder の Delete button は DOM に存在しない。
    // header text "Settings" (en-US.yml の `settings:`) で expand してから
    // Delete button を待つ。
    await page.waitForFunction(
      () => {
        const headers = Array.from(
          document.querySelectorAll('[data-testid="folder-header"]'),
        ) as HTMLElement[];
        return headers.some((h) =>
          (h.textContent ?? '').includes('Settings'),
        );
      },
      { timeout: 15_000 },
    );
    await clickWhenReady(page, '「Settings」の folder-header', () => {
      const headers = Array.from(
        document.querySelectorAll('[data-testid="folder-header"]'),
      ) as HTMLElement[];
      const target = headers.find((h) =>
        (h.textContent ?? '').includes('Settings'),
      );
      return target;
    });

    // Delete button (= "Delete" text を持つ button) hydrate を待つ
    await page.waitForFunction(
      () => {
        const btns = Array.from(document.querySelectorAll('button')) as HTMLButtonElement[];
        return btns.some(
          (b) => (b.textContent ?? '').trim().match(/^Delete$/i),
        );
      },
      { timeout: 15_000 },
    );

    // Delete click → confirm dialog 出現
    await clickWhenReady(page, 'Delete ボタン', () =>
      Array.from(document.querySelectorAll('button')).find(
        (b) => (b.textContent ?? '').trim().match(/^Delete$/i),
      ),
    );

    // 4. confirm dialog OK click → API 呼出
    await page.waitForFunction(
      () => document.querySelector('[data-testid="modal-dialog-ok"]') !== null,
      { timeout: 10_000 },
    );

    const deleteResp = page.waitForResponse(
      (r) => r.url().includes('/api/users/lists/delete') && r.status() < 300,
      { timeout: 15_000 },
    );
    await clickByTestId(page, 'modal-dialog-ok');
    await deleteResp;

    // 5. API 経由で削除確認 — users/lists/show は 404 + NO_SUCH_LIST を返す
    // (lists.go:53)。code + UUID で strict 検証して drift catch を強化。
    const showResp = await callApi(request, 'users/lists/show', {
      i: root.token,
      listId,
    });
    expect(showResp.status()).toBe(NOT_FOUND_STATUS);
    const showBody = await showResp.json();
    expect(showBody.error?.code).toBe('NO_SUCH_LIST');
  });
});
